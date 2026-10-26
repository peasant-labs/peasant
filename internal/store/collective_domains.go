package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

func readCollectiveDomain(conn *sqlite.Conn, invocation CollectiveObservationInvocation, domain schema.CollectiveFetchDomain) (schema.CollectiveFetchDomainSummary, int64, int64, error) {
	value := schema.CollectiveFetchDomainSummary{State: schema.CollectiveFetchStateNotRequested, Coverage: schema.CollectiveEvidenceCoverageUnknown}
	var installed, attempt int64
	err := sqlitex.Execute(conn, `SELECT installed_seq,attempt_seq,summary_json FROM collective_domain_observations WHERE village_url=? AND user_id=? AND domain=?`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID, string(domain)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		installed, attempt = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
		if err := json.Unmarshal([]byte(stmt.ColumnText(2)), &value); err != nil {
			return err
		}
		return value.Validate()
	}})
	return value, installed, attempt, err
}

func writeCollectiveDomain(conn *sqlite.Conn, invocation CollectiveObservationInvocation, result *schema.CollectiveFetchDomainResult) error {
	prior, installed, attempt, err := readCollectiveDomain(conn, invocation, result.Domain)
	if err != nil {
		return err
	}
	if result.State == schema.CollectiveFetchStateComplete {
		if installed > invocation.sequence || attempt > invocation.sequence {
			return ErrObservationSuperseded
		}
		installed = invocation.sequence
	} else {
		result.LastCompletedAt, result.LastInstalledRevision, result.Coverage = prior.LastCompletedAt, prior.LastInstalledRevision, prior.Coverage
	}
	if err := result.Validate(); err != nil {
		return err
	}
	summary := schema.CollectiveFetchDomainSummary{State: result.State, Coverage: result.Coverage, LastAttemptAt: result.LastAttemptAt, LastCompletedAt: result.LastCompletedAt, LastInstalledRevision: result.LastInstalledRevision, Issue: result.Issue}
	if attempt > invocation.sequence {
		return ErrObservationSuperseded
	} else {
		attempt = invocation.sequence
	}
	data, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	return sqlitex.Execute(conn, `INSERT INTO collective_domain_observations(village_url,user_id,domain,installed_seq,attempt_seq,summary_json) VALUES(?,?,?,?,?,?) ON CONFLICT(village_url,user_id,domain) DO UPDATE SET installed_seq=excluded.installed_seq,attempt_seq=excluded.attempt_seq,summary_json=excluded.summary_json`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID, string(result.Domain), installed, attempt, string(data)}})
}

// RecordCollectiveDomainFailure changes attempt reporting only. It loads the
// latest retained completion/revision under the writer, not a stale fetch-start
// snapshot, and cannot erase an independently committed complete domain.
func (s *Store) RecordCollectiveDomainFailure(ctx context.Context, invocation CollectiveObservationInvocation, result schema.CollectiveFetchDomainResult) (schema.CollectiveFetchDomainResult, error) {
	if invocation.store != s || result.State == schema.CollectiveFetchStateComplete {
		return result, fmt.Errorf("record fetch failure: invalid Store invocation or complete result; no reporting changed; use a failed requested domain result")
	}
	err := s.withObservationTransaction(ctx, invocation.credentials, true, func(conn *sqlite.Conn) error {
		if err := checkObservationInvocation(conn, invocation); err != nil {
			return err
		}
		if err := writeCollectiveDomain(conn, invocation, &result); err != nil {
			return err
		}
		_, err := bumpObservationRevision(conn, invocation.villageURL, invocation.userID)
		return err
	})
	return result, err
}

// CommitCollectiveCatalog installs ONLY after the four independent complete
// relevance inputs are present. Full event-history crawling is not a dependency.
func (s *Store) CommitCollectiveCatalog(ctx context.Context, invocation CollectiveObservationInvocation, rows []schema.CollectiveObservation, sources []schema.CollectiveFetchSourceResult, startedAt, completedAt time.Time) (schema.CollectiveFetchDomainResult, error) {
	result := schema.CollectiveFetchDomainResult{Domain: schema.CollectiveFetchDomainCatalog, Requested: true, State: schema.CollectiveFetchStateComplete, Commit: schema.CollectiveFetchCommitUnchanged, StartedAt: &startedAt, LastAttemptAt: &completedAt, CompletedAt: &completedAt, LastCompletedAt: &completedAt, Coverage: schema.CollectiveEvidenceCoverageAvailable, SourceOutcomes: sources}
	if invocation.store != s || rows == nil || len(rows) > schema.MaxCollectiveFetchItems {
		return result, fmt.Errorf("commit catalog: absent/oversized complete projection or wrong Store; nothing installed; stage the bounded complete relevance inputs")
	}
	seen := map[schema.CollectiveObservationSource]bool{}
	for _, source := range sources {
		if seen[source.Source] || !source.Complete {
			return result, fmt.Errorf("commit catalog: repeated or incomplete relevance input; prior catalog retained; obtain all four complete source inputs")
		}
		seen[source.Source] = true
	}
	for _, required := range []schema.CollectiveObservationSource{schema.CollectiveObservationSourceMembership, schema.CollectiveObservationSourceContributionDiscovery, schema.CollectiveObservationSourceOrganization, schema.CollectiveObservationSourceExplicit} {
		if !seen[required] {
			return result, fmt.Errorf("commit catalog: required %s input is missing; prior relevance is retained; fetch this complete input before retrying", required)
		}
	}
	if len(seen) != 4 {
		return result, fmt.Errorf("commit catalog: unrelated source supplied as a relevance prerequisite; no catalog installed; stage exactly the catalog's four inputs")
	}
	identities := map[schema.VillageUUID]bool{}
	for _, row := range rows {
		if err := row.Validate(); err != nil {
			return result, err
		}
		if identities[row.ID] {
			return result, fmt.Errorf("commit catalog: repeated collective UUID; no catalog installed; merge independent reasons per UUID before staging")
		}
		identities[row.ID] = true
	}
	err := s.withObservationTransaction(ctx, invocation.credentials, true, func(conn *sqlite.Conn) error {
		if err := checkObservationInvocation(conn, invocation); err != nil {
			return err
		}
		_, installed, issued, err := readCollectiveDomain(conn, invocation, result.Domain)
		if err != nil {
			return err
		}
		if installed > invocation.sequence || issued > invocation.sequence {
			return ErrObservationSuperseded
		}
		for _, source := range sources {
			if err := checkCollectiveSourceSequence(conn, invocation, source.Source, ""); err != nil {
				return err
			}
		}
		oldReasons := map[string]bool{}
		if err := sqlitex.Execute(conn, `SELECT collective_id,reason FROM collective_relevance_observations WHERE village_url=? AND user_id=? AND current=1`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID}, ResultFunc: func(stmt *sqlite.Stmt) error {
			oldReasons[stmt.ColumnText(0)+"/"+stmt.ColumnText(1)] = true
			return nil
		}}); err != nil {
			return err
		}
		if err := sqlitex.Execute(conn, `UPDATE collective_relevance_observations SET current=0 WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID}}); err != nil {
			return err
		}
		newReasons := map[string]bool{}
		changed := false
		for _, row := range rows {
			payloadChanged, err := writeCollectiveProjection(conn, invocation.villageURL, invocation.userID, row)
			if err != nil {
				return err
			}
			changed = changed || payloadChanged
			for _, reason := range row.Reasons {
				key := row.ID.String() + "/" + string(reason)
				newReasons[key] = true
				if !oldReasons[key] {
					changed = true
				}
				if err := writeCollectiveReason(conn, invocation.villageURL, invocation.userID, row.ID, reason, row.ObservedAt, true); err != nil {
					return err
				}
			}
		}
		for key := range oldReasons {
			if !newReasons[key] {
				changed = true
			}
		}
		for _, source := range sources {
			if err := markCollectiveSource(conn, invocation, source.Source, ""); err != nil {
				return err
			}
		}
		revision, err := bumpObservationRevision(conn, invocation.villageURL, invocation.userID)
		if err != nil {
			return err
		}
		result.LastInstalledRevision = &revision
		if changed {
			result.Commit = schema.CollectiveFetchCommitUpdated
		}
		return writeCollectiveDomain(conn, invocation, &result)
	})
	return result, err
}

// CommitCollectiveHistory reconciles the entire requested available dataset and
// its coverage/domain completion in the SAME transaction. Unknown source ranges
// are explicit coverage, not invented empty records or deletion of prior rows.
func (s *Store) CommitCollectiveHistory(ctx context.Context, invocation CollectiveObservationInvocation, pairs []ContributionAttemptSource, sources []schema.CollectiveFetchSourceResult, coverage schema.CollectiveEvidenceCoverage, startedAt, completedAt time.Time) (schema.CollectiveFetchDomainResult, error) {
	result := schema.CollectiveFetchDomainResult{Domain: schema.CollectiveFetchDomainHistory, Requested: true, State: schema.CollectiveFetchStateComplete, Commit: schema.CollectiveFetchCommitUnchanged, StartedAt: &startedAt, LastAttemptAt: &completedAt, CompletedAt: &completedAt, LastCompletedAt: &completedAt, Coverage: coverage, SourceOutcomes: sources}
	discovery := false
	for _, source := range sources {
		if !source.Complete {
			return result, fmt.Errorf("commit history: incomplete discovered source; all previous history retained; stage every required source before installing")
		}
		if source.Source == schema.CollectiveObservationSourceContributionDiscovery && source.Outcome == schema.CollectiveSourceDisclosed {
			discovery = true
		}
	}
	if !discovery {
		return result, fmt.Errorf("commit history: complete owner contribution discovery is absent; prior history retained; obtain complete owner discovery before pair crawling")
	}
	_, err := s.installContributionAttemptSources(ctx, invocation, pairs, func(conn *sqlite.Conn) error {
		_, installed, issued, err := readCollectiveDomain(conn, invocation, result.Domain)
		if err != nil {
			return err
		}
		if installed > invocation.sequence || issued > invocation.sequence {
			return ErrObservationSuperseded
		}
		return checkCollectiveSourceSequence(conn, invocation, schema.CollectiveObservationSourceAttempts, "")
	}, func(conn *sqlite.Conn, effect ContributionAttemptInstall) error {
		var oldCoverage string
		if err := sqlitex.Execute(conn, `SELECT attempt_coverage FROM collective_observation_scopes WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID}, ResultFunc: func(stmt *sqlite.Stmt) error { oldCoverage = stmt.ColumnText(0); return nil }}); err != nil {
			return err
		}
		if oldCoverage != string(coverage) {
			if !coverage.IsValid() {
				return fmt.Errorf("commit history: invalid coverage; transaction discarded; use the shared coverage menu")
			}
			if err := sqlitex.Execute(conn, `UPDATE collective_observation_scopes SET attempt_coverage=? WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{string(coverage), invocation.villageURL, invocation.userID}}); err != nil {
				return err
			}
			if err := bumpEvidence(conn, invocation.villageURL, invocation.userID, schema.LocalContributionEvidenceRemoteAttempts); err != nil {
				return err
			}
			effect.Changed = true
		}
		if effect.Changed {
			result.Commit = schema.CollectiveFetchCommitUpdated
		}
		revision, err := bumpObservationRevision(conn, invocation.villageURL, invocation.userID)
		if err != nil {
			return err
		}
		result.LastInstalledRevision = &revision
		return writeCollectiveDomain(conn, invocation, &result)
	})
	return result, err
}

func (s *Store) RecordCollectiveAssociationDomain(ctx context.Context, invocation CollectiveObservationInvocation, result schema.CollectiveFetchDomainResult) (schema.CollectiveFetchDomainResult, error) {
	if result.Domain != schema.CollectiveFetchDomainAssociations || invocation.store != s {
		return result, fmt.Errorf("record association domain: wrong domain or Store; no state changed; use the selected association invocation")
	}
	err := s.withObservationTransaction(ctx, invocation.credentials, true, func(conn *sqlite.Conn) error {
		if err := checkObservationInvocation(conn, invocation); err != nil {
			return err
		}
		revision, err := bumpObservationRevision(conn, invocation.villageURL, invocation.userID)
		if err != nil {
			return err
		}
		if result.State == schema.CollectiveFetchStateComplete {
			result.LastInstalledRevision = &revision
			result.LastCompletedAt = result.CompletedAt
		}
		return writeCollectiveDomain(conn, invocation, &result)
	})
	return result, err
}

// SaveCollectiveFetchReport records reporting metadata only, never invalidates
// evidence traversal tokens, and never advances all-complete time on partial.
func (s *Store) SaveCollectiveFetchReport(ctx context.Context, invocation CollectiveObservationInvocation, report schema.CollectiveFetchReport, attemptedAt time.Time) (schema.CollectiveFetchReport, error) {
	if invocation.store != s {
		return report, fmt.Errorf("save fetch report: wrong Store invocation; no report changed; use the selected observation Store")
	}
	if err := report.Validate(); err != nil {
		return report, err
	}
	scope, err := schema.NormalizeCollectiveObservationScope(invocation.villageURL, schema.VillageUUID(invocation.userID))
	if err != nil {
		return report, err
	}
	if report.Scope != scope {
		return report, fmt.Errorf("save fetch report: scope differs from invocation; no report changed; discard the late cross-account result")
	}
	err = s.withObservationTransaction(ctx, invocation.credentials, true, func(conn *sqlite.Conn) error {
		if err := checkObservationInvocation(conn, invocation); err != nil {
			return err
		}
		var installed int64
		if err := sqlitex.Execute(conn, `SELECT invocation_seq FROM collective_fetch_reports WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID}, ResultFunc: func(stmt *sqlite.Stmt) error { installed = stmt.ColumnInt64(0); return nil }}); err != nil {
			return err
		}
		if installed > invocation.sequence {
			return nil
		}
		revision, err := bumpObservationRevision(conn, invocation.villageURL, invocation.userID)
		if err != nil {
			return err
		}
		report.CacheRevision = revision
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		var completed any
		if report.Outcome == schema.CollectiveFetchOutcomeComplete {
			completed = attemptedAt.UTC().Format(observationOrderedTimeLayout)
		}
		return sqlitex.Execute(conn, `INSERT INTO collective_fetch_reports(village_url,user_id,invocation_seq,last_attempt_at,last_completed_at,report_json) VALUES(?,?,?,?,?,?) ON CONFLICT(village_url,user_id) DO UPDATE SET invocation_seq=excluded.invocation_seq,last_attempt_at=excluded.last_attempt_at,last_completed_at=coalesce(excluded.last_completed_at,collective_fetch_reports.last_completed_at),report_json=excluded.report_json`, &sqlitex.ExecOptions{Args: []any{invocation.villageURL, invocation.userID, invocation.sequence, attemptedAt.UTC().Format(observationOrderedTimeLayout), completed, string(data)}})
	})
	return report, err
}
