package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// Claim source values name why a claim entry was appended. They are the closed
// set enforced by the owner_claims.source CHECK constraint.
const (
	// ClaimSourceSignInPrompt records the claim the sign-in prompt performed.
	ClaimSourceSignInPrompt = "sign_in_prompt"
	// ClaimSourceAutomatic records the claim at the end of a harvest batch.
	ClaimSourceAutomatic = "automatic"
	// ClaimSourceManual records an explicit `peasant user claim`.
	ClaimSourceManual = "manual"
)

// LocalOwner is the permanent owner of one Peasant data directory. It is not a
// Village account, a username, or an email address.
type LocalOwner struct {
	ID        string
	CreatedAt int64
}

// OwnerClaim is one append-only claim-log entry. VillageURL and UserID reuse the
// V68 collective-observation account scope; UsernameSnapshot is a display
// snapshot only and never identity.
type OwnerClaim struct {
	ID               string
	LocalOwnerID     string
	VillageURL       string
	UserID           string
	UsernameSnapshot string
	ClaimedAt        int64
	Source           string
}

// ClaimResult reports one claim operation. Claimed counts artifacts whose
// current owner pointer this operation assigned; AlreadyClaimed counts artifacts
// in scope that already carried a pointer and were left alone. ClaimID is empty
// when nothing needed claiming, in which case no claim entry was appended.
type ClaimResult struct {
	Claimed        int
	AlreadyClaimed int
	ClaimID        string
}

// OwnershipCounts reports local artifact ownership at the row level. Pulled
// rows are foreign and are not part of these counts.
type OwnershipCounts struct {
	TotalSessions        int
	ClaimedSessions      int
	UnclaimedSessions    int
	TotalAnnotations     int
	ClaimedAnnotations   int
	UnclaimedAnnotations int
}

// EnsureLocalOwner returns the data directory's single local owner, creating it
// on first use. Creation needs no Village login and no network access. The
// singleton UNIQUE constraint and the read-back after an idempotent insert keep
// exactly one row even when two callers create it concurrently.
func (s *Store) EnsureLocalOwner(ctx context.Context, nowMs int64) (LocalOwner, error) {
	if nowMs <= 0 {
		return LocalOwner{}, fmt.Errorf("create local owner: created_at %d is not a positive wall-clock millisecond; no owner was created; supply the current Unix time in milliseconds", nowMs)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return LocalOwner{}, fmt.Errorf("create local owner: take SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO local_owner(id, singleton, created_at) VALUES(?, 1, ?) ON CONFLICT DO NOTHING`, &sqlitex.ExecOptions{
		Args: []any{"lo_" + uuid.NewString(), nowMs},
	}); err != nil {
		return LocalOwner{}, fmt.Errorf("create local owner: insert the singleton row: %w; no owner was created; retry after repairing local store access", err)
	}
	var owner LocalOwner
	if err := sqlitex.ExecuteTransient(conn, `SELECT id, created_at FROM local_owner LIMIT 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			owner = LocalOwner{ID: stmt.ColumnText(0), CreatedAt: stmt.ColumnInt64(1)}
			return nil
		},
	}); err != nil {
		return LocalOwner{}, fmt.Errorf("create local owner: read the singleton row: %w; no owner was returned; retry after repairing local store access", err)
	}
	if owner.ID == "" {
		return LocalOwner{}, fmt.Errorf("create local owner: the singleton row is missing after creation; no owner was returned; reopen the store and retry")
	}
	return owner, nil
}

// LocalOwner returns the data directory's local owner, or nil when the owner has
// not been created yet. It reads local state only: no credentials and no
// network access are required.
func (s *Store) LocalOwner(ctx context.Context) (*LocalOwner, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read local owner: take SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	var owner *LocalOwner
	if err := sqlitex.ExecuteTransient(conn, `SELECT id, created_at FROM local_owner LIMIT 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			read := LocalOwner{ID: stmt.ColumnText(0), CreatedAt: stmt.ColumnInt64(1)}
			owner = &read
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("read local owner: %w; the owner record was not changed; retry after repairing local store access", err)
	}
	return owner, nil
}

// OwnerClaims returns the append-only claim log in claim order. It reads local
// state only: no credentials and no network access are required.
func (s *Store) OwnerClaims(ctx context.Context) ([]OwnerClaim, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read owner claims: take SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	var claims []OwnerClaim
	if err := sqlitex.ExecuteTransient(conn, `SELECT id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source
FROM owner_claims ORDER BY claimed_at, id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			claims = append(claims, OwnerClaim{
				ID:               stmt.ColumnText(0),
				LocalOwnerID:     stmt.ColumnText(1),
				VillageURL:       stmt.ColumnText(2),
				UserID:           stmt.ColumnText(3),
				UsernameSnapshot: stmt.ColumnText(4),
				ClaimedAt:        stmt.ColumnInt64(5),
				Source:           stmt.ColumnText(6),
			})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("read owner claims: %w; no ownership was changed; retry after repairing local store access", err)
	}
	return claims, nil
}

// OwnershipCounts reports how many local sessions and local annotations carry a
// current owner pointer.
func (s *Store) OwnershipCounts(ctx context.Context) (OwnershipCounts, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return OwnershipCounts{}, fmt.Errorf("read ownership counts: take SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	sessionsTotal, sessionsClaimed, err := countOwnedRows(conn, "sessions")
	if err != nil {
		return OwnershipCounts{}, fmt.Errorf("read ownership counts: sessions: %w", err)
	}
	annotationsTotal, annotationsClaimed, err := countOwnedRows(conn, "annotations")
	if err != nil {
		return OwnershipCounts{}, fmt.Errorf("read ownership counts: annotations: %w", err)
	}
	return OwnershipCounts{
		TotalSessions:        int(sessionsTotal),
		ClaimedSessions:      int(sessionsClaimed),
		UnclaimedSessions:    int(sessionsTotal - sessionsClaimed),
		TotalAnnotations:     int(annotationsTotal),
		ClaimedAnnotations:   int(annotationsClaimed),
		UnclaimedAnnotations: int(annotationsTotal - annotationsClaimed),
	}, nil
}

// ClaimArtifacts appends one claim entry and points local sessions and local
// annotations at it. all=false changes only rows whose pointer is NULL;
// all=true reassigns every local row. Pulled transcripts and pulled annotations
// are foreign and are never read or written by this path.
func (s *Store) ClaimArtifacts(ctx context.Context, snapshot auth.CredentialSnapshot, all bool, source string, nowMs int64) (ClaimResult, error) {
	if err := validateClaimRequest(source, nowMs); err != nil {
		return ClaimResult{}, err
	}
	owner, err := s.EnsureLocalOwner(ctx, nowMs)
	if err != nil {
		return ClaimResult{}, err
	}
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim local artifacts: resolve the signed-in account scope: %w", err)
	}
	username := snapshot.Credentials().Username
	var result ClaimResult
	err = s.withObservationTransaction(ctx, snapshot, true, func(conn *sqlite.Conn) error {
		sessionsTotal, sessionsClaimed, err := countOwnedRows(conn, "sessions")
		if err != nil {
			return err
		}
		annotationsTotal, annotationsClaimed, err := countOwnedRows(conn, "annotations")
		if err != nil {
			return err
		}
		if all {
			result.AlreadyClaimed = 0
			if sessionsTotal+annotationsTotal == 0 {
				return nil
			}
		} else {
			result.AlreadyClaimed = int(sessionsClaimed + annotationsClaimed)
			if sessionsTotal+annotationsTotal == sessionsClaimed+annotationsClaimed {
				return nil
			}
		}
		claimID, err := appendOwnerClaim(conn, owner.ID, base, user, username, nowMs, source)
		if err != nil {
			return err
		}
		if all {
			if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET owner_claim_id = ?`, &sqlitex.ExecOptions{Args: []any{claimID}}); err != nil {
				return fmt.Errorf("claim all local sessions: %w", err)
			}
		} else if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET owner_claim_id = ? WHERE owner_claim_id IS NULL`, &sqlitex.ExecOptions{Args: []any{claimID}}); err != nil {
			return fmt.Errorf("claim unclaimed local sessions: %w", err)
		}
		sessionChanges := conn.Changes()
		if all {
			if err := sqlitex.ExecuteTransient(conn, `UPDATE annotations SET owner_claim_id = ?`, &sqlitex.ExecOptions{Args: []any{claimID}}); err != nil {
				return fmt.Errorf("claim all local annotations: %w", err)
			}
		} else if err := sqlitex.ExecuteTransient(conn, `UPDATE annotations SET owner_claim_id = ? WHERE owner_claim_id IS NULL`, &sqlitex.ExecOptions{Args: []any{claimID}}); err != nil {
			return fmt.Errorf("claim unclaimed local annotations: %w", err)
		}
		annotationChanges := conn.Changes()
		result.ClaimID = claimID
		result.Claimed = int(sessionChanges + annotationChanges)
		return nil
	})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim local artifacts: %w; no claim entry or owner pointer was installed", err)
	}
	return result, nil
}

// ClaimSessionIDs appends at most one claim entry and points only the named
// sessions, plus the local annotations whose target is one of those sessions, at
// it. It is the automatic harvest claim: unrelated unclaimed rows are never
// touched. Rows that already carry a pointer are left alone, so a later account
// cannot silently re-attribute an existing claim.
func (s *Store) ClaimSessionIDs(ctx context.Context, snapshot auth.CredentialSnapshot, sessionIDs []string, source string, nowMs int64) (ClaimResult, error) {
	if err := validateClaimRequest(source, nowMs); err != nil {
		return ClaimResult{}, err
	}
	ids := uniqueNonEmptyStrings(sessionIDs)
	if len(ids) == 0 {
		return ClaimResult{}, nil
	}
	owner, err := s.EnsureLocalOwner(ctx, nowMs)
	if err != nil {
		return ClaimResult{}, err
	}
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim harvest sessions: resolve the signed-in account scope: %w", err)
	}
	username := snapshot.Credentials().Username
	var result ClaimResult
	err = s.withObservationTransaction(ctx, snapshot, true, func(conn *sqlite.Conn) error {
		var sessionsTotal, sessionsClaimed, annotationsTotal, annotationsClaimed int64
		for start := 0; start < len(ids); start += indexFormatReadBatchSize {
			batch := ids[start:min(start+indexFormatReadBatchSize, len(ids))]
			total, claimed, err := countSessionsByID(conn, batch)
			if err != nil {
				return err
			}
			sessionsTotal += total
			sessionsClaimed += claimed
			total, claimed, err = countSessionTargetAnnotations(conn, batch)
			if err != nil {
				return err
			}
			annotationsTotal += total
			annotationsClaimed += claimed
		}
		result.AlreadyClaimed = int(sessionsClaimed + annotationsClaimed)
		if sessionsTotal+annotationsTotal == sessionsClaimed+annotationsClaimed {
			return nil
		}
		claimID, err := appendOwnerClaim(conn, owner.ID, base, user, username, nowMs, source)
		if err != nil {
			return err
		}
		var sessionChanges, annotationChanges int64
		for start := 0; start < len(ids); start += indexFormatReadBatchSize {
			batch := ids[start:min(start+indexFormatReadBatchSize, len(ids))]
			args := append([]any{claimID}, sessionIDArgs(batch)...)
			if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET owner_claim_id = ? WHERE owner_claim_id IS NULL AND session_id IN (`+sqlPlaceholders(len(batch))+`)`, &sqlitex.ExecOptions{Args: args}); err != nil {
				return fmt.Errorf("claim harvest sessions: %w", err)
			}
			sessionChanges += int64(conn.Changes())
			targets, err := json.Marshal(batch)
			if err != nil {
				return fmt.Errorf("claim harvest annotations: encode the session target set: %w", err)
			}
			if err := sqlitex.ExecuteTransient(conn, claimHarvestAnnotationsSQL, &sqlitex.ExecOptions{Args: []any{claimID, string(targets), string(targets), string(targets)}}); err != nil {
				return fmt.Errorf("claim harvest annotations: %w", err)
			}
			annotationChanges += int64(conn.Changes())
		}
		result.ClaimID = claimID
		result.Claimed = int(sessionChanges + annotationChanges)
		return nil
	})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim harvest sessions: %w; no claim entry or owner pointer was installed", err)
	}
	return result, nil
}

// validateClaimRequest rejects an unknown claim source and a non-positive claim
// time before any local state changes.
func validateClaimRequest(source string, nowMs int64) error {
	switch source {
	case ClaimSourceSignInPrompt, ClaimSourceAutomatic, ClaimSourceManual:
	default:
		return fmt.Errorf("claim local artifacts: unknown source %q; no ownership was changed; use %q, %q, or %q", source, ClaimSourceSignInPrompt, ClaimSourceAutomatic, ClaimSourceManual)
	}
	if nowMs <= 0 {
		return fmt.Errorf("claim local artifacts: claimed_at %d is not a positive wall-clock millisecond; no ownership was changed; supply the current Unix time in milliseconds", nowMs)
	}
	return nil
}

// appendOwnerClaim inserts one claim entry. The account scope row is ensured
// idempotently first: claim code creates no observation and advances no
// collective-observation invocation sequence.
func appendOwnerClaim(conn *sqlite.Conn, localOwnerID, villageURL, userID, usernameSnapshot string, claimedAt int64, source string) (string, error) {
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO collective_observation_scopes(village_url, user_id, incarnation)
VALUES(?, ?, ?) ON CONFLICT(village_url, user_id) DO NOTHING`, &sqlitex.ExecOptions{Args: []any{villageURL, userID, uuid.NewString()}}); err != nil {
		return "", fmt.Errorf("ensure the claim account scope: %w", err)
	}
	claimID := "oc_" + uuid.NewString()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO owner_claims(id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source)
VALUES(?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{claimID, localOwnerID, villageURL, userID, usernameSnapshot, claimedAt, source}}); err != nil {
		return "", fmt.Errorf("append the claim entry: %w", err)
	}
	return claimID, nil
}

// countOwnedRows returns the total and currently owned rows of one local
// artifact table. table is a package literal, never caller input.
func countOwnedRows(conn *sqlite.Conn, table string) (total, claimed int64, err error) {
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*), COUNT(owner_claim_id) FROM `+table, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			total, claimed = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
			return nil
		},
	}); err != nil {
		return 0, 0, err
	}
	return total, claimed, nil
}

// countSessionsByID returns the total and currently owned sessions among the
// named ids only.
func countSessionsByID(conn *sqlite.Conn, sessionIDs []string) (total, claimed int64, err error) {
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*), COUNT(owner_claim_id) FROM sessions WHERE session_id IN (`+sqlPlaceholders(len(sessionIDs))+`)`, &sqlitex.ExecOptions{
		Args: sessionIDArgs(sessionIDs),
		ResultFunc: func(stmt *sqlite.Stmt) error {
			total, claimed = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
			return nil
		},
	}); err != nil {
		return 0, 0, err
	}
	return total, claimed, nil
}

// countSessionTargetAnnotations returns the total and currently owned local
// annotations whose explicit target is one of the named sessions.
func countSessionTargetAnnotations(conn *sqlite.Conn, sessionIDs []string) (total, claimed int64, err error) {
	targets, err := json.Marshal(sessionIDs)
	if err != nil {
		return 0, 0, fmt.Errorf("encode the session target set: %w", err)
	}
	if err := sqlitex.ExecuteTransient(conn, countHarvestAnnotationsSQL, &sqlitex.ExecOptions{
		Args: []any{string(targets), string(targets), string(targets)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			total, claimed = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
			return nil
		},
	}); err != nil {
		return 0, 0, err
	}
	return total, claimed, nil
}

// countHarvestAnnotationsSQL counts annotations attached to a harvest session
// set: session-target annotations, entry-target annotations, and association
// targets whose session is in the set. The three json_each arguments each carry
// the same session-id array.
const countHarvestAnnotationsSQL = `SELECT COUNT(*), COUNT(a.owner_claim_id)
FROM annotations a
WHERE a.id IN (
  SELECT annotation_id FROM annotation_target_sessions WHERE session_id IN (SELECT value FROM json_each(?1))
  UNION
  SELECT annotation_id FROM annotation_target_entries WHERE session_id IN (SELECT value FROM json_each(?2))
  UNION
  SELECT annotation_id FROM annotation_target_associations WHERE association_id IN (
    SELECT association_id FROM session_commit_associations WHERE session_id IN (SELECT value FROM json_each(?3))
  )
)`

// claimHarvestAnnotationsSQL sets the current owner pointer on the same
// annotation set countHarvestAnnotationsSQL selects: session targets, entry
// targets, and association targets for the harvest's sessions. Materialized
// annotations outside those targets stay unclaimed and are picked up by an
// explicit `peasant user claim`.
const claimHarvestAnnotationsSQL = `UPDATE annotations SET owner_claim_id = ?1
WHERE owner_claim_id IS NULL
  AND id IN (
    SELECT annotation_id FROM annotation_target_sessions WHERE session_id IN (SELECT value FROM json_each(?2))
    UNION
    SELECT annotation_id FROM annotation_target_entries WHERE session_id IN (SELECT value FROM json_each(?3))
    UNION
    SELECT annotation_id FROM annotation_target_associations WHERE association_id IN (
      SELECT association_id FROM session_commit_associations WHERE session_id IN (SELECT value FROM json_each(?4))
    )
  )`

// uniqueNonEmptyStrings trims, drops empty values, and deduplicates ids while
// preserving their first-seen order.
func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
