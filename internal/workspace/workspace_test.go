package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManagerWorktreeLifecycle(t *testing.T) {
	repository := newRepository(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	worktreeDir := filepath.Join(t.TempDir(), "worktrees")
	manager, err := NewManager(Options{
		StateDir:    stateDir,
		WorktreeDir: worktreeDir,
		DefaultMode: ModeAuto,
	})
	require.NoError(t, err)

	var openedDirectory string
	record, err := manager.Open(t.Context(), OpenOptions{Directory: repository}, func(_ context.Context, directory string) (string, error) {
		openedDirectory = directory
		return "session-1", nil
	})
	require.NoError(t, err)
	require.Equal(t, ModeWorktree, record.Mode)
	require.NotEqual(t, repository, openedDirectory)
	require.Equal(t, openedDirectory, manager.Resolve("session-1", repository))
	require.FileExists(t, filepath.Join(openedDirectory, "tracked.txt"))

	require.NoError(t, os.WriteFile(filepath.Join(openedDirectory, "tracked.txt"), []byte("changed\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(openedDirectory, "new.txt"), []byte("new\n"), 0o600))
	report, ok, err := manager.Inspect(t.Context(), "session-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, report.Available)
	require.True(t, report.Dirty)
	require.True(t, report.HasChanges)
	require.Equal(t, 2, report.ChangedFileCount)
	require.ElementsMatch(t, []ChangedFile{
		{Status: "M", Path: "tracked.txt"},
		{Status: "??", Path: "new.txt"},
	}, report.ChangedFiles)
	require.Contains(t, report.DiffStat, "tracked.txt")

	diff, err := manager.Diff(t.Context(), "session-1", 10_000)
	require.NoError(t, err)
	require.Contains(t, diff.Text, "+changed")
	require.False(t, diff.Truncated)

	results, err := manager.Verify(t.Context(), "session-1", []VerificationCheck{
		{Name: "success", Command: "git", Args: []string{"status", "--short"}},
		{Name: "failure", Command: "git", Args: []string{"rev-parse", "missing-ref"}},
	})
	require.NoError(t, err)
	require.Equal(t, "passed", results[0].Status)
	require.Equal(t, "failed", results[1].Status)
	require.NotZero(t, results[1].ExitCode)

	reloaded, err := NewManager(Options{StateDir: stateDir, WorktreeDir: worktreeDir})
	require.NoError(t, err)
	persisted, ok := reloaded.Lookup("session-1")
	require.True(t, ok)
	require.Len(t, persisted.Verification, 2)
	persistedReport, ok, err := reloaded.Inspect(t.Context(), "session-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, persistedReport.VerificationCount)

	_, err = reloaded.Cleanup(t.Context(), "session-1", false)
	require.EqualError(t, err, "workspace has uncommitted changes or commits; pass force=true to discard them")
	cleanup, err := reloaded.Cleanup(t.Context(), "session-1", true)
	require.NoError(t, err)
	require.True(t, cleanup.Removed)
	require.NotZero(t, cleanup.Record.CleanedAt)
	require.NoDirExists(t, openedDirectory)
	require.Equal(t, repository, reloaded.Resolve("session-1", repository))
}

func TestManagerTracksCommits(t *testing.T) {
	repository := newRepository(t)
	manager, err := NewManager(Options{WorktreeDir: t.TempDir(), DefaultMode: ModeWorktree})
	require.NoError(t, err)
	record, err := manager.Open(t.Context(), OpenOptions{Directory: repository}, func(_ context.Context, _ string) (string, error) {
		return "session-commit", nil
	})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(record.Directory, "tracked.txt"), []byte("committed\n"), 0o600))
	git(t, record.Directory, "add", "tracked.txt")
	git(t, record.Directory, "commit", "-m", "executor change")
	report, ok, err := manager.Inspect(t.Context(), "session-commit")
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, report.Dirty)
	require.True(t, report.HasChanges)
	require.Len(t, report.Commits, 1)
	require.Equal(t, 1, report.CommitCount)
	require.Equal(t, []ChangedFile{{Status: "M", Path: "tracked.txt"}}, report.ChangedFiles)
	require.Equal(t, 1, report.ChangedFileCount)
	require.Equal(t, "executor change", report.Commits[0].Subject)
	require.NotEqual(t, report.BaseCommit, report.HeadCommit)
}

func TestManagerAutoUsesNonGitDirectoryDirectly(t *testing.T) {
	directory := t.TempDir()
	manager, err := NewManager(Options{WorktreeDir: t.TempDir(), DefaultMode: ModeAuto})
	require.NoError(t, err)
	record, err := manager.Open(t.Context(), OpenOptions{Directory: directory}, func(_ context.Context, opened string) (string, error) {
		require.Equal(t, directory, opened)
		return "session-direct", nil
	})
	require.NoError(t, err)
	require.Equal(t, ModeNone, record.Mode)
}

func TestManagerAutoUsesDirtyRepositoryDirectly(t *testing.T) {
	repository := newRepository(t)
	require.NoError(t, os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("dirty\n"), 0o600))
	manager, err := NewManager(Options{WorktreeDir: t.TempDir(), DefaultMode: ModeAuto})
	require.NoError(t, err)
	record, err := manager.Open(t.Context(), OpenOptions{Directory: repository}, func(_ context.Context, opened string) (string, error) {
		require.Equal(t, repository, opened)
		return "session-dirty-auto", nil
	})
	require.NoError(t, err)
	require.Equal(t, ModeNone, record.Mode)

	_, err = manager.Open(t.Context(), OpenOptions{Directory: repository, Mode: ModeWorktree}, func(context.Context, string) (string, error) {
		return "session-dirty-explicit", nil
	})
	require.EqualError(t, err, "worktree isolation requires a clean Git source; commit or stash changes, or use isolation mode none")
}

func TestManagerPreservesRepositorySubdirectory(t *testing.T) {
	repository := newRepository(t)
	subdirectory := filepath.Join(repository, "nested", "project")
	require.NoError(t, os.MkdirAll(subdirectory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(subdirectory, "file.txt"), []byte("nested\n"), 0o600))
	git(t, repository, "add", "nested/project/file.txt")
	git(t, repository, "commit", "-m", "add nested project")
	manager, err := NewManager(Options{WorktreeDir: t.TempDir(), DefaultMode: ModeWorktree})
	require.NoError(t, err)
	record, err := manager.Open(t.Context(), OpenOptions{Directory: subdirectory}, func(_ context.Context, opened string) (string, error) {
		require.Equal(t, filepath.Join(filepath.Dir(filepath.Dir(opened)), "nested", "project"), opened)
		return "session-subdirectory", nil
	})
	require.NoError(t, err)
	require.NotEqual(t, record.Directory, record.WorktreeRoot)
	require.FileExists(t, filepath.Join(record.Directory, "file.txt"))
}

func TestManagerRollsBackFailedOpen(t *testing.T) {
	repository := newRepository(t)
	worktreeDir := t.TempDir()
	manager, err := NewManager(Options{WorktreeDir: worktreeDir, DefaultMode: ModeWorktree})
	require.NoError(t, err)
	var openedDirectory string
	_, err = manager.Open(t.Context(), OpenOptions{Directory: repository}, func(_ context.Context, directory string) (string, error) {
		openedDirectory = directory
		return "", errors.New("open failed")
	})
	require.EqualError(t, err, "open failed")
	require.NoDirExists(t, openedDirectory)
	worktrees := git(t, repository, "worktree", "list", "--porcelain")
	require.NotContains(t, worktrees, openedDirectory)
}

func TestManagerVerificationTimeout(t *testing.T) {
	directory := t.TempDir()
	manager, err := NewManager(Options{DefaultMode: ModeNone})
	require.NoError(t, err)
	_, err = manager.Open(t.Context(), OpenOptions{Directory: directory}, func(context.Context, string) (string, error) {
		return "session-timeout", nil
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	results, err := manager.Verify(ctx, "session-timeout", []VerificationCheck{{Command: "sleep", Args: []string{"5"}}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "timed_out", results[0].Status)
}

func newRepository(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	git(t, directory, "init")
	git(t, directory, "config", "user.name", "Test User")
	git(t, directory, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(directory, "tracked.txt"), []byte("base\n"), 0o600))
	git(t, directory, "add", "tracked.txt")
	git(t, directory, "commit", "-m", "base")
	return directory
}

func git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	argv := append([]string{"-C", directory}, args...)
	output, err := exec.Command("git", argv...).CombinedOutput()
	require.NoError(t, err, string(output))
	return string(output)
}
