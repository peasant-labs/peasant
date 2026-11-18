package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// ClaimSource names why a claim entry was appended. It is the closed set
// enforced by the owner_claims.source CHECK constraint.
type ClaimSource string

const (
	// ClaimSourceSignInPrompt records the claim the sign-in prompt performed.
	ClaimSourceSignInPrompt ClaimSource = "sign_in_prompt"
	// ClaimSourceAutomatic records a best-effort claim by a writing command.
	ClaimSourceAutomatic ClaimSource = "automatic"
	// ClaimSourceManual records an explicit `peasant user claim`.
	ClaimSourceManual ClaimSource = "manual"
)

// AllClaimSources returns the closed claim-source set in contract order. The
// migration CHECK constraint and ClaimSourceSetAreEqual test use it as the one
// definition of the set.
//
// Follow-up: the schema contract now owns the same closed set as
// schema.AllLocalClaimSources. Replace this copy with that type when Peasant
// re-pins schema; it is kept local until then because this PR does not re-pin.
func AllClaimSources() []ClaimSource {
	return []ClaimSource{ClaimSourceSignInPrompt, ClaimSourceAutomatic, ClaimSourceManual}
}

// ParseClaimSource maps a stored or caller-supplied value to a ClaimSource. It
// reports false for anything outside the closed set.
func ParseClaimSource(value string) (ClaimSource, bool) {
	for _, source := range AllClaimSources() {
		if string(source) == value {
			return source, true
		}
	}
	return "", false
}

// LocalOwnerFileName is the owner permanence file beside the analytics
// database. It survives a database rebuild: the database may be deleted and
// re-ingested, but this file keeps the same local owner id for the directory.
//
// The file is a compact JSON object: {"id":"lo_…","createdAt":<unix-ms>}.
// It is written once and adopted when local_owner is empty.
const LocalOwnerFileName = "local-owner.json"

// localOwnerSchemaVersion is the migration that creates local_owner. A
// skip-migrations open below this version has no owner table to bootstrap.
const localOwnerSchemaVersion = 82

// LocalOwner is the permanent owner of one Peasant data directory. It is not a
// Village account, a username, or an email address.
type LocalOwner struct {
	ID        string
	CreatedAt int64
}

type localOwnerFile struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"createdAt"`
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
	Source           ClaimSource
}

// ClaimResult reports one claim operation. Claimed counts artifacts whose
// current owner pointer this operation assigned; AlreadyClaimed counts artifacts
// in scope that already carried a pointer and were left alone. Stored counts
// every local artifact in scope, so an empty store is distinguishable from a
// fully claimed one. An explicit claim always appends an entry, so ClaimID names
// it even when Claimed is zero. An automatic claim reuses its account's newest
// entry only under strict reuse; otherwise it appends a decision entry too, even
// when every artifact it was asked to cover already carries a pointer.
type ClaimResult struct {
	Claimed        int
	AlreadyClaimed int
	Stored         int
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

// LocalOwnerFilePath returns the owner permanence file that sits beside the
// analytics database at dbPath.
func LocalOwnerFilePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), LocalOwnerFileName)
}

// ownerFilePath returns the permanence file for this store, or "" for a store
// built without a database path (a test seam).
func (s *Store) ownerFilePath() string {
	if s.dbPath == "" {
		return ""
	}
	return LocalOwnerFilePath(s.dbPath)
}

// EnsureLocalOwner returns the data directory's single local owner, adopting
// the owner file when local_owner is empty and creating the row when neither
// source has an owner yet. The singleton UNIQUE constraint, the read-back after
// an idempotent insert, and the owner file keep exactly one owner on rebuild.
func (s *Store) EnsureLocalOwner(ctx context.Context, nowMs int64) (LocalOwner, error) {
	if nowMs <= 0 {
		return LocalOwner{}, fmt.Errorf("create local owner: created_at %d is not a positive wall-clock millisecond; no owner was created; supply the current Unix time in milliseconds", nowMs)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return LocalOwner{}, fmt.Errorf("create local owner: take SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	owner, err := bootstrapLocalOwnerOnConn(conn, s.ownerFilePath(), nowMs)
	if err != nil {
		return LocalOwner{}, err
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
	return readLocalOwnerOnConn(conn)
}

// OwnerClaimsPage is one newest-first page of the append-only claim log.
// NextBefore is the id of the last (oldest) returned entry, present exactly when
// an older entry exists. The contract uses it as the next page's cursor.
type OwnerClaimsPage struct {
	Claims     []OwnerClaim
	NextBefore string
}

// ownerClaimsPageSQL is the one page read. The owner and claimed_at index
// supplies the order; rowid is the implicit tie-break SQLite appends to the
// index, so no temporary B-tree is needed. The extra row decides nextBefore.
func ownerClaimsPageSQL(hasBefore bool) string {
	query := `SELECT id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source
FROM owner_claims
WHERE local_owner_id = ?1`
	if hasBefore {
		query += `
  AND (claimed_at, rowid) < (?2, ?3)`
	}
	limitArg := 2
	if hasBefore {
		limitArg = 4
	}
	return query + `
ORDER BY claimed_at DESC, rowid DESC
LIMIT ?` + fmt.Sprint(limitArg)
}

// ownerClaimCursorSQL resolves the cursor id to its (claimed_at, rowid) order
// key, scoped to this local owner.
const ownerClaimCursorSQL = `SELECT claimed_at, rowid FROM owner_claims WHERE local_owner_id = ? AND id = ?`

// newestOwnerClaimSQL reads the newest entry of the whole log for one local
// owner, the only entry strict reuse may reuse.
const newestOwnerClaimSQL = `SELECT id, village_url, user_id, source FROM owner_claims
WHERE local_owner_id = ?
ORDER BY claimed_at DESC, rowid DESC LIMIT 1`

// OwnerClaims returns the most recent page of the append-only claim log for
// this local owner, newest first: claimed_at descending, then insertion order
// (rowid) descending. A non-empty before continues after that entry. The read
// fetches the bound plus one row to decide whether an older page exists, and it
// is served by the (local_owner_id, claimed_at) index with no temporary sort.
func (s *Store) OwnerClaims(ctx context.Context, before string) (OwnerClaimsPage, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return OwnerClaimsPage{}, fmt.Errorf("read owner claims: take SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	owner, err := readLocalOwnerOnConn(conn)
	if err != nil {
		return OwnerClaimsPage{}, err
	}
	if owner == nil {
		return OwnerClaimsPage{}, nil
	}
	query := ownerClaimsPageSQL(before != "")
	args := []any{owner.ID}
	if before != "" {
		var claimedAt, rowid int64
		found := false
		if err := sqlitex.ExecuteTransient(conn, ownerClaimCursorSQL, &sqlitex.ExecOptions{
			Args: []any{owner.ID, before},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				claimedAt, rowid = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
				found = true
				return nil
			},
		}); err != nil {
			return OwnerClaimsPage{}, fmt.Errorf("read owner claims: resolve the before cursor %q: %w; no ownership was changed; retry after repairing local store access", before, err)
		}
		if !found {
			return OwnerClaimsPage{}, fmt.Errorf("read owner claims: the before cursor %q names no claim entry for this local owner; no ownership was changed; pass a nextBefore value from a previous page", before)
		}
		args = append(args, claimedAt, rowid)
	}
	args = append(args, OwnerClaimsPageSize+1)
	var claims []OwnerClaim
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			claims = append(claims, OwnerClaim{
				ID:               stmt.ColumnText(0),
				LocalOwnerID:     stmt.ColumnText(1),
				VillageURL:       stmt.ColumnText(2),
				UserID:           stmt.ColumnText(3),
				UsernameSnapshot: stmt.ColumnText(4),
				ClaimedAt:        stmt.ColumnInt64(5),
				Source:           ClaimSource(stmt.ColumnText(6)),
			})
			return nil
		},
	}); err != nil {
		return OwnerClaimsPage{}, fmt.Errorf("read owner claims: %w; no ownership was changed; retry after repairing local store access", err)
	}
	page := OwnerClaimsPage{Claims: claims}
	if len(claims) > OwnerClaimsPageSize {
		page.Claims = claims[:OwnerClaimsPageSize]
		page.NextBefore = page.Claims[len(page.Claims)-1].ID
	}
	return page, nil
}

// OwnerClaimsPageSize bounds one claim-log read. The contract exposes the most
// recent 100 entries; older entries remain in the append-only table.
const OwnerClaimsPageSize = 100

// OwnerClaimCount returns the full claim-log size, including entries older than
// one OwnerClaims page.
func (s *Store) OwnerClaimCount(ctx context.Context) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("count owner claims: take SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	owner, err := readLocalOwnerOnConn(conn)
	if err != nil {
		return 0, err
	}
	if owner == nil {
		return 0, nil
	}
	var count int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM owner_claims WHERE local_owner_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{owner.ID},
		ResultFunc: func(stmt *sqlite.Stmt) error { count = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		return 0, fmt.Errorf("count owner claims: %w; no ownership was changed; retry after repairing local store access", err)
	}
	return count, nil
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

// claimUpdateBatchSize bounds the rows one claim-pointer transaction touches.
// Each batch commits on its own, so an interrupted large claim resumes from the
// rows that are still NULL (default) or still point at another claim (--all).
const claimUpdateBatchSize = 5000

// ClaimArtifacts appends or reuses one claim entry and points local sessions
// and local annotations at it. all=false changes only rows whose pointer is
// NULL; all=true reassigns every local row. Pointer updates run in bounded
// transactions after the entry is durable, so a large store never needs one
// transaction over every row. Pulled transcripts and pulled annotations are
// foreign and are never read or written by this path.
func (s *Store) ClaimArtifacts(ctx context.Context, snapshot auth.CredentialSnapshot, all bool, source ClaimSource, nowMs int64) (ClaimResult, error) {
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
		result.Stored = int(sessionsTotal + annotationsTotal)
		if all {
			result.AlreadyClaimed = 0
		} else {
			result.AlreadyClaimed = int(sessionsClaimed + annotationsClaimed)
		}
		// An explicit claim always appends its decision, even on an empty store
		// or when every artifact already carries a pointer.
		claimID, err := appendOrReuseOwnerClaim(conn, owner.ID, base, user, username, nowMs, source)
		if err != nil {
			return err
		}
		result.ClaimID = claimID
		return nil
	})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim local artifacts: %w; no claim entry or owner pointer was installed", err)
	}
	if result.ClaimID == "" {
		return result, nil
	}
	for _, table := range []string{"sessions", "annotations"} {
		changed, err := s.updateClaimPointersInBatches(ctx, snapshot, table, result.ClaimID, all)
		if err != nil {
			return ClaimResult{}, err
		}
		result.Claimed += changed
	}
	return result, nil
}

// updateClaimPointersInBatches moves one local artifact table's owner pointers
// in bounded transactions. all=false touches only NULL pointers; all=true
// touches every row that does not already point at claimID. An interrupted run
// leaves earlier batches committed and the next run resumes the rest.
func (s *Store) updateClaimPointersInBatches(ctx context.Context, snapshot auth.CredentialSnapshot, table, claimID string, all bool) (int, error) {
	total := 0
	batchSize := s.claimBatchSize
	if batchSize <= 0 {
		batchSize = claimUpdateBatchSize
	}
	for batch := 0; ; batch++ {
		changed, err := s.updateClaimPointerBatch(ctx, snapshot, table, claimID, all, batchSize)
		if err != nil {
			return total, fmt.Errorf("claim %s pointers: %w; the claim entry and earlier batches are kept, and a retry resumes the remaining rows", table, err)
		}
		total += changed
		if seam := s.claimBatchSeam; seam != nil {
			if err := seam(table, batch, changed); err != nil {
				return total, fmt.Errorf("claim %s pointers interrupted after batch %d: %w; the claim entry and earlier batches are kept, and a retry resumes the remaining rows", table, batch, err)
			}
		}
		if changed == 0 || changed < batchSize {
			return total, nil
		}
	}
}

// updateClaimPointerBatch commits one bounded pointer update and returns its
// changed-row count. The table name is a package literal, never caller input.
func (s *Store) updateClaimPointerBatch(ctx context.Context, snapshot auth.CredentialSnapshot, table, claimID string, all bool, batchSize int) (int, error) {
	var changed int
	err := s.withObservationTransaction(ctx, snapshot, true, func(conn *sqlite.Conn) error {
		query := `UPDATE ` + table + ` SET owner_claim_id = ?1
WHERE rowid IN (
  SELECT rowid FROM ` + table + ` WHERE owner_claim_id IS NULL ORDER BY rowid LIMIT ?2
)`
		args := []any{claimID, batchSize}
		if all {
			query = `UPDATE ` + table + ` SET owner_claim_id = ?1
WHERE rowid IN (
  SELECT rowid FROM ` + table + ` WHERE owner_claim_id IS NOT ?1 ORDER BY rowid LIMIT ?2
)`
			args = []any{claimID, batchSize}
		}
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			return err
		}
		changed = int(conn.Changes())
		return nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}

// ClaimSessionIDs appends or reuses one claim entry and points only the named
// sessions, plus the local annotations whose target is one of those sessions, at
// it. It is the automatic harvest claim: unrelated unclaimed rows are never
// touched. Rows that already carry a pointer are left alone, so a later account
// cannot silently re-attribute an existing claim.
func (s *Store) ClaimSessionIDs(ctx context.Context, snapshot auth.CredentialSnapshot, sessionIDs []string, source ClaimSource, nowMs int64) (ClaimResult, error) {
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
		result.Stored = int(sessionsTotal + annotationsTotal)
		result.AlreadyClaimed = int(sessionsClaimed + annotationsClaimed)
		claimID, err := appendOrReuseOwnerClaim(conn, owner.ID, base, user, username, nowMs, source)
		if err != nil {
			return err
		}
		result.ClaimID = claimID
		return nil
	})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim harvest sessions: %w; no claim entry or owner pointer was installed", err)
	}
	if result.ClaimID == "" {
		return result, nil
	}
	for start := 0; start < len(ids); start += indexFormatReadBatchSize {
		batch := ids[start:min(start+indexFormatReadBatchSize, len(ids))]
		var sessionChanges, annotationChanges int
		err := s.withObservationTransaction(ctx, snapshot, true, func(conn *sqlite.Conn) error {
			args := append([]any{result.ClaimID}, sessionIDArgs(batch)...)
			if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET owner_claim_id = ? WHERE owner_claim_id IS NULL AND session_id IN (`+sqlPlaceholders(len(batch))+`)`, &sqlitex.ExecOptions{Args: args}); err != nil {
				return fmt.Errorf("claim harvest sessions: %w", err)
			}
			sessionChanges = int(conn.Changes())
			targets, err := json.Marshal(batch)
			if err != nil {
				return fmt.Errorf("claim harvest annotations: encode the session target set: %w", err)
			}
			if err := sqlitex.ExecuteTransient(conn, claimHarvestAnnotationsSQL, &sqlitex.ExecOptions{Args: []any{result.ClaimID, string(targets), string(targets), string(targets)}}); err != nil {
				return fmt.Errorf("claim harvest annotations: %w", err)
			}
			annotationChanges = int(conn.Changes())
			return nil
		})
		if err != nil {
			return ClaimResult{}, fmt.Errorf("claim harvest sessions: %w; the claim entry and earlier batches are kept, and a retry resumes the remaining rows", err)
		}
		result.Claimed += sessionChanges + annotationChanges
	}
	return result, nil
}

// ClaimAnnotationIDs points just-created local annotations at the signed-in
// account's latest claim entry, appending one only when the account has no entry
// yet. It is the annotate/import/web claim path: one annotation does not append
// a log entry per artifact. Rows that already carry a pointer are left alone.
func (s *Store) ClaimAnnotationIDs(ctx context.Context, snapshot auth.CredentialSnapshot, annotationIDs []string, source ClaimSource, nowMs int64) (ClaimResult, error) {
	if err := validateClaimRequest(source, nowMs); err != nil {
		return ClaimResult{}, err
	}
	ids := uniqueNonEmptyStrings(annotationIDs)
	if len(ids) == 0 {
		return ClaimResult{}, nil
	}
	owner, err := s.EnsureLocalOwner(ctx, nowMs)
	if err != nil {
		return ClaimResult{}, err
	}
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim local annotations: resolve the signed-in account scope: %w", err)
	}
	username := snapshot.Credentials().Username
	var result ClaimResult
	err = s.withObservationTransaction(ctx, snapshot, true, func(conn *sqlite.Conn) error {
		var total, claimed int64
		for start := 0; start < len(ids); start += indexFormatReadBatchSize {
			batch := ids[start:min(start+indexFormatReadBatchSize, len(ids))]
			if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*), COUNT(owner_claim_id) FROM annotations WHERE id IN (`+sqlPlaceholders(len(batch))+`)`, &sqlitex.ExecOptions{
				Args: sessionIDArgs(batch),
				ResultFunc: func(stmt *sqlite.Stmt) error {
					total += stmt.ColumnInt64(0)
					claimed += stmt.ColumnInt64(1)
					return nil
				},
			}); err != nil {
				return err
			}
		}
		result.Stored = int(total)
		result.AlreadyClaimed = int(claimed)
		claimID, err := appendOrReuseOwnerClaim(conn, owner.ID, base, user, username, nowMs, source)
		if err != nil {
			return err
		}
		result.ClaimID = claimID
		changes := 0
		for start := 0; start < len(ids); start += indexFormatReadBatchSize {
			batch := ids[start:min(start+indexFormatReadBatchSize, len(ids))]
			args := append([]any{claimID}, sessionIDArgs(batch)...)
			if err := sqlitex.ExecuteTransient(conn, `UPDATE annotations SET owner_claim_id = ? WHERE owner_claim_id IS NULL AND id IN (`+sqlPlaceholders(len(batch))+`)`, &sqlitex.ExecOptions{Args: args}); err != nil {
				return err
			}
			changes += int(conn.Changes())
		}
		result.Claimed = changes
		return nil
	})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim local annotations: %w; no claim entry or owner pointer was installed", err)
	}
	return result, nil
}

// validateClaimRequest rejects an unknown claim source and a non-positive claim
// time before any local state changes.
func validateClaimRequest(source ClaimSource, nowMs int64) error {
	if _, ok := ParseClaimSource(string(source)); !ok {
		return fmt.Errorf("claim local artifacts: unknown source %q; no ownership was changed; use %q, %q, or %q", source, ClaimSourceSignInPrompt, ClaimSourceAutomatic, ClaimSourceManual)
	}
	if nowMs <= 0 {
		return fmt.Errorf("claim local artifacts: claimed_at %d is not a positive wall-clock millisecond; no ownership was changed; supply the current Unix time in milliseconds", nowMs)
	}
	return nil
}

// appendOrReuseOwnerClaim inserts one claim entry for an explicit claim, or
// reuses an automatic entry only under strict reuse: the newest entry of the
// whole log must be this account's automatic entry. Otherwise it appends, so
// replaying the log in insertion order always names the account that receives
// new artifacts. The account scope row is ensured idempotently first: claim code
// creates no observation and advances no collective-observation invocation
// sequence.
func appendOrReuseOwnerClaim(conn *sqlite.Conn, localOwnerID, villageURL, userID, usernameSnapshot string, claimedAt int64, source ClaimSource) (string, error) {
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO collective_observation_scopes(village_url, user_id, incarnation)
VALUES(?, ?, ?) ON CONFLICT(village_url, user_id) DO NOTHING`, &sqlitex.ExecOptions{Args: []any{villageURL, userID, uuid.NewString()}}); err != nil {
		return "", fmt.Errorf("ensure the claim account scope: %w", err)
	}
	if source == ClaimSourceAutomatic {
		// Strict reuse: only the newest entry of the whole log for this local
		// owner decides. Reuse it when it is this account's automatic entry;
		// otherwise append, so replaying the log in insertion order always names
		// the account that receives new artifacts.
		var existingID, existingURL, existingUser string
		var existingSource ClaimSource
		if err := sqlitex.ExecuteTransient(conn, newestOwnerClaimSQL, &sqlitex.ExecOptions{
			Args: []any{localOwnerID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				existingID = stmt.ColumnText(0)
				existingURL = stmt.ColumnText(1)
				existingUser = stmt.ColumnText(2)
				existingSource = ClaimSource(stmt.ColumnText(3))
				return nil
			},
		}); err != nil {
			return "", fmt.Errorf("find the newest claim entry: %w", err)
		}
		if existingID != "" && existingSource == ClaimSourceAutomatic && existingURL == villageURL && existingUser == userID {
			return existingID, nil
		}
	}
	claimID := "oc_" + uuid.NewString()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO owner_claims(id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source)
VALUES(?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{claimID, localOwnerID, villageURL, userID, usernameSnapshot, claimedAt, string(source)}}); err != nil {
		return "", fmt.Errorf("append the claim entry: %w", err)
	}
	return claimID, nil
}

// readLocalOwnerOnConn reads the singleton row, or nil when it is absent.
func readLocalOwnerOnConn(conn *sqlite.Conn) (*LocalOwner, error) {
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

// bootstrapLocalOwnerOnConn adopts the owner file when local_owner is empty,
// creates the row when neither source has an owner, and ensures the file exists
// beside the database. The file is the permanence record across a rebuild.
func bootstrapLocalOwnerOnConn(conn *sqlite.Conn, ownerFilePath string, nowMs int64) (LocalOwner, error) {
	owner, err := readLocalOwnerOnConn(conn)
	if err != nil {
		return LocalOwner{}, err
	}
	if owner == nil {
		fromFile, fileErr := readLocalOwnerFile(ownerFilePath)
		if fileErr != nil {
			return LocalOwner{}, fileErr
		}
		createdAt, id := nowMs, "lo_"+uuid.NewString()
		if fromFile != nil {
			id, createdAt = fromFile.ID, fromFile.CreatedAt
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO local_owner(id, singleton, created_at)
SELECT ?, 1, ? WHERE NOT EXISTS (SELECT 1 FROM local_owner)`, &sqlitex.ExecOptions{
			Args: []any{id, createdAt},
		}); err != nil {
			return LocalOwner{}, fmt.Errorf("create local owner: insert the singleton row: %w; no owner was created; retry after repairing local store access", err)
		}
		owner, err = readLocalOwnerOnConn(conn)
		if err != nil {
			return LocalOwner{}, err
		}
		if owner == nil {
			return LocalOwner{}, fmt.Errorf("create local owner: the singleton row is missing after creation; no owner was returned; reopen the store and retry")
		}
	}
	if err := ensureLocalOwnerFile(ownerFilePath, *owner); err != nil {
		return LocalOwner{}, err
	}
	return *owner, nil
}

// readLocalOwnerFile reads the owner permanence file. A missing file is not an
// error; a malformed one is, because silently minting a new owner would break
// the permanence the file exists to provide.
func readLocalOwnerFile(path string) (*LocalOwner, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the local owner file %q: %w; no owner was returned; fix the data-directory permissions and retry", path, err)
	}
	var file localOwnerFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse the local owner file %q: %w; no owner was returned; repair the file from a backup or remove it to mint a new owner", path, err)
	}
	if strings.TrimSpace(file.ID) == "" || len(file.ID) > 64 || file.CreatedAt <= 0 {
		return nil, fmt.Errorf("parse the local owner file %q: invalid id or createdAt; no owner was returned; repair the file from a backup or remove it to mint a new owner", path)
	}
	return &LocalOwner{ID: file.ID, CreatedAt: file.CreatedAt}, nil
}

// ensureLocalOwnerFile writes the owner file once. An existing file is never
// replaced, so a later database rebuild can still adopt the original id. A file
// that names a different owner is refused with both ids: adopting either side
// silently would make the next rebuild switch owners.
func ensureLocalOwnerFile(path string, owner LocalOwner) error {
	if path == "" {
		return nil
	}
	existing, err := readLocalOwnerFile(path)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.ID != owner.ID {
			return fmt.Errorf("refuse to open: the local owner file %q names owner %q, but the database names owner %q; the two id sources disagree, so a rebuild would silently switch owners; repair it by moving the wrong source aside, or restore the matching file and database from one backup, then reopen", path, existing.ID, owner.ID)
		}
		return nil
	}
	data, err := json.MarshalIndent(localOwnerFile{ID: owner.ID, CreatedAt: owner.CreatedAt}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the local owner file %q: %w; the owner row was kept", path, err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".local-owner-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create a temporary local owner file beside %q: %w; the owner row was kept; fix directory permissions or free space and retry", path, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set private permissions on the local owner file %q: %w; the destination was not changed", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write the local owner file %q: %w; the owner row was kept; free disk space and retry", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync the local owner file %q: %w; the owner row was kept; repair the filesystem and retry", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close the local owner file %q: %w; the owner row was kept; repair the filesystem and retry", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish the local owner file %q: %w; the owner row was kept; fix destination permissions and retry", path, err)
	}
	committed = true
	return nil
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
