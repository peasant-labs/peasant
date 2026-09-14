package ingest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	openCodeNativeChild      = "ses_3cd91f52effeXd3QAJ54jOyzn1"
	openCodeNativeParent     = "ses_opencodeNativeParent"
	openCodeNativeForkSource = openCodeNativeParent
)

// TestOpenCodeProvenanceNativeReadOnlySnapshot proves the production snapshot
// path over a real synthetic OpenCode SQLite source: the typed user row with an
// explicit parent null admits one submission, the read issues SELECTs only, and
// the source's bytes and logical rows are unchanged.
func TestOpenCodeProvenanceNativeReadOnlySnapshot(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	before := testfixture.SnapshotSource(t, source)
	rowsBefore := countMaterializedCurrentRows(t, source.Path, openCodeNativeChild)

	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
	}
	if !snapshot.ParentNullProven || snapshot.HasParent {
		t.Fatalf("parentage = hasParent %t parentNullProven %t, want an explicit parent null", snapshot.HasParent, snapshot.ParentNullProven)
	}
	if len(snapshot.Messages) != 2 {
		t.Fatalf("snapshot rows = %d, want 2", len(snapshot.Messages))
	}
	if snapshot.SourceEvidenceDigest == "" {
		t.Fatal("snapshot carries no source evidence digest")
	}
	built := buildOpenCodeNativeGeneration(t, snapshot)
	if built.Generation.Metadata.Stats.InputSubmissionCount == nil || *built.Generation.Metadata.Stats.InputSubmissionCount != 1 {
		t.Fatalf("inputSubmissionCount = %v, want 1", built.Generation.Metadata.Stats.InputSubmissionCount)
	}
	if len(built.Generation.TitleRefs) != 1 {
		t.Fatalf("titleRefs = %v, want one admitted prose ref", built.Generation.TitleRefs)
	}

	testfixture.AssertUnchanged(t, source, before)
	if got := countMaterializedCurrentRows(t, source.Path, openCodeNativeChild); got != rowsBefore {
		t.Fatalf("child source rows = %d after the read, want %d", got, rowsBefore)
	}
}

// TestOpenCodeProvenanceParentDeleteCannotEraseManagedPrefix proves the copy
// proof is captured into the immutable generation: the reader copies the
// settled source rows through the same read-only source, and deleting the
// parent session and its rows afterwards cannot blank the built prefix.
func TestOpenCodeProvenanceParentDeleteCannotEraseManagedPrefix(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	_ = testfixture.SnapshotSource(t, source)
	applyOpenCodeNativeSetup(t, source, `
INSERT INTO session (id, parent_id, time_created, time_updated) VALUES ('`+openCodeNativeParent+`', '', 500, 2000);
UPDATE session SET parent_id = '`+openCodeNativeParent+`' WHERE id = '`+openCodeNativeChild+`';
INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES
  ('msg_parent_user', '`+openCodeNativeParent+`', 'user', 500, 500, '{"id":"msg_parent_user","type":"user","text":"captured parent question","time":{"created":500}}', 0),
  ('msg_parent_assistant', '`+openCodeNativeParent+`', 'assistant', 500, 600, '{"id":"msg_parent_assistant","type":"assistant","agent":"build","model":{"id":"synthetic-model","providerID":"synthetic-provider"},"content":[{"type":"text","id":"prt_parent_text","text":"captured parent answer"}],"time":{"created":500,"completed":600}}', 1);
`)

	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{
		Fork: &ingest.OpenCodeForkProof{SourceSessionID: openCodeNativeForkSource, ThroughSeq: int64Ptr(1), ThroughCompleted: true},
	})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
	}
	if len(snapshot.Copied) != 2 {
		t.Fatalf("captured source copies = %d, want 2", len(snapshot.Copied))
	}
	for _, copied := range snapshot.Copied {
		if copied.SourceMessageID == "" || copied.SourceSessionID != openCodeNativeForkSource {
			t.Fatalf("copied row %q lost its source evidence: %+v", copied.MessageID, copied)
		}
	}
	contentDigest := snapshot.SourceEvidenceDigest
	built := buildOpenCodeNativeGeneration(t, snapshot)
	if len(built.Generation.Segments) != 1 || len(built.Generation.Segments[0].CapturedRefs) == 0 {
		t.Fatalf("built generation has no captured prefix: %+v", built.Generation.Segments)
	}
	inheritedRefs := built.Generation.Segments[0].CapturedRefs

	applyOpenCodeNativeSetup(t, source, `
DELETE FROM session_message WHERE session_id = '`+openCodeNativeParent+`';
DELETE FROM session WHERE id = '`+openCodeNativeParent+`';
`)
	if got := countMaterializedCurrentRows(t, source.Path, openCodeNativeParent); got != 0 {
		t.Fatalf("parent source rows = %d after deletion, want 0", got)
	}
	if built.Generation.SourceEvidenceDigest != contentDigest {
		t.Fatal("deleting the parent changed the captured generation evidence digest")
	}
	contentByRef := map[schema.SourceEntryRef]indexformat.ContentRecord{}
	for _, record := range built.Generation.Content {
		contentByRef[record.Ref] = record
	}
	for _, ref := range inheritedRefs {
		if _, ok := contentByRef[ref]; !ok {
			t.Fatalf("inherited ref %q has no captured content after the parent delete", ref)
		}
	}
}

// TestOpenCodeProvenanceIndexerWiring proves the candidate leaves the existing
// indexer entry point as a validated format-2 result. It also proves the path
// fails closed when its dependencies are missing rather than degrading to a
// thinner V1 result.
func TestOpenCodeProvenanceIndexerWiring(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	session := ingest.DiscoveredSession{
		SessionID:        ingest.SessionID(openCodeNativeChild),
		Harness:          ingest.HarnessOpenCode,
		TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
	}
	config := ingest.OpenCodeProvenanceIndexerConfig{
		Enabled: true,
		Snapshot: func(ctx context.Context, _ ingest.DiscoveredSession) (ingest.OpenCodeHistorySnapshot, error) {
			return adapter.SnapshotOpenCodeProvenance(ctx, source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{})
		},
		Metadata: func(_ ingest.DiscoveredSession) (schema.UnifiedMetadata, error) {
			return schema.UnifiedMetadata{SchemaVersion: 1, SessionID: schema.SessionID(openCodeNativeChild), ModelHarness: schema.HarnessOpenCode}, nil
		},
		GenerationID: func(_ ingest.DiscoveredSession) string { return "gen-native-wiring" },
	}
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	result, err := indexer.IndexTranscriptResult(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscriptResult: %v", err)
	}
	format, ok := result.(indexformat.V2)
	if !ok {
		t.Fatalf("IndexTranscriptResult returned %T, want indexformat.V2", result)
	}
	if format.Generation.Metadata.Stats.InputSubmissionCount == nil || *format.Generation.Metadata.Stats.InputSubmissionCount != 1 {
		t.Fatalf("inputSubmissionCount = %v, want 1", format.Generation.Metadata.Stats.InputSubmissionCount)
	}
	if format.Generation.ID != "gen-native-wiring" {
		t.Fatalf("generation id = %q, want gen-native-wiring", format.Generation.ID)
	}

	missing := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(ingest.OpenCodeProvenanceIndexerConfig{Enabled: true}))
	if _, err := missing.IndexTranscriptResult(context.Background(), session); err == nil || !strings.Contains(err.Error(), "misses its snapshot, metadata, or generation dependency") {
		t.Fatalf("IndexTranscriptResult with missing dependencies = %v, want a fail-closed dependency refusal", err)
	}
}

func buildOpenCodeNativeGeneration(t *testing.T, snapshot ingest.OpenCodeHistorySnapshot) indexformat.V2 {
	t.Helper()
	capture, segmentKeys, err := ingest.BuildOpenCodeProvenanceCapture(snapshot, "gen-native", schema.UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     schema.SessionID(openCodeNativeChild),
		ModelHarness:  schema.HarnessOpenCode,
	})
	if err != nil {
		t.Fatalf("BuildOpenCodeProvenanceCapture: %v", err)
	}
	built, err := ingest.BuildV2(capture, ingest.RandomRefAllocator{})
	if err != nil {
		t.Fatalf("BuildV2: %v", err)
	}
	if err := ingest.ResolveOpenCodeSegmentRefs(segmentKeys, &built.Generation); err != nil {
		t.Fatalf("ResolveOpenCodeSegmentRefs: %v", err)
	}
	return built
}

func applyOpenCodeNativeSetup(t *testing.T, source testfixture.MaterializedSource, script string) {
	t.Helper()
	_ = testfixture.SnapshotSource(t, source)
	connection, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(connection, script, nil); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
}

func int64Ptr(value int64) *int64 { return &value }
