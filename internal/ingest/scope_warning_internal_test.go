package ingest

import (
	"strings"
	"testing"
)

// TestUnreadableWarningNamesTheArtifactTheCommandLost pins the wording of the
// stored-but-unreadable warning across the harness that recorded the session
// and the command that read it: a Claude source may be retained by Claude's own
// policy, another harness's source is not described as Claude's, and an index
// run names the saved pair it could not read rather than a transcript it never
// asked for.
func TestUnreadableWarningNamesTheArtifactTheCommandLost(t *testing.T) {
	t.Parallel()
	const sessionID = SessionID("5a1f0c3e-7b2d-4e8a-9c61-0d4b3e2f1a70")
	const path = ResolvedPath("/sources/5a1f0c3e-7b2d-4e8a-9c61-0d4b3e2f1a70.jsonl")
	gone := func(harness Harness) ScopeReport {
		return ScopeReport{Members: []LocatedMember{{
			ID:         sessionID,
			Harness:    harness,
			Role:       CohortSelected,
			Outcome:    LocateStoredUnreadable,
			Unreadable: UnreadableSourceGone,
			StoredPath: path,
		}}}
	}
	block := func(report ScopeReport, command string) string {
		warnings := report.UnreadableWarnings(command, "")
		if len(warnings) != 1 {
			t.Fatalf("%s emitted %d warnings, want one", command, len(warnings))
		}
		return warnings[0]
	}

	claude := block(gone(HarnessClaudeCode), "harvest")
	if !strings.Contains(claude, "(Claude Code removes old transcripts after its retention period") {
		t.Errorf("a Claude source does not name Claude's retention:\n%s", claude)
	}
	if !strings.Contains(claude, string(path)) {
		t.Errorf("the warning does not name the recorded path:\n%s", claude)
	}

	codex := block(gone(HarnessCodex), "harvest")
	if strings.Contains(codex, "Claude Code") {
		t.Errorf("a Codex source is described as Claude's:\n%s", codex)
	}
	if !strings.Contains(codex, "the source path recorded for this session no longer holds it") {
		t.Errorf("a Codex source does not say the path is gone:\n%s", codex)
	}

	index := block(gone(HarnessClaudeCode), "harvest index")
	for _, want := range []string{"indexed copy", "saved pair", "harvest index --session"} {
		if !strings.Contains(index, want) {
			t.Errorf("an index warning does not say %q:\n%s", want, index)
		}
	}
	for _, unwanted := range []string{"restore a transcript", "Claude Code removes old transcripts", "stored transcript was kept"} {
		if strings.Contains(index, unwanted) {
			t.Errorf("an index warning claims %q:\n%s", unwanted, index)
		}
	}
}
