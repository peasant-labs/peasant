package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

type PublicationRecord struct {
	VillageOrigin string
	OwnerUserID   string
	SessionID     string
	ProjectHash   schema.ProjectHash
	Receipt       schema.AuthoritativePublishResponse
}

type PublicationAttemptDiagnostic struct {
	VillageOrigin string
	OwnerUserID   string
	SessionID     string
	ProjectHash   schema.ProjectHash
	AttemptedAt   int64
	Stage         PublicationAttemptStage
	Message       string
}

type PublicationAttemptStage string

const (
	PublicationAttemptStagePublish     PublicationAttemptStage = "publish"
	PublicationAttemptStageValidate    PublicationAttemptStage = "validate"
	PublicationAttemptStageVisibility  PublicationAttemptStage = "visibility"
	PublicationAttemptStagePersistence PublicationAttemptStage = "persist"
)

var AllPublicationAttemptStages = [...]PublicationAttemptStage{
	PublicationAttemptStagePublish,
	PublicationAttemptStageValidate,
	PublicationAttemptStageVisibility,
	PublicationAttemptStagePersistence,
}

func (s PublicationAttemptStage) IsValid() bool {
	for _, candidate := range AllPublicationAttemptStages {
		if s == candidate {
			return true
		}
	}
	return false
}

func (s PublicationAttemptStage) Validate() error {
	if !s.IsValid() {
		return fmt.Errorf("publication attempt stage %q is not supported; use one of %v so durable recovery diagnostics remain classifiable", s, AllPublicationAttemptStages)
	}
	return nil
}

func (s *Store) SavePublication(ctx context.Context, record PublicationRecord) (err error) {
	if err := record.Receipt.Validate(); err != nil {
		return fmt.Errorf("save publication receipt: authoritative receipt is invalid and local applied state was not changed: %w", err)
	}
	if record.VillageOrigin == "" || record.OwnerUserID == "" || record.SessionID == "" || record.ProjectHash == "" {
		return fmt.Errorf("save publication receipt: publication identity is incomplete; origin, owner, session, and project are required")
	}
	raw, err := json.Marshal(record.Receipt)
	if err != nil {
		return fmt.Errorf("save publication receipt: encode validated authoritative receipt: %w", err)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("save publication receipt: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	defer sqlitex.Transaction(conn)(&err)
	// A project identity repair can move the local key while Village keeps the
	// same transcript identity. Remove only that session's superseded key so the
	// unique remote identity and its replacement receipt move atomically.
	if err = sqlitex.ExecuteTransient(conn, `DELETE FROM session_publications
WHERE village_origin=? AND owner_user_id=? AND remote_transcript_id=? AND session_id=? AND project_hash<>?`, &sqlitex.ExecOptions{Args: []any{record.VillageOrigin, record.OwnerUserID, record.Receipt.TranscriptID.String(), record.SessionID, record.ProjectHash.String()}}); err != nil {
		return fmt.Errorf("save publication receipt: relocate repaired project identity in the receipt transaction: %w", err)
	}
	err = sqlitex.ExecuteTransient(conn, `INSERT INTO session_publications
(village_origin,owner_user_id,session_id,remote_transcript_id,transcript_url,project_hash,operation_fingerprint,content_hash,visibility,published_at,remote_updated_at,receipt_json)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(village_origin,owner_user_id,project_hash,session_id) DO UPDATE SET
remote_transcript_id=excluded.remote_transcript_id,transcript_url=excluded.transcript_url,operation_fingerprint=excluded.operation_fingerprint,content_hash=excluded.content_hash,visibility=excluded.visibility,published_at=excluded.published_at,remote_updated_at=excluded.remote_updated_at,receipt_json=excluded.receipt_json`, &sqlitex.ExecOptions{Args: []any{record.VillageOrigin, record.OwnerUserID, record.SessionID, record.Receipt.TranscriptID.String(), record.Receipt.TranscriptURL, record.ProjectHash.String(), record.Receipt.RequestOperationFingerprint.String(), record.Receipt.ContentHash.String(), record.Receipt.Visibility.String(), record.Receipt.PublishedAt, record.Receipt.UpdatedAt, string(raw)}})
	if err != nil {
		return fmt.Errorf("save publication receipt: atomically replace validated applied state: %w", err)
	}
	var license any
	if record.Receipt.Applied.License != nil {
		license = record.Receipt.Applied.License.String()
	}
	if err = sqlitex.ExecuteTransient(conn, `UPDATE sessions SET pushed_at=?, license_id=? WHERE session_id=? AND project_hash=?`, &sqlitex.ExecOptions{Args: []any{record.Receipt.UpdatedAt, license, record.SessionID, record.ProjectHash.String()}}); err != nil {
		return fmt.Errorf("save publication receipt: update local publication cursor in the same transaction: %w", err)
	}
	if changes := conn.Changes(); changes != 1 {
		return fmt.Errorf("save publication receipt: session %q in project %q was not updated exactly once; the transaction was rolled back so retry can target the exact publication identity", record.SessionID, record.ProjectHash)
	}
	return nil
}

func (s *Store) Publication(ctx context.Context, origin, owner string, projectHash schema.ProjectHash, sessionID string) (*PublicationRecord, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read publication receipt: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	var out *PublicationRecord
	err = sqlitex.ExecuteTransient(conn, `SELECT receipt_json FROM session_publications WHERE village_origin=? AND owner_user_id=? AND project_hash=? AND session_id=?`, &sqlitex.ExecOptions{Args: []any{origin, owner, projectHash.String(), sessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
		r := PublicationRecord{VillageOrigin: origin, OwnerUserID: owner, SessionID: sessionID, ProjectHash: projectHash}
		if err := json.Unmarshal([]byte(stmt.ColumnText(0)), &r.Receipt); err != nil {
			return err
		}
		out = &r
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("read publication receipt: decode persisted authoritative state: %w", err)
	}
	return out, nil
}

// HasPublication reports whether the store holds a publication receipt for the
// session, from any Village origin and owner and under any project hash.
// SavePublication writes a receipt only after Village confirmed the publish, so
// its presence is the local record that the session was published. The read is
// keyed on the session alone: a later harvest can re-attribute the session to a
// new project hash, and the receipt keeps the hash it was published under until
// the next publish.
func (s *Store) HasPublication(ctx context.Context, sessionID string) (bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, fmt.Errorf("check publication receipt: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	found := false
	err = sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_publications WHERE session_id=? LIMIT 1`, &sqlitex.ExecOptions{Args: []any{sessionID}, ResultFunc: func(*sqlite.Stmt) error {
		found = true
		return nil
	}})
	if err != nil {
		return false, fmt.Errorf("check publication receipt: query session %q: %w", sessionID, err)
	}
	return found, nil
}

// PublishedToVillage reports whether the store holds a receipt for the session
// from this Village account, under any project identity. The Village keys a
// transcript by its owner and the local session, so a harvest that
// re-attributes the session to a new project does not make its next upload a
// first publication; a receipt from another Village or account does not count.
func (s *Store) PublishedToVillage(ctx context.Context, origin, owner, sessionID string) (bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, fmt.Errorf("check village publication receipt: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	found := false
	err = sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_publications WHERE village_origin=? AND owner_user_id=? AND session_id=? LIMIT 1`, &sqlitex.ExecOptions{Args: []any{origin, owner, sessionID}, ResultFunc: func(*sqlite.Stmt) error {
		found = true
		return nil
	}})
	if err != nil {
		return false, fmt.Errorf("check village publication receipt: query session %q: %w", sessionID, err)
	}
	return found, nil
}

// LatestSessionPublicationAttempt returns the most recent recorded attempt to
// publish the session to this Village account, under any project identity, or
// nil when none was recorded. Like PublishedToVillage it follows the Village's
// key, so an attempt recorded before a re-attribution still counts.
func (s *Store) LatestSessionPublicationAttempt(ctx context.Context, origin, owner, sessionID string) (*PublicationAttemptDiagnostic, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read publication attempt diagnostic: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	var out *PublicationAttemptDiagnostic
	err = sqlitex.ExecuteTransient(conn, `SELECT attempted_at,stage,message,project_hash FROM publication_attempt_diagnostics WHERE village_origin=? AND owner_user_id=? AND session_id=? ORDER BY attempted_at DESC,id DESC LIMIT 1`, &sqlitex.ExecOptions{Args: []any{origin, owner, sessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
		out = &PublicationAttemptDiagnostic{VillageOrigin: origin, OwnerUserID: owner, SessionID: sessionID, ProjectHash: schema.ProjectHash(stmt.ColumnText(3)), AttemptedAt: stmt.ColumnInt64(0), Stage: PublicationAttemptStage(stmt.ColumnText(1)), Message: stmt.ColumnText(2)}
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("read publication attempt diagnostic: query latest attempt for session %q: %w", sessionID, err)
	}
	return out, nil
}

func (s *Store) RecordPublicationAttempt(ctx context.Context, d PublicationAttemptDiagnostic) error {
	if d.VillageOrigin == "" || d.OwnerUserID == "" || d.SessionID == "" || d.ProjectHash == "" || d.Stage == "" || d.Message == "" {
		return fmt.Errorf("record publication attempt: origin, owner, project, session, stage, and actionable message are required")
	}
	if err := d.Stage.Validate(); err != nil {
		return fmt.Errorf("record publication attempt: reject diagnostic before SQLite persistence: %w", err)
	}
	if d.AttemptedAt == 0 {
		d.AttemptedAt = time.Now().UnixMilli()
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("record publication attempt: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	if err = sqlitex.ExecuteTransient(conn, `INSERT INTO publication_attempt_diagnostics(village_origin,owner_user_id,session_id,project_hash,attempted_at,stage,message)
SELECT ?,?,?,?,?,?,? FROM sessions WHERE session_id=? AND project_hash=?`, &sqlitex.ExecOptions{Args: []any{d.VillageOrigin, d.OwnerUserID, d.SessionID, d.ProjectHash.String(), d.AttemptedAt, string(d.Stage), d.Message, d.SessionID, d.ProjectHash.String()}}); err != nil {
		return fmt.Errorf("record publication attempt diagnostic without changing the last successful receipt: %w", err)
	}
	if changes := conn.Changes(); changes != 1 {
		return fmt.Errorf("record publication attempt: session %q in project %q did not match one local publication identity; no diagnostic was persisted", d.SessionID, d.ProjectHash)
	}
	return nil
}

func (s *Store) LatestPublicationAttempt(ctx context.Context, origin, owner string, projectHash schema.ProjectHash, sessionID string) (*PublicationAttemptDiagnostic, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read publication attempt diagnostic: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	var out *PublicationAttemptDiagnostic
	err = sqlitex.ExecuteTransient(conn, `SELECT attempted_at,stage,message FROM publication_attempt_diagnostics WHERE village_origin=? AND owner_user_id=? AND project_hash=? AND session_id=? ORDER BY attempted_at DESC,id DESC LIMIT 1`, &sqlitex.ExecOptions{Args: []any{origin, owner, projectHash.String(), sessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
		out = &PublicationAttemptDiagnostic{VillageOrigin: origin, OwnerUserID: owner, SessionID: sessionID, ProjectHash: projectHash, AttemptedAt: stmt.ColumnInt64(0), Stage: PublicationAttemptStage(stmt.ColumnText(1)), Message: stmt.ColumnText(2)}
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("read publication attempt diagnostic: query latest targeted failure: %w", err)
	}
	return out, nil
}

// SessionPublications returns, for each named session that holds a receipt, the
// receipt this account holds for it on this Village: the one origin and owner
// name. The read is keyed on the session and not on its project: a later
// harvest can re-attribute a session to another project hash, and the receipt
// keeps the hash it was published under until the next publish. When a session
// holds receipts under more than one project hash, the one Village updated last
// is returned. A session with no receipt for this account is absent from the
// map.
func (s *Store) SessionPublications(ctx context.Context, origin, owner string, sessionIDs []string) (map[string]PublicationRecord, error) {
	out := make(map[string]PublicationRecord, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read session publication receipts: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	for start := 0; start < len(sessionIDs); start += indexFormatReadBatchSize {
		batch := sessionIDs[start:min(start+indexFormatReadBatchSize, len(sessionIDs))]
		args := append([]any{origin, owner}, sessionIDArgs(batch)...)
		query := `SELECT session_id, project_hash, receipt_json FROM session_publications
WHERE village_origin=? AND owner_user_id=? AND session_id IN (` + placeholders(len(batch)) + `)
ORDER BY session_id, remote_updated_at DESC, project_hash`
		err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
			sessionID := stmt.ColumnText(0)
			if _, seen := out[sessionID]; seen {
				return nil
			}
			projectHash, hashErr := schema.NewProjectHash(stmt.ColumnText(1))
			if hashErr != nil {
				return fmt.Errorf("session %q: stored project hash: %w", sessionID, hashErr)
			}
			record := PublicationRecord{VillageOrigin: origin, OwnerUserID: owner, SessionID: sessionID, ProjectHash: projectHash}
			if decodeErr := json.Unmarshal([]byte(stmt.ColumnText(2)), &record.Receipt); decodeErr != nil {
				return fmt.Errorf("session %q: %w", sessionID, decodeErr)
			}
			out[sessionID] = record
			return nil
		}})
		if err != nil {
			return nil, fmt.Errorf("read session publication receipts: decode persisted authoritative state: %w", err)
		}
	}
	return out, nil
}

// SessionPublicationAttempts returns, for each named session, the most recent
// failed publish attempt this account recorded for it on this Village, under any
// project hash. A session with no recorded attempt is absent from the map. The
// attempt is reported whether or not a later publish succeeded; a caller
// compares its time with the receipt's.
func (s *Store) SessionPublicationAttempts(ctx context.Context, origin, owner string, sessionIDs []string) (map[string]PublicationAttemptDiagnostic, error) {
	out := make(map[string]PublicationAttemptDiagnostic, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read session publication attempts: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	for start := 0; start < len(sessionIDs); start += indexFormatReadBatchSize {
		batch := sessionIDs[start:min(start+indexFormatReadBatchSize, len(sessionIDs))]
		args := append([]any{origin, owner}, sessionIDArgs(batch)...)
		query := `SELECT session_id, project_hash, attempted_at, stage, message FROM publication_attempt_diagnostics
WHERE village_origin=? AND owner_user_id=? AND session_id IN (` + placeholders(len(batch)) + `)
ORDER BY session_id, attempted_at DESC, id DESC`
		err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
			sessionID := stmt.ColumnText(0)
			if _, seen := out[sessionID]; seen {
				return nil
			}
			out[sessionID] = PublicationAttemptDiagnostic{
				VillageOrigin: origin,
				OwnerUserID:   owner,
				SessionID:     sessionID,
				ProjectHash:   schema.ProjectHash(stmt.ColumnText(1)),
				AttemptedAt:   stmt.ColumnInt64(2),
				Stage:         PublicationAttemptStage(stmt.ColumnText(3)),
				Message:       stmt.ColumnText(4),
			}
			return nil
		}})
		if err != nil {
			return nil, fmt.Errorf("read session publication attempts: query the latest failure of each session: %w", err)
		}
	}
	return out, nil
}

func sessionIDArgs(sessionIDs []string) []any {
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
	}
	return args
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
