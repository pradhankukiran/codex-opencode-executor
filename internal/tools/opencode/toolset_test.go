package opencode

import (
	"context"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestParseToolset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  Toolset
		err   string
	}{
		{value: "", want: ToolsetFull},
		{value: "full", want: ToolsetFull},
		{value: " FULL ", want: ToolsetFull},
		{value: "core", want: ToolsetCore},
		{value: " Core ", want: ToolsetCore},
		{value: "minimal", err: "must be core or full"},
		{value: "invalid", err: "must be core or full"},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()

			got, err := ParseToolset(tt.value)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestToolsetMembership(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{
		ToolHandoffHealth,
		ToolHandoffModels,
		ToolHandoffExecute,
		ToolHandoffCheck,
		ToolHandoffReview,
		ToolHandoffCancel,
		ToolHandoffDiff,
		ToolHandoffCleanup,
		ToolHandoffPermissionReply,
		ToolHandoffQuestionReply,
	}, ToolsetCore.Tools())

	require.Equal(t, []string{
		ToolHandoffHealth,
		ToolHandoffAgents,
		ToolHandoffModels,
		ToolHandoffSessions,
		ToolHandoffCreateSession,
		ToolHandoffFire,
		ToolHandoffExecute,
		ToolHandoffCheck,
		ToolHandoffReview,
		ToolHandoffCancel,
		ToolHandoffWorkspace,
		ToolHandoffDiff,
		ToolHandoffVerify,
		ToolHandoffCleanup,
		ToolHandoffPermissionReply,
		ToolHandoffQuestionReply,
	}, ToolsetFull.Tools())

	require.True(t, ToolsetCore.Includes(ToolHandoffExecute))
	require.False(t, ToolsetCore.Includes(ToolHandoffAgents))
	require.False(t, ToolsetCore.Includes(ToolHandoffFire))
	require.False(t, ToolsetCore.Includes(ToolHandoffWorkspace))
	require.False(t, ToolsetCore.Includes(ToolHandoffVerify))
	require.True(t, ToolsetFull.Includes(ToolHandoffAgents))
	require.True(t, ToolsetFull.Includes(ToolHandoffVerify))
	require.False(t, Toolset("").Includes(ToolHandoffHealth))
	require.Nil(t, Toolset("").Tools())

	// Core is a subset of full with stable relative order.
	full := ToolsetFull.Tools()
	corePos := -1
	for _, name := range ToolsetCore.Tools() {
		idx := slices.Index(full, name)
		require.GreaterOrEqual(t, idx, 0, "core tool %s missing from full", name)
		require.Greater(t, idx, corePos, "core order diverges from full for %s", name)
		corePos = idx
	}
}

func TestToolCatalogMatchesFullOrder(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, Config{BaseURL: "http://127.0.0.1:1"})
	mgr := NewManager(t.Context(), client, ManagerOptions{})
	catalog := toolCatalog(client, mgr, nil, ExecutorOptions{})

	names := make([]string, 0, len(catalog))
	for _, tool := range catalog {
		names = append(names, tool.name)
	}
	require.Equal(t, ToolsetFull.Tools(), names, "catalog registration order must match full toolset")
}

func TestRegisterToolsetListTools(t *testing.T) {
	t.Parallel()

	for _, toolset := range []Toolset{ToolsetCore, ToolsetFull} {
		t.Run(string(toolset), func(t *testing.T) {
			t.Parallel()

			// MCP SDK lists tools sorted by name; assert exact membership sets.
			require.ElementsMatch(t, toolset.Tools(), listRegisteredTools(t, toolset))
		})
	}
}

func TestRegisterDefaultIsFull(t *testing.T) {
	t.Parallel()

	parsed, err := ParseToolset("")
	require.NoError(t, err)
	require.Equal(t, ToolsetFull, parsed)
	require.ElementsMatch(t, ToolsetFull.Tools(), listRegisteredTools(t, parsed))
}

func listRegisteredTools(t *testing.T, toolset Toolset) []string {
	t.Helper()

	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	client := newTestClient(t, Config{BaseURL: "http://127.0.0.1:1"})
	mgr := NewManager(ctx, client, ManagerOptions{})
	Register(server, client, mgr, nil, ExecutorOptions{}, toolset)

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientSession.Close() })

	res, err := clientSession.ListTools(ctx, nil)
	require.NoError(t, err)

	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}
