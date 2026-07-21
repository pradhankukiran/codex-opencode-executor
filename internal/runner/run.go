package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// RunOptions controls execution beyond Config.
type RunOptions struct {
	// LockDir overrides the default lock directory.
	LockDir string
	// Stdout is where the single result JSON is written (default os.Stdout).
	Stdout io.Writer
	// Stderr is for runner diagnostics only (default os.Stderr).
	Stderr io.Writer
	// Environ supplies the child environment; nil uses os.Environ().
	Environ []string
	// Now overrides time.Now for tests.
	Now func() time.Time
}

// Run executes OpenCode once and writes a single Result JSON to opts.Stdout.
// The process exit code is returned separately: 0 only when status is completed.
func Run(ctx context.Context, cfg Config, opts RunOptions) (Result, int) {
	start := time.Now()
	if opts.Now != nil {
		start = opts.Now()
	}
	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}

	res := Result{Status: StatusFailed}
	exitCode := 1

	finish := func(r Result, code int) (Result, int) {
		if opts.Now != nil {
			r.DurationMS = opts.Now().Sub(start).Milliseconds()
		} else {
			r.DurationMS = time.Since(start).Milliseconds()
		}
		if r.DurationMS < 0 {
			r.DurationMS = 0
		}
		_ = EncodeResult(out, r)
		return r, code
	}

	lockDir := opts.LockDir
	if lockDir == "" {
		var err error
		lockDir, err = DefaultLockDir()
		if err != nil {
			res.Error = "failed to resolve lock directory"
			return finish(res, 1)
		}
	}

	lock, err := AcquireDirLock(lockDir, cfg.Directory)
	if err != nil {
		res.Status = StatusFailed
		res.Error = err.Error()
		return finish(res, 1)
	}
	defer func() { _ = lock.Release() }()

	if err := os.MkdirAll(cfg.LogDir, 0o700); err != nil {
		res.Error = "failed to create log directory"
		return finish(res, 1)
	}

	runID := fmt.Sprintf("%d", start.UnixNano())
	eventLogPath := filepath.Join(cfg.LogDir, runID+".events.jsonl")
	stderrLogPath := filepath.Join(cfg.LogDir, runID+".stderr.log")
	res.EventLog = eventLogPath
	res.StderrLog = stderrLogPath

	eventFile, err := os.OpenFile(eventLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		res.Error = "failed to create event log"
		return finish(res, 1)
	}
	defer eventFile.Close()

	stderrFile, err := os.OpenFile(stderrLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		res.Error = "failed to create stderr log"
		return finish(res, 1)
	}
	defer stderrFile.Close()

	args := BuildOpenCodeArgs(cfg)
	cmd := exec.Command(cfg.OpenCode, args...) //nolint:gosec // operator-controlled binary and args
	configureChild(cmd)
	if opts.Environ != nil {
		cmd.Env = opts.Environ
	}
	cmd.Stdout = eventFile
	cmd.Stderr = stderrFile

	if err := cmd.Start(); err != nil {
		res.Error = "failed to start opencode"
		return finish(res, 1)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	var timerCh <-chan time.Time
	var timer *time.Timer
	if cfg.Timeout > 0 {
		timer = time.NewTimer(cfg.Timeout)
		timerCh = timer.C
		defer timer.Stop()
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var (
		waitErr   error
		timedOut  bool
		cancelled bool
	)

	select {
	case waitErr = <-waitDone:
	case <-sigCh:
		cancelled = true
		waitErr = stopChild(cmd, waitDone, killGrace)
	case <-timerCh:
		timedOut = true
		waitErr = stopChild(cmd, waitDone, killGrace)
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			timedOut = true
		} else {
			cancelled = true
		}
		waitErr = stopChild(cmd, waitDone, killGrace)
	}

	_ = eventFile.Sync()
	_ = stderrFile.Sync()

	eventReader, err := os.Open(eventLogPath)
	if err != nil {
		res.Error = "failed to read event log"
		return finish(res, 1)
	}
	defer eventReader.Close()

	summary := ParseEventStream(eventReader)
	res.SessionID = summary.SessionID

	final, truncated := TruncateFinalText(summary.FinalText, cfg.MaxFinalChars)
	res.FinalText = final
	res.FinalTextTruncated = truncated

	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	res.ExitCode = &code

	switch {
	case timedOut:
		res.Status = StatusTimedOut
		res.Error = "opencode timed out"
		exitCode = 1
	case cancelled:
		res.Status = StatusCancelled
		res.Error = "run cancelled"
		exitCode = 1
	case summary.HasError:
		res.Status = StatusFailed
		if summary.ErrorMsg != "" {
			res.Error = summary.ErrorMsg
		} else {
			res.Error = "opencode reported an error"
		}
		exitCode = 1
	case waitErr != nil || code != 0:
		res.Status = StatusFailed
		res.Error = "opencode exited with failure"
		exitCode = 1
	default:
		res.Status = StatusCompleted
		exitCode = 0
	}

	return finish(res, exitCode)
}
