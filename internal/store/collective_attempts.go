package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// ErrObservationSuperseded means a newer invocation installed the same source.
// It never means all other observation domains or sources became obsolete.
var ErrObservationSuperseded = errors.New("collective observation source was superseded by a newer invocation; retained evidence was not overwritten; fetch the source again to inspect its current observation")

// ContributionAttemptConflictError classifies canonical remote-data conflict,
// not SQLite failure. The shared schema helper remains the transition rule.
type ContributionAttemptConflictError struct{ Err error }

func (e *ContributionAttemptConflictError) Error() string { return e.Err.Error() }
func (e *ContributionAttemptConflictError) Unwrap() error { return e.Err }

// CollectiveObservationInvocation pins local credentials, scope incarnation and
// a durable sequence. Its fields are private: callers cannot forge a different
// account scope or substitute a sequence from another Store.
type CollectiveObservationInvocation struct {
	store       *Store
	credentials auth.CredentialSnapshot
	villageURL  string
	userID      string
	incarnation string
	sequence    int64
}

// ContributionAttemptSource is one completely read owned pair. Nil Events is
// not successful empty coverage. Known unavailability must not call this path.
// This is an internal staging value, not a new wire or transition contract.
type ContributionAttemptSource struct {
	CollectiveID schema.VillageUUID
	TranscriptID schema.TranscriptID
	Events       []schema.VillageShareEvent
	ObservedAt   time.Time
}

// ContributionAttemptInstall reports the local atomic effect only. It cannot
// claim a complete catalog/history fetch or grant remote publication authority.
type ContributionAttemptInstall struct {
	Changed    bool
	Generation int64
}

func checkObservationInvocation(conn *sqlite.Conn, invocation CollectiveObservationInvocation) error {
	var incarnation string
	if err := sqlitex.Execute(conn, `SELECT incarnation FROM collective_observation_scopes WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID}, ResultFunc: func(stmt *sqlite.Stmt) error { incarnation = stmt.ColumnText(0); return nil }}); err != nil {
		return err
	}
	if incarnation != invocation.incarnation {
		return ErrObservationSuperseded
	}
	return nil
}

func observationCredentialScope(snapshot auth.CredentialSnapshot) (string, string, error) {
	creds := snapshot.Credentials()
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return "", "", err
	}
	// The contract owns shared scope identity; the auth boundary above retains
	// Peasant's stricter transport/development exceptions before normalization.
	scope, err := schema.NormalizeCollectiveObservationScope(base, schema.VillageUUID(creds.UserID))
	if err != nil {
		return "", "", err
	}
	return scope.VillageURL, scope.UserID.String(), nil
}

func (s *Store) withObservationTransaction(ctx context.Context, snapshot auth.CredentialSnapshot, write bool, action func(*sqlite.Conn) error) error {
	return snapshot.WithCurrent(ctx, func() (err error) {
		if write {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, s.writeConfig.WithDefaults(1).HoldTarget)
			defer cancel()
		}
		conn, err := s.pool.Take(ctx)
		if err != nil {
			return fmt.Errorf("open collective observation transaction: %w; no cache changes committed; retry after restoring local Store access", err)
		}
		defer s.pool.Put(conn)
		if write {
			end, beginErr := sqlitex.ImmediateTransaction(conn)
			if beginErr != nil {
				return beginErr
			}
			defer end(&err)
		} else {
			defer sqlitex.Transaction(conn)(&err)
		}
		err = action(conn)
		return err
	})
}

// BeginCollectiveObservation allocates a durable ordering sequence under the
// credential guard. Online authentication belongs to the service BEFORE this
// call; there is no network work inside the credential or SQLite write lock.
func (s *Store) BeginCollectiveObservation(ctx context.Context, snapshot auth.CredentialSnapshot) (CollectiveObservationInvocation, error) {
	return s.beginCollectiveObservation(ctx, snapshot, nil)
}

func (s *Store) beginCollectiveObservation(ctx context.Context, snapshot auth.CredentialSnapshot, issue func(*sqlite.Conn, CollectiveObservationInvocation) error) (CollectiveObservationInvocation, error) {
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return CollectiveObservationInvocation{}, err
	}
	incarnation, err := uuid.NewRandom()
	if err != nil {
		return CollectiveObservationInvocation{}, fmt.Errorf("allocate collective scope incarnation: %w; no scope changed; restore system randomness and retry", err)
	}
	result := CollectiveObservationInvocation{store: s, credentials: snapshot, villageURL: base, userID: user}
	err = s.withObservationTransaction(ctx, snapshot, true, func(conn *sqlite.Conn) error {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO collective_observation_scopes(village_url,user_id,incarnation) VALUES(?,?,?) ON CONFLICT(village_url,user_id) DO NOTHING`, &sqlitex.ExecOptions{Args: []any{base, user, incarnation.String()}}); err != nil {
			return err
		}
		if err := sqlitex.ExecuteTransient(conn, `UPDATE collective_observation_scopes SET invocation_seq=invocation_seq+1 WHERE village_url=? AND user_id=? AND invocation_seq < ?`, &sqlitex.ExecOptions{Args: []any{base, user, int64(math.MaxInt64)}}); err != nil {
			return err
		}
		if conn.Changes() != 1 {
			return fmt.Errorf("allocate collective observation sequence: local sequence exhausted; no invocation allocated; recreate the selected cache before retrying")
		}
		if err := sqlitex.ExecuteTransient(conn, `SELECT incarnation,invocation_seq FROM collective_observation_scopes WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{base, user}, ResultFunc: func(stmt *sqlite.Stmt) error {
			result.incarnation, result.sequence = stmt.ColumnText(0), stmt.ColumnInt64(1)
			return nil
		}}); err != nil {
			return err
		}
		if issue != nil {
			return issue(conn, result)
		}
		return nil
	})
	if err != nil {
		return CollectiveObservationInvocation{}, err
	}
	return result, nil
}

// InstallContributionAttemptSources stages validation outside the writer, then
// atomically reconciles ALL provided complete sources against latest stored
// attempts. No missing discovery/pair is interpreted as deletion of history.
// A stale overlapping source rejects this install; disjoint sources do not.
func (s *Store) InstallContributionAttemptSources(ctx context.Context, invocation CollectiveObservationInvocation, sources []ContributionAttemptSource) (ContributionAttemptInstall, error) {
	return s.installContributionAttemptSources(ctx, invocation, sources, nil, nil)
}

func (s *Store) installContributionAttemptSources(ctx context.Context, invocation CollectiveObservationInvocation, sources []ContributionAttemptSource, before func(*sqlite.Conn) error, after func(*sqlite.Conn, ContributionAttemptInstall) error) (ContributionAttemptInstall, error) {
	if invocation.store != s || invocation.sequence < 1 {
		return ContributionAttemptInstall{}, fmt.Errorf("install contribution attempts: invocation does not belong to this Store; no evidence changed; begin an authenticated observation in this Store")
	}
	if sources == nil {
		return ContributionAttemptInstall{}, fmt.Errorf("install contribution attempts: staged sources are absent, not a complete empty set; no evidence changed; stage complete source responses before retrying")
	}
	seenSources := make(map[string]bool, len(sources))
	for _, source := range sources {
		if source.Events == nil || source.ObservedAt.IsZero() {
			return ContributionAttemptInstall{}, fmt.Errorf("install contribution attempts: source lacks a complete array or observation time; retained history is unchanged; fetch the complete source before retrying")
		}
		key := source.CollectiveID.String() + "/" + source.TranscriptID.String()
		if seenSources[key] {
			return ContributionAttemptInstall{}, fmt.Errorf("install contribution attempts: a pair is repeated in staged sources; no evidence changed; stage each complete pair exactly once")
		}
		seenSources[key] = true
		ordinals := make(map[int32]bool, len(source.Events))
		collective, err := uuid.Parse(source.CollectiveID.String())
		if err != nil || collective.String() != source.CollectiveID.String() {
			return ContributionAttemptInstall{}, fmt.Errorf("install contribution attempts: invalid collective ID; no evidence changed; stage canonical Village UUIDs")
		}
		if _, err := schema.NewTranscriptID(source.TranscriptID.String()); err != nil {
			return ContributionAttemptInstall{}, err
		}
		_, offset := source.ObservedAt.Zone()
		if offset != 0 {
			return ContributionAttemptInstall{}, fmt.Errorf("install contribution attempts: observation time is not UTC; no evidence changed; stage the source with its UTC observation time")
		}
		for _, event := range source.Events {
			if err := event.Validate(); err != nil {
				return ContributionAttemptInstall{}, err
			}
			if ordinals[event.EventNum] {
				return ContributionAttemptInstall{}, fmt.Errorf("install contribution attempts: repeated attempt ordinal; no evidence changed; obtain a complete non-repeated pair response")
			}
			ordinals[event.EventNum] = true
		}
	}
	var result ContributionAttemptInstall
	err := s.withObservationTransaction(ctx, invocation.credentials, true, func(conn *sqlite.Conn) error {
		if before != nil {
			if err := before(conn); err != nil {
				return err
			}
		}
		var incarnation string
		if err := sqlitex.ExecuteTransient(conn, `SELECT incarnation,attempt_generation FROM collective_observation_scopes WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID}, ResultFunc: func(stmt *sqlite.Stmt) error {
			incarnation = stmt.ColumnText(0)
			result.Generation = stmt.ColumnInt64(1)
			return nil
		}}); err != nil {
			return err
		}
		if incarnation != invocation.incarnation {
			return ErrObservationSuperseded
		}
		for _, source := range sources {
			if err := checkCollectiveSourceSequence(conn, invocation, schema.CollectiveObservationSourceAttempts, ContributionAttemptReadSource(source.CollectiveID, source.TranscriptID).ID); err != nil {
				return err
			}
			var installed int64
			args := []any{invocation.villageURL, invocation.userID, source.CollectiveID.String(), source.TranscriptID.String()}
			if err := sqlitex.Execute(conn, `SELECT installed_seq FROM collective_attempt_sources WHERE village_url=? AND user_id=? AND collective_id=? AND transcript_id=?`, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error { installed = stmt.ColumnInt64(0); return nil }}); err != nil {
				return err
			}
			if installed > invocation.sequence {
				return ErrObservationSuperseded
			}
			for _, event := range source.Events {
				old, err := readContributionAttempt(conn, invocation.villageURL, invocation.userID, source.CollectiveID, source.TranscriptID, event.EventNum)
				if err != nil {
					return err
				}
				incoming := schema.LocalRemoteContributionAttempt{CollectiveID: source.CollectiveID, TranscriptID: source.TranscriptID, Attempt: event, FirstSeenAt: source.ObservedAt}
				retained, effect, err := schema.ReconcileVillageContributionAttempt(old, incoming)
				if err != nil {
					return &ContributionAttemptConflictError{Err: err}
				}
				if effect == schema.VillageContributionAttemptInserted || effect == schema.VillageContributionAttemptUpdated {
					result.Changed = true
				}
				var decided any
				if retained.Attempt.DecidedAt != nil {
					decided = retained.Attempt.DecidedAt.Format(time.RFC3339Nano)
				}
				writeArgs := append(append([]any{}, args...), event.EventNum, string(retained.Attempt.Status), retained.Attempt.RecordedAt.Format(time.RFC3339Nano), decided, string(retained.Attempt.DecidedByActor), retained.FirstSeenAt.Format(time.RFC3339Nano), source.ObservedAt.Format(time.RFC3339Nano))
				if err := sqlitex.Execute(conn, `INSERT INTO remote_contribution_attempts(village_url,user_id,collective_id,transcript_id,event_num,status,recorded_at,decided_at,decided_by_actor,first_seen_at,last_validated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(village_url,user_id,collective_id,transcript_id,event_num) DO UPDATE SET status=excluded.status,recorded_at=excluded.recorded_at,decided_at=excluded.decided_at,decided_by_actor=excluded.decided_by_actor,first_seen_at=excluded.first_seen_at,last_validated_at=excluded.last_validated_at`, &sqlitex.ExecOptions{Args: writeArgs}); err != nil {
					return err
				}
			}
			if err := sqlitex.Execute(conn, `INSERT INTO collective_attempt_sources(village_url,user_id,collective_id,transcript_id,installed_seq,observed_at) VALUES(?,?,?,?,?,?) ON CONFLICT(village_url,user_id,collective_id,transcript_id) DO UPDATE SET installed_seq=excluded.installed_seq,observed_at=excluded.observed_at`, &sqlitex.ExecOptions{Args: append(append([]any{}, args...), invocation.sequence, source.ObservedAt.Format(time.RFC3339Nano))}); err != nil {
				return err
			}
			if err := markCollectiveSource(conn, invocation, schema.CollectiveObservationSourceAttempts, ContributionAttemptReadSource(source.CollectiveID, source.TranscriptID).ID); err != nil {
				return err
			}
		}
		if result.Changed {
			if result.Generation == math.MaxInt64 {
				return fmt.Errorf("install contribution attempts: category generation exhausted; no evidence changed; recreate the selected cache before retrying")
			}
			if err := bumpEvidence(conn, invocation.villageURL, invocation.userID, schema.LocalContributionEvidenceRemoteAttempts); err != nil {
				return err
			}
			result.Generation++
		}
		if after != nil {
			return after(conn, result)
		}
		return nil
	})
	if err != nil {
		return ContributionAttemptInstall{}, err
	}
	return result, nil
}

// ReadContributionAttempt is a selected-scope, single-identity read. It exposes
// no traversal token or full-history completion claim; bounded category browsing
// is a separate reader using the schema-owned token codec.
func (s *Store) ReadContributionAttempt(ctx context.Context, snapshot auth.CredentialSnapshot, collective schema.VillageUUID, transcript schema.TranscriptID, ordinal int32) (*schema.LocalRemoteContributionAttempt, error) {
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return nil, err
	}
	var result *schema.LocalRemoteContributionAttempt
	err = s.withObservationTransaction(ctx, snapshot, false, func(conn *sqlite.Conn) error {
		var err error
		result, err = readContributionAttempt(conn, base, user, collective, transcript, ordinal)
		return err
	})
	return result, err
}

func readContributionAttempt(conn *sqlite.Conn, base, user string, collective schema.VillageUUID, transcript schema.TranscriptID, ordinal int32) (*schema.LocalRemoteContributionAttempt, error) {
	var result *schema.LocalRemoteContributionAttempt
	err := sqlitex.Execute(conn, `SELECT status,recorded_at,decided_at,decided_by_actor,first_seen_at FROM remote_contribution_attempts WHERE village_url=? AND user_id=? AND collective_id=? AND transcript_id=? AND event_num=?`, &sqlitex.ExecOptions{Args: []any{base, user, collective.String(), transcript.String(), ordinal}, ResultFunc: func(stmt *sqlite.Stmt) error {
		var err error
		value := schema.LocalRemoteContributionAttempt{CollectiveID: collective, TranscriptID: transcript, Attempt: schema.VillageShareEvent{EventNum: ordinal}}
		// These closed enum values were validated on insertion and constrained by
		// SQLite; Validate below refuses corrupted persisted evidence on read.
		for _, status := range schema.AllVillageShareStatuses {
			if string(status) == stmt.ColumnText(0) {
				value.Attempt.Status = status
				break
			}
		}
		value.Attempt.RecordedAt, err = time.Parse(time.RFC3339Nano, stmt.ColumnText(1))
		if err != nil {
			return err
		}
		if stmt.ColumnType(2) != sqlite.TypeNull {
			decided, err := time.Parse(time.RFC3339Nano, stmt.ColumnText(2))
			if err != nil {
				return err
			}
			value.Attempt.DecidedAt = &decided
		}
		actorFound := false
		for _, actor := range schema.AllVillageShareEventActors {
			if string(actor) == stmt.ColumnText(3) {
				value.Attempt.DecidedByActor = actor
				actorFound = true
				break
			}
		}
		if !actorFound {
			return fmt.Errorf("read contribution attempt: stored actor is outside the schema menu; retained evidence cannot be rendered; repair the selected cache from validated history")
		}
		value.FirstSeenAt, err = time.Parse(time.RFC3339Nano, stmt.ColumnText(4))
		if err != nil {
			return err
		}
		if err := value.Validate(); err != nil {
			return err
		}
		result = &value
		return nil
	}})
	return result, err
}
