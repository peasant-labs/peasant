package defaults

import "testing"

func TestSourcePathFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		harness Harness
		want    SourcePath
	}{
		{HarnessClaudeCode, DefaultClaudePath},
		{HarnessOpenCode, DefaultOpenCodePath},
		{HarnessCodex, DefaultCodexPath},
		{HarnessCursor, DefaultCursorPath},
		{HarnessStrike, DefaultStrikePath},
		{HarnessPi, DefaultPiPath},
	}
	for _, test := range tests {
		got, ok := SourcePathFor(test.harness)
		if !ok || got != test.want {
			t.Fatalf("SourcePathFor(%q) = %q, %v; want %q, true", test.harness, got, ok, test.want)
		}
	}
	if got, ok := SourcePathFor(Harness("unknown")); ok || got != "" {
		t.Fatalf("SourcePathFor(unknown) = %q, %v; want empty, false", got, ok)
	}
}
