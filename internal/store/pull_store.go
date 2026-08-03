package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ---------------------------------------------------------------------------
// V34 row shapes: the store-side projection of the pulled_transcripts /
// pulled_annotations tables. The pull package declares its consumer-side
// PullStore interface against these shapes; *store.Store implements it.
//
// These tables are a DERIVED INDEX of the on-disk village-pulls/ manifests:
// any files↔DB divergence is repaired by re-pull (--force), never manual
// surgery. Nothing here feeds ingest/analytics or the annotate-push candidate
// set — pulled rows are foreign and one-way.
// ---------------------------------------------------------------------------

// PulledTranscriptRow is the V34 pulled_transcripts row. Title, Harness,
// ProjectName, and LocalSessionID are display/correlation snapshots that the
// village may omit (empty string ⇒ stored as SQL NULL). ContentHash is the
// SERVER-COMPUTED served-blob hash used for pull idempotency.
type PulledTranscriptRow struct {
	VillageHost     string
	TranscriptID    schema.TranscriptID
	OwnerUserID     string
	OwnerUsername   string
	LocalSessionID  string // round-trip correlation; "" ⇒ NULL
	Title           string // display; "" ⇒ NULL
	Harness         schema.Harness
	ProjectName     string // display; "" ⇒ NULL
	ContentHash     string // served-blob hash
	Visibility      schema.Visibility
	License         schema.License // village licenses.id; "" ⇒ NULL (no license granted)
	PullDir         string         // on-disk manifest location
	FirstPulledAt   int64
	LastPulledAt    int64
	AnnotationCount int
}

// PulledAnnotationRow is the V34 pulled_annotations row. ContentHash is the
// dedup key (per village). TranscriptID has no FK — it may target an OWN
// pushed transcript that has no pulled_transcripts row (refresh path).
// Payload holds the full schema.PullAnnotation JSON. RetractedAt is
// schema-only (designed for the deferred retraction feature; nil in the MVP).
type PulledAnnotationRow struct {
	VillageHost    string
	ContentHash    string
	TranscriptID   schema.TranscriptID
	LocalSessionID string // "" ⇒ NULL
	AuthorUserID   string
	AuthorUsername string
	Payload        string // schema.PullAnnotation JSON
	PulledAt       int64
	RetractedAt    *int64 // schema-only; nil in MVP
}

// PullCommit is the atomic payload for CommitPull: one transcript row plus its
// annotation rows, written in a SINGLE SQLite transaction. The field is named
// `commit` at the call site (it is a commit payload, not a transaction handle).
type PullCommit struct {
	Transcript  PulledTranscriptRow
	Annotations []PulledAnnotationRow
}

// ---------------------------------------------------------------------------
// SQL
// ---------------------------------------------------------------------------

const (
	// sqlUpsertPulledTranscript upserts a pulled_transcripts row keyed on the
	// composite PK (village_host, transcript_id). On conflict it refreshes the
	// mutable snapshot columns and last_pulled_at, preserving first_pulled_at.
	// license_id is OVERWRITTEN (not COALESCEd) like every other snapshot
	// column: the row mirrors server truth, and the village's sanctioned ops
	// path can legitimately clear a granted license — a re-pull must reflect
	// that, not resurrect a stale local value.
	sqlUpsertPulledTranscript = `INSERT INTO pulled_transcripts (
    village_host, transcript_id, owner_user_id, owner_username,
    local_session_id, title, harness, project_name,
    content_hash, visibility, pull_dir,
    first_pulled_at, last_pulled_at, annotation_count, license_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(village_host, transcript_id) DO UPDATE SET
    owner_user_id    = excluded.owner_user_id,
    owner_username   = excluded.owner_username,
    local_session_id = excluded.local_session_id,
    title            = excluded.title,
    harness          = excluded.harness,
    project_name     = excluded.project_name,
    content_hash     = excluded.content_hash,
    visibility       = excluded.visibility,
    pull_dir         = excluded.pull_dir,
    last_pulled_at   = excluded.last_pulled_at,
    annotation_count = excluded.annotation_count,
    license_id       = excluded.license_id`

	// sqlUpsertPulledAnnotation upserts a pulled_annotations row keyed on the
	// composite PK (village_host, content_hash). On conflict it refreshes the
	// row (a re-pull/refresh of the same content). retracted_at is never
	// written by the MVP (schema-only).
	sqlUpsertPulledAnnotation = `INSERT INTO pulled_annotations (
    village_host, content_hash, transcript_id, local_session_id,
    author_user_id, author_username, payload, pulled_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(village_host, content_hash) DO UPDATE SET
    transcript_id    = excluded.transcript_id,
    local_session_id = excluded.local_session_id,
    author_user_id   = excluded.author_user_id,
    author_username  = excluded.author_username,
    payload          = excluded.payload,
    pulled_at        = excluded.pulled_at`

	// sqlAnnotationCurrent fetches the mutable columns of an existing
	// (village_host, content_hash) row — used by UpsertPulledAnnotations to
	// classify created vs updated vs skipped (byte-identical) in one round-trip
	// per row. A row with no result ⇒ created; a result whose columns equal the
	// incoming row ⇒ skipped; otherwise ⇒ updated. pulled_at is intentionally
	// EXCLUDED from the comparison: it is a pull-bookkeeping timestamp that
	// changes on every refresh, so including it would make every re-pull look
	// "updated" — the skip notion is about CONTENT identity, not pull recency.
	sqlAnnotationCurrent = `SELECT transcript_id, local_session_id, author_user_id, author_username, payload
FROM pulled_annotations WHERE village_host = ? AND content_hash = ?`

	// sqlListPulledTranscripts returns all pulled transcripts, newest pull first.
	sqlListPulledTranscripts = `SELECT
    village_host, transcript_id, owner_user_id, owner_username,
    local_session_id, title, harness, project_name,
    content_hash, visibility, pull_dir,
    first_pulled_at, last_pulled_at, annotation_count, license_id
FROM pulled_transcripts
ORDER BY last_pulled_at DESC, transcript_id`

	// sqlGetPulledTranscript returns a single pulled transcript by composite PK.
	sqlGetPulledTranscript = `SELECT
    village_host, transcript_id, owner_user_id, owner_username,
    local_session_id, title, harness, project_name,
    content_hash, visibility, pull_dir,
    first_pulled_at, last_pulled_at, annotation_count, license_id
FROM pulled_transcripts
WHERE village_host = ? AND transcript_id = ?`
)

// ---------------------------------------------------------------------------
// PullStore write path
// ---------------------------------------------------------------------------

// CommitPull atomically writes one pulled_transcripts row plus all of its
// pulled_annotations rows in a SINGLE SQLite transaction. Any error rolls the
// whole transaction back, leaving zero rows — satisfying the inverted
// fail-open doctrine (a failed pull touches nothing local). The DB write is
// the last commit point of the pull pipeline (after the atomic dir rename);
// the V34 tables are a derived index repaired by re-pull on divergence.
//
// The parameter is named `commit` deliberately: it is a commit payload, not a
// transaction handle.
func (s *Store) CommitPull(ctx context.Context, commit PullCommit) (err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store.CommitPull: take connection for transcript %s @ %s: %w",
			commit.Transcript.TranscriptID, commit.Transcript.VillageHost, err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	t := commit.Transcript
	if err = sqlitex.Execute(conn, sqlUpsertPulledTranscript, &sqlitex.ExecOptions{
		Args: []any{
			t.VillageHost, string(t.TranscriptID), t.OwnerUserID, t.OwnerUsername,
			nullableString(t.LocalSessionID), nullableString(t.Title),
			nullableString(string(t.Harness)), nullableString(t.ProjectName),
			t.ContentHash, string(t.Visibility), t.PullDir,
			t.FirstPulledAt, t.LastPulledAt, t.AnnotationCount,
			nullableString(string(t.License)),
		},
	}); err != nil {
		return fmt.Errorf("store.CommitPull: upsert transcript %s @ %s: %w",
			t.TranscriptID, t.VillageHost, err)
	}

	for i := range commit.Annotations {
		a := &commit.Annotations[i]
		if err = sqlitex.Execute(conn, sqlUpsertPulledAnnotation, &sqlitex.ExecOptions{
			Args: []any{
				a.VillageHost, a.ContentHash, string(a.TranscriptID),
				nullableString(a.LocalSessionID),
				a.AuthorUserID, a.AuthorUsername, a.Payload, a.PulledAt,
			},
		}); err != nil {
			return fmt.Errorf("store.CommitPull: upsert annotation %s for transcript %s @ %s: %w",
				a.ContentHash, t.TranscriptID, t.VillageHost, err)
		}
	}

	return nil
}

// UpsertPulledAnnotations writes a batch of foreign annotations in a single
// SQLite transaction and reports how many were created, updated, or skipped.
// This is the annotation-refresh path (foreign annotations on OWN pushed
// transcripts); it touches no files, so a failure simply leaves zero rows.
//
//   - created: no prior (village_host, content_hash) row existed → INSERT
//   - updated: a prior row existed and its content-bearing columns DIFFER from
//     the incoming row → UPDATE
//   - skipped: a prior row existed and is BYTE-IDENTICAL on the content-bearing
//     columns (transcript_id, local_session_id, author_user_id, author_username,
//     payload) → no write at all (a re-refresh of unchanged content). pulled_at
//     is deliberately NOT part of the comparison, so an unchanged annotation
//     re-pull reports `skipped`, not `updated` — matching the created/updated/
//     skipped vocabulary of the push path (AnnotationPushResponse).
//
// Own-authored exclusion (creds.UserID == AuthorUserID) is the caller's
// responsibility (the pipeline filters before calling this method).
func (s *Store) UpsertPulledAnnotations(ctx context.Context, annotations []PulledAnnotationRow) (created, updated, skipped int, err error) {
	if len(annotations) == 0 {
		return 0, 0, 0, nil
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store.UpsertPulledAnnotations: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	for i := range annotations {
		a := &annotations[i]

		// Fetch the current content-bearing columns (if any) to classify the
		// write as created / updated / skipped in one round-trip.
		var (
			exists  bool
			current PulledAnnotationRow
		)
		if err = sqlitex.Execute(conn, sqlAnnotationCurrent, &sqlitex.ExecOptions{
			Args: []any{a.VillageHost, a.ContentHash},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				exists = true
				current.TranscriptID = schema.TranscriptID(stmt.ColumnText(0))
				current.LocalSessionID = stmt.ColumnText(1)
				current.AuthorUserID = stmt.ColumnText(2)
				current.AuthorUsername = stmt.ColumnText(3)
				current.Payload = stmt.ColumnText(4)
				return nil
			},
		}); err != nil {
			return 0, 0, 0, fmt.Errorf("store.UpsertPulledAnnotations: existence check for %s @ %s: %w",
				a.ContentHash, a.VillageHost, err)
		}

		// Byte-identical re-refresh: skip the UPDATE entirely (no write, no
		// pulled_at bump). The stored NULLs decode to "" via ColumnText, which
		// is exactly how nullableString round-trips an empty LocalSessionID.
		if exists &&
			current.TranscriptID == a.TranscriptID &&
			current.LocalSessionID == a.LocalSessionID &&
			current.AuthorUserID == a.AuthorUserID &&
			current.AuthorUsername == a.AuthorUsername &&
			current.Payload == a.Payload {
			skipped++
			continue
		}

		if err = sqlitex.Execute(conn, sqlUpsertPulledAnnotation, &sqlitex.ExecOptions{
			Args: []any{
				a.VillageHost, a.ContentHash, string(a.TranscriptID),
				nullableString(a.LocalSessionID),
				a.AuthorUserID, a.AuthorUsername, a.Payload, a.PulledAt,
			},
		}); err != nil {
			return 0, 0, 0, fmt.Errorf("store.UpsertPulledAnnotations: upsert annotation %s @ %s: %w",
				a.ContentHash, a.VillageHost, err)
		}

		if exists {
			updated++
		} else {
			created++
		}
	}

	return created, updated, skipped, nil
}

// ---------------------------------------------------------------------------
// PullStore read path
// ---------------------------------------------------------------------------

// ListPulledTranscripts returns every pulled transcript, newest pull first.
// Offline read (no network) backing `village transcripts list --local`.
func (s *Store) ListPulledTranscripts(ctx context.Context) ([]PulledTranscriptRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.ListPulledTranscripts: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []PulledTranscriptRow
	err = sqlitex.Execute(conn, sqlListPulledTranscripts, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanPulledTranscriptRow(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store.ListPulledTranscripts: query: %w", err)
	}
	return rows, nil
}

// GetPulledTranscript returns a single pulled transcript by its composite PK,
// or (nil, nil) if no such row exists. Offline read.
func (s *Store) GetPulledTranscript(ctx context.Context, villageHost string, id schema.TranscriptID) (*PulledTranscriptRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.GetPulledTranscript: take connection for %s @ %s: %w", id, villageHost, err)
	}
	defer s.pool.Put(conn)

	var row *PulledTranscriptRow
	err = sqlitex.Execute(conn, sqlGetPulledTranscript, &sqlitex.ExecOptions{
		Args: []any{villageHost, string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			r := scanPulledTranscriptRow(stmt)
			row = &r
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store.GetPulledTranscript: query for %s @ %s: %w", id, villageHost, err)
	}
	return row, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// scanPulledTranscriptRow reads a PulledTranscriptRow from the current
// statement position. Column order must match the SELECT lists above.
// Nullable columns (local_session_id, title, harness, project_name,
// license_id) decode to the zero value ("") when SQL NULL.
func scanPulledTranscriptRow(stmt *sqlite.Stmt) PulledTranscriptRow {
	return PulledTranscriptRow{
		VillageHost:     stmt.ColumnText(0),
		TranscriptID:    schema.TranscriptID(stmt.ColumnText(1)),
		OwnerUserID:     stmt.ColumnText(2),
		OwnerUsername:   stmt.ColumnText(3),
		LocalSessionID:  stmt.ColumnText(4),
		Title:           stmt.ColumnText(5),
		Harness:         schema.Harness(stmt.ColumnText(6)),
		ProjectName:     stmt.ColumnText(7),
		ContentHash:     stmt.ColumnText(8),
		Visibility:      schema.Visibility(stmt.ColumnText(9)),
		PullDir:         stmt.ColumnText(10),
		FirstPulledAt:   stmt.ColumnInt64(11),
		LastPulledAt:    stmt.ColumnInt64(12),
		AnnotationCount: stmt.ColumnInt(13),
		License:         schema.License(stmt.ColumnText(14)),
	}
}
