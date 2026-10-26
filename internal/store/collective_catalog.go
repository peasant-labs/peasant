package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

func bumpObservationRevision(conn *sqlite.Conn, base, user string) (string, error) {
	if err := sqlitex.Execute(conn, `UPDATE collective_observation_scopes SET cache_revision=cache_revision+1 WHERE village_url=? AND user_id=? AND cache_revision<9223372036854775807`, &sqlitex.ExecOptions{Args: []any{base, user}}); err != nil {
		return "", err
	}
	if conn.Changes() != 1 {
		return "", fmt.Errorf("advance collective cache revision: scope disappeared or revision exhausted; no changes committed; recreate the selected observation cache")
	}
	var revision string
	err := sqlitex.Execute(conn, `SELECT CAST(cache_revision AS TEXT) FROM collective_observation_scopes WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{base, user}, ResultFunc: func(stmt *sqlite.Stmt) error { revision = stmt.ColumnText(0); return nil }})
	return revision, err
}

func checkCollectiveSourceSequence(conn *sqlite.Conn, invocation CollectiveObservationInvocation, source schema.CollectiveObservationSource, id string) error {
	var installed int64
	if err := sqlitex.Execute(conn, `SELECT coalesce(max(max(installed_seq,issued_seq)),0) FROM collective_source_installs WHERE village_url=? AND user_id=? AND source=? AND (source_id='' OR source_id=? OR ?='')`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID, string(source), id, id}, ResultFunc: func(stmt *sqlite.Stmt) error { installed = stmt.ColumnInt64(0); return nil }}); err != nil {
		return err
	}
	if installed > invocation.sequence {
		return ErrObservationSuperseded
	}
	return nil
}
func markCollectiveSource(conn *sqlite.Conn, invocation CollectiveObservationInvocation, source schema.CollectiveObservationSource, id string) error {
	return sqlitex.Execute(conn, `INSERT INTO collective_source_installs(village_url,user_id,source,source_id,installed_seq) VALUES(?,?,?,?,?) ON CONFLICT(village_url,user_id,source,source_id) DO UPDATE SET installed_seq=excluded.installed_seq`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID, string(source), id, invocation.sequence}})
}

func writeCollectiveProjection(conn *sqlite.Conn, base, user string, row schema.CollectiveObservation) (bool, error) {
	var description, acceptance, access any
	if row.Description != nil {
		description = *row.Description
	}
	if row.AcceptanceMode != nil {
		acceptance = string(*row.AcceptanceMode)
	}
	if row.DataAccess != nil {
		access = string(*row.DataAccess)
	}
	args := []any{base, user, row.ID.String()}
	var matching, current int64
	if err := sqlitex.Execute(conn, `SELECT version FROM collective_observation_versions WHERE village_url=? AND user_id=? AND collective_id=? AND current=1`, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error { current = stmt.ColumnInt64(0); return nil }}); err != nil {
		return false, err
	}
	if err := sqlitex.Execute(conn, `SELECT version FROM collective_observation_versions WHERE village_url=? AND user_id=? AND collective_id=? AND name=? AND description IS ? AND acceptance_mode IS ? AND data_access IS ? AND state=? ORDER BY version DESC LIMIT 1`, &sqlitex.ExecOptions{Args: append(append([]any{}, args...), row.Name, description, acceptance, access, string(row.State)), ResultFunc: func(stmt *sqlite.Stmt) error { matching = stmt.ColumnInt64(0); return nil }}); err != nil {
		return false, err
	}
	if matching != current || matching == 0 {
		if err := sqlitex.Execute(conn, `UPDATE collective_observation_versions SET current=0 WHERE village_url=? AND user_id=? AND collective_id=?`, &sqlitex.ExecOptions{Args: args}); err != nil {
			return false, err
		}
	}
	at := row.ObservedAt.UTC().Format(observationOrderedTimeLayout)
	if matching > 0 {
		if err := sqlitex.Execute(conn, `UPDATE collective_observation_versions SET current=1,last_observed_at=? WHERE village_url=? AND user_id=? AND collective_id=? AND version=?`, &sqlitex.ExecOptions{Args: append(append([]any{at}, args...), matching)}); err != nil {
			return false, err
		}
	} else {
		if err := sqlitex.Execute(conn, `INSERT INTO collective_observation_versions(village_url,user_id,collective_id,version,name,description,acceptance_mode,data_access,state,first_observed_at,last_observed_at,current) VALUES(?,?,?,(SELECT coalesce(max(version),0)+1 FROM collective_observation_versions WHERE village_url=? AND user_id=? AND collective_id=?),?,?,?,?,?,?,?,1)`, &sqlitex.ExecOptions{Args: append(append(append([]any{}, args...), args...), row.Name, description, acceptance, access, string(row.State), at, at)}); err != nil {
			return false, err
		}
	}
	return matching == 0 || matching != current, nil
}

func writeCollectiveReason(conn *sqlite.Conn, base, user string, id schema.VillageUUID, reason schema.CollectiveObservationReason, at time.Time, current bool) error {
	value := 0
	if current {
		value = 1
	}
	return sqlitex.Execute(conn, `INSERT INTO collective_relevance_observations(village_url,user_id,collective_id,reason,observed_at,current) VALUES(?,?,?,?,?,?) ON CONFLICT(village_url,user_id,collective_id,reason) DO UPDATE SET observed_at=excluded.observed_at,current=excluded.current`, &sqlitex.ExecOptions{Args: []any{base, user, id.String(), string(reason), at.UTC().Format(observationOrderedTimeLayout), value}})
}

func (s *Store) TrackCollectiveObservation(ctx context.Context, invocation CollectiveObservationInvocation, row schema.CollectiveObservation) error {
	if err := row.Validate(); err != nil {
		return err
	}
	if invocation.store != s {
		return fmt.Errorf("track collective: invocation belongs to another Store; no preference changed; use the selected Store invocation")
	}
	return s.withObservationTransaction(ctx, invocation.credentials, true, func(conn *sqlite.Conn) error {
		if err := checkObservationInvocation(conn, invocation); err != nil {
			return err
		}
		if err := checkCollectiveSourceSequence(conn, invocation, schema.CollectiveObservationSourceExplicit, row.ID.String()); err != nil {
			return err
		}
		if _, err := writeCollectiveProjection(conn, invocation.villageURL, invocation.userID, row); err != nil {
			return err
		}
		if err := sqlitex.Execute(conn, `INSERT INTO collective_tracking_preferences(village_url,user_id,collective_id,selected_at) VALUES(?,?,?,?) ON CONFLICT(village_url,user_id,collective_id) DO NOTHING`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID, row.ID.String(), row.ObservedAt.UTC().Format(observationOrderedTimeLayout)}}); err != nil {
			return err
		}
		if err := writeCollectiveReason(conn, invocation.villageURL, invocation.userID, row.ID, schema.CollectiveObservationReasonExplicit, row.ObservedAt, true); err != nil {
			return err
		}
		if err := markCollectiveSource(conn, invocation, schema.CollectiveObservationSourceExplicit, row.ID.String()); err != nil {
			return err
		}
		_, err := bumpObservationRevision(conn, invocation.villageURL, invocation.userID)
		return err
	})
}

func (s *Store) UntrackCollectiveObservation(ctx context.Context, invocation CollectiveObservationInvocation, id schema.VillageUUID, at time.Time) error {
	if invocation.store != s {
		return fmt.Errorf("untrack collective: invocation belongs to another Store; no preference changed; use a selected-scope invocation")
	}
	if _, err := schema.NewTranscriptID(id.String()); err != nil {
		return err
	}
	return s.withObservationTransaction(ctx, invocation.credentials, true, func(conn *sqlite.Conn) error {
		if err := checkObservationInvocation(conn, invocation); err != nil {
			return err
		}
		if err := checkCollectiveSourceSequence(conn, invocation, schema.CollectiveObservationSourceExplicit, id.String()); err != nil {
			return err
		}
		args := []any{invocation.villageURL, invocation.userID, id.String()}
		if err := sqlitex.Execute(conn, `DELETE FROM collective_tracking_preferences WHERE village_url=? AND user_id=? AND collective_id=?`, &sqlitex.ExecOptions{Args: args}); err != nil {
			return err
		}
		changed := conn.Changes() != 0
		if err := sqlitex.Execute(conn, `UPDATE collective_relevance_observations SET current=0,observed_at=? WHERE village_url=? AND user_id=? AND collective_id=? AND reason=? AND current=1`, &sqlitex.ExecOptions{Args: append(append([]any{at.UTC().Format(observationOrderedTimeLayout)}, args...), string(schema.CollectiveObservationReasonExplicit))}); err != nil {
			return err
		}
		changed = changed || conn.Changes() != 0
		if err := markCollectiveSource(conn, invocation, schema.CollectiveObservationSourceExplicit, id.String()); err != nil {
			return err
		}
		if !changed {
			return nil
		}
		_, err := bumpObservationRevision(conn, invocation.villageURL, invocation.userID)
		return err
	})
}

func readCollectiveSummaries(conn *sqlite.Conn, base, user string) (schema.CollectiveFetchSummary, error) {
	unknown := schema.CollectiveFetchDomainSummary{State: schema.CollectiveFetchStateNotRequested, Coverage: schema.CollectiveEvidenceCoverageUnknown}
	summary := schema.CollectiveFetchSummary{Catalog: unknown, History: unknown, Associations: unknown}
	err := sqlitex.Execute(conn, `SELECT domain,summary_json FROM collective_domain_observations WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{base, user}, ResultFunc: func(stmt *sqlite.Stmt) error {
		var value schema.CollectiveFetchDomainSummary
		if err := json.Unmarshal([]byte(stmt.ColumnText(1)), &value); err != nil {
			return err
		}
		if err := value.Validate(); err != nil {
			return err
		}
		switch stmt.ColumnText(0) {
		case string(schema.CollectiveFetchDomainCatalog):
			summary.Catalog = value
		case string(schema.CollectiveFetchDomainHistory):
			summary.History = value
		case string(schema.CollectiveFetchDomainAssociations):
			summary.Associations = value
		default:
			return fmt.Errorf("read collective summary: stored domain is unknown; no cache response returned; repair the selected cache")
		}
		return nil
	}})
	if err != nil {
		return summary, err
	}
	err = sqlitex.Execute(conn, `SELECT last_attempt_at,last_completed_at,report_json FROM collective_fetch_reports WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{base, user}, ResultFunc: func(stmt *sqlite.Stmt) error {
		attempt, err := time.Parse(time.RFC3339Nano, stmt.ColumnText(0))
		if err != nil {
			return err
		}
		summary.LastAttemptAt = &attempt
		if stmt.ColumnType(1) != sqlite.TypeNull {
			at, err := time.Parse(time.RFC3339Nano, stmt.ColumnText(1))
			if err != nil {
				return err
			}
			summary.LastCompletedFetchAt = &at
		}
		var report schema.CollectiveFetchReport
		if err := json.Unmarshal([]byte(stmt.ColumnText(2)), &report); err != nil {
			return err
		}
		if err := report.Validate(); err != nil {
			return err
		}
		summary.LatestReport = &report
		return nil
	}})
	return summary, err
}

func (s *Store) ReadCollectiveObservations(ctx context.Context, snapshot auth.CredentialSnapshot) (schema.CollectiveObservationResponse, error) {
	var response schema.CollectiveObservationResponse
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return response, err
	}
	response.CacheRevision = "0"
	response.Scope, err = schema.NormalizeCollectiveObservationScope(base, schema.VillageUUID(user))
	if err != nil {
		return response, err
	}
	response.Collectives = []schema.CollectiveObservation{}
	err = s.withObservationTransaction(ctx, snapshot, false, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `SELECT cache_revision FROM collective_observation_scopes WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{base, user}, ResultFunc: func(stmt *sqlite.Stmt) error {
			response.CacheRevision = strconv.FormatInt(stmt.ColumnInt64(0), 10)
			return nil
		}}); err != nil {
			return err
		}
		reasons := map[schema.VillageUUID]map[schema.CollectiveObservationReason]bool{}
		if err := sqlitex.Execute(conn, `SELECT collective_id,reason FROM collective_relevance_observations WHERE village_url=? AND user_id=? AND current=1`, &sqlitex.ExecOptions{Args: []any{base, user}, ResultFunc: func(stmt *sqlite.Stmt) error {
			id := schema.VillageUUID(stmt.ColumnText(0))
			if reasons[id] == nil {
				reasons[id] = map[schema.CollectiveObservationReason]bool{}
			}
			found := false
			for _, known := range schema.AllCollectiveObservationReasons {
				if string(known) == stmt.ColumnText(1) {
					reasons[id][known] = true
					found = true
				}
			}
			if !found {
				return fmt.Errorf("read catalog relevance: unknown stored reason; no evidence returned; repair the selected cache")
			}
			return nil
		}}); err != nil {
			return err
		}
		if err := sqlitex.Execute(conn, `SELECT collective_id,name,description,acceptance_mode,data_access,last_observed_at,state FROM collective_observation_versions WHERE village_url=? AND user_id=? AND current=1 AND EXISTS(SELECT 1 FROM collective_relevance_observations r WHERE r.village_url=collective_observation_versions.village_url AND r.user_id=collective_observation_versions.user_id AND r.collective_id=collective_observation_versions.collective_id AND r.current=1) ORDER BY collective_id LIMIT ?`, &sqlitex.ExecOptions{Args: []any{base, user, schema.MaxCollectiveFetchItems + 1}, ResultFunc: func(stmt *sqlite.Stmt) error {
			if len(response.Collectives) >= schema.MaxCollectiveFetchItems {
				return fmt.Errorf("read catalog: saved relevant catalog exceeds its response bound; no prefix returned; narrow explicit tracking or fetch bounded sources")
			}
			row := schema.CollectiveObservation{ID: schema.VillageUUID(stmt.ColumnText(0)), Name: stmt.ColumnText(1), Reasons: []schema.CollectiveObservationReason{}}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				text := stmt.ColumnText(2)
				row.Description = &text
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				for _, known := range schema.AllVillageGroupAcceptanceModes {
					if string(known) == stmt.ColumnText(3) {
						value := known
						row.AcceptanceMode = &value
					}
				}
				if row.AcceptanceMode == nil {
					return fmt.Errorf("read catalog: invalid stored acceptance policy")
				}
			}
			if stmt.ColumnType(4) != sqlite.TypeNull {
				for _, known := range schema.AllVillageGroupDataAccessPolicies {
					if string(known) == stmt.ColumnText(4) {
						value := known
						row.DataAccess = &value
					}
				}
				if row.DataAccess == nil {
					return fmt.Errorf("read catalog: invalid stored access policy")
				}
			}
			var err error
			row.ObservedAt, err = time.Parse(time.RFC3339Nano, stmt.ColumnText(5))
			if err != nil {
				return err
			}
			for _, known := range schema.AllCollectiveObservationStates {
				if string(known) == stmt.ColumnText(6) {
					row.State = known
				}
			}
			for _, known := range schema.AllCollectiveObservationReasons {
				if reasons[row.ID][known] {
					row.Reasons = append(row.Reasons, known)
				}
			}
			if err := row.Validate(); err != nil {
				return err
			}
			response.Collectives = append(response.Collectives, row)
			return nil
		}}); err != nil {
			return err
		}
		var err error
		response.Fetch, err = readCollectiveSummaries(conn, base, user)
		return err
	})
	if err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	if err := response.Validate(); err != nil {
		return schema.CollectiveObservationResponse{}, err
	}
	return response, nil
}

func (s *Store) CollectiveTrackingIDs(ctx context.Context, snapshot auth.CredentialSnapshot) ([]schema.VillageUUID, error) {
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return nil, err
	}
	ids := []schema.VillageUUID{}
	err = s.withObservationTransaction(ctx, snapshot, false, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `SELECT collective_id FROM collective_tracking_preferences WHERE village_url=? AND user_id=? ORDER BY collective_id LIMIT ?`, &sqlitex.ExecOptions{Args: []any{base, user, schema.MaxCollectiveFetchItems + 1}, ResultFunc: func(stmt *sqlite.Stmt) error {
			if len(ids) >= schema.MaxCollectiveFetchItems {
				return fmt.Errorf("read tracked collectives: too many saved selections; no prefix returned; narrow explicit tracking")
			}
			ids = append(ids, schema.VillageUUID(stmt.ColumnText(0)))
			return nil
		}})
	})
	return ids, err
}

// ResolveSavedCollectiveName searches private dated names in the selected
// scope. It returns all exact matches so the service can refuse ambiguity.
func (s *Store) ResolveSavedCollectiveName(ctx context.Context, snapshot auth.CredentialSnapshot, name string) ([]schema.VillageUUID, error) {
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return nil, err
	}
	ids := []schema.VillageUUID{}
	err = s.withObservationTransaction(ctx, snapshot, false, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `SELECT DISTINCT collective_id FROM collective_observation_versions WHERE village_url=? AND user_id=? AND name=? COLLATE NOCASE ORDER BY collective_id LIMIT ?`, &sqlitex.ExecOptions{Args: []any{base, user, name, schema.MaxCollectiveFetchItems + 1}, ResultFunc: func(stmt *sqlite.Stmt) error {
			if len(ids) >= schema.MaxCollectiveFetchItems {
				return fmt.Errorf("resolve collective name: saved matching choices exceed their bound; use a canonical UUID")
			}
			ids = append(ids, schema.VillageUUID(stmt.ColumnText(0)))
			return nil
		}})
	})
	return ids, err
}
