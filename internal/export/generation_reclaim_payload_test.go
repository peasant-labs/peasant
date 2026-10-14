package export_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// reclaimPayloadSessionID is the sandbox session the payload-identity test
// reclaims around.
const reclaimPayloadSessionID = schema.SessionID("bbbb2222-2222-4222-8222-222222222222")

// openReclaimPayloadStore opens a real generation-capable store with the
// production artifact and lock implementations and returns the store and its
// owned-artifact root.
func openReclaimPayloadStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	return openBarrierGenerationStore(t, make(chan struct{}, 8), make(chan time.Time, 8))
}

// openReclaimPayloadFileBackedStore opens the same generation-capable store
// and additionally returns its database path, so the test can seed the
// file-backed catalog rows the legacy seeding package (owned by the
// migration change) will later provide. The production activation writes
// harmonized rows after Release N, so the file-backed representation the
// retained reclaim assertions cover is seeded here with raw SQL.
func openReclaimPayloadFileBackedStore(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(dir, "generations.db")
	storetest.CopyGoldenTo(t, destPath)
	s, err := store.Open(
		destPath,
		store.WithSkipMigrations(),
		store.WithPoolSize(2),
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, locker),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root, destPath
}

// seedFileBackedPayloadGeneration installs one candidate into the
// file-backed storage Release N still reads: the projection generation row
// with its entry, content, alias, and section rows, plus the owned
// generation directory with its manifest and content blobs. The session
// points at the last generation seeded.
func seedFileBackedPayloadGeneration(t *testing.T, dbPath, root string, sid schema.SessionID, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) {
	t.Helper()
	filled := append([]indexformat.ContentRecord(nil), v2.Generation.Content...)
	for i := range filled {
		payload, ok := blobs[filled[i].Ref]
		if !ok {
			t.Fatalf("no staged bytes for content record %q", filled[i].Ref)
		}
		sum := sha256.Sum256(payload)
		digest := hex.EncodeToString(sum[:])
		if filled[i].RelativeBlob == "" {
			filled[i].RelativeBlob = "c_" + digest + ".blob"
		}
		if filled[i].ByteLength == 0 {
			filled[i].ByteLength = int64(len(payload))
		}
		if filled[i].Digest == "" {
			filled[i].Digest = digest
		}
	}
	generation := v2.Generation
	generation.Content = filled
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open payload database: %v", err)
	}
	defer conn.Close()
	execPayloadSQL(t, conn, `INSERT INTO session_projection_generations(session_id, generation_id, metadata_json, title_refs_json, input_submission_count, source_evidence_digest, completeness, index_format_version, installed_at_ms, activated_at_ms) VALUES (`+
		quotePayloadLiteral(string(sid))+`,`+quotePayloadLiteral(generation.ID)+`,`+
		quotePayloadLiteral(mustMarshalPayloadJSON(t, generation.Metadata))+`,`+
		quotePayloadLiteral(mustMarshalPayloadJSON(t, generation.TitleRefs))+`,1,`+
		quotePayloadLiteral(generation.SourceEvidenceDigest)+`,'complete',2,1,1);`)
	for _, entry := range generation.Main.Entries {
		execPayloadSQL(t, conn, `INSERT INTO session_projection_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, entry_json) VALUES (`+
			quotePayloadLiteral(string(sid))+`,`+quotePayloadLiteral(generation.ID)+`,0,`+
			strconv.Itoa(entry.EntryIndex)+`,`+quotePayloadLiteral(string(entry.SourceEntryRef))+`,`+
			quotePayloadLiteral(mustMarshalPayloadJSON(t, entry))+`);`)
	}
	for _, record := range generation.Content {
		execPayloadSQL(t, conn, `INSERT INTO session_projection_content(session_id, generation_id, source_entry_ref, relative_blob, byte_length, integrity_digest) VALUES (`+
			quotePayloadLiteral(string(sid))+`,`+quotePayloadLiteral(generation.ID)+`,`+
			quotePayloadLiteral(string(record.Ref))+`,`+quotePayloadLiteral(record.RelativeBlob)+`,`+
			strconv.FormatInt(record.ByteLength, 10)+`,`+quotePayloadLiteral(record.Digest)+`);`)
	}
	for _, alias := range generation.Aliases {
		execPayloadSQL(t, conn, `INSERT INTO session_projection_aliases(session_id, generation_id, native_key, source_entry_ref) VALUES (`+
			quotePayloadLiteral(string(sid))+`,`+quotePayloadLiteral(generation.ID)+`,`+
			quotePayloadLiteral(alias.NativeKey)+`,`+quotePayloadLiteral(string(alias.Ref))+`);`)
	}
	execPayloadSQL(t, conn, `INSERT INTO session_projection_sections(session_id, generation_id, partition_id, earlier_state) VALUES (`+
		quotePayloadLiteral(string(sid))+`,`+quotePayloadLiteral(generation.ID)+`,0,NULL);`)
	genDir := filepath.Join(root, string(sid), "generations", generation.ID)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), []byte(mustMarshalPayloadJSON(t, generation)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, record := range generation.Content {
		if err := os.WriteFile(filepath.Join(genDir, record.RelativeBlob), blobs[record.Ref], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	execPayloadSQL(t, conn, `UPDATE sessions SET active_generation_id = `+quotePayloadLiteral(generation.ID)+` WHERE session_id = `+quotePayloadLiteral(string(sid))+`;`)
}

// execPayloadFlag sets the sweep flag for the payload session, mirroring
// the v62 backfill for sessions with a non-active projection row.
func execPayloadFlag(t *testing.T, dbPath string) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open payload database: %v", err)
	}
	defer conn.Close()
	execPayloadSQL(t, conn, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = `+quotePayloadLiteral(string(reclaimPayloadSessionID))+`;`)
}

// quotePayloadLiteral escapes one SQL string literal for the file-backed
// seed.
func quotePayloadLiteral(raw string) string {
	return "'" + strings.ReplaceAll(raw, "'", "''") + "'"
}

// mustMarshalPayloadJSON marshals one seed document for the file-backed
// catalog.
func mustMarshalPayloadJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// execPayloadSQL runs one seed script on the payload test's direct database
// connection.
func execPayloadSQL(t *testing.T, conn *sqlite.Conn, script string) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("exec seed SQL: %v\nscript: %.200s", err, script)
	}
}

// reclaimPayloadGeneration builds a valid managed candidate with literal bodies.
func reclaimPayloadGeneration(t *testing.T, id string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	return buildBarrierGeneration(t, reclaimPayloadSessionID, barrierGenerationSpec{
		ID:            id,
		Text:          generationBlobSpec{Literal: "payload user text"},
		ToolInput:     generationBlobSpec{Literal: "payload tool input"},
		ToolOutput:    generationBlobSpec{Literal: "payload tool output"},
		IncludeResult: true,
	})
}

// TestSupersededGenerationReclaimPayloadsAreByteIdentical proves the three
// consumers that read a session — detail, export and publication packaging —
// produce byte-identical payloads before and after the superseded-generation
// reclaim. All three derive from the durable snapshot of the active
// generation, which the reclaim never touches.
func TestSupersededGenerationReclaimPayloadsAreByteIdentical(t *testing.T) {
	ctx := context.Background()
	db, root, dbPath := openReclaimPayloadFileBackedStore(t)
	storetest.SeedSession(t, db, string(reclaimPayloadSessionID))

	superseded, supersededBlobs := reclaimPayloadGeneration(t, "g_reclaim_payload_old")
	seedFileBackedPayloadGeneration(t, dbPath, root, reclaimPayloadSessionID, superseded, supersededBlobs)
	active, activeBlobs := reclaimPayloadGeneration(t, "g_reclaim_payload_active")
	seedFileBackedPayloadGeneration(t, dbPath, root, reclaimPayloadSessionID, active, activeBlobs)
	// The v62 backfill flags every session with a non-active projection
	// row; the seed mirrors it so the flag-selected reclaim finds the
	// session.
	execPayloadFlag(t, dbPath)

	detailBefore, detailPayloadBefore := reclaimPayloadBytes(t, db)
	exportBefore := reclaimExportBytes(t, db)
	publishBefore := reclaimPublishBytes(t, db, detailPayloadBefore)

	result, err := db.ReclaimSupersededGenerations(ctx, 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if result.Sessions != 1 || result.DirectoriesRemoved != 1 {
		t.Fatalf("reclaim = %d sessions, %d directories, want 1 and 1", result.Sessions, result.DirectoriesRemoved)
	}

	detailAfter, detailPayloadAfter := reclaimPayloadBytes(t, db)
	exportAfter := reclaimExportBytes(t, db)
	publishAfter := reclaimPublishBytes(t, db, detailPayloadAfter)

	if !bytes.Equal(detailBefore, detailAfter) {
		t.Fatal("detail payload changed across the reclaim")
	}
	if !bytes.Equal(exportBefore, exportAfter) {
		t.Fatal("export payload changed across the reclaim")
	}
	if !bytes.Equal(publishBefore, publishAfter) {
		t.Fatal("publication payload changed across the reclaim")
	}
}

// reclaimPayloadBytes builds the shared detail bytes and payload.
func reclaimPayloadBytes(t *testing.T, db *store.Store) ([]byte, *schema.SessionDetailPayload) {
	t.Helper()
	raw, payload, err := transcript.BuildSnapshotDetailBytes(context.Background(), db, db, reclaimPayloadSessionID)
	if err != nil {
		t.Fatalf("build detail bytes: %v", err)
	}
	return raw, payload
}

// reclaimExportBytes builds the export payload and marshals it.
func reclaimExportBytes(t *testing.T, db *store.Store) []byte {
	t.Helper()
	payload, err := export.ExportSnapshotPayload(context.Background(), db, db, reclaimPayloadSessionID)
	if err != nil {
		t.Fatalf("build export payload: %v", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal export payload: %v", err)
	}
	return encoded
}

// reclaimPublishBytes builds the publication transcript content through the
// production publish builder and marshals it. The builder's managed-detail arm
// does not read the capture metadata; a nil metadata is its documented
// no-capture shape, so the assertion isolates the detail payload the reclaim
// must not disturb.
func reclaimPublishBytes(t *testing.T, db *store.Store, detail *schema.SessionDetailPayload) []byte {
	t.Helper()
	content, err := push.BuildPublishTranscriptContent(detail, nil, nil, schema.PushContractVersion("test-contract"), config.PushFieldVisibility{}, sessionorigin.User)
	if err != nil {
		t.Fatalf("build publication payload: %v", err)
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal publication payload: %v", err)
	}
	return encoded
}
