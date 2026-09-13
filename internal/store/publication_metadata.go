package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.PublicationInputReader = (*Store)(nil)
var _ ingest.PublicationCaptureStore = (*Store)(nil)

func publicationRepairError(reason string) error {
	return fmt.Errorf("store: %s during publication metadata capture/read; publication is held to avoid mixing source evidence; run peasant ingest with retained sources and retry", reason)
}

// Bulk discovery uses the same integrity rules without acquiring a connection
// per session. A damaged capture is selected for normal-ingest repair.
func publicationLocationSnapshotValid(stmt *sqlite.Stmt) bool {
	var m schema.UnifiedMetadata
	kind, err := ingest.NewCWDProvenanceKind(stmt.ColumnText(14))
	if err != nil || json.Unmarshal([]byte(stmt.ColumnText(10)), &m) != nil || validateCaptureMetadata(&m, kind) != nil {
		return false
	}
	parent, remote := "", ""
	if m.ParentUUID != nil {
		parent = string(*m.ParentUUID)
	}
	if m.Git.Remote != nil {
		remote = *m.Git.Remote
	}
	return string(m.SessionID) == stmt.ColumnText(0) && string(m.HostSlug) == stmt.ColumnText(1) && parent == stmt.ColumnText(2) && string(m.Project.Hash) == stmt.ColumnText(5) && ingest.NormalizeRemoteForMatch(remote) == ingest.NormalizeRemoteForMatch(stmt.ColumnText(7)) && m.MetadataHash == stmt.ColumnText(11) && m.ContentHash == stmt.ColumnText(12) && m.CWD == stmt.ColumnText(13)
}

func validateCaptureMetadata(m *schema.UnifiedMetadata, kind ingest.CWDProvenanceKind) error {
	if _, err := ingest.NewCWDProvenanceKind(string(kind)); err != nil {
		return err
	}
	if kind == ingest.CWDNotRecovered {
		return publicationRepairError("source has not been inspected")
	}
	if (kind == ingest.CWDSourceExact) != (m.CWD != "") {
		return publicationRepairError("CWD and its source provenance disagree")
	}
	if m.SchemaVersion != ingest.CurrentSchemaVersion {
		return publicationRepairError("unsupported metadata schema")
	}
	if _, err := schema.NewSessionID(string(m.SessionID)); err != nil {
		return publicationRepairError("invalid metadata session identity")
	}
	if _, err := schema.NewProjectHash(string(m.Project.Hash)); err != nil {
		return publicationRepairError("invalid metadata project identity")
	}
	if _, err := schema.NewTranscriptContentHash(m.ContentHash); err != nil {
		return publicationRepairError("invalid captured content digest")
	}
	if m.MetadataHash != schema.ComputeMetadataHash(m) {
		return publicationRepairError("metadata integrity digest does not match snapshot")
	}
	return nil
}

// Validate source evidence before any dimensions or session facts change.
// Project attribution may transition with the adapter's tracked project repair.
func validatePublicationCapture(entry ingest.StoreEntry) error {
	m := entry.Metadata
	if err := validateCaptureMetadata(m, entry.CWDProvenance); err != nil {
		return err
	}
	if entry.Session.SessionID != "" && entry.Session.SessionID != m.SessionID {
		return publicationRepairError("discovered and captured session identities disagree")
	}
	return nil
}

// Validate the resulting relation inside the same transaction as the upsert,
// before publishing its capture revision. Receipts are not changed by ingest.
// The shared host dimension retains its first remote spelling; compare remote
// identity like ingest does, without discarding the capture's source spelling.
func validateStoredPublicationCapture(conn *sqlite.Conn, entry ingest.StoreEntry) error {
	m := entry.Metadata
	return sqlitex.ExecuteTransient(conn, `SELECT s.project_hash, COALESCE(s.parent_id,''), h.host_slug, COALESCE(h.git_remote,'')
FROM sessions s JOIN host_slugs h ON h.opaque_id=s.opaque_host_id WHERE s.session_id=?`, &sqlitex.ExecOptions{
		Args: []any{string(m.SessionID)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			parent, remote := "", ""
			if m.ParentUUID != nil {
				parent = string(*m.ParentUUID)
			}
			if m.Git.Remote != nil {
				remote = *m.Git.Remote
			}
			if stmt.ColumnText(0) != string(m.Project.Hash) || stmt.ColumnText(1) != parent || stmt.ColumnText(2) != string(m.HostSlug) || ingest.NormalizeRemoteForMatch(stmt.ColumnText(3)) != ingest.NormalizeRemoteForMatch(remote) {
				return publicationRepairError("captured metadata disagrees with stored attribution")
			}
			return nil
		},
	})
}

// publicationCaptureSnapshot is the capture state as it stood BEFORE a session
// upsert. It must be read first: the upsert names every column the v51 trigger
// watches, so the trigger clears the indexed binding and the recovered
// provenance on every write, and the post-upsert row can no longer say whether
// a session fact actually changed.
//
// It deliberately carries no index binding. Nothing on this path may read or
// re-state one: the binding is the index writer's success evidence.
type publicationCaptureSnapshot struct {
	Found         bool
	Revision      int64
	MetadataHash  string
	ContentHash   string
	SchemaVersion int
	CWD           string
	CWDProvenance string
}

func readPublicationCaptureSnapshot(conn *sqlite.Conn, id ingest.SessionID) (snapshot publicationCaptureSnapshot, err error) {
	err = sqlitex.ExecuteTransient(conn, `SELECT s.publication_capture_revision,
 COALESCE(s.session_cwd,''),s.cwd_provenance_kind,p.capture_revision,p.schema_version,p.metadata_hash,p.content_hash
 FROM sessions s JOIN session_publication_metadata p ON p.session_id=s.session_id WHERE s.session_id=?`, &sqlitex.ExecOptions{
		Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			// A snapshot only describes a capture the session actually carries.
			if stmt.ColumnInt64(0) != stmt.ColumnInt64(3) {
				return nil
			}
			snapshot = publicationCaptureSnapshot{
				Found: true, Revision: stmt.ColumnInt64(0),
				CWD: stmt.ColumnText(1), CWDProvenance: stmt.ColumnText(2),
				SchemaVersion: stmt.ColumnInt(4), MetadataHash: stmt.ColumnText(5), ContentHash: stmt.ColumnText(6),
			}
			return nil
		},
	})
	if err != nil {
		return publicationCaptureSnapshot{}, fmt.Errorf("store: read publication capture for session %s before its metadata upsert: %w; nothing was changed; restore database access and retry ingest", id, err)
	}
	return snapshot, nil
}

// unchangedCapture reports that this write re-states exactly the capture the
// session already carried. Only the metadata digest, the transcript digest, the
// schema version and the recovered working directory decide it; each session
// column the trigger watches is derived from that same metadata, so an
// identical digest means no watched fact moved.
//
// Whether the INDEX has caught up is deliberately not part of it. An unchanged
// session keeps its capture revision whether or not its index write succeeded,
// because requiring the binding here would judge a session "changed" on its
// second unchanged re-ingest -- the first having cleared the binding -- and the
// revision climb would resume for exactly the sessions whose index write is
// held or failing, which are the ones that can least afford it.
func (snapshot publicationCaptureSnapshot) unchangedCapture(entry ingest.StoreEntry) bool {
	m := entry.Metadata
	return snapshot.Found && snapshot.Revision > 0 &&
		snapshot.MetadataHash == m.MetadataHash && snapshot.ContentHash == m.ContentHash &&
		snapshot.SchemaVersion == m.SchemaVersion && snapshot.CWD == m.CWD &&
		snapshot.CWDProvenance == string(entry.CWDProvenance)
}

func persistPublicationCapture(conn *sqlite.Conn, entry ingest.StoreEntry, prior publicationCaptureSnapshot) (int64, error) {
	m := entry.Metadata
	body, err := json.Marshal(m)
	if err != nil {
		return 0, publicationRepairError("metadata cannot be encoded")
	}
	var revision int64
	if prior.unchangedCapture(entry) {
		// Re-ingesting an unchanged session is not a new capture. Allocating a
		// revision here would leave the index stamp one behind on every single
		// harvest, so the session could never be published again: the stamp can
		// never catch a number that moves each time it is read. Re-state the
		// capture the upsert's trigger just cleared, at its own revision.
		//
		// The index binding is NOT re-stated. This is a metadata transaction,
		// and the binding is the index writer's success evidence: only a
		// successful index write may certify one. Re-binding here would report
		// a held or refused reindex as publishable, which is fabricated
		// success. The binding comes back when the index writer re-stamps it.
		revision = prior.Revision
		if err = sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_cwd=?, cwd_provenance_kind=?,
 publication_capture_revision=? WHERE session_id=?`, &sqlitex.ExecOptions{
			Args: []any{m.CWD, string(entry.CWDProvenance), revision, string(m.SessionID)},
		}); err != nil {
			return 0, fmt.Errorf("store: restore unchanged publication capture; transaction rolled back, retry ingest: %w", err)
		}
	} else {
		err = sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_cwd=?, cwd_provenance_kind=?,
 publication_capture_revision=publication_capture_revision+1 WHERE session_id=? RETURNING publication_capture_revision`, &sqlitex.ExecOptions{
			Args:       []any{m.CWD, string(entry.CWDProvenance), string(m.SessionID)},
			ResultFunc: func(stmt *sqlite.Stmt) error { revision = stmt.ColumnInt64(0); return nil },
		})
		if err != nil {
			return 0, fmt.Errorf("store: allocate publication capture revision; transaction rolled back, retry ingest: %w", err)
		}
	}
	// The snapshot is rewritten on BOTH paths. The digest that decided the
	// restore covers the metadata but not its redaction record or derivation
	// time, so re-serialising is what keeps the stored snapshot equal to the
	// metadata this ingest actually captured, at whichever revision it carries.
	err = sqlitex.ExecuteTransient(conn, `INSERT INTO session_publication_metadata
 (session_id,capture_revision,schema_version,metadata_json,metadata_hash,content_hash,captured_at)
 VALUES (?,?,?,?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET
 capture_revision=excluded.capture_revision,schema_version=excluded.schema_version,metadata_json=excluded.metadata_json,
 metadata_hash=excluded.metadata_hash,content_hash=excluded.content_hash,captured_at=excluded.captured_at`, &sqlitex.ExecOptions{
		Args: []any{string(m.SessionID), revision, m.SchemaVersion, string(body), m.MetadataHash, m.ContentHash, time.Now().UnixMilli()},
	})
	if err != nil {
		return 0, fmt.Errorf("store: persist publication snapshot; transaction rolled back, retry ingest: %w", err)
	}
	return revision, nil
}

func checkPublicationIndexRevision(conn *sqlite.Conn, id ingest.SessionID, expected int64) error {
	if expected == 0 {
		return nil
	}
	var current int64
	err := sqlitex.ExecuteTransient(conn, `SELECT s.publication_capture_revision FROM sessions s
 JOIN session_publication_metadata p ON p.session_id=s.session_id AND p.capture_revision=s.publication_capture_revision
 WHERE s.session_id=? AND s.cwd_provenance_kind!='not_recovered'`, &sqlitex.ExecOptions{
		Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error { current = stmt.ColumnInt64(0); return nil },
	})
	if err != nil {
		return err
	}
	if expected < 0 {
		return publicationRepairError("stale index capture revision; existing entries were not changed")
	}
	// No current capture means there is nothing this write could race: the
	// session's counter advanced past a capture that no longer exists. The
	// entries are written, and the binding predicate, which needs the capture
	// row, leaves publication held until a capture exists again.
	if current == 0 {
		return nil
	}
	if current != expected {
		return publicationRepairError("stale index capture revision; existing entries were not changed")
	}
	return nil
}

func stampPublicationIndex(conn *sqlite.Conn, id ingest.SessionID, revision int64) error {
	return sqlitex.ExecuteTransient(conn, `UPDATE sessions SET indexed_publication_capture_revision=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{revision, string(id)}})
}

// LoadPublicationInput takes one deferred read transaction. Every constituent
// reader uses this same connection, including extension rows and current metrics.
// Missing/unsupported captures return needs_ingest. Corrupt or conflicting
// evidence returns an error, never an approximation or a filesystem fallback.
func (s *Store) LoadPublicationInput(ctx context.Context, id ingest.SessionID) (bundle ingest.PublicationInputBundle, err error) {
	bundle.Readiness = ingest.PublicationNeedsIngest
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return bundle, err
	}
	defer s.pool.Put(conn)
	defer func() {
		if err != nil {
			bundle.Readiness = ingest.PublicationNeedsIngest
			bundle.Entries = nil
		}
	}()
	end := sqlitex.Transaction(conn)
	defer end(&err)
	found := false
	err = sqlitex.ExecuteTransient(conn, publicationMetadataSelect+` WHERE s.session_id=?`, &sqlitex.ExecOptions{
		Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			var scanErr error
			bundle, scanErr = scanPublicationMetadata(stmt, id)
			return scanErr
		},
	})
	if err != nil {
		return bundle, err
	}
	if !found {
		return bundle, publicationRepairError("session is missing from database")
	}
	if bundle.Readiness != ingest.PublicationReady {
		return bundle, nil
	}
	if err = s.ValidateIndexFormatsOnConn(conn, []schema.SessionID{id}); err != nil {
		return bundle, err
	}
	if err = sqlitex.ExecuteTransient(conn, `SELECT COALESCE(NULLIF(s.git_worktree,''),p.canonical_cwd,'') FROM sessions s JOIN projects p ON p.project_hash=s.project_hash WHERE s.session_id=?`, &sqlitex.ExecOptions{Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error { bundle.ProjectPath = stmt.ColumnText(0); return nil }}); err != nil {
		return bundle, err
	}
	bundle.Entries, bundle.ContentCapture, err = loadFullSessionEntriesOnConn(ctx, conn, id, 0)
	if err != nil {
		return bundle, err
	}
	if bundle.ContentCapture.SessionID != id || bundle.ContentCapture.PublicationCaptureRevision != bundle.CaptureRevision {
		return bundle, publicationRepairError("full content identity or publication revision disagrees with eligible metadata")
	}
	metrics, err := getMetricsOnConn(conn, id)
	if err != nil {
		return bundle, err
	}
	if metrics != nil {
		quality := metrics.QualityMetrics
		bundle.Quality = &quality
	}
	bundle.Associations = make([]schema.PublishedAssociation, 0)
	err = sqlitex.ExecuteTransient(conn, sqlListCurrentSessionCommitAssociations, &sqlitex.ExecOptions{
		Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			association, scanErr := scanSessionCommitAssociation(stmt)
			if scanErr != nil {
				return scanErr
			}
			bundle.Associations = append(bundle.Associations, schema.PublishedAssociation{ID: association.ID, ObservedCommitHash: association.ObservedCommitHash})
			return nil
		},
	})
	return bundle, err
}

const publicationMetadataSelect = `SELECT s.project_hash,s.session_origin,s.publication_capture_revision,
 s.indexed_publication_capture_revision,COALESCE(s.session_cwd,''),s.cwd_provenance_kind,
 COALESCE(s.parent_id,''),h.host_slug,COALESCE(h.git_remote,''),
 p.capture_revision,p.schema_version,p.metadata_json,p.metadata_hash,p.content_hash,s.session_id,
 c.status,c.full_capture_sha256,c.publication_capture_revision,COALESCE(c.failure_code,''),COALESCE(c.capture_format,'')
 FROM sessions s JOIN host_slugs h ON h.opaque_id=s.opaque_host_id
 LEFT JOIN session_publication_metadata p ON p.session_id=s.session_id
 LEFT JOIN session_content_captures c ON c.session_id=s.session_id`

// Both the full bundle and list projection validate exactly the same evidence.
func scanPublicationMetadata(stmt *sqlite.Stmt, id ingest.SessionID) (bundle ingest.PublicationInputBundle, err error) {
	bundle, err = scanPublicationMetadataProof(stmt, id)
	eligible, captureErr := publicationContentEligible(stmt, 15, bundle.CaptureRevision)
	if err != nil || captureErr != nil || !eligible {
		bundle.Readiness = ingest.PublicationNeedsIngest
	}
	if err == nil {
		err = captureErr
	}
	return bundle, err
}

// Capture-state columns only: eligibility deliberately does not verify payload.
//
// The columns are read back into the typed capture value and judged by the ONE
// publication rule (PublishableWithOmissions), so readiness cannot drift from
// what the complete-content readers will actually serve. Both readiness queries
// select the same five capture columns in the same order from this offset:
// status, full-capture proof, publication revision, failure code, capture format.
func publicationContentEligible(stmt *sqlite.Stmt, offset int, revision int64) (bool, error) {
	if stmt.ColumnType(offset) == sqlite.TypeNull {
		return false, nil
	}
	status, err := ingest.NewContentCaptureStatus(stmt.ColumnText(offset))
	if err != nil {
		return false, publicationRepairError("invalid full content capture status")
	}
	hash := stmt.ColumnText(offset + 1)
	if hash != "" || status == ingest.ContentCaptureComplete {
		decoded, decodeErr := hex.DecodeString(hash)
		if decodeErr != nil || len(decoded) != 32 {
			return false, publicationRepairError("malformed full content SHA-256 proof")
		}
	}
	contentRevision := stmt.ColumnInt64(offset + 2)
	if contentRevision < 0 {
		return false, publicationRepairError("negative full content publication revision")
	}
	// An unrecognized code or format fails CLOSED: readiness acts on these
	// values, so a state this build cannot name must never read as publishable.
	code, err := ingest.NewContentCaptureFailureCode(stmt.ColumnText(offset + 3))
	if err != nil {
		return false, publicationRepairError("invalid full content capture failure code")
	}
	format, err := ingest.NewContentCaptureFormat(stmt.ColumnText(offset + 4))
	if err != nil {
		return false, publicationRepairError("invalid full content capture format")
	}
	capture := ingest.SessionContentCapture{Status: status, FailureCode: code, CaptureFormat: format}
	// A publishable capture that is not complete still needs its full-capture
	// proof: its entries are read and hashed like any other full capture.
	if status != ingest.ContentCaptureComplete && hash == "" {
		return false, nil
	}
	return PublishableWithOmissions(capture) && revision > 0 && contentRevision == revision, nil
}

// Metadata/index proof is also used to bind content-only retained backfills.
// It does not require a previous full capture: that is the state being repaired.
func scanPublicationMetadataProof(stmt *sqlite.Stmt, id ingest.SessionID) (bundle ingest.PublicationInputBundle, err error) {
	bundle.Readiness = ingest.PublicationNeedsIngest
	err = func() error {
		var parseErr error
		bundle.ReceiptProjectHash, parseErr = schema.NewProjectHash(stmt.ColumnText(0))
		if parseErr != nil {
			return publicationRepairError("invalid stored project identity")
		}
		bundle.SessionOrigin, parseErr = sessionorigin.Parse(stmt.ColumnText(1))
		if parseErr != nil {
			return publicationRepairError("invalid stored session origin")
		}
		bundle.CaptureRevision = stmt.ColumnInt64(2)
		if stmt.ColumnType(9) == sqlite.TypeNull || stmt.ColumnInt(10) != ingest.CurrentSchemaVersion {
			return nil
		}
		kind, parseErr := ingest.NewCWDProvenanceKind(stmt.ColumnText(5))
		if parseErr != nil {
			return publicationRepairError("invalid stored CWD provenance")
		}
		if kind == ingest.CWDNotRecovered {
			return nil
		}
		if json.Unmarshal([]byte(stmt.ColumnText(11)), &bundle.Metadata) != nil {
			return publicationRepairError("malformed metadata snapshot")
		}
		m := &bundle.Metadata
		if parseErr = validateCaptureMetadata(m, kind); parseErr != nil {
			return parseErr
		}
		parent, remote := "", ""
		if m.ParentUUID != nil {
			parent = string(*m.ParentUUID)
		}
		if m.Git.Remote != nil {
			remote = *m.Git.Remote
		}
		if m.SessionID != id || m.Project.Hash != bundle.ReceiptProjectHash || parent != stmt.ColumnText(6) || string(m.HostSlug) != stmt.ColumnText(7) || ingest.NormalizeRemoteForMatch(remote) != ingest.NormalizeRemoteForMatch(stmt.ColumnText(8)) || m.CWD != stmt.ColumnText(4) || m.MetadataHash != stmt.ColumnText(12) || m.ContentHash != stmt.ColumnText(13) {
			return publicationRepairError("snapshot identity, CWD or integrity columns disagree")
		}
		if bundle.CaptureRevision > 0 && bundle.CaptureRevision == stmt.ColumnInt64(3) && bundle.CaptureRevision == stmt.ColumnInt64(9) {
			bundle.Readiness = ingest.PublicationReady
		}
		return nil
	}()
	return bundle, err
}

var _ ingest.PublicationMetadataReader = (*Store)(nil)

// LoadPublicationMetadata reads only requested capture rows in one SQLite read
// transaction. Transcript entries, quality and association ledgers are not read.
func (s *Store) LoadPublicationMetadata(ctx context.Context, ids []ingest.SessionID) (result map[ingest.SessionID]ingest.PublicationMetadata, err error) {
	result = make(map[ingest.SessionID]ingest.PublicationMetadata, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	missing := publicationRepairError("session is missing from database")
	for _, id := range ids {
		result[id] = ingest.PublicationMetadata{Readiness: ingest.PublicationNeedsIngest, Error: missing}
	}
	requested, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	defer func() {
		end(&err)
		if err != nil {
			result = nil
		}
	}()
	err = sqlitex.ExecuteTransient(conn, publicationMetadataSelect+` WHERE s.session_id IN (SELECT value FROM json_each(?))`, &sqlitex.ExecOptions{
		Args: []any{string(requested)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			id, parseErr := ingest.NewSessionID(stmt.ColumnText(14))
			if parseErr != nil {
				return parseErr
			}
			bundle, scanErr := scanPublicationMetadata(stmt, id)
			result[id] = ingest.PublicationMetadata{Metadata: bundle.Metadata, Readiness: bundle.Readiness, CaptureRevision: bundle.CaptureRevision, Error: scanErr}
			return nil
		},
	})
	return result, err
}
