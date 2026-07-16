package opencode

import (
	"fmt"
	"strings"
)

// PermissionMode controls how opencode handles tool permission checks.
type PermissionMode string

const (
	// PermissionModeInherit leaves permission decisions to opencode's configuration.
	PermissionModeInherit PermissionMode = "inherit"
	// PermissionModeAsk requires approval for every permission.
	PermissionModeAsk PermissionMode = "ask"
	// PermissionModeDeny blocks every permission.
	PermissionModeDeny PermissionMode = "deny"
	// PermissionModeYOLO allows every permission without approval.
	PermissionModeYOLO PermissionMode = "yolo"
)

// ParsePermissionMode parses an executor permission mode.
func ParsePermissionMode(value string) (PermissionMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(PermissionModeInherit):
		return PermissionModeInherit, nil
	case string(PermissionModeAsk):
		return PermissionModeAsk, nil
	case string(PermissionModeDeny):
		return PermissionModeDeny, nil
	case string(PermissionModeYOLO), "allow":
		return PermissionModeYOLO, nil
	default:
		return "", fmt.Errorf("permission mode %q must be inherit, ask, deny, or yolo", value)
	}
}

// OpenCodePermissionJSON returns the inline opencode permission configuration.
// An empty result means that opencode should inherit its existing configuration.
func (m PermissionMode) OpenCodePermissionJSON() string {
	action, ok := m.action()
	if !ok {
		return ""
	}
	return fmt.Sprintf(`{"*":%q}`, action)
}

func (m PermissionMode) action() (string, bool) {
	switch m {
	case PermissionModeAsk:
		return "ask", true
	case PermissionModeDeny:
		return "deny", true
	case PermissionModeYOLO:
		return "allow", true
	case "", PermissionModeInherit:
		return "", false
	default:
		return "", false
	}
}
