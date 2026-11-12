package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const (
	v71OwnerID  = "11111111-1111-4111-8111-111111111111"
	v71LayoutID = "22222222-2222-4222-8222-222222222222"
	v71ChildID  = "agent-abc123"
)

// TestMigrationV71UnparseableSkeletonIsRemined migrates one skeleton the
// backfill cannot parse and drives that row through discovery. The migrated
// row is proof_version 0 with default facts. The discovered parent must be the
// owner the transcript proves, not the layout parent those defaults would leave.
func TestMigrationV71UnparseableSkeletonIsRemined(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	project := filepath.Join(root, "-workspace")
	ownerPath := filepath.Join(project, v71OwnerID+".jsonl")
	layoutPath := filepath.Join(project, v71LayoutID+".jsonl")
	childPath := filepath.Join(project, v71LayoutID, "subagents", v71ChildID+".jsonl")
	writeV71Line(t, ownerPath, `{"sessionId":"`+v71OwnerID+`","uuid":"s1","type":"user","message":{"role":"user","content":"synthetic"}}`)
	writeV71Line(t, layoutPath, `{"sessionId":"`+v71LayoutID+`","uuid":"s1","type":"user","message":{"role":"user","content":"synthetic"}}`+"\n"+
		`{"sessionId":"`+v71OwnerID+`","uuid":"command","parentUuid":"s1","type":"system","subtype":"local_command"}`+"\n"+
		`{"sessionId":"`+v71OwnerID+`","uuid":"fork-tail","parentUuid":"command","type":"assistant","message":{"role":"assistant","content":"synthetic"}}`)
	writeV71Line(t, childPath, `{"sessionId":"`+v71OwnerID+`","agentId":"abc123","isSidechain":true,"uuid":"agent-record","type":"assistant","message":{"role":"assistant","content":"synthetic"}}`)

	dbPath := filepath.Join(t.TempDir(), "unparseable.db")
	conn := openV70(t, dbPath)
	err := sqlitex.ExecuteTransient(conn, `INSERT INTO claude_transcript_evidence
		(source_path, scope, mod_time_unix_nano, size_bytes, has_conversation, spawns_json, title, branch, cwd, origin,
		 proof_version, native_agent_id, native_agent_valid, content_digest, identity_records_json)
		VALUES (?, 'root', 1, 1, 1, '[]', '', '', '', 'unknown', 4, '', 0, '', ?)`, &sqlitex.ExecOptions{
		Args: []any{layoutPath, `[{"uuid":"missing-session"}]`},
	})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	database := openMigrated(t, dbPath)
	conn, err = database.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var proof int
	var sessions, chain string
	func() {
		defer database.pool.Put(conn)
		proof, sessions, chain = readV71Facts(t, conn, layoutPath)
	}()
	if proof != 0 || sessions != "[]" || chain != "[]" {
		t.Fatalf("unparseable skeleton migrated to proof=%d sessions=%s chain=%s, want proof 0 and default facts", proof, sessions, chain)
	}

	adapter := ingest.NewClaudeAdapter(&ingest.OSFileSystem{}, nil, salt.Salt{})
	ingest.AttachClaudeEvidenceCache(adapter, database)
	sessionsFound, err := adapter.Discover(ctx, ingest.SourceConfig{Paths: []ingest.ResolvedPath{ingest.ResolvedPath(root)}, Enabled: true})
	if err != nil {
		t.Fatalf("discovery after migrating an unparseable skeleton: %v", err)
	}
	got := ""
	for _, session := range sessionsFound {
		if session.SessionID.String() != v71ChildID || session.ParentUUID == nil {
			continue
		}
		got = session.ParentUUID.String()
	}
	if got != v71OwnerID {
		t.Fatalf("discovered parent = %q, want %q; proof_version 0 default facts would leave the layout parent %s", got, v71OwnerID, v71LayoutID)
	}
	conn, err = database.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.pool.Put(conn)
	proof, _, chain = readV71Facts(t, conn, layoutPath)
	if proof == 0 || chain == "[]" {
		t.Fatalf("after discovery proof=%d chain=%s; the defaults were kept instead of a re-mine", proof, chain)
	}
}

// TestMigrationV71BackfillsOldWriterSkeleton compares the migration to a fresh
// mine of one transcript with ten or more records. The skeleton is the JSON
// the previous writer stored, including empty coordinates, not a hand-built
// omitempty encoding of a short list.
func TestMigrationV71BackfillsOldWriterSkeleton(t *testing.T) {
	ctx := t.Context()
	records := v71OldWriterRecords()
	if len(records) < 10 {
		t.Fatalf("old-writer transcript has %d records, want at least 10", len(records))
	}
	raw, err := json.Marshal(v71OldWriterRows(records))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "-workspace", v71LayoutID+".jsonl")
	writeV71Line(t, path, v71Transcript(t, records))

	dbPath := filepath.Join(t.TempDir(), "old-writer.db")
	conn := openV70(t, dbPath)
	err = sqlitex.ExecuteTransient(conn, `INSERT INTO claude_transcript_evidence
		(source_path, scope, mod_time_unix_nano, size_bytes, has_conversation, spawns_json, title, branch, cwd, origin,
		 proof_version, native_agent_id, native_agent_valid, content_digest, identity_records_json)
		VALUES (?, 'root', 1, 1, 1, '[]', '', '', '', 'unknown', 4, '', 0, '', ?)`, &sqlitex.ExecOptions{
		Args: []any{path, string(raw)},
	})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	database := openMigrated(t, dbPath)
	want := ingest.ClaudeTranscriptEvidence{Scope: ingest.ClaudeEvidenceScopeRoot, Records: records}
	ingest.SealClaudeIdentity(&want)
	conn, err = database.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer database.pool.Put(conn)
		assertV71FactsMatch(t, conn, path, want, "backfill")
	}()

	adapter := ingest.NewClaudeAdapter(&ingest.OSFileSystem{}, nil, salt.Salt{})
	ingest.AttachClaudeEvidenceCache(adapter, database)
	if _, err := adapter.Discover(ctx, ingest.SourceConfig{Paths: []ingest.ResolvedPath{ingest.ResolvedPath(root)}, Enabled: true}); err != nil {
		t.Fatalf("fresh mine of the old-writer transcript: %v", err)
	}
	conn, err = database.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.pool.Put(conn)
	assertV71FactsMatch(t, conn, path, want, "fresh mine")
	proof, _, _ := readV71Facts(t, conn, path)
	if proof == 4 {
		t.Fatal("fresh mine left the pre-migration proof version; the backfill was not compared to a re-mine")
	}
}

func v71OldWriterRecords() []ingest.ClaudeIdentityRecord {
	owner := v71OwnerID
	copy := v71LayoutID
	sidechainFalse := false
	return []ingest.ClaudeIdentityRecord{
		{SessionID: copy, UUID: "u0", Type: "user"},
		{SessionID: copy, UUID: "u1", ParentUUID: "u0", Type: "assistant"},
		{SessionID: copy, UUID: "u2", ParentUUID: "u1", Type: "user"},
		{SessionID: copy, UUID: "u3", ParentUUID: "u2", Type: "assistant"},
		{SessionID: copy, UUID: "u4", ParentUUID: "u3", Type: "user"},
		{SessionID: owner, UUID: "shared", ParentUUID: "u4", Type: "user"},
		{SessionID: owner, UUID: "shared", ParentUUID: "u4", Type: "assistant"},
		{SessionID: owner, UUID: "shared", ParentUUID: "u4", Type: "user"},
		{SessionID: copy, UUID: "boundary", Type: "system", Subtype: "compact_boundary"},
		{SessionID: copy, ParentUUID: "boundary", Type: "user"},
		{SessionID: copy, UUID: "u6", ParentUUID: "boundary", Type: "user", Sidechain: &sidechainFalse},
		{SessionID: owner, UUID: "u7", ParentUUID: "shared", Type: "assistant"},
	}
}

func v71OldWriterRows(records []ingest.ClaudeIdentityRecord) []claudeIdentityRecordRow {
	rows := make([]claudeIdentityRecordRow, len(records))
	for i, record := range records {
		rows[i] = claudeIdentityRecordRow{
			SessionID: record.SessionID, UUID: record.UUID, ParentUUID: record.ParentUUID,
			Type: record.Type, Subtype: record.Subtype, SpawnAgentID: record.SpawnAgentID,
			AgentID: record.AgentID, Sidechain: record.Sidechain,
		}
	}
	return rows
}

func v71Transcript(t *testing.T, records []ingest.ClaudeIdentityRecord) string {
	t.Helper()
	var body string
	for i, record := range records {
		line := map[string]any{
			"sessionId":  record.SessionID,
			"uuid":       record.UUID,
			"parentUuid": record.ParentUUID,
			"type":       record.Type,
			"subtype":    record.Subtype,
			"message":    map[string]any{"role": "user", "content": "synthetic"},
		}
		if record.Sidechain != nil {
			line["isSidechain"] = *record.Sidechain
		}
		raw, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("encode transcript record %d: %v", i, err)
		}
		if i > 0 {
			body += "\n"
		}
		body += string(raw)
	}
	return body
}

func writeV71Line(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openV70(t *testing.T, dbPath string) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(70)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return conn
}

func openMigrated(t *testing.T, dbPath string) *Store {
	t.Helper()
	database, err := openUpgraded(dbPath, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	return database
}

func readV71Facts(t *testing.T, conn *sqlite.Conn, path string) (proof int, sessions, chain string) {
	t.Helper()
	err := sqlitex.ExecuteTransient(conn, `SELECT proof_version, identity_session_ids_json, identity_chain_json
		FROM claude_transcript_evidence WHERE source_path = ?`, &sqlitex.ExecOptions{
		Args: []any{path},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			proof = stmt.ColumnInt(0)
			sessions = stmt.ColumnText(1)
			chain = stmt.ColumnText(2)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sessions == "" && chain == "" && proof == 0 {
		t.Fatalf("no evidence row for %s", path)
	}
	return proof, sessions, chain
}

func assertV71FactsMatch(t *testing.T, conn *sqlite.Conn, path string, want ingest.ClaudeTranscriptEvidence, label string) {
	t.Helper()
	var sessions, spawns, chain string
	var legacy, contradictory int
	err := sqlitex.ExecuteTransient(conn, `SELECT identity_session_ids_json, identity_spawn_agent_ids_json,
		identity_legacy_agent_proof, identity_contradictory_sidechain, identity_chain_json
		FROM claude_transcript_evidence WHERE source_path = ?`, &sqlitex.ExecOptions{
		Args: []any{path},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sessions = stmt.ColumnText(0)
			spawns = stmt.ColumnText(1)
			legacy = stmt.ColumnInt(2)
			contradictory = stmt.ColumnInt(3)
			chain = stmt.ColumnText(4)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSessions, err := json.Marshal(want.SessionIDs)
	if err != nil {
		t.Fatal(err)
	}
	wantSpawns, err := json.Marshal(want.SpawnAgentIDs)
	if err != nil {
		t.Fatal(err)
	}
	wantChain, err := json.Marshal(want.Chain)
	if err != nil {
		t.Fatal(err)
	}
	sessionsMatch := sessions == string(wantSessions)
	spawnsMatch := spawns == string(wantSpawns)
	chainMatch := chain == string(wantChain)
	legacyMatch := (legacy == 1) == want.LegacyAgentProof
	contradictoryMatch := (contradictory == 1) == want.ContradictorySidechain
	if !sessionsMatch ||
		!spawnsMatch ||
		!chainMatch ||
		!legacyMatch ||
		!contradictoryMatch {
		t.Fatalf("%s facts disagree with the sealed transcript:\nsessions=%s want %s\nspawns=%s want %s\nlegacy=%d want %t\ncontradictory=%d want %t\nchain=%s\nwant %s",
			label, sessions, wantSessions, spawns, wantSpawns, legacy, want.LegacyAgentProof, contradictory, want.ContradictorySidechain, chain, wantChain)
	}
}
