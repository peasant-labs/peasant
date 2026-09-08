package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const fullContentChunkBytes = 64 * 1024

var ContentBackfillShapeMismatch = ingest.ContentBackfillShapeMismatch

func contentSHA(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func hashString(s *string) *string {
	if s == nil {
		return nil
	}
	h := contentSHA(*s)
	return &h
}

// The canonical projection includes every retained semantic field. Full strings
// are hashed individually so encoding the capture never duplicates their bytes.
func fullCaptureHash(entries []schema.SessionEntry) (string, error) {
	h := sha256.New()
	enc := json.NewEncoder(h)
	if err := enc.Encode("peasant.full_content.v1"); err != nil {
		return "", err
	}
	for _, e := range orderedSessionEntries(entries) {
		p := sessionEntriesHashEntryFromEntry(&e)
		p.ContentPreview = hashString(e.ContentPreview)
		p.ToolInput = hashString(e.ToolInput)
		p.ToolOutput = hashString(e.ToolOutput)
		if err := enc.Encode(p); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func contentPreview(s string) string {
	n := min(len(s), defaults.ContentPreviewLimit)
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func writeSessionContentOnConn(ctx context.Context, conn *sqlite.Conn, w ingest.SessionEntryWrite, entries []schema.SessionEntry, stmts *sessionEntryWriteStatements) (sessionEntryWriteOutcome, error) {
	var out sessionEntryWriteOutcome
	if err := ctx.Err(); err != nil {
		return out, err
	}
	mode, err := ingest.NewSessionEntryWriteMode(string(w.Mode))
	if err != nil {
		return out, err
	}
	if !w.RequireFullContent && mode == ingest.SessionEntryWriteContentBackfill {
		return out, fmt.Errorf("store content backfill requires authoritative full entries; prior capture unchanged; enable RequireFullContent after strict parsing")
	}
	for _, e := range entries {
		if string(e.SessionID) != string(w.SessionID) {
			return out, fmt.Errorf("store content write: entry belongs to a different session; batch unchanged; supply entries for the requested session")
		}
	}
	if !w.RequireFullContent {
		out, err = indexSessionEntriesOnConn(conn, w.SessionID, entries, stmts)
		if err != nil {
			return out, err
		}
		if err = sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_full_content WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(w.SessionID)}}); err != nil {
			return out, err
		}
		c := w.ContentCapture
		c.PublicationCaptureRevision = 0
		if c.Status == "" {
			c.Status = ingest.ContentCaptureIncomplete
		}
		if c.Status == ingest.ContentCaptureComplete {
			return out, fmt.Errorf("store content write: preview-only caller cannot certify completeness; use strict full capture before writing")
		}
		if c.SourceAuthority == "" {
			c.SourceAuthority = ingest.ContentSourceNone
		}
		if c.CaptureFormat == "" {
			c.CaptureFormat = ingest.ContentCaptureFormatPreviewOnly
		}
		return out, writeCapture(conn, w.SessionID, c, len(entries), 0, "")
	}
	c := w.ContentCapture
	c.PublicationCaptureRevision = w.CaptureRevision
	if mode == ingest.SessionEntryWriteContentBackfill {
		c.PublicationCaptureRevision, err = contentBackfillPublicationRevision(conn, w.SessionID)
		if err != nil {
			return out, err
		}
		if w.CaptureRevision != 0 && w.CaptureRevision != c.PublicationCaptureRevision {
			return out, publicationRepairError("content backfill has no coherent current metadata/index revision")
		}
	}
	if w.ContentCapture.PublicationCaptureRevision != 0 && w.ContentCapture.PublicationCaptureRevision != c.PublicationCaptureRevision {
		return out, publicationRepairError("full content write revision disagrees with the indexed metadata capture")
	}
	if c.Status == "" {
		c.Status = ingest.ContentCaptureComplete
	}
	if c.SourceAuthority == "" {
		c.SourceAuthority = ingest.ContentSourceNewIngest
	}
	if c.CaptureFormat == "" {
		c.CaptureFormat = ingest.ContentCaptureFormatFull
	}
	if c.Status != ingest.ContentCaptureComplete || c.SourceAuthority == ingest.ContentSourceNone || c.FailureCode != "" || c.FailureMessage != "" {
		return out, fmt.Errorf("store full content write: capture is not complete and attributable; prior data unchanged; resolve strict parser failures before retrying")
	}
	if c.CapturedAtMs == 0 {
		c.CapturedAtMs = time.Now().UnixMilli()
	}
	if _, err := ingest.NewContentSourceAuthority(string(c.SourceAuthority)); err != nil {
		return out, err
	}
	if err := c.TranscriptOrigin.Validate(); err != nil {
		return out, err
	}
	normalized := append([]schema.SessionEntry(nil), entries...)
	for i, e := range normalized {
		if e.ContentPreview != nil {
			if !utf8.ValidString(*e.ContentPreview) {
				return out, fmt.Errorf("store full content write: invalid UTF-8 in entry %d; prior data unchanged; repair source encoding and recapture", e.EntryIndex)
			}
			p := contentPreview(*e.ContentPreview)
			normalized[i].ContentPreview = &p
		}
	}
	fullHash, err := fullCaptureHash(entries)
	if err != nil {
		return out, err
	}
	projectionMatch, err := sessionEntryTablesMatch(conn, string(w.SessionID), normalized)
	if err != nil {
		return out, err
	}
	if mode == ingest.SessionEntryWriteContentBackfill && !projectionMatch {
		return out, ContentBackfillShapeMismatch
	}
	old, found, err := readCapture(conn, w.SessionID)
	if err != nil {
		return out, err
	}
	integrityMatch := false
	if found && old.Status == ingest.ContentCaptureComplete && old.FullCaptureSHA256 == fullHash && projectionMatch {
		integrityMatch = verifyStoredContent(ctx, conn, w.SessionID, old) == nil
	}
	if mode != ingest.SessionEntryWriteContentBackfill {
		// A stored hash is only a cache. If the actual projection differs,
		// invalidate it before the legacy writer considers its fast skip path.
		if !projectionMatch {
			if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_entries_hash=NULL WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(w.SessionID)}}); err != nil {
				return out, err
			}
		}
		out, err = indexSessionEntriesOnConn(conn, w.SessionID, normalized, stmts)
		if err != nil {
			return out, err
		}
	}
	if integrityMatch && out.skipped && old.SourceAuthority == c.SourceAuthority && old.TranscriptOrigin == c.TranscriptOrigin && old.CaptureFormat == c.CaptureFormat {
		// Identical content need not be rewritten, but a new metadata capture
		// must be bound even when the bounded index took its hash-skip path.
		if old.PublicationCaptureRevision == c.PublicationCaptureRevision {
			return out, nil
		}
		return out, writeCapture(conn, w.SessionID, c, old.EntryCount, old.ContentRowCount, fullHash)
	}
	if out.skipped {
		out.stats.SkippedByHash = 0
		out.stats.SkippedByCompare = 0
		out.stats.Rewrites++
	}
	out.skipped = false
	if err = sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_full_content WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(w.SessionID)}}); err != nil {
		return out, err
	}
	rows := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if e.ContentPreview == nil {
			continue
		}
		rows++
		text := *e.ContentPreview
		preview := contentPreview(text)
		chunks := (len(text) + fullContentChunkBytes - 1) / fullContentChunkBytes
		err = sqlitex.ExecuteTransient(conn, `INSERT INTO session_entry_full_content VALUES(?,?,?,?,?,?,?,?,?)`, &sqlitex.ExecOptions{Args: []any{string(w.SessionID), e.EntryIndex, len(text), contentSHA(text), len(preview), contentSHA(preview), boolToInt(text == preview), chunks, c.CapturedAtMs}})
		if err != nil {
			return out, err
		}
		for offset, idx := 0, 0; offset < len(text); idx++ {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			end := min(offset+fullContentChunkBytes, len(text))
			chunk := text[offset:end]
			err = sqlitex.ExecuteTransient(conn, `INSERT INTO session_entry_full_content_chunks VALUES(?,?,?,?,?,?,?)`, &sqlitex.ExecOptions{Args: []any{string(w.SessionID), e.EntryIndex, idx, offset, len(chunk), contentSHA(chunk), []byte(chunk)}})
			if err != nil {
				return out, err
			}
			offset = end
		}
	}
	return out, writeCapture(conn, w.SessionID, c, len(entries), rows, fullHash)
}

func writeCapture(conn *sqlite.Conn, id ingest.SessionID, c ingest.SessionContentCaptureWrite, entries, rows int, hash string) error {
	if err := c.TranscriptOrigin.Validate(); err != nil {
		return err
	}
	if _, err := ingest.NewContentCaptureStatus(string(c.Status)); err != nil {
		return err
	}
	if _, err := ingest.NewContentSourceAuthority(string(c.SourceAuthority)); err != nil {
		return err
	}
	if _, err := ingest.NewContentCaptureFormat(string(c.CaptureFormat)); err != nil {
		return err
	}
	return sqlitex.ExecuteTransient(conn, `INSERT INTO session_content_captures (session_id,status,source_authority,transcript_origin,capture_format,entry_count,content_row_count,full_capture_sha256,captured_at_ms,failure_code,failure_message,publication_capture_revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET status=excluded.status,source_authority=excluded.source_authority,transcript_origin=excluded.transcript_origin,capture_format=excluded.capture_format,entry_count=excluded.entry_count,content_row_count=excluded.content_row_count,full_capture_sha256=excluded.full_capture_sha256,captured_at_ms=excluded.captured_at_ms,failure_code=excluded.failure_code,failure_message=excluded.failure_message,publication_capture_revision=excluded.publication_capture_revision`, &sqlitex.ExecOptions{Args: []any{string(id), string(c.Status), string(c.SourceAuthority), int(c.TranscriptOrigin), string(c.CaptureFormat), entries, rows, nullString(hash), c.CapturedAtMs, nullString(c.FailureCode), nullString(c.FailureMessage), c.PublicationCaptureRevision}})
}

func contentBackfillPublicationRevision(conn *sqlite.Conn, id ingest.SessionID) (revision int64, err error) {
	err = sqlitex.ExecuteTransient(conn, publicationMetadataSelect+` WHERE s.session_id=?`, &sqlitex.ExecOptions{
		Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			bundle, scanErr := scanPublicationMetadataProof(stmt, id)
			if scanErr != nil {
				return scanErr
			}
			if bundle.Readiness == ingest.PublicationReady {
				revision = bundle.CaptureRevision
			}
			return nil
		},
	})
	return revision, err
}
