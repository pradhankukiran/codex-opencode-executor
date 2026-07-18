package opencode

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	opencodeapi "github.com/pradhankukiran/codex-opencode-executor/internal/tools/opencode/opencodeapi"
)

// jobWaitHandlerMock returns early from OpenCode V2SessionWait so handler tests
// prove wait_seconds is driven by Manager job completion, not the remote API.
func jobWaitHandlerMock(
	prompt func(context.Context) (opencodeapi.SessionMessageRes, error),
) *HandlerMock {
	return &HandlerMock{
		SessionMessageFunc: func(ctx context.Context, _ *opencodeapi.SessionMessageReq, _ opencodeapi.SessionMessageParams) (opencodeapi.SessionMessageRes, error) {
			return prompt(ctx)
		},
		SessionMessagesFunc: func(context.Context, opencodeapi.SessionMessagesParams) (opencodeapi.SessionMessagesRes, error) {
			var resp opencodeapi.SessionMessagesOKApplicationJSON
			return &resp, nil
		},
		V2SessionContextFunc: func(context.Context, opencodeapi.V2SessionContextParams) (opencodeapi.V2SessionContextRes, error) {
			return &opencodeapi.V2SessionContextOK{Data: []opencodeapi.SessionMessage{}}, nil
		},
		V2SessionPermissionListFunc: func(context.Context, opencodeapi.V2SessionPermissionListParams) (opencodeapi.V2SessionPermissionListRes, error) {
			return &opencodeapi.V2SessionPermissionListOK{Data: []opencodeapi.PermissionV2Request{}}, nil
		},
		V2PermissionRequestListFunc: func(context.Context, opencodeapi.V2PermissionRequestListParams) (opencodeapi.V2PermissionRequestListRes, error) {
			return &opencodeapi.V2PermissionRequestListOK{Data: []opencodeapi.PermissionV2Request{}}, nil
		},
		V2QuestionRequestListFunc: func(context.Context, opencodeapi.V2QuestionRequestListParams) (opencodeapi.V2QuestionRequestListRes, error) {
			return &opencodeapi.V2QuestionRequestListOK{Data: []opencodeapi.QuestionV2Request{}}, nil
		},
		V2SessionWaitFunc: func(context.Context, opencodeapi.V2SessionWaitParams) (opencodeapi.V2SessionWaitRes, error) {
			return &opencodeapi.V2SessionWaitNoContent{}, nil
		},
	}
}

// TestCheckHandlerWaitSecondsBlocksUntilJobTerminal is the regression seam for
// handoff_check long-polling: wait_seconds>0 must not return while a tracked
// job is still running, even if OpenCode's session wait endpoint returns early.
func TestCheckHandlerWaitSecondsBlocksUntilJobTerminal(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	promptRelease := make(chan struct{})
	handler := jobWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		select {
		case <-promptRelease:
			return &opencodeapi.SessionMessageOK{
				Info: opencodeapi.SessionMessageOKInfo{ID: "msg-1", Role: "assistant"},
			}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})

	_, err := mgr.SubmitJob(t.Context(), Location{}, "ses-wait", PromptRequest{
		Prompt: PromptPayload{Text: "long running"},
	}, SubmitOptions{})
	require.NoError(t, err)
	<-promptStarted

	type checkOutcome struct {
		res HandoffCheckResult
		err error
	}
	done := make(chan checkOutcome, 1)
	h := checkHandler(client, mgr, nil)
	go func() {
		_, res, err := h(t.Context(), nil, checkParams{
			SessionID:   "ses-wait",
			WaitSeconds: 30,
		})
		done <- checkOutcome{res: res, err: err}
	}()

	// While the job is still running, check must not complete.
	select {
	case outcome := <-done:
		t.Fatalf("handoff_check returned while job still running: status=%s err=%v", outcome.res.Status, outcome.err)
	case <-time.After(75 * time.Millisecond):
	}

	close(promptRelease)

	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.Equal(t, string(JobDone), outcome.res.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("handoff_check did not wake when job reached terminal state")
	}
}

func TestCheckHandlerWaitSecondsTimeout(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	handler := jobWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})

	_, err := mgr.SubmitJob(t.Context(), Location{}, "ses-timeout-wait", PromptRequest{
		Prompt: PromptPayload{Text: "never finishes in time"},
	}, SubmitOptions{})
	require.NoError(t, err)
	<-promptStarted

	start := time.Now()
	h := checkHandler(client, mgr, nil)
	_, res, err := h(t.Context(), nil, checkParams{
		SessionID:   "ses-timeout-wait",
		WaitSeconds: 1,
	})
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, string(JobRunning), res.Status)
	require.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	require.Less(t, elapsed, 5*time.Second)
}

func TestCheckHandlerZeroWaitIsNonBlocking(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	handler := jobWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})

	_, err := mgr.SubmitJob(t.Context(), Location{}, "ses-zero-wait", PromptRequest{
		Prompt: PromptPayload{Text: "running"},
	}, SubmitOptions{})
	require.NoError(t, err)
	<-promptStarted

	start := time.Now()
	h := checkHandler(client, mgr, nil)
	_, res, err := h(t.Context(), nil, checkParams{SessionID: "ses-zero-wait", WaitSeconds: 0})
	require.NoError(t, err)
	require.Equal(t, string(JobRunning), res.Status)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestCheckHandlerWaitSecondsContextCancel(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	handler := jobWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})

	_, err := mgr.SubmitJob(t.Context(), Location{}, "ses-cancel-wait", PromptRequest{
		Prompt: PromptPayload{Text: "running"},
	}, SubmitOptions{})
	require.NoError(t, err)
	<-promptStarted

	// Exercise Manager.Wait cancellation directly at the coordination seam.
	waitCtx, cancel := context.WithCancel(t.Context())
	waiting := make(chan error, 1)
	go func() {
		waiting <- mgr.Wait(waitCtx, "ses-cancel-wait")
	}()

	select {
	case err := <-waiting:
		t.Fatalf("Manager.Wait returned before cancel: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()

	select {
	case err := <-waiting:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Manager.Wait did not return after context cancellation")
	}
}

func TestCheckHandlerWaitSecondsAlreadyTerminal(t *testing.T) {
	t.Parallel()

	handler := jobWaitHandlerMock(func(context.Context) (opencodeapi.SessionMessageRes, error) {
		return &opencodeapi.SessionMessageOK{
			Info: opencodeapi.SessionMessageOKInfo{ID: "msg-done", Role: "assistant"},
		}, nil
	})
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})

	_, err := mgr.SubmitJob(t.Context(), Location{}, "ses-already-done", PromptRequest{
		Prompt: PromptPayload{Text: "quick"},
	}, SubmitOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		job, ok := mgr.Job("ses-already-done")
		return ok && job.Status == JobDone
	}, time.Second, 5*time.Millisecond)

	start := time.Now()
	h := checkHandler(client, mgr, nil)
	_, res, err := h(t.Context(), nil, checkParams{SessionID: "ses-already-done", WaitSeconds: 30})
	require.NoError(t, err)
	require.Equal(t, string(JobDone), res.Status)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

// TestCheckHandlerWaitsThroughJobDoneToolCallsRace covers the live OpenCode
// 1.18.3 failure mode: local Manager already JobDone (done channel closed) while
// the latest assistant finish is still "tool-calls". handoff_check must keep
// waiting until finish="stop" and only then return done + final_text.
func TestCheckHandlerWaitsThroughJobDoneToolCallsRace(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	phase := "tool-calls"

	handler := &HandlerMock{
		SessionMessagesFunc: func(context.Context, opencodeapi.SessionMessagesParams) (opencodeapi.SessionMessagesRes, error) {
			mu.Lock()
			current := phase
			mu.Unlock()
			payload := realisticToolCallsOnlyMessages
			if current == "stop" {
				payload = realisticToolCallsThenStopMessages
			}
			var resp opencodeapi.SessionMessagesOKApplicationJSON
			require.NoError(t, json.Unmarshal([]byte(payload), &resp))
			return &resp, nil
		},
		V2SessionContextFunc: func(context.Context, opencodeapi.V2SessionContextParams) (opencodeapi.V2SessionContextRes, error) {
			return &opencodeapi.V2SessionContextOK{Data: []opencodeapi.SessionMessage{}}, nil
		},
		V2SessionPermissionListFunc: func(context.Context, opencodeapi.V2SessionPermissionListParams) (opencodeapi.V2SessionPermissionListRes, error) {
			return &opencodeapi.V2SessionPermissionListOK{Data: []opencodeapi.PermissionV2Request{}}, nil
		},
		V2PermissionRequestListFunc: func(context.Context, opencodeapi.V2PermissionRequestListParams) (opencodeapi.V2PermissionRequestListRes, error) {
			return &opencodeapi.V2PermissionRequestListOK{Data: []opencodeapi.PermissionV2Request{}}, nil
		},
		V2QuestionRequestListFunc: func(context.Context, opencodeapi.V2QuestionRequestListParams) (opencodeapi.V2QuestionRequestListRes, error) {
			return &opencodeapi.V2QuestionRequestListOK{Data: []opencodeapi.QuestionV2Request{}}, nil
		},
		V2SessionWaitFunc: func(context.Context, opencodeapi.V2SessionWaitParams) (opencodeapi.V2SessionWaitRes, error) {
			return &opencodeapi.V2SessionWaitNoContent{}, nil
		},
	}
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{})

	// Seed a local job that already raced to JobDone while OpenCode is mid-loop.
	doneCh := make(chan struct{})
	close(doneCh)
	mgr.mu.Lock()
	mgr.jobs["ses-toolcalls-race"] = &Job{
		SessionID: "ses-toolcalls-race",
		Status:    JobDone,
		done:      doneCh,
	}
	mgr.mu.Unlock()

	type checkOutcome struct {
		res HandoffCheckResult
		err error
	}
	outcomeCh := make(chan checkOutcome, 1)
	h := checkHandler(client, mgr, nil)
	go func() {
		_, res, err := h(t.Context(), nil, checkParams{
			SessionID:   "ses-toolcalls-race",
			WaitSeconds: 30,
		})
		outcomeCh <- checkOutcome{res: res, err: err}
	}()

	// While OpenCode is still on tool-calls, check must not complete as done.
	select {
	case outcome := <-outcomeCh:
		t.Fatalf("handoff_check returned during tool-calls race: status=%s final=%q err=%v",
			outcome.res.Status, outcome.res.FinalText, outcome.err)
	case <-time.After(150 * time.Millisecond):
	}

	// Compact poll during the race must report running with no final_text.
	_, mid, err := h(t.Context(), nil, checkParams{SessionID: "ses-toolcalls-race", WaitSeconds: 0})
	require.NoError(t, err)
	require.Equal(t, string(JobRunning), mid.Status)
	require.Empty(t, mid.FinalText)

	mu.Lock()
	phase = "stop"
	mu.Unlock()

	select {
	case outcome := <-outcomeCh:
		require.NoError(t, outcome.err)
		require.Equal(t, string(JobDone), outcome.res.Status)
		require.Equal(t, "FINAL ANSWER: Fixed the race with a wait condition.", outcome.res.FinalText)
		require.NotContains(t, outcome.res.FinalText, "PROGRESS NARRATION")
	case <-time.After(2 * time.Second):
		t.Fatal("handoff_check did not wake when OpenCode reached finish=stop")
	}
}

// TestManagerAwaitOpenCodeFinishedKeepsJobRunningThroughToolCalls ensures the
// job lifecycle does not signal JobDone while the latest assistant finish is
// still tool-calls after Prompt returns.
func TestManagerAwaitOpenCodeFinishedKeepsJobRunningThroughToolCalls(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	phase := "tool-calls"
	promptReturned := make(chan struct{})

	client := &fakeJobClient{
		prompt: func(ctx context.Context, _ Location, _ string, _ PromptRequest) (json.RawMessage, error) {
			close(promptReturned)
			return json.RawMessage(`{"id":"msg-prompt"}`), nil
		},
		messages: func(context.Context, Location, string) (json.RawMessage, error) {
			mu.Lock()
			current := phase
			mu.Unlock()
			if current == "stop" {
				return json.RawMessage(realisticToolCallsThenStopMessages), nil
			}
			return json.RawMessage(realisticToolCallsOnlyMessages), nil
		},
	}
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})

	_, err := mgr.SubmitJob(t.Context(), Location{}, "ses-await-toolcalls", PromptRequest{
		Prompt: PromptPayload{Text: "work"},
	}, SubmitOptions{})
	require.NoError(t, err)
	<-promptReturned

	// Give await loop time to observe tool-calls and remain running.
	time.Sleep(150 * time.Millisecond)
	job, ok := mgr.Job("ses-await-toolcalls")
	require.True(t, ok)
	require.Equal(t, JobRunning, job.Status)

	mu.Lock()
	phase = "stop"
	mu.Unlock()

	require.Eventually(t, func() bool {
		job, ok := mgr.Job("ses-await-toolcalls")
		return ok && job.Status == JobDone
	}, 2*time.Second, 20*time.Millisecond)
}

func TestFireHandlerWaitSecondsBlocksUntilJobTerminal(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	promptRelease := make(chan struct{})
	handler := jobWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		select {
		case <-promptRelease:
			return &opencodeapi.SessionMessageOK{
				Info: opencodeapi.SessionMessageOKInfo{ID: "msg-fire", Role: "assistant"},
			}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	h := fireHandler(client, mgr, nil, ExecutorOptions{})

	type fireOutcome struct {
		res HandoffFireResult
		err error
	}
	done := make(chan fireOutcome, 1)
	go func() {
		_, res, err := h(t.Context(), nil, fireParams{
			SessionID:   "ses-fire-wait",
			Prompt:      "long running",
			WaitSeconds: 30,
		})
		done <- fireOutcome{res: res, err: err}
	}()

	<-promptStarted

	select {
	case outcome := <-done:
		t.Fatalf("handoff_fire returned while job still running: status=%s err=%v", outcome.res.Status, outcome.err)
	case <-time.After(75 * time.Millisecond):
	}

	close(promptRelease)

	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.Equal(t, string(JobDone), outcome.res.Status)
		require.False(t, outcome.res.Duplicate)
	case <-time.After(2 * time.Second):
		t.Fatal("handoff_fire did not wake when job reached terminal state")
	}
}

func TestFireHandlerWaitSecondsTimeout(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	handler := jobWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	h := fireHandler(client, mgr, nil, ExecutorOptions{})

	start := time.Now()
	done := make(chan struct {
		res HandoffFireResult
		err error
	}, 1)
	go func() {
		_, res, err := h(t.Context(), nil, fireParams{
			SessionID:   "ses-fire-timeout",
			Prompt:      "never finishes in time",
			WaitSeconds: 1,
		})
		done <- struct {
			res HandoffFireResult
			err error
		}{res: res, err: err}
	}()
	<-promptStarted

	outcome := <-done
	elapsed := time.Since(start)
	require.NoError(t, outcome.err)
	require.Equal(t, string(JobRunning), outcome.res.Status)
	require.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	require.Less(t, elapsed, 5*time.Second)
}

func TestFireHandlerZeroWaitIsNonBlocking(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	handler := jobWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	h := fireHandler(client, mgr, nil, ExecutorOptions{})

	start := time.Now()
	_, res, err := h(t.Context(), nil, fireParams{
		SessionID:   "ses-fire-zero",
		Prompt:      "running",
		WaitSeconds: 0,
	})
	require.NoError(t, err)
	require.Equal(t, string(JobRunning), res.Status)
	require.Less(t, time.Since(start), 500*time.Millisecond)
	<-promptStarted
}

func TestFireHandlerWaitSecondsDuplicateIdempotency(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	promptRelease := make(chan struct{})
	handler := jobWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		select {
		case <-promptRelease:
			return &opencodeapi.SessionMessageOK{
				Info: opencodeapi.SessionMessageOKInfo{ID: "msg-dup", Role: "assistant"},
			}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	h := fireHandler(client, mgr, nil, ExecutorOptions{})

	args := fireParams{
		SessionID:      "ses-fire-dup",
		Prompt:         "idempotent",
		IdempotencyKey: "key-1",
		WaitSeconds:    0,
	}
	_, first, err := h(t.Context(), nil, args)
	require.NoError(t, err)
	require.False(t, first.Duplicate)
	require.Equal(t, string(JobRunning), first.Status)
	<-promptStarted

	// Duplicate with wait should block on the original job and report terminal status.
	type fireOutcome struct {
		res HandoffFireResult
		err error
	}
	done := make(chan fireOutcome, 1)
	go func() {
		dupArgs := args
		dupArgs.WaitSeconds = 30
		_, res, err := h(t.Context(), nil, dupArgs)
		done <- fireOutcome{res: res, err: err}
	}()

	select {
	case outcome := <-done:
		t.Fatalf("duplicate handoff_fire returned while job still running: status=%s err=%v", outcome.res.Status, outcome.err)
	case <-time.After(75 * time.Millisecond):
	}

	close(promptRelease)

	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.True(t, outcome.res.Duplicate)
		require.Equal(t, string(JobDone), outcome.res.Status)
		require.Equal(t, "key-1", outcome.res.IdempotencyKey)
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate handoff_fire did not wake when job reached terminal state")
	}
}
