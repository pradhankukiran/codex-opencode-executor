package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Status values returned in the result JSON.
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
	StatusCancelled = "cancelled"
)

// Result is the single compact JSON object written to the caller's stdout.
type Result struct {
	Status             string `json:"status"`
	SessionID          string `json:"session_id,omitempty"`
	FinalText          string `json:"final_text,omitempty"`
	FinalTextTruncated bool   `json:"final_text_truncated,omitempty"`
	EventLog           string `json:"event_log,omitempty"`
	StderrLog          string `json:"stderr_log,omitempty"`
	ExitCode           *int   `json:"exit_code,omitempty"`
	DurationMS         int64  `json:"duration_ms"`
	Error              string `json:"error,omitempty"`
}

// EventSummary holds fields extracted from OpenCode NDJSON stdout.
type EventSummary struct {
	SessionID string
	FinalText string
	HasError  bool
	ErrorMsg  string
}

type rawEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
	Error     json.RawMessage `json:"error"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseEventStream reads newline-delimited JSON events from r.
// Malformed lines are ignored for extraction but the caller should still
// preserve raw stdout in the event log separately.
func ParseEventStream(r io.Reader) EventSummary {
	var summary EventSummary
	var texts []string

	scanner := bufio.NewScanner(r)
	// OpenCode events can be large; allow up to 10 MiB per line.
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev rawEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.SessionID != "" {
			summary.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "text":
			var part textPart
			if err := json.Unmarshal(ev.Part, &part); err != nil {
				continue
			}
			if part.Type == "text" && part.Text != "" {
				texts = append(texts, part.Text)
			}
		case "error":
			summary.HasError = true
			if msg := formatEventError(ev.Error); msg != "" {
				if summary.ErrorMsg == "" {
					summary.ErrorMsg = msg
				} else {
					summary.ErrorMsg = summary.ErrorMsg + "; " + msg
				}
			} else if summary.ErrorMsg == "" {
				summary.ErrorMsg = "opencode reported an error"
			}
		}
	}

	if len(texts) > 0 {
		summary.FinalText = strings.Join(texts, "\n\n")
	}
	return summary
}

func formatEventError(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Prefer structured {name, data:{message}} from session.error.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if data, ok := obj["data"].(map[string]any); ok {
			if msg, ok := data["message"].(string); ok && strings.TrimSpace(msg) != "" {
				return truncateError(strings.TrimSpace(msg))
			}
		}
		if msg, ok := obj["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return truncateError(strings.TrimSpace(msg))
		}
		if name, ok := obj["name"].(string); ok && strings.TrimSpace(name) != "" {
			return truncateError(strings.TrimSpace(name))
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
		return truncateError(strings.TrimSpace(s))
	}
	return truncateError(string(raw))
}

func truncateError(msg string) string {
	const max = 240
	msg = strings.Join(strings.Fields(msg), " ")
	if utf8.RuneCountInString(msg) <= max {
		return msg
	}
	runes := []rune(msg)
	return string(runes[:max]) + "…"
}

// TruncateFinalText shortens final text to maxChars runes.
func TruncateFinalText(text string, maxChars int) (string, bool) {
	if maxChars <= 0 {
		maxChars = DefaultMaxFinalChars
	}
	if maxChars > HardMaxFinalChars {
		maxChars = HardMaxFinalChars
	}
	if utf8.RuneCountInString(text) <= maxChars {
		return text, false
	}
	runes := []rune(text)
	return string(runes[:maxChars]), true
}

// EncodeResult marshals the result as a single JSON object plus newline.
func EncodeResult(w io.Writer, res Result) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	return nil
}
