package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The shim suite seeds one harmonized session and one mirror session, then
// reads both through the same production methods: the shimmed rows must
// match the mirror rows field for field.
//
// The full-read vector below reuses the minted entries (user "hello",
// assistant "hi there", refs mint-test:0/1): its capture hash is the
// fullCaptureHash domain over those exact entries, hardcoded so the test
// pins the domain instead of recomputing it.
const (
	shimSessionID     = "04999aaa-36bc-424c-a789-8be54d9702bd"
	shimGenerationID  = "gen-shim-suite"
	shimFullTokenHash = "2d49f8174a0aa58ad30af2e02b6ac8c14f246e7284e905125916c906499f705e"
	mirrorSessionID   = "05999aaa-36bc-424c-a789-8be54d9702be"
)

// seedShimSuite writes the harmonized session (bodies plus mapping, active
// pointer, capture, and stats rows) and a mirror session with the same
// visible entries bounded the way the mirror writer bound them.
func seedShimSuite(t *testing.T, s *store.Store) {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed shim suite: %v", err)
		}
	}
	exec(`INSERT INTO host_slugs(opaque_id, host_slug) VALUES('shim-host','shim-host')`)
	exec(`INSERT INTO projects(project_hash, canonical_cwd) VALUES('shim-project','/synthetic/shim')`)
	for _, id := range []string{shimSessionID, mirrorSessionID} {
		active := any(nil)
		if id == shimSessionID {
			active = shimGenerationID
		}
		exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id, index_format_version)
VALUES(?, 'opencode', 'shim-model', 'shim-host', 'shim-project', 1, 2, 3, '/synthetic/shim.jsonl', 'jsonl', 11, ?, 2)`, id, active)
	}
	digest := "2222222222222222222222222222222222222222222222222222222222222222"
	exec(`INSERT INTO session_generations(session_id, generation_id, schema_version, harness, model, version,
ts_start, ts_end, source_format, project_hash, project_name, host_slug,
content_hash, metadata_hash, redaction_applied, completeness,
source_evidence_digest, index_format_version, candidate_digest, installed_at_ms)
VALUES(?, ?, 11, 'opencode', 'shim-model', 'v', 1, 2, 'jsonl', 'shim-project', 'shim', 'shim-host',
?, ?, 0, 'complete', ?, 2, ?, 2)`, shimSessionID, shimGenerationID, digest, digest, digest, digest)
	exec(`INSERT INTO session_projection_sections(session_id, generation_id, partition_id) VALUES(?, ?, 0)`, shimSessionID, shimGenerationID)
	texts := []struct {
		role string
		text string
	}{{"user", "hello"}, {"assistant", "hi there"}}
	for i, entry := range texts {
		ref := "mint-test:0"
		if i == 1 {
			ref = "mint-test:1"
		}
		preview := entry.text
		record := store.EntryRecord{
			SessionID:      schema.SessionID(shimSessionID),
			EntryIndex:     i,
			Harness:        schema.HarnessOpenCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.Role(entry.role),
			ContentPreview: &preview,
			Depth:          0,
			SourceEntryRef: schema.SourceEntryRef(ref),
		}
		bodyDigest := store.SerializeEntryDigest(record)
		exec(`INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role,
has_tool_use, has_thinking, is_error, depth, content_preview, source_entry_ref)
VALUES(?, ?, ?, ?, 'opencode', 'text', ?, 0, 0, 0, 0, ?, ?)`,
			int64(store.BodyRowIDBase+400+i), shimSessionID, bodyDigest, i, entry.role, entry.text, ref)
		exec(`INSERT INTO session_generation_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, body_digest)
VALUES(?, ?, 0, ?, ?, ?)`, shimSessionID, shimGenerationID, i, ref, bodyDigest)
		exec(`INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview, depth)
VALUES(?, ?, 'opencode', 'text', ?, ?, 0)`, mirrorSessionID, i, entry.role, entry.text)
	}
	exec(`INSERT INTO session_content_captures(session_id, status, source_authority, transcript_origin, capture_format,
entry_count, content_row_count, full_capture_sha256, captured_at_ms, failure_code, publication_capture_revision)
VALUES(?, 'complete', 'peasant_snapshot', 1, 'full', 2, 2, ?, 4, NULL, 0)`, shimSessionID, shimFullTokenHash)
	exec(`INSERT INTO session_content_captures(session_id, status, source_authority, transcript_origin, capture_format,
entry_count, content_row_count, full_capture_sha256, captured_at_ms, failure_code, publication_capture_revision)
VALUES(?, 'complete', 'peasant_snapshot', 1, 'full', 2, 2, ?, 4, NULL, 0)`, mirrorSessionID, shimFullTokenHash)
}

// TestShimListParity pins the routed list read: ListEntries serves the same
// rows for the shimmed session as for its mirror twin.
func TestShimListParity(t *testing.T) {
	s := openSnapshotStore(t)
	ctx := context.Background()
	seedShimSuite(t, s)
	shimmed, err := s.ListEntries(ctx, ingest.SessionID(shimSessionID))
	if err != nil {
		t.Fatalf("shim list: %v", err)
	}
	mirrored, err := s.ListEntries(ctx, ingest.SessionID(mirrorSessionID))
	if err != nil {
		t.Fatalf("mirror list: %v", err)
	}
	if len(shimmed) != 2 || len(mirrored) != 2 {
		t.Fatalf("lengths = (%d, %d), want (2, 2)", len(shimmed), len(mirrored))
	}
	for i := range shimmed {
		shimmed[i].SessionID = ""
		mirrored[i].SessionID = ""
		if !reflect.DeepEqual(shimmed[i], mirrored[i]) {
			t.Fatalf("entry %d differs:\nshim   = %+v\nmirror = %+v", i, shimmed[i], mirrored[i])
		}
	}
}

// TestShimRangeMaxFirst pins the remaining ListEntries-family routers:
// range, max index, and first entry agree across representations.
func TestShimRangeMaxFirst(t *testing.T) {
	s := openSnapshotStore(t)
	ctx := context.Background()
	seedShimSuite(t, s)
	ranged, err := s.ListEntriesRange(ctx, schema.SessionID(shimSessionID), 1, 1)
	if err != nil {
		t.Fatalf("shim range: %v", err)
	}
	if len(ranged) != 1 || ranged[0].EntryIndex != 1 {
		t.Fatalf("shim range = %+v, want the index-1 entry", ranged)
	}
	maxIdx, err := s.MaxEntryIndex(ctx, schema.SessionID(shimSessionID))
	if err != nil || maxIdx != 1 {
		t.Fatalf("shim max = (%d, %v), want (1, nil)", maxIdx, err)
	}
	head, err := s.FirstEntry(ctx, schema.SessionID(shimSessionID))
	if err != nil || head == nil || head.EntryIndex != 0 || head.Role != schema.Role("user") {
		t.Fatalf("shim first = (%+v, %v), want index 0 user", head, err)
	}
}

// TestShimFirstUserPreviews pins the navigation previews across a mixed
// batch: the shimmed session previews exactly like its mirror twin.
func TestShimFirstUserPreviews(t *testing.T) {
	s := openSnapshotStore(t)
	ctx := context.Background()
	seedShimSuite(t, s)
	single, err := s.FirstUserMessage(ctx, shimSessionID)
	if err != nil || single != "hello" {
		t.Fatalf("shim single = (%q, %v), want (hello, nil)", single, err)
	}
	bulk, err := s.FirstUserMessageBulk(ctx, []string{shimSessionID, mirrorSessionID})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if bulk[shimSessionID] != "hello" || bulk[mirrorSessionID] != "hello" {
		t.Fatalf("bulk = %v, want hello for both", bulk)
	}
	leading, err := s.LeadingUserMessagesBulk(ctx, []string{shimSessionID, mirrorSessionID}, 3)
	if err != nil {
		t.Fatalf("leading: %v", err)
	}
	if len(leading[shimSessionID]) != 1 || leading[shimSessionID][0] != "hello" {
		t.Fatalf("shim leading = %v, want [hello]", leading[shimSessionID])
	}
	if len(leading[mirrorSessionID]) != 1 || leading[mirrorSessionID][0] != "hello" {
		t.Fatalf("mirror leading = %v, want [hello]", leading[mirrorSessionID])
	}
}

// TestShimCoverage pins the existence routers: a harmonized session counts
// as holding entries through its mapping rows.
func TestShimCoverage(t *testing.T) {
	s := openSnapshotStore(t)
	ctx := context.Background()
	seedShimSuite(t, s)
	exists, err := s.SessionEntriesExist(ctx, ingest.SessionID(shimSessionID))
	if err != nil || !exists {
		t.Fatalf("shim exists = (%v, %v), want (true, nil)", exists, err)
	}
	without, err := s.SessionsWithoutEntries(ctx, []ingest.SessionID{ingest.SessionID(shimSessionID), ingest.SessionID(mirrorSessionID), ingest.SessionID("06999aaa-36bc-424c-a789-8be54d9702bf")})
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if without[ingest.SessionID(shimSessionID)] || without[ingest.SessionID(mirrorSessionID)] {
		t.Fatalf("coverage = %v, want false for both seeded sessions", without)
	}
	if !without[ingest.SessionID("06999aaa-36bc-424c-a789-8be54d9702bf")] {
		t.Fatalf("coverage = %v, want true for the missing session", without)
	}
}

// TestShimFullRead pins the publication full read on the harmonized path:
// verified body rows serve whole with the minted capture hash, and the paged
// read honors its cursor contract.
func TestShimFullRead(t *testing.T) {
	s := openSnapshotStore(t)
	ctx := context.Background()
	seedShimSuite(t, s)
	entries, capture, err := s.LoadFullSessionEntries(ctx, ingest.SessionID(shimSessionID), 0)
	if err != nil {
		t.Fatalf("full read: %v", err)
	}
	if len(entries) != 2 || capture.EntryCount != 2 {
		t.Fatalf("full read = (%d entries, count %d), want (2, 2)", len(entries), capture.EntryCount)
	}
	if entries[0].ContentPreview == nil || *entries[0].ContentPreview != "hello" {
		t.Fatalf("first full entry = %+v, want hello", entries[0])
	}
	page, err := s.ReadSessionEntries(ctx, ingest.SessionID(shimSessionID), ingest.SessionEntryReadOptions{
		Mode: ingest.SessionEntryReadFullContent, FromIndex: 1, Limit: 10,
	})
	if err != nil {
		t.Fatalf("paged read: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].EntryIndex != 1 {
		t.Fatalf("page = %+v, want the index-1 entry", page.Entries)
	}
	if page.NextIndex != nil {
		t.Fatalf("page next = %v, want nil at the end", *page.NextIndex)
	}
}
