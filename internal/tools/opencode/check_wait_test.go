package opencode

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	opencodeapi "github.com/pradhankukiran/codex-opencode-executor/internal/tools/opencode/opencodeapi"
)

func checkWaitHandlerMock(
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
		// Simulates the production defect: OpenCode wait returns immediately
		// while the executor job is still running.
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
	handler := checkWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
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
	handler := checkWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
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
	handler := checkWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
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
	handler := checkWaitHandlerMock(func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
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

	handler := checkWaitHandlerMock(func(context.Context) (opencodeapi.SessionMessageRes, error) {
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
