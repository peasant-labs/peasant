package store

import (
	"context"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TranscriptCollectiveObservation is the private row-level adapter input. An
// absent Metadata means never observed, not successful empty disclosure.
type TranscriptCollectiveObservation struct {
	TranscriptID schema.TranscriptID
	Metadata     *schema.PullAssociationMetadataResult
	Collectives  []schema.VillageTranscriptCollective
}

// InstallTranscriptAssociations installs ONE complete viewer-disclosed set.
// Known negative disclosure invalidates only this transcript/account scope.
// Earlier payload versions remain private dated evidence, never current matches.
func (s *Store) InstallTranscriptAssociations(ctx context.Context, invocation CollectiveObservationInvocation, id schema.TranscriptID, rows []schema.VillageTranscriptCollective, observedAt time.Time, invalidated bool) error {
	if invocation.store != s || invocation.sequence < 1 {
		return fmt.Errorf("install transcript associations: invocation belongs to another Store; no observation changed; begin a selected-account observation in this Store")
	}
	if _, err := schema.NewTranscriptID(id.String()); err != nil {
		return err
	}
	if rows == nil || len(rows) > schema.MaxCollectiveFetchItems {
		return fmt.Errorf("install transcript associations: disclosed set is missing or oversized; prior evidence remains dated; obtain a complete bounded array before retrying")
	}
	if invalidated && len(rows) != 0 {
		return fmt.Errorf("install transcript associations: negative disclosure cannot carry a current set; no evidence changed; retain hidden-resource details privately only")
	}
	metadata := schema.PullAssociationMetadataResult{State: schema.PullAssociationMetadataObserved, ObservedAt: &observedAt}
	if invalidated {
		metadata.State = schema.PullAssociationMetadataInvalidated
	}
	if err := metadata.Validate(); err != nil {
		return err
	}
	seen := map[schema.VillageUUID]bool{}
	for _, row := range rows {
		scope, err := schema.NormalizeCollectiveObservationScope(invocation.villageURL, row.ID)
		if err != nil {
			return err
		}
		_ = scope
		if row.Name == "" || row.SharedAt.IsZero() || seen[row.ID] {
			return fmt.Errorf("install transcript associations: missing/repeated identity, name or source time; no set installed; fetch validated dedicated associations")
		}
		seen[row.ID] = true
	}
	return s.withObservationTransaction(ctx, invocation.credentials, true, func(conn *sqlite.Conn) error {
		if err := checkObservationInvocation(conn, invocation); err != nil {
			return err
		}
		args := []any{invocation.villageURL, invocation.userID, id.String()}
		var installed, attempt int64
		if err := sqlitex.Execute(conn, `SELECT installed_seq,attempt_seq FROM transcript_collective_observations WHERE village_url=? AND user_id=? AND transcript_id=?`, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
			installed, attempt = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
			return nil
		}}); err != nil {
			return err
		}
		if installed > invocation.sequence || attempt > invocation.sequence {
			return ErrObservationSuperseded
		}
		at := observedAt.UTC().Format(observationOrderedTimeLayout)
		state := string(schema.PullAssociationMetadataObserved)
		if invalidated {
			state = string(schema.PullAssociationMetadataInvalidated)
		}
		if err := sqlitex.Execute(conn, `INSERT INTO transcript_collective_observations(village_url,user_id,transcript_id,installed_seq,attempt_seq,observed_at,state,last_attempt_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(village_url,user_id,transcript_id) DO UPDATE SET installed_seq=excluded.installed_seq,observed_at=excluded.observed_at,state=excluded.state`, &sqlitex.ExecOptions{Args: append(append([]any{}, args...), invocation.sequence, invocation.sequence, at, state, at)}); err != nil {
			return err
		}
		if attempt <= invocation.sequence {
			if err := sqlitex.Execute(conn, `UPDATE transcript_collective_observations SET attempt_seq=?,last_attempt_at=?,issue_code=NULL,issue_message=NULL,issue_retry=NULL WHERE village_url=? AND user_id=? AND transcript_id=?`, &sqlitex.ExecOptions{Args: append([]any{invocation.sequence, at}, args...)}); err != nil {
				return err
			}
		}
		if err := sqlitex.Execute(conn, `UPDATE transcript_collective_evidence SET current=0 WHERE village_url=? AND user_id=? AND transcript_id=?`, &sqlitex.ExecOptions{Args: args}); err != nil {
			return err
		}
		for _, row := range rows {
			var description, org any
			if row.Description != nil {
				description = *row.Description
			}
			if row.LinkedGithubOrg != nil {
				org = *row.LinkedGithubOrg
			}
			pairArgs := append(append([]any{}, args...), row.ID.String())
			var version int64
			if err := sqlitex.Execute(conn, `SELECT version FROM transcript_collective_evidence WHERE village_url=? AND user_id=? AND transcript_id=? AND collective_id=? AND name=? AND description IS ? AND linked_github_org IS ? AND shared_at=? ORDER BY version DESC LIMIT 1`, &sqlitex.ExecOptions{Args: append(append([]any{}, pairArgs...), row.Name, description, org, row.SharedAt.UTC().Format(observationOrderedTimeLayout)), ResultFunc: func(stmt *sqlite.Stmt) error { version = stmt.ColumnInt64(0); return nil }}); err != nil {
				return err
			}
			if version > 0 {
				if err := sqlitex.Execute(conn, `UPDATE transcript_collective_evidence SET current=1,last_observed_at=? WHERE village_url=? AND user_id=? AND transcript_id=? AND collective_id=? AND version=?`, &sqlitex.ExecOptions{Args: append(append([]any{at}, pairArgs...), version)}); err != nil {
					return err
				}
			} else {
				if err := sqlitex.Execute(conn, `INSERT INTO transcript_collective_evidence(village_url,user_id,transcript_id,collective_id,version,name,description,linked_github_org,shared_at,first_observed_at,last_observed_at,current) VALUES(?,?,?,?,(SELECT coalesce(max(version),0)+1 FROM transcript_collective_evidence WHERE village_url=? AND user_id=? AND transcript_id=? AND collective_id=?),?,?,?,?,?,?,1)`, &sqlitex.ExecOptions{Args: append(append(append([]any{}, pairArgs...), pairArgs...), row.Name, description, org, row.SharedAt.UTC().Format(observationOrderedTimeLayout), at, at)}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RecordTranscriptAssociationFailure records uncertainty only, never clears the
// last complete set or changes disclosure authority based on a network error.
func (s *Store) RecordTranscriptAssociationFailure(ctx context.Context, invocation CollectiveObservationInvocation, id schema.TranscriptID, attemptedAt time.Time, issue schema.OperationIssue) error {
	if invocation.store != s || invocation.sequence < 1 {
		return fmt.Errorf("record association failure: invalid invocation; no cache changed; begin a selected-account observation")
	}
	if _, err := schema.NewTranscriptID(id.String()); err != nil {
		return err
	}
	if err := issue.Validate(); err != nil {
		return err
	}
	if issue.Operation != schema.CollectiveOperationPull {
		return fmt.Errorf("record association failure: optional metadata warnings must name pull; no cache changed; use the shared pull warning shape")
	}
	probe := schema.PullAssociationMetadataResult{State: schema.PullAssociationMetadataStale, ObservedAt: &attemptedAt, Warning: &issue}
	if err := probe.Validate(); err != nil {
		return err
	}
	return s.withObservationTransaction(ctx, invocation.credentials, true, func(conn *sqlite.Conn) error {
		if err := checkObservationInvocation(conn, invocation); err != nil {
			return err
		}
		if err := checkTranscriptReadSequence(conn, invocation, id); err != nil {
			return err
		}
		return sqlitex.Execute(conn, `INSERT INTO transcript_collective_observations(village_url,user_id,transcript_id,attempt_seq,last_attempt_at,issue_code,issue_message,issue_retry) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(village_url,user_id,transcript_id) DO UPDATE SET attempt_seq=excluded.attempt_seq,last_attempt_at=excluded.last_attempt_at,issue_code=excluded.issue_code,issue_message=excluded.issue_message,issue_retry=excluded.issue_retry WHERE attempt_seq<=excluded.attempt_seq`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID, id.String(), invocation.sequence, attemptedAt.UTC().Format(observationOrderedTimeLayout), string(issue.Code), issue.Message, issue.Retry}})
	})
}

// ReadTranscriptAssociations reads actual per-row disclosure, never the union
// selector that happened to discover a transcript. Signed-out guards and scope
// selection are applied before reading any private rows.
func (s *Store) ReadTranscriptAssociations(ctx context.Context, snapshot auth.CredentialSnapshot, ids []schema.TranscriptID) (map[schema.TranscriptID]TranscriptCollectiveObservation, error) {
	if len(ids) > schema.MaxCollectiveFetchItems {
		return nil, fmt.Errorf("read transcript associations: too many IDs; no rows returned; request at most the schema fetch bound")
	}
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return nil, err
	}
	result := make(map[schema.TranscriptID]TranscriptCollectiveObservation, len(ids))
	for _, id := range ids {
		if _, err := schema.NewTranscriptID(id.String()); err != nil {
			return nil, err
		}
	}
	err = s.withObservationTransaction(ctx, snapshot, false, func(conn *sqlite.Conn) error {
		for _, id := range ids {
			row := TranscriptCollectiveObservation{TranscriptID: id}
			args := []any{base, user, id.String()}
			var persistedState string
			if err := sqlitex.Execute(conn, `SELECT observed_at,state,issue_code,issue_message,issue_retry FROM transcript_collective_observations WHERE village_url=? AND user_id=? AND transcript_id=?`, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
				metadata := schema.PullAssociationMetadataResult{}
				persistedState = stmt.ColumnText(1)
				if stmt.ColumnType(0) != sqlite.TypeNull {
					at, err := time.Parse(time.RFC3339Nano, stmt.ColumnText(0))
					if err != nil {
						return err
					}
					metadata.ObservedAt = &at
				}
				if stmt.ColumnType(2) != sqlite.TypeNull {
					issue := schema.OperationIssue{Operation: schema.CollectiveOperationPull, Message: stmt.ColumnText(3), Retry: stmt.ColumnText(4)}
					for _, code := range schema.AllOperationIssueCodes {
						if string(code) == stmt.ColumnText(2) {
							issue.Code = code
						}
					}
					metadata.Warning = &issue
					metadata.State = schema.PullAssociationMetadataUnknown
					if metadata.ObservedAt != nil {
						metadata.State = schema.PullAssociationMetadataStale
					}
				} else {
					for _, state := range schema.AllPullAssociationMetadataStates {
						if string(state) == persistedState {
							metadata.State = state
						}
					}
				}
				// An issued read with no completed observation or failure is still
				// never observed, not a fabricated unknown-result warning.
				if metadata.ObservedAt == nil && metadata.Warning == nil && persistedState == "" {
					return nil
				}
				if err := metadata.Validate(); err != nil {
					return err
				}
				row.Metadata = &metadata
				return nil
			}}); err != nil {
				return err
			}
			if row.Metadata != nil {
				row.Collectives = []schema.VillageTranscriptCollective{}
				if persistedState == string(schema.PullAssociationMetadataObserved) {
					if err := sqlitex.Execute(conn, `SELECT collective_id,name,description,linked_github_org,shared_at FROM transcript_collective_evidence WHERE village_url=? AND user_id=? AND transcript_id=? AND current=1 ORDER BY collective_id`, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
						value := schema.VillageTranscriptCollective{ID: schema.VillageUUID(stmt.ColumnText(0)), Name: stmt.ColumnText(1)}
						if stmt.ColumnType(2) != sqlite.TypeNull {
							text := stmt.ColumnText(2)
							value.Description = &text
						}
						if stmt.ColumnType(3) != sqlite.TypeNull {
							text := stmt.ColumnText(3)
							value.LinkedGithubOrg = &text
						}
						var err error
						value.SharedAt, err = time.Parse(time.RFC3339Nano, stmt.ColumnText(4))
						if err != nil {
							return err
						}
						row.Collectives = append(row.Collectives, value)
						return nil
					}}); err != nil {
						return err
					}
				}
			}
			result[id] = row
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
