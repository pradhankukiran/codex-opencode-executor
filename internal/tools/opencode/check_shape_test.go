package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	opencodeapi "github.com/pradhankukiran/codex-opencode-executor/internal/tools/opencode/opencodeapi"
)

// realisticRunningMessages mirrors a live OpenCode WithParts payload mid-turn:
// user prompt, assistant reasoning, partial assistant text, tool narration, and
// a temporary conclusion — none of which is a final answer.
const realisticRunningMessages = `[
  {
    "info": {
      "id": "msg_user_01",
      "role": "user",
      "path": {"cwd": "/project", "root": "/project"}
    },
    "parts": [
      {"type": "text", "id": "prt_u1", "sessionID": "ses_1", "messageID": "msg_user_01",
       "text": "USER PROMPT: refactor the auth module and explain the result"}
    ]
  },
  {
    "info": {
      "id": "msg_asst_01",
      "role": "assistant",
      "modelID": "claude-sonnet-4-5",
      "providerID": "anthropic",
      "mode": "default",
      "agent": "coder",
      "path": {"cwd": "/project", "root": "/project"},
      "cost": 0,
      "tokens": {"input": 500, "output": 120, "reasoning": 80, "cache": {"read": 0, "write": 0}}
    },
    "parts": [
      {"type": "reasoning", "id": "prt_r1", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "text": "INTERNAL NARRATION: I should inspect auth.go next"},
      {"type": "text", "id": "prt_t1", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "text": "PARTIAL: Looking at the auth module…"},
      {"type": "tool", "id": "prt_tool1", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "tool": "read", "state": {"status": "running", "input": {"path": "auth.go"}},
       "text": "TOOL CALL: read auth.go"},
      {"type": "text", "id": "prt_t2", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "text": "TEMPORARY CONCLUSION: maybe extract validateToken"}
    ]
  }
]`

// realisticDoneMessages is the finished form of the same session: finished
// assistant turn with reasoning/tool noise plus a real final answer text part.
// Live OpenCode 1.18.x uses finish="stop" on the terminal turn.
const realisticDoneMessages = `[
  {
    "info": {
      "id": "msg_user_01",
      "role": "user",
      "path": {"cwd": "/project", "root": "/project"}
    },
    "parts": [
      {"type": "text", "id": "prt_u1", "sessionID": "ses_1", "messageID": "msg_user_01",
       "text": "USER PROMPT: refactor the auth module and explain the result"}
    ]
  },
  {
    "info": {
      "id": "msg_asst_01",
      "role": "assistant",
      "modelID": "claude-sonnet-4-5",
      "providerID": "anthropic",
      "mode": "default",
      "agent": "coder",
      "path": {"cwd": "/project", "root": "/project"},
      "cost": 0.01,
      "tokens": {"input": 1500, "output": 400, "reasoning": 80, "cache": {"read": 0, "write": 0}},
      "finish": "stop"
    },
    "parts": [
      {"type": "reasoning", "id": "prt_r1", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "text": "INTERNAL NARRATION: extract validateToken into its own helper"},
      {"type": "text", "id": "prt_progress", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "text": "PROGRESS NARRATION: about to edit auth.go"},
      {"type": "tool", "id": "prt_tool1", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "tool": "edit", "state": {"status": "completed", "output": "edited auth.go"},
       "text": "TOOL RESULT: edited auth.go"},
      {"type": "text", "id": "prt_final", "sessionID": "ses_1", "messageID": "msg_asst_01",
       "text": "FINAL ANSWER: Extracted validateToken and updated callers."}
    ]
  }
]`

// realisticToolCallsThenStopMessages mirrors live OpenCode 1.18.3 multi-turn
// agent loops: intermediate assistant turns finish with "tool-calls", and only
// the last turn finishes with "stop".
const realisticToolCallsThenStopMessages = `[
  {
    "info": {"id": "msg_user_01", "role": "user"},
    "parts": [{"type": "text", "text": "USER PROMPT: fix the flaky test"}]
  },
  {
    "info": {"id": "msg_asst_01", "role": "assistant", "finish": "tool-calls"},
    "parts": [
      {"type": "text", "text": "PROGRESS NARRATION: inspecting the failing test"},
      {"type": "tool", "tool": "read", "state": {"status": "completed"},
       "text": "TOOL RESULT: read test.go"}
    ]
  },
  {
    "info": {"id": "msg_asst_02", "role": "assistant", "finish": "tool-calls"},
    "parts": [
      {"type": "text", "text": "PROGRESS NARRATION: applying a patch"},
      {"type": "tool", "tool": "edit", "state": {"status": "completed"},
       "text": "TOOL RESULT: edited test.go"}
    ]
  },
  {
    "info": {"id": "msg_asst_03", "role": "assistant", "finish": "stop"},
    "parts": [
      {"type": "reasoning", "text": "INTERNAL NARRATION: the flake was a race"},
      {"type": "text", "text": "PROGRESS NARRATION: summarizing"},
      {"type": "text", "text": "FINAL ANSWER: Fixed the race with a wait condition."}
    ]
  }
]`

// realisticToolCallsOnlyMessages is a mid-loop snapshot where every assistant
// turn so far ended with finish="tool-calls".
const realisticToolCallsOnlyMessages = `[
  {
    "info": {"id": "msg_user_01", "role": "user"},
    "parts": [{"type": "text", "text": "USER PROMPT: fix the flaky test"}]
  },
  {
    "info": {"id": "msg_asst_01", "role": "assistant", "finish": "tool-calls"},
    "parts": [
      {"type": "text", "text": "PROGRESS NARRATION: inspecting the failing test"},
      {"type": "tool", "tool": "read", "state": {"status": "completed"},
       "text": "TOOL RESULT: read test.go"}
    ]
  }
]`

// realisticContextLeak is what /context may return — often dominated by the
// user prompt when no assistant final is present.
const realisticContextLeak = `[
  {"role": "user", "content": [{"type": "text", "text": "USER PROMPT FROM CONTEXT: refactor auth"}]}
]`

func TestHandoffCheckResponseShaping(t *testing.T) {
	t.Parallel()

	t.Run("running compact omits final_text and messages", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-running-compact",
			tracked:   true,
			jobStatus: JobRunning,
			messages:  realisticRunningMessages,
			context:   realisticContextLeak,
			verbose:   false,
		})
		require.Equal(t, string(JobRunning), res.Status)
		require.Empty(t, res.FinalText, "running compact must not leak partial/progress text as final_text")
		require.Nil(t, res.Messages, "verbose=false must omit messages")
		requireNoLeakMarkers(t, res)
	})

	t.Run("running verbose includes messages but not final_text", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-running-verbose",
			tracked:   true,
			jobStatus: JobRunning,
			messages:  realisticRunningMessages,
			context:   realisticContextLeak,
			verbose:   true,
		})
		require.Equal(t, string(JobRunning), res.Status)
		require.Empty(t, res.FinalText, "running must never label partial content as final_text")
		require.NotEmpty(t, res.Messages, "verbose=true may include bounded raw messages while running")
		require.LessOrEqual(t, len(res.Messages), 6)
	})

	t.Run("done compact returns only final assistant answer", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-done-compact",
			tracked:   true,
			jobStatus: JobDone,
			messages:  realisticDoneMessages,
			context:   realisticContextLeak,
			verbose:   false,
		})
		require.Equal(t, string(JobDone), res.Status)
		require.Equal(t, "FINAL ANSWER: Extracted validateToken and updated callers.", res.FinalText)
		require.Nil(t, res.Messages)
		require.NotContains(t, res.FinalText, "USER PROMPT")
		require.NotContains(t, res.FinalText, "INTERNAL NARRATION")
		require.NotContains(t, res.FinalText, "TOOL RESULT")
	})

	t.Run("done verbose includes messages and clean final_text", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-done-verbose",
			tracked:   true,
			jobStatus: JobDone,
			messages:  realisticDoneMessages,
			context:   realisticContextLeak,
			verbose:   true,
		})
		require.Equal(t, string(JobDone), res.Status)
		require.Equal(t, "FINAL ANSWER: Extracted validateToken and updated callers.", res.FinalText)
		require.NotEmpty(t, res.Messages)
		require.LessOrEqual(t, len(res.Messages), 6)
	})

	t.Run("terminal error omits final_text without finished assistant answer", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-error",
			tracked:   true,
			jobStatus: JobError,
			jobErr:    errors.New("model failed"),
			messages:  realisticRunningMessages,
			verbose:   false,
		})
		require.Equal(t, string(JobError), res.Status)
		require.Equal(t, "model failed", res.JobError)
		require.Empty(t, res.FinalText)
		require.Nil(t, res.Messages)
	})

	t.Run("terminal canceled omits final_text without finished assistant answer", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-canceled",
			tracked:   true,
			jobStatus: JobCanceled,
			messages:  realisticRunningMessages,
			verbose:   false,
		})
		require.Equal(t, string(JobCanceled), res.Status)
		require.Empty(t, res.FinalText)
		require.Nil(t, res.Messages)
	})

	t.Run("terminal timed_out omits final_text without finished assistant answer", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-timeout",
			tracked:   true,
			jobStatus: JobTimedOut,
			jobErr:    errors.New("job execution timed out"),
			messages:  realisticRunningMessages,
			verbose:   false,
		})
		require.Equal(t, string(JobTimedOut), res.Status)
		require.Equal(t, "job execution timed out", res.JobError)
		require.Empty(t, res.FinalText)
		require.Nil(t, res.Messages)
	})

	t.Run("terminal error keeps finished assistant final_text", func(t *testing.T) {
		t.Parallel()
		// Pure boundary: status/error stay authoritative; finished assistant answer may
		// still populate final_text. (doCheck/Reconcile can promote error→done when
		// OpenCode reports finished, so shape at the response boundary directly.)
		res := shapeHandoffCheckResult(HandoffCheckResult{
			SessionID: "ses-error-with-answer",
			Status:    string(JobError),
			JobError:  "post-complete side effect failed",
		}, json.RawMessage(realisticDoneMessages), false, true)
		require.Equal(t, string(JobError), res.Status)
		require.Equal(t, "post-complete side effect failed", res.JobError)
		require.Equal(t, "FINAL ANSWER: Extracted validateToken and updated callers.", res.FinalText)
		require.Nil(t, res.Messages)
	})

	t.Run("untracked running omits final_text", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-ext-running",
			tracked:   false,
			messages:  realisticRunningMessages,
			context:   realisticContextLeak,
			verbose:   false,
		})
		require.Equal(t, string(JobRunning), res.Status)
		require.Empty(t, res.FinalText)
		require.Nil(t, res.Messages)
	})

	t.Run("untracked finished returns final assistant answer only", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-ext-done",
			tracked:   false,
			messages:  realisticDoneMessages,
			context:   realisticContextLeak,
			verbose:   false,
		})
		require.Equal(t, string(JobDone), res.Status)
		require.Equal(t, "FINAL ANSWER: Extracted validateToken and updated callers.", res.FinalText)
		require.Nil(t, res.Messages)
	})

	t.Run("untracked finished verbose includes messages", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-ext-done-verbose",
			tracked:   false,
			messages:  realisticDoneMessages,
			verbose:   true,
		})
		require.Equal(t, string(JobDone), res.Status)
		require.Equal(t, "FINAL ANSWER: Extracted validateToken and updated callers.", res.FinalText)
		require.NotEmpty(t, res.Messages)
	})

	t.Run("unknown tracked job omits final_text like running", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-unknown",
			tracked:   true,
			jobStatus: JobUnknown,
			messages:  realisticRunningMessages,
			verbose:   false,
		})
		require.Equal(t, string(JobUnknown), res.Status)
		require.Empty(t, res.FinalText)
		require.Nil(t, res.Messages)
	})

	t.Run("tool-calls continuation is not finished", func(t *testing.T) {
		t.Parallel()
		require.False(t, isSessionFinishedJSON(json.RawMessage(realisticToolCallsOnlyMessages)))
		require.True(t, openCodeAssistantContinuing(json.RawMessage(realisticToolCallsOnlyMessages)))
	})

	t.Run("tool-calls then stop is finished with trailing final answer only", func(t *testing.T) {
		t.Parallel()
		require.True(t, isSessionFinishedJSON(json.RawMessage(realisticToolCallsThenStopMessages)))
		got := finalAssistantAnswerText(json.RawMessage(realisticToolCallsThenStopMessages), true)
		require.Equal(t, "FINAL ANSWER: Fixed the race with a wait condition.", got)
		require.NotContains(t, got, "PROGRESS NARRATION")
		require.NotContains(t, got, "INTERNAL NARRATION")
	})

	t.Run("local JobDone with tool-calls reports running without final_text", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-done-race-tool-calls",
			tracked:   true,
			jobStatus: JobDone,
			messages:  realisticToolCallsOnlyMessages,
			verbose:   false,
		})
		require.Equal(t, string(JobRunning), res.Status)
		require.Empty(t, res.FinalText)
		require.Nil(t, res.Messages)
	})

	t.Run("local JobDone with stop reports done and clean final_text", func(t *testing.T) {
		t.Parallel()
		res := checkViaDoCheck(t, checkShapeFixture{
			sessionID: "ses-done-stop",
			tracked:   true,
			jobStatus: JobDone,
			messages:  realisticToolCallsThenStopMessages,
			verbose:   false,
		})
		require.Equal(t, string(JobDone), res.Status)
		require.Equal(t, "FINAL ANSWER: Fixed the race with a wait condition.", res.FinalText)
		require.Nil(t, res.Messages)
	})
}

func TestFinalAssistantAnswerText(t *testing.T) {
	t.Parallel()

	t.Run("done payload yields only final text part", func(t *testing.T) {
		t.Parallel()
		got := finalAssistantAnswerText(json.RawMessage(realisticDoneMessages), true)
		require.Equal(t, "FINAL ANSWER: Extracted validateToken and updated callers.", got)
		require.NotContains(t, got, "PROGRESS NARRATION")
	})

	t.Run("running payload yields empty when finish required", func(t *testing.T) {
		t.Parallel()
		got := finalAssistantAnswerText(json.RawMessage(realisticRunningMessages), true)
		require.Empty(t, got)
	})

	t.Run("tool-calls finish yields empty", func(t *testing.T) {
		t.Parallel()
		got := finalAssistantAnswerText(json.RawMessage(realisticToolCallsOnlyMessages), true)
		require.Empty(t, got)
	})

	t.Run("never returns user prompt", func(t *testing.T) {
		t.Parallel()
		raw := json.RawMessage(`[
			{"info":{"id":"u","role":"user"},"parts":[{"type":"text","text":"only the user spoke"}]},
			{"info":{"id":"a","role":"assistant","finish":"stop"},"parts":[{"type":"reasoning","text":"thinking only"}]}
		]`)
		require.Empty(t, finalAssistantAnswerText(raw, true))
	})
}

func TestIsTerminalFinish(t *testing.T) {
	t.Parallel()
	require.True(t, isTerminalFinish("stop"))
	require.True(t, isTerminalFinish("end_turn"))
	require.False(t, isTerminalFinish("tool-calls"))
	require.False(t, isTerminalFinish("unknown"))
	require.False(t, isTerminalFinish(""))
	require.False(t, isTerminalFinish("brand-new-reason"))
}

type checkShapeFixture struct {
	sessionID string
	tracked   bool
	jobStatus JobStatus
	jobErr    error
	messages  string
	context   string // retained for fixture realism; check path no longer uses context for final_text
	verbose   bool
}

func checkViaDoCheck(t *testing.T, fix checkShapeFixture) HandoffCheckResult {
	t.Helper()

	handler := &HandlerMock{
		SessionMessagesFunc: func(context.Context, opencodeapi.SessionMessagesParams) (opencodeapi.SessionMessagesRes, error) {
			var resp opencodeapi.SessionMessagesOKApplicationJSON
			require.NoError(t, json.Unmarshal([]byte(fix.messages), &resp))
			return &resp, nil
		},
		V2SessionContextFunc: func(context.Context, opencodeapi.V2SessionContextParams) (opencodeapi.V2SessionContextRes, error) {
			_ = fix.context
			return &opencodeapi.V2SessionContextOK{Data: []opencodeapi.SessionMessage{}}, nil
		},
		V2PermissionRequestListFunc: func(context.Context, opencodeapi.V2PermissionRequestListParams) (opencodeapi.V2PermissionRequestListRes, error) {
			return &opencodeapi.V2PermissionRequestListOK{Data: []opencodeapi.PermissionV2Request{}}, nil
		},
		V2QuestionRequestListFunc: func(context.Context, opencodeapi.V2QuestionRequestListParams) (opencodeapi.V2QuestionRequestListRes, error) {
			return &opencodeapi.V2QuestionRequestListOK{Data: []opencodeapi.QuestionV2Request{}}, nil
		},
	}
	client := setupTestServer(t, handler)
	mgr := NewManager(t.Context(), client, ManagerOptions{})

	if fix.tracked {
		job := &Job{
			SessionID: fix.sessionID,
			Status:    fix.jobStatus,
			Err:       fix.jobErr,
			done:      make(chan struct{}),
		}
		if fix.jobStatus.terminal() {
			close(job.done)
		}
		mgr.mu.Lock()
		mgr.jobs[fix.sessionID] = job
		mgr.mu.Unlock()
	}

	res, err := doCheck(t.Context(), client, mgr, checkParams{
		SessionID: fix.sessionID,
		Verbose:   fix.verbose,
	})
	require.NoError(t, err)

	if fix.tracked && fix.jobStatus.terminal() && fix.jobStatus != JobDone {
		// doCheck/Reconcile must not promote canceled/timed_out/error to done
		// when the fixture seeds those terminal states with unfinished messages.
		require.Equal(t, string(fix.jobStatus), res.Status)
	}
	return res
}

func requireNoLeakMarkers(t *testing.T, res HandoffCheckResult) {
	t.Helper()
	encoded, err := json.Marshal(res)
	require.NoError(t, err)
	body := string(encoded)
	for _, marker := range []string{
		"USER PROMPT",
		"INTERNAL NARRATION",
		"PARTIAL:",
		"TOOL CALL:",
		"TOOL RESULT:",
		"TEMPORARY CONCLUSION",
		"USER PROMPT FROM CONTEXT",
	} {
		require.NotContains(t, body, marker, "compact running response leaked %q", marker)
	}
}
