package ingest_test

import (
	"context"
	"errors"
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
// proof is captured into the immutable generation and survives a full
// activation/refresh cycle: the first candidate captures the settled parent
// rows, the activation-derived prior keeps their identities, and after the
// parent session and its rows are deleted the production refresh path rebuilds
// the child from the retained prefix with the same ordered refs, boundary and
// full content bytes.
func TestOpenCodeProvenanceParentDeleteCannotEraseManagedPrefix(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	seedOpenCodeNativeForkParent(t, source)

	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	fork := &ingest.OpenCodeForkProof{SourceSessionID: openCodeNativeForkSource, ThroughSeq: int64Ptr(1), ThroughCompleted: true}
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{Fork: fork})
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
	first := buildOpenCodeNativeGeneration(t, snapshot)
	if len(first.Generation.Segments) != 1 || len(first.Generation.Segments[0].CapturedRefs) == 0 {
		t.Fatalf("built generation has no captured prefix: %+v", first.Generation.Segments)
	}
	inheritedRefs := append([]schema.SourceEntryRef(nil), first.Generation.Segments[0].CapturedRefs...)
	firstDigest := first.Generation.SourceEvidenceDigest
	firstContent := openCodeContentByRef(first.Generation)

	// The activation owns persisting the alias state and the captured prefix;
	// the test derives them exactly as an activation would.
	state, err := ingest.PriorStateFromGeneration(first.Generation)
	if err != nil {
		t.Fatalf("PriorStateFromGeneration: %v", err)
	}
	prior := ingest.OpenCodeProvenancePrior{
		Aliases:               state,
		CapturedPrefix:        append([]ingest.OpenCodeHistoryRow(nil), snapshot.Copied...),
		HasCapturedPrefix:     true,
		HasCompleteGeneration: true,
	}

	applyOpenCodeNativeSetup(t, source, `
DELETE FROM session_message WHERE session_id = '`+openCodeNativeParent+`';
DELETE FROM session WHERE id = '`+openCodeNativeParent+`';
`)
	if got := countMaterializedCurrentRows(t, source.Path, openCodeNativeParent); got != 0 {
		t.Fatalf("parent source rows = %d after deletion, want 0", got)
	}

	session := ingest.DiscoveredSession{
		SessionID:        ingest.SessionID(openCodeNativeChild),
		Harness:          ingest.HarnessOpenCode,
		TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
	}
	config := openCodeNativeProvenanceConfig(source, openCodeNativeChild, prior, fork)
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	result, err := indexer.IndexTranscriptResult(context.Background(), session)
	if err != nil {
		t.Fatalf("refresh after parent delete: %v", err)
	}
	refreshed, ok := result.(indexformat.V2)
	if !ok {
		t.Fatalf("refresh returned %T, want indexformat.V2", result)
	}
	if refreshed.Generation.SourceEvidenceDigest != firstDigest {
		t.Fatal("deleting the parent changed the captured generation evidence digest")
	}
	if len(refreshed.Generation.Segments) != 1 {
		t.Fatalf("refreshed segments = %d, want 1", len(refreshed.Generation.Segments))
	}
	gotRefs := refreshed.Generation.Segments[0].CapturedRefs
	if !equalSourceRefSlices(gotRefs, inheritedRefs) {
		t.Fatalf("refreshed captured refs = %v, want %v", gotRefs, inheritedRefs)
	}
	refreshedContent := openCodeContentByRef(refreshed.Generation)
	for _, ref := range inheritedRefs {
		firstRecord, ok := firstContent[ref]
		if !ok {
			t.Fatalf("first generation lost captured content for ref %q", ref)
		}
		refreshedRecord, ok := refreshedContent[ref]
		if !ok {
			t.Fatalf("refreshed generation lost captured content for ref %q", ref)
		}
		if firstRecord.ByteLength != refreshedRecord.ByteLength || firstRecord.Digest != refreshedRecord.Digest {
			t.Fatalf("refreshed content for ref %q changed: %+v -> %+v", ref, firstRecord, refreshedRecord)
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

// TestOpenCodeProvenanceUnchangedCaptureKeepsIdentities proves the production
// indexer reuses every prior identity: repeating the candidate on unchanged
// native SQL keeps the exact block, submission, alias and segment refs, an
// append keeps the existing refs and adds only new ones, and a retry reuses the
// same allocations.
func TestOpenCodeProvenanceUnchangedCaptureKeepsIdentities(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	session := ingest.DiscoveredSession{
		SessionID:        ingest.SessionID(openCodeNativeChild),
		Harness:          ingest.HarnessOpenCode,
		TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
	}
	first := indexOpenCodeNative(t, source, session, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}, nil)
	state, err := ingest.PriorStateFromGeneration(first.Generation)
	if err != nil {
		t.Fatalf("PriorStateFromGeneration: %v", err)
	}
	prior := ingest.OpenCodeProvenancePrior{Aliases: state, HasCompleteGeneration: true}

	repeated := indexOpenCodeNative(t, source, session, prior, nil)
	assertOpenCodeExistingIdentitiesStable(t, "repeat", first.Generation, repeated.Generation)
	retried := indexOpenCodeNative(t, source, session, prior, nil)
	assertOpenCodeExistingIdentitiesStable(t, "retry", first.Generation, retried.Generation)

	applyOpenCodeNativeSetup(t, source, `INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES
  ('msg_native_append', '`+openCodeNativeChild+`', 'user', 2000, 2000, '{"id":"msg_native_append","type":"user","text":"appended question","time":{"created":2000}}', 2);
`)
	appended := indexOpenCodeNative(t, source, session, prior, nil)
	assertOpenCodeExistingIdentitiesStable(t, "append", first.Generation, appended.Generation)
	if len(appended.Generation.Main.Entries) <= len(first.Generation.Main.Entries) {
		t.Fatalf("append did not add a main entry: before %d, after %d", len(first.Generation.Main.Entries), len(appended.Generation.Main.Entries))
	}
}

// TestOpenCodeProvenanceFirstIncompleteInstallRetained proves the ratified
// first-discovery incomplete behavior over the real production candidate path:
// a first child whose fork boundary is unproven returns its validated
// incomplete_new generation with the readable child and uncertain evidence,
// while an incomplete candidate that would replace a complete generation is
// refused.
func TestOpenCodeProvenanceFirstIncompleteInstallRetained(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	seedOpenCodeNativeForkParent(t, source)
	fork := &ingest.OpenCodeForkProof{SourceSessionID: openCodeNativeForkSource}
	session := ingest.DiscoveredSession{
		SessionID:        ingest.SessionID(openCodeNativeChild),
		Harness:          ingest.HarnessOpenCode,
		TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
	}

	first := indexOpenCodeNative(t, source, session, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}, fork)
	if first.Generation.Completeness != indexformat.GenerationCompletenessIncompleteNew {
		t.Fatalf("first incomplete completeness = %q, want incomplete_new", first.Generation.Completeness)
	}
	if first.Generation.Metadata.Stats.InputSubmissionCount != nil {
		t.Fatalf("first incomplete inputSubmissionCount = %v, want absent", *first.Generation.Metadata.Stats.InputSubmissionCount)
	}
	if len(first.Generation.Main.Entries) == 0 {
		t.Fatal("first incomplete capture dropped the child's readable own work")
	}
	if len(first.Generation.Earlier) == 0 || len(first.Generation.Earlier[0].Content.Entries) == 0 {
		t.Fatal("first incomplete capture dropped the uncertain copied evidence")
	}

	completePrior := ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState(), HasCompleteGeneration: true}
	config := openCodeNativeProvenanceConfig(source, openCodeNativeChild, completePrior, fork)
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	_, err := indexer.IndexTranscriptResult(context.Background(), session)
	var incomplete *ingest.OpenCodeIncompleteProvenanceError
	if !errors.As(err, &incomplete) {
		t.Fatalf("incomplete replacement error = %v, want OpenCodeIncompleteProvenanceError", err)
	}
}

// TestOpenCodeProvenanceForkReadBoundedToCutoff proves the fork acquisition
// reads and decodes only the checked prefix: a malformed parent row far past
// the cutoff cannot fail a bounded child capture, an appended suffix cannot
// change the child proof, and an unproven boundary still reads every candidate.
func TestOpenCodeProvenanceForkReadBoundedToCutoff(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	applyOpenCodeNativeSetup(t, source, `
INSERT INTO session (id, parent_id, time_created, time_updated) VALUES ('`+openCodeNativeParent+`', '', 500, 2000);
UPDATE session SET parent_id = '`+openCodeNativeParent+`' WHERE id = '`+openCodeNativeChild+`';
INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES
  ('msg_prefix_valid', '`+openCodeNativeParent+`', 'user', 500, 500, '{"id":"msg_prefix_valid","type":"user","text":"captured prefix before the cutoff","time":{"created":500}}', 0),
  ('msg_suffix_valid', '`+openCodeNativeParent+`', 'user', 700, 700, '{"id":"msg_suffix_valid","type":"user","text":"suffix after the cutoff","time":{"created":700}}', 50),
  ('msg_suffix_malformed', '`+openCodeNativeParent+`', 'user', 800, 800, '{not json', 100);
`)
	before := testfixture.SnapshotSource(t, source)

	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	bounded := &ingest.OpenCodeForkProof{SourceSessionID: openCodeNativeForkSource, BeforeSeq: int64Ptr(1)}
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{Fork: bounded})
	if err != nil {
		t.Fatalf("bounded fork snapshot: %v", err)
	}
	if len(snapshot.Copied) != 1 {
		t.Fatalf("bounded captured copies = %d, want 1", len(snapshot.Copied))
	}
	digest := snapshot.SourceEvidenceDigest
	testfixture.AssertUnchanged(t, source, before)

	// Appending another suffix row must not change the bounded child proof.
	applyOpenCodeNativeSetup(t, source, `INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES
  ('msg_suffix_later', '`+openCodeNativeParent+`', 'user', 900, 900, '{"id":"msg_suffix_later","type":"user","text":"later suffix","time":{"created":900}}', 150);
`)
	again, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{Fork: bounded})
	if err != nil {
		t.Fatalf("bounded fork snapshot after append: %v", err)
	}
	if again.SourceEvidenceDigest != digest {
		t.Fatal("appending a parent suffix changed the bounded child proof")
	}

	// Without a proven boundary there is no cutoff: the same malformed row is
	// reached and the capture fails closed.
	unproven := &ingest.OpenCodeForkProof{SourceSessionID: openCodeNativeForkSource}
	if _, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{Fork: unproven}); err == nil {
		t.Fatal("unproven fork snapshot succeeded despite a malformed suffix row")
	}
}

// TestOpenCodeProvenanceAdmissionThroughNativeSQL binds the admitted,
// agent-delivery, and unproved-child corpus to real native SQL acquisition: the
// session row proves parentage, the session_message rows carry the payloads,
// and the classified projection keeps the exact fixture assertions.
func TestOpenCodeProvenanceAdmissionThroughNativeSQL(t *testing.T) {
	for _, row := range loadOpenCodeProvenanceCases(t) {
		t.Run(row.Name, func(t *testing.T) {
			source := testfixture.MaterializeByName(t, "native-current-rows")
			seedOpenCodeProvenanceCase(t, source, row)
			options := ingest.OpenCodeSnapshotOptions{}
			if row.AgentDeliver {
				delivered := make(map[string]bool, len(row.Rows))
				for _, native := range row.Rows {
					delivered[native.ID] = true
				}
				options.AgentDeliveredIDs = delivered
			}
			adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
			snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, row.Scope.SessionID, options)
			if err != nil {
				t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
			}
			if snapshot.ParentNullProven != row.Scope.ParentNullProven || snapshot.HasParent != row.Scope.HasParent {
				t.Fatalf("snapshot parentage = nullProven %t hasParent %t, want %t/%t", snapshot.ParentNullProven, snapshot.HasParent, row.Scope.ParentNullProven, row.Scope.HasParent)
			}
			skipped := 0
			for _, msg := range snapshot.Messages {
				if !msg.Settled {
					skipped++
				}
			}
			if skipped != row.Want.SkippedRows {
				t.Fatalf("skipped unfinished rows = %d, want %d", skipped, row.Want.SkippedRows)
			}
			capture, err := ingest.BuildOpenCodeProvenanceCapture(snapshot, "gen-"+row.Name, schema.UnifiedMetadata{
				SchemaVersion: 1,
				SessionID:     schema.SessionID(row.Scope.SessionID),
				ModelHarness:  schema.HarnessOpenCode,
			}, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()})
			if err != nil {
				t.Fatalf("BuildOpenCodeProvenanceCapture: %v", err)
			}
			allocator := &queueAllocator{entry: append([]string(nil), row.Allocator.Entries...), submission: append([]string(nil), row.Allocator.Submissions...)}
			built, err := ingest.BuildV2(capture, allocator)
			if err != nil {
				t.Fatalf("BuildV2: %v", err)
			}
			assertOpenCodeCounts(t, built.Generation, row.Want)
			assertOpenCodeEntries(t, built.Generation.Main.Entries, row.Want.Main)
		})
	}
}

// TestOpenCodeSnapshotDigestBindsCapturedContent proves the source-evidence
// digest binds the captured payload bytes and the delivery correlation: a real
// SQLite payload edit and a changed per-input delivery correlation each change
// the proof, so a same-identity row can never keep a stale proof.
func TestOpenCodeSnapshotDigestBindsCapturedContent(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})

	baseline, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
	}
	if baseline.SourceEvidenceDigest == "" {
		t.Fatal("snapshot carries no source evidence digest")
	}

	delivered, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{
		AgentDeliveredIDs: map[string]bool{"msg_native_user": true},
	})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance with delivery correlation: %v", err)
	}
	if delivered.SourceEvidenceDigest == baseline.SourceEvidenceDigest {
		t.Fatal("changing the delivery correlation did not change the source evidence digest")
	}

	applyOpenCodeNativeSetup(t, source, `UPDATE session_message SET data = '{"text":"mutated native question","time":{"created":1000}}' WHERE id = 'msg_native_user';`)
	mutated, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance after payload edit: %v", err)
	}
	if mutated.SourceEvidenceDigest == baseline.SourceEvidenceDigest {
		t.Fatal("editing the captured payload did not change the source evidence digest")
	}
}

// TestOpenCodeProvenanceDecodeErrorsDoNotLeakNativeValues proves the row decoder
// never echoes an arbitrary native payload identity or type value in a failure.
func TestOpenCodeProvenanceDecodeErrorsDoNotLeakNativeValues(t *testing.T) {
	const sentinel = "PRIVATE_NATIVE_SENTINEL"
	scope := ingest.OpenCodeProvenanceScope{SessionID: "ses_synthetic_scope", Shape: ingest.OpenCodeProvenanceCurrent, ParentNullProven: true}

	_, _, err := ingest.DecodeOpenCodeProvenanceRow(ingest.OpenCodeProvenanceRow{
		ID:        "msg_row_identity",
		SessionID: "ses_synthetic_scope",
		Type:      "user",
		Data:      `{"id":"` + sentinel + `","type":"user","text":"synthetic","time":{"created":1}}`,
	}, scope)
	if err == nil {
		t.Fatal("identity-conflicting payload decoded without an error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("decode error leaked the native payload identity: %v", err)
	}

	_, _, err = ingest.DecodeOpenCodeProvenanceRow(ingest.OpenCodeProvenanceRow{
		ID:        "msg_row_type",
		SessionID: "ses_synthetic_scope",
		Type:      "user",
		Data:      `{"id":"msg_row_type","type":"` + sentinel + `","text":"synthetic","time":{"created":1}}`,
	}, scope)
	if err == nil {
		t.Fatal("type-conflicting payload decoded without an error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("decode error leaked the native payload type: %v", err)
	}
}

func indexOpenCodeNative(t *testing.T, source testfixture.MaterializedSource, session ingest.DiscoveredSession, prior ingest.OpenCodeProvenancePrior, fork *ingest.OpenCodeForkProof) indexformat.V2 {
	t.Helper()
	config := openCodeNativeProvenanceConfig(source, session.SessionID.String(), prior, fork)
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	result, err := indexer.IndexTranscriptResult(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscriptResult: %v", err)
	}
	built, ok := result.(indexformat.V2)
	if !ok {
		t.Fatalf("IndexTranscriptResult returned %T, want indexformat.V2", result)
	}
	return built
}

func openCodeNativeProvenanceConfig(source testfixture.MaterializedSource, sessionID string, prior ingest.OpenCodeProvenancePrior, fork *ingest.OpenCodeForkProof) ingest.OpenCodeProvenanceIndexerConfig {
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	return ingest.OpenCodeProvenanceIndexerConfig{
		Enabled: true,
		Snapshot: func(ctx context.Context, _ ingest.DiscoveredSession) (ingest.OpenCodeHistorySnapshot, error) {
			return adapter.SnapshotOpenCodeProvenance(ctx, source.Path, sessionID, ingest.OpenCodeSnapshotOptions{Fork: fork})
		},
		Metadata: func(_ ingest.DiscoveredSession) (schema.UnifiedMetadata, error) {
			return schema.UnifiedMetadata{SchemaVersion: 1, SessionID: schema.SessionID(sessionID), ModelHarness: schema.HarnessOpenCode}, nil
		},
		GenerationID: func(_ ingest.DiscoveredSession) string { return "gen-native-" + sessionID },
		Prior: func(context.Context, ingest.DiscoveredSession) (ingest.OpenCodeProvenancePrior, error) {
			return prior, nil
		},
	}
}

func seedOpenCodeNativeForkParent(t *testing.T, source testfixture.MaterializedSource) {
	t.Helper()
	applyOpenCodeNativeSetup(t, source, `
INSERT INTO session (id, parent_id, time_created, time_updated) VALUES ('`+openCodeNativeParent+`', '', 500, 2000);
UPDATE session SET parent_id = '`+openCodeNativeParent+`' WHERE id = '`+openCodeNativeChild+`';
INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES
  ('msg_parent_user', '`+openCodeNativeParent+`', 'user', 500, 500, '{"id":"msg_parent_user","type":"user","text":"captured parent question","time":{"created":500}}', 0),
  ('msg_parent_assistant', '`+openCodeNativeParent+`', 'assistant', 500, 600, '{"id":"msg_parent_assistant","type":"assistant","agent":"build","model":{"id":"synthetic-model","providerID":"synthetic-provider"},"content":[{"type":"text","id":"prt_parent_text","text":"captured parent answer"}],"time":{"created":500,"completed":600}}', 1);
`)
}

func seedOpenCodeProvenanceCase(t *testing.T, source testfixture.MaterializedSource, row ocProvCase) {
	t.Helper()
	_ = testfixture.SnapshotSource(t, source)
	connection, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	parentID := ""
	if row.Scope.HasParent {
		parentID = "ses_opencodeFixtureParent"
	}
	if err := sqlitex.Execute(connection, "INSERT OR REPLACE INTO session (id, parent_id, time_created, time_updated) VALUES (?1, ?2, 1000, 1000)", &sqlitex.ExecOptions{Args: []any{row.Scope.SessionID, parentID}}); err != nil {
		t.Fatalf("seed session row for %q: %v", row.Scope.SessionID, err)
	}
	if err := sqlitex.Execute(connection, "DELETE FROM session_message WHERE session_id = ?1", &sqlitex.ExecOptions{Args: []any{row.Scope.SessionID}}); err != nil {
		t.Fatalf("clear session_message rows for %q: %v", row.Scope.SessionID, err)
	}
	for _, native := range row.Rows {
		if err := sqlitex.Execute(connection, "INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", &sqlitex.ExecOptions{Args: []any{native.ID, row.Scope.SessionID, native.Type, native.TimeCreated, native.TimeUpdated, native.Data, native.Seq}}); err != nil {
			t.Fatalf("seed message row %q: %v", native.ID, err)
		}
	}
}

func openCodeContentByRef(generation indexformat.Generation) map[schema.SourceEntryRef]indexformat.ContentRecord {
	out := make(map[schema.SourceEntryRef]indexformat.ContentRecord, len(generation.Content))
	for _, record := range generation.Content {
		out[record.Ref] = record
	}
	return out
}

func equalSourceRefSlices(left, right []schema.SourceEntryRef) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// assertOpenCodeExistingIdentitiesStable requires every alias and title ref the
// earlier generation proved to keep its exact identity in the later one.
func assertOpenCodeExistingIdentitiesStable(t *testing.T, label string, before, after indexformat.Generation) {
	t.Helper()
	afterAliases := make(map[string]schema.SourceEntryRef, len(after.Aliases))
	for _, alias := range after.Aliases {
		afterAliases[alias.NativeKey] = alias.Ref
	}
	for _, alias := range before.Aliases {
		got, ok := afterAliases[alias.NativeKey]
		if !ok || got != alias.Ref {
			t.Fatalf("%s alias %q = %q (present %t), want %q", label, alias.NativeKey, got, ok, alias.Ref)
		}
	}
	for _, ref := range before.TitleRefs {
		found := false
		for _, candidate := range after.TitleRefs {
			if candidate == ref {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s dropped title ref %q", label, ref)
		}
	}
}

func buildOpenCodeNativeGeneration(t *testing.T, snapshot ingest.OpenCodeHistorySnapshot) indexformat.V2 {
	t.Helper()
	return buildOpenCodeNativeGenerationPrior(t, snapshot, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()})
}

func buildOpenCodeNativeGenerationPrior(t *testing.T, snapshot ingest.OpenCodeHistorySnapshot, prior ingest.OpenCodeProvenancePrior) indexformat.V2 {
	t.Helper()
	capture, err := ingest.BuildOpenCodeProvenanceCapture(snapshot, "gen-native", schema.UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     schema.SessionID(openCodeNativeChild),
		ModelHarness:  schema.HarnessOpenCode,
	}, prior)
	if err != nil {
		t.Fatalf("BuildOpenCodeProvenanceCapture: %v", err)
	}
	built, err := ingest.BuildV2(capture, ingest.RandomRefAllocator{})
	if err != nil {
		t.Fatalf("BuildV2: %v", err)
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
