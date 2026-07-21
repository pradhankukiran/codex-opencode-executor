package runner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseEventStreamSuccessText(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`{"type":"step_start","timestamp":1,"sessionID":"ses_1","part":{"type":"step-start"}}`,
		`{"type":"reasoning","timestamp":2,"sessionID":"ses_1","part":{"type":"reasoning","text":"secret thoughts"}}`,
		`{"type":"tool_use","timestamp":3,"sessionID":"ses_1","part":{"type":"tool","tool":"bash","state":{"status":"completed","output":"tool out"}}}`,
		`{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"type":"text","text":"Hello world"}}`,
		`{"type":"text","timestamp":5,"sessionID":"ses_1","part":{"type":"text","text":"Done."}}`,
	}, "\n") + "\n"

	summary := ParseEventStream(strings.NewReader(input))
	require.Equal(t, "ses_1", summary.SessionID)
	require.Equal(t, "Hello world\n\nDone.", summary.FinalText)
	require.False(t, summary.HasError)
	require.NotContains(t, summary.FinalText, "secret")
	require.NotContains(t, summary.FinalText, "tool out")
}

func TestParseEventStreamErrorEvent(t *testing.T) {
	t.Parallel()

	input := `{"type":"error","timestamp":1,"sessionID":"ses_err","error":{"name":"ProviderError","data":{"message":"rate limited"}}}` + "\n"
	summary := ParseEventStream(strings.NewReader(input))
	require.Equal(t, "ses_err", summary.SessionID)
	require.True(t, summary.HasError)
	require.Equal(t, "rate limited", summary.ErrorMsg)
}

func TestParseEventStreamMalformedPreservedByCaller(t *testing.T) {
	t.Parallel()

	// Parser skips bad lines for extraction; raw preservation is the event log's job.
	input := strings.Join([]string{
		`not-json`,
		`{"type":"text","timestamp":1,"sessionID":"ses_2","part":{"type":"text","text":"ok"}}`,
		`{broken`,
	}, "\n") + "\n"

	summary := ParseEventStream(strings.NewReader(input))
	require.Equal(t, "ses_2", summary.SessionID)
	require.Equal(t, "ok", summary.FinalText)
}

func TestTruncateFinalText(t *testing.T) {
	t.Parallel()

	text, trunc := TruncateFinalText("hello", 10)
	require.Equal(t, "hello", text)
	require.False(t, trunc)

	text, trunc = TruncateFinalText("abcdefghijKLM", 10)
	require.Equal(t, "abcdefghij", text)
	require.True(t, trunc)

	// Hard max clamp.
	long := strings.Repeat("x", HardMaxFinalChars+50)
	text, trunc = TruncateFinalText(long, HardMaxFinalChars+100)
	require.Equal(t, HardMaxFinalChars, len([]rune(text)))
	require.True(t, trunc)
}

func TestEncodeResult(t *testing.T) {
	t.Parallel()

	code := 0
	var buf bytes.Buffer
	err := EncodeResult(&buf, Result{
		Status:     StatusCompleted,
		SessionID:  "ses_1",
		FinalText:  "hi",
		ExitCode:   &code,
		DurationMS: 12,
	})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, "completed", got["status"])
	require.Equal(t, "ses_1", got["session_id"])
	require.Equal(t, "hi", got["final_text"])
}
