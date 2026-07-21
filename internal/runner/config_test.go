package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseArgsPositionalPromptAndDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg, err := ParseArgs([]string{
		"--directory", dir,
		"fix", "the", "bug",
	}, nil, func(string) string { return "" })
	require.NoError(t, err)
	require.Equal(t, "fix the bug", cfg.Prompt)
	require.Equal(t, DefaultModel, cfg.Model)
	require.True(t, cfg.Auto)
	require.Equal(t, DefaultTimeout, cfg.Timeout)
	require.Equal(t, DefaultMaxFinalChars, cfg.MaxFinalChars)
	require.Equal(t, "opencode", cfg.OpenCode)

	canon, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, canon, cfg.Directory)
}

func TestParseArgsStdinPrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg, err := ParseArgs(
		[]string{"--directory", dir},
		strings.NewReader("  do the work  \n"),
		func(string) string { return "" },
	)
	require.NoError(t, err)
	require.Equal(t, "do the work", cfg.Prompt)
}

func TestParseArgsEmptyPromptRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, err := ParseArgs([]string{"--directory", dir}, strings.NewReader("  \n"), nil)
	require.ErrorContains(t, err, "prompt is empty")

	_, err = ParseArgs([]string{"--directory", dir, "  "}, nil, nil)
	require.ErrorContains(t, err, "prompt is empty")
}

func TestParseArgsModelPrecedence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg, err := ParseArgs(
		[]string{"--directory", dir, "--model", "openai/gpt-5", "hi"},
		nil,
		func(k string) string {
			if k == OpenCodeEnvModel {
				return "env/model"
			}
			return ""
		},
	)
	require.NoError(t, err)
	require.Equal(t, "openai/gpt-5", cfg.Model)

	cfg, err = ParseArgs(
		[]string{"--directory", dir, "hi"},
		nil,
		func(k string) string {
			if k == OpenCodeEnvModel {
				return "env/model"
			}
			return ""
		},
	)
	require.NoError(t, err)
	require.Equal(t, "env/model", cfg.Model)
}

func TestParseArgsAutoFalse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg, err := ParseArgs([]string{"--directory", dir, "--auto=false", "hi"}, nil, func(string) string { return "" })
	require.NoError(t, err)
	require.False(t, cfg.Auto)
}

func TestParseArgsDirectoryValidation(t *testing.T) {
	t.Parallel()

	_, err := ParseArgs([]string{"hi"}, nil, nil)
	require.ErrorContains(t, err, "required flag: --directory")

	missing := filepath.Join(t.TempDir(), "nope")
	_, err = ParseArgs([]string{"--directory", missing, "hi"}, nil, nil)
	require.ErrorContains(t, err, "does not exist")

	file := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, err = ParseArgs([]string{"--directory", file, "hi"}, nil, nil)
	require.ErrorContains(t, err, "not a directory")
}

func TestParseArgsMaxFinalCharsCapped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg, err := ParseArgs([]string{
		"--directory", dir,
		"--max-final-chars", "99999",
		"--timeout", "5s",
		"--opencode", "/bin/true",
		"--log-dir", t.TempDir(),
		"hi",
	}, nil, func(string) string { return "" })
	require.NoError(t, err)
	require.Equal(t, HardMaxFinalChars, cfg.MaxFinalChars)
	require.Equal(t, 5*time.Second, cfg.Timeout)
	require.Equal(t, "/bin/true", cfg.OpenCode)
}
