// Package workspace owns isolated Git worktrees used by delegated sessions.
package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultOutputLimit     = 20_000
	maxSessionIDLength     = 256
	maxVerificationChecks  = 20
	maxVerificationHistory = 100
)

type Mode string

const (
	ModeAuto     Mode = "auto"
	ModeWorktree Mode = "worktree"
	ModeNone     Mode = "none"
)

func ParseMode(value string) (Mode, error) {
	switch mode := Mode(strings.ToLower(strings.TrimSpace(value))); mode {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeWorktree, ModeNone:
		return mode, nil
	default:
		return "", fmt.Errorf("unknown isolation mode %q: expected auto, worktree, or none", value)
	}
}

type VerificationCheck struct {
	Name    string   `json:"name,omitempty"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

type VerificationResult struct {
	Name       string    `json:"name,omitempty"`
	Command    string    `json:"command"`
	Args       []string  `json:"args,omitempty"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	Output     string    `json:"output,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	RecordedAt time.Time `json:"recorded_at"`
}

type Record struct {
	SessionID       string               `json:"session_id"`
	Mode            Mode                 `json:"mode"`
	SourceDirectory string               `json:"source_directory,omitempty"`
	Directory       string               `json:"directory,omitempty"`
	RepositoryRoot  string               `json:"repository_root,omitempty"`
	WorktreeRoot    string               `json:"worktree_root,omitempty"`
	WorktreeID      string               `json:"worktree_id,omitempty"`
	Branch          string               `json:"branch,omitempty"`
	BaseCommit      string               `json:"base_commit,omitempty"`
	CreatedAt       time.Time            `json:"created_at"`
	CleanedAt       time.Time            `json:"cleaned_at,omitempty"`
	Verification    []VerificationResult `json:"verification,omitempty"`
}

type ChangedFile struct {
	Status       string `json:"status"`
	Path         string `json:"path"`
	OriginalPath string `json:"original_path,omitempty"`
}

type Commit struct {
	Hash      string `json:"hash"`
	AuthorISO string `json:"author_date"`
	Subject   string `json:"subject"`
}

type Report struct {
	Record
	Available         bool          `json:"available"`
	HeadCommit        string        `json:"head_commit,omitempty"`
	Dirty             bool          `json:"dirty,omitempty"`
	HasChanges        bool          `json:"has_changes,omitempty"`
	ChangedFiles      []ChangedFile `json:"changed_files,omitempty"`
	Commits           []Commit      `json:"commits,omitempty"`
	DiffStat          string        `json:"diff_stat,omitempty"`
	ChangedFileCount  int           `json:"changed_file_count,omitempty"`
	CommitCount       int           `json:"commit_count,omitempty"`
	VerificationCount int           `json:"verification_count,omitempty"`
}

type Diff struct {
	SessionID  string `json:"session_id"`
	BaseCommit string `json:"base_commit,omitempty"`
	HeadCommit string `json:"head_commit,omitempty"`
	Text       string `json:"text,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type CleanupResult struct {
	Record  Record `json:"workspace"`
	Removed bool   `json:"removed"`
}

type Options struct {
	Logger           *slog.Logger
	StateDir         string
	WorktreeDir      string
	DefaultDirectory string
	DefaultMode      Mode
}

type Manager struct {
	logger           *slog.Logger
	stateDir         string
	worktreeDir      string
	defaultDirectory string
	defaultMode      Mode

	mu      sync.Mutex
	records map[string]Record
}

func NewManager(opts Options) (*Manager, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	mode, err := ParseMode(string(opts.DefaultMode))
	if err != nil {
		return nil, err
	}
	manager := &Manager{
		logger:           opts.Logger,
		stateDir:         strings.TrimSpace(opts.StateDir),
		worktreeDir:      strings.TrimSpace(opts.WorktreeDir),
		defaultDirectory: strings.TrimSpace(opts.DefaultDirectory),
		defaultMode:      mode,
		records:          make(map[string]Record),
	}
	if manager.stateDir != "" {
		if err := os.MkdirAll(manager.stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("create workspace state directory: %w", err)
		}
		manager.load()
	}
	if manager.worktreeDir != "" {
		absolute, err := filepath.Abs(manager.worktreeDir)
		if err != nil {
			return nil, fmt.Errorf("resolve worktree directory: %w", err)
		}
		manager.worktreeDir = absolute
	}
	return manager, nil
}

// Open selects or creates a workspace, invokes opener inside it, then binds
// the resulting session ID to that workspace. A failed opener rolls back a
// newly-created worktree.
func (m *Manager) Open(ctx context.Context, directory string, requested Mode, opener func(context.Context, string) (string, error)) (Record, error) {
	if opener == nil {
		return Record{}, errors.New("workspace opener is required")
	}
	mode, err := m.mode(requested)
	if err != nil {
		return Record{}, err
	}
	source, err := m.sourceDirectory(directory)
	if err != nil {
		return Record{}, err
	}

	record, created, err := m.prepare(ctx, source, mode)
	if err != nil {
		return Record{}, err
	}
	sessionID, openErr := opener(ctx, record.Directory)
	if openErr != nil {
		if created {
			m.rollback(record)
		}
		return Record{}, openErr
	}
	if err := validateSessionID(sessionID); err != nil {
		if created {
			m.rollback(record)
		}
		return Record{}, err
	}
	record.SessionID = sessionID
	record.CreatedAt = time.Now().UTC()

	m.mu.Lock()
	m.records[sessionID] = record
	m.mu.Unlock()
	if err := m.save(record); err != nil {
		m.logger.Error("failed to persist workspace", "session_id", sessionID, "err", err)
		return record, fmt.Errorf("persist workspace for session %s: %w", sessionID, err)
	}
	return copyRecord(record), nil
}

func (m *Manager) Resolve(sessionID, fallback string) string {
	m.mu.Lock()
	record, ok := m.records[sessionID]
	m.mu.Unlock()
	if ok && record.CleanedAt.IsZero() && record.Directory != "" {
		return record.Directory
	}
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return m.defaultDirectory
}

func (m *Manager) Lookup(sessionID string) (Record, bool) {
	m.mu.Lock()
	record, ok := m.records[sessionID]
	m.mu.Unlock()
	return copyRecord(record), ok
}

func (m *Manager) Inspect(ctx context.Context, sessionID string) (Report, bool, error) {
	record, ok := m.Lookup(sessionID)
	if !ok {
		return Report{}, false, nil
	}
	report := Report{Record: record, VerificationCount: len(record.Verification)}
	if !record.CleanedAt.IsZero() || record.RepositoryRoot == "" || record.Directory == "" {
		return report, true, nil
	}
	if _, err := os.Stat(record.Directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return report, true, nil
		}
		return Report{}, true, fmt.Errorf("inspect workspace directory: %w", err)
	}
	report.Available = true

	head, err := gitOutput(ctx, record.Directory, "rev-parse", "HEAD")
	if err != nil {
		return Report{}, true, err
	}
	report.HeadCommit = strings.TrimSpace(head)
	status, err := gitBytes(ctx, record.Directory, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return Report{}, true, err
	}
	report.ChangedFiles = parseStatus(status)
	report.ChangedFileCount = len(report.ChangedFiles)
	report.Dirty = len(report.ChangedFiles) > 0

	if record.BaseCommit != "" {
		logOutput, err := gitOutput(ctx, record.Directory, "log", "--format=%H%x09%aI%x09%s", record.BaseCommit+"..HEAD")
		if err != nil {
			return Report{}, true, err
		}
		report.Commits = parseCommits(logOutput)
		report.CommitCount = len(report.Commits)
		stat, err := gitOutput(ctx, record.Directory, "diff", "--stat", "--no-ext-diff", record.BaseCommit)
		if err != nil {
			return Report{}, true, err
		}
		report.DiffStat = strings.TrimSpace(stat)
	}
	report.HasChanges = report.Dirty || report.HeadCommit != record.BaseCommit
	return report, true, nil
}

func (m *Manager) Diff(ctx context.Context, sessionID string, maxChars int) (Diff, error) {
	report, ok, err := m.Inspect(ctx, sessionID)
	if err != nil {
		return Diff{}, err
	}
	if !ok {
		return Diff{}, fmt.Errorf("no workspace tracked for session %s", sessionID)
	}
	if !report.Available || report.BaseCommit == "" {
		return Diff{}, fmt.Errorf("workspace for session %s has no available Git checkout", sessionID)
	}
	if maxChars <= 0 {
		maxChars = defaultOutputLimit
	}
	limit := maxChars + 1
	var output limitedBuffer
	output.limit = limit
	cmd := exec.CommandContext(ctx, "git", "-C", report.Directory, "diff", "--no-ext-diff", "--no-color", report.BaseCommit)
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return Diff{}, fmt.Errorf("git diff: %w: %s", err, strings.TrimSpace(output.String()))
	}
	text := output.String()
	truncated := output.truncated || len(text) > maxChars
	if len(text) > maxChars {
		text = text[:maxChars]
	}
	return Diff{
		SessionID:  sessionID,
		BaseCommit: report.BaseCommit,
		HeadCommit: report.HeadCommit,
		Text:       text,
		Truncated:  truncated,
	}, nil
}

func (m *Manager) Verify(ctx context.Context, sessionID string, checks []VerificationCheck) ([]VerificationResult, error) {
	record, ok := m.Lookup(sessionID)
	if !ok {
		return nil, fmt.Errorf("no workspace tracked for session %s", sessionID)
	}
	if !record.CleanedAt.IsZero() || record.Directory == "" {
		return nil, fmt.Errorf("workspace for session %s is not available", sessionID)
	}
	if len(checks) == 0 {
		return nil, errors.New("at least one verification check is required")
	}
	if len(checks) > maxVerificationChecks {
		return nil, fmt.Errorf("verification is limited to %d checks per call", maxVerificationChecks)
	}

	results := make([]VerificationResult, 0, len(checks))
	for _, check := range checks {
		if strings.TrimSpace(check.Command) == "" {
			return nil, errors.New("verification command is required")
		}
		result := runVerification(ctx, record.Directory, check)
		results = append(results, result)

		m.mu.Lock()
		current := m.records[sessionID]
		current.Verification = append(current.Verification, result)
		if len(current.Verification) > maxVerificationHistory {
			current.Verification = append([]VerificationResult(nil), current.Verification[len(current.Verification)-maxVerificationHistory:]...)
		}
		m.records[sessionID] = current
		m.mu.Unlock()
		if err := m.save(current); err != nil {
			return results, fmt.Errorf("persist verification result: %w", err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return results, nil
}

func (m *Manager) Cleanup(ctx context.Context, sessionID string, force bool) (CleanupResult, error) {
	report, ok, err := m.Inspect(ctx, sessionID)
	if err != nil {
		return CleanupResult{}, err
	}
	if !ok {
		return CleanupResult{}, fmt.Errorf("no workspace tracked for session %s", sessionID)
	}
	if report.Mode != ModeWorktree {
		return CleanupResult{Record: report.Record}, errors.New("workspace is not an executor-owned worktree")
	}
	if !report.CleanedAt.IsZero() {
		return CleanupResult{Record: report.Record}, nil
	}
	if report.HasChanges && !force {
		return CleanupResult{}, errors.New("workspace has uncommitted changes or commits; pass force=true to discard them")
	}
	if err := m.validateOwnedPath(report.WorktreeRoot); err != nil {
		return CleanupResult{}, err
	}
	args := []string{"-C", report.RepositoryRoot, "worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, report.WorktreeRoot)
	if output, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
		return CleanupResult{}, fmt.Errorf("remove Git worktree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if report.Branch != "" {
		branchArgs := []string{"-C", report.RepositoryRoot, "branch", "-d", report.Branch}
		if force {
			branchArgs[3] = "-D"
		}
		if output, err := exec.CommandContext(ctx, "git", branchArgs...).CombinedOutput(); err != nil {
			return CleanupResult{}, fmt.Errorf("remove Git branch: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}

	record := report.Record
	record.CleanedAt = time.Now().UTC()
	m.mu.Lock()
	m.records[sessionID] = record
	m.mu.Unlock()
	if err := m.save(record); err != nil {
		return CleanupResult{}, err
	}
	return CleanupResult{Record: copyRecord(record), Removed: true}, nil
}

func (m *Manager) mode(requested Mode) (Mode, error) {
	if requested == "" {
		return m.defaultMode, nil
	}
	return ParseMode(string(requested))
}

func (m *Manager) sourceDirectory(directory string) (string, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		directory = m.defaultDirectory
	}
	if directory == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve source directory: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect source directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source path %q is not a directory", absolute)
	}
	return absolute, nil
}

func (m *Manager) prepare(ctx context.Context, source string, mode Mode) (Record, bool, error) {
	if mode == ModeNone {
		return m.directRecord(ctx, source), false, nil
	}
	if source == "" {
		if mode == ModeAuto {
			return Record{Mode: ModeNone}, false, nil
		}
		return Record{}, false, errors.New("worktree isolation requires a project directory")
	}
	repoRoot, err := gitOutput(ctx, source, "rev-parse", "--show-toplevel")
	if err != nil {
		if mode == ModeAuto {
			return Record{Mode: ModeNone, SourceDirectory: source, Directory: source}, false, nil
		}
		return Record{}, false, fmt.Errorf("worktree isolation requires a Git repository: %w", err)
	}
	repoRoot = strings.TrimSpace(repoRoot)
	status, err := gitBytes(ctx, repoRoot, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return Record{}, false, fmt.Errorf("inspect Git source state: %w", err)
	}
	if len(status) != 0 {
		if mode == ModeAuto {
			return m.directRecord(ctx, source), false, nil
		}
		return Record{}, false, errors.New("worktree isolation requires a clean Git source; commit or stash changes, or use isolation mode none")
	}
	base, err := gitOutput(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return Record{}, false, fmt.Errorf("resolve Git base commit: %w", err)
	}
	if m.worktreeDir == "" {
		return Record{}, false, errors.New("worktree directory is not configured")
	}
	id, err := randomID()
	if err != nil {
		return Record{}, false, err
	}
	repoSum := sha256.Sum256([]byte(repoRoot))
	repoID := hex.EncodeToString(repoSum[:6])
	worktreeRoot := filepath.Join(m.worktreeDir, repoID, id)
	relativeSource, err := filepath.Rel(repoRoot, source)
	if err != nil {
		return Record{}, false, fmt.Errorf("resolve source path within repository: %w", err)
	}
	directory := filepath.Join(worktreeRoot, relativeSource)
	branch := "codex-opencode-executor/" + id
	if err := os.MkdirAll(filepath.Dir(worktreeRoot), 0o700); err != nil {
		return Record{}, false, fmt.Errorf("create worktree parent: %w", err)
	}
	output, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "add", "-b", branch, worktreeRoot, strings.TrimSpace(base)).CombinedOutput()
	if err != nil {
		return Record{}, false, fmt.Errorf("create Git worktree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return Record{
		Mode:            ModeWorktree,
		SourceDirectory: source,
		Directory:       directory,
		RepositoryRoot:  repoRoot,
		WorktreeRoot:    worktreeRoot,
		WorktreeID:      id,
		Branch:          branch,
		BaseCommit:      strings.TrimSpace(base),
	}, true, nil
}

func (m *Manager) directRecord(ctx context.Context, source string) Record {
	record := Record{Mode: ModeNone, SourceDirectory: source, Directory: source}
	if source == "" {
		return record
	}
	root, err := gitOutput(ctx, source, "rev-parse", "--show-toplevel")
	if err != nil {
		return record
	}
	record.RepositoryRoot = strings.TrimSpace(root)
	base, err := gitOutput(ctx, source, "rev-parse", "HEAD")
	if err == nil {
		record.BaseCommit = strings.TrimSpace(base)
	}
	return record
}

func (m *Manager) rollback(record Record) {
	if record.Mode != ModeWorktree || m.validateOwnedPath(record.WorktreeRoot) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, removeErr := exec.CommandContext(ctx, "git", "-C", record.RepositoryRoot, "worktree", "remove", "--force", record.WorktreeRoot).CombinedOutput()
	_, branchErr := exec.CommandContext(ctx, "git", "-C", record.RepositoryRoot, "branch", "-D", record.Branch).CombinedOutput()
	if removeErr != nil || branchErr != nil {
		m.logger.Warn("failed to roll back worktree", "directory", record.WorktreeRoot, "remove_error", removeErr, "branch_error", branchErr)
	}
}

func (m *Manager) validateOwnedPath(path string) error {
	if m.worktreeDir == "" {
		return errors.New("worktree directory is not configured")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(m.worktreeDir, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to remove non-owned worktree path %q", path)
	}
	return nil
}

func (m *Manager) load() {
	entries, err := os.ReadDir(m.stateDir)
	if err != nil {
		m.logger.Warn("failed to read workspace state", "dir", m.stateDir, "err", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.stateDir, entry.Name()))
		if err != nil {
			m.logger.Warn("failed to read workspace record", "file", entry.Name(), "err", err)
			continue
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil || validateSessionID(record.SessionID) != nil {
			m.logger.Warn("failed to parse workspace record", "file", entry.Name(), "err", err)
			continue
		}
		m.records[record.SessionID] = record
	}
}

func (m *Manager) save(record Record) error {
	if m.stateDir == "" {
		return nil
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.stateDir, ".workspace-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	path := filepath.Join(m.stateDir, record.SessionID+".json")
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func runVerification(ctx context.Context, directory string, check VerificationCheck) VerificationResult {
	started := time.Now()
	var output limitedBuffer
	output.limit = defaultOutputLimit + 1
	cmd := exec.CommandContext(ctx, check.Command, check.Args...)
	cmd.Dir = directory
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	ended := time.Now()
	result := VerificationResult{
		Name:       check.Name,
		Command:    check.Command,
		Args:       append([]string(nil), check.Args...),
		Status:     "passed",
		ExitCode:   0,
		Output:     output.String(),
		Truncated:  output.truncated,
		StartedAt:  started.UTC(),
		DurationMS: ended.Sub(started).Milliseconds(),
		RecordedAt: ended.UTC(),
	}
	if len(result.Output) > defaultOutputLimit {
		result.Output = result.Output[:defaultOutputLimit]
		result.Truncated = true
	}
	if err == nil {
		return result
	}
	result.ExitCode = -1
	result.Status = "error"
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		result.Status = "failed"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Status = "timed_out"
	}
	if result.Output == "" {
		result.Output = err.Error()
	}
	return result
}

func gitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	output, err := gitBytes(ctx, directory, args...)
	return string(output), err
}

func gitBytes(ctx context.Context, directory string, args ...string) ([]byte, error) {
	argv := append([]string{"-C", directory}, args...)
	output, err := exec.CommandContext(ctx, "git", argv...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func parseStatus(data []byte) []ChangedFile {
	fields := bytes.Split(data, []byte{0})
	files := make([]ChangedFile, 0, len(fields))
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if len(field) < 4 {
			continue
		}
		status := string(field[:2])
		changed := ChangedFile{Status: status, Path: string(field[3:])}
		if status[0] == 'R' || status[0] == 'C' || status[1] == 'R' || status[1] == 'C' {
			if index+1 < len(fields) {
				index++
				changed.OriginalPath = string(fields[index])
			}
		}
		files = append(files, changed)
	}
	return files
}

func parseCommits(output string) []Commit {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	commits := make([]Commit, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		commits = append(commits, Commit{Hash: parts[0], AuthorISO: parts[1], Subject: parts[2]})
	}
	return commits
}

func randomID() (string, error) {
	data := make([]byte, 8)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate worktree id: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func validateSessionID(sessionID string) error {
	if sessionID == "" || len(sessionID) > maxSessionIDLength || filepath.Base(sessionID) != sessionID || strings.ContainsAny(sessionID, `/\\`) {
		return fmt.Errorf("invalid session id %q", sessionID)
	}
	return nil
}

func copyRecord(record Record) Record {
	record.Verification = append([]VerificationResult(nil), record.Verification...)
	for index := range record.Verification {
		record.Verification[index].Args = append([]string(nil), record.Verification[index].Args...)
	}
	return record
}

type limitedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, data[:min(remaining, len(data))]...)
	}
	if len(data) > remaining {
		b.truncated = true
	}
	return len(data), nil
}

func (b *limitedBuffer) String() string {
	return string(b.data)
}
