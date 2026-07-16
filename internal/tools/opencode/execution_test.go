package opencode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseModelRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  ModelRef
		err   string
	}{
		{name: "empty", value: "", want: ModelRef{}},
		{name: "provider and model", value: "xai/grok-4.5", want: ModelRef{ProviderID: "xai", ModelID: "grok-4.5"}},
		{name: "nested model id", value: "openrouter/anthropic/claude-sonnet-4", want: ModelRef{ProviderID: "openrouter", ModelID: "anthropic/claude-sonnet-4"}},
		{name: "trimmed", value: " xai / grok-4.5 ", want: ModelRef{ProviderID: "xai", ModelID: "grok-4.5"}},
		{name: "missing provider", value: "/grok-4.5", err: "must use provider/model format"},
		{name: "missing model", value: "xai/", err: "must use provider/model format"},
		{name: "missing separator", value: "grok-4.5", err: "must use provider/model format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseModelRef(tt.value)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.want.String(), got.String())
		})
	}
}

func TestExecutorOptionsSessionRequest(t *testing.T) {
	t.Parallel()

	opts := ExecutorOptions{
		DefaultModel: ModelRef{ProviderID: "xai", ModelID: "grok-4.5"},
		DefaultAgent: "build",
	}

	t.Run("defaults", func(t *testing.T) {
		req, err := opts.sessionRequest(createSessionParams{Title: "task"})
		require.NoError(t, err)
		require.Equal(t, "xai", req.ProviderID)
		require.Equal(t, "grok-4.5", req.ModelID)
		require.Equal(t, "build", req.Agent)
	})

	t.Run("model and agent overrides", func(t *testing.T) {
		req, err := opts.sessionRequest(createSessionParams{
			Model: "anthropic/claude-sonnet-4",
			Agent: "review",
		})
		require.NoError(t, err)
		require.Equal(t, "anthropic", req.ProviderID)
		require.Equal(t, "claude-sonnet-4", req.ModelID)
		require.Equal(t, "review", req.Agent)
	})

	t.Run("legacy provider and model override", func(t *testing.T) {
		req, err := opts.sessionRequest(createSessionParams{
			ProviderID: "openai",
			ModelID:    "gpt-5",
		})
		require.NoError(t, err)
		require.Equal(t, "openai", req.ProviderID)
		require.Equal(t, "gpt-5", req.ModelID)
	})

	t.Run("rejects mixed model inputs", func(t *testing.T) {
		_, err := opts.sessionRequest(createSessionParams{
			Model:      "xai/grok-4.5",
			ProviderID: "xai",
			ModelID:    "grok-4.5",
		})
		require.EqualError(t, err, "model cannot be combined with provider_id or model_id")
	})

	t.Run("rejects incomplete legacy model", func(t *testing.T) {
		_, err := opts.sessionRequest(createSessionParams{ProviderID: "xai"})
		require.EqualError(t, err, "provider_id and model_id must be provided together")
	})
}

func TestExecutorOptionsPromptRequest(t *testing.T) {
	t.Parallel()

	opts := ExecutorOptions{DefaultAgent: "build"}

	defaulted := opts.promptRequest(fireParams{Prompt: "implement it"})
	require.Equal(t, "build", defaulted.Agent)
	require.Equal(t, "implement it", defaulted.Prompt.Text)

	overridden := opts.promptRequest(fireParams{Prompt: "review it", Agent: "review"})
	require.Equal(t, "review", overridden.Agent)
}
