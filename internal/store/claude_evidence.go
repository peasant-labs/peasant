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

// A whole-table load can skip an unreadable row; the caller that counts from
// the store needs to know so it does not add the readable subset again.
var _ ingest.ClaudeEvidenceCompleteness = (*Store)(nil)

const (
	// sqlSelectClaudeEvidenceColumns is the one column list both the whole-table
	// read and the by-path read use, so the two shapes cannot drift.
	sqlSelectClaudeEvidenceColumns = `SELECT
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

	sqlSelectClaudeEvidence = sqlSelectClaudeEvidenceColumns

	// sqlSelectClaudeEvidenceForTemplate is the by-path read over the source_path
	// primary key. Production renders it through sqlSelectClaudeEvidenceFor, and
	// its query-plan guard explains the same rendered statement.
	sqlSelectClaudeEvidenceForTemplate = sqlSelectClaudeEvidenceColumns + ` WHERE source_path IN (%s)`

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
// projection inside the caller's transaction. The evidence upsert is an
// INSERT OR REPLACE, whose conflict delete already cascades the replaced
// row's spawns away; the delete here is the belt-and-braces that keeps the
// projection correct if the upsert ever stops replacing the row, because an
// upsert that updates in place does not fire that cascade. The inserts are
// limited to a root record's mined spawns, which is the only source the
// projection is derived from. INSERT OR IGNORE is required because mining
// appends spawn records without deduplicating them.
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
	records, _, err := s.loadClaudeEvidence(ctx)
	return records, err
}

// LoadClaudeEvidenceCompletely is LoadClaudeEvidence plus whether every stored
// row was readable. A row the whole-table scan cannot parse is skipped by
// LoadClaudeEvidence, and the readable map is then a strict subset of the
// store; a caller that counts identities from the store needs to know, because
// adding its own records for the skipped paths would double-book them.
func (s *Store) LoadClaudeEvidenceCompletely(ctx context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	return s.loadClaudeEvidence(ctx)
}

// loadClaudeEvidence is the one whole-table scan both readers use.
func (s *Store) loadClaudeEvidence(ctx context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("store.LoadClaudeEvidence: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	records := make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence)
	skipped := 0
	err = sqlitex.Execute(conn, sqlSelectClaudeEvidence, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			record, ok := scanClaudeEvidence(stmt)
			if !ok {
				skipped++
				return nil
			}
			records[record.SourcePath] = record
			return nil
		},
	})
	if err != nil {
		return nil, false, fmt.Errorf("store.LoadClaudeEvidence: query: %w", err)
	}
	return records, skipped == 0, nil
}

// sqlSelectClaudeEvidenceFor renders the by-path primary-key read for the given
// path count. Production and its query-plan guard call it, so the guarded
// statement is the statement production runs.
func sqlSelectClaudeEvidenceFor(pathCount int) string {
	return fmt.Sprintf(sqlSelectClaudeEvidenceForTemplate, sqlPlaceholders(pathCount))
}

// LoadClaudeEvidenceFor returns the cached records for the named source paths,
// keyed by path. It is a primary-key read: a whole-table load on a scoped path
// would read every cached transcript to answer a question about a few. A path
// that is not cached is absent from the map. An empty input returns an empty
// map without reading anything.
//
// A row whose spawn list or identity-record list cannot be parsed is still
// returned with its readable scope and identity fields, and with a proof
// version that forces discovery to mine it again. Its spawn side is filled from
// claude_teammate_spawns, and a malformed record list leaves Records nil; no
// caller may treat such a row as fresh. The by-path read does that so a broken
// projection column cannot hide a row's contribution from the count delta.
//
// The named paths are read in batches of indexFormatReadBatchSize, in the order
// the caller supplied them, so one unbounded IN list can neither hit SQLite's
// variable limit nor build a statement whose size grows with the stale-origin
// population. Each batch is one round trip, the same batching the other bulk
// readers use.
func (s *Store) LoadClaudeEvidenceFor(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	records, _, err := s.loadClaudeEvidenceFor(ctx, paths)
	return records, err
}

// LoadClaudeEvidenceForCompletely is LoadClaudeEvidenceFor plus whether every
// requested stored row was readable. A row the by-path scan cannot parse is
// skipped, so the returned map can be a strict subset of the rows that exist
// for the requested paths; a caller that counts identities from the store must
// not add its own records for the skipped paths again.
func (s *Store) LoadClaudeEvidenceForCompletely(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	return s.loadClaudeEvidenceFor(ctx, paths)
}

// loadClaudeEvidenceFor is the one batched by-path scan both readers use.
func (s *Store) loadClaudeEvidenceFor(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	records := make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence)
	if len(paths) == 0 {
		return records, true, nil
	}
	unique := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		value := path.String()
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	if len(unique) == 0 {
		return records, true, nil
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, false, fmt.Errorf(
			"store.LoadClaudeEvidenceFor: take connection: %w; "+
				"what: failed to obtain a database connection for the by-path evidence read; "+
				"why: the pool is exhausted or the context was cancelled; "+
				"where: store.LoadClaudeEvidenceFor in internal/store/claude_evidence.go; "+
				"when: before the first by-path batch ran; "+
				"impact: no cached transcript evidence was returned, so the caller cannot reuse the cache; "+
				"fix: check the pool size and context deadline, then retry the scoped read", err)
	}
	defer s.pool.Put(conn)

	skipped := 0
	unreadableProjections := make([]ingest.ResolvedPath, 0)
	for start := 0; start < len(unique); start += indexFormatReadBatchSize {
		end := min(start+indexFormatReadBatchSize, len(unique))
		batch := unique[start:end]
		args := make([]any, len(batch))
		for index, value := range batch {
			args[index] = value
		}
		err = sqlitex.Execute(conn, sqlSelectClaudeEvidenceFor(len(batch)), &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				record, ok, projectionUnreadable := scanClaudeEvidenceWithProjections(stmt, true)
				if !ok {
					skipped++
					return nil
				}
				records[record.SourcePath] = record
				if projectionUnreadable {
					unreadableProjections = append(unreadableProjections, record.SourcePath)
				}
				return nil
			},
		})
		if err != nil {
			return nil, false, fmt.Errorf(
				"store.LoadClaudeEvidenceFor: query batch %d-%d of %d: %w; "+
					"what: the by-path evidence query failed; "+
					"why: the statement could not run against the evidence table (database access or a malformed statement); "+
					"where: store.LoadClaudeEvidenceFor in internal/store/claude_evidence.go; "+
					"when: reading cached evidence for paths %d-%d of %d; "+
					"impact: the returned map is incomplete, so the caller must not treat it as the full cache; "+
					"fix: restore database access and retry the scoped read",
				start, end, len(unique), err, start, end, len(unique))
		}
	}
	if len(unreadableProjections) > 0 {
		// The unparsed column may be the spawn list, so fill it from the derived
		// projection: tolerating the row with no spawns would under-subtract a
		// stored spawner that this path recorded before the column broke.
		spawns, spawnErr := s.claudeTeammateSpawnsForPathsOnConn(ctx, conn, unreadableProjections)
		if spawnErr != nil {
			return nil, false, spawnErr
		}
		for _, path := range unreadableProjections {
			record, ok := records[path]
			if !ok {
				continue
			}
			record.Spawns = spawns[path]
			records[path] = record
		}
	}
	return records, skipped == 0, nil
}

// scanClaudeEvidence reads one row. It reports false for a row whose scope this
// build does not know or whose spawn or identity record list does not parse.
func scanClaudeEvidence(stmt *sqlite.Stmt) (ingest.ClaudeTranscriptEvidence, bool) {
	record, ok, _ := scanClaudeEvidenceWithProjections(stmt, false)
	return record, ok
}

// scanClaudeEvidenceWithProjections reads one row. With
// tolerateUnreadableProjections, a row whose spawn list or identity-record list
// cannot be parsed is still returned with the fields the identity-count
// decision reads (scope and identity) and a proof version that forces
// discovery to re-mine it. The third result says the row was returned that
// way, so a by-path caller can fill its spawn side from the derived spawn
// projection instead of trusting the unparsed column. The by-path read uses
// that; the whole-table read keeps skipping such a row.
func scanClaudeEvidenceWithProjections(stmt *sqlite.Stmt, tolerateUnreadableProjections bool) (ingest.ClaudeTranscriptEvidence, bool, bool) {
	scope := ingest.ClaudeEvidenceScope(stmt.ColumnText(1))
	if !scope.IsValid() {
		return ingest.ClaudeTranscriptEvidence{}, false, false
	}
	projectionUnreadable := false
	var spawnRows []claudeSpawnRow
	if err := json.Unmarshal([]byte(stmt.ColumnText(7)), &spawnRows); err != nil {
		if !tolerateUnreadableProjections {
			return ingest.ClaudeTranscriptEvidence{}, false, false
		}
		projectionUnreadable = true
		spawnRows = nil
	}
	var identityRows []*claudeIdentityRecordRow
	if err := json.Unmarshal([]byte(stmt.ColumnText(16)), &identityRows); err != nil || identityRows == nil {
		if !tolerateUnreadableProjections {
			return ingest.ClaudeTranscriptEvidence{}, false, false
		}
		projectionUnreadable = true
		identityRows = nil
	}
	if identityRows != nil {
		for _, row := range identityRows {
			if row == nil {
				if !tolerateUnreadableProjections {
					return ingest.ClaudeTranscriptEvidence{}, false, false
				}
				projectionUnreadable = true
				identityRows = nil
				break
			}
			id, err := ingest.NewSessionID(row.SessionID)
			if err != nil {
				if !tolerateUnreadableProjections {
					return ingest.ClaudeTranscriptEvidence{}, false, false
				}
				projectionUnreadable = true
				identityRows = nil
				break
			}
			row.SessionID = id.String()
		}
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
	if projectionUnreadable {
		// A proof version no Fresh check accepts forces discovery to mine this
		// transcript again before any reader trusts its records or spawn list.
		record.ProofVersion = 0
		return record, true, true
	}
	for _, row := range identityRows {
		record.Records = append(record.Records, ingest.ClaudeIdentityRecord{
			SessionID: row.SessionID, UUID: row.UUID, ParentUUID: row.ParentUUID,
			Type: row.Type, Subtype: row.Subtype, SpawnAgentID: row.SpawnAgentID, AgentID: row.AgentID, Sidechain: row.Sidechain,
		})
	}
	return record, true, false
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
		// Only a root record contributes spawns to the projection. The
		// maintenance runs for every upserted path, including a non-root one, so
		// a scope flip clears the rows even if the evidence upsert stops
		// cascading them.
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
