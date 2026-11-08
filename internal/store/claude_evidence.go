package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The local store is also the discovery evidence cache. Claude discovery mines
// the teammate links from the transcripts, and that mining reads whole files.
var _ ingest.ClaudeEvidenceCache = (*Store)(nil)

const (
	sqlSelectClaudeEvidence = `SELECT
    source_path,
    scope,
    mod_time_unix_nano,
    size_bytes,
    has_conversation,
    identity_team,
    identity_name,
    spawns_json,
    title,
    branch,
    cwd,
    origin,
    proof_version,
    native_agent_id,
    native_agent_valid,
    content_digest,
    identity_records_json
FROM claude_transcript_evidence`

	sqlUpsertClaudeEvidence = `INSERT OR REPLACE INTO claude_transcript_evidence (
    source_path, scope, mod_time_unix_nano, size_bytes, has_conversation,
    identity_team, identity_name, spawns_json, title, branch, cwd, origin,
    proof_version, native_agent_id, native_agent_valid, content_digest, identity_records_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlDeleteClaudeEvidence = `DELETE FROM claude_transcript_evidence WHERE source_path = ?`

	sqlDeleteClaudeTeammateSpawns = `DELETE FROM claude_teammate_spawns WHERE spawner_path = ?`
	sqlUpsertClaudeTeammateSpawn  = `INSERT OR IGNORE INTO claude_teammate_spawns (spawner_path, team, name) VALUES (?, ?, ?)`
)

// saveClaudeTeammateSpawns is the package-level seam SaveClaudeEvidence calls
// for the spawns projection: delete one transcript's rows and re-insert the
// ones just mined. Production never reassigns it; an in-package store test
// swaps in a stub that fails after it has seen the evidence row on the
// transaction's connection, which proves the evidence write and the derived
// write commit or roll back together. Swapping it is serialized with
// saveClaudeTeammateSpawnsMu so a parallel reader never observes a torn value.
var saveClaudeTeammateSpawns = writeClaudeTeammateSpawns

var saveClaudeTeammateSpawnsMu sync.RWMutex

// swapSaveClaudeTeammateSpawns installs write as the seam and returns the
// function it replaced. It exists for the in-package rollback test, which
// restores the replaced function when the test finishes.
func swapSaveClaudeTeammateSpawns(write func(*sqlite.Conn, string, []ingest.ClaudeTeammateIdentity) error) func(*sqlite.Conn, string, []ingest.ClaudeTeammateIdentity) error {
	saveClaudeTeammateSpawnsMu.Lock()
	defer saveClaudeTeammateSpawnsMu.Unlock()
	previous := saveClaudeTeammateSpawns
	saveClaudeTeammateSpawns = write
	return previous
}

func runSaveClaudeTeammateSpawns(conn *sqlite.Conn, sourcePath string, spawns []ingest.ClaudeTeammateIdentity) error {
	saveClaudeTeammateSpawnsMu.RLock()
	write := saveClaudeTeammateSpawns
	saveClaudeTeammateSpawnsMu.RUnlock()
	return write(conn, sourcePath, spawns)
}

// writeClaudeTeammateSpawns replaces one transcript's rows in the spawns
// projection inside the caller's transaction. The delete runs for every
// upserted path so a transcript whose scope changed cannot leave rows behind;
// the inserts are limited to a root record's mined spawns, which is the only
// source the projection is derived from. INSERT OR IGNORE is required because
// mining appends spawn records without deduplicating them.
func writeClaudeTeammateSpawns(conn *sqlite.Conn, sourcePath string, spawns []ingest.ClaudeTeammateIdentity) error {
	if err := sqlitex.ExecuteTransient(conn, sqlDeleteClaudeTeammateSpawns, &sqlitex.ExecOptions{
		Args: []any{sourcePath},
	}); err != nil {
		return fmt.Errorf("remove the previous spawn rows for %q: %w", sourcePath, err)
	}
	for _, spawn := range spawns {
		if err := sqlitex.Execute(conn, sqlUpsertClaudeTeammateSpawn, &sqlitex.ExecOptions{
			Args: []any{sourcePath, spawn.Team, spawn.Name},
		}); err != nil {
			return fmt.Errorf("record spawn (%q, %q) for %q: %w", spawn.Team, spawn.Name, sourcePath, err)
		}
	}
	return nil
}

// claudeSpawnRow is the stored form of one mined spawn record. The stored form
// is explicit so a change to the discovery type cannot silently change what an
// older database means.
type claudeSpawnRow struct {
	Team string `json:"team"`
	Name string `json:"name"`
}

// claudeIdentityRecordRow pins the stored layout proof independently of the
// discovery representation. It contains structural identity, never message text.
type claudeIdentityRecordRow struct {
	SessionID    string `json:"sessionId"`
	UUID         string `json:"uuid"`
	ParentUUID   string `json:"parentUuid"`
	Type         string `json:"type"`
	Subtype      string `json:"subtype"`
	SpawnAgentID string `json:"spawnAgentId,omitempty"`
	AgentID      string `json:"agentId,omitempty"`
	Sidechain    *bool  `json:"isSidechain,omitempty"`
}

// LoadClaudeEvidence returns every cached transcript record, keyed by source
// path. A row this build cannot read is skipped, so discovery mines that
// transcript again instead of trusting a value it does not understand.
func (s *Store) LoadClaudeEvidence(ctx context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.LoadClaudeEvidence: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	records := make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence)
	err = sqlitex.Execute(conn, sqlSelectClaudeEvidence, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			record, ok := scanClaudeEvidence(stmt)
			if ok {
				records[record.SourcePath] = record
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store.LoadClaudeEvidence: query: %w", err)
	}
	return records, nil
}

// scanClaudeEvidence reads one row. It reports false for a row whose scope this
// build does not know or whose spawn or identity record list does not parse.
func scanClaudeEvidence(stmt *sqlite.Stmt) (ingest.ClaudeTranscriptEvidence, bool) {
	scope := ingest.ClaudeEvidenceScope(stmt.ColumnText(1))
	if !scope.IsValid() {
		return ingest.ClaudeTranscriptEvidence{}, false
	}
	var spawnRows []claudeSpawnRow
	if err := json.Unmarshal([]byte(stmt.ColumnText(7)), &spawnRows); err != nil {
		return ingest.ClaudeTranscriptEvidence{}, false
	}
	var identityRows []*claudeIdentityRecordRow
	if err := json.Unmarshal([]byte(stmt.ColumnText(16)), &identityRows); err != nil || identityRows == nil {
		return ingest.ClaudeTranscriptEvidence{}, false
	}
	for _, row := range identityRows {
		if row == nil {
			return ingest.ClaudeTranscriptEvidence{}, false
		}
		id, err := ingest.NewSessionID(row.SessionID)
		if err != nil {
			return ingest.ClaudeTranscriptEvidence{}, false
		}
		row.SessionID = id.String()
	}
	// Column 11 (origin) is read as raw text and assigned directly, never
	// through sessionorigin.Parse or Origin.Validate. The empty string is a
	// legal stored value on THIS table only — it means the row predates the
	// origin field — and Fresh (internal/ingest/claude_evidence.go) resolves
	// that marker to "record incomplete" before anything treats the value as
	// a decided Origin. Calling Validate here would reject a legitimate,
	// pre-upgrade cache row instead of letting discovery re-mine it.
	record := ingest.ClaudeTranscriptEvidence{
		SourcePath:            ingest.ResolvedPath(stmt.ColumnText(0)),
		Scope:                 scope,
		ModTimeUnixNano:       stmt.ColumnInt64(2),
		SizeBytes:             stmt.ColumnInt64(3),
		HasConversationRecord: stmt.ColumnInt64(4) == 1,
		Title:                 stmt.ColumnText(8),
		Branch:                stmt.ColumnText(9),
		CWD:                   stmt.ColumnText(10),
		Origin:                sessionorigin.Origin(stmt.ColumnText(11)),
		ProofVersion:          int(stmt.ColumnInt64(12)),
		NativeAgentID:         stmt.ColumnText(13),
		NativeAgentValid:      stmt.ColumnInt64(14) == 1,
		ContentDigest:         stmt.ColumnText(15),
	}
	if stmt.ColumnType(5) != sqlite.TypeNull && stmt.ColumnType(6) != sqlite.TypeNull {
		record.Identity = &ingest.ClaudeTeammateIdentity{
			Team: stmt.ColumnText(5),
			Name: stmt.ColumnText(6),
		}
	}
	for _, row := range spawnRows {
		record.Spawns = append(record.Spawns, ingest.ClaudeTeammateIdentity{Team: row.Team, Name: row.Name})
	}
	for _, row := range identityRows {
		record.Records = append(record.Records, ingest.ClaudeIdentityRecord{
			SessionID: row.SessionID, UUID: row.UUID, ParentUUID: row.ParentUUID,
			Type: row.Type, Subtype: row.Subtype, SpawnAgentID: row.SpawnAgentID, AgentID: row.AgentID, Sidechain: row.Sidechain,
		})
	}
	return record, true
}

// SaveClaudeEvidence writes the records one discovery mined and removes the
// records whose transcripts are gone. Both happen in one transaction, so a
// failure leaves the cache exactly as it was and the next discovery mines the
// transcripts again.
func (s *Store) SaveClaudeEvidence(ctx context.Context, upserts []ingest.ClaudeTranscriptEvidence, deletes []ingest.ResolvedPath) (err error) {
	if len(upserts) == 0 && len(deletes) == 0 {
		return nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store.SaveClaudeEvidence: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	for i := range upserts {
		record := &upserts[i]
		if !record.Scope.IsValid() {
			return fmt.Errorf("store.SaveClaudeEvidence: transcript %q carries unknown scope %q", record.SourcePath, record.Scope)
		}
		spawnRows := make([]claudeSpawnRow, 0, len(record.Spawns))
		for _, spawn := range record.Spawns {
			spawnRows = append(spawnRows, claudeSpawnRow{Team: spawn.Team, Name: spawn.Name})
		}
		spawnsJSON, marshalErr := json.Marshal(spawnRows)
		if marshalErr != nil {
			return fmt.Errorf("store.SaveClaudeEvidence: encode spawns for transcript %q: %w", record.SourcePath, marshalErr)
		}
		identityRows := make([]claudeIdentityRecordRow, 0, len(record.Records))
		for index, identity := range record.Records {
			id, validationErr := ingest.NewSessionID(identity.SessionID)
			if validationErr != nil {
				return fmt.Errorf("store.SaveClaudeEvidence: transcript %q identity record %d has invalid session ID %q; cache transaction rolled back; correct the native recorded ID before retrying: %w", record.SourcePath, index, identity.SessionID, validationErr)
			}
			identityRows = append(identityRows, claudeIdentityRecordRow{
				SessionID: id.String(), UUID: identity.UUID, ParentUUID: identity.ParentUUID,
				Type: identity.Type, Subtype: identity.Subtype, SpawnAgentID: identity.SpawnAgentID, AgentID: identity.AgentID, Sidechain: identity.Sidechain,
			})
		}
		identityJSON, marshalErr := json.Marshal(identityRows)
		if marshalErr != nil {
			return fmt.Errorf("store.SaveClaudeEvidence: encode identity records for transcript %q: %w", record.SourcePath, marshalErr)
		}
		var team, name any
		if record.Identity != nil {
			team = record.Identity.Team
			name = record.Identity.Name
		}
		if err = sqlitex.ExecuteTransient(conn, sqlUpsertClaudeEvidence, &sqlitex.ExecOptions{
			Args: []any{
				record.SourcePath.String(),
				record.Scope.String(),
				record.ModTimeUnixNano,
				record.SizeBytes,
				boolToInt(record.HasConversationRecord),
				team,
				name,
				string(spawnsJSON),
				record.Title,
				record.Branch,
				record.CWD,
				record.Origin.String(),
				record.ProofVersion,
				record.NativeAgentID,
				boolToInt(record.NativeAgentValid),
				record.ContentDigest,
				string(identityJSON),
			},
		}); err != nil {
			return fmt.Errorf("store.SaveClaudeEvidence: write evidence for transcript %q: %w", record.SourcePath, err)
		}
		// Only a root record contributes spawns to the projection. The delete
		// still runs for every upserted path through the seam, so a scope change
		// cannot leave rows that no root record derives.
		projectedSpawns := record.Spawns
		if record.Scope != ingest.ClaudeEvidenceScopeRoot {
			projectedSpawns = nil
		}
		if err = runSaveClaudeTeammateSpawns(conn, record.SourcePath.String(), projectedSpawns); err != nil {
			return fmt.Errorf("store.SaveClaudeEvidence: write teammate spawns for transcript %q: %w", record.SourcePath, err)
		}
	}

	for _, path := range deletes {
		if err = sqlitex.ExecuteTransient(conn, sqlDeleteClaudeEvidence, &sqlitex.ExecOptions{
			Args: []any{path.String()},
		}); err != nil {
			return fmt.Errorf("store.SaveClaudeEvidence: remove evidence for transcript %q: %w", path, err)
		}
	}
	return nil
}
