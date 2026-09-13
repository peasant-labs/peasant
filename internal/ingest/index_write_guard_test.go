package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// A parsed output that reaches the writer without its captured input has no
// expected state and no input identity. The batch writer must refuse it with
// a visible error outcome instead of dereferencing the absent capture.
func TestIndexWriteRefusesParsedOutputWithoutCapturedInput(t *testing.T) {
	observer := &captureBatchObserver{}
	// flushIndexParseResults reads metricsStore (the batch writer), versionTargets
	// (defaults when harvesterVersions is nil) and the diagnostics set.
	pipeline := &Pipeline{metricsStore: observer}
	id, err := NewSessionID("ses_uncapturedoutput")
	if err != nil {
		t.Fatal(err)
	}
	text := "parsed without capture"
	parsed := []indexParseResult{{
		im:     indexedMeta{session: DiscoveredSession{SessionID: id, Harness: HarnessClaudeCode}},
		output: indexformat.V1{Entries: []schema.SessionEntry{{SessionID: id, ContentPreview: &text}}},
	}}
	flush := pipeline.flushIndexParseResults(context.Background(), parsed, IndexOutcomeIndexed, "guard test", nil)
	if len(observer.batches) != 0 {
		t.Fatalf("uncaptured output reached the store: batches=%v", observer.batches)
	}
	if len(flush.logEntries) != 1 || flush.logEntries[0].Outcome != IndexOutcomeError || flush.logEntries[0].ErrorMessage == nil {
		t.Fatalf("refusal is not a visible error outcome: %+v", flush.logEntries)
	}
	if !strings.Contains(*flush.logEntries[0].ErrorMessage, id.String()) || !strings.Contains(*flush.logEntries[0].ErrorMessage, "preserved") {
		t.Fatalf("refusal is not actionable: %s", *flush.logEntries[0].ErrorMessage)
	}
	if len(flush.indexed) != 1 || flush.indexed[0].indexed {
		t.Fatalf("refused session was reported as indexed: %+v", flush.indexed)
	}
	refused := false
	for _, diagnostic := range pipeline.snapshotDiagnostics() {
		if diagnostic.ErrorType == "index_refused" && strings.Contains(diagnostic.Message, id.String()) {
			refused = true
		}
	}
	if !refused {
		t.Fatal("refusal was not reported as a run diagnostic")
	}
}
