package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestContentMigrationConversion runs the conversion-owned
// content_migration cases under their section-10 names: each session ends
// converted, rolled back and marked, skipped, or halted; dry-run counts
// equal applied counts; re-runs are idempotent; non-native rows are
// byte-unchanged.
func TestContentMigrationConversion(t *testing.T) {
	// No t.Parallel here or in the subtests: the seam-hook cases arm
	// process-global hooks, so the whole family runs sequentially and a
	// hook can never fire in the wrong test.
	migrateShadowSeam = nil
	migrateStageSeam = nil
	fixtures := loadContentMigrationFixtures(t)
	for _, c := range fixtures.Cases {
		if c.OwnedBy != "conversion" || c.Profile == "" {
			continue
		}
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if c.Drain != "" {
				runMigrateDrainCase(t, c)
				return
			}
			if c.Driver != "" {
				runMigrateDriverCase(t, c)
				return
			}
			runMigrateConvertCase(t, c, 0)
		})
	}
}

// migrateCaseSessionID derives a deterministic session identity per case
// and index: the seeds never collide across cases or repeated sessions.
func migrateCaseSessionID(t *testing.T, name string, index int) schema.SessionID {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	sid, err := schema.NewSessionID(fmt.Sprintf("%08x-%04x-4%03x-8%03x-%010x%02x",
		sum[0:4], sum[4:6], sum[6], sum[7], sum[8:13], index))
	if err != nil {
		t.Fatalf("derive session identity for %s: %v", name, err)
	}
	return sid
}

// migrateSeedTexts are the long conversion texts: past the 2,000-byte
// mirror bound, so the bounded-shape dimension proves the bound.
func migrateSeedTexts(genID string) (text, input, output string) {
	text = strings.Repeat("migration text for "+genID+" ", 200)
	input = strings.Repeat("migration input for "+genID+" ", 200)
	output = strings.Repeat("migration output for "+genID+" ", 200)
	return text, input, output
}

// seedMigrateProfile builds the file-backed seed for one conversion
// profile and returns the candidate with its blob bytes.
func seedMigrateProfile(t *testing.T, s *Store, root string, sid schema.SessionID, profile, genID string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	text, input, output := migrateSeedTexts(genID)
	switch profile {
	case "clean", "extra-keys", "newer-stats", "rich-metadata", "annotated", "preview-only", "settled-refusal", "two-earlier", "empty-blob", "parent-only-metadata", "parent-row-wins":
		v2, blobs := buildTestGeneration(t, sid, genID, text, input, output)
		switch profile {
		case "preview-only":
			v2.Generation.Main.Entries[0].Provenance = &schema.ContentProvenance{
				Origin:        schema.ContentOriginSubmittedInput,
				Actor:         schema.ActorOriginUnknown,
				Delivery:      schema.DeliveryOriginSessionAdmission,
				Ownership:     schema.ContentOwnershipLocal,
				Evidence:      schema.EvidenceNativeTyped,
				InputModality: schema.InputModalityText,
			}
		case "extra-keys":
			applyMigrateExtraKeys(t, &v2, blobs)
		case "rich-metadata":
			applyMigrateRichMetadata(t, s, sid, &v2, blobs, genID)
		case "parent-only-metadata", "parent-row-wins":
			applyMigrateParentOnlyMetadata(t, s, sid, &v2, profile == "parent-row-wins")
		case "two-earlier":
			applyMigrateTwoEarlier(t, sid, &v2, blobs, genID)
		case "empty-blob":
			applyMigrateEmptyBlob(t, &v2, blobs)
		}
		stampSeedMetadataHash(&v2)
		full := profile != "preview-only"
		seedFileBackedGeneration(t, s, root, sid, v2, blobs, full)
		if profile == "parent-row-wins" {
			seedMigrateParentRow(t, s, sid)
		}
		switch profile {
		case "annotated":
			seedMigrateAnnotation(t, s, sid)
		case "newer-stats":
			seedMigrateNewerStats(t, s, sid)
		case "settled-refusal":
			seedMigrateSettledRefusal(t, s, sid)
		case "rich-metadata":
			seedMigrateRichChildren(t, s, sid, genID, v2)
		}
		return v2, blobs
	case "superseded":
		v2, blobs := buildTestGeneration(t, sid, genID, text, input, output)
		stampSeedMetadataHash(&v2)
		old, oldBlobs := buildTestGeneration(t, sid, genID+"-old", text, input, output)
		stampSeedMetadataHash(&old)
		seedFileBackedGeneration(t, s, root, sid, old, oldBlobs, true)
		seedFileBackedGeneration(t, s, root, sid, v2, blobs, true)
		execMigrateSQL(t, s, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = '`+string(sid)+`'`)
		return v2, blobs
	case "pending-intent":
		v2, blobs := buildTestGeneration(t, sid, genID, text, input, output)
		stampSeedMetadataHash(&v2)
		seedFileBackedGeneration(t, s, root, sid, v2, blobs, true)
		seedMigratePendingIntent(t, s, root, sid, v2, blobs)
		return v2, blobs
	case "non-emitted":
		v2, blobs := buildTestGeneration(t, sid, genID, text, input, output)
		extra := schema.SourceEntryRef("e_migrate_extra")
		v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: extra})
		blobs[extra] = []byte(strings.Repeat("non-emitted bytes for "+genID+" ", 100))
		v2.Generation.Segments = []indexformat.ContextSegment{{
			Ordinal: 0, PhysicalSourceID: "source-migrate-extra",
			Coordinates:  indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
			Inclusion:    indexformat.SegmentInclusionInherited,
			CapturedRefs: []schema.SourceEntryRef{extra},
		}}
		stampSeedMetadataHash(&v2)
		seedFileBackedGeneration(t, s, root, sid, v2, blobs, true)
		return v2, blobs
	case "non-native-full", "non-native-preview-only":
		seedMigrateNonNative(t, s, sid, profile == "non-native-full")
		return indexformat.V2{}, nil
	default:
		t.Fatalf("unknown migrate profile %q", profile)
		return indexformat.V2{}, nil
	}
}

// applyMigrateExtraKeys gives the seed entries canonical extra documents
// with known keys to promote and unknown keys to carry: the conversion
// promotes the four known keys into columns and keeps the remainder.
func applyMigrateExtraKeys(t *testing.T, v2 *indexformat.V2, _ map[schema.SourceEntryRef][]byte) {
	t.Helper()
	entries := v2.Generation.Main.Entries
	if len(entries) != 3 {
		t.Fatalf("seed generation holds %d entries, want 3", len(entries))
	}
	extras := []string{
		`{"cache_read":1,"cache_write":2,"custom":"kept","model_id":"m-1","tokens_reasoning":5}`,
		`{"model_id":"m-2","z":1}`,
		`{"unrelated":{"nested":true}}`,
	}
	for i := range entries {
		extra := extras[i]
		entries[i].Extra = &extra
	}
}

// applyMigrateTwoEarlier extends the seed candidate with two earlier
// partitions in distinct history states, each with one emitted entry
// carrying a content record and bytes like the main ones. The shadow
// verify aligns staged rows against ascending partition order, so this
// profile pins the ordering a map range would randomize.
func applyMigrateTwoEarlier(t *testing.T, sid schema.SessionID, v2 *indexformat.V2, blobs map[schema.SourceEntryRef][]byte, genID string) {
	t.Helper()
	states := []schema.EarlierHistoryState{
		schema.EarlierHistoryUncertainMigrated,
		schema.EarlierHistoryUncertainUnresolved,
	}
	for i, state := range states {
		ref := schema.SourceEntryRef(fmt.Sprintf("e_earlier_%d", i+1))
		text := fmt.Sprintf("earlier %d text for %s ", i+1, genID) + strings.Repeat("x", 100)
		v2.Generation.Earlier = append(v2.Generation.Earlier, indexformat.EarlierPartition{
			State: state,
			Content: indexformat.Partition{Entries: []schema.SessionEntry{{
				SessionID: sid, EntryIndex: 0, Harness: v2.Generation.Metadata.ModelHarness,
				EntryType: schema.EntryTypeText, Role: schema.RoleUser,
				ContentPreview: &text, SourceEntryRef: ref,
			}}},
		})
		v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: ref})
		blobs[ref] = []byte(text)
	}
}

// applyMigrateEmptyBlob adds one zero-length non-emitted ref with its
// bytes and covering segment: the conversion must carry the empty blob
// through instead of refusing it as a torn object.
func applyMigrateEmptyBlob(t *testing.T, v2 *indexformat.V2, blobs map[schema.SourceEntryRef][]byte) {
	t.Helper()
	empty := schema.SourceEntryRef("e_migrate_empty")
	v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: empty})
	blobs[empty] = []byte{}
	v2.Generation.Segments = []indexformat.ContextSegment{{
		Ordinal: 0, PhysicalSourceID: "source-migrate-empty",
		Coordinates:  indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
		Inclusion:    indexformat.SegmentInclusionInherited,
		CapturedRefs: []schema.SourceEntryRef{empty},
	}}
}

// applyMigrateParentOnlyMetadata names a parent in the captured document
// without touching the sessions row, the shape thousands of live subagent
// sessions hold. When rowWins it also records the winning row parent;
// seedMigrateParentRow applies it after seeding because the mirror batch
// below rewrites the sessions row.
func applyMigrateParentOnlyMetadata(t *testing.T, s *Store, sid schema.SessionID, v2 *indexformat.V2, rowWins bool) {
	t.Helper()
	docParent := migrateCaseSessionID(t, "migrate-doc-parent", 0)
	seedGenerationSession(t, s, string(docParent))
	v2.Generation.Metadata.ParentUUID = &docParent
	if rowWins {
		rowParent := migrateCaseSessionID(t, "migrate-row-parent", 0)
		seedGenerationSession(t, s, string(rowParent))
	}
}

// seedMigrateParentRow sets the sessions row parent after the file-backed
// seed (and its mirror batch) completes, so the conversion meets a set
// row the way a live row survives harvests that preserve parentage.
func seedMigrateParentRow(t *testing.T, s *Store, sid schema.SessionID) {
	t.Helper()
	rowParent := migrateCaseSessionID(t, "migrate-row-parent", 0)
	execMigrateSQL(t, s, `UPDATE sessions SET parent_id = '`+string(rowParent)+`' WHERE session_id = '`+string(sid)+`'`)
}

// collections the metadata dimension proves: a parent link, subagents, a
// commit, a relationship with an anchor, title refs, an earlier
// partition, native metadata, and a context segment.
func applyMigrateRichMetadata(t *testing.T, s *Store, sid schema.SessionID, v2 *indexformat.V2, blobs map[schema.SourceEntryRef][]byte, genID string) {
	t.Helper()
	parent := migrateCaseSessionID(t, "migrate-parent", 0)
	seedGenerationSession(t, s, string(parent))
	execMigrateSQL(t, s, `UPDATE sessions SET parent_id = '`+string(parent)+`' WHERE session_id = '`+string(sid)+`'`)
	metadata := &v2.Generation.Metadata
	metadata.Subagents = []schema.SubagentRef{{SessionID: parent, ParentUUID: sid}}
	metadata.Git.Commits = []schema.CommitInfo{{
		Hash: "abc123", Message: "seed commit", AuthorName: "seed", AuthorEmail: "seed@example.com",
		CommitTime: 1700000000, AuthorTime: 1700000000,
	}}
	metadata.Relationships = []schema.SessionRelationship{{
		Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetKnown,
		TargetLocalID: &parent, Evidence: schema.EvidenceNativeTyped,
		Anchor: &schema.PublicSourceAnchor{Kind: schema.PublicSourceAnchorGeneral},
	}}
	v2.Generation.TitleRefs = []schema.SourceEntryRef{"e_u1", "e_result1"}
	earlierText := strings.Repeat("earlier text for "+genID+" ", 100)
	earlier := schema.SessionEntry{
		SessionID: sid, EntryIndex: 0, Harness: metadata.ModelHarness,
		EntryType: schema.EntryTypeText, Role: schema.RoleUser,
		ContentPreview: &earlierText, SourceEntryRef: "e_earlier",
	}
	// The old model keeps a blob for every emitted ref, so the earlier
	// entry carries a content record with its bytes like the main ones.
	v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: "e_earlier"})
	blobs["e_earlier"] = []byte(earlierText)
	v2.Generation.Earlier = []indexformat.EarlierPartition{{
		State:   schema.EarlierHistoryUncertainMigrated,
		Content: indexformat.Partition{Entries: []schema.SessionEntry{earlier}},
	}}
	data := json.RawMessage(`{"usage":"seed"}`)
	v2.Generation.Main.NativeMetadata = []schema.NativeMetadataRecord{{
		ID: "n1", Kind: schema.NativeMetadataPiCustomData,
		Source: schema.NativeSourceRef{EntryRef: "e_u1", SourceType: schema.NativeSourcePiCustom},
		Data:   data,
	}}
	v2.Generation.Segments = []indexformat.ContextSegment{{
		Ordinal: 0, PhysicalSourceID: "source-seed",
		Coordinates:  indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
		Inclusion:    indexformat.SegmentInclusionInherited,
		CapturedRefs: []schema.SourceEntryRef{"e_u1"},
	}}
	_ = genID
}

// seedMigrateRichChildren writes the old child rows the oracle carries
// and the file-backed seed does not: the structured relationship rows
// (post-reshape shape) and the native metadata rows. Segments ride the
// file-backed seed from the candidate.
func seedMigrateRichChildren(t *testing.T, s *Store, sid schema.SessionID, genID string, v2 indexformat.V2) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	exec := func(script string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, script, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed rich children: %v", err)
		}
	}
	for ordinal, relationship := range v2.Generation.Metadata.Relationships {
		var target, evidence, anchorKind, anchorRef any
		if relationship.TargetLocalID != nil {
			target = string(*relationship.TargetLocalID)
		}
		if relationship.Evidence != "" {
			evidence = string(relationship.Evidence)
		}
		if relationship.Anchor != nil {
			anchorKind = string(relationship.Anchor.Kind)
			if relationship.Anchor.SourceEntryRef != "" {
				anchorRef = string(relationship.Anchor.SourceEntryRef)
			}
		}
		exec(`INSERT INTO session_relationship_evidence(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			string(sid), genID, ordinal, string(relationship.Kind), string(relationship.TargetState), target, evidence, anchorKind, anchorRef)
	}
	for partition, records := range map[int][]schema.NativeMetadataRecord{0: v2.Generation.Main.NativeMetadata} {
		for ordinal, record := range records {
			exec(`INSERT INTO session_section_native_metadata(session_id, generation_id, partition_id, ordinal, native_id, kind, source_entry_ref, source_type, source_message_role, attachment_turn_index, attachment_tool_call_id, custom_type, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL, ?)`,
				string(sid), genID, partition, ordinal, record.ID, string(record.Kind), string(record.Source.EntryRef), string(record.Source.SourceType), string(record.Data))
		}
	}
}

// seedMigrateAnnotation targets the seed's first entry through the
// guarded insert, which checks file-backed sessions against the mirror.
// The annotator and type come from the baseline seeds, like every other
// annotation test's.
func seedMigrateAnnotation(t *testing.T, s *Store, sid schema.SessionID) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(query string) string {
		t.Helper()
		var id string
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				id = stmt.ColumnText(0)
				return nil
			},
		}); err != nil || id == "" {
			t.Fatalf("annotation seed lookup: %v", err)
		}
		return id
	}
	annotator := lookup(`SELECT id FROM annotators WHERE name = 'outcome-classifier'`)
	typeID := lookup(`SELECT id FROM annotation_types WHERE type_id = 'quality.session_outcome'`)
	s.pool.Put(conn)
	_, err = s.CreateAnnotation(ctx, CreateAnnotationParams{
		EntryTarget:      &EntryTarget{SessionID: string(sid), EntryIndex: 0, EndIndex: 1},
		AnnotatorID:      annotator,
		AnnotationTypeID: typeID,
		Value:            "migrate annotation",
	})
	if err != nil {
		t.Fatalf("seed annotation: %v", err)
	}
}

// seedMigratePendingIntent stages a pending intent with its staged
// generation directory plus a reserved staging directory: the drain
// discards all three without replay.
func seedMigratePendingIntent(t *testing.T, s *Store, root string, sid schema.SessionID, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) {
	t.Helper()
	intentDir := filepath.Join(root, string(sid))
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "generation-intent.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := v2.Generation
	staged.ID = "staged-old"
	filled := filledCandidateForValidation(t, indexformat.V2{Generation: staged}, blobs)
	stagedDir := filepath.Join(intentDir, "generations", "staged-old")
	if err := os.MkdirAll(stagedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(filled.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagedDir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, record := range filled.Generation.Content {
		payload, ok := blobs[record.Ref]
		if !ok {
			t.Fatalf("no staged bytes for content record %q", record.Ref)
		}
		if err := os.WriteFile(filepath.Join(stagedDir, record.RelativeBlob), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(intentDir, "generations", ".tmp-gen-reserved"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedMigrateNonNative writes a legacy-representation session with no
// generation-backed rows at all: the conversion never selects it.
func seedMigrateNonNative(t *testing.T, s *Store, sid schema.SessionID, full bool) {
	t.Helper()
	ctx := context.Background()
	text := strings.Repeat("legacy text ", 300)
	entries := []schema.SessionEntry{{
		SessionID: sid, EntryIndex: 0, Harness: schema.Harness("claude-code"),
		EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text,
	}}
	capture := ingest.SessionContentCaptureWrite{
		Status:          ingest.ContentCaptureIncomplete,
		SourceAuthority: ingest.ContentSourceNone,
		CaptureFormat:   ingest.ContentCaptureFormatPreviewOnly,
	}
	if full {
		capture = ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureComplete,
			SourceAuthority:  ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatFull,
			CapturedAtMs:     1,
		}
	}
	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID:          sid,
		Result:             indexformat.V1{Entries: entries},
		IndexVersion:       1,
		IndexerVersion:     1,
		IndexedAtMs:        1,
		RequireFullContent: full,
		ContentCapture:     capture,
	}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed non-native mirror rows: %+v", results)
	}
}

// seedMigrateNewerStats applies a COMPUTE-style derived update with a
// later stamp over the backfilled row: the conversion's older harness
// upsert must not clobber it, and the conversion still proceeds.
func seedMigrateNewerStats(t *testing.T, s *Store, sid schema.SessionID) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	turnCount, toolCalls, subagents := 99, 50, 5
	duration := int64(424242)
	tokensIn, tokensOut := 111, 222
	if _, err := upsertCapturedStatsOnConn(conn, CapturedStats{
		SessionID:     sid,
		TurnCount:     &turnCount,
		ToolCallCount: &toolCalls,
		SubagentCount: &subagents,
		DurationMs:    &duration,
		TokensIn:      &tokensIn,
		TokensOut:     &tokensOut,
		Source:        StatsSourceDerived,
		UpdatedAtMs:   6000,
	}); err != nil {
		t.Fatalf("seed newer derived stats: %v", err)
	}
}

// seedMigrateSettledRefusal marks the capture failed with a failure code
// this build cannot lift: conversion never touches the session.
func seedMigrateSettledRefusal(t *testing.T, s *Store, sid schema.SessionID) {
	t.Helper()
	execMigrateSQL(t, s, `UPDATE session_content_captures SET status = 'incomplete', full_capture_sha256 = NULL, failure_code = 'source_records_omitted', failure_message = 'settled' WHERE session_id = '`+string(sid)+`'`)
}

// execMigrateSQL runs one seed script on a pooled connection.
func execMigrateSQL(t *testing.T, s *Store, script string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("exec seed SQL: %v", err)
	}
}

// queryMigrateString reads one nullable string column.
func queryMigrateString(t *testing.T, s *Store, query string) *string {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var value *string
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				text := stmt.ColumnText(0)
				value = &text
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

// queryMigrateInt reads one integer column.
func queryMigrateInt(t *testing.T, s *Store, query string) int64 {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var value int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			value = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

// applyMigrateDamage corrupts exactly one dimension of the seeded input
// or, for the aux-json and detail-bytes damages, arms the shadow seam to
// corrupt one derived row between the catalog insert and the verify. It
// returns a restore function for the heal case, or nil when the damage
// cannot be restored.
func applyMigrateDamage(t *testing.T, s *Store, root string, sid schema.SessionID, genID string, damage string) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	get := func(query string, args ...any) string {
		t.Helper()
		var value string
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				value = stmt.ColumnText(0)
				return nil
			},
		}); err != nil {
			t.Fatalf("read damage target: %v", err)
		}
		return value
	}
	put := func(script string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, script, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("apply damage: %v", err)
		}
	}
	switch damage {
	case "field-blob", "serialization":
		raw := get(`SELECT entry_json FROM session_projection_entries WHERE session_id = ? AND generation_id = ? AND partition_id = 0 AND entry_index = 2`, string(sid), genID)
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatal(err)
		}
		if damage == "field-blob" {
			entry["toolOutput"] = strings.Repeat("divergent tool output ", 200)
		} else {
			entry["toolCallId"] = "call-divergent"
		}
		modified, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		put(`UPDATE session_projection_entries SET entry_json = ? WHERE session_id = ? AND generation_id = ? AND partition_id = 0 AND entry_index = 2`, string(modified), string(sid), genID)
		return func() {
			conn, err := s.pool.Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer s.pool.Put(conn)
			if err := sqlitex.ExecuteTransient(conn, `UPDATE session_projection_entries SET entry_json = ? WHERE session_id = ? AND generation_id = ? AND partition_id = 0 AND entry_index = 2`, &sqlitex.ExecOptions{
				Args: []any{raw, string(sid), genID},
			}); err != nil {
				t.Fatalf("restore entry_json: %v", err)
			}
		}
	case "missing-blob", "damaged-blob":
		rel := get(`SELECT relative_blob FROM session_projection_content WHERE session_id = ? AND generation_id = ? ORDER BY source_entry_ref LIMIT 1`, string(sid), genID)
		blobPath := filepath.Join(root, string(sid), "generations", genID, rel)
		payload, err := os.ReadFile(blobPath)
		if err != nil {
			t.Fatal(err)
		}
		if damage == "missing-blob" {
			if err := os.Remove(blobPath); err != nil {
				t.Fatal(err)
			}
			return func() {
				if err := os.WriteFile(blobPath, payload, 0o600); err != nil {
					t.Fatalf("restore blob: %v", err)
				}
			}
		}
		payload[0] ^= 0xff
		if err := os.WriteFile(blobPath, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		return nil
	case "shim":
		// The tool-names column is mirror-shaped but hydration-blind:
		// corrupting it breaks the bounded-shape dimension while the
		// oracle full read still hydrates.
		put(`UPDATE session_entries SET tool_names_csv = 'shim-corrupt' WHERE session_id = ? AND entry_index = 0`, string(sid))
		return nil
	case "aux-json":
		armMigrateSeamOnce(t, "shadow", func(conn *sqlite.Conn, stage string) error {
			if stage != "before-shadow-verify" {
				return nil
			}
			return sqlitex.ExecuteTransient(conn, `UPDATE session_generation_title_refs SET source_entry_ref = 'e_bogus' WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
				Args: []any{string(sid), genID},
			})
		})
		return nil
	case "detail-bytes":
		armMigrateSeamOnce(t, "shadow", func(conn *sqlite.Conn, stage string) error {
			if stage != "before-shadow-verify" {
				return nil
			}
			// The catalog row is immutable, so the seam drops its
			// trigger for the corrupt write: the defect halt rolls
			// the whole transaction back, trigger with it.
			if err := sqlitex.ExecuteTransient(conn, `DROP TRIGGER session_generations_immutable`, nil); err != nil {
				return err
			}
			return sqlitex.ExecuteTransient(conn, `UPDATE session_generations SET completeness = 'incomplete_new' WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
				Args: []any{string(sid), genID},
			})
		})
		return nil
	case "full-shape":
		rewriteMigrateFullText(t, s, sid, 0, strings.Repeat("divergent full tail ", 500))
		return nil
	case "capture-hash":
		put(`UPDATE session_content_captures SET full_capture_sha256 = '`+strings.Repeat("e", 64)+`' WHERE session_id = ?`, string(sid))
		return nil
	case "session-hash":
		put(`UPDATE sessions SET session_entries_hash = '`+strings.Repeat("f", 64)+`' WHERE session_id = ?`, string(sid))
		return nil
	case "null-hash":
		put(`UPDATE sessions SET session_entries_hash = NULL WHERE session_id = ?`, string(sid))
		return nil
	case "unknown-key":
		// A top-level key the catalog has no column for: the struct decode
		// would drop it, so the verify must refuse instead of converting
		// with silent loss.
		raw := get(`SELECT metadata_json FROM session_projection_generations WHERE session_id = ? AND generation_id = ?`, string(sid), genID)
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		doc["futureUnknownKey"] = "not-yet-promoted"
		modified, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		put(`UPDATE session_projection_generations SET metadata_json = ? WHERE session_id = ? AND generation_id = ?`, string(modified), string(sid), genID)
		return nil
	case "stale-hash":
		// The stored hash predates the current hashed fields (stats and
		// timestamps move without a rehash), so the rebuilt document can
		// never reproduce it byte for byte. The conversion still proceeds:
		// the verify compares hash-excluded bytes and later reads recompute
		// the fresh hash.
		raw := get(`SELECT metadata_json FROM session_projection_generations WHERE session_id = ? AND generation_id = ?`, string(sid), genID)
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		doc["metadataHash"] = strings.Repeat("0", 64)
		modified, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		put(`UPDATE session_projection_generations SET metadata_json = ? WHERE session_id = ? AND generation_id = ?`, string(modified), string(sid), genID)
		return func() {
			conn, err := s.pool.Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer s.pool.Put(conn)
			if err := sqlitex.ExecuteTransient(conn, `UPDATE session_projection_generations SET metadata_json = ? WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
				Args: []any{raw, string(sid), genID},
			}); err != nil {
				t.Fatalf("restore metadata_json: %v", err)
			}
		}
	default:
		t.Fatalf("unknown migrate damage %q", damage)
		return nil
	}
}

// armMigrateSeamOnce arms a migration seam hook that fires once, then
// disarms itself and restores the production nil through cleanup. The
// shadow hook runs on the catalog transaction's connection; the stage
// hook runs between body sub-transactions with no connection. Seam cases
// never run parallel: the hooks are process-global.
func armMigrateSeamOnce(t *testing.T, which string, hook func(conn *sqlite.Conn, stage string) error) {
	t.Helper()
	fired := false
	fire := func(conn *sqlite.Conn, stage string) error {
		if fired {
			return nil
		}
		fired = true
		return hook(conn, stage)
	}
	switch which {
	case "shadow":
		previous := migrateShadowSeam
		migrateShadowSeam = fire
		t.Cleanup(func() {
			migrateShadowSeam = previous
		})
	case "stage":
		previous := migrateStageSeam
		migrateStageSeam = func(stage string) error {
			return fire(nil, stage)
		}
		t.Cleanup(func() {
			migrateStageSeam = previous
		})
	default:
		t.Fatalf("unknown migrate seam %q", which)
	}
}

// rewriteMigrateFullText replaces one entry's full text with internally
// consistent chunks and manifest rows: the mirror and entry documents
// stay intact, so only the full-shape dimension refuses. The divergent
// text keeps the stored preview head, which the manifest still certifies.
func rewriteMigrateFullText(t *testing.T, s *Store, sid schema.SessionID, entryIndex int, tail string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	preview := ""
	if err := sqlitex.ExecuteTransient(conn, `SELECT content_preview FROM session_entries WHERE session_id = ? AND entry_index = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), entryIndex},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			preview = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	text := preview + tail
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_entry_full_content SET full_byte_length = ?, full_sha256 = ?, chunk_count = ? WHERE session_id = ? AND entry_index = ?`, &sqlitex.ExecOptions{
		Args: []any{len(text), digest, (len(text) + fullContentChunkBytes - 1) / fullContentChunkBytes, string(sid), entryIndex},
	}); err != nil {
		t.Fatalf("rewrite full manifest: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_full_content_chunks WHERE session_id = ? AND entry_index = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), entryIndex},
	}); err != nil {
		t.Fatalf("clear full chunks: %v", err)
	}
	for index, offset := 0, 0; offset < len(text); index, offset = index+1, offset+fullContentChunkBytes {
		end := offset + fullContentChunkBytes
		if end > len(text) {
			end = len(text)
		}
		chunk := text[offset:end]
		chunkSum := sha256.Sum256([]byte(chunk))
		if err := sqlitex.Execute(conn, `INSERT INTO session_entry_full_content_chunks(session_id, entry_index, chunk_index, byte_offset, byte_length, chunk_sha256, data) VALUES (?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{
			Args: []any{string(sid), entryIndex, index, offset, len(chunk), hex.EncodeToString(chunkSum[:]), []byte(chunk)},
		}); err != nil {
			t.Fatalf("rewrite full chunk %d: %v", index, err)
		}
	}
}

// runMigrateConvertCase seeds one conversion session, applies the case
// damage, runs the conversion, and asserts the expected disposition with
// its end state. A data mismatch rolls back and marks; a defect mismatch
// halts naming the session and the dimension.
func runMigrateConvertCase(t *testing.T, c contentMigrationCase, index int) {
	t.Helper()
	s, root := openGenerationStore(t)
	sid := migrateCaseSessionID(t, c.Name, index)
	seedGenerationSession(t, s, string(sid))
	genID := "gen_migrate_case"
	v2, _ := seedMigrateProfile(t, s, root, sid, c.Profile, genID)
	var skipBefore, sessionBefore string
	nonNative := c.Profile == "non-native-full" || c.Profile == "non-native-preview-only"
	if c.Profile == "settled-refusal" || nonNative || c.AssertSkipState {
		skipBefore, sessionBefore = snapshotMigrateSkipState(t, s, sid)
	}
	var restore func()
	if c.Damage != "" {
		restore = applyMigrateDamage(t, s, root, sid, genID, c.Damage)
	}
	if c.Crash == "stage-once" || c.Crash == "shadow-once" {
		which := "stage"
		if c.Crash == "shadow-once" {
			which = "shadow"
		}
		armMigrateSeamOnce(t, which, func(_ *sqlite.Conn, stage string) error {
			return errors.New("injected crash before " + stage)
		})
		outcome, err := s.MigrateSession(context.Background(), sid)
		if err == nil {
			t.Fatalf("crash seam did not interrupt the conversion (outcome %q)", outcome)
		}
		var rollback *MigrateDataRollbackError
		if errors.As(err, &rollback) {
			t.Fatalf("crash seam returned a data rollback; a crash must halt, not roll back: %v", err)
		}
		outcome, err = s.MigrateSession(context.Background(), sid)
		if err != nil {
			t.Fatalf("resume after the crash: %v", err)
		}
		if outcome != MigrateOutcomeConverted {
			t.Fatalf("resume outcome = %q, want converted", outcome)
		}
		assertSessionConverted(t, s, root, sid, genID)
		return
	}
	outcome, err := s.MigrateSession(context.Background(), sid)
	switch c.Expect {
	case "converted":
		if err != nil {
			t.Fatalf("MigrateSession: %v", err)
		}
		if outcome != MigrateOutcomeConverted {
			t.Fatalf("outcome = %q, want converted", outcome)
		}
		assertSessionConverted(t, s, root, sid, genID)
		assertMigrateProfile(t, s, root, sid, genID, c, v2)
		if c.AssertSkipState {
			assertMigrateSkipState(t, s, sid, skipBefore, sessionBefore)
		}
		if c.Heal {
			t.Fatalf("heal is only set with a damage that rolls back")
		}
		// A converted session re-runs idempotently: no work left.
		if again, err := s.MigrateSession(context.Background(), sid); err != nil || again != MigrateOutcomeSkipped {
			t.Fatalf("re-run outcome = %q, err = %v; want skipped idempotence", again, err)
		}
	case "rolled-back":
		var rollback *MigrateDataRollbackError
		if !errors.As(err, &rollback) {
			t.Fatalf("MigrateSession err = %v (outcome %q); want a data rollback", err, outcome)
		}
		if outcome != MigrateOutcomeRolledBack {
			t.Fatalf("outcome = %q, want rolled_back", outcome)
		}
		if c.ExpectDimension != "" && rollback.Dimension != c.ExpectDimension {
			t.Fatalf("rollback dimension = %q, want %q", rollback.Dimension, c.ExpectDimension)
		}
		assertMigrateRolledBack(t, s, root, sid, genID, rollback)
		if c.Heal {
			if restore == nil {
				t.Fatalf("heal is set but the damage cannot be restored")
			}
			restore()
			again, err := s.MigrateSession(context.Background(), sid)
			if err != nil {
				t.Fatalf("convert after healing: %v", err)
			}
			if again != MigrateOutcomeConverted {
				t.Fatalf("post-heal outcome = %q, want converted", again)
			}
			assertSessionConverted(t, s, root, sid, genID)
		}
	case "halted":
		if err == nil {
			t.Fatalf("MigrateSession outcome = %q; want a halting defect error", outcome)
		}
		var rollback *MigrateDataRollbackError
		if errors.As(err, &rollback) {
			t.Fatalf("defect case returned a data rollback; a defect must halt: %v", err)
		}
		if !strings.Contains(err.Error(), string(sid)) {
			t.Fatalf("halt error does not name the session: %v", err)
		}
		assertMigrateOldIntact(t, s, root, sid, genID)
	case "skipped":
		if err != nil {
			t.Fatalf("MigrateSession: %v", err)
		}
		if outcome != MigrateOutcomeSkipped {
			t.Fatalf("outcome = %q, want skipped", outcome)
		}
		assertMigrateUntouched(t, s, root, sid, c.Profile, skipBefore, sessionBefore)
	default:
		t.Fatalf("unknown expect %q", c.Expect)
	}
}

// assertMigrateRolledBack proves the data-rollback end state: the old
// representation is intact and readable, the session is marked for
// re-index, and the sweep flag stays set for the orphan sweep.
func assertMigrateRolledBack(t *testing.T, s *Store, root string, sid schema.SessionID, genID string, rollback *MigrateDataRollbackError) {
	t.Helper()
	if rollback.Dimension == "" {
		t.Fatal("data rollback names no dimension")
	}
	t.Logf("rolled back at %s: %s", rollback.Dimension, rollback.Reason)
	assertMigrateOldIntact(t, s, root, sid, genID)
	if hash := queryMigrateString(t, s, `SELECT indexed_input_hash FROM sessions WHERE session_id = '`+string(sid)+`'`); hash != nil {
		t.Fatalf("rolled-back session keeps input proof %q; want it cleared for re-index", *hash)
	}
	if flag := queryMigrateInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id = '`+string(sid)+`'`); flag != 1 {
		t.Fatalf("rolled-back session flag = %d; want it set for the orphan sweep", flag)
	}
}

// assertMigrateOldIntact proves the old representation survived: the
// projection rows, the mirror rows, and the owned generation directory
// are all still present.
func assertMigrateOldIntact(t *testing.T, s *Store, root string, sid schema.SessionID, genID string) {
	t.Helper()
	for _, table := range []string{"session_projection_generations", "session_projection_entries", "session_projection_content"} {
		if count := queryMigrateInt(t, s, `SELECT COUNT(*) FROM `+table+` WHERE session_id = '`+string(sid)+`'`); count == 0 {
			t.Fatalf("old %s rows are gone for session %s; a rollback must keep them", table, sid)
		}
	}
	if count := queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_entries WHERE session_id = '`+string(sid)+`'`); count == 0 {
		t.Fatalf("mirror rows are gone for session %s; a rollback must keep them", sid)
	}
	entries, err := os.ReadDir(filepath.Join(root, string(sid), "generations", genID))
	if err != nil || len(entries) == 0 {
		t.Fatalf("owned generation directory is gone for session %s; a rollback must keep it", sid)
	}
	_ = genID
}

// assertMigrateUntouched proves a skipped session changed in no way: its
// rows, capture state, and files are exactly as seeded.
func assertMigrateUntouched(t *testing.T, s *Store, root string, sid schema.SessionID, profile, skipBefore, sessionBefore string) {
	t.Helper()
	switch profile {
	case "non-native-full", "non-native-preview-only":
		if count := queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_entries WHERE session_id = '`+string(sid)+`'`); count == 0 {
			t.Fatalf("non-native mirror rows changed for session %s", sid)
		}
		if count := queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_generations WHERE session_id = '`+string(sid)+`'`); count != 0 {
			t.Fatalf("non-native session %s gained harmonized rows", sid)
		}
	case "settled-refusal":
		assertMigrateOldIntact(t, s, root, sid, "gen_migrate_case")
		skipAfter, sessionAfter := snapshotMigrateSkipState(t, s, sid)
		if skipAfter != skipBefore || sessionAfter != sessionBefore {
			t.Fatalf("settled refusal changed for session %s", sid)
		}
	default:
		t.Fatalf("no untouched assertions for profile %q", profile)
	}
}

// snapshotMigrateSkipState captures the session's skip and bookkeeping
// state: the index state plus the sessions-row columns conversion must
// preserve.
func snapshotMigrateSkipState(t *testing.T, s *Store, sid schema.SessionID) (string, string) {
	t.Helper()
	state, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatalf("read index state: %v", err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var row string
	if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(session_entries_hash, ''), COALESCE(indexed_input_hash, ''), COALESCE(artifact_hash, ''), COALESCE(adapter_version, -1), COALESCE(input_submission_count, -1), COALESCE(metric_seed_json, ''), content_sweep_pending, COALESCE(active_generation_id, ''), COALESCE(parent_id, '') FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			var columns []string
			for i := 0; i < 9; i++ {
				columns = append(columns, stmt.ColumnText(i))
			}
			row = strings.Join(columns, "|")
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return string(raw), row
}

// assertMigrateSkipState proves the conversion preserved the skip and
// bookkeeping state: the index state and every sessions-row column but
// the sweep flag, which the conversion sets and clears symmetrically.
func assertMigrateSkipState(t *testing.T, s *Store, sid schema.SessionID, skipBefore, sessionBefore string) {
	t.Helper()
	skipAfter, sessionAfter := snapshotMigrateSkipState(t, s, sid)
	if skipAfter != skipBefore {
		t.Fatalf("index state changed across conversion:\nbefore %s\nafter  %s", skipBefore, skipAfter)
	}
	if sessionAfter != sessionBefore {
		t.Fatalf("sessions row changed across conversion:\nbefore %s\nafter  %s", sessionBefore, sessionAfter)
	}
}

// assertMigrateProfile proves the profile-specific post-conversion
// state: promoted extra keys, the surviving newer stats row, the
// recomputed metadata anchor, the kept annotation, and the drained
// discards.
func assertMigrateProfile(t *testing.T, s *Store, _ string, sid schema.SessionID, genID string, c contentMigrationCase, v2 indexformat.V2) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	switch c.Profile {
	case "preview-only":
		seed := v2.Generation.Main.Entries[0]
		if seed.SourceEntryRef == "" || seed.Provenance == nil {
			t.Fatal("preview-only seed must carry both generation fields")
		}
		records, err := shimBodyRecordsOnConn(conn, string(sid), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 0 {
			t.Fatal("converted preview-only generation has no body rows")
		}
		if records[0].SourceEntryRef != seed.SourceEntryRef || records[0].Provenance == nil {
			t.Fatal("conversion discarded generation fields instead of projecting the mirror shape")
		}
		entries, err := shimListEntries(records, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.SourceEntryRef != "" || entry.Provenance != nil {
				t.Fatalf("mirror-shaped entry %d leaks generation fields", entry.EntryIndex)
			}
		}
		if entries[0].ContentPreview == nil || *entries[0].ContentPreview != *seed.ContentPreview {
			t.Fatal("preview-only mirror shape must preserve the unbounded preview")
		}
	case "two-earlier":
		partitions, records, err := harmonizedPartitionsOnConn(conn, sid, genID)
		if err != nil {
			t.Fatalf("read earlier partitions after conversion: %v", err)
		}
		if len(partitions.earlier) != len(v2.Generation.Earlier) {
			t.Fatalf("earlier partitions = %d, want %d", len(partitions.earlier), len(v2.Generation.Earlier))
		}
		for i, want := range v2.Generation.Earlier {
			if partitions.earlier[i].State != want.State {
				t.Fatalf("earlier partition %d state = %s, want %s", i+1, partitions.earlier[i].State, want.State)
			}
			for _, entry := range want.Content.Entries {
				mapped := 0
				if err := sqlitex.Execute(conn, `SELECT 1 FROM session_generation_entries m JOIN session_entry_bodies b ON b.session_id=m.session_id AND b.body_digest=m.body_digest WHERE m.session_id=? AND m.generation_id=? AND m.partition_id=? AND m.entry_index=? AND m.source_entry_ref=? AND b.content_preview=?`, &sqlitex.ExecOptions{
					Args: []any{string(sid), genID, i + 1, entry.EntryIndex, string(entry.SourceEntryRef), *entry.ContentPreview},
					ResultFunc: func(*sqlite.Stmt) error {
						mapped++
						return nil
					},
				}); err != nil {
					t.Fatal(err)
				}
				if mapped != 1 {
					t.Fatalf("earlier partition %d ref %s has %d surviving mapping/body rows, want 1", i+1, entry.SourceEntryRef, mapped)
				}
				found := false
				for _, record := range records {
					if record.Ref != entry.SourceEntryRef {
						continue
					}
					found = true
					data, err := readHarmonizedContentOnConn(conn, sid, genID, record)
					if err != nil || string(data) != *entry.ContentPreview {
						t.Fatalf("earlier ref %s content changed: %v", record.Ref, err)
					}
				}
				if !found {
					t.Fatalf("earlier ref %s missing from converted content records", entry.SourceEntryRef)
				}
			}
		}
	case "empty-blob":
		rows := 0
		if err := sqlitex.ExecuteTransient(conn, `SELECT c.byte_length, c.digest FROM session_generation_content g JOIN session_content c ON c.session_id=g.session_id AND c.digest=g.digest WHERE g.session_id=? AND g.generation_id=? AND g.source_entry_ref='e_migrate_empty'`, &sqlitex.ExecOptions{
			Args: []any{string(sid), genID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows++
				if stmt.ColumnInt64(0) != 0 {
					t.Fatalf("empty blob byte_length = %d, want 0", stmt.ColumnInt64(0))
				}
				if sum := sha256.Sum256(nil); stmt.ColumnText(1) != hex.EncodeToString(sum[:]) {
					t.Fatal("empty blob digest does not certify zero bytes")
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("empty blob has %d surviving mapping/content rows, want 1", rows)
		}
	case "extra-keys":
		type promoted struct {
			model                  *string
			reasoning, read, write *int
			extra, verbatim        *string
		}
		_ = promoted{}
		byIndex := map[int]map[string]*string{}
		cols := []string{"model_id", "tokens_reasoning", "cache_read", "cache_write", "extra", "extra_verbatim"}
		if err := sqlitex.ExecuteTransient(conn, `SELECT entry_index, model_id, tokens_reasoning, cache_read, cache_write, extra, extra_verbatim FROM session_entry_bodies WHERE session_id = ? ORDER BY entry_index`, &sqlitex.ExecOptions{
			Args: []any{string(sid)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				row := map[string]*string{}
				for i, col := range cols {
					if stmt.ColumnType(i+1) != sqlite.TypeNull {
						value := stmt.ColumnText(i + 1)
						row[col] = &value
					}
				}
				byIndex[stmt.ColumnInt(0)] = row
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		str := func(row map[string]*string, col, want string) {
			t.Helper()
			got := row[col]
			if got == nil || *got != want {
				value := "<nil>"
				if got != nil {
					value = *got
				}
				t.Fatalf("entry extra %s = %q, want %q", col, value, want)
			}
		}
		str(byIndex[0], "model_id", "m-1")
		str(byIndex[0], "tokens_reasoning", "5")
		str(byIndex[0], "cache_read", "1")
		str(byIndex[0], "cache_write", "2")
		str(byIndex[0], "extra", `{"custom":"kept"}`)
		str(byIndex[1], "model_id", "m-2")
		str(byIndex[1], "extra", `{"z":1}`)
		str(byIndex[2], "extra", `{"unrelated":{"nested":true}}`)
		if byIndex[2]["extra_verbatim"] != nil {
			t.Fatalf("canonical remainder stored verbatim: %q", *byIndex[2]["extra_verbatim"])
		}
	case "newer-stats":
		stats, err := readCapturedStatsOnConn(conn, sid)
		if err != nil {
			t.Fatal(err)
		}
		if stats.TurnCount == nil || *stats.TurnCount != 99 {
			t.Fatalf("converted stats turn_count = %v; the newer derived row did not survive", stats.TurnCount)
		}
		if stats.Source != StatsSourceDerived || stats.UpdatedAtMs != 6000 {
			t.Fatalf("converted stats source/stamp = %q/%d; want derived/6000", stats.Source, stats.UpdatedAtMs)
		}
		seed, err := json.Marshal(v2.Generation.Metadata.Stats)
		if err != nil {
			t.Fatal(err)
		}
		if stats.SeedJSON == nil || *stats.SeedJSON != string(seed) {
			t.Fatalf("converted seed_json changed; a derived update must never clobber the harness seed")
		}
	case "parent-only-metadata":
		wantParent := v2.Generation.Metadata.ParentUUID
		if wantParent == nil {
			t.Fatalf("seed metadata carries no parent; the profile must name one")
		}
		var storedParent *string
		if err := sqlitex.ExecuteTransient(conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sid)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if stmt.ColumnType(0) != sqlite.TypeNull {
					value := stmt.ColumnText(0)
					storedParent = &value
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		storedValue := "<null>"
		if storedParent != nil {
			storedValue = *storedParent
		}
		if storedParent == nil || *storedParent != string(*wantParent) {
			t.Fatalf("converted sessions.parent_id = %s, want %s", storedValue, string(*wantParent))
		}
		snapshot, err := harmonizedReadSnapshotOnConn(conn, sid, genID)
		if err != nil {
			t.Fatalf("read converted snapshot: %v", err)
		}
		if snapshot.Metadata.ParentUUID == nil || *snapshot.Metadata.ParentUUID != *wantParent {
			t.Fatalf("converted snapshot parent = %v, want %s", snapshot.Metadata.ParentUUID, string(*wantParent))
		}
	case "rich-metadata":
		var stored string
		if err := sqlitex.ExecuteTransient(conn, `SELECT metadata_hash FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sid), genID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				stored = stmt.ColumnText(0)
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		if stored != statsExcludedMetadataHash(v2.Generation.Metadata) {
			t.Fatalf("stored metadata_hash is not the stats-excluded anchor")
		}
	case "annotated":
		rows, err := s.GetEntryAnnotationsForSession(ctx, string(sid))
		if err != nil {
			t.Fatalf("read annotations after conversion: %v", err)
		}
		kept := 0
		for _, row := range rows {
			if row.TargetEntryIndex != nil && *row.TargetEntryIndex == 0 {
				kept++
			}
		}
		if kept != 1 {
			t.Fatalf("entry annotations at index 0 after conversion = %d; want the kept annotation", kept)
		}
		return
	case "superseded":
		if count := queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_projection_generations WHERE session_id = '`+string(sid)+`'`); count != 0 {
			t.Fatalf("superseded projection rows remain: %d", count)
		}
		return
	case "pending-intent":
		if intent, err := s.generationArtifacts.ReadIntent(ctx, sid); err != nil || intent != nil {
			t.Fatalf("pending intent survives the drain: %v %+v", err, intent)
		}
		dirs, err := s.generationArtifacts.ListGenerationDirectories(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if len(dirs) != 0 {
			t.Fatalf("staged directories survive the drain: %v", dirs)
		}
		return
	}
}

// runMigrateDriverCase runs the driver-level conversion cases: the
// rollback threshold, the search consolidation shapes, the dry run, and
// the converted-with-leftovers resume.
func runMigrateDriverCase(t *testing.T, c contentMigrationCase) {
	t.Helper()
	switch c.Driver {
	case "threshold":
		runMigrateThresholdCase(t, c)
	case "consolidate", "consolidate-unconverted", "consolidate-twice":
		runMigrateConsolidateCase(t, c)
	case "dry-run":
		runMigrateDryRunCase(t, c)
	case "resume":
		runMigrateResumeCase(t, c)
	default:
		t.Fatalf("unknown driver %q", c.Driver)
	}
}

// runMigrateThresholdCase seeds count damaged sessions and proves the
// systematic-cause halt: the run stops past 1% data rollbacks with a
// minimum of 20, recording every rollback with its dimension.
func runMigrateThresholdCase(t *testing.T, c contentMigrationCase) {
	t.Helper()
	if c.Count < 21 {
		t.Fatalf("threshold case needs count >= 21, has %d", c.Count)
	}
	s, root := openGenerationStore(t)
	for i := 0; i < c.Count; i++ {
		sid := migrateCaseSessionID(t, c.Name, i)
		seedGenerationSession(t, s, string(sid))
		genID := "gen_migrate_case"
		seedMigrateProfile(t, s, root, sid, c.Profile, genID)
		applyMigrateDamage(t, s, root, sid, genID, c.Damage)
	}
	result, err := s.Migrate(context.Background(), MigrateOptions{})
	if err == nil || !strings.Contains(err.Error(), "exceeds 1%") {
		t.Fatalf("Migrate err = %v; want the systematic-cause halt", err)
	}
	if len(result.Rollbacks) != migrateDataRollbackMin+1 {
		t.Fatalf("rollbacks = %d; want the halt at %d", len(result.Rollbacks), migrateDataRollbackMin+1)
	}
	for _, rollback := range result.Rollbacks {
		if rollback.Dimension == "" {
			t.Fatalf("rollback of %s names no dimension", rollback.SessionID)
		}
	}
}

// runMigrateConsolidateCase proves the Phase 3 shapes: the rebuild over
// the union view, the dropped old index with its retargeted triggers,
// and the cleared flag. The unconverted variant keeps a settled
// file-backed session searchable through the consolidated index; the
// twice variant proves the re-run skips the phase.
func runMigrateConsolidateCase(t *testing.T, c contentMigrationCase) {
	t.Helper()
	s, root := openGenerationStore(t)
	sid := migrateCaseSessionID(t, c.Name, 0)
	seedGenerationSession(t, s, string(sid))
	genID := "gen_migrate_case"
	seedMigrateProfile(t, s, root, sid, c.Profile, genID)
	var settled schema.SessionID
	if c.Driver == "consolidate-unconverted" {
		settled = migrateCaseSessionID(t, c.Name, 1)
		seedGenerationSession(t, s, string(settled))
		seedMigrateProfile(t, s, root, settled, "settled-refusal", genID)
	}
	result, err := s.Migrate(context.Background(), MigrateOptions{})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if result.Converted != 1 {
		t.Fatalf("converted = %d; want 1", result.Converted)
	}
	if !result.SearchConsolidated {
		t.Fatal("search consolidation did not run")
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer s.pool.Put(conn)
		if tableExistsOnConn(conn, "session_entries_fts") {
			t.Fatal("the retired search index survives consolidation")
		}
		for _, trigger := range []string{"session_entries_ai", "session_entries_ad", "session_entries_au"} {
			found := false
			if err := sqlitex.Execute(conn, `SELECT 1 FROM sqlite_master WHERE type = 'trigger' AND name = ?`, &sqlitex.ExecOptions{
				Args: []any{trigger},
				ResultFunc: func(*sqlite.Stmt) error {
					found = true
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatalf("retargeted trigger %s is missing", trigger)
			}
		}
		state, err := searchStateReadOnConn(conn)
		if err != nil {
			t.Fatal(err)
		}
		if state.NeedsRebuild {
			t.Fatal("the rebuild flag stays set after consolidation")
		}
	}()
	// The check connection returns before the match queries and the
	// second pass: the driver takes its own connections, and holding one
	// across the call would starve the pool.
	assertMigrateMatch(t, s, "migration")
	if c.Driver == "consolidate-unconverted" {
		if count := queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_projection_generations WHERE session_id = '`+string(settled)+`'`); count == 0 {
			t.Fatalf("unconverted session %s lost its old rows", settled)
		}
		assertMigrateMatch(t, s, "migration text")
	}
	if c.Driver == "consolidate-twice" {
		again, err := s.Migrate(context.Background(), MigrateOptions{})
		if err != nil {
			t.Fatalf("second Migrate: %v", err)
		}
		if again.Converted != 0 || again.SearchConsolidated {
			t.Fatalf("second pass converted %d and consolidated %v; want no work", again.Converted, again.SearchConsolidated)
		}
	}
}

// assertMigrateMatch proves the consolidated index serves a term through
// the production search path.
func assertMigrateMatch(t *testing.T, s *Store, term string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_search_fts WHERE session_search_fts MATCH ? LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{term},
		ResultFunc: func(*sqlite.Stmt) error {
			found = true
			return nil
		},
	}); err != nil {
		t.Fatalf("match over the consolidated index: %v", err)
	}
	if !found {
		t.Fatalf("consolidated index misses term %q", term)
	}
}

// runMigrateDryRunCase proves a dry run writes nothing and its counts
// equal the applied counts.
func runMigrateDryRunCase(t *testing.T, c contentMigrationCase) {
	t.Helper()
	s, root := openGenerationStore(t)
	sid := migrateCaseSessionID(t, c.Name, 0)
	seedGenerationSession(t, s, string(sid))
	genID := "gen_migrate_case"
	seedMigrateProfile(t, s, root, sid, c.Profile, genID)
	before := snapshotMigrateStore(t, s, root, sid)
	plan, err := s.PlanMigration(context.Background())
	if err != nil {
		t.Fatalf("PlanMigration: %v", err)
	}
	if len(plan.Sessions) != 1 {
		t.Fatalf("plan sessions = %d; want 1", len(plan.Sessions))
	}
	after := snapshotMigrateStore(t, s, root, sid)
	if before != after {
		t.Fatalf("dry run changed the store:\nbefore %s\nafter  %s", before, after)
	}
	result, err := s.Migrate(context.Background(), MigrateOptions{})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if result.Converted != int64(len(plan.Sessions)) {
		t.Fatalf("converted = %d but the plan listed %d; dry-run counts must equal applied counts", result.Converted, len(plan.Sessions))
	}
}

// snapshotMigrateStore fingerprints the convertible state: the old rows,
// the mirror, the flag, and the owned files.
func snapshotMigrateStore(t *testing.T, s *Store, root string, sid schema.SessionID) string {
	t.Helper()
	var fingerprint []string
	for _, table := range []string{"session_projection_generations", "session_projection_entries", "session_projection_content", "session_entries", "session_entry_full_content", "session_entry_full_content_chunks"} {
		fingerprint = append(fingerprint, fmt.Sprintf("%s=%d", table, queryMigrateInt(t, s, `SELECT COUNT(*) FROM `+table+` WHERE session_id = '`+string(sid)+`'`)))
	}
	fingerprint = append(fingerprint, fmt.Sprintf("flag=%d", queryMigrateInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id = '`+string(sid)+`'`)))
	if hash := queryMigrateString(t, s, `SELECT indexed_input_hash FROM sessions WHERE session_id = '`+string(sid)+`'`); hash == nil {
		fingerprint = append(fingerprint, "hash=NULL")
	} else {
		fingerprint = append(fingerprint, "hash="+*hash)
	}
	var files []string
	_ = filepath.Walk(filepath.Join(root, string(sid)), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, fmt.Sprintf("%s:%d", rel, info.Size()))
		}
		return nil
	})
	fingerprint = append(fingerprint, "files="+strings.Join(files, ","))
	return strings.Join(fingerprint, "\n")
}

// runMigrateResumeCase proves the converted-with-leftovers resume: a
// converted session with restored owned files, a set flag, and the
// crash artifacts heals fully on the next pass.
func runMigrateResumeCase(t *testing.T, c contentMigrationCase) {
	t.Helper()
	s, root := openGenerationStore(t)
	sid := migrateCaseSessionID(t, c.Name, 0)
	seedGenerationSession(t, s, string(sid))
	genID := "gen_migrate_case"
	seedMigrateProfile(t, s, root, sid, c.Profile, genID)
	if outcome, err := s.MigrateSession(context.Background(), sid); err != nil || outcome != MigrateOutcomeConverted {
		t.Fatalf("convert outcome = %q, err = %v; want converted", outcome, err)
	}
	genDir := filepath.Join(root, string(sid), "generations", genID)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "c_leftover.blob"), []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, string(sid), "metadata.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	execMigrateSQL(t, s, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = '`+string(sid)+`'`)
	result, err := s.Migrate(context.Background(), MigrateOptions{})
	if err != nil {
		t.Fatalf("resume Migrate: %v", err)
	}
	if result.Converted != 0 {
		t.Fatalf("resume converted %d; the session was already converted", result.Converted)
	}
	assertSessionConverted(t, s, root, sid, genID)
}

// TestMigrateInterruptFinishesSession proves the stop-point contract: a
// cancelled context finishes the current session's transaction and exits
// cleanly with a partial report instead of a half-converted session.
func TestMigrateInterruptFinishesSession(t *testing.T) {
	s, root := openGenerationStore(t)
	for i := 0; i < 2; i++ {
		sid := migrateCaseSessionID(t, "migrate-interrupt", i)
		seedGenerationSession(t, s, string(sid))
		seedMigrateProfile(t, s, root, sid, "clean", "gen_migrate_case")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	result, err := s.Migrate(ctx, MigrateOptions{Progress: func(progress MigrateProgress) {
		calls++
		if progress.Phase == MigrationPhaseConvertSessions && progress.SessionsDone == 1 {
			cancel()
		}
	}})
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Migrate err = %v; want the clean interrupt", err)
	}
	if result.Converted != 1 {
		t.Fatalf("converted = %d; want the finished session in the partial report", result.Converted)
	}
	if calls == 0 {
		t.Fatal("no progress events fired")
	}
	// Exactly one session finished converting; the interrupted one keeps
	// its old representation for the next pass.
	done := 0
	for i := 0; i < 2; i++ {
		sid := migrateCaseSessionID(t, "migrate-interrupt", i)
		conn, err := s.pool.Take(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, harmonized, err := harmonizedActiveOnConn(conn, sid)
		s.pool.Put(conn)
		if err != nil {
			t.Fatal(err)
		}
		if harmonized {
			done++
			assertSessionConverted(t, s, root, sid, "gen_migrate_case")
		} else {
			assertMigrateOldIntact(t, s, root, sid, "gen_migrate_case")
		}
	}
	if done != 1 {
		t.Fatalf("converted sessions = %d; want exactly the finished one", done)
	}
}

// TestPlanMigrationSamplesMismatches proves the Phase 0 preflight
// forecasts rollbacks: a sampled session with a field/blob mismatch is
// reported before anything converts, while a clean store samples clean.
func TestPlanMigrationSamplesMismatches(t *testing.T) {
	s, root := openGenerationStore(t)
	clean := migrateCaseSessionID(t, "migrate-sample-clean", 0)
	seedGenerationSession(t, s, string(clean))
	seedMigrateProfile(t, s, root, clean, "clean", "gen_migrate_case")
	damaged := migrateCaseSessionID(t, "migrate-sample-damaged", 0)
	seedGenerationSession(t, s, string(damaged))
	seedMigrateProfile(t, s, root, damaged, "clean", "gen_migrate_case")
	applyMigrateDamage(t, s, root, damaged, "gen_migrate_case", "field-blob")
	plan, err := s.PlanMigration(context.Background())
	if err != nil {
		t.Fatalf("PlanMigration: %v", err)
	}
	if plan.SampledSessions != 2 {
		t.Fatalf("sampled sessions = %d; want both work sessions", plan.SampledSessions)
	}
	if plan.SampledMismatchSessions != 1 {
		t.Fatalf("sampled mismatch sessions = %d; want the damaged one", plan.SampledMismatchSessions)
	}
	if plan.SampledMismatchedRefs == 0 {
		t.Fatal("sampled mismatched refs = 0; want the divergent ref counted")
	}
}
