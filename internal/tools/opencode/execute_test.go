package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	opencodeapi "github.com/pradhankukiran/codex-opencode-executor/internal/tools/opencode/opencodeapi"
	"github.com/pradhankukiran/codex-opencode-executor/internal/workspace"
)

func TestResolveExecuteWaitSeconds(t *testing.T) {
	t.Parallel()

	got, err := resolveExecuteWaitSeconds(false, nil)
	require.NoError(t, err)
	require.Equal(t, 300, got)

	zero := 0
	got, err = resolveExecuteWaitSeconds(false, &zero)
	require.NoError(t, err)
	require.Equal(t, 0, got)

	n := 45
	got, err = resolveExecuteWaitSeconds(false, &n)
	require.NoError(t, err)
	require.Equal(t, 45, got)

	got, err = resolveExecuteWaitSeconds(true, nil)
	require.NoError(t, err)
	require.Equal(t, 0, got)

	got, err = resolveExecuteWaitSeconds(true, &n)
	require.NoError(t, err)
	require.Equal(t, 0, got)

	bad := -1
	_, err = resolveExecuteWaitSeconds(false, &bad)
	require.ErrorContains(t, err, "wait_seconds must be between 0 and 300")

	over := 301
	_, err = resolveExecuteWaitSeconds(false, &over)
	require.ErrorContains(t, err, "wait_seconds must be between 0 and 300")
}

func TestApplyExecuteProfile(t *testing.T) {
	t.Parallel()

	task := "fix the flaky test in auth_test.go"
	impl, err := applyExecuteProfile(task, "")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(impl, task))
	require.Contains(t, impl, executeProfileSuffixImplementation)
	require.NotEqual(t, task, impl)

	implNamed, err := applyExecuteProfile(task, "implementation")
	require.NoError(t, err)
	require.Equal(t, impl, implNamed)

	diag, err := applyExecuteProfile(task, "diagnosis")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(diag, task))
	require.Contains(t, diag, executeProfileSuffixDiagnosis)
	require.Contains(t, diag, "Read-only")

	rev, err := applyExecuteProfile(task, "review")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(rev, task))
	require.Contains(t, rev, executeProfileSuffixReview)

	none, err := applyExecuteProfile(task, "none")
	require.NoError(t, err)
	require.Equal(t, task, none)

	_, err = applyExecuteProfile(task, "ship-it")
	require.ErrorContains(t, err, "profile must be one of")
}

func TestExecuteValidationBeforeCreate(t *testing.T) {
	t.Parallel()

	var creates atomic.Int32
	handler := &HandlerMock{
		SessionCreateFunc: func(context.Context, opencodeapi.OptSessionCreateReq, opencodeapi.SessionCreateParams) (opencodeapi.SessionCreateRes, error) {
			creates.Add(1)
			t.Fatal("create must not run when validation fails")
			return nil, nil
		},
	}
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{})
	h := executeHandler(client, mgr, nil, ExecutorOptions{})

	cases := []struct {
		name string
		args executeParams
		err  string
	}{
		{name: "empty prompt", args: executeParams{}, err: "prompt is required"},
		{name: "bad profile", args: executeParams{Prompt: "do it", Profile: "nope"}, err: "profile must be one of"},
		{name: "bad model", args: executeParams{Prompt: "do it", createSessionParams: createSessionParams{Model: "not-a-ref"}}, err: "provider/model"},
		{name: "bad max final", args: executeParams{Prompt: "do it", MaxFinalChars: -1}, err: "max_final_chars"},
		{name: "bad wait", args: executeParams{Prompt: "do it", WaitSeconds: intPtr(999)}, err: "wait_seconds"},
		{name: "bad timeout", args: executeParams{Prompt: "do it", TimeoutSeconds: -1}, err: "timeout_seconds"},
		{name: "create_directory without workspaces", args: executeParams{
			Prompt: "do it",
			createSessionParams: createSessionParams{
				locationParams:  locationParams{Directory: filepath.Join(t.TempDir(), "x")},
				CreateDirectory: true,
			},
		}, err: "workspace management"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := h(t.Context(), nil, tc.args)
			require.ErrorContains(t, err, tc.err)
		})
	}
	require.Equal(t, int32(0), creates.Load())
}

func TestExecuteAsyncImmediate(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	handler := executeHandlerMock(t, "ses-exec-async", func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}, realisticRunningMessages)
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	h := executeHandler(client, mgr, nil, ExecutorOptions{DefaultAgent: "build"})

	start := time.Now()
	_, res, err := h(t.Context(), nil, executeParams{
		Prompt:  "run long",
		Async:   true,
		Profile: executeProfileNone,
	})
	require.NoError(t, err)
	require.Equal(t, "ses-exec-async", res.SessionID)
	require.Equal(t, string(JobRunning), res.Status)
	require.Empty(t, res.FinalText)
	require.Nil(t, res.Review)
	require.Less(t, time.Since(start), 500*time.Millisecond)
	<-promptStarted
}

func TestExecuteRunningOnWaitTimeout(t *testing.T) {
	t.Parallel()

	promptStarted := make(chan struct{})
	handler := executeHandlerMock(t, "ses-exec-timeout", func(ctx context.Context) (opencodeapi.SessionMessageRes, error) {
		close(promptStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}, realisticRunningMessages)
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	h := executeHandler(client, mgr, nil, ExecutorOptions{})

	_, res, err := h(t.Context(), nil, executeParams{
		Prompt:      "still going",
		WaitSeconds: intPtr(0),
		Profile:     executeProfileNone,
	})
	require.NoError(t, err)
	require.Equal(t, "ses-exec-timeout", res.SessionID)
	require.Equal(t, string(JobRunning), res.Status)
	require.Empty(t, res.FinalText)
	require.Nil(t, res.Review)
	<-promptStarted
}

func TestExecuteTerminalCompactWithReview(t *testing.T) {
	t.Parallel()

	repository := t.TempDir()
	runGit(t, repository, "init")
	runGit(t, repository, "config", "user.name", "Test User")
	runGit(t, repository, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "file.txt"), []byte("base\n"), 0o600))
	runGit(t, repository, "add", "file.txt")
	runGit(t, repository, "commit", "-m", "base")

	handler := executeHandlerMock(t, "ses-exec-done", func(context.Context) (opencodeapi.SessionMessageRes, error) {
		return &opencodeapi.SessionMessageOK{
			Info: opencodeapi.SessionMessageOKInfo{ID: "msg-done", Role: "assistant"},
		}, nil
	}, realisticDoneMessages)
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	workspaces, err := workspace.NewManager(workspace.Options{
		StateDir:    filepath.Join(t.TempDir(), "state"),
		WorktreeDir: filepath.Join(t.TempDir(), "worktrees"),
		DefaultMode: workspace.ModeNone,
	})
	require.NoError(t, err)

	h := executeHandler(client, mgr, workspaces, ExecutorOptions{
		DefaultModel: ModelRef{ProviderID: "xai", ModelID: "grok-4.5"},
		DefaultAgent: "build",
	})
	_, res, err := h(t.Context(), nil, executeParams{
		createSessionParams: createSessionParams{
			locationParams: locationParams{Directory: repository},
			Title:          "exec",
		},
		Prompt:      "finish up",
		WaitSeconds: intPtr(5),
		Profile:     executeProfileNone,
	})
	require.NoError(t, err)
	require.Equal(t, "ses-exec-done", res.SessionID)
	require.Equal(t, string(JobDone), res.Status)
	require.Equal(t, "xai/grok-4.5", res.Model)
	require.Equal(t, "build", res.Agent)
	require.NotEmpty(t, res.FinalText)
	require.NotContains(t, res.FinalText, "INTERNAL NARRATION")
	require.NotNil(t, res.Review)
	require.Equal(t, "ses-exec-done", res.Review.SessionID)

	// Mutate workspace after bind so review has content if inspect runs later;
	// for ModeNone the same directory is used.
	require.NoError(t, os.WriteFile(filepath.Join(repository, "file.txt"), []byte("changed\n"), 0o600))

	encoded, err := json.Marshal(res)
	require.NoError(t, err)
	body := string(encoded)
	require.NotContains(t, body, `"workspace"`)
	require.NotContains(t, body, `"messages"`)
	require.NotContains(t, body, `"source_directory"`)
	require.NotContains(t, body, `"directory"`)
	require.NotContains(t, body, repository)
	require.NotContains(t, body, "USER PROMPT")
}

func TestExecuteSkipReview(t *testing.T) {
	t.Parallel()

	handler := executeHandlerMock(t, "ses-exec-skip", func(context.Context) (opencodeapi.SessionMessageRes, error) {
		return &opencodeapi.SessionMessageOK{
			Info: opencodeapi.SessionMessageOKInfo{ID: "msg-skip", Role: "assistant"},
		}, nil
	}, realisticDoneMessages)
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	h := executeHandler(client, mgr, nil, ExecutorOptions{})

	_, res, err := h(t.Context(), nil, executeParams{
		Prompt:      "done",
		WaitSeconds: intPtr(5),
		SkipReview:  true,
		Profile:     executeProfileNone,
	})
	require.NoError(t, err)
	require.Equal(t, string(JobDone), res.Status)
	require.Nil(t, res.Review)
}

func TestExecuteModelSelection(t *testing.T) {
	t.Parallel()

	var gotProvider, gotModel string
	handler := executeHandlerMock(t, "ses-exec-model", func(context.Context) (opencodeapi.SessionMessageRes, error) {
		return &opencodeapi.SessionMessageOK{
			Info: opencodeapi.SessionMessageOKInfo{ID: "msg-m", Role: "assistant"},
		}, nil
	}, realisticDoneMessages)
	handler.SessionCreateFunc = func(_ context.Context, req opencodeapi.OptSessionCreateReq, _ opencodeapi.SessionCreateParams) (opencodeapi.SessionCreateRes, error) {
		if req.IsSet() && req.Value.Model.IsSet() {
			gotProvider = req.Value.Model.Value.ProviderID
			gotModel = req.Value.Model.Value.ID
		}
		return &opencodeapi.Session{ID: "ses-exec-model", Title: "m"}, nil
	}
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
	h := executeHandler(client, mgr, nil, ExecutorOptions{
		DefaultModel: ModelRef{ProviderID: "openai", ModelID: "gpt-4o"},
	})

	_, res, err := h(t.Context(), nil, executeParams{
		createSessionParams: createSessionParams{Model: "vercel/xai/grok-4.5"},
		Prompt:              "use model",
		Async:               true,
		Profile:             executeProfileNone,
	})
	require.NoError(t, err)
	require.Equal(t, "vercel", gotProvider)
	require.Equal(t, "xai/grok-4.5", gotModel)
	require.Equal(t, "vercel/xai/grok-4.5", res.Model)
}

func TestExecuteProfilePromptSubmitted(t *testing.T) {
	t.Parallel()

	task := "inspect flaky auth test"

	t.Run("diagnosis suffix", func(t *testing.T) {
		var submitted string
		handler := executeHandlerMock(t, "ses-exec-profile", func(context.Context) (opencodeapi.SessionMessageRes, error) {
			return &opencodeapi.SessionMessageOK{
				Info: opencodeapi.SessionMessageOKInfo{ID: "msg-p", Role: "assistant"},
			}, nil
		}, realisticDoneMessages)
		baseMsg := handler.SessionMessageFunc
		handler.SessionMessageFunc = func(ctx context.Context, req *opencodeapi.SessionMessageReq, params opencodeapi.SessionMessageParams) (opencodeapi.SessionMessageRes, error) {
			if req != nil && len(req.Parts) > 0 && req.Parts[0].GetText().IsSet() {
				submitted = req.Parts[0].GetText().Value
			}
			return baseMsg(ctx, req, params)
		}
		client := setupTestServer(t, handler)
		mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
		h := executeHandler(client, mgr, nil, ExecutorOptions{})
		_, _, err := h(t.Context(), nil, executeParams{
			Prompt:      task,
			WaitSeconds: intPtr(5),
			Profile:     executeProfileDiagnosis,
		})
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(submitted, task))
		require.Contains(t, submitted, executeProfileSuffixDiagnosis)
	})

	t.Run("none exact", func(t *testing.T) {
		var submitted string
		handler := executeHandlerMock(t, "ses-exec-none", func(context.Context) (opencodeapi.SessionMessageRes, error) {
			return &opencodeapi.SessionMessageOK{
				Info: opencodeapi.SessionMessageOKInfo{ID: "msg-n", Role: "assistant"},
			}, nil
		}, realisticDoneMessages)
		baseMsg := handler.SessionMessageFunc
		handler.SessionMessageFunc = func(ctx context.Context, req *opencodeapi.SessionMessageReq, params opencodeapi.SessionMessageParams) (opencodeapi.SessionMessageRes, error) {
			if req != nil && len(req.Parts) > 0 && req.Parts[0].GetText().IsSet() {
				submitted = req.Parts[0].GetText().Value
			}
			return baseMsg(ctx, req, params)
		}
		client := setupTestServer(t, handler)
		mgr := NewManager(t.Context(), client, ManagerOptions{DefaultTimeout: time.Minute})
		h := executeHandler(client, mgr, nil, ExecutorOptions{})
		_, _, err := h(t.Context(), nil, executeParams{
			Prompt:      task,
			WaitSeconds: intPtr(5),
			Profile:     executeProfileNone,
		})
		require.NoError(t, err)
		require.Equal(t, task, submitted)
	})
}

func TestExecuteSerializedOmitsWorkspaceAndMessages(t *testing.T) {
	t.Parallel()

	res := HandoffExecuteResult{
		SessionID: "ses-x",
		Status:    string(JobDone),
		Model:     "xai/grok-4.5",
		Agent:     "build",
		FinalText: "all good",
		Review: &HandoffReviewResult{
			SessionID:  "ses-x",
			Available:  true,
			HasChanges: true,
		},
	}
	encoded, err := json.Marshal(res)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(encoded, &raw))
	_, hasWorkspace := raw["workspace"]
	_, hasMessages := raw["messages"]
	require.False(t, hasWorkspace)
	require.False(t, hasMessages)
	require.Contains(t, raw, "session_id")
	require.Contains(t, raw, "final_text")
	require.Contains(t, raw, "review")
}

func intPtr(v int) *int { return &v }

// executeHandlerMock builds a mock that creates a fixed session, runs promptFn
// for SessionMessage, and returns messagesJSON for SessionMessages polls.
func executeHandlerMock(t *testing.T, sessionID string, promptFn func(context.Context) (opencodeapi.SessionMessageRes, error), messagesJSON string) *HandlerMock {
	t.Helper()
	if promptFn == nil {
		promptFn = func(context.Context) (opencodeapi.SessionMessageRes, error) {
			return &opencodeapi.SessionMessageOK{
				Info: opencodeapi.SessionMessageOKInfo{ID: "msg", Role: "assistant"},
			}, nil
		}
	}
	return &HandlerMock{
		SessionCreateFunc: func(context.Context, opencodeapi.OptSessionCreateReq, opencodeapi.SessionCreateParams) (opencodeapi.SessionCreateRes, error) {
			return &opencodeapi.Session{ID: sessionID, Title: "exec"}, nil
		},
		SessionMessageFunc: func(ctx context.Context, _ *opencodeapi.SessionMessageReq, _ opencodeapi.SessionMessageParams) (opencodeapi.SessionMessageRes, error) {
			return promptFn(ctx)
		},
		SessionMessagesFunc: func(context.Context, opencodeapi.SessionMessagesParams) (opencodeapi.SessionMessagesRes, error) {
			var resp opencodeapi.SessionMessagesOKApplicationJSON
			require.NoError(t, json.Unmarshal([]byte(messagesJSON), &resp))
			return &resp, nil
		},
		V2SessionContextFunc: func(context.Context, opencodeapi.V2SessionContextParams) (opencodeapi.V2SessionContextRes, error) {
			return &opencodeapi.V2SessionContextOK{Data: []opencodeapi.SessionMessage{}}, nil
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
