package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.SessionEntryFullReader = (*Store)(nil)
var _ ingest.FullSessionEntryReader = (*Store)(nil)
var _ ingest.ContentBackfillTargetStore = (*Store)(nil)

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ErrContentCaptureIncomplete is the category of every refusal that is caused
// only by a missing or unfinished full capture, and never by damaged data.
// Export and publication wrap it so their callers keep that category through
// errors.Is; previews must not consult it at all, because available stored
// content is shown without a completeness gate.
var ErrContentCaptureIncomplete = errors.New("session capture is incomplete; bounded previews are not authoritative")

func contentIntegrityError() error {
	return fmt.Errorf("store full content read: capture manifest, chunks or semantic entries are inconsistent; complete transcript cannot be trusted; run harvest index --force from retained artifacts to repair")
}

func readCapture(conn *sqlite.Conn, id ingest.SessionID) (c ingest.SessionContentCapture, found bool, err error) {
	c.SessionID = id
	err = sqlitex.ExecuteTransient(conn, `SELECT status,source_authority,transcript_origin,capture_format,entry_count,content_row_count,full_capture_sha256,captured_at_ms,failure_code,failure_message,publication_capture_revision FROM session_content_captures WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(id)}, ResultFunc: func(st *sqlite.Stmt) error {
		found = true
		var e error
		c.Status, e = ingest.NewContentCaptureStatus(st.ColumnText(0))
		if e != nil {
			return e
		}
		c.SourceAuthority, e = ingest.NewContentSourceAuthority(st.ColumnText(1))
		if e != nil {
			return e
		}
		c.TranscriptOrigin, e = ingest.NewTranscriptOrigin(st.ColumnInt(2))
		if e != nil {
			return e
		}
		c.CaptureFormat, e = ingest.NewContentCaptureFormat(st.ColumnText(3))
		if e != nil {
			return e
		}
		c.EntryCount = st.ColumnInt(4)
		c.ContentRowCount = st.ColumnInt(5)
		c.FullCaptureSHA256 = st.ColumnText(6)
		c.CapturedAtMs = st.ColumnInt64(7)
		c.FailureCode = st.ColumnText(8)
		c.FailureMessage = st.ColumnText(9)
		c.PublicationCaptureRevision = st.ColumnInt64(10)
		return nil
	}})
	return
}
func (s *Store) GetSessionContentCapture(ctx context.Context, id ingest.SessionID) (ingest.SessionContentCapture, bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return ingest.SessionContentCapture{}, false, err
	}
	defer s.pool.Put(conn)
	return readCapture(conn, id)
}
func (s *Store) ListContentCaptureIncompleteSessions(ctx context.Context, limit int) ([]ingest.SessionID, error) {
	targets, err := s.ListContentCaptureIncompleteSessionsAfter(ctx, "", limit)
	if err != nil {
		return nil, err
	}
	ids := make([]ingest.SessionID, 0, len(targets))
	for _, target := range targets {
		ids = append(ids, target.SessionID)
	}
	return ids, nil
}

// After is an exclusive keyset cursor. Failed targets cannot starve later ones.
func (s *Store) ListContentCaptureIncompleteSessionsAfter(ctx context.Context, after ingest.SessionID, limit int) ([]ingest.ContentCaptureIncompleteSession, error) {
	if limit <= 0 {
		limit = 100
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	// Ineligible rows are excluded by the QUERY, not after it: a row this build
	// cannot harness-parse must not consume a LIMIT slot, or a page made only of
	// such rows would come back empty and read as the end of the table, ending
	// the recovery walk before the sessions behind it are ever visited.
	placeholders := make([]string, 0, len(ingest.AllHarnesses))
	args := make([]any, 0, len(ingest.AllHarnesses)+2)
	args = append(args, string(after))
	for _, known := range ingest.AllHarnesses {
		placeholders = append(placeholders, "?")
		args = append(args, string(known))
	}
	args = append(args, limit)
	var targets []ingest.ContentCaptureIncompleteSession
	err = sqlitex.ExecuteTransient(conn, `SELECT s.session_id,s.model_harness,s.start_ms FROM sessions s LEFT JOIN session_content_captures c ON c.session_id=s.session_id WHERE s.session_id>? AND s.model_harness IN (`+strings.Join(placeholders, ",")+`) AND (c.status IS NULL OR c.status!='complete') ORDER BY s.session_id LIMIT ?`, &sqlitex.ExecOptions{Args: args, ResultFunc: func(st *sqlite.Stmt) error {
		id, e := ingest.NewSessionID(st.ColumnText(0))
		if e != nil {
			return e
		}
		var harness schema.Harness
		if e := harness.UnmarshalText([]byte(st.ColumnText(1))); e != nil || !harness.IsKnown() {
			// The WHERE clause binds ingest.AllHarnesses, so every selected row
			// is parseable. That list is a SUBSET of what this parser accepts,
			// not the same set, and it must not be widened here: a harness this
			// build recognises but does not bind is one it does not recover
			// content for. Reaching this branch means a BOUND harness cannot be
			// parsed at all, which no stored data can express and which
			// silently skipping would hide.
			return fmt.Errorf("store content recovery targets: session %s passed the bound known-harness filter but its harness %q cannot be parsed; the bound harness list and the harness parser disagree inside this build, so no target list can be trusted; upgrade Peasant to a build whose harness list and parser agree", id, st.ColumnText(1))
		}
		targets = append(targets, ingest.ContentCaptureIncompleteSession{SessionID: id, Harness: harness, StartMs: st.ColumnInt64(2)})
		return nil
	}})
	return targets, err
}

type contentManifest struct {
	length        int
	hash          string
	previewLength int
	previewHash   string
	previewFull   bool
	chunks        int
}

func readManifest(conn *sqlite.Conn, e schema.SessionEntry) (m contentManifest, err error) {
	found := false
	err = sqlitex.ExecuteTransient(conn, `SELECT full_byte_length,full_sha256,preview_byte_length,preview_sha256,preview_is_full,chunk_count FROM session_entry_full_content WHERE session_id=? AND entry_index=?`, &sqlitex.ExecOptions{Args: []any{string(e.SessionID), e.EntryIndex}, ResultFunc: func(st *sqlite.Stmt) error {
		found = true
		m = contentManifest{st.ColumnInt(0), st.ColumnText(1), st.ColumnInt(2), st.ColumnText(3), st.ColumnInt(4) == 1, st.ColumnInt(5)}
		return nil
	}})
	if err != nil {
		return m, err
	}
	if (e.ContentPreview != nil) != found {
		return m, contentIntegrityError()
	}
	if found && (m.previewLength != len(*e.ContentPreview) || m.previewHash != contentSHA(*e.ContentPreview) || m.chunks != (m.length+fullContentChunkBytes-1)/fullContentChunkBytes || m.previewFull != (m.length == m.previewLength)) {
		return m, contentIntegrityError()
	}
	return m, nil
}
func hydrateContent(ctx context.Context, conn *sqlite.Conn, e *schema.SessionEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m, err := readManifest(conn, *e)
	if err != nil {
		return err
	}
	if e.ContentPreview == nil {
		return nil
	}
	var b strings.Builder
	count := 0
	err = sqlitex.ExecuteTransient(conn, `SELECT chunk_index,byte_offset,byte_length,chunk_sha256,data FROM session_entry_full_content_chunks WHERE session_id=? AND entry_index=? ORDER BY chunk_index`, &sqlitex.ExecOptions{Args: []any{string(e.SessionID), e.EntryIndex}, ResultFunc: func(st *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := st.ColumnLen(4)
		if n > fullContentChunkBytes || n != min(fullContentChunkBytes, m.length-b.Len()) || st.ColumnInt(0) != count || st.ColumnInt(1) != b.Len() || st.ColumnInt(2) != n {
			return contentIntegrityError()
		}
		data := make([]byte, n)
		st.ColumnBytes(4, data)
		if contentSHA(string(data)) != st.ColumnText(3) {
			return contentIntegrityError()
		}
		b.Write(data)
		count++
		return nil
	}})
	if err != nil {
		return err
	}
	text := b.String()
	if count != m.chunks || len(text) != m.length || contentSHA(text) != m.hash || !utf8.ValidString(text) || contentPreview(text) != *e.ContentPreview {
		return contentIntegrityError()
	}
	e.ContentPreview = &text
	return nil
}

// Verify the full semantic projection without materializing full prose. Chunk
// verification is performed separately, either for each page or for a skip.
func verifyCaptureProjection(ctx context.Context, conn *sqlite.Conn, id ingest.SessionID, c ingest.SessionContentCapture, verifyChunks bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	if err := enc.Encode("peasant.full_content.v1"); err != nil {
		return err
	}
	entries, rows := 0, 0
	err := sqlitex.ExecuteTransient(conn, sqlListEntries, &sqlitex.ExecOptions{Args: []any{string(id)}, ResultFunc: func(st *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := scanSessionEntry(st)
		m, err := readManifest(conn, e)
		if err != nil {
			return err
		}
		if verifyChunks {
			if err := hydrateContent(ctx, conn, &e); err != nil {
				return err
			}
		}
		p := sessionEntriesHashEntryFromEntry(&e)
		p.ContentPreview = nil
		if e.ContentPreview != nil {
			rows++
			p.ContentPreview = &m.hash
		}
		p.ToolInput = hashString(e.ToolInput)
		p.ToolOutput = hashString(e.ToolOutput)
		entries++
		return enc.Encode(p)
	}})
	if err != nil {
		return err
	}
	if entries != c.EntryCount || rows != c.ContentRowCount || hex.EncodeToString(h.Sum(nil)) != c.FullCaptureSHA256 {
		return contentIntegrityError()
	}
	return nil
}
func verifyStoredContent(ctx context.Context, conn *sqlite.Conn, id ingest.SessionID, c ingest.SessionContentCapture) error {
	return verifyCaptureProjection(ctx, conn, id, c, true)
}

func (s *Store) ReadSessionEntries(ctx context.Context, id ingest.SessionID, opts ingest.SessionEntryReadOptions) (page ingest.SessionEntryReadPage, err error) {
	mode, err := ingest.NewSessionEntryReadMode(string(opts.Mode))
	if err != nil {
		return page, err
	}
	if opts.FromIndex < 0 || opts.Limit < 0 || opts.SoftMaxBytes < 0 {
		return page, fmt.Errorf("store content page: negative cursor or budget; no entries read; use nonnegative options")
	}
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	if opts.SoftMaxBytes == 0 {
		opts.SoftMaxBytes = contentPageBudget(opts.FromIndex, 0)
	}
	page.FromIndex = opts.FromIndex
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return page, err
	}
	defer s.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	defer end(&err)
	if err := s.ValidateIndexFormatsOnConn(conn, []schema.SessionID{id}); err != nil {
		return page, err
	}
	c, found, err := readCapture(conn, id)
	if err != nil {
		return page, err
	}
	page.Capture = c
	// The available mode resolves here, on the same snapshot that read the
	// capture: complete content is served whole, anything else is served as the
	// bounded projection that is actually stored. It never refuses for
	// incompleteness alone.
	if mode == ingest.SessionEntryReadAvailable {
		if found && c.Status == ingest.ContentCaptureComplete {
			mode = ingest.SessionEntryReadFullContent
		} else {
			mode = ingest.SessionEntryReadPreview
		}
		opts.Mode = mode
	}
	if mode == ingest.SessionEntryReadFullContent {
		if !found || c.Status != ingest.ContentCaptureComplete {
			return page, fmt.Errorf("store full content read: %w; run harvest index --force with retained artifacts before viewing, exporting or publishing full content", ErrContentCaptureIncomplete)
		}
		// Validate even a standalone nonzero cursor: callers need not have read
		// an earlier page, and persisted semantic columns may have been damaged.
		if err := verifyCaptureProjection(ctx, conn, id, c, false); err != nil {
			return page, err
		}
	}
	result, err := readContentPageOnConn(ctx, conn, id, opts)
	result.Capture = c
	return result, err
}

// readContentPageOnConn requires prior projection verification for full reads on
// the same transaction. Public callers cannot bypass standalone verification.
func readContentPageOnConn(ctx context.Context, conn *sqlite.Conn, id ingest.SessionID, opts ingest.SessionEntryReadOptions) (page ingest.SessionEntryReadPage, err error) {
	page.FromIndex = opts.FromIndex
	mode := opts.Mode
	query := strings.Replace(sqlListEntries, "WHERE session_id = ?", "WHERE session_id = ? AND entry_index >= ?", 1) + " LIMIT ?"
	st, _, err := conn.PrepareTransient(query)
	if err != nil {
		return page, err
	}
	defer st.Finalize()
	st.BindText(1, string(id))
	st.BindInt64(2, int64(opts.FromIndex))
	st.BindInt64(3, int64(opts.Limit)+1)
	for {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		ok, err := st.Step()
		if err != nil {
			return page, err
		}
		if !ok {
			break
		}
		e := scanSessionEntry(st)
		if len(page.Entries) >= opts.Limit {
			next := e.EntryIndex
			page.NextIndex = &next
			break
		}
		n := entryStringBytes(e)
		if mode == ingest.SessionEntryReadFullContent {
			m, err := readManifest(conn, e)
			if err != nil {
				return page, err
			}
			if e.ContentPreview != nil {
				n += int64(m.length - len(*e.ContentPreview))
			}
		}
		if len(page.Entries) > 0 && page.BytesRead+n > opts.SoftMaxBytes {
			next := e.EntryIndex
			page.NextIndex = &next
			break
		}
		if mode == ingest.SessionEntryReadFullContent {
			if err := hydrateContent(ctx, conn, &e); err != nil {
				return page, err
			}
		}
		page.Entries = append(page.Entries, e)
		page.BytesRead += n
	}
	return page, nil
}
func contentPageBudget(from int, override int64) int64 {
	if override > 0 {
		return override
	}
	if from == 0 {
		return defaults.TranscriptInitialReadBytes
	}
	return defaults.TranscriptContinuationReadBytes
}

// LoadFullSessionEntries releases its snapshot and pooled connection before
// returning entries to rendering, redaction or network consumers.
func (s *Store) LoadFullSessionEntries(ctx context.Context, id ingest.SessionID, softMaxBytes int64) (entries []schema.SessionEntry, capture ingest.SessionContentCapture, err error) {
	if softMaxBytes < 0 {
		return nil, capture, fmt.Errorf("store complete content read: negative page budget; no transcript read; use zero defaults or a positive override")
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, capture, err
	}
	defer s.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	defer func() {
		end(&err)
		if err != nil {
			entries = nil
		}
	}()
	if err := s.ValidateIndexFormatsOnConn(conn, []schema.SessionID{id}); err != nil {
		return nil, capture, err
	}
	return loadFullSessionEntriesOnConn(ctx, conn, id, softMaxBytes)
}

// loadFullSessionEntriesOnConn borrows an already-open read transaction. This
// also lets publication load its metadata and content from the same snapshot.
func loadFullSessionEntriesOnConn(ctx context.Context, conn *sqlite.Conn, id ingest.SessionID, softMaxBytes int64) (entries []schema.SessionEntry, capture ingest.SessionContentCapture, err error) {
	fail := func(cause error) ([]schema.SessionEntry, ingest.SessionContentCapture, error) {
		if ctx.Err() != nil {
			cause = ctx.Err()
		}
		return nil, capture, fmt.Errorf("store complete content read: verification or hydration failed; no complete transcript returned: %w; retry or run harvest index --force from retained artifacts to repair", cause)
	}
	if softMaxBytes < 0 {
		return fail(fmt.Errorf("negative page budget; use zero defaults or a positive override"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	var found bool
	capture, found, err = readCapture(conn, id)
	if err != nil {
		return fail(err)
	}
	if !found || capture.Status != ingest.ContentCaptureComplete || capture.FullCaptureSHA256 == "" || capture.SessionID != id {
		return fail(ErrContentCaptureIncomplete)
	}
	if err := verifyCaptureProjection(ctx, conn, id, capture, false); err != nil {
		return fail(err)
	}
	for from := 0; ; {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		page, err := readContentPageOnConn(ctx, conn, id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent, FromIndex: from, Limit: 100, SoftMaxBytes: contentPageBudget(from, softMaxBytes)})
		if err != nil {
			return fail(err)
		}
		entries = append(entries, page.Entries...)
		if page.NextIndex == nil {
			if len(entries) != capture.EntryCount {
				return fail(contentIntegrityError())
			}
			return entries, capture, nil
		}
		if len(page.Entries) == 0 || *page.NextIndex <= from || *page.NextIndex <= page.Entries[len(page.Entries)-1].EntryIndex {
			return fail(fmt.Errorf("full content cursor did not advance"))
		}
		from = *page.NextIndex
	}
}

// loadAvailableSessionEntriesOnConn returns the content that is actually
// stored, with no completeness, publication-readiness, recovery or native-source
// gate: the verified full text when the capture is complete, and the bounded
// projection SQLite already holds otherwise. A damaged complete capture still
// fails, because a preview may show less than the session, never something the
// database cannot prove it stored. The caller validates the stored index format
// on the same connection before calling this.
func loadAvailableSessionEntriesOnConn(ctx context.Context, conn *sqlite.Conn, id ingest.SessionID) (entries []schema.SessionEntry, capture ingest.SessionContentCapture, err error) {
	capture, found, err := readCapture(conn, id)
	if err != nil {
		return nil, capture, err
	}
	if found && capture.Status == ingest.ContentCaptureComplete {
		return loadFullSessionEntriesOnConn(ctx, conn, id, 0)
	}
	entries, err = listEntriesOnConn(conn, id)
	if err != nil {
		return nil, capture, err
	}
	return entries, capture, nil
}

func entryStringBytes(e schema.SessionEntry) int64 {
	var n int64
	for _, v := range []*string{e.ContentPreview, e.ToolInput, e.ToolOutput, e.Extra, e.EntryID, e.ParentEntryID, e.ToolCallID, e.ToolNamesCSV, e.PartType} {
		if v != nil {
			n += int64(len(*v))
		}
	}
	return n
}
