package opencode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePermissionMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  PermissionMode
		err   string
	}{
		{value: "", want: PermissionModeInherit},
		{value: "inherit", want: PermissionModeInherit},
		{value: "ask", want: PermissionModeAsk},
		{value: "deny", want: PermissionModeDeny},
		{value: "yolo", want: PermissionModeYOLO},
		{value: "allow", want: PermissionModeYOLO},
		{value: " YOLO ", want: PermissionModeYOLO},
		{value: "auto", err: "must be inherit, ask, deny, or yolo"},
		{value: "invalid", err: "must be inherit, ask, deny, or yolo"},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()

			got, err := ParsePermissionMode(tt.value)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestPermissionModeOpenCodePermissionJSON(t *testing.T) {
	t.Parallel()

	require.Empty(t, PermissionModeInherit.OpenCodePermissionJSON())
	require.Equal(t, `{"*":"ask"}`, PermissionModeAsk.OpenCodePermissionJSON())
	require.Equal(t, `{"*":"deny"}`, PermissionModeDeny.OpenCodePermissionJSON())
	require.Equal(t, `{"*":"allow"}`, PermissionModeYOLO.OpenCodePermissionJSON())
}
