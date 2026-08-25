package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
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
    origin
FROM claude_transcript_evidence`

	sqlUpsertClaudeEvidence = `INSERT OR REPLACE INTO claude_transcript_evidence (
    source_path, scope, mod_time_unix_nano, size_bytes, has_conversation,
    identity_team, identity_name, spawns_json, title, branch, cwd, origin
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlDeleteClaudeEvidence = `DELETE FROM claude_transcript_evidence WHERE source_path = ?`
)

// claudeSpawnRow is the stored form of one mined spawn record. The stored form
// is explicit so a change to the discovery type cannot silently change what an
// older database means.
type claudeSpawnRow struct {
	Team string `json:"team"`
	Name string `json:"name"`
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
	err = sqlitex.ExecuteTransient(conn, sqlSelectClaudeEvidence, &sqlitex.ExecOptions{
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
// build does not know or whose spawn list does not parse.
func scanClaudeEvidence(stmt *sqlite.Stmt) (ingest.ClaudeTranscriptEvidence, bool) {
	scope := ingest.ClaudeEvidenceScope(stmt.ColumnText(1))
	if !scope.IsValid() {
		return ingest.ClaudeTranscriptEvidence{}, false
	}
	var spawnRows []claudeSpawnRow
	if err := json.Unmarshal([]byte(stmt.ColumnText(7)), &spawnRows); err != nil {
		return ingest.ClaudeTranscriptEvidence{}, false
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
			},
		}); err != nil {
			return fmt.Errorf("store.SaveClaudeEvidence: write evidence for transcript %q: %w", record.SourcePath, err)
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
