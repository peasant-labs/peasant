package ingest

import (
	"sort"
	"testing"

	"github.com/peasant-labs/schema"
)

// TestControlRecordKindParity asserts that the stored control vocabulary the
// writers can produce is exactly the vocabulary ControlRecordKindFor classifies
// as a rendered control kind. It enumerates the Claude census
// (claudeControlKindLabels) through the wire→stored-part-type alias rather than
// a hand-written list, expands the artifact- prefix family with one
// representative, and reads the Codex writer census (codexNativeControlKinds).
// The compaction notices are negatives: a writer stores them as system rows, and
// the reader must never classify them as control rows.
func TestControlRecordKindParity(t *testing.T) {
	t.Parallel()

	// Claude: map each census label to the stored part type the writer emits.
	claudePositives := make(map[string]bool)
	claudeNegatives := make(map[string]bool)
	for _, label := range claudeControlKindLabels() {
		switch label {
		case "compact_boundary":
			// The wire form names the system subtype; the writer stores the
			// hyphenated part type.
			claudeNegatives[ClaudeCompactBoundaryLabel] = true
		case claudeArtifactControlPrefix:
			// The census names a prefix family; one representative stands for
			// every member.
			claudePositives["artifact-transcript"] = true
		default:
			claudePositives[label] = true
		}
	}
	if len(claudePositives) == 0 {
		t.Fatal("claude control census produced no positives; the parity test would be vacuous")
	}
	for kind := range claudePositives {
		if got := ControlRecordKindFor(schema.HarnessClaudeCode, kind); got != kind {
			t.Errorf("Claude stored control kind %q classified as %q, want %q (ControlRecordKindFor must consult the writer's own predicate)", kind, got, kind)
		}
	}
	for kind := range claudeNegatives {
		if claudePositives[kind] {
			t.Fatalf("Claude notice %q is also a positive; a compaction notice is never a control kind", kind)
		}
		if got := ControlRecordKindFor(schema.HarnessClaudeCode, kind); got != "" {
			t.Errorf("Claude compaction notice %q classified as %q, want no control kind", kind, got)
		}
	}

	// Codex: iterate the writer's own census (codexNativeControlKinds), not a
	// hand copy, plus the compaction notice as the one negative row. A widened
	// writer arm must extend a census slice, so this test sees it.
	codexPositives := codexNativeControlKinds()
	if len(codexPositives) == 0 {
		t.Fatal("Codex control census produced no positives; the parity test would be vacuous")
	}
	for _, kind := range codexPositives {
		if got := ControlRecordKindFor(schema.HarnessCodex, kind); got != kind {
			t.Errorf("Codex stored control kind %q classified as %q, want %q", kind, got, kind)
		}
	}
	if got := ControlRecordKindFor(schema.HarnessCodex, CodexTypeCompacted); got != "" {
		t.Errorf("Codex compaction notice %q classified as %q, want no control kind", CodexTypeCompacted, got)
	}

	// The classification is harness-keyed: a kind one harness stores is not a
	// control kind for another, and harnesses with no control vocabulary
	// classify nothing.
	for _, kind := range []string{codexTypeWorldState, codexEventPatchApplyEnd} {
		if got := ControlRecordKindFor(schema.HarnessClaudeCode, kind); got != "" {
			t.Errorf("Codex kind %q classified as %q for Claude, want none", kind, got)
		}
	}
	for _, kind := range []string{"mode", "attachment", "artifact-transcript"} {
		if got := ControlRecordKindFor(schema.HarnessCodex, kind); got != "" {
			t.Errorf("Claude kind %q classified as %q for Codex, want none", kind, got)
		}
	}
	for _, harness := range []schema.Harness{
		schema.HarnessOpenCode, schema.HarnessPi, schema.HarnessCursor,
		schema.HarnessStrike, schema.HarnessGeminiCLI,
	} {
		for _, kind := range []string{"text", "reasoning", PiCarrierPartType, claudeArtifactControlPrefix + "transcript"} {
			if got := ControlRecordKindFor(harness, kind); got != "" {
				t.Errorf("harness %q stored part %q classified as %q, want none", harness, kind, got)
			}
		}
	}

	// A readable failure names the exact census when a future writer widens it
	// without the reader following.
	missing := make([]string, 0)
	for kind := range claudePositives {
		if ControlRecordKindFor(schema.HarnessClaudeCode, kind) == "" {
			missing = append(missing, kind)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("Claude control kinds the writer stores but the reader ignores: %v; widen ControlRecordKindFor with the writer's predicate", missing)
	}
}
