package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxIdempotencyKeyLength = 256
	defaultRecoveryTimeout  = 30 * time.Second
	abortTimeout            = 10 * time.Second
)

type JobStatus string

const (
	JobRunning  JobStatus = "running"
	JobDone     JobStatus = "done"
	JobError    JobStatus = "error"
	JobCanceled JobStatus = "canceled"
	JobTimedOut JobStatus = "timed_out"
	// JobUnknown means the executor restarted while the job was running.
	// Recovery reconciles the persisted job with the opencode session.
	JobUnknown JobStatus = "unknown"
)

func (s JobStatus) terminal() bool {
	switch s {
	case JobDone, JobError, JobCanceled, JobTimedOut:
		return true
	default:
		return false
	}
}

type Job struct {
	SessionID       string    `json:"session_id"`
	Location        Location  `json:"location"`
	Status          JobStatus `json:"status"`
	IdempotencyKey  string    `json:"idempotency_key,omitempty"`
	RequestHash     string    `json:"request_hash,omitempty"`
	PromptMessageID string    `json:"prompt_message_id,omitempty"`
	Err             error     `json:"-"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Deadline        time.Time `json:"deadline,omitempty"`

	cancel context.CancelFunc
	mu     sync.Mutex
}

// jobRecord is the on-disk representation of a Job. Err is stored as a string
// because the error interface is not JSON-serializable.
type jobRecord struct {
	SessionID       string    `json:"session_id"`
	Location        Location  `json:"location"`
	Status          JobStatus `json:"status"`
	IdempotencyKey  string    `json:"idempotency_key,omitempty"`
	RequestHash     string    `json:"request_hash,omitempty"`
	PromptMessageID string    `json:"prompt_message_id,omitempty"`
	ErrMessage      string    `json:"err_message,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Deadline        time.Time `json:"deadline,omitempty"`
}

type SubmitOptions struct {
	IdempotencyKey string
	Timeout        time.Duration
}

type SubmitResult struct {
	Job       *Job
	Duplicate bool
}

type CancelResult struct {
	Job     *Job
	Aborted bool
}

type jobClient interface {
	Prompt(context.Context, Location, string, PromptRequest) (json.RawMessage, error)
	Abort(context.Context, Location, string) (bool, error)
	Wait(context.Context, Location, string) error
	Messages(context.Context, Location, string) (json.RawMessage, error)
}

type Manager struct {
	ctx             context.Context
	client          jobClient
	jobs            map[string]*Job
	idempotency     map[string]*Job
	mu              sync.Mutex
	logger          *slog.Logger
	stateDir        string
	defaultTimeout  time.Duration
	recoveryTimeout time.Duration
}

type ManagerOptions struct {
	Logger          *slog.Logger
	StateDir        string
	DefaultTimeout  time.Duration
	RecoveryTimeout time.Duration
}

func (opts *ManagerOptions) setDefaults() {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RecoveryTimeout <= 0 {
		opts.RecoveryTimeout = defaultRecoveryTimeout
	}
}

func NewManager(ctx context.Context, client jobClient, opts ManagerOptions) *Manager {
	opts.setDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	m := &Manager{
		ctx:             ctx,
		client:          client,
		jobs:            make(map[string]*Job),
		idempotency:     make(map[string]*Job),
		logger:          opts.Logger,
		stateDir:        opts.StateDir,
		defaultTimeout:  opts.DefaultTimeout,
		recoveryTimeout: opts.RecoveryTimeout,
	}
	if opts.StateDir != "" {
		m.loadJobs()
	}
	m.recoverJobs()
	return m
}

// loadJobs reads persisted job records from stateDir on startup. A job found
// in running state becomes unknown until it is reconciled with opencode.
func (m *Manager) loadJobs() {
	entries, err := os.ReadDir(m.stateDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.logger.Warn("failed to read state dir", "dir", m.stateDir, "err", err)
		}
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(m.stateDir, entry.Name())
		data, err := os.ReadFile(path) //nolint:gosec // Operator-controlled state directory and ReadDir filename.
		if err != nil {
			m.logger.Warn("failed to read job file", "path", path, "err", err)
			continue
		}
		var rec jobRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			m.logger.Warn("failed to parse job file", "path", path, "err", err)
			continue
		}
		if err := validateSessionID(rec.SessionID); err != nil || rec.SessionID == "" {
			m.logger.Warn("invalid persisted session id", "path", path, "session_id", rec.SessionID)
			continue
		}
		job := &Job{
			SessionID:       rec.SessionID,
			Location:        rec.Location,
			Status:          rec.Status,
			IdempotencyKey:  rec.IdempotencyKey,
			RequestHash:     rec.RequestHash,
			PromptMessageID: rec.PromptMessageID,
			CreatedAt:       rec.CreatedAt,
			UpdatedAt:       rec.UpdatedAt,
			Deadline:        rec.Deadline,
		}
		if rec.ErrMessage != "" {
			job.Err = errors.New(rec.ErrMessage)
		}
		if job.Status == JobRunning {
			job.Status = JobUnknown
			job.UpdatedAt = time.Now()
			job.Err = nil
		}
		m.jobs[job.SessionID] = job
		if job.IdempotencyKey != "" {
			m.idempotency[idempotencyScope(job.SessionID, job.IdempotencyKey)] = job
		}
		m.logger.Debug("loaded persisted job", "session_id", job.SessionID, "status", job.Status)
	}
}

func (m *Manager) recoverJobs() {
	if m.client == nil {
		return
	}
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		job.mu.Lock()
		unknown := job.Status == JobUnknown
		job.mu.Unlock()
		if unknown {
			jobs = append(jobs, job)
		}
	}
	m.mu.Unlock()
	for _, job := range jobs {
		go m.recover(job)
	}
}

func (m *Manager) recover(job *Job) {
	snapshot := snapshotJob(job)
	if !snapshot.Deadline.IsZero() && !time.Now().Before(snapshot.Deadline) {
		m.timeout(job)
		return
	}

	recoveryDeadline := time.Now().Add(m.recoveryTimeout)
	if !snapshot.Deadline.IsZero() && snapshot.Deadline.Before(recoveryDeadline) {
		recoveryDeadline = snapshot.Deadline
	}
	ctx, cancel := context.WithDeadline(m.ctx, recoveryDeadline)
	defer cancel()

	if err := m.client.Wait(ctx, snapshot.Location, snapshot.SessionID); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) &&
			!snapshot.Deadline.IsZero() &&
			!time.Now().Before(snapshot.Deadline) {
			m.timeout(job)
			return
		}
		m.setRecoveryError(job, fmt.Errorf("recover job: %w", err))
		return
	}

	messages, err := m.client.Messages(ctx, snapshot.Location, snapshot.SessionID)
	if err != nil {
		m.setRecoveryError(job, fmt.Errorf("recover job messages: %w", err))
		return
	}
	if isSessionFinishedJSON(messages) {
		m.finish(job, JobDone, nil, "")
		return
	}
	m.finish(job, JobError, errors.New("execution was interrupted before completion"), "")
}

func (m *Manager) setRecoveryError(job *Job, err error) {
	job.mu.Lock()
	if job.Status == JobUnknown {
		job.Err = err
		job.UpdatedAt = time.Now()
	}
	job.mu.Unlock()
	m.saveJob(job)
}

func (m *Manager) saveJob(job *Job) {
	if m.stateDir == "" {
		return
	}
	snapshot := snapshotJob(job)
	if err := validateSessionID(snapshot.SessionID); err != nil || snapshot.SessionID == "" {
		m.logger.Warn("invalid session id, skipping save", "session_id", snapshot.SessionID)
		return
	}
	if err := os.MkdirAll(m.stateDir, 0o700); err != nil {
		m.logger.Warn("failed to create state dir", "dir", m.stateDir, "err", err)
		return
	}

	rec := jobRecord{
		SessionID:       snapshot.SessionID,
		Location:        snapshot.Location,
		Status:          snapshot.Status,
		IdempotencyKey:  snapshot.IdempotencyKey,
		RequestHash:     snapshot.RequestHash,
		PromptMessageID: snapshot.PromptMessageID,
		CreatedAt:       snapshot.CreatedAt,
		UpdatedAt:       snapshot.UpdatedAt,
		Deadline:        snapshot.Deadline,
	}
	if snapshot.Err != nil {
		rec.ErrMessage = snapshot.Err.Error()
	}
	data, err := json.Marshal(rec)
	if err != nil {
		m.logger.Warn("failed to marshal job", "session_id", snapshot.SessionID, "err", err)
		return
	}

	tmp, err := os.CreateTemp(m.stateDir, ".job-*")
	if err != nil {
		m.logger.Warn("failed to create temporary job file", "dir", m.stateDir, "err", err)
		return
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		m.logger.Warn("failed to write temporary job file", "path", tmpName, "err", err)
		return
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		m.logger.Warn("failed to sync temporary job file", "path", tmpName, "err", err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		m.logger.Warn("failed to close temporary job file", "path", tmpName, "err", err)
		return
	}

	path := filepath.Join(m.stateDir, filepath.Clean(snapshot.SessionID)+".json")
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		m.logger.Warn("failed to replace job file", "path", path, "err", err)
	}
}

// Submit preserves the original manager interface using default lifecycle options.
func (m *Manager) Submit(ctx context.Context, loc Location, sessionID string, req PromptRequest) (string, error) {
	result, err := m.SubmitJob(ctx, loc, sessionID, req, SubmitOptions{})
	if err != nil {
		return "", err
	}
	return result.Job.SessionID, nil
}

// SubmitJob starts or deduplicates a durable opencode execution.
func (m *Manager) SubmitJob(ctx context.Context, loc Location, sessionID string, req PromptRequest, opts SubmitOptions) (SubmitResult, error) {
	if err := ctx.Err(); err != nil {
		return SubmitResult{}, err
	}
	if req.Prompt.Text == "" {
		return SubmitResult{}, fmt.Errorf("prompt is required")
	}
	if err := validateSessionID(sessionID); err != nil {
		return SubmitResult{}, err
	}
	if sessionID == "" {
		return SubmitResult{}, fmt.Errorf("session_id is required")
	}

	key := strings.TrimSpace(opts.IdempotencyKey)
	if len(key) > maxIdempotencyKeyLength {
		return SubmitResult{}, fmt.Errorf("idempotency_key exceeds %d characters", maxIdempotencyKeyLength)
	}
	timeout := opts.Timeout
	if timeout < 0 {
		return SubmitResult{}, fmt.Errorf("timeout must not be negative")
	}
	if timeout == 0 {
		timeout = m.defaultTimeout
	}
	requestHash, err := hashSubmission(loc, req, timeout)
	if err != nil {
		return SubmitResult{}, err
	}

	m.mu.Lock()
	if key != "" {
		if existing, ok := m.idempotency[idempotencyScope(sessionID, key)]; ok {
			snapshot := snapshotJob(existing)
			m.mu.Unlock()
			if snapshot.RequestHash != "" && snapshot.RequestHash != requestHash {
				return SubmitResult{}, fmt.Errorf("idempotency_key %q was already used with a different request", key)
			}
			return SubmitResult{Job: snapshot, Duplicate: true}, nil
		}
	}
	if existing, ok := m.jobs[sessionID]; ok {
		snapshot := snapshotJob(existing)
		if snapshot.Status == JobRunning || snapshot.Status == JobUnknown {
			m.mu.Unlock()
			return SubmitResult{}, fmt.Errorf("session %q already has an active handoff job", sessionID)
		}
		if snapshot.IdempotencyKey != "" {
			delete(m.idempotency, idempotencyScope(sessionID, snapshot.IdempotencyKey))
		}
	}

	now := time.Now()
	runCtx := m.ctx
	var cancel context.CancelFunc
	var deadline time.Time
	if timeout > 0 {
		deadline = now.Add(timeout)
		runCtx, cancel = context.WithDeadline(m.ctx, deadline)
	} else {
		runCtx, cancel = context.WithCancel(m.ctx)
	}
	job := &Job{
		SessionID:      sessionID,
		Location:       loc,
		Status:         JobRunning,
		IdempotencyKey: key,
		RequestHash:    requestHash,
		CreatedAt:      now,
		UpdatedAt:      now,
		Deadline:       deadline,
		cancel:         cancel,
	}
	m.jobs[sessionID] = job
	if key != "" {
		m.idempotency[idempotencyScope(sessionID, key)] = job
	}
	m.mu.Unlock()

	m.saveJob(job)
	go m.run(runCtx, job, req)
	return SubmitResult{Job: snapshotJob(job)}, nil
}

func (m *Manager) Cancel(ctx context.Context, loc Location, sessionID string) (CancelResult, error) {
	if err := validateSessionID(sessionID); err != nil {
		return CancelResult{}, err
	}
	if sessionID == "" {
		return CancelResult{}, fmt.Errorf("session_id is required")
	}
	if m.client == nil {
		return CancelResult{}, errors.New("opencode client is unavailable")
	}

	job, tracked := m.jobPointer(sessionID)
	if tracked {
		snapshot := snapshotJob(job)
		if snapshot.Status.terminal() {
			return CancelResult{Job: snapshot}, nil
		}
		if loc == (Location{}) {
			loc = snapshot.Location
		}
	}

	aborted, err := m.client.Abort(ctx, loc, sessionID)
	if err != nil {
		return CancelResult{}, err
	}
	if !tracked {
		now := time.Now()
		return CancelResult{
			Job: &Job{
				SessionID: sessionID,
				Location:  loc,
				Status:    JobCanceled,
				CreatedAt: now,
				UpdatedAt: now,
			},
			Aborted: aborted,
		}, nil
	}

	job.mu.Lock()
	if !job.Status.terminal() {
		job.Status = JobCanceled
		job.Err = nil
		job.UpdatedAt = time.Now()
		cancel := job.cancel
		job.cancel = nil
		job.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		m.saveJob(job)
	} else {
		job.mu.Unlock()
	}
	return CancelResult{Job: snapshotJob(job), Aborted: aborted}, nil
}

func (m *Manager) Job(sessionID string) (*Job, bool) {
	job, ok := m.jobPointer(sessionID)
	if !ok {
		return nil, false
	}
	return snapshotJob(job), true
}

func (m *Manager) jobPointer(sessionID string) (*Job, bool) {
	m.mu.Lock()
	job, ok := m.jobs[sessionID]
	m.mu.Unlock()
	return job, ok
}

func (m *Manager) Jobs() []Job {
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()

	out := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, *snapshotJob(job))
	}
	return out
}

// Reconcile marks a persisted or uncertain job done when the opencode message
// stream proves that the session completed.
func (m *Manager) Reconcile(sessionID string, finished bool) (*Job, bool) {
	job, ok := m.jobPointer(sessionID)
	if !ok {
		return nil, false
	}
	if finished {
		job.mu.Lock()
		var cancel context.CancelFunc
		if job.Status != JobCanceled && job.Status != JobTimedOut {
			job.Status = JobDone
			job.Err = nil
			job.UpdatedAt = time.Now()
			cancel = job.cancel
			job.cancel = nil
		}
		job.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		m.saveJob(job)
	}
	return snapshotJob(job), true
}

func (m *Manager) run(ctx context.Context, job *Job, req PromptRequest) {
	defer func() {
		job.mu.Lock()
		cancel := job.cancel
		job.cancel = nil
		job.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}()

	m.logger.Debug("running job", "session_id", job.SessionID)
	res, err := m.client.Prompt(ctx, job.Location, job.SessionID, req)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		m.timeout(job)
		return
	}

	job.mu.Lock()
	if job.Status == JobCanceled || job.Status == JobTimedOut {
		job.mu.Unlock()
		return
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		// The manager is shutting down. Preserve running state so the next
		// executor instance can recover it as unknown.
		job.UpdatedAt = time.Now()
		job.mu.Unlock()
		m.saveJob(job)
		return
	}

	job.PromptMessageID = extractMessageID(res)
	job.UpdatedAt = time.Now()
	if err != nil {
		job.Status = JobError
		job.Err = err
		m.logger.Warn("opencode executor job failed", "session_id", job.SessionID, "err", err)
	} else {
		job.Status = JobDone
		job.Err = nil
	}
	job.mu.Unlock()
	m.saveJob(job)
}

func (m *Manager) timeout(job *Job) {
	snapshot := snapshotJob(job)
	if snapshot.Status.terminal() {
		return
	}
	if m.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
		_, err := m.client.Abort(ctx, snapshot.Location, snapshot.SessionID)
		cancel()
		if err != nil {
			m.logger.Warn("failed to abort timed out opencode session", "session_id", snapshot.SessionID, "err", err)
		}
	}
	m.finish(job, JobTimedOut, errors.New("job execution timed out"), "")
}

func (m *Manager) finish(job *Job, status JobStatus, err error, promptMessageID string) {
	job.mu.Lock()
	if job.Status.terminal() {
		job.mu.Unlock()
		return
	}
	job.Status = status
	job.Err = err
	if promptMessageID != "" {
		job.PromptMessageID = promptMessageID
	}
	job.UpdatedAt = time.Now()
	cancel := job.cancel
	job.cancel = nil
	job.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.saveJob(job)
}

func snapshotJob(job *Job) *Job {
	job.mu.Lock()
	defer job.mu.Unlock()
	return &Job{
		SessionID:       job.SessionID,
		Location:        job.Location,
		Status:          job.Status,
		IdempotencyKey:  job.IdempotencyKey,
		RequestHash:     job.RequestHash,
		PromptMessageID: job.PromptMessageID,
		Err:             job.Err,
		CreatedAt:       job.CreatedAt,
		UpdatedAt:       job.UpdatedAt,
		Deadline:        job.Deadline,
	}
}

func idempotencyScope(sessionID, key string) string {
	return sessionID + "\x00" + key
}

func hashSubmission(loc Location, req PromptRequest, timeout time.Duration) (string, error) {
	data, err := json.Marshal(struct {
		Location Location      `json:"location"`
		Request  PromptRequest `json:"request"`
		Timeout  time.Duration `json:"timeout"`
	}{
		Location: loc,
		Request:  req,
		Timeout:  timeout,
	})
	if err != nil {
		return "", fmt.Errorf("hash submission: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
