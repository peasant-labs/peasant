package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// errAssociationAlias reports an attempt to assign a second durable ID to an
// existing session/observed-commit relationship. Callers must replay the ID
// already returned by the producer instead.
var errAssociationAlias = errors.New("session commit association alias")

// errAssociationRebind reports an attempt to reuse an existing durable ID for
// a different session/observed-commit relationship. Durable association IDs
// are opaque identities, never mutable pointers.
var errAssociationRebind = errors.New("session commit association rebind")

// associationConflictError preserves the typed conflict class while describing
// the complete relationship that could not be persisted.
type associationConflictError struct {
	Kind               error
	RequestedID        schema.AssociationID
	ExistingID         schema.AssociationID
	SessionID          schema.SessionID
	ObservedCommitHash string
}

func (e *associationConflictError) Error() string {
	switch {
	case errors.Is(e.Kind, errAssociationAlias):
		return fmt.Sprintf("association alias: session %q and observed commit %q already own durable ID %q; requested ID %q would create a second identity, so replay the stored ID", e.SessionID, e.ObservedCommitHash, e.ExistingID, e.RequestedID)
	case errors.Is(e.Kind, errAssociationRebind):
		return fmt.Sprintf("association rebind: durable ID %q cannot be rebound to session %q and observed commit %q; retain the ID's original relationship and allocate a new producer-owned ID", e.RequestedID, e.SessionID, e.ObservedCommitHash)
	default:
		return fmt.Sprintf("association conflict for session %q and observed commit %q", e.SessionID, e.ObservedCommitHash)
	}
}

func (e *associationConflictError) Unwrap() error { return e.Kind }

// associationLookupRequest identifies the producer-owned relationship to look
// up, create, or replay. ID is optional for ordinary ingest: when absent the
// store allocates an opaque ID exactly once. Supplying ID is reserved for a
// producer replay and makes alias/rebind mistakes explicit rather than silently
// changing a stored relationship.
type associationLookupRequest struct {
	ID                 *schema.AssociationID
	SessionID          schema.SessionID
	ObservedCommitHash string
	Subject            string
	AuthorTime         *int64
}

// sessionCommitAssociation is the normalized persisted relationship. Subject
// and AuthorTime are captured with the original observation for the rewrite
// ledger; they are intentionally not refreshed on replay.
type sessionCommitAssociation struct {
	ID                 schema.AssociationID
	SessionID          schema.SessionID
	ObservedCommitHash string
	Subject            string
	AuthorTime         *int64
}

const (
	sqlInsertSessionCommitAssociation = `INSERT INTO session_commit_associations (
    association_id, session_id, observed_commit_hash, subject, author_time
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING`

	sqlFindAssociationByID = `SELECT
    association_id, session_id, observed_commit_hash, COALESCE(subject, ''), author_time
FROM session_commit_associations
WHERE association_id = ?`

	sqlFindAssociationByRelationship = `SELECT
    association_id, session_id, observed_commit_hash, COALESCE(subject, ''), author_time
FROM session_commit_associations
WHERE session_id = ? AND observed_commit_hash = ?`

	sqlListCurrentSessionCommitAssociations = `SELECT
    association_id, session_id, observed_commit_hash, COALESCE(subject, ''), author_time
FROM session_commit_associations
WHERE session_id = ?
  AND EXISTS (
      SELECT 1
      FROM session_commits sc
      WHERE sc.session_id = session_commit_associations.session_id
        AND sc.commit_hash = session_commit_associations.observed_commit_hash
  )
ORDER BY observed_commit_hash`
)

// lookupOrCreateSessionCommitAssociation is the only storage entry point for
// durable association identity. It is transactional and idempotent: an exact
// replay returns the existing row without mutation; a second ID for the same
// relationship returns errAssociationAlias; and an existing ID for a different
// relationship returns errAssociationRebind.
func (s *Store) lookupOrCreateSessionCommitAssociation(ctx context.Context, request associationLookupRequest) (result sessionCommitAssociation, err error) {
	if err := validateAssociationLookupRequest(request); err != nil {
		return sessionCommitAssociation{}, err
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return sessionCommitAssociation{}, fmt.Errorf("store.lookupOrCreateSessionCommitAssociation: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)
	return lookupOrCreateSessionCommitAssociation(conn, request)
}

func validateAssociationLookupRequest(request associationLookupRequest) error {
	if request.SessionID == "" {
		return fmt.Errorf("store.lookupOrCreateSessionCommitAssociation: session ID is empty; a durable association must name the authoritative recorded session; provide the stored session ID before creating or replaying an association")
	}
	if strings.TrimSpace(request.ObservedCommitHash) == "" {
		return fmt.Errorf("store.lookupOrCreateSessionCommitAssociation: observed commit hash is empty; a durable association must retain the commit hash observed during the session; provide that observed hash before creating or replaying an association")
	}
	if request.ID != nil {
		if err := request.ID.Validate(); err != nil {
			return fmt.Errorf("store.lookupOrCreateSessionCommitAssociation: validate supplied association ID: %w", err)
		}
	}
	return nil
}

func lookupOrCreateSessionCommitAssociation(conn *sqlite.Conn, request associationLookupRequest) (sessionCommitAssociation, error) {
	requestedID := request.ID
	for attempt := 0; attempt < 4; attempt++ {
		candidate := schema.AssociationID("")
		if requestedID != nil {
			candidate = *requestedID
		} else {
			candidate = schema.AssociationID("assoc-" + uuid.NewString())
		}

		if err := sqlitex.ExecuteTransient(conn, sqlInsertSessionCommitAssociation, &sqlitex.ExecOptions{
			Args: []any{candidate.String(), string(request.SessionID), request.ObservedCommitHash, nullableString(request.Subject), nullableAssociationAuthorTime(request.AuthorTime)},
		}); err != nil {
			return sessionCommitAssociation{}, fmt.Errorf("store.lookupOrCreateSessionCommitAssociation: create association %q for session %q observed commit %q: %w", candidate, request.SessionID, request.ObservedCommitHash, err)
		}

		byRelationship, relationshipExists, err := lookupAssociationByRelationship(conn, request.SessionID, request.ObservedCommitHash)
		if err != nil {
			return sessionCommitAssociation{}, err
		}
		if relationshipExists {
			if requestedID != nil && byRelationship.ID != *requestedID {
				return sessionCommitAssociation{}, &associationConflictError{
					Kind:               errAssociationAlias,
					RequestedID:        *requestedID,
					ExistingID:         byRelationship.ID,
					SessionID:          request.SessionID,
					ObservedCommitHash: request.ObservedCommitHash,
				}
			}
			return byRelationship, nil
		}

		byID, idExists, err := lookupAssociationByID(conn, candidate)
		if err != nil {
			return sessionCommitAssociation{}, err
		}
		if idExists {
			if requestedID == nil {
				// A UUID collision is extraordinarily unlikely, but retrying keeps
				// generated IDs opaque instead of exposing a spurious rebind error.
				continue
			}
			return sessionCommitAssociation{}, &associationConflictError{
				Kind:               errAssociationRebind,
				RequestedID:        *requestedID,
				ExistingID:         byID.ID,
				SessionID:          request.SessionID,
				ObservedCommitHash: request.ObservedCommitHash,
			}
		}
	}
	return sessionCommitAssociation{}, fmt.Errorf("store.lookupOrCreateSessionCommitAssociation: could not allocate an opaque association ID after repeated collisions; retry the ingest operation")
}

func nullableAssociationAuthorTime(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func lookupAssociationByID(conn *sqlite.Conn, id schema.AssociationID) (sessionCommitAssociation, bool, error) {
	return scanOneAssociation(conn, sqlFindAssociationByID, []any{id.String()})
}

func lookupAssociationByRelationship(conn *sqlite.Conn, sessionID schema.SessionID, observedCommitHash string) (sessionCommitAssociation, bool, error) {
	return scanOneAssociation(conn, sqlFindAssociationByRelationship, []any{string(sessionID), observedCommitHash})
}

func scanOneAssociation(conn *sqlite.Conn, query string, args []any) (sessionCommitAssociation, bool, error) {
	var (
		row   sessionCommitAssociation
		found bool
	)
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			association, err := scanSessionCommitAssociation(stmt)
			if err != nil {
				return err
			}
			row = association
			found = true
			return nil
		},
	})
	if err != nil {
		return sessionCommitAssociation{}, false, fmt.Errorf("store: lookup session commit association: %w", err)
	}
	return row, found, nil
}

func scanSessionCommitAssociation(stmt *sqlite.Stmt) (sessionCommitAssociation, error) {
	id, err := schema.NewAssociationID(stmt.ColumnText(0))
	if err != nil {
		return sessionCommitAssociation{}, fmt.Errorf("store: stored session commit association ID is invalid: %w; run `peasant ingest verify` and repair the store before publishing timeline data", err)
	}
	row := sessionCommitAssociation{
		ID:                 id,
		SessionID:          schema.SessionID(stmt.ColumnText(1)),
		ObservedCommitHash: stmt.ColumnText(2),
		Subject:            stmt.ColumnText(3),
	}
	if stmt.ColumnType(4) != sqlite.TypeNull {
		value := stmt.ColumnInt64(4)
		row.AuthorTime = &value
	}
	return row, nil
}

// ListCurrentSessionCommitAssociations returns the durable associations for the
// session's authoritative current session_commits rows. Historical ledger rows
// deliberately stay out of publish context after a re-ingest removes a current
// binding; they remain available only to rewrite rendering.
func (s *Store) ListCurrentSessionCommitAssociations(ctx context.Context, sessionID ingest.SessionID) ([]ingest.CurrentCommitAssociation, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.ListCurrentSessionCommitAssociations: take connection for session %q: %w", sessionID, err)
	}
	defer s.pool.Put(conn)

	rows := make([]ingest.CurrentCommitAssociation, 0)
	err = sqlitex.ExecuteTransient(conn, sqlListCurrentSessionCommitAssociations, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			association, scanErr := scanSessionCommitAssociation(stmt)
			if scanErr != nil {
				return scanErr
			}
			rows = append(rows, ingest.CurrentCommitAssociation{
				ID:                 association.ID,
				ObservedCommitHash: association.ObservedCommitHash,
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store.ListCurrentSessionCommitAssociations: query session %q: %w", sessionID, err)
	}
	return rows, nil
}
