package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunSuccessExtractsFinalText(t *testing.T) {
	t.Parallel()

	fake := writeFakeOpenCode(t, `#!/bin/sh
# record argv
printf '%s\n' "$@" > "$FAKE_ARGV_FILE"
cat <<'EOF'
{"type":"reasoning","timestamp":1,"sessionID":"ses_ok","part":{"type":"reasoning","text":"think"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_ok","part":{"type":"tool","tool":"bash","state":{"status":"completed","output":"secret-tool"}}}
{"type":"text","timestamp":3,"sessionID":"ses_ok","part":{"type":"text","text":"All good"}}
EOF
echo "noise on stderr" >&2
exit 0
`)

	dir := t.TempDir()
	logDir := t.TempDir()
	lockDir := t.TempDir()
	argvFile := filepath.Join(t.TempDir(), "argv.txt")

	var stdout bytes.Buffer
	cfg := Config{
		Directory:     mustCanon(t, dir),
		Prompt:        "implement feature",
		Model:         "xai/grok-4.5",
		Auto:          true,
		Timeout:       time.Minute,
		OpenCode:      fake,
		LogDir:        logDir,
		MaxFinalChars: 1200,
	}
	res, code := Run(context.Background(), cfg, RunOptions{
		LockDir: lockDir,
		Stdout:  &stdout,
		Environ: append(os.Environ(), "FAKE_ARGV_FILE="+argvFile),
	})
	require.Equal(t, 0, code)
	require.Equal(t, StatusCompleted, res.Status)
	require.Equal(t, "ses_ok", res.SessionID)
	require.Equal(t, "All good", res.FinalText)
	require.NotContains(t, res.FinalText, "think")
	require.NotContains(t, res.FinalText, "secret-tool")
	require.FileExists(t, res.EventLog)
	require.FileExists(t, res.StderrLog)

	// Single JSON object on stdout.
	var decoded Result
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	require.Equal(t, StatusCompleted, decoded.Status)

	// Event log preserves all raw lines including non-text.
	raw, err := os.ReadFile(res.EventLog)
	require.NoError(t, err)
	require.Contains(t, string(raw), "reasoning")
	require.Contains(t, string(raw), "secret-tool")

	// Stderr captured separately, not on runner stdout.
	stderrBody, err := os.ReadFile(res.StderrLog)
	require.NoError(t, err)
	require.Contains(t, string(stderrBody), "noise on stderr")
	require.NotContains(t, stdout.String(), "noise on stderr")
	require.NotContains(t, stdout.String(), "secret-tool")

	argvBody, err := os.ReadFile(argvFile)
	require.NoError(t, err)
	lines := nonEmptyLines(string(argvBody))
	require.Equal(t, []string{
		"run",
		"--agent",
		"build",
		"--model",
		"xai/grok-4.5",
		"--auto",
		"--format",
		"json",
		"--dir",
		cfg.Directory,
		"implement feature",
	}, lines)
}

func TestRunMalformedJSONPreservedInEventLog(t *testing.T) {
	t.Parallel()

	fake := writeFakeOpenCode(t, `#!/bin/sh
cat <<'EOF'
not-json-line
{"type":"text","timestamp":1,"sessionID":"ses_m","part":{"type":"text","text":"recovered"}}
{broken
EOF
exit 0
`)

	var stdout bytes.Buffer
	res, code := Run(context.Background(), Config{
		Directory:     mustCanon(t, t.TempDir()),
		Prompt:        "p",
		Model:         DefaultModel,
		Auto:          true,
		Timeout:       time.Minute,
		OpenCode:      fake,
		LogDir:        t.TempDir(),
		MaxFinalChars: 1200,
	}, RunOptions{LockDir: t.TempDir(), Stdout: &stdout})

	require.Equal(t, 0, code)
	require.Equal(t, "recovered", res.FinalText)
	raw, err := os.ReadFile(res.EventLog)
	require.NoError(t, err)
	require.Contains(t, string(raw), "not-json-line")
	require.Contains(t, string(raw), "{broken")
}

func TestRunErrorEventAndNonzeroExit(t *testing.T) {
	t.Parallel()

	fake := writeFakeOpenCode(t, `#!/bin/sh
echo '{"type":"error","timestamp":1,"sessionID":"ses_e","error":{"name":"X","data":{"message":"boom"}}}'
exit 2
`)

	var stdout bytes.Buffer
	res, code := Run(context.Background(), Config{
		Directory:     mustCanon(t, t.TempDir()),
		Prompt:        "p",
		Model:         DefaultModel,
		Auto:          true,
		Timeout:       time.Minute,
		OpenCode:      fake,
		LogDir:        t.TempDir(),
		MaxFinalChars: 1200,
	}, RunOptions{LockDir: t.TempDir(), Stdout: &stdout})

	require.Equal(t, 1, code)
	require.Equal(t, StatusFailed, res.Status)
	require.Equal(t, "boom", res.Error)
	require.Equal(t, "ses_e", res.SessionID)
	require.NotNil(t, res.ExitCode)
	require.Equal(t, 2, *res.ExitCode)
}

func TestRunTimeoutCleansUp(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("process group tests are unix-oriented")
	}

	fake := writeFakeOpenCode(t, `#!/bin/sh
# Ignore TERM briefly to exercise escalation path; still exit on KILL.
trap '' TERM
sleep 60
`)

	var stdout bytes.Buffer
	start := time.Now()
	res, code := Run(context.Background(), Config{
		Directory:     mustCanon(t, t.TempDir()),
		Prompt:        "p",
		Model:         DefaultModel,
		Auto:          true,
		Timeout:       200 * time.Millisecond,
		OpenCode:      fake,
		LogDir:        t.TempDir(),
		MaxFinalChars: 1200,
	}, RunOptions{LockDir: t.TempDir(), Stdout: &stdout})

	require.Equal(t, 1, code)
	require.Equal(t, StatusTimedOut, res.Status)
	require.Less(t, time.Since(start), 15*time.Second)
}

func TestRunConcurrentDirectoryRejected(t *testing.T) {
	t.Parallel()

	dir := mustCanon(t, t.TempDir())
	lockDir := t.TempDir()

	// Hold the lock like a concurrent runner.
	lock, err := AcquireDirLock(lockDir, dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })

	fake := writeFakeOpenCode(t, `#!/bin/sh
echo should-not-run >&2
exit 0
`)

	var stdout bytes.Buffer
	res, code := Run(context.Background(), Config{
		Directory:     dir,
		Prompt:        "p",
		Model:         DefaultModel,
		Auto:          true,
		Timeout:       time.Minute,
		OpenCode:      fake,
		LogDir:        t.TempDir(),
		MaxFinalChars: 1200,
	}, RunOptions{LockDir: lockDir, Stdout: &stdout})

	require.Equal(t, 1, code)
	require.Equal(t, StatusFailed, res.Status)
	require.Contains(t, res.Error, "already in use")
}

func TestRunConcurrentDirectoryTwoProcesses(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("flock semantics differ")
	}

	dir := mustCanon(t, t.TempDir())
	lockDir := t.TempDir()
	logDir := t.TempDir()

	fake := writeFakeOpenCode(t, `#!/bin/sh
sleep 0.5
echo '{"type":"text","timestamp":1,"sessionID":"ses_c","part":{"type":"text","text":"ok"}}'
exit 0
`)

	cfg := Config{
		Directory:     dir,
		Prompt:        "p",
		Model:         DefaultModel,
		Auto:          true,
		Timeout:       time.Minute,
		OpenCode:      fake,
		LogDir:        logDir,
		MaxFinalChars: 1200,
	}

	var (
		wg                  sync.WaitGroup
		mu                  sync.Mutex
		completed, rejected int
	)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			res, _ := Run(context.Background(), cfg, RunOptions{
				LockDir: lockDir,
				Stdout:  &buf,
			})
			mu.Lock()
			defer mu.Unlock()
			switch res.Status {
			case StatusCompleted:
				completed++
			case StatusFailed:
				if strings.Contains(res.Error, "already in use") {
					rejected++
				}
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, completed)
	require.Equal(t, 1, rejected)
}

func TestStaleLockRecovered(t *testing.T) {
	t.Parallel()

	dir := mustCanon(t, t.TempDir())
	lockDir := t.TempDir()

	// Create a lock file with a dead pid but no live flock holder.
	name := lockFileName(dir)
	path := filepath.Join(lockDir, name)
	require.NoError(t, os.MkdirAll(lockDir, 0o700))
	require.NoError(t, os.WriteFile(path, []byte("999999\n"+dir+"\n0\n"), 0o600))

	lock, err := AcquireDirLock(lockDir, dir)
	require.NoError(t, err)
	require.NoError(t, lock.Release())
}

func writeFakeOpenCode(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-opencode")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
	return path
}

func mustCanon(t *testing.T, dir string) string {
	t.Helper()
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)
	canon, err := filepath.EvalSymlinks(abs)
	require.NoError(t, err)
	return canon
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
