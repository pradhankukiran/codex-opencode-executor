package opencode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLegacyPermissionList(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/permission", r.URL.Path)
		require.Equal(t, "/dir", r.URL.Query().Get("directory"))
		require.Equal(t, "workspace", r.URL.Query().Get("workspace"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"per-1","sessionID":"ses-target","permission":"write_file","patterns":["*.go"],"metadata":{},"always":false,"tool":"bash"},
			{"id":"per-2","session_id":"ses-other","permission":"read_file","patterns":["*"],"metadata":{},"always":false,"tool":"bash"},
			{"id":"per-3","sessionID":"ses-target","permission":"edit_file","patterns":["*.md"],"metadata":{},"always":false,"tool":"bash"}
		]`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	loc := Location{Directory: "/dir", Workspace: "workspace"}

	raw, err := client.Permissions(t.Context(), loc, "ses-target")
	require.NoError(t, err)
	var perms []map[string]any
	require.NoError(t, json.Unmarshal(raw, &perms))
	require.Len(t, perms, 2)
	require.Equal(t, "per-1", perms[0]["id"])
	require.Equal(t, "per-3", perms[1]["id"])

	reqs, err := client.SessionPermissionRequests(t.Context(), loc, "ses-target")
	require.NoError(t, err)
	require.Len(t, reqs, 2)
	require.Equal(t, "per-1", reqs[0]["id"])
}

func TestLegacyPermissionListHTTPErrors(t *testing.T) {
	t.Parallel()

	t.Run("non-2xx", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "permission denied", http.StatusForbidden)
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, Config{BaseURL: server.URL})
		_, err := client.Permissions(t.Context(), Location{}, "ses-1")
		require.ErrorContains(t, err, "GET /permission: status 403 Forbidden: permission denied")
	})

	t.Run("invalid JSON", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`not json`))
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, Config{BaseURL: server.URL})
		_, err := client.SessionPermissionRequests(t.Context(), Location{}, "ses-1")
		require.ErrorContains(t, err, "decode /permission response")
	})

	t.Run("empty sessionID", func(t *testing.T) {
		t.Parallel()
		client := newTestClient(t, Config{BaseURL: "http://127.0.0.1:1"})
		_, err := client.Permissions(t.Context(), Location{}, "")
		require.EqualError(t, err, "session_id is required")
	})
}

func TestLegacyPermissionReply(t *testing.T) {
	t.Parallel()

	type captured struct {
		Method    string
		Path      string
		Directory string
		Workspace string
		Body      map[string]any
	}
	requests := make(chan captured, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body, &decoded))
		requests <- captured{
			Method:    r.Method,
			Path:      r.URL.Path,
			Directory: r.URL.Query().Get("directory"),
			Workspace: r.URL.Query().Get("workspace"),
			Body:      decoded,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`true`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	loc := Location{Directory: "/dir", Workspace: "workspace"}
	raw, err := client.PermissionReply(t.Context(), loc, "ses-1", "per/id", "always", "approved")
	require.NoError(t, err)
	require.JSONEq(t, "true", string(raw))

	got := <-requests
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, "/permission/"+url.PathEscape("per/id")+"/reply", got.Path)
	require.Equal(t, "/dir", got.Directory)
	require.Equal(t, "workspace", got.Workspace)
	require.Equal(t, "always", got.Body["reply"])
	require.Equal(t, "approved", got.Body["message"])
}

func TestLegacyPermissionReplyValidation(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, Config{BaseURL: "http://127.0.0.1:1"})

	_, err := client.PermissionReply(t.Context(), Location{}, "", "per-1", "once", "")
	require.EqualError(t, err, "session_id is required")

	_, err = client.PermissionReply(t.Context(), Location{}, "ses-1", "", "once", "")
	require.EqualError(t, err, "request_id is required")

	_, err = client.PermissionReply(t.Context(), Location{}, "ses-1", "per-1", "invalid", "")
	require.EqualError(t, err, `invalid permission reply "invalid"`)
}

func TestLegacyPermissionReplyHTTPErrors(t *testing.T) {
	t.Parallel()

	t.Run("non-2xx", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, Config{BaseURL: server.URL})
		_, err := client.PermissionReply(t.Context(), Location{}, "ses-1", "per-1", "once", "")
		require.ErrorContains(t, err, "POST /permission/per-1/reply: status 404 Not Found: not found")
	})

	t.Run("invalid JSON", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`not json`))
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, Config{BaseURL: server.URL})
		_, err := client.PermissionReply(t.Context(), Location{}, "ses-1", "per-1", "once", "")
		require.ErrorContains(t, err, "decode permission reply response")
	})
}

func TestLegacyQuestionList(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/question", r.URL.Path)
		require.Equal(t, "/dir", r.URL.Query().Get("directory"))
		require.Equal(t, "workspace", r.URL.Query().Get("workspace"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"que-1","sessionID":"ses-target","questions":[{"question":"What?"}]},
			{"id":"que-2","session_id":"ses-other","questions":[{"question":"Who?"}]},
			{"id":"que-3","sessionID":"ses-target","questions":[{"question":"When?"}]}
		]`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	loc := Location{Directory: "/dir", Workspace: "workspace"}

	raw, err := client.Questions(t.Context(), loc, "ses-target")
	require.NoError(t, err)
	var questions []map[string]any
	require.NoError(t, json.Unmarshal(raw, &questions))
	require.Len(t, questions, 2)
	require.Equal(t, "que-1", questions[0]["id"])
	require.Equal(t, "que-3", questions[1]["id"])

	reqs, err := client.SessionQuestionRequests(t.Context(), loc, "ses-target")
	require.NoError(t, err)
	require.Len(t, reqs, 2)
	require.Equal(t, "que-1", reqs[0]["id"])
}

func TestLegacyQuestionListHTTPErrors(t *testing.T) {
	t.Parallel()

	t.Run("non-2xx", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "server error", http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, Config{BaseURL: server.URL})
		_, err := client.Questions(t.Context(), Location{}, "ses-1")
		require.ErrorContains(t, err, "GET /question: status 500 Internal Server Error: server error")
	})

	t.Run("invalid JSON", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{broken`))
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, Config{BaseURL: server.URL})
		_, err := client.SessionQuestionRequests(t.Context(), Location{}, "ses-1")
		require.ErrorContains(t, err, "decode /question response")
	})

	t.Run("empty sessionID", func(t *testing.T) {
		t.Parallel()
		client := newTestClient(t, Config{BaseURL: "http://127.0.0.1:1"})
		_, err := client.Questions(t.Context(), Location{}, "")
		require.EqualError(t, err, "session_id is required")
	})
}

func TestLegacyQuestionReply(t *testing.T) {
	t.Parallel()

	type captured struct {
		Method      string
		Path        string
		Directory   string
		Workspace   string
		Body        string
		ContentType string
	}
	requests := make(chan captured, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- captured{
			Method:      r.Method,
			Path:        r.URL.Path,
			Directory:   r.URL.Query().Get("directory"),
			Workspace:   r.URL.Query().Get("workspace"),
			Body:        string(body),
			ContentType: r.Header.Get("Content-Type"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`true`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, Config{BaseURL: server.URL})
	loc := Location{Directory: "/dir", Workspace: "workspace"}

	raw, err := client.QuestionReply(t.Context(), loc, "ses-1", "que/id", true, nil)
	require.NoError(t, err)
	require.JSONEq(t, "true", string(raw))

	got := <-requests
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, "/question/"+url.PathEscape("que/id")+"/reject", got.Path)
	require.Equal(t, "/dir", got.Directory)
	require.Equal(t, "workspace", got.Workspace)
	require.Empty(t, got.Body)
	require.Empty(t, got.ContentType)

	raw, err = client.QuestionReply(t.Context(), loc, "ses-1", "que/id", false, [][]string{{"a", "b"}, {"c"}})
	require.NoError(t, err)
	require.JSONEq(t, "true", string(raw))

	got = <-requests
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, "/question/"+url.PathEscape("que/id")+"/reply", got.Path)
	require.Equal(t, "/dir", got.Directory)
	require.Equal(t, "workspace", got.Workspace)
	require.JSONEq(t, `{"answers":[["a","b"],["c"]]}`, got.Body)
	require.Equal(t, "application/json", got.ContentType)
}

func TestLegacyQuestionReplyValidation(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, Config{BaseURL: "http://127.0.0.1:1"})

	_, err := client.QuestionReply(t.Context(), Location{}, "", "que-1", false, nil)
	require.EqualError(t, err, "session_id is required")

	_, err = client.QuestionReply(t.Context(), Location{}, "ses-1", "", false, nil)
	require.EqualError(t, err, "request_id is required")
}

func TestLegacyQuestionReplyHTTPErrors(t *testing.T) {
	t.Parallel()

	t.Run("non-2xx", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "conflict", http.StatusConflict)
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, Config{BaseURL: server.URL})
		_, err := client.QuestionReply(t.Context(), Location{}, "ses-1", "que-1", false, [][]string{{"a"}})
		require.ErrorContains(t, err, "POST /question/que-1/reply: status 409 Conflict: conflict")
	})

	t.Run("invalid JSON", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`not json`))
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, Config{BaseURL: server.URL})
		_, err := client.QuestionReply(t.Context(), Location{}, "ses-1", "que-1", true, nil)
		require.ErrorContains(t, err, "decode question reply response")
	})

	t.Run("empty body treated as success", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)

		client := newTestClient(t, Config{BaseURL: server.URL})
		raw, err := client.QuestionReply(t.Context(), Location{}, "ses-1", "que-1", true, nil)
		require.NoError(t, err)
		require.JSONEq(t, "{}", string(raw))
	})
}
