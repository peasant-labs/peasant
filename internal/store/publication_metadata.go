package store

import (
	"context"
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
	return string(m.SessionID) == stmt.ColumnText(0) && string(m.HostSlug) == stmt.ColumnText(1) && parent == stmt.ColumnText(2) && string(m.Project.Hash) == stmt.ColumnText(5) && remote == stmt.ColumnText(7) && m.MetadataHash == stmt.ColumnText(11) && m.ContentHash == stmt.ColumnText(12) && m.CWD == stmt.ColumnText(13)
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

// Compare historical identity before any dimensions or session facts are changed.
// The ingest normalizer must overlay this attribution before computing hashes.
func validatePublicationCapture(conn *sqlite.Conn, entry ingest.StoreEntry) error {
	m := entry.Metadata
	if err := validateCaptureMetadata(m, entry.CWDProvenance); err != nil {
		return err
	}
	if entry.Session.SessionID != "" && entry.Session.SessionID != m.SessionID {
		return publicationRepairError("discovered and captured session identities disagree")
	}
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
			if stmt.ColumnText(0) != string(m.Project.Hash) || stmt.ColumnText(1) != parent || stmt.ColumnText(2) != string(m.HostSlug) || stmt.ColumnText(3) != remote {
				return publicationRepairError("captured metadata disagrees with stored historical attribution")
			}
			return nil
		},
	})
}

func persistPublicationCapture(conn *sqlite.Conn, entry ingest.StoreEntry) (int64, error) {
	m := entry.Metadata
	body, err := json.Marshal(m)
	if err != nil {
		return 0, publicationRepairError("metadata cannot be encoded")
	}
	var revision int64
	err = sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_cwd=?, cwd_provenance_kind=?,
 publication_capture_revision=publication_capture_revision+1 WHERE session_id=? RETURNING publication_capture_revision`, &sqlitex.ExecOptions{
		Args:       []any{m.CWD, string(entry.CWDProvenance), string(m.SessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error { revision = stmt.ColumnInt64(0); return nil },
	})
	if err != nil {
		return 0, fmt.Errorf("store: allocate publication capture revision; transaction rolled back, retry ingest: %w", err)
	}
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
	if expected < 0 || current != expected {
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
		}
	}()
	end := sqlitex.Transaction(conn)
	defer end(&err)
	found := false
	err = sqlitex.ExecuteTransient(conn, `SELECT s.project_hash,s.session_origin,s.publication_capture_revision,
 s.indexed_publication_capture_revision,COALESCE(s.session_cwd,''),s.cwd_provenance_kind,
 COALESCE(s.parent_id,''),h.host_slug,COALESCE(h.git_remote,''),
 p.capture_revision,p.schema_version,p.metadata_json,p.metadata_hash,p.content_hash
 FROM sessions s JOIN host_slugs h ON h.opaque_id=s.opaque_host_id
 LEFT JOIN session_publication_metadata p ON p.session_id=s.session_id WHERE s.session_id=?`, &sqlitex.ExecOptions{
		Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			var parseErr error
			bundle.ReceiptProjectHash, parseErr = schema.NewProjectHash(stmt.ColumnText(0))
			if parseErr != nil {
				return publicationRepairError("invalid stored project identity")
			}
			bundle.SessionOrigin, parseErr = sessionorigin.Parse(stmt.ColumnText(1))
			if parseErr != nil {
				return parseErr
			}
			bundle.CaptureRevision = stmt.ColumnInt64(2)
			if stmt.ColumnType(9) == sqlite.TypeNull || stmt.ColumnInt(10) != ingest.CurrentSchemaVersion {
				return nil
			}
			kind, parseErr := ingest.NewCWDProvenanceKind(stmt.ColumnText(5))
			if parseErr != nil {
				return parseErr
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
			if m.SessionID != id || m.Project.Hash != bundle.ReceiptProjectHash || parent != stmt.ColumnText(6) || string(m.HostSlug) != stmt.ColumnText(7) || remote != stmt.ColumnText(8) || m.CWD != stmt.ColumnText(4) || m.MetadataHash != stmt.ColumnText(12) || m.ContentHash != stmt.ColumnText(13) {
				return publicationRepairError("snapshot identity, CWD or integrity columns disagree")
			}
			if bundle.CaptureRevision > 0 && bundle.CaptureRevision == stmt.ColumnInt64(3) && bundle.CaptureRevision == stmt.ColumnInt64(9) {
				bundle.Readiness = ingest.PublicationReady
			}
			return nil
		},
	})
	if err != nil {
		return bundle, err
	}
	if !found {
		return bundle, publicationRepairError("session is missing from database")
	}
	bundle.Entries, err = listEntriesOnConn(conn, id)
	if err != nil {
		return bundle, err
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
