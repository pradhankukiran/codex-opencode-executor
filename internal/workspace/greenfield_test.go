package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenMissingDirectoryWithoutCreateFails(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "missing-project")
	manager, err := NewManager(Options{StateDir: t.TempDir(), DefaultMode: ModeAuto})
	require.NoError(t, err)

	_, err = manager.Open(t.Context(), OpenOptions{Directory: target}, func(context.Context, string) (string, error) {
		t.Fatal("opener must not run when source is missing")
		return "", nil
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "inspect source directory")
	require.NoDirExists(t, target)
}

func TestOpenCreateDirectoryBindsExactTarget(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "greenfield-app")
	manager, err := NewManager(Options{StateDir: t.TempDir(), DefaultMode: ModeAuto})
	require.NoError(t, err)

	var opened string
	record, err := manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(_ context.Context, directory string) (string, error) {
		opened = directory
		return "session-greenfield", nil
	})
	require.NoError(t, err)
	require.Equal(t, ModeGreenfield, record.Mode)
	require.True(t, record.Owned)
	require.Equal(t, target, opened)
	require.Equal(t, target, record.Directory)
	require.Equal(t, target, record.SourceDirectory)
	require.DirExists(t, target)
	require.Equal(t, target, manager.Resolve("session-greenfield", parent))
}

func TestOpenCreateDirectoryRefusesExistingPath(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "already-there")
	require.NoError(t, os.Mkdir(target, 0o700))

	manager, err := NewManager(Options{DefaultMode: ModeNone})
	require.NoError(t, err)
	_, err = manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(context.Context, string) (string, error) {
		return "session-collision", nil
	})
	require.ErrorContains(t, err, "already exists")
}

func TestOpenCreateDirectoryRefusesFinalComponentSymlink(t *testing.T) {
	parent := t.TempDir()
	realDir := filepath.Join(parent, "real")
	require.NoError(t, os.Mkdir(realDir, 0o700))
	target := filepath.Join(parent, "link-target")
	require.NoError(t, os.Symlink(realDir, target))

	manager, err := NewManager(Options{DefaultMode: ModeNone})
	require.NoError(t, err)
	_, err = manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(context.Context, string) (string, error) {
		return "session-symlink", nil
	})
	require.ErrorContains(t, err, "already exists")
}

func TestOpenCreateDirectoryRequiresExplicitDirectory(t *testing.T) {
	manager, err := NewManager(Options{DefaultDirectory: t.TempDir(), DefaultMode: ModeNone})
	require.NoError(t, err)
	_, err = manager.Open(t.Context(), OpenOptions{CreateDirectory: true}, func(context.Context, string) (string, error) {
		return "session-default", nil
	})
	require.ErrorContains(t, err, "explicit directory")
}

func TestOpenCreateDirectoryRollsBackOpenerFailure(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "rollback-open")
	manager, err := NewManager(Options{DefaultMode: ModeNone})
	require.NoError(t, err)

	_, err = manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(context.Context, string) (string, error) {
		require.DirExists(t, target)
		return "", errors.New("opener failed")
	})
	require.EqualError(t, err, "opener failed")
	require.NoDirExists(t, target)
}

func TestOpenCreateDirectoryRollsBackInvalidSessionID(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "rollback-session")
	manager, err := NewManager(Options{DefaultMode: ModeNone})
	require.NoError(t, err)

	_, err = manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(context.Context, string) (string, error) {
		return "../bad", nil
	})
	require.ErrorContains(t, err, "invalid session id")
	require.NoDirExists(t, target)
}

func TestGreenfieldInspectBeforeAndAfterGitInit(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "inspect-me")
	stateDir := t.TempDir()
	manager, err := NewManager(Options{StateDir: stateDir, DefaultMode: ModeNone})
	require.NoError(t, err)

	_, err = manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(context.Context, string) (string, error) {
		return "session-inspect", nil
	})
	require.NoError(t, err)

	before, ok, err := manager.Inspect(t.Context(), "session-inspect")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, before.Available)
	require.Equal(t, target, before.Directory)
	require.Equal(t, ModeGreenfield, before.Mode)
	require.Empty(t, before.RepositoryRoot)
	require.Empty(t, before.BaseCommit)
	require.Empty(t, before.HeadCommit)
	require.False(t, before.HasChanges)

	git(t, target, "init")
	git(t, target, "config", "user.name", "Test User")
	git(t, target, "config", "user.email", "test@example.com")
	emptyTree := emptyTreeOID(t, target)

	unborn, ok, err := manager.Inspect(t.Context(), "session-inspect")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, unborn.Available)
	require.Equal(t, target, unborn.RepositoryRoot)
	require.Empty(t, unborn.HeadCommit)
	require.Equal(t, emptyTree, unborn.BaseCommit)

	require.NoError(t, os.WriteFile(filepath.Join(target, "readme.txt"), []byte("hello\n"), 0o600))
	git(t, target, "add", "readme.txt")
	git(t, target, "commit", "-m", "initial")

	require.NoError(t, os.WriteFile(filepath.Join(target, "readme.txt"), []byte("hello world\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(target, "extra.txt"), []byte("extra\n"), 0o600))

	after, ok, err := manager.Inspect(t.Context(), "session-inspect")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, after.Available)
	require.Equal(t, target, after.RepositoryRoot)
	require.Equal(t, emptyTree, after.BaseCommit)
	require.NotEmpty(t, after.HeadCommit)
	require.True(t, after.Dirty)
	require.True(t, after.HasChanges)
	require.Equal(t, 1, after.CommitCount)
	require.Equal(t, "initial", after.Commits[0].Subject)
	require.ElementsMatch(t, []ChangedFile{
		{Status: "A", Path: "readme.txt"},
		{Status: "??", Path: "extra.txt"},
	}, filterChangedNames(after.ChangedFiles))

	diff, err := manager.Diff(t.Context(), "session-inspect", 20_000)
	require.NoError(t, err)
	require.Equal(t, emptyTree, diff.BaseCommit)
	require.Contains(t, diff.Text, "readme.txt")
	require.Contains(t, diff.Text, "+hello")

	results, err := manager.Verify(t.Context(), "session-inspect", []VerificationCheck{
		{Name: "pwd-file", Command: "test", Args: []string{"-f", "readme.txt"}},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", results[0].Status)

	reloaded, err := NewManager(Options{StateDir: stateDir})
	require.NoError(t, err)
	persisted, ok := reloaded.Lookup("session-inspect")
	require.True(t, ok)
	require.Equal(t, ModeGreenfield, persisted.Mode)
	require.True(t, persisted.Owned)
	require.Equal(t, target, persisted.Directory)
	require.Len(t, persisted.Verification, 1)
}

func TestGreenfieldIgnoresAncestorRepository(t *testing.T) {
	ancestor := newRepository(t)
	parent := filepath.Join(ancestor, "nested-parent")
	require.NoError(t, os.MkdirAll(parent, 0o700))
	target := filepath.Join(parent, "child-app")

	manager, err := NewManager(Options{StateDir: t.TempDir(), DefaultMode: ModeNone})
	require.NoError(t, err)
	_, err = manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(context.Context, string) (string, error) {
		return "session-nested", nil
	})
	require.NoError(t, err)

	report, ok, err := manager.Inspect(t.Context(), "session-nested")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, report.Available)
	require.Empty(t, report.RepositoryRoot)
	require.Empty(t, report.BaseCommit)
	require.Empty(t, report.HeadCommit)
	require.Zero(t, report.CommitCount)
	require.Empty(t, report.ChangedFiles)

	git(t, target, "init")
	emptyTree := emptyTreeOID(t, target)
	after, ok, err := manager.Inspect(t.Context(), "session-nested")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, target, after.RepositoryRoot)
	require.Equal(t, emptyTree, after.BaseCommit)
	require.Empty(t, after.HeadCommit)
}

func TestGreenfieldUnbornStagedDiff(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "staged-app")
	manager, err := NewManager(Options{StateDir: t.TempDir(), DefaultMode: ModeNone})
	require.NoError(t, err)
	_, err = manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(context.Context, string) (string, error) {
		return "session-staged", nil
	})
	require.NoError(t, err)

	git(t, target, "init")
	require.NoError(t, os.WriteFile(filepath.Join(target, "staged.txt"), []byte("staged body\n"), 0o600))
	git(t, target, "add", "staged.txt")
	require.NoError(t, os.WriteFile(filepath.Join(target, "loose.txt"), []byte("loose\n"), 0o600))
	emptyTree := emptyTreeOID(t, target)

	report, ok, err := manager.Inspect(t.Context(), "session-staged")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, report.Available)
	require.Empty(t, report.HeadCommit)
	require.Equal(t, emptyTree, report.BaseCommit)
	require.True(t, report.Dirty)
	require.True(t, report.HasChanges)
	require.ElementsMatch(t, []ChangedFile{
		{Status: "A", Path: "staged.txt"},
		{Status: "??", Path: "loose.txt"},
	}, filterChangedNames(report.ChangedFiles))

	diff, err := manager.Diff(t.Context(), "session-staged", 20_000)
	require.NoError(t, err)
	require.Equal(t, emptyTree, diff.BaseCommit)
	require.Empty(t, diff.HeadCommit)
	require.Contains(t, diff.Text, "staged.txt")
	require.Contains(t, diff.Text, "+staged body")
	require.NotContains(t, diff.Text, "loose.txt")
}

func TestGreenfieldSHA256EmptyTree(t *testing.T) {
	if !supportsSHA256ObjectFormat(t) {
		t.Skip("git init --object-format=sha256 is unsupported")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "sha256-app")
	manager, err := NewManager(Options{StateDir: t.TempDir(), DefaultMode: ModeNone})
	require.NoError(t, err)
	_, err = manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(context.Context, string) (string, error) {
		return "session-sha256", nil
	})
	require.NoError(t, err)

	git(t, target, "init", "--object-format=sha256")
	require.NoError(t, os.WriteFile(filepath.Join(target, "note.txt"), []byte("sha256\n"), 0o600))
	git(t, target, "add", "note.txt")
	emptyTree := emptyTreeOID(t, target)
	require.Len(t, emptyTree, 64)

	report, ok, err := manager.Inspect(t.Context(), "session-sha256")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, emptyTree, report.BaseCommit)
	require.Equal(t, target, report.RepositoryRoot)

	diff, err := manager.Diff(t.Context(), "session-sha256", 20_000)
	require.NoError(t, err)
	require.Equal(t, emptyTree, diff.BaseCommit)
	require.Contains(t, diff.Text, "+sha256")
}

func TestOpenCreateDirectoryRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real-parent")
	require.NoError(t, os.Mkdir(realParent, 0o700))
	linkParent := filepath.Join(root, "link-parent")
	require.NoError(t, os.Symlink(realParent, linkParent))
	target := filepath.Join(linkParent, "child")

	manager, err := NewManager(Options{DefaultMode: ModeNone})
	require.NoError(t, err)
	_, err = manager.Open(t.Context(), OpenOptions{
		Directory:       target,
		CreateDirectory: true,
	}, func(context.Context, string) (string, error) {
		t.Fatal("opener must not run for symlink parent")
		return "", nil
	})
	require.ErrorContains(t, err, "symlink")
	require.NoDirExists(t, filepath.Join(realParent, "child"))
}

func TestGreenfieldCleanupRefusesMismatchedOwnedPaths(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "owned")
	require.NoError(t, os.Mkdir(target, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep\n"), 0o600))

	manager, err := NewManager(Options{StateDir: t.TempDir(), DefaultMode: ModeNone})
	require.NoError(t, err)
	manager.mu.Lock()
	manager.records["session-bad-paths"] = Record{
		SessionID:       "session-bad-paths",
		Mode:            ModeGreenfield,
		Owned:           true,
		SourceDirectory: target,
		Directory:       filepath.Join(parent, "other-path"),
		CreatedAt:       time.Now().UTC(),
	}
	manager.mu.Unlock()

	_, err = manager.Cleanup(t.Context(), "session-bad-paths", true)
	require.Error(t, err)
	require.DirExists(t, target)
	require.FileExists(t, filepath.Join(target, "keep.txt"))
}

func TestGreenfieldCleanupRules(t *testing.T) {
	parent := t.TempDir()
	stateDir := t.TempDir()

	t.Run("empty without force", func(t *testing.T) {
		target := filepath.Join(parent, "clean-empty")
		manager, err := NewManager(Options{StateDir: stateDir, DefaultMode: ModeNone})
		require.NoError(t, err)
		_, err = manager.Open(t.Context(), OpenOptions{
			Directory:       target,
			CreateDirectory: true,
		}, func(context.Context, string) (string, error) {
			return "session-empty-clean", nil
		})
		require.NoError(t, err)

		cleanup, err := manager.Cleanup(t.Context(), "session-empty-clean", false)
		require.NoError(t, err)
		require.True(t, cleanup.Removed)
		require.NoDirExists(t, target)

		again, err := manager.Cleanup(t.Context(), "session-empty-clean", false)
		require.NoError(t, err)
		require.False(t, again.Removed)
		require.NotZero(t, again.Record.CleanedAt)
	})

	t.Run("refuses non-empty without force", func(t *testing.T) {
		target := filepath.Join(parent, "dirty-greenfield")
		manager, err := NewManager(Options{StateDir: filepath.Join(stateDir, "dirty"), DefaultMode: ModeNone})
		require.NoError(t, err)
		_, err = manager.Open(t.Context(), OpenOptions{
			Directory:       target,
			CreateDirectory: true,
		}, func(context.Context, string) (string, error) {
			return "session-dirty-clean", nil
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(target, "keep.txt"), []byte("x\n"), 0o600))

		_, err = manager.Cleanup(t.Context(), "session-dirty-clean", false)
		require.ErrorContains(t, err, "force=true")
		require.DirExists(t, target)

		cleanup, err := manager.Cleanup(t.Context(), "session-dirty-clean", true)
		require.NoError(t, err)
		require.True(t, cleanup.Removed)
		require.NoDirExists(t, target)

		again, err := manager.Cleanup(t.Context(), "session-dirty-clean", true)
		require.NoError(t, err)
		require.False(t, again.Removed)
	})

	t.Run("refuses commits without force", func(t *testing.T) {
		target := filepath.Join(parent, "committed-greenfield")
		manager, err := NewManager(Options{StateDir: filepath.Join(stateDir, "commits"), DefaultMode: ModeNone})
		require.NoError(t, err)
		_, err = manager.Open(t.Context(), OpenOptions{
			Directory:       target,
			CreateDirectory: true,
		}, func(context.Context, string) (string, error) {
			return "session-commit-clean", nil
		})
		require.NoError(t, err)
		git(t, target, "init")
		git(t, target, "config", "user.name", "Test User")
		git(t, target, "config", "user.email", "test@example.com")
		require.NoError(t, os.WriteFile(filepath.Join(target, "a.txt"), []byte("a\n"), 0o600))
		git(t, target, "add", "a.txt")
		git(t, target, "commit", "-m", "commit")

		_, err = manager.Cleanup(t.Context(), "session-commit-clean", false)
		require.ErrorContains(t, err, "force=true")
		cleanup, err := manager.Cleanup(t.Context(), "session-commit-clean", true)
		require.NoError(t, err)
		require.True(t, cleanup.Removed)
		require.NoDirExists(t, target)
	})
}

func filterChangedNames(files []ChangedFile) []ChangedFile {
	out := make([]ChangedFile, 0, len(files))
	for _, file := range files {
		status := file.Status
		if len(status) > 1 && status != "??" {
			// name-status uses single-letter codes; normalize modified working tree " M" etc.
			status = string(status[0])
			if status == " " {
				status = string(file.Status[1])
			}
		}
		out = append(out, ChangedFile{Status: status, Path: file.Path})
	}
	return out
}

func emptyTreeOID(t *testing.T, directory string) string {
	t.Helper()
	output, err := exec.Command("git", "-C", directory, "hash-object", "-t", "tree", "/dev/null").CombinedOutput()
	require.NoError(t, err, string(output))
	return strings.TrimSpace(string(output))
}

func supportsSHA256ObjectFormat(t *testing.T) bool {
	t.Helper()
	directory := t.TempDir()
	output, err := exec.Command("git", "-C", directory, "init", "--object-format=sha256").CombinedOutput()
	return err == nil && !strings.Contains(strings.ToLower(string(output)), "unknown")
}
