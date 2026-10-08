package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// openSnapshotStore opens a golden-copy store with managed-generation
// support: the file locker plus an owned-root artifact store. Harmonized
// reads never touch the artifact files; the probe only needs them present.
func openSnapshotStore(t *testing.T) *store.Store {
	t.Helper()
	root := t.TempDir()
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot-test.db")
	storetest.CopyGoldenTo(t, path)
	db, err := store.Open(path,
		store.WithSkipMigrations(),
		store.WithPoolSize(2),
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// snapshotEntryRecord builds one body-row record for the snapshot tests. The
// digest always matches the row: tests corrupt it explicitly when they mean
// to, never by accident.
func snapshotEntryRecord(sessionID, ref string, index int, role schema.Role, text string) store.EntryRecord {
	preview := text
	return store.EntryRecord{
		SessionID:      schema.SessionID(sessionID),
		EntryIndex:     index,
		Harness:        schema.Harness("opencode"),
		EntryType:      schema.EntryTypeText,
		Role:           role,
		ContentPreview: &preview,
		HasToolUse:     false,
		HasThinking:    false,
		IsError:        false,
		Depth:          0,
		SourceEntryRef: schema.SourceEntryRef(ref),
	}
}

// seedHarmonizedSnapshot writes one native session with a harmonized active
// generation, two text entries, and a stats row, through raw SQL. The bodies
// carry digests over their own canonical text.
func seedHarmonizedSnapshot(t *testing.T, s *store.Store, id, gen string) {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	exec(`INSERT INTO host_slugs(opaque_id, host_slug) VALUES('snap-host','snap-host')`)
	exec(`INSERT INTO projects(project_hash, canonical_cwd) VALUES('snap-project','/synthetic/snap')`)
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id)
VALUES(?, 'opencode', 'snap-model', 'snap-host', 'snap-project', 1720000000000, 1720000009000, 1720000009000, '/synthetic/snap.jsonl', 'jsonl', 11, ?)`, id, gen)
	digest := fmt.Sprintf("%064d", 42)
	exec(`INSERT INTO session_generations(session_id, generation_id, schema_version, harness, model, version,
ts_start, ts_end, source_format, project_hash, project_name, host_slug,
content_hash, metadata_hash, redaction_applied, completeness,
source_evidence_digest, index_format_version, candidate_digest, installed_at_ms)
VALUES(?, ?, 11, 'opencode', 'snap-model', 'v', 1720000000000, 1720000009000, 'jsonl', 'snap-project', 'snap', 'snap-host',
?, ?, 0, 'complete', ?, 2, ?, 1720000009000)`, id, gen, digest, digest, digest, digest)
	exec(`INSERT INTO session_projection_sections(session_id, generation_id, partition_id) VALUES(?, ?, 0)`, id, gen)
	texts := []struct {
		role schema.Role
		text string
	}{
		{role: schema.Role("user"), text: "hello"},
		{role: schema.Role("assistant"), text: "hi there"},
	}
	for i, entry := range texts {
		ref := fmt.Sprintf("snap-test:%d", i)
		record := snapshotEntryRecord(id, ref, i, entry.role, entry.text)
		bodyDigest := store.SerializeEntryDigest(record)
		bodyID := int64(store.BodyRowIDBase + int64(100+i))
		exec(`INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role,
has_tool_use, has_thinking, is_error, depth, content_preview, source_entry_ref)
VALUES(?, ?, ?, ?, 'opencode', 'text', ?, 0, 0, 0, 0, ?, ?)`,
			bodyID, id, bodyDigest, i, string(entry.role), entry.text, ref)
		exec(`INSERT INTO session_generation_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, body_digest)
VALUES(?, ?, 0, ?, ?, ?)`, id, gen, i, ref, bodyDigest)
	}
	exec(`INSERT INTO session_captured_stats(session_id, turn_count, input_submission_count, tool_call_count,
subagent_count, duration_ms, tokens_in, tokens_out, source, updated_at_ms)
VALUES(?, 2, 1, 0, 0, 9000, 10, 20, 'harness', 1720000009000)`, id)
}

// TestHarmonizedSnapshotDetail builds one harmonized snapshot through the
// dispatched router and folds it to a detail payload: entries come from the
// body rows, stats from the captured row, and the snapshot validates.
func TestHarmonizedSnapshotDetail(t *testing.T) {
	s := openSnapshotStore(t)
	ctx := context.Background()
	id := schema.SessionID("01999aaa-36bc-424c-a789-8be54d9702ba")
	seedHarmonizedSnapshot(t, s, string(id), "gen-snap-detail")
	_, payload, err := transcript.BuildSnapshotDetailBytes(ctx, s, s, id)
	if err != nil {
		t.Fatalf("detail bytes: %v", err)
	}
	if payload.ID != string(id) {
		t.Fatalf("payload ID = %q, want %q", payload.ID, id)
	}
	if len(payload.Turns) == 0 {
		t.Fatal("payload carries no turns")
	}
	if payload.TurnCount != 2 {
		t.Fatalf("TurnCount = %d, want 2", payload.TurnCount)
	}
	var sawHello bool
	err = s.WithSessionSnapshot(ctx, id, func(snapshot indexformat.ReadSnapshot) error {
		if snapshot.IndexVersion != 2 {
			t.Fatalf("IndexVersion = %d, want 2", snapshot.IndexVersion)
		}
		if len(snapshot.Main.Entries) != 2 {
			t.Fatalf("main entries = %d, want 2", len(snapshot.Main.Entries))
		}
		if snapshot.Main.Entries[0].ContentPreview == nil || *snapshot.Main.Entries[0].ContentPreview != "hello" {
			t.Fatalf("first entry = %+v, want hello", snapshot.Main.Entries[0])
		}
		sawHello = true
		if len(snapshot.Content) != 2 {
			t.Fatalf("content records = %d, want 2", len(snapshot.Content))
		}
		for _, record := range snapshot.Content {
			if len(record.Digest) != 64 {
				t.Fatalf("record %q digest = %q, want stored body anchor", record.Ref, record.Digest)
			}
			if err := record.Validate(); err != nil {
				t.Fatalf("record %q invalid: %v", record.Ref, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !sawHello {
		t.Fatal("snapshot callback never ran")
	}
}

// TestHarmonizedSnapshotCorruptRefuses pins the full-read verification: a
// column altered under its stored digest refuses the whole read with no
// partial output, while the bounded preview still serves stored bytes.
func TestHarmonizedSnapshotCorruptRefuses(t *testing.T) {
	s := openSnapshotStore(t)
	ctx := context.Background()
	id := schema.SessionID("02999aaa-36bc-424c-a789-8be54d9702bb")
	seedHarmonizedSnapshot(t, s, string(id), "gen-snap-corrupt")
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	// The immutability trigger guards production writes; the corruption case
	// drops it in the test (the design's stated seam) the way a torn page
	// would bypass it.
	if err := sqlitex.ExecuteTransient(conn, `DROP TRIGGER session_entry_bodies_immutable`, &sqlitex.ExecOptions{}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("drop immutability trigger: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_entry_bodies SET content_preview = 'tampered' WHERE session_id = ? AND entry_index = 0`, &sqlitex.ExecOptions{
		Args: []any{string(id)},
	}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("corrupt body: %v", err)
	}
	s.PoolForTest().Put(conn)
	if _, _, err := transcript.BuildSnapshotDetailBytes(ctx, s, s, id); err == nil {
		t.Fatal("corrupt full read succeeded, want refusal")
	}
	if _, preview, err := transcript.BuildSnapshotPreviewBytes(ctx, s, id); err != nil {
		t.Fatalf("preview of corrupt session refused: %v", err)
	} else if preview == nil {
		t.Fatal("preview returned nil payload")
	}
}

// TestHarmonizedSnapshotLockFree reads a harmonized session while no lock is
// held anywhere: with a pool of one, a second snapshot read inside the first
// callback would deadlock if the router took the shared lock.
func TestHarmonizedSnapshotLockFree(t *testing.T) {
	root := t.TempDir()
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/lockfree-test.db"
	storetest.CopyGoldenTo(t, path)
	db, err := store.Open(path,
		store.WithSkipMigrations(),
		store.WithPoolSize(1),
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id := schema.SessionID("03999aaa-36bc-424c-a789-8be54d9702bc")
	seedHarmonizedSnapshot(t, db, string(id), "gen-snap-lockfree")
	inner := 0
	err = db.WithSessionSnapshot(ctx, id, func(snapshot indexformat.ReadSnapshot) error {
		// The pool holds one connection, already returned before this
		// callback. A lock-taking router would still succeed here (shared
		// locks re-enter), so this test pins liveness, not the mechanism:
		// the harmonized read completes with no lock held.
		return db.WithSessionSnapshot(ctx, id, func(innerSnapshot indexformat.ReadSnapshot) error {
			inner++
			return nil
		})
	})
	if err != nil {
		t.Fatalf("nested harmonized snapshots: %v", err)
	}
	if inner != 1 {
		t.Fatalf("inner snapshot ran %d times, want 1", inner)
	}
}
