package push

import "testing"

func TestPushAction_String(t *testing.T) {
	tests := []struct {
		action PushAction
		want   string
	}{
		{PushWithRedaction, "push (redacted)"},
		{PushExclude, "exclude"},
		{PushAction(99), "unknown"},
	}
	for _, tt := range tests {
		got := tt.action.String()
		if got != tt.want {
			t.Errorf("PushAction(%d).String() = %q, want %q", tt.action, got, tt.want)
		}
	}
}
