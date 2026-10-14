package main

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The research sampling counts read each session's current representation.
// A converted session reports its mapped entries (so --min-user-turns keeps
// it); every other session reports its mirror rows. This test runs the two
// shipped SQL fragments both sites share against a harmonized session and
// its mirror twin.
func TestResearchSamplingCountsBothRepresentations(t *testing.T) {
	dataHome := t.TempDir()
	db := openSeedStore(t, dataHome, "research-counts")
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer db.Pool().Put(conn)
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed research counts: %v", err)
		}
	}
	exec(`INSERT INTO host_slugs(opaque_id, host_slug) VALUES('research-host','research-host')`)
	exec(`INSERT INTO projects(project_hash, canonical_cwd) VALUES('research-project','/synthetic/research')`)
	harmonizedID := "0c999aaa-36bc-424c-a789-8be54d9702f1"
	mirrorID := "0c999aaa-36bc-424c-a789-8be54d9702f2"
	genID := "gen-research-counts"
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id, index_format_version)
VALUES(?, 'opencode', 'research-model', 'research-host', 'research-project', 1, 2, 3, '/synthetic/research.jsonl', 'jsonl', 11, ?, 2)`, harmonizedID, genID)
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, index_format_version)
VALUES(?, 'opencode', 'research-model', 'research-host', 'research-project', 1, 2, 3, '/synthetic/research.jsonl', 'jsonl', 11, 2)`, mirrorID)
	digest := "4444444444444444444444444444444444444444444444444444444444444444"
	exec(`INSERT INTO session_generations(session_id, generation_id, schema_version, harness, model, version,
ts_start, ts_end, source_format, project_hash, project_name, host_slug,
content_hash, metadata_hash, redaction_applied, completeness,
source_evidence_digest, index_format_version, candidate_digest, installed_at_ms)
VALUES(?, ?, 11, 'opencode', 'research-model', 'v', 1, 2, 'jsonl', 'research-project', 'research', 'research-host',
?, ?, 0, 'complete', ?, 2, ?, 2)`, harmonizedID, genID, digest, digest, digest, digest)
	exec(`INSERT INTO session_projection_sections(session_id, generation_id, partition_id) VALUES(?, ?, 0)`, harmonizedID, genID)
	// Three user turns plus one assistant turn on each side.
	roles := []string{"user", "user", "assistant", "user"}
	for i, role := range roles {
		ref := "research-counts:ref"
		preview := "turn"
		record := store.EntryRecord{
			SessionID:      schema.SessionID(harmonizedID),
			EntryIndex:     i,
			Harness:        schema.HarnessOpenCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.Role(role),
			ContentPreview: &preview,
			Depth:          0,
			SourceEntryRef: schema.SourceEntryRef(ref),
		}
		bodyDigest := store.SerializeEntryDigest(record)
		exec(`INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role,
has_tool_use, has_thinking, is_error, depth, content_preview, source_entry_ref)
VALUES(?, ?, ?, ?, 'opencode', 'text', ?, 0, 0, 0, 0, ?, ?)`,
			int64(store.BodyRowIDBase+600+i), harmonizedID, bodyDigest, i, role, preview, ref)
		exec(`INSERT INTO session_generation_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, body_digest)
VALUES(?, ?, 0, ?, ?, ?)`, harmonizedID, genID, i, ref, bodyDigest)
		exec(`INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview, depth)
VALUES(?, ?, 'opencode', 'text', ?, 'turn', 0)`, mirrorID, i, role)
	}
	// The display fragments.
	for _, sessionID := range []string{harmonizedID, mirrorID} {
		var userTurns, totalTurns int
		if err := sqlitex.ExecuteTransient(conn, `SELECT (`+sqlResearchUserTurns+`), (`+sqlResearchTotalTurns+`) FROM sessions s WHERE s.session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{sessionID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				userTurns = stmt.ColumnInt(0)
				totalTurns = stmt.ColumnInt(1)
				return nil
			},
		}); err != nil {
			t.Fatalf("count fragments for %s: %v", sessionID, err)
		}
		if userTurns != 3 || totalTurns != 4 {
			t.Fatalf("counts for %s = (%d, %d), want (3, 4)", sessionID, userTurns, totalTurns)
		}
	}
	// The filter fragments: min-user-turns 3 keeps both, max-total-turns 3
	// drops both.
	var kept int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sessions s WHERE s.session_id IN (?, ?) AND (`+sqlResearchUserTurns+`) >= ?`, &sqlitex.ExecOptions{
		Args: []any{harmonizedID, mirrorID, 3},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			kept = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("min-user-turns filter: %v", err)
	}
	if kept != 2 {
		t.Fatalf("min-user-turns kept = %d, want 2", kept)
	}
	var within int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sessions s WHERE s.session_id IN (?, ?) AND (`+sqlResearchTotalTurns+`) <= ?`, &sqlitex.ExecOptions{
		Args: []any{harmonizedID, mirrorID, 3},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			within = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("max-total-turns filter: %v", err)
	}
	if within != 0 {
		t.Fatalf("max-total-turns kept = %d, want 0", within)
	}
}
