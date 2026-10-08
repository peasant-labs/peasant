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
	for i, section := range candidate.Generation.Earlier {
		for _, entry := range section.Content.Entries {
			want = append(want, expected{partition: i + 1, entry: entry})
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
	want := map[int]string{0: "<null>"}
	for i, section := range candidate.Generation.Earlier {
		want[i+1] = string(section.State)
	}
	if len(got) != len(want) {
		t.Fatalf("installed section rows = %v, want %v", got, want)
	}
	for partition, state := range want {
		if got[partition] != state {
			t.Fatalf("section %d earlier_state = %q, want %q", partition, got[partition], state)
		}
	}
	type nativeRow struct {
		ordinal     int
		nativeID    string
		kind        string
		entryRef    string
		sourceType  string
		messageRole *string
		turnIndex   *int
		toolCallID  *string
		customType  *string
		data        string
		dataNull    bool
	}
	readNative := func(partition int) []nativeRow {
		var rows []nativeRow
		if err := sqlitex.ExecuteTransient(conn, `SELECT ordinal, native_id, kind, source_entry_ref, source_type, source_message_role, attachment_turn_index, attachment_tool_call_id, custom_type, data FROM session_section_native_metadata WHERE session_id = ? AND generation_id = ? AND partition_id = ? ORDER BY ordinal`, &sqlitex.ExecOptions{
			Args: []any{string(sid), genID, partition},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				row := nativeRow{
					ordinal:    stmt.ColumnInt(0),
					nativeID:   stmt.ColumnText(1),
					kind:       stmt.ColumnText(2),
					entryRef:   stmt.ColumnText(3),
					sourceType: stmt.ColumnText(4),
					dataNull:   stmt.ColumnType(9) == sqlite.TypeNull,
				}
				if stmt.ColumnType(5) != sqlite.TypeNull {
					role := stmt.ColumnText(5)
					row.messageRole = &role
				}
				if stmt.ColumnType(6) != sqlite.TypeNull {
					turn := stmt.ColumnInt(6)
					row.turnIndex = &turn
				}
				if stmt.ColumnType(7) != sqlite.TypeNull {
					toolCallID := stmt.ColumnText(7)
					row.toolCallID = &toolCallID
				}
				if stmt.ColumnType(8) != sqlite.TypeNull {
					customType := stmt.ColumnText(8)
					row.customType = &customType
				}
				if !row.dataNull {
					row.data = stmt.ColumnText(9)
				}
				rows = append(rows, row)
				return nil
			},
		}); err != nil {
			t.Fatalf("read native metadata for partition %d: %v", partition, err)
		}
		return rows
	}
	checkNative := func(partition int, records []schema.NativeMetadataRecord) {
		t.Helper()
		rows := readNative(partition)
		if len(rows) != len(records) {
			t.Fatalf("partition %d native metadata rows = %d, want %d", partition, len(rows), len(records))
		}
		for i, want := range records {
			got := rows[i]
			if got.ordinal != i || got.nativeID != want.ID || got.kind != string(want.Kind) || got.entryRef != string(want.Source.EntryRef) || got.sourceType != string(want.Source.SourceType) {
				t.Fatalf("partition %d record %d identity = %+v, want id %q kind %q ref %q type %q", partition, i, got, want.ID, want.Kind, want.Source.EntryRef, want.Source.SourceType)
			}
			var wantRole *string
			if want.Source.MessageRole != "" {
				role := string(want.Source.MessageRole)
				wantRole = &role
			}
			if !equalOptionalStringPtr(got.messageRole, wantRole) {
				t.Fatalf("partition %d record %d message role = %v, want %v", partition, i, got.messageRole, wantRole)
			}
			var wantTurn *int
			if want.Attachment != nil && want.Attachment.TurnIndex != nil {
				wantTurn = want.Attachment.TurnIndex
			}
			if !equalOptionalIntPtr(got.turnIndex, wantTurn) {
				t.Fatalf("partition %d record %d turn index = %v, want %v", partition, i, got.turnIndex, wantTurn)
			}
			var wantToolCallID *string
			if want.Attachment != nil && want.Attachment.ToolCallID != "" {
				toolCallID := want.Attachment.ToolCallID
				wantToolCallID = &toolCallID
			}
			if !equalOptionalStringPtr(got.toolCallID, wantToolCallID) {
				t.Fatalf("partition %d record %d tool call id = %v, want %v", partition, i, got.toolCallID, wantToolCallID)
			}
			var wantCustom *string
			if want.CustomType != "" {
				customType := want.CustomType
				wantCustom = &customType
			}
			if !equalOptionalStringPtr(got.customType, wantCustom) {
				t.Fatalf("partition %d record %d custom type = %v, want %v", partition, i, got.customType, wantCustom)
			}
			if got.dataNull || got.data != string(want.Data) {
				t.Fatalf("partition %d record %d data = %q (null=%v), want %q", partition, i, got.data, got.dataNull, string(want.Data))
			}
		}
	}
	checkNative(0, candidate.Generation.Main.NativeMetadata)
	for i, section := range candidate.Generation.Earlier {
		checkNative(i+1, section.Content.NativeMetadata)
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
			logical, inclusion                   string
			start, end, decodedStart, decodedEnd *int64
		)
		if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(logical_session_id, ''), start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion FROM session_context_segments WHERE session_id = ? AND generation_id = ? AND segment_ordinal = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sid), genID, segment.Ordinal},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				logical = stmt.ColumnText(0)
				start = nullableColumnInt64ForTest(stmt, 1)
				end = nullableColumnInt64ForTest(stmt, 2)
				decodedStart = nullableColumnInt64ForTest(stmt, 3)
				decodedEnd = nullableColumnInt64ForTest(stmt, 4)
				inclusion = stmt.ColumnText(5)
				return nil
			},
		}); err != nil {
			t.Fatalf("read segment %d: %v", segment.Ordinal, err)
		}
		var wantLogical string
		if segment.LogicalSessionID != nil {
			wantLogical = string(*segment.LogicalSessionID)
		}
		if logical != wantLogical {
			t.Fatalf("segment %d logical_session_id = %q, want %q", segment.Ordinal, logical, wantLogical)
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
		type storedRef struct {
			ordinal int
			ref     string
		}
		var stored []storedRef
		if err := sqlitex.ExecuteTransient(conn, `SELECT ordinal, source_entry_ref FROM session_context_segment_refs WHERE session_id = ? AND generation_id = ? AND segment_ordinal = ? ORDER BY ordinal`, &sqlitex.ExecOptions{
			Args: []any{string(sid), genID, segment.Ordinal},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				stored = append(stored, storedRef{ordinal: stmt.ColumnInt(0), ref: stmt.ColumnText(1)})
				return nil
			},
		}); err != nil {
			t.Fatalf("read segment %d refs: %v", segment.Ordinal, err)
		}
		if len(stored) != len(segment.CapturedRefs) {
			t.Fatalf("segment %d refs = %d rows, want %d", segment.Ordinal, len(stored), len(segment.CapturedRefs))
		}
		for i, want := range segment.CapturedRefs {
			if stored[i].ordinal != i || stored[i].ref != string(want) {
				t.Fatalf("segment %d ref %d = (%d,%q), want (%d,%q)", segment.Ordinal, i, stored[i].ordinal, stored[i].ref, i, want)
			}
		}
	}
}

func assertInstalledRelationshipEvidence(t *testing.T, conn *sqlite.Conn, sid schema.SessionID, genID string, candidate indexformat.V2) {
	t.Helper()
	type evidenceRow struct {
		targetState string
		targetLocal *string
		evidence    *string
		anchorKind  *string
		anchorRef   *string
		anchorRev   *string
	}
	got := map[string]evidenceRow{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ? ORDER BY kind`, &sqlitex.ExecOptions{
		Args: []any{string(sid), genID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row := evidenceRow{targetState: stmt.ColumnText(1)}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				target := stmt.ColumnText(2)
				row.targetLocal = &target
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				evidence := stmt.ColumnText(3)
				row.evidence = &evidence
			}
			if stmt.ColumnType(4) != sqlite.TypeNull {
				kind := stmt.ColumnText(4)
				row.anchorKind = &kind
			}
			if stmt.ColumnType(5) != sqlite.TypeNull {
				ref := stmt.ColumnText(5)
				row.anchorRef = &ref
			}
			if stmt.ColumnType(6) != sqlite.TypeNull {
				rev := stmt.ColumnText(6)
				row.anchorRev = &rev
			}
			got[stmt.ColumnText(0)] = row
			return nil
		},
	}); err != nil {
		t.Fatalf("read relationship evidence: %v", err)
	}
	if len(got) != len(candidate.Generation.Metadata.Relationships) {
		t.Fatalf("installed evidence rows = %v, want %d", got, len(candidate.Generation.Metadata.Relationships))
	}
	optStr := func(v *string) string {
		if v == nil {
			return "<null>"
		}
		return *v
	}
	for _, relationship := range candidate.Generation.Metadata.Relationships {
		row, ok := got[string(relationship.Kind)]
		if !ok {
			t.Fatalf("evidence %s missing", relationship.Kind)
		}
		if row.targetState != string(relationship.TargetState) {
			t.Fatalf("evidence %s target_state = %q, want %q", relationship.Kind, row.targetState, relationship.TargetState)
		}
		var wantLocal, wantEvidence, wantKind, wantRef, wantRev *string
		if relationship.TargetLocalID != nil {
			local := string(*relationship.TargetLocalID)
			wantLocal = &local
		}
		if relationship.Evidence != "" {
			evidence := string(relationship.Evidence)
			wantEvidence = &evidence
		}
		if relationship.Anchor != nil {
			kind := string(relationship.Anchor.Kind)
			wantKind = &kind
			ref := string(relationship.Anchor.SourceEntryRef)
			wantRef = &ref
			if relationship.Anchor.SourceRevisionRef != "" {
				rev := string(relationship.Anchor.SourceRevisionRef)
				wantRev = &rev
			}
		}
		if optStr(row.targetLocal) != optStr(wantLocal) || optStr(row.evidence) != optStr(wantEvidence) || optStr(row.anchorKind) != optStr(wantKind) || optStr(row.anchorRef) != optStr(wantRef) || optStr(row.anchorRev) != optStr(wantRev) {
			t.Fatalf("evidence %s = (%q,%q,%q,%q,%q), want (%q,%q,%q,%q,%q)", relationship.Kind,
				optStr(row.targetLocal), optStr(row.evidence), optStr(row.anchorKind), optStr(row.anchorRef), optStr(row.anchorRev),
				optStr(wantLocal), optStr(wantEvidence), optStr(wantKind), optStr(wantRef), optStr(wantRev))
		}
	}
}

// nullableColumnInt64ForTest reads a nullable INTEGER column as a *int64.

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
