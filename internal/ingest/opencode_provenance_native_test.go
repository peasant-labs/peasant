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

const openCodeNativeChild = "ses_3cd91f52effeXd3QAJ54jOyzn1"

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
