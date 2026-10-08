package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// fileBackedPublicationBind carries the publication binding a file-backed
// seed records: the capture revision the V1 mirror batch binds to.
type fileBackedPublicationBind struct {
	revision int64
	capture  *ingest.PublicationCaptureWrite
}

// seedFileBackedExternal installs one candidate into the file-backed storage
// Release N still reads, for external (store_test) tests that cannot reach
// the white-box seeder: the projection generation row with its flattened
// JSON documents, the entry rows, the content records, the aliases, the
// sections, the owned generation directory with its manifest and content
// blobs, the mirror rows through the production V1 batch, and the session
// pointer with its mirrors. It opens a direct connection for the catalog
// SQL (the store pool stays untouched), so it works at any pool size. The
// harmonized catalog rows the writer installs are asserted by the writer's
// own round-trip test; the readers change wires them to these same
// consumers.
func seedFileBackedExternal(t *testing.T, db *store.Store, dbPath, root string, sid schema.SessionID, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte, full bool, bind *fileBackedPublicationBind) {
	t.Helper()
	ctx := context.Background()
	filled := fillFileBackedRecords(t, v2, blobs)
	generation := filled.Generation
	genDir := filepath.Join(root, string(sid), "generations", generation.ID)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, record := range generation.Content {
		payload, ok := blobs[record.Ref]
		if !ok {
			t.Fatalf("no staged bytes for content record %q", record.Ref)
		}
		blobPath := filepath.Join(genDir, record.RelativeBlob)
		if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blobPath, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA busy_timeout = 5000`, nil); err != nil {
		t.Fatal(err)
	}
	exec := func(script string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, script, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed file-backed SQL: %v", err)
		}
	}
	metadataJSON, err := json.Marshal(generation.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	titleRefsJSON, err := json.Marshal(generation.TitleRefs)
	if err != nil {
		t.Fatal(err)
	}
	var inputCount any
	if generation.Metadata.Stats.InputSubmissionCount != nil {
		inputCount = *generation.Metadata.Stats.InputSubmissionCount
	}
	exec(`INSERT INTO session_projection_generations(session_id, generation_id, metadata_json, title_refs_json, input_submission_count, source_evidence_digest, completeness, index_format_version, installed_at_ms, activated_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, 2, 1, 1)`,
		string(sid), generation.ID, string(metadataJSON), string(titleRefsJSON), inputCount, generation.SourceEvidenceDigest, string(generation.Completeness))
	insertEntries := func(partition int, entries []schema.SessionEntry) {
		t.Helper()
		for i := range entries {
			entry := entries[i]
			encoded, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			var ref any
			if entry.SourceEntryRef != "" {
				ref = string(entry.SourceEntryRef)
			}
			exec(`INSERT INTO session_projection_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, entry_json) VALUES (?, ?, ?, ?, ?, ?)`,
				string(sid), generation.ID, partition, entry.EntryIndex, ref, string(encoded))
		}
	}
	insertEntries(0, generation.Main.Entries)
	exec(`INSERT INTO session_projection_sections(session_id, generation_id, partition_id, earlier_state) VALUES (?, ?, 0, NULL)`, string(sid), generation.ID)
	for i := range generation.Earlier {
		section := generation.Earlier[i]
		exec(`INSERT INTO session_projection_sections(session_id, generation_id, partition_id, earlier_state) VALUES (?, ?, ?, ?)`,
			string(sid), generation.ID, i+1, string(section.State))
		insertEntries(i+1, section.Content.Entries)
	}
	for _, record := range generation.Content {
		exec(`INSERT INTO session_projection_content(session_id, generation_id, source_entry_ref, relative_blob, byte_length, integrity_digest) VALUES (?, ?, ?, ?, ?, ?)`,
			string(sid), generation.ID, string(record.Ref), record.RelativeBlob, record.ByteLength, record.Digest)
	}
	for _, alias := range generation.Aliases {
		exec(`INSERT INTO session_projection_aliases(session_id, generation_id, native_key, source_entry_ref) VALUES (?, ?, ?, ?)`,
			string(sid), generation.ID, alias.NativeKey, string(alias.Ref))
	}
	// The session pointer and its mirrors, mirroring the activation's
	// pointing update: the durable parent only when it names a stored
	// session, the metrics row with the derived title, and the count
	// mirrors from the captured stats. The recorded publication provenance
	// is re-stated after the UPDATE: pointing a session at a generation is
	// not a new capture, so it must not invalidate the recorded agreement
	// the sessions trigger would otherwise clear.
	var provenance string
	_ = sqlitex.ExecuteTransient(conn, `SELECT cwd_provenance_kind FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			provenance = stmt.ColumnText(0)
			return nil
		},
	})
	var rootID, purpose, inputMirror, adapter any
	if generation.Metadata.RootSessionID != nil {
		rootID = string(*generation.Metadata.RootSessionID)
	}
	if generation.Metadata.Purpose != "" {
		purpose = string(generation.Metadata.Purpose)
	}
	if generation.Metadata.Stats.InputSubmissionCount != nil {
		inputMirror = *generation.Metadata.Stats.InputSubmissionCount
	}
	if generation.Metadata.AdapterVersion != nil {
		adapter = *generation.Metadata.AdapterVersion
	}
	parent := fileBackedDurableParent(t, conn, string(sid), generation)
	exec(`UPDATE sessions SET active_generation_id = ?, root_session_id = ?, session_purpose = ?, input_submission_count = ?, adapter_version = ?, start_ms = ?, end_ms = ?, model_harness = ?, parent_id = ? WHERE session_id = ?`,
		generation.ID, rootID, purpose, inputMirror, adapter,
		generation.Metadata.Timestamp.Start, generation.Metadata.Timestamp.End,
		string(generation.Metadata.ModelHarness), parent, string(sid))
	if provenance != "" {
		exec(`UPDATE sessions SET cwd_provenance_kind = ? WHERE session_id = ?`, provenance, string(sid))
	}
	title := fileBackedGenerationTitle(generation)
	var titleValue any
	if title != "" {
		titleValue = title
	}
	exec(`INSERT INTO session_metrics (session_id, turn_count, tool_calls, title) VALUES (?, ?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET turn_count = excluded.turn_count, tool_calls = excluded.tool_calls, title = excluded.title`,
		string(sid), generation.Metadata.Stats.TurnCount, generation.Metadata.Stats.ToolCallCount, titleValue)
	// The direct connection holds no locks (every statement above
	// autocommits), so the mirror batch below takes pool connections
	// freely, even at pool size one.
	// The mirror rows come from the production V1 batch write over the main
	// entries: the same rows, hashes, and (for a full capture) chunks and
	// capture certificate the pre-harmonized activation persisted beside
	// its projection tables.
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
	write := ingest.SessionEntryWrite{
		SessionID:          sid,
		Result:             indexformat.V1{Entries: generation.Main.Entries},
		IndexVersion:       1,
		IndexerVersion:     1,
		IndexedAtMs:        1,
		RequireFullContent: full,
		ContentCapture:     capture,
	}
	if bind != nil {
		write.PublicationCapture = bind.capture
		write.CaptureRevision = bind.revision
	}
	results := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{write})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed file-backed mirror rows: %+v", results)
	}
}

// fillFileBackedRecords completes content records from the staged bytes so
// the seeded candidate validates: the owned relative path, the length, and
// the payload digest each record names.
func fillFileBackedRecords(t *testing.T, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) indexformat.V2 {
	t.Helper()
	filled := append([]indexformat.ContentRecord(nil), v2.Generation.Content...)
	for i := range filled {
		payload, ok := blobs[filled[i].Ref]
		if !ok {
			continue
		}
		sum := sha256.Sum256(payload)
		if filled[i].RelativeBlob == "" {
			filled[i].RelativeBlob = "c_" + hex.EncodeToString(sum[:]) + ".blob"
		}
		if filled[i].ByteLength == 0 {
			filled[i].ByteLength = int64(len(payload))
		}
		if filled[i].Digest == "" {
			filled[i].Digest = hex.EncodeToString(sum[:])
		}
	}
	v2.Generation.Content = filled
	return v2
}

// fileBackedDurableParent resolves the durable started_by target when it
// names a stored session, else NULL so an admitted child survives an
// absent, unselected or cyclic parent.
func fileBackedDurableParent(t *testing.T, conn *sqlite.Conn, sessionID string, generation indexformat.Generation) any {
	t.Helper()
	var target *string
	for i := range generation.Metadata.Relationships {
		relationship := generation.Metadata.Relationships[i]
		if relationship.Kind != schema.SessionRelationshipStartedBy {
			continue
		}
		if relationship.TargetState == schema.RelationshipTargetKnown || relationship.TargetState == schema.RelationshipTargetKnownRetained {
			if relationship.TargetLocalID != nil && *relationship.TargetLocalID != "" {
				value := string(*relationship.TargetLocalID)
				target = &value
			}
		}
	}
	if target == nil {
		return nil
	}
	exists := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sessions WHERE session_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{*target},
		ResultFunc: func(*sqlite.Stmt) error { exists = true; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if !exists {
		return nil
	}
	return *target
}

// fileBackedGenerationTitle derives the display title from the generation's
// title refs: the first ref whose main entry carries usable prose, reduced
// to its first non-empty line. It mirrors the activation's pointing update
// so seeded sessions read the same title.
func fileBackedGenerationTitle(generation indexformat.Generation) string {
	if len(generation.TitleRefs) == 0 {
		return ""
	}
	byRef := make(map[schema.SourceEntryRef]schema.SessionEntry, len(generation.Main.Entries))
	for _, entry := range generation.Main.Entries {
		if entry.SourceEntryRef != "" {
			byRef[entry.SourceEntryRef] = entry
		}
	}
	for _, ref := range generation.TitleRefs {
		entry, ok := byRef[ref]
		if !ok {
			continue
		}
		var text string
		if entry.ContentPreview != nil && *entry.ContentPreview != "" {
			text = *entry.ContentPreview
		} else if entry.ToolInput != nil && *entry.ToolInput != "" {
			text = *entry.ToolInput
		} else if entry.ToolOutput != nil && *entry.ToolOutput != "" {
			text = *entry.ToolOutput
		}
		if title := fileBackedFirstTitleLine(text); title != "" {
			return title
		}
	}
	return ""
}

func fileBackedFirstTitleLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			if runes := []rune(trimmed); len(runes) > 80 {
				return string(runes[:77]) + "..."
			}
			return trimmed
		}
	}
	return ""
}
