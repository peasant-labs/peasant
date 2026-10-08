package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The annotation remap suite pins the guarded existence check at all five
// insert sites, across all three representations: a missing start entry
// refuses with the six-part error, and presence reads the session's current
// representation (mapping rows for harmonized sessions, the mirror for
// non-native and file-backed ones).

const (
	annotationHarmonizedID = "07999aaa-36bc-424c-a789-8be54d9702c1"
	annotationFileBackedID = "07999aaa-36bc-424c-a789-8be54d9702c2"
	annotationV1ID         = "07999aaa-36bc-424c-a789-8be54d9702c3"
	annotationGenerationID = "gen-annotation-remap"
)

// openV2TestStore opens a golden-copy store with the format-2 index handler
// registered, so format-2 sessions pass the projection gate.
func openV2TestStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.OpenWith(t, store.WithIndexFormats(store.V2IndexFormat()))
}

// seedAnnotationRemapSuite writes three sessions with entries 0-2 each: a
// harmonized native (bodies plus mapping), a file-backed native (mirror
// rows under an active file-backed pointer), and a V1 session (mirror rows,
// no active generation).
func seedAnnotationRemapSuite(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed annotation remap: %v", err)
		}
	}
	exec(`INSERT INTO host_slugs(opaque_id, host_slug) VALUES('ann-host','ann-host')`)
	exec(`INSERT INTO projects(project_hash, canonical_cwd) VALUES('ann-project','/synthetic/ann')`)
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id, index_format_version)
VALUES(?, 'opencode', 'ann-model', 'ann-host', 'ann-project', 1, 2, 3, '/synthetic/ann.jsonl', 'jsonl', 11, ?, 2)`, annotationHarmonizedID, annotationGenerationID)
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id, index_format_version)
VALUES(?, 'opencode', 'ann-model', 'ann-host', 'ann-project', 1, 2, 3, '/synthetic/ann.jsonl', 'jsonl', 11, 'gen-filebacked', 2)`, annotationFileBackedID)
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, index_format_version)
VALUES(?, 'opencode', 'ann-model', 'ann-host', 'ann-project', 1, 2, 3, '/synthetic/ann.jsonl', 'jsonl', 11, 1)`, annotationV1ID)
	digest := "3333333333333333333333333333333333333333333333333333333333333333"
	exec(`INSERT INTO session_generations(session_id, generation_id, schema_version, harness, model, version,
ts_start, ts_end, source_format, project_hash, project_name, host_slug,
content_hash, metadata_hash, redaction_applied, completeness,
source_evidence_digest, index_format_version, candidate_digest, installed_at_ms)
VALUES(?, ?, 11, 'opencode', 'ann-model', 'v', 1, 2, 'jsonl', 'ann-project', 'ann', 'ann-host',
?, ?, 0, 'complete', ?, 2, ?, 2)`, annotationHarmonizedID, annotationGenerationID, digest, digest, digest, digest)
	exec(`INSERT INTO session_projection_sections(session_id, generation_id, partition_id) VALUES(?, ?, 0)`, annotationHarmonizedID, annotationGenerationID)
	for i := 0; i < 3; i++ {
		ref := "ann-remap:0"
		if i == 1 {
			ref = "ann-remap:1"
		} else if i == 2 {
			ref = "ann-remap:2"
		}
		preview := "entry"
		record := store.EntryRecord{
			SessionID:      schema.SessionID(annotationHarmonizedID),
			EntryIndex:     i,
			Harness:        schema.Harness("opencode"),
			EntryType:      schema.EntryTypeText,
			Role:           schema.Role("user"),
			ContentPreview: &preview,
			Depth:          0,
			SourceEntryRef: schema.SourceEntryRef(ref),
		}
		bodyDigest := store.SerializeEntryDigest(record)
		exec(`INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role,
has_tool_use, has_thinking, is_error, depth, content_preview, source_entry_ref)
VALUES(?, ?, ?, ?, 'opencode', 'text', 'user', 0, 0, 0, 0, ?, ?)`,
			int64(store.BodyRowIDBase+500+i), annotationHarmonizedID, bodyDigest, i, preview, ref)
		exec(`INSERT INTO session_generation_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, body_digest)
VALUES(?, ?, 0, ?, ?, ?)`, annotationHarmonizedID, annotationGenerationID, i, ref, bodyDigest)
		exec(`INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview, depth)
VALUES(?, ?, 'opencode', 'text', 'user', 'entry', 0)`, annotationFileBackedID, i)
		exec(`INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview, depth)
VALUES(?, ?, 'opencode', 'text', 'user', 'entry', 0)`, annotationV1ID, i)
	}
}

// annotationIDs seeds one annotator and type for the remap suite.
func annotationIDs(t *testing.T, s *store.Store) (annotatorID, typeID string) {
	t.Helper()
	return seedAnnotatorIDForTest(t, s), seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)
}

// TestAnnotationTargetsNoFK pins create-annotation-in-range and
// create-annotation-out-of-range-refused: every representation accepts a
// start entry it stores and refuses one it does not, with the six-part
// error.
func TestAnnotationTargetsNoFK(t *testing.T) {
	s := openV2TestStore(t)
	ctx := context.Background()
	seedAnnotationRemapSuite(t, s)
	annotatorID, typeID := annotationIDs(t, s)
	for _, sessionID := range []string{annotationHarmonizedID, annotationFileBackedID, annotationV1ID} {
		if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
			EntryTarget:      &store.EntryTarget{SessionID: sessionID, EntryIndex: 1},
			AnnotatorID:      annotatorID,
			AnnotationTypeID: typeID,
			Value:            "resolved",
		}); err != nil {
			t.Fatalf("create in range for %s: %v", sessionID, err)
		}
		_, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
			EntryTarget:      &store.EntryTarget{SessionID: sessionID, EntryIndex: 99},
			AnnotatorID:      annotatorID,
			AnnotationTypeID: typeID,
			Value:            "resolved",
		})
		if err == nil {
			t.Fatalf("create out of range for %s succeeded, want refusal", sessionID)
		}
		if !strings.Contains(err.Error(), "matches no stored entry") {
			t.Fatalf("create out of range for %s error = %v, want the six-part refusal", sessionID, err)
		}
	}
}

// TestAnnotationSupersedeAndBatchRefused pins
// create-and-supersede-out-of-range-refused and
// batch-create-out-of-range-refused at their sites.
func TestAnnotationSupersedeAndBatchRefused(t *testing.T) {
	s := openV2TestStore(t)
	ctx := context.Background()
	seedAnnotationRemapSuite(t, s)
	annotatorID, typeID := annotationIDs(t, s)
	first, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		EntryTarget:      &store.EntryTarget{SessionID: annotationHarmonizedID, EntryIndex: 0},
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("seed annotation: %v", err)
	}
	if _, err := s.CreateAnnotationAndSupersede(ctx, ingest.CreateAnnotationParams{
		EntryTarget:      &ingest.EntryTarget{SessionID: annotationHarmonizedID, EntryIndex: 77},
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	}, first, "hash-supersede"); err == nil {
		t.Fatal("supersede create out of range succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "matches no stored entry") {
		t.Fatalf("supersede create error = %v, want the six-part refusal", err)
	}
	if _, err := s.BatchCreateAnnotations(ctx, []store.CreateAnnotationParams{
		{
			EntryTarget:      &store.EntryTarget{SessionID: annotationV1ID, EntryIndex: 0},
			AnnotatorID:      annotatorID,
			AnnotationTypeID: typeID,
			Value:            "resolved",
		},
		{
			EntryTarget:      &store.EntryTarget{SessionID: annotationV1ID, EntryIndex: 78},
			AnnotatorID:      annotatorID,
			AnnotationTypeID: typeID,
			Value:            "resolved",
		},
	}); err == nil {
		t.Fatal("batch create with out-of-range target succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "matches no stored entry") {
		t.Fatalf("batch create error = %v, want the six-part refusal", err)
	}
}

// TestClassifierInsertOutOfRangeRefused pins
// classifier-insert-out-of-range-refused through the classifier write path.
func TestClassifierInsertOutOfRangeRefused(t *testing.T) {
	s := openV2TestStore(t)
	ctx := context.Background()
	seedAnnotationRemapSuite(t, s)
	annotatorID, typeID := annotationIDs(t, s)
	sid := annotationFileBackedID
	write := ingest.ClassifierAnnotationWrite{
		Create: ingest.CreateAnnotationParams{
			EntryTarget:      &ingest.EntryTarget{SessionID: sid, EntryIndex: 79},
			AnnotatorID:      annotatorID,
			AnnotationTypeID: typeID,
			Value:            "resolved",
		},
		Find: ingest.FindAnnotationParams{
			AnnotationTypeID: typeID,
			AnnotatorID:      annotatorID,
			SessionID:        &sid,
		},
		ContentHash: "hash-classifier-out-of-range",
	}
	results := s.ApplyClassifierAnnotations(ctx, []ingest.ClassifierAnnotationWrite{write})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("classifier insert out of range succeeded, want refusal")
	}
	if !strings.Contains(results[0].Err.Error(), "matches no stored entry") {
		t.Fatalf("classifier insert error = %v, want the six-part refusal", results[0].Err)
	}
}

// seedRemapSession writes a V1 session with distinct entry previews so
// anchor content keys match uniquely across a re-index.
func seedRemapSession(t *testing.T, s *store.Store, sessionID string, previews []string) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed remap session: %v", err)
		}
	}
	exec(`INSERT INTO host_slugs(opaque_id, host_slug) VALUES('remap-host','remap-host')`)
	exec(`INSERT INTO projects(project_hash, canonical_cwd) VALUES('remap-project','/synthetic/remap')`)
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, index_format_version)
VALUES(?, 'opencode', 'remap-model', 'remap-host', 'remap-project', 1, 2, 3, '/synthetic/remap.jsonl', 'jsonl', 11, 1)`, sessionID)
	for i, preview := range previews {
		exec(`INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview, depth)
VALUES(?, ?, 'opencode', 'text', 'user', ?, 0)`, sessionID, i, preview)
	}
}

// humanAnnotatorID returns the seeded human annotator's ID.
func humanAnnotatorID(t *testing.T, s *store.Store) string {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	var id string
	if err := sqlitex.ExecuteTransient(conn, `SELECT id FROM annotators WHERE name = 'human-web'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			// The vendored driver surfaces TEXT columns directly.
			id = stmt.ColumnText(0)
			return nil
		},
	}); err != nil || id == "" {
		t.Fatalf("human-web annotator not found: %v", err)
	}
	return id
}

// annotationAnchorState reads one annotation's anchor state.
func annotationAnchorState(t *testing.T, s *store.Store, annotationID string) string {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	state := ""
	if err := sqlitex.ExecuteTransient(conn, `SELECT state FROM annotation_target_anchors WHERE annotation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{annotationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			state = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("anchor state: %v", err)
	}
	return state
}

// TestAnnotationRemapAfterReindex pins remap-after-reindex: shifting entries
// across a re-index moves the carried target to the matching span instead
// of dropping it.
func TestAnnotationRemapAfterReindex(t *testing.T) {
	s := openV2TestStore(t)
	ctx := context.Background()
	sessionID := "08999aaa-36bc-424c-a789-8be54d9702d1"
	seedRemapSession(t, s, sessionID, []string{"alpha", "bravo", "charlie"})
	annotatorID, typeID := annotationIDs(t, s)
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		EntryTarget:      &store.EntryTarget{SessionID: sessionID, EntryIndex: 1, EndIndex: 2},
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	}); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}
	// Shift every entry one index forward; the bravo content key now lives
	// at index 2 and matches uniquely.
	shifted := []schema.SessionEntry{
		{SessionID: schema.SessionID(sessionID), EntryIndex: 0, Harness: schema.Harness("opencode"), EntryType: schema.EntryTypeText, Role: schema.Role("user"), ContentPreview: strptr("zero")},
		{SessionID: schema.SessionID(sessionID), EntryIndex: 1, Harness: schema.Harness("opencode"), EntryType: schema.EntryTypeText, Role: schema.Role("user"), ContentPreview: strptr("alpha")},
		{SessionID: schema.SessionID(sessionID), EntryIndex: 2, Harness: schema.Harness("opencode"), EntryType: schema.EntryTypeText, Role: schema.Role("user"), ContentPreview: strptr("bravo")},
		{SessionID: schema.SessionID(sessionID), EntryIndex: 3, Harness: schema.Harness("opencode"), EntryType: schema.EntryTypeText, Role: schema.Role("user"), ContentPreview: strptr("charlie")},
	}
	if err := s.IndexSessionEntries(ctx, schema.SessionID(sessionID), shifted); err != nil {
		t.Fatalf("shift re-index: %v", err)
	}
	rows, err := s.GetAnnotationsForEntry(ctx, sessionID, 2)
	if err != nil || len(rows) != 1 {
		t.Fatalf("remapped annotations at 2 = (%d, %v), want (1, nil)", len(rows), err)
	}
	stale, err := s.GetAnnotationsForEntry(ctx, sessionID, 1)
	if err != nil {
		t.Fatalf("stale span read: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("annotations at old span 1 = %d, want 0 after remap", len(stale))
	}
}

func strptr(v string) *string { return &v }

// TestAnnotationRestoreOutOfRange pins restore-out-of-range-refused and
// unresolved-kept: a carried span with no remappable entry is not
// reattached at its missing span. Machine annotations settle superseded,
// human ones unresolved, and the annotation itself is kept in both cases.
func TestAnnotationRestoreOutOfRange(t *testing.T) {
	for _, human := range []bool{false, true} {
		name := "machine-superseded"
		if human {
			name = "human-unresolved"
		}
		t.Run(name, func(t *testing.T) {
			s := openV2TestStore(t)
			ctx := context.Background()
			sessionID := "08999aaa-36bc-424c-a789-8be54d9702d2"
			if human {
				sessionID = "08999aaa-36bc-424c-a789-8be54d9702d3"
			}
			seedRemapSession(t, s, sessionID, []string{"alpha", "bravo", "charlie"})
			annotatorID, typeID := annotationIDs(t, s)
			if human {
				annotatorID = humanAnnotatorID(t, s)
			}
			annotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
				EntryTarget:      &store.EntryTarget{SessionID: sessionID, EntryIndex: 1, EndIndex: 2},
				AnnotatorID:      annotatorID,
				AnnotationTypeID: typeID,
				Value:            "resolved",
			})
			if err != nil {
				t.Fatalf("seed annotation: %v", err)
			}
			// Drop every entry: nothing remaps, so the span is refused
			// reattachment at its missing index.
			if err := s.IndexSessionEntries(ctx, schema.SessionID(sessionID), nil); err != nil {
				t.Fatalf("empty re-index: %v", err)
			}
			rows, err := s.GetAnnotationsForEntry(ctx, sessionID, 1)
			if err != nil {
				t.Fatalf("missing-span read: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("annotations reattached at missing span = %d, want 0", len(rows))
			}
			wantState := "superseded"
			if human {
				wantState = "unresolved"
			}
			if state := annotationAnchorState(t, s, annotationID); state != wantState {
				t.Fatalf("anchor state = %q, want %q", state, wantState)
			}
		})
	}
}

// TestAnnotationPruneCascade pins prune-cascade: pruning a session removes
// its entry targets with it.
func TestAnnotationPruneCascade(t *testing.T) {
	s := openV2TestStore(t)
	ctx := context.Background()
	seedAnnotationRemapSuite(t, s)
	annotatorID, typeID := annotationIDs(t, s)
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		EntryTarget:      &store.EntryTarget{SessionID: annotationHarmonizedID, EntryIndex: 2},
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	}); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}
	if _, err := s.PruneSessions(ctx, []ingest.SessionID{ingest.SessionID(annotationHarmonizedID)}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	rows, err := s.GetAnnotationsForEntry(ctx, annotationHarmonizedID, 2)
	if err != nil {
		t.Fatalf("targets after prune: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("targets after prune = %d, want 0", len(rows))
	}
}
