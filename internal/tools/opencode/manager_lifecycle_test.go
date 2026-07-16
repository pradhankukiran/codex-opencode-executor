package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeJobClient struct {
	prompt   func(context.Context, Location, string, PromptRequest) (json.RawMessage, error)
	abort    func(context.Context, Location, string) (bool, error)
	wait     func(context.Context, Location, string) error
	messages func(context.Context, Location, string) (json.RawMessage, error)
}

func (f *fakeJobClient) Prompt(ctx context.Context, loc Location, sessionID string, req PromptRequest) (json.RawMessage, error) {
	if f.prompt == nil {
		return nil, errors.New("unexpected Prompt call")
	}
	return f.prompt(ctx, loc, sessionID, req)
}

func (f *fakeJobClient) Abort(ctx context.Context, loc Location, sessionID string) (bool, error) {
	if f.abort == nil {
		return false, errors.New("unexpected Abort call")
	}
	return f.abort(ctx, loc, sessionID)
}

func (f *fakeJobClient) Wait(ctx context.Context, loc Location, sessionID string) error {
	if f.wait == nil {
		return errors.New("unexpected Wait call")
	}
	return f.wait(ctx, loc, sessionID)
}

func (f *fakeJobClient) Messages(ctx context.Context, loc Location, sessionID string) (json.RawMessage, error) {
	if f.messages == nil {
		return nil, errors.New("unexpected Messages call")
	}
	return f.messages(ctx, loc, sessionID)
}

func TestManagerSubmitJobIdempotency(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	var promptCalls atomic.Int32
	client := &fakeJobClient{
		prompt: func(ctx context.Context, loc Location, sessionID string, req PromptRequest) (json.RawMessage, error) {
			if promptCalls.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
				return json.RawMessage(`{"id":"msg-1"}`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Second})
	loc := Location{Directory: "/project"}
	req := PromptRequest{Prompt: PromptPayload{Text: "implement it"}, Agent: "build"}
	opts := SubmitOptions{IdempotencyKey: "request-1"}

	first, err := mgr.SubmitJob(t.Context(), loc, "ses-1", req, opts)
	require.NoError(t, err)
	require.False(t, first.Duplicate)
	require.Equal(t, JobRunning, first.Job.Status)
	<-started

	duplicate, err := mgr.SubmitJob(t.Context(), loc, "ses-1", req, opts)
	require.NoError(t, err)
	require.True(t, duplicate.Duplicate)
	require.Equal(t, int32(1), promptCalls.Load())

	_, err = mgr.SubmitJob(t.Context(), loc, "ses-1", PromptRequest{
		Prompt: PromptPayload{Text: "different task"},
	}, opts)
	require.ErrorContains(t, err, "different request")

	_, err = mgr.SubmitJob(t.Context(), loc, "ses-1", req, SubmitOptions{IdempotencyKey: "request-2"})
	require.ErrorContains(t, err, "already has an active")

	close(release)
	require.Eventually(t, func() bool {
		job, ok := mgr.Job("ses-1")
		return ok && job.Status == JobDone && job.PromptMessageID == "msg-1"
	}, time.Second, 5*time.Millisecond)
}

func TestManagerJobTimeoutAbortsSession(t *testing.T) {
	t.Parallel()

	var abortCalls atomic.Int32
	client := &fakeJobClient{
		prompt: func(ctx context.Context, loc Location, sessionID string, req PromptRequest) (json.RawMessage, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		abort: func(context.Context, Location, string) (bool, error) {
			abortCalls.Add(1)
			return true, nil
		},
	}
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: 20 * time.Millisecond})

	result, err := mgr.SubmitJob(t.Context(), Location{}, "ses-timeout", PromptRequest{
		Prompt: PromptPayload{Text: "take too long"},
	}, SubmitOptions{})
	require.NoError(t, err)
	require.False(t, result.Job.Deadline.IsZero())

	require.Eventually(t, func() bool {
		job, ok := mgr.Job("ses-timeout")
		return ok && job.Status == JobTimedOut
	}, time.Second, 5*time.Millisecond)
	require.Equal(t, int32(1), abortCalls.Load())
}

func TestManagerCancel(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	client := &fakeJobClient{
		prompt: func(ctx context.Context, loc Location, sessionID string, req PromptRequest) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		abort: func(context.Context, Location, string) (bool, error) {
			return true, nil
		},
	}
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Second})
	_, err := mgr.SubmitJob(t.Context(), Location{Directory: "/project"}, "ses-cancel", PromptRequest{
		Prompt: PromptPayload{Text: "cancel me"},
	}, SubmitOptions{})
	require.NoError(t, err)
	<-started

	result, err := mgr.Cancel(t.Context(), Location{}, "ses-cancel")
	require.NoError(t, err)
	require.True(t, result.Aborted)
	require.Equal(t, JobCanceled, result.Job.Status)

	require.Eventually(t, func() bool {
		job, ok := mgr.Job("ses-cancel")
		return ok && job.Status == JobCanceled
	}, time.Second, 5*time.Millisecond)
}

func TestManagerIdempotencySurvivesRestart(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	client := &fakeJobClient{
		prompt: func(context.Context, Location, string, PromptRequest) (json.RawMessage, error) {
			return json.RawMessage(`{"id":"msg-persisted"}`), nil
		},
	}
	managerOpts := ManagerOptions{
		StateDir:       stateDir,
		DefaultTimeout: time.Minute,
	}
	mgr := NewManager(t.Context(), client, managerOpts)
	loc := Location{Directory: "/project"}
	req := PromptRequest{Prompt: PromptPayload{Text: "persist me"}}
	submitOpts := SubmitOptions{IdempotencyKey: "persistent-request"}
	_, err := mgr.SubmitJob(t.Context(), loc, "ses-persisted", req, submitOpts)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		job, ok := mgr.Job("ses-persisted")
		return ok && job.Status == JobDone
	}, time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return persistedJobStatus(stateDir, "ses-persisted") == JobDone
	}, time.Second, 5*time.Millisecond)

	restarted := NewManager(t.Context(), nil, managerOpts)
	duplicate, err := restarted.SubmitJob(t.Context(), loc, "ses-persisted", req, submitOpts)
	require.NoError(t, err)
	require.True(t, duplicate.Duplicate)
	require.Equal(t, JobDone, duplicate.Job.Status)
}

func TestManagerRecoversCompletedJob(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	rec := jobRecord{
		SessionID: "ses-recover",
		Location:  Location{Directory: "/project"},
		Status:    JobRunning,
		CreatedAt: time.Now().Add(-time.Minute),
		UpdatedAt: time.Now().Add(-time.Minute),
		Deadline:  time.Now().Add(time.Minute),
	}
	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, "ses-recover.json"), raw, 0o600))

	client := &fakeJobClient{
		wait: func(context.Context, Location, string) error {
			return nil
		},
		messages: func(context.Context, Location, string) (json.RawMessage, error) {
			return json.RawMessage(`[{"info":{"role":"assistant","finish":"stop"}}]`), nil
		},
	}
	mgr := NewManager(t.Context(), client, ManagerOptions{
		StateDir:        stateDir,
		RecoveryTimeout: time.Second,
	})

	require.Eventually(t, func() bool {
		job, ok := mgr.Job("ses-recover")
		return ok && job.Status == JobDone
	}, time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return persistedJobStatus(stateDir, "ses-recover") == JobDone
	}, time.Second, 5*time.Millisecond)
}

func TestManagerExpiresRecoveredJob(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	rec := jobRecord{
		SessionID: "ses-expired",
		Status:    JobRunning,
		CreatedAt: time.Now().Add(-time.Minute),
		UpdatedAt: time.Now().Add(-time.Minute),
		Deadline:  time.Now().Add(-time.Second),
	}
	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, "ses-expired.json"), raw, 0o600))

	var abortCalls atomic.Int32
	client := &fakeJobClient{
		abort: func(context.Context, Location, string) (bool, error) {
			abortCalls.Add(1)
			return true, nil
		},
	}
	mgr := NewManager(t.Context(), client, ManagerOptions{StateDir: stateDir})

	require.Eventually(t, func() bool {
		job, ok := mgr.Job("ses-expired")
		return ok && job.Status == JobTimedOut
	}, time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return persistedJobStatus(stateDir, "ses-expired") == JobTimedOut
	}, time.Second, 5*time.Millisecond)
	require.Equal(t, int32(1), abortCalls.Load())
}

func persistedJobStatus(stateDir, sessionID string) JobStatus {
	data, err := os.ReadFile(filepath.Join(stateDir, sessionID+".json"))
	if err != nil {
		return ""
	}
	var rec jobRecord
	if json.Unmarshal(data, &rec) != nil {
		return ""
	}
	return rec.Status
}
