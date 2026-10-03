package opencode

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	opencodeapi "github.com/pradhankukiran/codex-opencode-executor/internal/tools/opencode/opencodeapi"
)

// pendingPermissionHandlerMock returns a handler that keeps the prompt goroutine
// alive until the context is done and surfaces a permission request once the
// pending channel is closed. Session messages remain in a running state so the
// job never terminates while we are testing pending detection.
func pendingPermissionHandlerMock(t *testing.T, promptStarted chan struct{}, pending <-chan struct{}, sessionID string) *HandlerMock {
	t.Helper()
	return &HandlerMock{
		SessionMessageFunc: func(ctx context.Context, _ *opencodeapi.SessionMessageReq, _ opencodeapi.SessionMessageParams) (opencodeapi.SessionMessageRes, error) {
			close(promptStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		SessionMessagesFunc: func(context.Context, opencodeapi.SessionMessagesParams) (opencodeapi.SessionMessagesRes, error) {
			var resp opencodeapi.SessionMessagesOKApplicationJSON
			require.NoError(t, json.Unmarshal([]byte(realisticRunningMessages), &resp))
			return &resp, nil
		},
		V2SessionContextFunc: func(context.Context, opencodeapi.V2SessionContextParams) (opencodeapi.V2SessionContextRes, error) {
			return &opencodeapi.V2SessionContextOK{Data: []opencodeapi.SessionMessage{}}, nil
		},
		V2SessionPermissionListFunc: func(_ context.Context, params opencodeapi.V2SessionPermissionListParams) (opencodeapi.V2SessionPermissionListRes, error) {
			require.Equal(t, sessionID, params.SessionID)
			select {
			case <-pending:
				return &opencodeapi.V2SessionPermissionListOK{Data: []opencodeapi.PermissionV2Request{{
					ID:        "perm-1",
					Action:    "write file",
					SessionID: sessionID,
				}}}, nil
			default:
				return &opencodeapi.V2SessionPermissionListOK{Data: []opencodeapi.PermissionV2Request{}}, nil
			}
		},
		V2QuestionRequestListFunc: func(context.Context, opencodeapi.V2QuestionRequestListParams) (opencodeapi.V2QuestionRequestListRes, error) {
			return &opencodeapi.V2QuestionRequestListOK{Data: []opencodeapi.QuestionV2Request{}}, nil
		},
		V2SessionWaitFunc: func(context.Context, opencodeapi.V2SessionWaitParams) (opencodeapi.V2SessionWaitRes, error) {
			return &opencodeapi.V2SessionWaitNoContent{}, nil
		},
	}
}

// pendingQuestionHandlerMock is like pendingPermissionHandlerMock but surfaces a
// pending clarification question when the pending channel is closed.
func pendingQuestionHandlerMock(t *testing.T, promptStarted chan struct{}, pending <-chan struct{}, sessionID string) *HandlerMock {
	t.Helper()
	return &HandlerMock{
		SessionMessageFunc: func(ctx context.Context, _ *opencodeapi.SessionMessageReq, _ opencodeapi.SessionMessageParams) (opencodeapi.SessionMessageRes, error) {
			close(promptStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		SessionMessagesFunc: func(context.Context, opencodeapi.SessionMessagesParams) (opencodeapi.SessionMessagesRes, error) {
			var resp opencodeapi.SessionMessagesOKApplicationJSON
			require.NoError(t, json.Unmarshal([]byte(realisticRunningMessages), &resp))
			return &resp, nil
		},
		V2SessionContextFunc: func(context.Context, opencodeapi.V2SessionContextParams) (opencodeapi.V2SessionContextRes, error) {
			return &opencodeapi.V2SessionContextOK{Data: []opencodeapi.SessionMessage{}}, nil
		},
		V2SessionPermissionListFunc: func(_ context.Context, params opencodeapi.V2SessionPermissionListParams) (opencodeapi.V2SessionPermissionListRes, error) {
			require.Equal(t, sessionID, params.SessionID)
			return &opencodeapi.V2SessionPermissionListOK{Data: []opencodeapi.PermissionV2Request{}}, nil
		},
		V2QuestionRequestListFunc: func(context.Context, opencodeapi.V2QuestionRequestListParams) (opencodeapi.V2QuestionRequestListRes, error) {
			select {
			case <-pending:
				return &opencodeapi.V2QuestionRequestListOK{Data: []opencodeapi.QuestionV2Request{{
					ID:        "que-1",
					SessionID: sessionID,
					Questions: []opencodeapi.QuestionV2Info{{
						Question: "Which file should I edit?",
					}},
				}}}, nil
			default:
				return &opencodeapi.V2QuestionRequestListOK{Data: []opencodeapi.QuestionV2Request{}}, nil
			}
		},
		V2SessionWaitFunc: func(context.Context, opencodeapi.V2SessionWaitParams) (opencodeapi.V2SessionWaitRes, error) {
			return &opencodeapi.V2SessionWaitNoContent{}, nil
		},
	}
}

func TestFireHandlerWaitSecondsReturnsOnPendingPermission(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	pendingAppeared := make(chan struct{})
	handler := pendingPermissionHandlerMock(t, promptStarted, pendingAppeared, "ses-fire-pending")
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
			SessionID:   "ses-fire-pending",
			Prompt:      "needs permission",
			WaitSeconds: 30,
		})
		done <- fireOutcome{res: res, err: err}
	}()

	<-promptStarted

	select {
	case outcome := <-done:
		t.Fatalf("handoff_fire returned before pending appeared: status=%s pending=%d err=%v",
			outcome.res.Status, len(outcome.res.PendingPermissions), outcome.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(pendingAppeared)

	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.Equal(t, string(JobRunning), outcome.res.Status, "pending request must not mark job terminal")
		require.Len(t, outcome.res.PendingPermissions, 1)
		require.Equal(t, "write file", outcome.res.PendingPermissions[0].Title)
		job, ok := mgr.Job("ses-fire-pending")
		require.True(t, ok)
		require.Equal(t, JobRunning, job.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("handoff_fire did not return promptly when a permission became pending")
	}
}

func TestCheckHandlerWaitSecondsReturnsOnPendingPermission(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	pendingAppeared := make(chan struct{})
	handler := pendingPermissionHandlerMock(t, promptStarted, pendingAppeared, "ses-check-pending")
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})

	_, err := mgr.SubmitJob(t.Context(), Location{}, "ses-check-pending", PromptRequest{
		Prompt: PromptPayload{Text: "needs permission"},
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
			SessionID:   "ses-check-pending",
			WaitSeconds: 30,
		})
		done <- checkOutcome{res: res, err: err}
	}()

	select {
	case outcome := <-done:
		t.Fatalf("handoff_check returned before pending appeared: status=%s pending=%d err=%v",
			outcome.res.Status, len(outcome.res.PendingPermissions), outcome.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(pendingAppeared)

	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.Equal(t, string(JobRunning), outcome.res.Status, "pending request must not mark job terminal")
		require.Len(t, outcome.res.PendingPermissions, 1)
		require.Equal(t, "write file", outcome.res.PendingPermissions[0].Title)
		job, ok := mgr.Job("ses-check-pending")
		require.True(t, ok)
		require.Equal(t, JobRunning, job.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("handoff_check did not return promptly when a permission became pending")
	}
}

func TestCheckHandlerWaitSecondsReturnsOnPendingQuestion(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	pendingAppeared := make(chan struct{})
	handler := pendingQuestionHandlerMock(t, promptStarted, pendingAppeared, "ses-q-pending")
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})

	_, err := mgr.SubmitJob(t.Context(), Location{}, "ses-q-pending", PromptRequest{
		Prompt: PromptPayload{Text: "needs clarification"},
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
			SessionID:   "ses-q-pending",
			WaitSeconds: 30,
		})
		done <- checkOutcome{res: res, err: err}
	}()

	select {
	case outcome := <-done:
		t.Fatalf("handoff_check returned before question appeared: status=%s questions=%d err=%v",
			outcome.res.Status, len(outcome.res.PendingQuestions), outcome.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(pendingAppeared)

	loc := Location{}
	rawQuestions, err := client.Questions(t.Context(), loc, "ses-q-pending")
	require.NoError(t, err)
	require.Contains(t, string(rawQuestions), `"que-1"`)
	require.Contains(t, string(rawQuestions), `"ses-q-pending"`)
	summarized := summarizeQuestionRequests(rawQuestions, "ses-q-pending")
	require.Len(t, summarized, 1, "summarizeQuestionRequests must extract pending questions")
	require.Contains(t, summarized[0].Text, "Which file should I edit?")

	// Regression coverage: pending questions must remain observable across
	// repeated diagnostic calls and wait-loop polling.
	pending := fetchPending(t.Context(), client, loc, "ses-q-pending")
	require.Empty(t, pending.Errors)
	require.Len(t, pending.Questions, 1, "fetchPending must surface pending questions")
	require.Contains(t, pending.Questions[0].Text, "Which file should I edit?")

	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.Equal(t, string(JobRunning), outcome.res.Status)
		require.Len(t, outcome.res.PendingQuestions, 1)
		require.Contains(t, outcome.res.PendingQuestions[0].Text, "Which file should I edit?")
	case <-time.After(2 * time.Second):
		t.Fatal("handoff_check did not return promptly when a question became pending")
	}
}

func TestExecuteWaitSecondsReturnsOnPendingPermission(t *testing.T) {
	t.Parallel()

	pendingAppeared := make(chan struct{})
	handler := executeHandlerMock(t, "ses-exec-pending", func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, realisticRunningMessages)

	handler.V2SessionPermissionListFunc = func(ctx context.Context, p opencodeapi.V2SessionPermissionListParams) (opencodeapi.V2SessionPermissionListRes, error) {
		select {
		case <-pendingAppeared:
			return &opencodeapi.V2SessionPermissionListOK{Data: []opencodeapi.PermissionV2Request{{
				ID:        "perm-exec",
				Action:    "edit",
				SessionID: "ses-exec-pending",
			}}}, nil
		default:
			return &opencodeapi.V2SessionPermissionListOK{Data: []opencodeapi.PermissionV2Request{}}, nil
		}
	}

	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	h := executeHandler(client, mgr, nil, ExecutorOptions{})

	type execOutcome struct {
		res HandoffExecuteResult
		err error
	}
	done := make(chan execOutcome, 1)
	go func() {
		_, res, err := h(t.Context(), nil, executeParams{
			Prompt:      "needs permission",
			WaitSeconds: intPtr(30),
			Profile:     executeProfileNone,
		})
		done <- execOutcome{res: res, err: err}
	}()

	select {
	case outcome := <-done:
		t.Fatalf("handoff_execute returned before pending appeared: status=%s pending=%d err=%v",
			outcome.res.Status, len(outcome.res.PendingPermissions), outcome.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(pendingAppeared)

	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.Equal(t, string(JobRunning), outcome.res.Status, "pending request must not mark job terminal")
		require.Len(t, outcome.res.PendingPermissions, 1)
		require.Equal(t, "edit", outcome.res.PendingPermissions[0].Title)
		job, ok := mgr.Job("ses-exec-pending")
		require.True(t, ok)
		require.Equal(t, JobRunning, job.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("handoff_execute did not return promptly when a permission became pending")
	}
}
