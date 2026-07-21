package runner

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildOpenCodeArgsFixedAgentAndShape(t *testing.T) {
	t.Parallel()

	args := BuildOpenCodeArgs(Config{
		Directory: "/proj",
		Prompt:    "do work",
		Model:     "xai/grok-4.5",
		Auto:      true,
	})
	require.Equal(t, []string{
		"run",
		"--agent", "build",
		"--model", "xai/grok-4.5",
		"--auto",
		"--format", "json",
		"--dir", "/proj",
		"do work",
	}, args)
	require.NotContains(t, args, "codex-opencode-executor")
}

func TestBuildOpenCodeArgsAutoOptOut(t *testing.T) {
	t.Parallel()

	args := BuildOpenCodeArgs(Config{
		Directory: "/proj",
		Prompt:    "prompt with spaces",
		Model:     "openai/gpt-5",
		Auto:      false,
	})
	require.Equal(t, []string{
		"run",
		"--agent", "build",
		"--model", "openai/gpt-5",
		"--format", "json",
		"--dir", "/proj",
		"prompt with spaces",
	}, args)
	// Prompt is a single argv element, not split.
	require.Equal(t, "prompt with spaces", args[len(args)-1])
}
