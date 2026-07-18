package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pradhankukiran/codex-opencode-executor/internal/tools/opencode"
)

func TestWaitForTCP(t *testing.T) {
	t.Run("listener ready", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })

		done := make(chan error, 1)
		err = waitForTCP(t.Context(), "http://"+listener.Addr().String(), done, time.Second)
		require.NoError(t, err)
	})

	t.Run("process exits", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		address := listener.Addr().String()
		require.NoError(t, listener.Close())

		done := make(chan error, 1)
		done <- errors.New("startup failed")
		err = waitForTCP(t.Context(), "http://"+address, done, time.Second)
		require.EqualError(t, err, "opencode serve exited before accepting connections: startup failed")
		require.EqualError(t, <-done, "startup failed")
	})

	t.Run("context canceled", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		address := listener.Addr().String()
		require.NoError(t, listener.Close())

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err = waitForTCP(ctx, "http://"+address, make(chan error, 1), time.Second)
		require.ErrorIs(t, err, context.Canceled)
	})
}

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
