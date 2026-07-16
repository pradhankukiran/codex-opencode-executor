// Package mcputil provides helpers for registering MCP tools.
package mcputil

import (
	"context"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolFlags defines traits of an MCP tool.
type ToolFlags uint32

const (
	// ReadOnly indicates that a tool has no side effects.
	ReadOnly ToolFlags = 1 << iota
	// Destructive indicates that a tool can delete, modify, or stop resources.
	Destructive
)

// Has reports whether the flags contain target.
func (f ToolFlags) Has(target ToolFlags) bool {
	return f&target != 0
}

// ToolDef contains the metadata used to register an MCP tool.
type ToolDef struct {
	Name        string
	Description string
	Flags       ToolFlags
}

// Register adds a typed MCP tool with the appropriate annotations.
func Register[In, Out any](s *mcp.Server, def ToolDef, handler mcp.ToolHandlerFor[In, Out]) {
	readOnly := def.Flags.Has(ReadOnly)
	annotations := &mcp.ToolAnnotations{
		ReadOnlyHint: readOnly,
	}
	if !readOnly {
		destructive := def.Flags.Has(Destructive)
		annotations.DestructiveHint = &destructive
	}

	wrapped := func(ctx context.Context, req *mcp.CallToolRequest, args In) (*mcp.CallToolResult, Out, error) {
		slog.Info("called tool handler", "tool", def.Name, "session_id", req.Session.ID())
		return handler(ctx, req, args)
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        def.Name,
		Description: def.Description,
		Annotations: annotations,
	}, wrapped)
}
