package store

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// managedRefreshTestSession is the session identity every synthetic refresh
// fixture uses. It is a valid, stable UUID and names no real database.
const managedRefreshTestSession = schema.SessionID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")

// managedRefreshTestParent is the durable started_by target the seeded
// generation carries. It does not have to be a stored session: the parent cache
// is nil when the target is absent, while the generation evidence keeps it.
const managedRefreshTestParent = schema.SessionID("11111111-1111-4111-8111-111111111111")

// managedRefreshTestGenerationID is the source generation installed by the
// seeding helper.
const managedRefreshTestGenerationID = "gen-refresh-source"

// managedRefreshTestBlobRef is a captured context ref no entry emits, so the
// generation owns a session_content blob and a session_generation_content
// descriptor for it.
const managedRefreshTestBlobRef = schema.SourceEntryRef("e_ctx_0000")

// managedRefreshTestCaptureRevision is the publication capture revision the
// seeding session records, so the refresh can bind the index to it.
const managedRefreshTestCaptureRevision = int64(7)

// managedRefreshTestCandidate builds one valid complete generation with every
// generation-scoped collection represented: main and earlier entries, native
// metadata (seeded separately), a non-emitted content blob reached through a
// captured context segment, an alias, commits, associations, subagents,
// diagnostics, a relationship, and title refs.
func managedRefreshTestCandidate(sid schema.SessionID, generationID string) (indexformat.V2, map[schema.SourceEntryRef][]byte, schema.UnifiedMetadata) {
	inputCount := int64(9)
	mainRef := func(i int) schema.SourceEntryRef { return schema.SourceEntryRef(fmt.Sprintf("e_%06d", i)) }
	preview := func(text string) *string { return &text }
	toolInput := `{"command":"ls"}`
	toolOutput := "file-a\nfile-b"
	main := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: preview("run the refresh benchmark"), SourceEntryRef: mainRef(0)},
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: preview("listing the directory"), SourceEntryRef: mainRef(1)},
		{SessionID: sid, EntryIndex: 2, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeToolUse, Role: schema.RoleAssistant, ToolInput: &toolInput, SourceEntryRef: mainRef(2), Depth: 0},
		{SessionID: sid, EntryIndex: 3, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeToolResult, Role: schema.RoleTool, ToolOutput: &toolOutput, SourceEntryRef: mainRef(3), Depth: 1, ParentIndex: refreshIntPtr(2)},
		{SessionID: sid, EntryIndex: 4, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: preview("the refresh reuses the stored parse"), SourceEntryRef: mainRef(4)},
	}
	earlier := []indexformat.EarlierPartition{{
		State: schema.EarlierHistoryUncertainMigrated,
		Content: indexformat.Partition{Entries: []schema.SessionEntry{
			{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: preview("inherited earlier context"), SourceEntryRef: mainRef(10)},
		}},
	}}
	blob := []byte(`{"captured":"context"}`)
	blobSum := sha256.Sum256(blob)
	blobDigest := hex.EncodeToString(blobSum[:])
	metadata := schema.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion,
		SessionID:     sid,
		ModelHarness:  defaults.HarnessClaudeCode,
		Model:         "claude-sonnet-4-5",
		Version:       "2.0.0",
		Timestamp:     schema.TimestampInfo{Start: 1700000000000, End: 1700000009000},
		Source:        schema.SourceInfo{FilePath: "/synthetic/session.jsonl", Format: schema.SourceFormatJSONL},
		Git: schema.GitContext{
			Branch:   refreshStringPtr("main"),
			Remote:   refreshStringPtr("github.com/synthetic/refresh.git"),
			Worktree: refreshStringPtr("/synthetic/worktree"),
			Tracking: refreshStringPtr("origin/main"),
			Commits: []schema.CommitInfo{{
				Hash: "abc123", Message: "seed", AuthorName: "Synthetic", AuthorEmail: "synthetic@example.test",
				CommitTime: 1700000001000, AuthorTime: 1700000001000,
			}},
			Associations: []schema.PublishedAssociation{{ID: "assoc-seed", ObservedCommitHash: "abc123"}},
		},
		Project:  schema.ProjectContext{Hash: "proj-gen", FilePath: "/synthetic", Name: "synthetic"},
		HostSlug: "host-gen",
		Stats: schema.SessionStats{
			TurnCount: 3, ConversationTurnCount: refreshInt64Ptr(2), InputSubmissionCount: &inputCount,
			ToolCallCount: 1, SubagentCount: 1, DurationMs: 9000, TokensIn: 12, TokensOut: 34,
		},
		Subagents: []schema.SubagentRef{{SessionID: managedRefreshTestParent, ParentUUID: sid}},
		Purpose:   schema.SessionPurposeInteraction,
		Relationships: []schema.SessionRelationship{{
			Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetKnown,
			TargetLocalID: refreshSessionIDPtr(managedRefreshTestParent), Evidence: schema.EvidenceNativeTyped,
		}},
		CWD:         "/synthetic",
		Diagnostics: schema.DiagnosticsInfo{Partial: refreshBoolPtr(false), Warnings: []schema.DiagnosticEntry{{ErrorType: "seed_warning", Location: "seed", Message: "seed warning", Remediation: "none"}}},
		ContentHash: strings.Repeat("a", 64),
		Redaction: schema.RedactionInfo{
			Applied: true, Level: "standard", RuleSetVersion: "1.0.0",
			RedactedAtMs: refreshInt64Ptr(1700000002000), ContentHashAtRedact: strings.Repeat("a", 64),
		},
		AdapterVersion: refreshIntPtr(3),
	}
	metadata.MetadataHash = schema.ComputeMetadataHash(&metadata)
	generation := indexformat.Generation{
		ID:                   generationID,
		Completeness:         indexformat.GenerationCompletenessComplete,
		Metadata:             metadata,
		Main:                 indexformat.Partition{Entries: main},
		Earlier:              earlier,
		Segments:             []indexformat.ContextSegment{{Ordinal: 0, PhysicalSourceID: "synthetic-source", Coordinates: indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly}, Inclusion: indexformat.SegmentInclusionInherited, CapturedRefs: []schema.SourceEntryRef{managedRefreshTestBlobRef}}},
		Content:              []indexformat.ContentRecord{{Ref: managedRefreshTestBlobRef, RelativeBlob: "c_" + blobDigest + ".blob", ByteLength: int64(len(blob)), Digest: blobDigest}},
		Aliases:              []indexformat.NativeAlias{{NativeKey: "native-message-0", Ref: mainRef(0)}},
		SourceEvidenceDigest: strings.Repeat("e", 64),
		TitleRefs:            []schema.SourceEntryRef{mainRef(0), mainRef(1)},
	}
	return indexformat.V2{Generation: generation}, map[schema.SourceEntryRef][]byte{managedRefreshTestBlobRef: blob}, metadata
}

// seedManagedRefreshSource installs the synthetic source generation and the
// extra stored rows the clone plan must copy (native metadata and the
// file-backed projection leftovers a managed session should not normally
// carry). It returns the source metadata, the generation, and the expected
// index proof read after all seeds.
func seedManagedRefreshSource(t *testing.T, s *Store) (schema.UnifiedMetadata, indexformat.V2, *ingest.SessionIndexState) {
	t.Helper()
	ctx := t.Context()
	sid := managedRefreshTestSession
	seedBenchGenerationSession(t, s, sid)
	candidate, blobs, metadata := managedRefreshTestCandidate(sid, managedRefreshTestGenerationID)
	state, err := s.ReadIndexState(ctx, ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("read the seeded index state: %v", err)
	}
	inputHash := strings.Repeat("b", 64)
	artifact := strings.Repeat("c", 64)
	outcome, err := s.ActivateGeneration(ctx, GenerationActivation{
		Generation: candidate, Blobs: blobs,
		IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
		IndexedAtMs:    1700000005000, ExpectedState: state, IndexedInputHash: &inputHash,
		ArtifactIdentity: &artifact, CaptureRevision: managedRefreshTestCaptureRevision,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull,
			CapturedAtMs: 1700000005000,
		},
	})
	if err != nil {
		t.Fatalf("activate the synthetic source generation: %v", err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("source activation disposition = %v, want committed now", outcome.Disposition)
	}
	runRefreshSQL(t, s, `INSERT INTO session_section_native_metadata
(session_id, generation_id, partition_id, ordinal, native_id, kind, source_entry_ref, source_type, source_message_role, attachment_turn_index, attachment_tool_call_id, custom_type, data)
VALUES (?, ?, 0, 0, 'native-0', 'pi.custom.data', 'e_000000', 'pi.custom', NULL, NULL, NULL, NULL, '{"seed":true}')`,
		string(sid), managedRefreshTestGenerationID)
	runRefreshSQL(t, s, `INSERT INTO session_section_native_metadata
(session_id, generation_id, partition_id, ordinal, native_id, kind, source_entry_ref, source_type, source_message_role, attachment_turn_index, attachment_tool_call_id, custom_type, data)
VALUES (?, ?, 1, 0, 'native-1', 'pi.compaction.details', 'e_000010', 'pi.compaction', NULL, NULL, NULL, NULL, '{"seed":true}')`,
		string(sid), managedRefreshTestGenerationID)
	runRefreshSQL(t, s, `INSERT INTO session_projection_entries
(session_id, generation_id, partition_id, entry_index, source_entry_ref, entry_json)
VALUES (?, ?, 0, 0, 'e_000000', '{"seed":"file-backed"}')`,
		string(sid), managedRefreshTestGenerationID)
	runRefreshSQL(t, s, `INSERT INTO session_projection_content
(session_id, generation_id, source_entry_ref, relative_blob, byte_length, integrity_digest)
VALUES (?, ?, 'e_000000', 'seed.blob', 4, 'seed')`,
		string(sid), managedRefreshTestGenerationID)
	// Record the publication capture the refreshed metadata pair would have
	// written, and leave the index unbound from it: the state a metadata-only
	// install leaves before the index is re-bound.
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("encode the seeded publication capture: %v", err)
	}
	runRefreshSQL(t, s, `INSERT INTO session_publication_metadata
(session_id, capture_revision, schema_version, metadata_json, metadata_hash, content_hash, captured_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(sid), managedRefreshTestCaptureRevision, ingest.CurrentSchemaVersion,
		string(encoded), schema.ComputeMetadataHash(&metadata), metadata.ContentHash, 1700000005000)
	runRefreshSQL(t, s, `UPDATE sessions SET publication_capture_revision = ?, indexed_publication_capture_revision = 0, cwd_provenance_kind = 'source_exact' WHERE session_id = ?`,
		managedRefreshTestCaptureRevision, string(sid))
	// A completed harvest has already run its reclaim/sweep pass. Run the
	// production pass so the seeded baseline is settled (no pending sweep) and a
	// refused refresh can assert that nothing moved.
	if _, err := s.ReclaimSupersededGenerations(ctx, 0); err != nil {
		t.Fatalf("settle the seeded source with the reclaim pass: %v", err)
	}
	expected, err := s.ReadIndexState(ctx, ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("read the seeded index proof: %v", err)
	}
	if expected.PublicationCaptureRevision != managedRefreshTestCaptureRevision {
		t.Fatalf("seeded publication capture revision = %d, want %d", expected.PublicationCaptureRevision, managedRefreshTestCaptureRevision)
	}
	if expected.PublicationBound {
		t.Fatal("seeded index is already bound to the publication capture; the refresh binding would not be exercised")
	}
	return metadata, candidate, expected
}

// refreshedManagedMetadata returns the source metadata with the fields a
// metadata-only refresh is allowed to move: project, git, cwd, model, version,
// redaction, adapter revision, measured stats, derived stamp, and the
// metadata-derived children. Content identity, harness, source format, and
// relationships stay untouched.
func refreshedManagedMetadata(source schema.UnifiedMetadata) schema.UnifiedMetadata {
	refreshed := source
	inputCount := int64(6)
	refreshed.SchemaVersion = ingest.CurrentSchemaVersion
	refreshed.Model = "claude-sonnet-4-6"
	refreshed.Version = "2.1.0"
	refreshed.Timestamp = schema.TimestampInfo{Start: 1700000000000, End: 1700000011000}
	refreshed.Git.Branch = refreshStringPtr("feature/refresh")
	refreshed.Git.Remote = refreshStringPtr("github.com/synthetic/refresh.git")
	refreshed.Git.Worktree = refreshStringPtr("/synthetic/refresh-worktree")
	refreshed.Git.Tracking = refreshStringPtr("origin/feature/refresh")
	refreshed.Git.Commits = append(append([]schema.CommitInfo(nil), source.Git.Commits...), schema.CommitInfo{
		Hash: "def456", Message: "refresh", AuthorName: "Synthetic", AuthorEmail: "synthetic@example.test",
		CommitTime: 1700000008000, AuthorTime: 1700000008000,
	})
	refreshed.Git.Associations = []schema.PublishedAssociation{{ID: "assoc-refresh", ObservedCommitHash: "def456"}}
	refreshed.Project = schema.ProjectContext{Hash: source.Project.Hash, FilePath: "/synthetic/refreshed", Name: "synthetic-refreshed"}
	refreshed.Stats = schema.SessionStats{
		TurnCount: 4, ConversationTurnCount: refreshInt64Ptr(3), InputSubmissionCount: &inputCount,
		ToolCallCount: 2, SubagentCount: 2, DurationMs: 11000, TokensIn: 21, TokensOut: 43,
	}
	refreshed.Subagents = append(append([]schema.SubagentRef(nil), source.Subagents...), schema.SubagentRef{
		SessionID: schema.SessionID("22222222-2222-4222-8222-222222222222"), ParentUUID: managedRefreshTestSession,
	})
	refreshed.CWD = "/synthetic/refreshed"
	refreshed.DerivedAt = nil
	refreshed.Diagnostics = schema.DiagnosticsInfo{Partial: refreshBoolPtr(true), Warnings: append(append([]schema.DiagnosticEntry(nil), source.Diagnostics.Warnings...), schema.DiagnosticEntry{
		ErrorType: "refresh_warning", Location: "refresh", Message: "refresh warning", Remediation: "none",
	})}
	refreshed.Redaction = schema.RedactionInfo{
		Applied: true, Level: "standard", RuleSetVersion: "1.1.0",
		RedactedAtMs: refreshInt64Ptr(1700000020000), ContentHashAtRedact: source.ContentHash,
	}
	refreshed.AdapterVersion = refreshIntPtr(4)
	refreshed.MetadataHash = schema.ComputeMetadataHash(&refreshed)
	return refreshed
}

// runRefreshSQL executes one statement on the store's pool.
func runRefreshSQL(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("execute refresh SQL %q: %v", query, err)
	}
}

// refreshQueryString reads one text column.
func refreshQueryString(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.pool.Put(conn)
	value := ""
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		value = stmt.ColumnText(0)
		return nil
	}}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// refreshQueryInt reads one integer column.
func refreshQueryInt(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.pool.Put(conn)
	value := int64(0)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		value = stmt.ColumnInt64(0)
		return nil
	}}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// generationTableSnapshot is one generation-scoped table's exact row multiset:
// the row count, and a digest over every column value except the generation id.
// Rows are sorted before hashing, so the digest compares the copied rows
// without depending on insertion order.
type generationTableSnapshot struct {
	Rows   int
	Digest string
}

// snapshotGenerationTable reads every row of one generation-scoped table for a
// session and returns its count and a canonical digest. The generation id is
// excluded because it is the one column a refresh must change.
func snapshotGenerationTable(t *testing.T, s *Store, table string, sid schema.SessionID, generationID string) generationTableSnapshot {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.pool.Put(conn)
	columns := generationTableColumns(t, conn, table)
	var rows []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT * FROM `+table+` WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			parts := make([]string, 0, len(columns))
			for i, column := range columns {
				if column == "generation_id" {
					continue
				}
				value := "<null>"
				if stmt.ColumnType(i) != sqlite.TypeNull {
					value = stmt.ColumnText(i)
				}
				parts = append(parts, column+"="+value)
			}
			rows = append(rows, strings.Join(parts, "\x1f"))
			return nil
		},
	}); err != nil {
		t.Fatalf("snapshot %s: %v", table, err)
	}
	sort.Strings(rows)
	sum := sha256.Sum256([]byte(strings.Join(rows, "\x1e")))
	return generationTableSnapshot{Rows: len(rows), Digest: hex.EncodeToString(sum[:])}
}

// generationTableColumns reads a table's schema-order column names.
func generationTableColumns(t *testing.T, conn *sqlite.Conn, table string) []string {
	t.Helper()
	var columns []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, &sqlitex.ExecOptions{
		Args: []any{table},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			columns = append(columns, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("read columns of %s: %v", table, err)
	}
	if len(columns) == 0 {
		t.Fatalf("table %s has no columns", table)
	}
	return columns
}

// refreshSnapshotTables are the tables a metadata-only refresh must copy. The
// catalog row is inserted, and the metadata-derived children are regenerated
// and asserted separately.
var refreshSnapshotTables = []string{
	"session_context_segment_refs",
	"session_section_native_metadata",
	"session_generation_entries",
	"session_generation_content",
	"session_generation_title_refs",
	"session_projection_entries",
	"session_projection_content",
	"session_projection_aliases",
	"session_projection_sections",
	"session_context_segments",
	"session_relationship_evidence",
}

func refreshIntPtr(v int) *int       { return &v }
func refreshInt64Ptr(v int64) *int64 { return &v }
func refreshBoolPtr(v bool) *bool    { return &v }

func refreshStringPtr(v string) *string { return &v }

func refreshSessionIDPtr(v schema.SessionID) *schema.SessionID { return &v }

// readRefreshEntries loads the main and earlier entries through the production
// snapshot reader, so the assertion observes what a reopen serves.
func readRefreshEntries(t *testing.T, s *Store, sid schema.SessionID) ([]schema.SessionEntry, []indexformat.EarlierPartition) {
	t.Helper()
	var main []schema.SessionEntry
	var earlier []indexformat.EarlierPartition
	if err := s.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
		main = append([]schema.SessionEntry(nil), snapshot.Main.Entries...)
		earlier = append([]indexformat.EarlierPartition(nil), snapshot.Earlier...)
		return nil
	}); err != nil {
		t.Fatalf("read the session snapshot: %v", err)
	}
	return main, earlier
}

// searchRefreshHits runs the production merged search over one synthetic
// session.
func searchRefreshHits(t *testing.T, s *Store, sid schema.SessionID, match string) []UnifiedSearchHit {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection for search: %v", err)
	}
	defer s.pool.Put(conn)
	hits, err := SearchMergedOnConn(conn, match, 20, 0, SearchFilters{SessionIDs: []string{string(sid)}})
	if err != nil {
		t.Fatalf("search %q: %v", match, err)
	}
	return hits
}

func TestManagedGenerationMetadataRefreshInstallsNewAuthority(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer func() { _ = s.Close() }()
	ctx := t.Context()
	sid := managedRefreshTestSession
	sourceMetadata, sourceGeneration, expected := seedManagedRefreshSource(t, s)
	refreshed := refreshedManagedMetadata(sourceMetadata)

	beforeTables := map[string]generationTableSnapshot{}
	for _, table := range refreshSnapshotTables {
		beforeTables[table] = snapshotGenerationTable(t, s, table, sid, managedRefreshTestGenerationID)
	}
	bodyRowsBefore := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, string(sid))
	contentRowsBefore := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_content WHERE session_id = ?`, string(sid))
	chunkRowsBefore := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_content_chunks WHERE session_id = ?`, string(sid))
	mainBefore, earlierBefore := readRefreshEntries(t, s, sid)
	hitsBefore := searchRefreshHits(t, s, sid, "directory")
	titleBefore := refreshQueryString(t, s, `SELECT COALESCE(title, '') FROM session_metrics WHERE session_id = ?`, string(sid))

	result, err := s.RefreshManagedGeneration(ctx, ManagedGenerationRefresh{
		SessionID: sid, SourceGenerationID: managedRefreshTestGenerationID,
		Metadata: refreshed, Expected: expected,
	})
	if err != nil {
		t.Fatalf("refresh the managed generation: %v", err)
	}
	if result.Disposition != ManagedGenerationRefreshInstalled {
		t.Fatalf("refresh disposition = %q, want %q", result.Disposition, ManagedGenerationRefreshInstalled)
	}
	if result.GenerationID == "" || result.GenerationID == managedRefreshTestGenerationID {
		t.Fatalf("refresh generation id = %q, want a new identity", result.GenerationID)
	}
	if active := refreshQueryString(t, s, `SELECT COALESCE(active_generation_id, '') FROM sessions WHERE session_id = ?`, string(sid)); active != result.GenerationID {
		t.Fatalf("active generation = %q, want the refreshed generation %q", active, result.GenerationID)
	}
	if pending := refreshQueryInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, string(sid)); pending != 1 {
		t.Fatalf("content_sweep_pending = %d, want 1 so the superseded generation is reclaimed", pending)
	}
	if sourceRows := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), managedRefreshTestGenerationID); sourceRows != 1 {
		t.Fatalf("source generation row count = %d, want 1 (immutable history keeps it until reclaim)", sourceRows)
	}
	if sourceActivated := refreshQueryInt(t, s, `SELECT COALESCE(activated_at_ms, 0) FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), managedRefreshTestGenerationID); sourceActivated == 0 {
		t.Fatal("source generation lost its activation stamp")
	}
	if rows := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_projection_generations WHERE session_id = ?`, string(sid)); rows != 0 {
		t.Fatalf("session_projection_generations rows = %d, want 0: a managed generation has no file-backed catalog row", rows)
	}

	// The parse-owned rows are copied exactly: same count and same values for
	// every generation-scoped table the clone plan covers.
	for _, table := range refreshSnapshotTables {
		after := snapshotGenerationTable(t, s, table, sid, result.GenerationID)
		if after != beforeTables[table] {
			t.Errorf("%s after refresh = %+v, want the source rows %+v", table, after, beforeTables[table])
		}
	}
	// No body, content header, or chunk was rewritten or added.
	if got := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, string(sid)); got != bodyRowsBefore {
		t.Errorf("session_entry_bodies rows = %d, want unchanged %d", got, bodyRowsBefore)
	}
	if got := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_content WHERE session_id = ?`, string(sid)); got != contentRowsBefore {
		t.Errorf("session_content rows = %d, want unchanged %d", got, contentRowsBefore)
	}
	if got := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_content_chunks WHERE session_id = ?`, string(sid)); got != chunkRowsBefore {
		t.Errorf("session_content_chunks rows = %d, want unchanged %d", got, chunkRowsBefore)
	}

	// The refreshed metadata is the new generation's catalog row.
	if got := refreshQueryString(t, s, `SELECT project_name FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got != refreshed.Project.Name {
		t.Errorf("project_name = %q, want %q", got, refreshed.Project.Name)
	}
	if got := refreshQueryString(t, s, `SELECT COALESCE(git_branch, '') FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got != "feature/refresh" {
		t.Errorf("git_branch = %q, want the refreshed branch", got)
	}
	if got := refreshQueryString(t, s, `SELECT COALESCE(cwd, '') FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got != refreshed.CWD {
		t.Errorf("cwd = %q, want %q", got, refreshed.CWD)
	}
	if got := refreshQueryInt(t, s, `SELECT COALESCE(adapter_version, 0) FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got != 4 {
		t.Errorf("generation adapter_version = %d, want 4", got)
	}
	if got := refreshQueryString(t, s, `SELECT metadata_hash FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got != statsExcludedMetadataHash(refreshed) {
		t.Errorf("metadata_hash = %q, want the recomputed stats-excluded hash %q", got, statsExcludedMetadataHash(refreshed))
	}
	if got := refreshQueryString(t, s, `SELECT source_evidence_digest FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got != sourceGeneration.Generation.SourceEvidenceDigest {
		t.Errorf("source_evidence_digest = %q, want the preserved %q", got, sourceGeneration.Generation.SourceEvidenceDigest)
	}
	if got := refreshQueryString(t, s, `SELECT completeness FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got != string(indexformat.GenerationCompletenessComplete) {
		t.Errorf("completeness = %q, want complete", got)
	}
	if got := refreshQueryString(t, s, `SELECT candidate_digest FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got == "" {
		t.Error("candidate_digest is empty; an idempotent retry cannot bind")
	}

	// The metadata-derived children are the refreshed capture's, not the
	// source's.
	if got := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generation_commits WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got != int64(len(refreshed.Git.Commits)) {
		t.Errorf("new generation commits = %d, want %d", got, len(refreshed.Git.Commits))
	}
	if got := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generation_commits WHERE session_id = ? AND generation_id = ?`, string(sid), managedRefreshTestGenerationID); got != int64(len(sourceMetadata.Git.Commits)) {
		t.Errorf("source generation commits = %d, want the source's %d", got, len(sourceMetadata.Git.Commits))
	}
	if got := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generation_diagnostics WHERE session_id = ? AND generation_id = ?`, string(sid), result.GenerationID); got != int64(len(refreshed.Diagnostics.Warnings)) {
		t.Errorf("new generation diagnostics = %d, want %d", got, len(refreshed.Diagnostics.Warnings))
	}
	// The relationship rows are preserved, not replaced.
	if got := snapshotGenerationTable(t, s, "session_relationship_evidence", sid, result.GenerationID); got != beforeTables["session_relationship_evidence"] {
		t.Errorf("relationship evidence = %+v, want the preserved source rows %+v", got, beforeTables["session_relationship_evidence"])
	}

	// Mutable stats and the session mirrors carry the refresh.
	if got := refreshQueryInt(t, s, `SELECT turn_count FROM session_captured_stats WHERE session_id = ?`, string(sid)); got != int64(sourceMetadata.Stats.TurnCount) {
		t.Errorf("captured turn_count = %d, want %d", got, sourceMetadata.Stats.TurnCount)
	}
	if got := refreshQueryInt(t, s, `SELECT conversation_turn_count FROM session_captured_stats WHERE session_id = ?`, string(sid)); got != *sourceMetadata.Stats.ConversationTurnCount {
		t.Errorf("captured conversation_turn_count = %d, want %d", got, *sourceMetadata.Stats.ConversationTurnCount)
	}
	if got := refreshQueryInt(t, s, `SELECT input_submission_count FROM session_captured_stats WHERE session_id = ?`, string(sid)); got != *sourceMetadata.Stats.InputSubmissionCount {
		t.Errorf("captured input_submission_count = %d, want %d", got, *sourceMetadata.Stats.InputSubmissionCount)
	}
	if got := refreshQueryInt(t, s, `SELECT tool_call_count FROM session_captured_stats WHERE session_id = ?`, string(sid)); got != int64(refreshed.Stats.ToolCallCount) {
		t.Errorf("captured tool_call_count = %d, want %d", got, refreshed.Stats.ToolCallCount)
	}
	if got := refreshQueryInt(t, s, `SELECT turn_count FROM session_metrics WHERE session_id = ?`, string(sid)); got != int64(sourceMetadata.Stats.TurnCount) {
		t.Errorf("metric turn_count = %d, want %d", got, sourceMetadata.Stats.TurnCount)
	}
	if got := refreshQueryInt(t, s, `SELECT tool_calls FROM session_metrics WHERE session_id = ?`, string(sid)); got != int64(refreshed.Stats.ToolCallCount) {
		t.Errorf("metric tool_calls = %d, want %d", got, refreshed.Stats.ToolCallCount)
	}
	if title := refreshQueryString(t, s, `SELECT COALESCE(title, '') FROM session_metrics WHERE session_id = ?`, string(sid)); title != titleBefore {
		t.Errorf("title = %q, want the unchanged %q", title, titleBefore)
	}
	if got := refreshQueryInt(t, s, `SELECT adapter_version FROM sessions WHERE session_id = ?`, string(sid)); got != 4 {
		t.Errorf("session adapter_version = %d, want 4", got)
	}
	if got := refreshQueryInt(t, s, `SELECT input_submission_count FROM sessions WHERE session_id = ?`, string(sid)); got != *sourceMetadata.Stats.InputSubmissionCount {
		t.Errorf("session input_submission_count = %d, want %d", got, *sourceMetadata.Stats.InputSubmissionCount)
	}
	if got := refreshQueryInt(t, s, `SELECT COALESCE(indexed_publication_capture_revision, 0) FROM sessions WHERE session_id = ?`, string(sid)); got != managedRefreshTestCaptureRevision {
		t.Errorf("indexed publication revision = %d, want the bound %d", got, managedRefreshTestCaptureRevision)
	}
	if got := refreshQueryInt(t, s, `SELECT COALESCE(publication_capture_revision, 0) FROM session_content_captures WHERE session_id = ?`, string(sid)); got != managedRefreshTestCaptureRevision {
		t.Errorf("content capture publication revision = %d, want %d", got, managedRefreshTestCaptureRevision)
	}

	// A reopen serves the same entries and the refreshed metadata.
	mainAfter, earlierAfter := readRefreshEntries(t, s, sid)
	if !reflect.DeepEqual(mainAfter, mainBefore) {
		t.Errorf("main entries changed across the metadata refresh: %d before, %d after, equal=%t", len(mainBefore), len(mainAfter), reflect.DeepEqual(mainAfter, mainBefore))
	}
	if !reflect.DeepEqual(earlierAfter, earlierBefore) {
		t.Errorf("earlier partitions changed across the metadata refresh")
	}
	var snapshotMetadata schema.UnifiedMetadata
	if err := s.WithSessionSnapshot(ctx, sid, func(snapshot indexformat.ReadSnapshot) error {
		snapshotMetadata = snapshot.Metadata
		return nil
	}); err != nil {
		t.Fatalf("read the refreshed snapshot metadata: %v", err)
	}
	if snapshotMetadata.Project.Name != refreshed.Project.Name {
		t.Errorf("snapshot project name = %q, want %q", snapshotMetadata.Project.Name, refreshed.Project.Name)
	}
	if snapshotMetadata.Stats.TurnCount != sourceMetadata.Stats.TurnCount {
		t.Errorf("snapshot turn count = %d, want %d", snapshotMetadata.Stats.TurnCount, sourceMetadata.Stats.TurnCount)
	}
	if !reflect.DeepEqual(snapshotMetadata.Relationships, refreshed.Relationships) {
		t.Errorf("snapshot relationships = %+v, want the preserved %+v", snapshotMetadata.Relationships, refreshed.Relationships)
	}

	// Search returns the same text because the bodies did not move.
	hitsAfter := searchRefreshHits(t, s, sid, "directory")
	if !reflect.DeepEqual(hitsAfter, hitsBefore) {
		t.Errorf("search hits changed across the metadata refresh: before %+v, after %+v", hitsBefore, hitsAfter)
	}

	// The existing reclaim removes the superseded rows and keeps the shared
	// bodies the refreshed generation still references.
	reclaimed, err := s.ReclaimSupersededGenerations(ctx, 0)
	if err != nil {
		t.Fatalf("reclaim the superseded generation: %v", err)
	}
	if reclaimed.Generations != 1 {
		t.Fatalf("reclaimed generations = %d, want 1", reclaimed.Generations)
	}
	if rows := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), managedRefreshTestGenerationID); rows != 0 {
		t.Errorf("source generation rows after reclaim = %d, want 0", rows)
	}
	for _, table := range refreshSnapshotTables {
		after := snapshotGenerationTable(t, s, table, sid, result.GenerationID)
		if after != beforeTables[table] {
			t.Errorf("%s after reclaim = %+v, want the refreshed generation's rows %+v", table, after, beforeTables[table])
		}
	}
	if pending := refreshQueryInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, string(sid)); pending != 0 {
		t.Errorf("content_sweep_pending after reclaim = %d, want 0", pending)
	}
	if got := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, string(sid)); got != bodyRowsBefore {
		t.Errorf("session_entry_bodies rows after reclaim = %d, want the shared %d", got, bodyRowsBefore)
	}
	if got := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_content WHERE session_id = ?`, string(sid)); got != contentRowsBefore {
		t.Errorf("session_content rows after reclaim = %d, want the shared %d", got, contentRowsBefore)
	}
}

func TestManagedGenerationRefreshCommitFailureReachesIngest(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer func() { _ = s.Close() }()
	metadata, _, expected := seedManagedRefreshSource(t, s)
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`CREATE TABLE refresh_commit_parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE refresh_commit_child (id INTEGER REFERENCES refresh_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER refresh_commit_failure AFTER INSERT ON session_generations BEGIN INSERT INTO refresh_commit_child VALUES (123); END`,
	} {
		if err := sqlitex.Execute(conn, query, nil); err != nil {
			s.pool.Put(conn)
			t.Fatal(err)
		}
	}
	s.pool.Put(conn)
	outcome, err := s.RefreshManagedGenerationWithoutParse(t.Context(), ingest.ManagedGenerationRefreshRequest{
		SessionID: ingest.SessionID(managedRefreshTestSession),
		Metadata:  refreshedManagedMetadata(metadata), ExpectedState: expected,
	})
	if err == nil || outcome != (ingest.ManagedGenerationRefreshOutcome{}) {
		t.Fatalf("commit failure returned outcome %+v, error %v; want error and no successful disposition", outcome, err)
	}
	if active := refreshQueryString(t, s, `SELECT active_generation_id FROM sessions WHERE session_id = ?`, string(managedRefreshTestSession)); active != managedRefreshTestGenerationID {
		t.Fatalf("failed commit changed active generation to %q", active)
	}
	if got := refreshQueryInt(t, s, `SELECT count(*) FROM refresh_commit_child`); got != 0 {
		t.Fatalf("failed commit left %d child rows", got)
	}
}

func TestManagedGenerationRefreshMissingParseCountsRefuses(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer func() { _ = s.Close() }()
	metadata, _, expected := seedManagedRefreshSource(t, s)
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.Execute(conn, `UPDATE session_captured_stats SET input_submission_count = NULL WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(managedRefreshTestSession)}})
	s.pool.Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.RefreshManagedGenerationWithoutParse(t.Context(), ingest.ManagedGenerationRefreshRequest{
		SessionID: ingest.SessionID(managedRefreshTestSession),
		Metadata:  refreshedManagedMetadata(metadata), ExpectedState: expected,
	})
	if !errors.Is(err, ErrManagedGenerationRefreshRefused) || outcome != (ingest.ManagedGenerationRefreshOutcome{}) {
		t.Fatalf("missing counts returned outcome %+v, error %v; want refusal and no successful disposition", outcome, err)
	}
	if active := refreshQueryString(t, s, `SELECT active_generation_id FROM sessions WHERE session_id = ?`, string(managedRefreshTestSession)); active != managedRefreshTestGenerationID {
		t.Fatalf("refusal changed active generation to %q", active)
	}
}

//go:embed testdata/generation_refresh_identity.yaml
var managedRefreshIdentityYAML []byte

//go:embed testdata/generation_refresh_identity.manifest.yaml
var managedRefreshIdentityManifestYAML []byte

//go:embed testdata/generation_refresh_refusal.yaml
var managedRefreshRefusalYAML []byte

//go:embed testdata/generation_refresh_refusal.manifest.yaml
var managedRefreshRefusalManifestYAML []byte

// managedRefreshIdentityCase is one refusal fixture row: the refreshed capture
// field to mutate. The test owns the mutation value so the fixture states the
// scenario, not a duplicated document.
type managedRefreshIdentityCase struct {
	Name  string `yaml:"name"`
	Field string `yaml:"field"`
}

type managedRefreshIdentityFixture struct {
	Cases []managedRefreshIdentityCase `yaml:"cases"`
}

func loadManagedRefreshIdentityFixture(t *testing.T) managedRefreshIdentityFixture {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(string(managedRefreshIdentityYAML)))
	decoder.KnownFields(true)
	var fixture managedRefreshIdentityFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_refresh_identity.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("generation_refresh_identity.yaml must contain exactly one document: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(managedRefreshIdentityManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_refresh_identity manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("generation refresh identity case name is empty")
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "generation refresh identity"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// attemptManagedRefresh calls the production entry point with the seeded source.
func attemptManagedRefresh(t *testing.T, s *Store, expected *ingest.SessionIndexState, metadata schema.UnifiedMetadata) (ManagedGenerationRefreshResult, error) {
	t.Helper()
	return s.RefreshManagedGeneration(t.Context(), ManagedGenerationRefresh{
		SessionID: managedRefreshTestSession, SourceGenerationID: managedRefreshTestGenerationID,
		Metadata: metadata, Expected: expected,
	})
}

// assertRefreshSourceUnchanged proves no row moved: the source is still the
// active authority, no generation was added, and no sweep is pending.
func assertRefreshSourceUnchanged(t *testing.T, s *Store) {
	t.Helper()
	if active := refreshQueryString(t, s, `SELECT COALESCE(active_generation_id, '') FROM sessions WHERE session_id = ?`, string(managedRefreshTestSession)); active != managedRefreshTestGenerationID {
		t.Fatalf("active generation = %q, want the unchanged source %q", active, managedRefreshTestGenerationID)
	}
	if rows := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, string(managedRefreshTestSession)); rows != 1 {
		t.Fatalf("session_generations rows = %d, want the unchanged 1", rows)
	}
	if pending := refreshQueryInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, string(managedRefreshTestSession)); pending != 0 {
		t.Fatalf("content_sweep_pending = %d, want 0", pending)
	}
	if got := refreshQueryInt(t, s, `SELECT COALESCE(indexed_publication_capture_revision, 0) FROM sessions WHERE session_id = ?`, string(managedRefreshTestSession)); got != 0 {
		t.Fatalf("indexed publication revision = %d, want the unbound 0", got)
	}
}

func TestManagedGenerationMetadataRefreshRefusesIdentityChanges(t *testing.T) {
	const identityFieldHarness = "harness"
	fixture := loadManagedRefreshIdentityFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			s, _ := openGenerationStore(t)
			defer func() { _ = s.Close() }()
			source, _, expected := seedManagedRefreshSource(t, s)
			refreshed := refreshedManagedMetadata(source)
			switch testCase.Field {
			case "session_id":
				refreshed.SessionID = schema.SessionID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			case "content_hash":
				refreshed.ContentHash = strings.Repeat("9", 64)
			case identityFieldHarness:
				refreshed.ModelHarness = schema.HarnessCodex
			case "source_format":
				refreshed.Source.Format = schema.SourceFormatJSON
			case "relationships":
				refreshed.Relationships = []schema.SessionRelationship{{
					Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetExplicitNone,
					Evidence: schema.EvidenceNativeTyped,
				}}
			default:
				t.Fatalf("identity fixture names unknown field %q", testCase.Field)
			}
			_, err := attemptManagedRefresh(t, s, expected, refreshed)
			var refusal *ManagedGenerationRefreshError
			if !errors.As(err, &refusal) {
				t.Fatalf("refresh error = %v, want a managed-generation refusal", err)
			}
			if refusal.Reason != ManagedGenerationRefreshIdentityChanged {
				t.Fatalf("refusal reason = %q, want %q", refusal.Reason, ManagedGenerationRefreshIdentityChanged)
			}
			if !errors.Is(err, ErrManagedGenerationRefreshRefused) {
				t.Fatalf("refusal does not match the sentinel: %v", err)
			}
			if !strings.Contains(err.Error(), "nothing changed") && !strings.Contains(err.Error(), "impact:") {
				t.Fatalf("refusal does not state its impact: %v", err)
			}
			assertRefreshSourceUnchanged(t, s)
		})
	}
}

// managedRefreshRefusalCase is one stored-state refusal fixture row. state
// names the seed mutation; reason is the closed-set refusal the store must
// report.
type managedRefreshRefusalCase struct {
	Name   string `yaml:"name"`
	State  string `yaml:"state"`
	Reason string `yaml:"reason"`
}

type managedRefreshRefusalFixture struct {
	Cases []managedRefreshRefusalCase `yaml:"cases"`
}

func loadManagedRefreshRefusalFixture(t *testing.T) managedRefreshRefusalFixture {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(string(managedRefreshRefusalYAML)))
	decoder.KnownFields(true)
	var fixture managedRefreshRefusalFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_refresh_refusal.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("generation_refresh_refusal.yaml must contain exactly one document: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(managedRefreshRefusalManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_refresh_refusal manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("generation refresh refusal case name is empty")
		}
		switch c.State {
		case "non_active_source", "file_backed_row", "latest_attempt", "missing_catalog_row":
		default:
			t.Fatalf("case %q names unknown seed state %q", c.Name, c.State)
		}
		switch ManagedGenerationRefreshRefusalReason(c.Reason) {
		case ManagedGenerationRefreshSourceNotActive, ManagedGenerationRefreshSourceUnsupported,
			ManagedGenerationRefreshLatestAttemptHeld, ManagedGenerationRefreshIdentityChanged:
		default:
			t.Fatalf("case %q names unknown refusal reason %q", c.Name, c.Reason)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "generation refresh refusal"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestManagedGenerationMetadataRefreshRefusesStateThatCannotClone(t *testing.T) {
	fixture := loadManagedRefreshRefusalFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			s, _ := openGenerationStore(t)
			defer func() { _ = s.Close() }()
			source, _, expected := seedManagedRefreshSource(t, s)
			sourceID := managedRefreshTestGenerationID
			switch testCase.State {
			case "non_active_source":
				sourceID = "gen-not-active"
			case "file_backed_row":
				runRefreshSQL(t, s, `INSERT INTO session_projection_generations
(session_id, generation_id, metadata_json, title_refs_json, source_evidence_digest, completeness, index_format_version, installed_at_ms)
VALUES (?, ?, '{}', '[]', ?, 'complete', 2, 1)`,
					string(managedRefreshTestSession), managedRefreshTestGenerationID, strings.Repeat("f", 64))
			case "latest_attempt":
				runRefreshSQL(t, s, `UPDATE sessions SET latest_attempt_generation_id = 'gen-latest-attempt', latest_attempt_artifact_hash = ?, latest_attempt_indexer_version = 1, latest_attempt_captured_at_ms = 1 WHERE session_id = ?`,
					strings.Repeat("d", 64), string(managedRefreshTestSession))
			case "missing_catalog_row":
				runRefreshSQL(t, s, `DELETE FROM session_generations WHERE session_id = ? AND generation_id = ?`,
					string(managedRefreshTestSession), managedRefreshTestGenerationID)
			}
			generationsBefore := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, string(managedRefreshTestSession))
			_, err := s.RefreshManagedGeneration(t.Context(), ManagedGenerationRefresh{
				SessionID: managedRefreshTestSession, SourceGenerationID: sourceID,
				Metadata: refreshedManagedMetadata(source), Expected: expected,
			})
			var refusal *ManagedGenerationRefreshError
			if !errors.As(err, &refusal) {
				t.Fatalf("refresh error = %v, want a managed-generation refusal", err)
			}
			if want := ManagedGenerationRefreshRefusalReason(testCase.Reason); refusal.Reason != want {
				t.Fatalf("refusal reason = %q, want %q", refusal.Reason, want)
			}
			if active := refreshQueryString(t, s, `SELECT COALESCE(active_generation_id, '') FROM sessions WHERE session_id = ?`, string(managedRefreshTestSession)); active != managedRefreshTestGenerationID {
				t.Fatalf("active generation = %q, want the unchanged source", active)
			}
			if rows := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, string(managedRefreshTestSession)); rows != generationsBefore {
				t.Fatalf("session_generations rows = %d, want the unchanged %d", rows, generationsBefore)
			}
			if pending := refreshQueryInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, string(managedRefreshTestSession)); pending != 0 {
				t.Fatalf("content_sweep_pending = %d, want 0", pending)
			}
		})
	}
}

func TestManagedGenerationMetadataRefreshRefusesStaleProof(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer func() { _ = s.Close() }()
	source, _, expected := seedManagedRefreshSource(t, s)
	runRefreshSQL(t, s, `UPDATE sessions SET indexed_at = indexed_at + 1 WHERE session_id = ?`, string(managedRefreshTestSession))
	_, err := attemptManagedRefresh(t, s, expected, refreshedManagedMetadata(source))
	var stale *ingest.StaleIndexWorkError
	if !errors.As(err, &stale) {
		t.Fatalf("refresh error = %v, want a stale index work error", err)
	}
	assertRefreshSourceUnchanged(t, s)
}

func TestManagedGenerationMetadataRefreshRollsBackOnInjectedError(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer func() { _ = s.Close() }()
	source, _, expected := seedManagedRefreshSource(t, s)
	bodyRows := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, string(managedRefreshTestSession))
	managedGenerationRefreshSeam = func(stage string) error {
		if stage == managedGenerationRefreshSeamAfterInsert {
			return errors.New("injected refresh failure")
		}
		return nil
	}
	defer func() { managedGenerationRefreshSeam = nil }()
	_, err := attemptManagedRefresh(t, s, expected, refreshedManagedMetadata(source))
	if err == nil || !strings.Contains(err.Error(), "injected refresh failure") {
		t.Fatalf("refresh error = %v, want the injected failure", err)
	}
	assertRefreshSourceUnchanged(t, s)
	if got := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, string(managedRefreshTestSession)); got != bodyRows {
		t.Fatalf("session_entry_bodies rows after rollback = %d, want unchanged %d", got, bodyRows)
	}
}

func TestManagedGenerationMetadataRefreshSettlesUnchangedCaptureAndRefusesRepeat(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer func() { _ = s.Close() }()
	source, _, expected := seedManagedRefreshSource(t, s)

	// A capture identical to the active generation installs nothing.
	unchanged, err := attemptManagedRefresh(t, s, expected, source)
	if err != nil {
		t.Fatalf("unchanged refresh: %v", err)
	}
	if unchanged.Disposition != ManagedGenerationRefreshUnchanged || unchanged.GenerationID != managedRefreshTestGenerationID {
		t.Fatalf("unchanged result = %+v, want the source generation and the unchanged disposition", unchanged)
	}
	if rows := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, string(managedRefreshTestSession)); rows != 1 {
		t.Fatalf("session_generations rows after an unchanged refresh = %d, want 1", rows)
	}
	if bound := refreshQueryInt(t, s, `SELECT COALESCE(indexed_publication_capture_revision, 0) FROM sessions WHERE session_id = ?`, string(managedRefreshTestSession)); bound != managedRefreshTestCaptureRevision {
		t.Fatalf("indexed publication revision after unchanged refresh = %d, want %d", bound, managedRefreshTestCaptureRevision)
	}

	// A changed capture installs one new generation. The unchanged settle moved
	// the binding, so the caller re-reads its proof before the next attempt.
	installProof, err := s.ReadIndexState(t.Context(), ingest.SessionID(managedRefreshTestSession))
	if err != nil {
		t.Fatalf("read the proof after the unchanged settle: %v", err)
	}
	refreshed := refreshedManagedMetadata(source)
	installed, err := attemptManagedRefresh(t, s, installProof, refreshed)
	if err != nil {
		t.Fatalf("installing refresh: %v", err)
	}
	if installed.Disposition != ManagedGenerationRefreshInstalled {
		t.Fatalf("installed disposition = %q, want installed", installed.Disposition)
	}

	// The exact same request names a source that is no longer active: the retry
	// is safely refused rather than installing another copy.
	_, err = attemptManagedRefresh(t, s, expected, refreshed)
	var refusal *ManagedGenerationRefreshError
	if !errors.As(err, &refusal) || refusal.Reason != ManagedGenerationRefreshSourceNotActive {
		t.Fatalf("repeat refresh error = %v, want source-not-active", err)
	}
	if rows := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, string(managedRefreshTestSession)); rows != 2 {
		t.Fatalf("session_generations rows after the refused repeat = %d, want 2", rows)
	}

	// Re-reading the active generation and presenting the same capture is the
	// idempotent settle: no third generation is installed.
	settleProof, err := s.ReadIndexState(t.Context(), ingest.SessionID(managedRefreshTestSession))
	if err != nil {
		t.Fatalf("read the installed proof: %v", err)
	}
	settled, err := s.RefreshManagedGeneration(t.Context(), ManagedGenerationRefresh{
		SessionID: managedRefreshTestSession, SourceGenerationID: installed.GenerationID,
		Metadata: refreshed, Expected: settleProof,
	})
	if err != nil {
		t.Fatalf("idempotent settle: %v", err)
	}
	if settled.Disposition != ManagedGenerationRefreshUnchanged || settled.GenerationID != installed.GenerationID {
		t.Fatalf("settle result = %+v, want the installed generation and unchanged", settled)
	}
	if rows := refreshQueryInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, string(managedRefreshTestSession)); rows != 2 {
		t.Fatalf("session_generations rows after the settle = %d, want 2", rows)
	}
}

func TestManagedGenerationRefreshPlanCoversReclaimInventory(t *testing.T) {
	if len(managedGenerationRefreshPlan) != len(reclaimTableNames) {
		t.Fatalf("refresh plan has %d entries for %d generation-scoped tables", len(managedGenerationRefreshPlan), len(reclaimTableNames))
	}
	for _, table := range reclaimTableNames {
		step, ok := managedGenerationRefreshPlan[table]
		if !ok {
			t.Fatalf("generation-scoped table %s has no refresh plan", table)
		}
		if step.action == managedGenerationRefreshClone && step.statement == "" {
			t.Fatalf("clone plan for %s carries no statement", table)
		}
	}
	for table := range managedGenerationRefreshPlan {
		found := false
		for _, inventory := range reclaimTableNames {
			if inventory == table {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("refresh plan names %s outside reclaimTableNames", table)
		}
	}
}
