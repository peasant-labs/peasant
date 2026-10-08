package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// parityEntry builds one entry with every text column populated: the
// worst case for the row mapping, because every field must survive the
// round trip with its presence and value intact. The timestamp is set so
// the digest binding covers it: a scanner that drops timestamp_ms
// recomputes a different digest for this entry.
func parityEntry(sid schema.SessionID, index int) schema.SessionEntry {
	preview := "preview bytes"
	input := `{"tool":"input"}`
	output := "tool output bytes"
	toolKind := schema.ToolCallKind("read")
	stopReason := schema.StopReason("end_turn")
	entry := schema.SessionEntry{
		SessionID: sid, EntryIndex: index, Harness: schema.Harness("claude-code"),
		EntryType: schema.EntryType("tool_result"), Role: schema.Role("tool"),
		ContentPreview: &preview, ToolInput: &input, ToolOutput: &output,
		HasToolUse: true, ToolKind: &toolKind,
		HasThinking: true, IsError: true, StopReason: &stopReason,
		ToolCallID: strPtr("call-1"), EntryID: strPtr("e-1"), ParentEntryID: strPtr("p-1"),
		Depth: 1, ParentIndex: intPtrForTest(0),
		PartType: strPtr("tool_result"), SourceEntryRef: schema.SourceEntryRef("e_parity"),
		Provenance: &schema.ContentProvenance{
			Origin: schema.ContentOrigin("submitted_input"), Actor: schema.ActorOrigin("unknown"),
			Delivery: schema.DeliveryOrigin("session_admission"), Ownership: schema.ContentOwnership("local"),
			Evidence: schema.EvidenceKind("native_typed"), InputModality: schema.InputModality("text"),
			SubmissionRef: schema.SubmissionRef("s_1"),
		},
	}
	tokensIn, tokensOut := 10, 20
	entry.TokensIn, entry.TokensOut = &tokensIn, &tokensOut
	timestampMs := int64(1700000000123)
	entry.TimestampMs = &timestampMs
	entry.ToolNamesCSV = strPtr("read,grep")
	rawLen := 1234
	entry.RawByteLength = &rawLen
	return entry
}

func intPtrForTest(v int) *int { return &v }

// TestSerializeEntryParity proves the writer's byte-parity core: the row
// mapping preserves every field's presence and value, and the canonical
// text is byte-identical to the writer's own marshal of the in-memory
// entry, with the digest as its sha256.
func TestSerializeEntryParity(t *testing.T) {
	sid, err := schema.NewSessionID("c9c9c9c9-c9c9-49c9-89c9-c9c9c9c9c9c9")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("full-row-round-trip", func(t *testing.T) {
		entry := parityEntry(sid, 0)
		record, err := entryRecordFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(serializeEntry(record)); got != mustMarshalEntry(t, entry) {
			t.Fatalf("canonical text differs:\n got %.160q\nwant %.160q", got, mustMarshalEntry(t, entry))
		}
		sum := sha256.Sum256([]byte(mustMarshalEntry(t, entry)))
		if string(bodyDigestForRecord(record)) != hex.EncodeToString(sum[:]) {
			t.Fatal("body digest is not the sha256 of the canonical text")
		}
		if roundTripped := entryFromRow(record); !entriesEqualForParity(t, entry, roundTripped) {
			t.Fatal("row-to-struct reconstruction lost a field")
		}
	})
	t.Run("nil-preservation", func(t *testing.T) {
		entry := schema.SessionEntry{
			SessionID: sid, EntryIndex: 1, Harness: schema.Harness("opencode"),
			EntryType: schema.EntryType("text"), Role: schema.Role("user"),
		}
		record, err := entryRecordFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(serializeEntry(record)); got != mustMarshalEntry(t, entry) {
			t.Fatalf("sparse canonical text differs:\n got %.160q\nwant %.160q", got, mustMarshalEntry(t, entry))
		}
	})
	t.Run("empty-vs-nil", func(t *testing.T) {
		empty := ""
		entry := parityEntry(sid, 2)
		entry.ContentPreview = &empty
		entry.ToolInput = nil
		record, err := entryRecordFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(serializeEntry(record)); got != mustMarshalEntry(t, entry) {
			t.Fatal("empty-vs-nil preview changed the canonical text")
		}
	})
	t.Run("extra-promotion", func(t *testing.T) {
		extra := `{"cache_read":7,"cache_write":8,"model_id":"m","tokens_reasoning":9,"unknown":"kept"}`
		entry := parityEntry(sid, 3)
		entry.Extra = &extra
		record, err := entryRecordFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if record.ModelID == nil || *record.ModelID != "m" {
			t.Fatalf("model_id not promoted: %+v", record)
		}
		if record.TokensReasoning == nil || *record.TokensReasoning != 9 {
			t.Fatalf("tokens_reasoning not promoted: %+v", record)
		}
		if record.CacheRead == nil || *record.CacheRead != 7 || record.CacheWrite == nil || *record.CacheWrite != 8 {
			t.Fatalf("cache counts not promoted: %+v", record)
		}
		t.Logf("extra=%q verbatim=%v", derefOrNil(record.Extra), record.ExtraVerbatim)
		if record.Extra == nil || *record.Extra != `{"unknown":"kept"}` {
			t.Fatalf("unknown remainder = %v, want the canonical remainder", record.Extra)
		}
		if record.ExtraVerbatim != nil {
			t.Fatalf("verbatim set for a canonical rebuild: %v", *record.ExtraVerbatim)
		}
		if got := string(serializeEntry(record)); got != mustMarshalEntry(t, entry) {
			t.Fatal("promoted entry changed the canonical text")
		}
	})
	t.Run("extra-verbatim-residue", func(t *testing.T) {
		// The original orders model_id after the unknown key, so the
		// canonical rebuild (promoted keys first, sorted) cannot be
		// byte-identical: the original stays verbatim.
		extra := `{"unknown":"kept","model_id":"m"}`
		entry := parityEntry(sid, 4)
		entry.Extra = &extra
		record, err := entryRecordFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if record.ExtraVerbatim == nil || *record.ExtraVerbatim != extra {
			t.Fatalf("verbatim = %v, want the original bytes", record.ExtraVerbatim)
		}
		if record.Extra != nil {
			t.Fatalf("canonical side must stay NULL with verbatim set: %v", *record.Extra)
		}
		if got := string(serializeEntry(record)); got != mustMarshalEntry(t, entry) {
			t.Fatal("verbatim entry changed the canonical text")
		}
	})
	t.Run("content-of", func(t *testing.T) {
		result := parityEntry(sid, 5)
		resultRecord, err := entryRecordFromEntry(result)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(contentOf(resultRecord)); got != "tool output bytes" {
			t.Fatalf("tool result content = %q, want the tool output", got)
		}
		call := parityEntry(sid, 6)
		call.ToolOutput = nil
		callRecord, err := entryRecordFromEntry(call)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(contentOf(callRecord)); got != `{"tool":"input"}` {
			t.Fatalf("tool call content = %q, want the tool input", got)
		}
		text := parityEntry(sid, 7)
		text.ToolOutput, text.ToolInput = nil, nil
		textRecord, err := entryRecordFromEntry(text)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(contentOf(textRecord)); got != "preview bytes" {
			t.Fatalf("text content = %q, want the preview", got)
		}
	})
	t.Run("timestamped-scan-digest", func(t *testing.T) {
		// The production row scanner must round-trip the timestamp: a
		// scan that drops timestamp_ms recomputes a different digest,
		// which the sweep and search verification would flag as
		// untrusted. The probe mirrors the session_entry_bodies shape
		// the shared column list selects from.
		entry := parityEntry(sid, 10)
		record, err := entryRecordFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := sqlite.OpenConn(":memory:", sqlite.OpenReadWrite|sqlite.OpenCreate)
		if err != nil {
			t.Fatalf("open probe: %v", err)
		}
		defer conn.Close()
		if err := sqlitex.ExecuteTransient(conn, `CREATE TABLE session_entry_bodies (
			body_id INTEGER PRIMARY KEY, session_id TEXT NOT NULL, body_digest TEXT NOT NULL,
			entry_index INTEGER NOT NULL, harness TEXT NOT NULL, entry_type TEXT NOT NULL,
			role TEXT NOT NULL, timestamp_ms INTEGER, content_preview TEXT,
			tokens_in INTEGER, tokens_out INTEGER, has_tool_use INTEGER NOT NULL,
			tool_kind TEXT, tool_names_csv TEXT, has_thinking INTEGER NOT NULL,
			is_error INTEGER NOT NULL, stop_reason TEXT, raw_byte_length INTEGER,
			tool_call_id TEXT, entry_id TEXT, parent_entry_id TEXT,
			depth INTEGER NOT NULL, parent_index INTEGER, tool_input TEXT,
			tool_output TEXT, model_id TEXT, tokens_reasoning INTEGER,
			cache_read INTEGER, cache_write INTEGER, extra TEXT,
			extra_verbatim TEXT, part_type TEXT, source_entry_ref TEXT,
			prov_origin TEXT, prov_actor TEXT, prov_delivery TEXT,
			prov_ownership TEXT, prov_evidence TEXT, prov_input_modality TEXT,
			prov_submission_ref TEXT, UNIQUE (session_id, body_digest)
		) STRICT`, nil); err != nil {
			t.Fatalf("create probe: %v", err)
		}
		if err := insertStagedBodyOnConn(conn, sid, &record); err != nil {
			t.Fatal(err)
		}
		var scanned EntryRecord
		found := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sid)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				scanned = scanEntryRecord(stmt)
				found = true
				return nil
			},
		}); err != nil {
			t.Fatalf("read probe: %v", err)
		}
		if !found {
			t.Fatal("staged body not found")
		}
		if scanned.TimestampMs == nil || *scanned.TimestampMs != *record.TimestampMs {
			t.Fatalf("scanned timestamp = %v, want %d", scanned.TimestampMs, *record.TimestampMs)
		}
		if got := string(bodyDigestForRecord(scanned)); got != string(bodyDigestForRecord(record)) {
			t.Fatal("scanned row recomputes a different digest; the scanner lost a digest-covered field")
		}
		if got := string(bodyDigestForRecord(scanned)); got != scanned.BodyDigest {
			t.Fatal("scanned row digest differs from the stored body_digest")
		}
	})
	t.Run("unicode-boundary", func(t *testing.T) {
		entry := parityEntry(sid, 8)
		preview := "café \U0001F600 multi-byte ✓"
		entry.ContentPreview = &preview
		record, err := entryRecordFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(serializeEntry(record)); got != mustMarshalEntry(t, entry) {
			t.Fatal("unicode preview changed the canonical text")
		}
	})
	t.Run("html-escapes", func(t *testing.T) {
		entry := parityEntry(sid, 9)
		preview := `{"h":"a<b>&"q""}`
		entry.ContentPreview = &preview
		record, err := entryRecordFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(serializeEntry(record)); got != mustMarshalEntry(t, entry) {
			t.Fatal("escapable bytes changed the canonical text")
		}
	})
}

func derefOrNil(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func mustMarshalEntry(t *testing.T, entry schema.SessionEntry) string {
	t.Helper()
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// entriesEqualForParity compares two entries through their canonical texts:
// byte-identical text is the parity contract, and it is stronger than a
// field-by-field walk (it also pins key order and escaping).
func entriesEqualForParity(t *testing.T, want, got schema.SessionEntry) bool {
	t.Helper()
	return mustMarshalEntry(t, want) == mustMarshalEntry(t, got)
}

// TestComputeActivationBindingDeterminism proves the binding is a pure
// function of its inputs: same catalog values and digests bind equally,
// and any read-visible change binds differently.
func TestComputeActivationBindingDeterminism(t *testing.T) {
	sid, err := schema.NewSessionID("d0d0d0d0-d0d0-40d0-80d0-d0d0d0d0d0d0")
	if err != nil {
		t.Fatal(err)
	}
	v2, blobs := buildTestGeneration(t, sid, "gen_bind", "bind text", "bind input", "bind output")
	prepared, err := prepareHarmonizedCandidate(sid, v2.Generation, blobs)
	if err != nil {
		t.Fatal(err)
	}
	again, err := prepareHarmonizedCandidate(sid, v2.Generation, blobs)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.binding != again.binding {
		t.Fatal("identical candidates bound differently; the binding must be deterministic")
	}
	if len(prepared.binding) != 64 {
		t.Fatalf("binding = %q, want 64 hex bytes", prepared.binding)
	}
	altered, _ := buildTestGeneration(t, sid, "gen_bind_other", "bind text altered", "bind input", "bind output")
	preparedAltered, err := prepareHarmonizedCandidate(sid, altered.Generation, blobs)
	if err != nil {
		t.Fatal(err)
	}
	if preparedAltered.binding == prepared.binding {
		t.Fatal("changed content bound identically; the binding must cover every body digest")
	}
}
