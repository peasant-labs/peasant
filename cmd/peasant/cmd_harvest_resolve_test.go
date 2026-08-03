package main

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestResolveHarnessFlag guards the user-facing flag contract: legacy harness
// names get a tailored "renamed to X" error; cursor and antigravity are recognized
// but unsupported without a version commitment; and unknown and valid values stay
// distinct.
func TestResolveHarnessFlag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		raw         string
		want        defaults.Harness
		wantErr     bool
		errContains []string
	}{
		{
			name: "valid claude-code",
			raw:  string(defaults.HarnessClaudeCode),
			want: defaults.HarnessClaudeCode,
		},
		{
			name: "valid opencode",
			raw:  string(defaults.HarnessOpenCode),
			want: defaults.HarnessOpenCode,
		},
		{
			name: "valid codex",
			raw:  string(defaults.HarnessCodex),
			want: defaults.HarnessCodex,
		},
		{
			name:        "legacy claude gets rename hint (C2)",
			raw:         string(defaults.LegacyHarnessClaude),
			wantErr:     true,
			errContains: []string{"deprecated", "renamed", string(defaults.HarnessClaudeCode)},
		},
		{
			name:        "legacy gemini gets rename hint (C2)",
			raw:         string(defaults.LegacyHarnessGemini),
			wantErr:     true,
			errContains: []string{"deprecated", "renamed", string(defaults.HarnessGeminiCLI)},
		},
		{
			name:        "cursor recognized but unsupported (C7)",
			raw:         string(defaults.HarnessCursor),
			wantErr:     true,
			errContains: []string{"planned for a future release"},
		},
		{
			name:        "antigravity recognized but unsupported (C7)",
			raw:         string(defaults.HarnessAntigravity),
			wantErr:     true,
			errContains: []string{"planned for a future release"},
		},
		{
			name:        "unknown value",
			raw:         "bogus-harness",
			wantErr:     true,
			errContains: []string{"unknown harness", "bogus-harness"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveHarnessFlag(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveHarnessFlag(%q): expected error, got nil (result %q)", tc.raw, got)
				}
				for _, sub := range tc.errContains {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q should contain %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveHarnessFlag(%q): unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("resolveHarnessFlag(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
