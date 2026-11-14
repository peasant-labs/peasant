package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/attachments"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// AttachmentDir is the content-addressed file directory beside a database file.
func AttachmentDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), attachments.DirName)
}

// AttachmentStore is the file-and-row store constructed when this database was
// opened. Put, GC, and prune use this pointer so they share one lock.
func (s *Store) AttachmentStore() *attachments.Store {
	return s.attachments
}

// AttachmentStoreWithHooks sets rendezvous hooks on the store Open constructed
// and returns that same store. It does not build a second lock.
func (s *Store) AttachmentStoreWithHooks(hooks attachments.Hooks) *attachments.Store {
	if s.attachments == nil {
		s.attachments = attachments.New(s.attachmentDir, attachmentRows{s}, hooks)
		return s.attachments
	}
	s.attachments.SetHooks(hooks)
	return s.attachments
}

type attachmentRows struct{ store *Store }

func (r attachmentRows) UpsertAttachment(ctx context.Context, row attachments.Row) error {
	conn, err := r.store.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store attachment upsert: take connection: %w", err)
	}
	defer r.store.pool.Put(conn)
	err = sqlitex.ExecuteTransient(conn, `
INSERT INTO session_attachments (
    session_id, source_entry_ref, partition_id, sha256, media_type, byte_size, width, height, omitted_reason
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, source_entry_ref) DO UPDATE SET
    partition_id = excluded.partition_id,
    sha256 = excluded.sha256,
    media_type = excluded.media_type,
    byte_size = excluded.byte_size,
    width = excluded.width,
    height = excluded.height,
    omitted_reason = excluded.omitted_reason`, &sqlitex.ExecOptions{Args: attachmentArgs(row)})
	if err != nil {
		return fmt.Errorf("store attachment upsert for session %q ref %q: %w", row.SessionID, row.SourceEntryRef, err)
	}
	return nil
}

func (r attachmentRows) AttachmentBySource(ctx context.Context, sessionID, sourceRef string) (attachments.Row, bool, error) {
	conn, err := r.store.pool.Take(ctx)
	if err != nil {
		return attachments.Row{}, false, fmt.Errorf("store attachment read: take connection: %w", err)
	}
	defer r.store.pool.Put(conn)
	var row attachments.Row
	found := false
	err = sqlitex.ExecuteTransient(conn, `
SELECT session_id, source_entry_ref, partition_id, COALESCE(sha256, ''), COALESCE(media_type, ''),
       byte_size, width, height, COALESCE(omitted_reason, '')
FROM session_attachments WHERE session_id = ? AND source_entry_ref = ?`, &sqlitex.ExecOptions{
		Args: []any{sessionID, sourceRef},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row = scanAttachment(stmt)
			found = true
			return nil
		},
	})
	if err != nil {
		return attachments.Row{}, false, fmt.Errorf("store attachment read for session %q ref %q: %w", sessionID, sourceRef, err)
	}
	return row, found, nil
}

func (r attachmentRows) AttachmentsBySHA(ctx context.Context, sha string) ([]attachments.Row, error) {
	conn, err := r.store.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store attachment digest read: take connection: %w", err)
	}
	defer r.store.pool.Put(conn)
	var rows []attachments.Row
	err = sqlitex.ExecuteTransient(conn, `
SELECT session_id, source_entry_ref, partition_id, COALESCE(sha256, ''), COALESCE(media_type, ''),
       byte_size, width, height, COALESCE(omitted_reason, '')
FROM session_attachments WHERE sha256 = ?`, &sqlitex.ExecOptions{
		Args: []any{sha},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanAttachment(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store attachment digest read for %s: %w", sha, err)
	}
	return rows, nil
}

func (r attachmentRows) ReferencedSHAs(ctx context.Context) (map[string]struct{}, error) {
	conn, err := r.store.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store attachment digest list: take connection: %w", err)
	}
	defer r.store.pool.Put(conn)
	out := map[string]struct{}{}
	err = sqlitex.ExecuteTransient(conn, `SELECT sha256 FROM session_attachments WHERE sha256 IS NOT NULL AND sha256 <> ''`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			out[stmt.ColumnText(0)] = struct{}{}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store attachment digest list: %w", err)
	}
	return out, nil
}

func (r attachmentRows) ListSessionAttachments(ctx context.Context, sessionID string) ([]attachments.Row, error) {
	conn, err := r.store.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store attachment list: take connection: %w", err)
	}
	defer r.store.pool.Put(conn)
	var rows []attachments.Row
	err = sqlitex.ExecuteTransient(conn, `
SELECT session_id, source_entry_ref, partition_id, COALESCE(sha256, ''), COALESCE(media_type, ''),
       byte_size, width, height, COALESCE(omitted_reason, '')
FROM session_attachments WHERE session_id = ? ORDER BY source_entry_ref`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanAttachment(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store attachment list for session %q: %w", sessionID, err)
	}
	return rows, nil
}

func (r attachmentRows) ReplaceSessionAttachments(ctx context.Context, sessionID string, rows []attachments.Row) error {
	conn, err := r.store.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store attachment replace: take connection: %w", err)
	}
	defer r.store.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	err = sqlitex.ExecuteTransient(conn, `DELETE FROM session_attachments WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{sessionID}})
	if err != nil {
		end(&err)
		return fmt.Errorf("store attachment replace for session %q: delete previous rows: %w", sessionID, err)
	}
	for _, row := range rows {
		row.SessionID = sessionID
		if insertErr := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_attachments (
    session_id, source_entry_ref, partition_id, sha256, media_type, byte_size, width, height, omitted_reason
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: attachmentArgs(row)}); insertErr != nil {
			err = insertErr
			end(&err)
			return fmt.Errorf("store attachment replace for session %q ref %q: %w", sessionID, row.SourceEntryRef, insertErr)
		}
	}
	end(&err)
	return err
}

func (r attachmentRows) DeleteSessionAttachments(ctx context.Context, sessionID string) error {
	conn, err := r.store.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store attachment delete: take connection: %w", err)
	}
	defer r.store.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_attachments WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{sessionID}}); err != nil {
		return fmt.Errorf("store attachment delete for session %q: %w", sessionID, err)
	}
	return nil
}

func attachmentArgs(row attachments.Row) []any {
	var sha any
	if row.SHA256 != "" {
		sha = row.SHA256
	}
	var media any
	if row.MediaType != "" {
		media = row.MediaType
	}
	var reason any
	if row.OmittedReason != "" {
		reason = row.OmittedReason
	}
	return []any{row.SessionID, row.SourceEntryRef, row.PartitionID, sha, media, row.ByteSize, row.Width, row.Height, reason}
}

func scanAttachment(stmt *sqlite.Stmt) attachments.Row {
	return attachments.Row{
		SessionID:      stmt.ColumnText(0),
		SourceEntryRef: stmt.ColumnText(1),
		PartitionID:    stmt.ColumnText(2),
		SHA256:         stmt.ColumnText(3),
		MediaType:      stmt.ColumnText(4),
		ByteSize:       stmt.ColumnInt(5),
		Width:          stmt.ColumnInt(6),
		Height:         stmt.ColumnInt(7),
		OmittedReason:  stmt.ColumnText(8),
	}
}

// PersistImage stores one image through the store Open constructed.
func (s *Store) PersistImage(ctx context.Context, pending attachments.Pending) error {
	if err := s.EnsureAttachmentDir(); err != nil {
		return err
	}
	if _, err := s.AttachmentStore().Put(ctx, pending); err != nil {
		return err
	}
	return nil
}

// ReplaceSessionImages installs the activated generation's image set and drops
// every other attachment row for that session. A rejected attempt never calls
// this, so it cannot overwrite the active generation or leave an orphan row.
// It does not collect: a file the replace left unreferenced stays on disk
// until CollectUnreferencedImages runs at the end of the run.
func (s *Store) ReplaceSessionImages(ctx context.Context, sessionID string, pending []attachments.Pending) error {
	if s.attachments == nil {
		return fmt.Errorf("store attachment replace for session %q: the attachment store was not constructed; open the database before activating a generation", sessionID)
	}
	if err := s.EnsureAttachmentDir(); err != nil {
		return err
	}
	return s.attachments.ReplaceSession(ctx, sessionID, pending)
}

// CollectUnreferencedImages removes the image files no attachment row
// references. The pipeline calls it once per run, from its cleanup stage,
// after the last activation: one listing of referenced digests and one
// listing of the image directory, under the lock shared with Put and
// ReplaceSession. A run that stops before its cleanup stage leaves the
// unreferenced files in place, and the next run's collection removes them.
func (s *Store) CollectUnreferencedImages(ctx context.Context) error {
	// A store with no image directory holds no image files to collect.
	if s.attachments == nil || s.attachmentDir == "" {
		return nil
	}
	if _, err := s.attachments.GC(ctx); err != nil {
		return fmt.Errorf("store image collection failed at store.CollectUnreferencedImages at the end of the run: %w; every referenced image was kept and unreferenced files stay on disk until the next run collects them; rerun harvest", err)
	}
	return nil
}

// ImageFacts is the projection join for one session. The entry extra keeps only
// the source ref; media, size, dimensions, and the omission reason come from
// these rows.
func (s *Store) ImageFacts(ctx context.Context, sessionID ingest.SessionID) (map[string]ingest.ImageFacts, error) {
	if s.attachments == nil {
		return nil, fmt.Errorf("store image facts for session %q: the attachment store was not constructed; open the database before reading a session", sessionID)
	}
	rows, err := s.attachments.SessionRows(ctx, string(sessionID))
	if err != nil {
		return nil, err
	}
	facts := make(map[string]ingest.ImageFacts, len(rows))
	for _, row := range rows {
		facts[row.SourceEntryRef] = ingest.ImageFacts{
			SourceRef:     row.SourceEntryRef,
			SHA256:        row.SHA256,
			MediaType:     row.MediaType,
			ByteSize:      row.ByteSize,
			Width:         row.Width,
			Height:        row.Height,
			OmittedReason: row.OmittedReason,
			PartitionID:   row.PartitionID,
		}
	}
	return facts, nil
}

// EnsureAttachmentDir creates the file directory for this store.
func (s *Store) EnsureAttachmentDir() error {
	if s.attachmentDir == "" {
		return fmt.Errorf("store attachment directory is unset; open the store with a database path before storing images")
	}
	if err := os.MkdirAll(s.attachmentDir, 0o755); err != nil {
		return fmt.Errorf("create attachment directory %s: %w", s.attachmentDir, err)
	}
	return nil
}
