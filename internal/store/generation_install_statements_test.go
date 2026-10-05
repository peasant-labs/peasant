package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestGenerationInstallRowRoundTrip proves the reused per-row install
// statements write exactly the rows the generation describes. It installs a
// multi-partition generation that carries projection entries, an earlier
// section, context segments, content records, native aliases and relationship
// evidence, then reads every family back from durable rows. The generation
// builder is the existing multi-partition detail-serve candidate; the test only
// adds the row families it does not already carry.
func TestGenerationInstallRowRoundTrip(t *testing.T) {
	sid, err := schema.NewSessionID("a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := schema.NewSessionID("b2b2b2b2-b2b2-4b2b-8b2b-b2b2b2b2b2b2")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	seedGenerationSession(t, s, string(parent))

	genID := "g-install-round-trip"
	candidate, blobs := buildDetailServeGeneration(t, sid, genID)
	inherited := schema.SourceEntryRef("e_inh")
	candidate.Generation.Segments = []indexformat.ContextSegment{
		{
			Ordinal:          0,
			PhysicalSourceID: "source-inherited",
			Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
			Inclusion:        indexformat.SegmentInclusionInherited,
			CapturedRefs:     []schema.SourceEntryRef{inherited},
		},
		{
			Ordinal:          1,
			PhysicalSourceID: "source-native",
			Coordinates: indexformat.SegmentCoordinates{
				Kind:                    indexformat.CoordinateKindCodexOrdinalRange,
				Start:                   installTestInt64Ptr(10),
				EndExclusive:            installTestInt64Ptr(20),
				DecodedByteStart:        installTestInt64Ptr(100),
				DecodedByteEndExclusive: installTestInt64Ptr(200),
			},
			Inclusion:    indexformat.SegmentInclusionSameThreadSurvivingOwn,
			CapturedRefs: []schema.SourceEntryRef{"e_old"},
		},
	}
	candidate.Generation.Content = append(candidate.Generation.Content, indexformat.ContentRecord{Ref: inherited})
	candidate.Generation.Aliases = []indexformat.NativeAlias{
		{NativeKey: "native-u1", Ref: "e_u1"},
		{NativeKey: "native-inh", Ref: inherited},
	}
	candidate.Generation.Metadata.Relationships = []schema.SessionRelationship{{
		Kind:          schema.SessionRelationshipStartedBy,
		TargetState:   schema.RelationshipTargetKnown,
		TargetLocalID: &parent,
		Evidence:      schema.EvidenceNativeTyped,
	}}
	blobs[inherited] = []byte("inherited body")

	if err := activateTestGeneration(t, s, candidate, blobs); err != nil {
		t.Fatalf("activate round-trip generation: %v", err)
	}

	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)

	assertInstalledEntries(t, conn, sid, genID, candidate)
	assertInstalledSections(t, conn, sid, genID, candidate)
	assertInstalledContent(t, s, conn, sid, genID)
	assertInstalledAliases(t, conn, sid, genID, candidate)
	assertInstalledSegments(t, conn, sid, genID, candidate)
	assertInstalledRelationshipEvidence(t, conn, sid, genID, candidate)
}

func assertInstalledEntries(t *testing.T, conn *sqlite.Conn, sid schema.SessionID, genID string, candidate indexformat.V2) {
	t.Helper()
	type row struct {
		partition int
		index     int
		ref       *string
		entryJSON string
	}
	var rows []row
	if err := sqlitex.ExecuteTransient(conn, `SELECT partition_id, entry_index, source_entry_ref, entry_json FROM session_projection_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(sid), genID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			r := row{partition: stmt.ColumnInt(0), index: stmt.ColumnInt(1), entryJSON: stmt.ColumnText(3)}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				ref := stmt.ColumnText(2)
				r.ref = &ref
			}
			rows = append(rows, r)
			return nil
		},
	}); err != nil {
		t.Fatalf("read projection entries: %v", err)
	}
	type expected struct {
		partition int
		entry     schema.SessionEntry
	}
	var want []expected
	for _, entry := range candidate.Generation.Main.Entries {
		want = append(want, expected{partition: 0, entry: entry})
	}
	for _, section := range candidate.Generation.Earlier {
		for _, entry := range section.Content.Entries {
			want = append(want, expected{partition: 1, entry: entry})
		}
	}
	if len(rows) != len(want) {
		t.Fatalf("installed entry rows = %d, want %d", len(rows), len(want))
	}
	for i := range want {
		encoded, err := json.Marshal(want[i].entry)
		if err != nil {
			t.Fatal(err)
		}
		if rows[i].partition != want[i].partition || rows[i].index != want[i].entry.EntryIndex {
			t.Fatalf("entry row %d = (partition %d, index %d), want (partition %d, index %d)", i, rows[i].partition, rows[i].index, want[i].partition, want[i].entry.EntryIndex)
		}
		if rows[i].entryJSON != string(encoded) {
			t.Fatalf("entry row %d json = %q, want %q", i, rows[i].entryJSON, string(encoded))
		}
		wantRef := want[i].entry.SourceEntryRef
		if wantRef == "" {
			if rows[i].ref != nil {
				t.Fatalf("entry row %d ref = %q, want NULL", i, *rows[i].ref)
			}
			continue
		}
		if rows[i].ref == nil || *rows[i].ref != string(wantRef) {
			t.Fatalf("entry row %d ref = %v, want %q", i, rows[i].ref, wantRef)
		}
	}
}

func assertInstalledSections(t *testing.T, conn *sqlite.Conn, sid schema.SessionID, genID string, candidate indexformat.V2) {
	t.Helper()
	got := map[int]string{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT partition_id, COALESCE(earlier_state, '<null>') FROM session_projection_sections WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, &sqlitex.ExecOptions{
		Args: []any{string(sid), genID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			got[stmt.ColumnInt(0)] = stmt.ColumnText(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("read projection sections: %v", err)
	}
	want := map[int]string{0: "<null>", 1: string(candidate.Generation.Earlier[0].State)}
	if len(got) != len(want) {
		t.Fatalf("installed section rows = %v, want %v", got, want)
	}
	for partition, state := range want {
		if got[partition] != state {
			t.Fatalf("section %d earlier_state = %q, want %q", partition, got[partition], state)
		}
	}
}

func assertInstalledContent(t *testing.T, s *Store, conn *sqlite.Conn, sid schema.SessionID, genID string) {
	t.Helper()
	manifest, err := s.generationArtifacts.ReadManifest(context.Background(), sid, genID)
	if err != nil {
		t.Fatalf("read staged manifest: %v", err)
	}
	want := map[schema.SourceEntryRef]indexformat.ContentRecord{}
	for _, record := range manifest.Content {
		want[record.Ref] = record
	}
	got := map[schema.SourceEntryRef]indexformat.ContentRecord{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT source_entry_ref, relative_blob, byte_length, integrity_digest FROM session_projection_content WHERE session_id = ? AND generation_id = ? ORDER BY source_entry_ref`, &sqlitex.ExecOptions{
		Args: []any{string(sid), genID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ref, err := schema.NewSourceEntryRef(stmt.ColumnText(0))
			if err != nil {
				return err
			}
			got[ref] = indexformat.ContentRecord{
				Ref:          ref,
				RelativeBlob: stmt.ColumnText(1),
				ByteLength:   stmt.ColumnInt64(2),
				Digest:       stmt.ColumnText(3),
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read projection content: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("installed content rows = %d, want %d", len(got), len(want))
	}
	for ref, record := range want {
		if got[ref] != record {
			t.Fatalf("content row %q = %+v, want %+v", ref, got[ref], record)
		}
	}
}

func assertInstalledAliases(t *testing.T, conn *sqlite.Conn, sid schema.SessionID, genID string, candidate indexformat.V2) {
	t.Helper()
	got := map[string]schema.SourceEntryRef{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT native_key, source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? ORDER BY native_key`, &sqlitex.ExecOptions{
		Args: []any{string(sid), genID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			got[stmt.ColumnText(0)] = schema.SourceEntryRef(stmt.ColumnText(1))
			return nil
		},
	}); err != nil {
		t.Fatalf("read projection aliases: %v", err)
	}
	want := map[string]schema.SourceEntryRef{}
	for _, alias := range candidate.Generation.Aliases {
		want[alias.NativeKey] = alias.Ref
	}
	if len(got) != len(want) {
		t.Fatalf("installed alias rows = %v, want %v", got, want)
	}
	for key, ref := range want {
		if got[key] != ref {
			t.Fatalf("alias %q = %q, want %q", key, got[key], ref)
		}
	}
}

func assertInstalledSegments(t *testing.T, conn *sqlite.Conn, sid schema.SessionID, genID string, candidate indexformat.V2) {
	t.Helper()
	var ordinals []int
	if err := sqlitex.ExecuteTransient(conn, `SELECT segment_ordinal FROM session_context_segments WHERE session_id = ? AND generation_id = ? ORDER BY segment_ordinal`, &sqlitex.ExecOptions{
		Args: []any{string(sid), genID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ordinals = append(ordinals, stmt.ColumnInt(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("read context segments: %v", err)
	}
	if len(ordinals) != len(candidate.Generation.Segments) {
		t.Fatalf("installed segment rows = %v, want %d", ordinals, len(candidate.Generation.Segments))
	}
	for _, segment := range candidate.Generation.Segments {
		var (
			logical, inclusion, capturedJSON     string
			start, end, decodedStart, decodedEnd *int64
		)
		if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(logical_session_id, ''), start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion, captured_refs_json FROM session_context_segments WHERE session_id = ? AND generation_id = ? AND segment_ordinal = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sid), genID, segment.Ordinal},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				logical = stmt.ColumnText(0)
				start = nullableColumnInt64ForTest(stmt, 1)
				end = nullableColumnInt64ForTest(stmt, 2)
				decodedStart = nullableColumnInt64ForTest(stmt, 3)
				decodedEnd = nullableColumnInt64ForTest(stmt, 4)
				inclusion = stmt.ColumnText(5)
				capturedJSON = stmt.ColumnText(6)
				return nil
			},
		}); err != nil {
			t.Fatalf("read segment %d: %v", segment.Ordinal, err)
		}
		if logical != "" {
			t.Fatalf("segment %d logical_session_id = %q, want NULL", segment.Ordinal, logical)
		}
		if !equalOptionalInt64Ptr(start, segment.Coordinates.Start) ||
			!equalOptionalInt64Ptr(end, segment.Coordinates.EndExclusive) ||
			!equalOptionalInt64Ptr(decodedStart, segment.Coordinates.DecodedByteStart) ||
			!equalOptionalInt64Ptr(decodedEnd, segment.Coordinates.DecodedByteEndExclusive) {
			t.Fatalf("segment %d coordinates = (%v,%v,%v,%v), want (%v,%v,%v,%v)", segment.Ordinal, start, end, decodedStart, decodedEnd, segment.Coordinates.Start, segment.Coordinates.EndExclusive, segment.Coordinates.DecodedByteStart, segment.Coordinates.DecodedByteEndExclusive)
		}
		if inclusion != string(segment.Inclusion) {
			t.Fatalf("segment %d inclusion = %q, want %q", segment.Ordinal, inclusion, segment.Inclusion)
		}
		encodedRefs, err := json.Marshal(segment.CapturedRefs)
		if err != nil {
			t.Fatal(err)
		}
		if capturedJSON != string(encodedRefs) {
			t.Fatalf("segment %d captured_refs_json = %q, want %q", segment.Ordinal, capturedJSON, string(encodedRefs))
		}
	}
}

func assertInstalledRelationshipEvidence(t *testing.T, conn *sqlite.Conn, sid schema.SessionID, genID string, candidate indexformat.V2) {
	t.Helper()
	got := map[string]string{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT kind, target_state, COALESCE(target_local_id, ''), COALESCE(evidence, '') FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ? ORDER BY kind`, &sqlitex.ExecOptions{
		Args: []any{string(sid), genID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			got[stmt.ColumnText(0)] = stmt.ColumnText(1) + "|" + stmt.ColumnText(2) + "|" + stmt.ColumnText(3)
			return nil
		},
	}); err != nil {
		t.Fatalf("read relationship evidence: %v", err)
	}
	if len(got) != len(candidate.Generation.Metadata.Relationships) {
		t.Fatalf("installed evidence rows = %v, want %d", got, len(candidate.Generation.Metadata.Relationships))
	}
	for _, relationship := range candidate.Generation.Metadata.Relationships {
		want := string(relationship.TargetState)
		if relationship.TargetLocalID != nil {
			want += "|" + string(*relationship.TargetLocalID)
		} else {
			want += "|"
		}
		want += "|" + string(relationship.Evidence)
		if got[string(relationship.Kind)] != want {
			t.Fatalf("evidence %s = %q, want %q", relationship.Kind, got[string(relationship.Kind)], want)
		}
	}
}

// nullableColumnInt64ForTest reads a nullable INTEGER column as a *int64.
func nullableColumnInt64ForTest(stmt *sqlite.Stmt, col int) *int64 {
	if stmt.ColumnType(col) == sqlite.TypeNull {
		return nil
	}
	value := stmt.ColumnInt64(col)
	return &value
}

// installTestInt64Ptr returns a pointer to v for the nullable coordinate
// columns in the round-trip assertion.
func installTestInt64Ptr(v int64) *int64 { return &v }
