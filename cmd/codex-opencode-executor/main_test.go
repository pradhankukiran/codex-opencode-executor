package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhankukiran/codex-opencode-executor/internal/tools/opencode"
)

func TestExecutorOptionsPermissionMode(t *testing.T) {
	t.Parallel()

	t.Run("configured mode", func(t *testing.T) {
		cfg := opencodeCfg{
			DefaultModel:   "xai/grok-4.5",
			PermissionMode: "ask",
		}
		opts, err := cfg.ExecutorOptions()
		require.NoError(t, err)
		require.Equal(t, opencode.PermissionModeAsk, opts.DefaultPermission)
	})

	t.Run("yolo shortcut", func(t *testing.T) {
		cfg := opencodeCfg{
			DefaultModel:   "xai/grok-4.5",
			PermissionMode: "inherit",
			YOLO:           true,
		}
		opts, err := cfg.ExecutorOptions()
		require.NoError(t, err)
		require.Equal(t, opencode.PermissionModeYOLO, opts.DefaultPermission)
	})

	t.Run("yolo overrides configured mode", func(t *testing.T) {
		cfg := opencodeCfg{
			DefaultModel:   "xai/grok-4.5",
			PermissionMode: "deny",
			YOLO:           true,
		}
		opts, err := cfg.ExecutorOptions()
		require.NoError(t, err)
		require.Equal(t, opencode.PermissionModeYOLO, opts.DefaultPermission)
	})
}

func TestLocalEnvironmentPermission(t *testing.T) {
	t.Parallel()

	cfg := opencodeCfg{Env: repeatFlag{"EXISTING=value"}}

	require.Equal(t,
		[]string{"EXISTING=value"},
		cfg.localEnvironment(opencode.PermissionModeInherit),
	)
	require.Equal(t,
		[]string{"EXISTING=value", `OPENCODE_PERMISSION={"*":"allow"}`},
		cfg.localEnvironment(opencode.PermissionModeYOLO),
	)
}
