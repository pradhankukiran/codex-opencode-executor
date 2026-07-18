package opencode

import (
	"fmt"
	"strings"
)

// Toolset selects which MCP handoff tools are registered.
type Toolset string

const (
	// ToolsetCore registers the minimal token-efficient handoff surface.
	ToolsetCore Toolset = "core"
	// ToolsetFull registers every handoff tool (default for backward compatibility).
	ToolsetFull Toolset = "full"
)

// Tool names in deterministic registration order.
const (
	ToolHandoffHealth          = "handoff_health"
	ToolHandoffAgents          = "handoff_agents"
	ToolHandoffModels          = "handoff_models"
	ToolHandoffSessions        = "handoff_sessions"
	ToolHandoffCreateSession   = "handoff_create_session"
	ToolHandoffFire            = "handoff_fire"
	ToolHandoffExecute         = "handoff_execute"
	ToolHandoffCheck           = "handoff_check"
	ToolHandoffReview          = "handoff_review"
	ToolHandoffCancel          = "handoff_cancel"
	ToolHandoffWorkspace       = "handoff_workspace"
	ToolHandoffDiff            = "handoff_diff"
	ToolHandoffVerify          = "handoff_verify"
	ToolHandoffCleanup         = "handoff_cleanup"
	ToolHandoffPermissionReply = "handoff_permission_reply"
	ToolHandoffQuestionReply   = "handoff_question_reply"
)

// fullToolOrder is the authoritative registration order for ToolsetFull.
var fullToolOrder = []string{
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
}

// coreToolOrder is the authoritative registration order for ToolsetCore.
// Relative order matches fullToolOrder.
var coreToolOrder = []string{
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
}

var coreToolMembership = func() map[string]struct{} {
	m := make(map[string]struct{}, len(coreToolOrder))
	for _, name := range coreToolOrder {
		m[name] = struct{}{}
	}
	return m
}()

// ParseToolset parses a toolset setting. Empty defaults to full.
func ParseToolset(value string) (Toolset, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(ToolsetFull):
		return ToolsetFull, nil
	case string(ToolsetCore):
		return ToolsetCore, nil
	default:
		return "", fmt.Errorf("toolset %q must be core or full", value)
	}
}

// Tools returns the deterministic tool name list for this toolset.
func (t Toolset) Tools() []string {
	switch t {
	case ToolsetCore:
		return append([]string(nil), coreToolOrder...)
	case ToolsetFull:
		return append([]string(nil), fullToolOrder...)
	default:
		return nil
	}
}

// Includes reports whether name is registered for this toolset.
func (t Toolset) Includes(name string) bool {
	switch t {
	case ToolsetCore:
		_, ok := coreToolMembership[name]
		return ok
	case ToolsetFull:
		for _, n := range fullToolOrder {
			if n == name {
				return true
			}
		}
		return false
	default:
		return false
	}
}
