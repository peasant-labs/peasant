package collectiveobservations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// FetchFailure retains the complete typed report even when the explicit action
// fails. Transport adapters must emit this report, not replace it with text.
type FetchFailure struct{ Report schema.CollectiveFetchReport }

func (e *FetchFailure) Error() string {
	return fmt.Sprintf("collective metadata fetch %s (HTTP %d); completed domain effects remain installed; inspect domains/issues and explicitly retry incomplete sources", e.Report.Outcome, schema.CollectiveFetchHTTPStatus(e.Report))
}

func sourceOutcome(source schema.CollectiveObservationSource, at time.Time, err error) schema.CollectiveFetchSourceResult {
	r := schema.CollectiveFetchSourceResult{Source: source, Outcome: schema.CollectiveSourceDisclosed, Complete: true, ObservedAt: &at}
	if err != nil {
		issue := operationIssue(err, schema.CollectiveOperationFetch)
		r.Complete = false
		r.ObservedAt = nil
		r.Outcome = schema.CollectiveSourceFailed
		r.Issue = &issue
	}
	return r
}
func negativeDisclosure(err error) bool {
	var status *village.StatusError
	return errors.As(err, &status) && (status.StatusCode == 403 || status.StatusCode == 404)
}
func failureState(issue schema.OperationIssue) schema.CollectiveFetchState {
	if issue.Code == schema.OperationIssueDeadline {
		return schema.CollectiveFetchStateCancelled
	}
	if issue.Code == schema.OperationIssueSuperseded {
		return schema.CollectiveFetchStateSuperseded
	}
	return schema.CollectiveFetchStateFailed
}

func (s *Service) failedDomain(ctx context.Context, invocation store.CollectiveObservationInvocation, domain schema.CollectiveFetchDomain, started time.Time, prior schema.CollectiveFetchDomainSummary, sources []schema.CollectiveFetchSourceResult, cause error) schema.CollectiveFetchDomainResult {
	at := s.now().UTC()
	issue := operationIssue(cause, schema.CollectiveOperationFetch)
	r := schema.CollectiveFetchDomainResult{Domain: domain, Requested: true, State: failureState(issue), Commit: schema.CollectiveFetchCommitRetained, StartedAt: &started, LastAttemptAt: &at, LastCompletedAt: prior.LastCompletedAt, LastInstalledRevision: prior.LastInstalledRevision, Coverage: prior.Coverage, SourceOutcomes: sources, Issue: &issue}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	saved, err := s.store.RecordCollectiveDomainFailure(cleanup, invocation, r)
	if err != nil {
		extra := operationIssue(persistenceError(err), schema.CollectiveOperationFetch)
		if extra.Code != schema.OperationIssueAuth && extra.Code != schema.OperationIssueSuperseded {
			r.Issue = &extra
			r.State = failureState(extra)
		}
		return r
	}
	return saved
}

// Fetch performs explicit metadata-only work. Catalog, available owner history,
// and each per-transcript disclosed set commit independently. No network runs
// under a credential or SQLite writer; every install checks original scope.
func (s *Service) Fetch(ctx context.Context) (schema.CollectiveFetchReport, error) {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	var report schema.CollectiveFetchReport
	selected, err := s.SelectedCredentials(ctx)
	if err != nil {
		return report, err
	}
	creds := selected.Credentials()
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return report, err
	}
	scope, err := schema.NormalizeCollectiveObservationScope(base, schema.VillageUUID(creds.UserID))
	if err != nil {
		return report, err
	}
	report = schema.CollectiveFetchReport{Scope: scope, InvocationID: schema.VillageUUID(uuid.NewString()), CacheRevision: "0", Outcome: schema.CollectiveFetchOutcomeFailed, Domains: []schema.CollectiveFetchDomainResult{}, Issues: []schema.OperationIssue{}}
	unknown := schema.CollectiveFetchDomainSummary{State: schema.CollectiveFetchStateNotRequested, Coverage: schema.CollectiveEvidenceCoverageUnknown}
	if s.store == nil || s.localTranscriptIDs == nil {
		return s.initialFetchFailure(report, schema.CollectiveFetchSummary{Catalog: unknown, History: unknown, Associations: unknown}, s.now().UTC(), persistenceError(fmt.Errorf("fetch collective observations: local Store or held-inventory adapter is not configured; no metadata installed; configure the Store and canonical held-manifest inventory")))
	}
	prior, err := s.store.ReadCollectiveObservations(ctx, selected)
	if err != nil {
		return s.initialFetchFailure(report, schema.CollectiveFetchSummary{Catalog: unknown, History: unknown, Associations: unknown}, s.now().UTC(), persistenceError(err))
	}
	report.CacheRevision = prior.CacheRevision
	started := s.now().UTC()
	snapshot, source, authErr := s.AuthenticatedSourceFor(ctx, selected)
	var invocation store.CollectiveObservationInvocation
	if authErr == nil {
		invocation, authErr = s.store.BeginCollectiveRead(ctx, snapshot, store.CollectiveReadPlan{Domains: schema.AllCollectiveFetchDomains}, s.now().UTC())
		authErr = persistenceError(authErr)
	}
	if authErr != nil {
		return s.initialFetchFailure(report, prior.Fetch, started, authErr)
	}
	// Register requested sets before delayed discovery. Inventory/read-issuance
	// failure stays local to associations; other complete domains can proceed.
	associationIDs, inventoryErr := s.localTranscriptIDs(ctx, scope)
	if inventoryErr == nil && len(associationIDs) > schema.MaxCollectiveFetchItems {
		inventoryErr = fmt.Errorf("issue association reads: held inventory exceeds metadata bound; no prefix requested; narrow held inventory")
	}
	if inventoryErr == nil {
		for _, id := range associationIDs {
			err := s.store.IssueCollectiveReads(ctx, invocation, store.CollectiveReadPlan{Transcripts: []schema.TranscriptID{id}}, s.now().UTC())
			if err != nil && !errors.Is(err, store.ErrObservationSuperseded) {
				inventoryErr = err
				break
			}
		}
	}
	discovery, discoveryErr := source.ReadOwnContributionDiscovery(ctx)
	discoveryResult := sourceOutcome(schema.CollectiveObservationSourceContributionDiscovery, s.now().UTC(), discoveryErr)
	// Independent domains must also make progress during an unrelated stalled
	// source, not only after it fails quickly. Each owns its result and stages
	// outside the writer. The shared source semaphore caps all HTTP reads at 4;
	// Store transactions still use the existing serial writer lane.
	var catalog, history, associations schema.CollectiveFetchDomainResult
	var domains sync.WaitGroup
	domains.Add(3)
	go func() {
		defer domains.Done()
		rows, sources, err := s.fetchCatalog(ctx, source, snapshot, discovery, discoveryResult, discoveryErr)
		if err == nil {
			catalog, err = s.store.CommitCollectiveCatalog(ctx, invocation, rows, sources, started, s.now().UTC())
			err = persistenceError(err)
		}
		if err != nil {
			catalog = s.failedDomain(ctx, invocation, schema.CollectiveFetchDomainCatalog, started, prior.Fetch.Catalog, sources, err)
		}
	}()
	go func() {
		defer domains.Done()
		pairs, sources, coverage, err := s.fetchHistory(ctx, source, invocation, discovery, discoveryResult, discoveryErr)
		if err == nil {
			history, err = s.store.CommitCollectiveHistory(ctx, invocation, pairs, sources, coverage, started, s.now().UTC())
			err = persistenceError(err)
		}
		if err != nil {
			history = s.failedDomain(ctx, invocation, schema.CollectiveFetchDomainHistory, started, prior.Fetch.History, sources, err)
		}
	}()
	go func() {
		defer domains.Done()
		associations = s.fetchAssociations(ctx, snapshot, source, invocation, associationIDs, inventoryErr, started, prior.Fetch.Associations)
	}()
	domains.Wait()
	report.Domains = append(report.Domains, catalog, history, associations)
	setFetchOutcome(&report)
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	if report.Outcome != schema.CollectiveFetchOutcomeComplete {
		for _, domain := range report.Domains {
			if domain.Issue == nil {
				continue
			}
			diagnostic := schema.LocalOperationDiagnostic{OperationID: report.InvocationID, Operation: schema.CollectiveOperationFetch, AttemptedAt: s.now().UTC(), Outcome: schema.LocalOperationDiagnosticFailed, Issue: *domain.Issue}
			if err := s.RecordDiagnostic(cleanup, snapshot, diagnostic); err != nil {
				report.Issues = append(report.Issues, operationIssue(persistenceError(err), schema.CollectiveOperationFetch))
			}
			break
		}
	}
	saved, saveErr := s.store.SaveCollectiveFetchReport(cleanup, invocation, report, s.now().UTC())
	if saveErr != nil {
		issue := operationIssue(persistenceError(saveErr), schema.CollectiveOperationFetch)
		report.Issues = append(report.Issues, issue)
		// Report finalization is part of the final domain's completion. Preserve
		// every actual per-set commit while truthfully reporting that this
		// invocation could not finish its local completion/reporting step.
		last := &report.Domains[len(report.Domains)-1]
		last.State = failureState(issue)
		last.CompletedAt = nil
		last.Issue = &issue
		if last.Associations.Completed == 0 {
			last.Commit = schema.CollectiveFetchCommitRetained
		}
		setFetchOutcome(&report)
	} else {
		report = saved
	}
	// A reporting failure must not rewrite complete domain effects. The report
	// outcome still describes those domains; HTTP precedence visits all issues.
	if err := report.Validate(); err != nil {
		return report, fmt.Errorf("validate fetch report after independent installs: %w", err)
	}
	if schema.CollectiveFetchHTTPStatus(report) != 200 {
		return report, &FetchFailure{Report: report}
	}
	return report, nil
}

func (s *Service) initialFetchFailure(report schema.CollectiveFetchReport, prior schema.CollectiveFetchSummary, started time.Time, cause error) (schema.CollectiveFetchReport, error) {
	issue := operationIssue(cause, schema.CollectiveOperationFetch)
	for _, domain := range schema.AllCollectiveFetchDomains {
		summary := prior.Catalog
		if domain == schema.CollectiveFetchDomainHistory {
			summary = prior.History
		}
		if domain == schema.CollectiveFetchDomainAssociations {
			summary = prior.Associations
		}
		at := s.now().UTC()
		r := schema.CollectiveFetchDomainResult{Domain: domain, Requested: true, State: failureState(issue), Commit: schema.CollectiveFetchCommitRetained, StartedAt: &started, LastAttemptAt: &at, LastCompletedAt: summary.LastCompletedAt, LastInstalledRevision: summary.LastInstalledRevision, Coverage: summary.Coverage, SourceOutcomes: []schema.CollectiveFetchSourceResult{}, Issue: &issue}
		if domain == schema.CollectiveFetchDomainAssociations {
			r.Associations = &schema.CollectiveFetchAssociationResults{Scopes: []schema.CollectiveFetchAssociationScopeResult{}}
		}
		report.Domains = append(report.Domains, r)
	}
	report.Issues = append(report.Issues, issue)
	return report, &FetchFailure{Report: report}
}

func setFetchOutcome(report *schema.CollectiveFetchReport) {
	complete := 0
	progress := false
	for _, domain := range report.Domains {
		if domain.State == schema.CollectiveFetchStateComplete {
			complete++
			progress = true
		}
		if domain.Commit != schema.CollectiveFetchCommitRetained {
			progress = true
		}
	}
	report.Outcome = schema.CollectiveFetchOutcomeFailed
	if complete == len(report.Domains) {
		report.Outcome = schema.CollectiveFetchOutcomeComplete
	} else if progress {
		report.Outcome = schema.CollectiveFetchOutcomePartial
	}
}

func (s *Service) fetchCatalog(ctx context.Context, source Source, snapshot auth.CredentialSnapshot, discovery []schema.VillageContributedCollective, discoveryResult schema.CollectiveFetchSourceResult, discoveryErr error) ([]schema.CollectiveObservation, []schema.CollectiveFetchSourceResult, error) {
	sources := []schema.CollectiveFetchSourceResult{discoveryResult}
	var failure error = discoveryErr
	rows := map[schema.VillageUUID]schema.CollectiveObservation{}
	add := func(id schema.VillageUUID, name string, description *string, reason schema.CollectiveObservationReason) {
		row, exists := rows[id]
		if !exists {
			row = schema.CollectiveObservation{ID: id, Name: name, Description: description, State: schema.CollectiveObservationStateObserved, ObservedAt: s.now().UTC(), Reasons: []schema.CollectiveObservationReason{}}
		}
		for _, present := range row.Reasons {
			if present == reason {
				return
			}
		}
		row.Reasons = append(row.Reasons, reason)
		rows[id] = row
	}
	members, err := source.ReadObservationMemberships(ctx)
	sources = append(sources, sourceOutcome(schema.CollectiveObservationSourceMembership, s.now().UTC(), err))
	if err != nil {
		failure = errors.Join(failure, err)
	}
	for _, row := range members {
		add(row.ID, row.Name, row.Description, schema.CollectiveObservationReasonMembership)
	}
	for _, row := range discovery {
		if row.ApprovedCount > 0 || row.PendingCount > 0 {
			add(row.ID, row.Name, row.Description, schema.CollectiveObservationReasonContribution)
		}
	}
	orgs, orgErr := source.ReadObservationOrganizations(ctx)
	if orgErr == nil {
		for _, org := range orgs {
			linked, err := source.ReadOrganizationCollectives(ctx, org.OrgLogin)
			if err != nil {
				orgErr = err
				break
			}
			for _, row := range linked {
				add(row.ID, row.Name, row.Description, schema.CollectiveObservationReasonOrganization)
			}
			if len(rows) > schema.MaxCollectiveFetchItems {
				orgErr = &observationSourceContractError{error: fmt.Errorf("organization relevance exceeds the complete catalog bound; no prefix installed; narrow explicitly selected sources")}
				break
			}
		}
	}
	sources = append(sources, sourceOutcome(schema.CollectiveObservationSourceOrganization, s.now().UTC(), orgErr))
	failure = errors.Join(failure, orgErr)
	tracked, trackErr := s.store.CollectiveTrackingIDs(ctx, snapshot)
	if trackErr == nil {
		for _, id := range tracked {
			add(id, "", nil, schema.CollectiveObservationReasonExplicit)
		}
	} else {
		trackErr = persistenceError(trackErr)
	}
	if len(rows) > schema.MaxCollectiveFetchItems {
		trackErr = &observationSourceContractError{error: fmt.Errorf("complete catalog exceeds metadata item bound; no prefix installed; narrow explicit sources")}
	}
	ids := make([]schema.VillageUUID, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if trackErr == nil && failure == nil {
		for _, id := range ids {
			row := rows[id]
			detail, err := source.ReadObservationCollective(ctx, id)
			if negativeDisclosure(err) {
				row.State = schema.CollectiveObservationStateUnavailable
				rows[id] = row
				continue
			}
			if err != nil {
				trackErr = err
				break
			}
			rows[id] = observationFromDetail(detail, row.Reasons, s)
		}
	}
	sources = append(sources, sourceOutcome(schema.CollectiveObservationSourceExplicit, s.now().UTC(), trackErr))
	failure = errors.Join(failure, trackErr)
	result := make([]schema.CollectiveObservation, 0, len(rows))
	for _, id := range ids {
		result = append(result, rows[id])
	}
	return result, sources, failure
}

func (s *Service) fetchHistory(ctx context.Context, source Source, invocation store.CollectiveObservationInvocation, discovery []schema.VillageContributedCollective, discoveryResult schema.CollectiveFetchSourceResult, discoveryErr error) ([]store.ContributionAttemptSource, []schema.CollectiveFetchSourceResult, schema.CollectiveEvidenceCoverage, error) {
	sources := []schema.CollectiveFetchSourceResult{discoveryResult}
	pairs := []store.ContributionAttemptSource{}
	coverage := schema.CollectiveEvidenceCoverageAvailable
	if discoveryErr != nil {
		return pairs, sources, coverage, discoveryErr
	}
	items := 0
	for _, collective := range discovery {
		rows, err := source.ReadOwnSubmissions(ctx, collective.ID)
		if negativeDisclosure(err) {
			r := sourceOutcome(schema.CollectiveObservationSourceSubmissions, s.now().UTC(), nil)
			r.Outcome = schema.CollectiveSourceNotDisclosed
			sources = append(sources, r)
			coverage = schema.CollectiveEvidenceCoverageUnknown
			continue
		}
		sources = append(sources, sourceOutcome(schema.CollectiveObservationSourceSubmissions, s.now().UTC(), err))
		if err != nil {
			return nil, sources, coverage, err
		}
		seen := map[schema.TranscriptID]bool{}
		for _, row := range rows {
			if seen[row.TranscriptID] {
				continue
			}
			seen[row.TranscriptID] = true
			if err := s.store.IssueCollectiveReads(ctx, invocation, store.CollectiveReadPlan{Sources: []store.CollectiveReadSource{store.ContributionAttemptReadSource(collective.ID, row.TranscriptID)}}, s.now().UTC()); err != nil {
				return nil, sources, coverage, persistenceError(err)
			}
			events, err := source.ReadOwnShareEvents(ctx, row.TranscriptID, collective.ID)
			if negativeDisclosure(err) {
				r := sourceOutcome(schema.CollectiveObservationSourceAttempts, s.now().UTC(), nil)
				r.Outcome = schema.CollectiveSourceNotDisclosed
				sources = append(sources, r)
				coverage = schema.CollectiveEvidenceCoverageUnknown
				continue
			}
			sources = append(sources, sourceOutcome(schema.CollectiveObservationSourceAttempts, s.now().UTC(), err))
			if err != nil {
				return nil, sources, coverage, err
			}
			items += len(events)
			if items > schema.MaxCollectiveFetchItems || len(sources) > schema.MaxCollectiveFetchItems-1 {
				return nil, sources[:1], coverage, &observationSourceContractError{error: fmt.Errorf("available history exceeds the complete invocation item bound; no partial history installed; narrow configured history sources before retrying")}
			}
			pairs = append(pairs, store.ContributionAttemptSource{CollectiveID: collective.ID, TranscriptID: row.TranscriptID, Events: events, ObservedAt: s.now().UTC()})
		}
	}
	return pairs, sources, coverage, nil
}

func (s *Service) fetchAssociations(ctx context.Context, snapshot auth.CredentialSnapshot, source Source, invocation store.CollectiveObservationInvocation, ids []schema.TranscriptID, inventoryErr error, started time.Time, prior schema.CollectiveFetchDomainSummary) schema.CollectiveFetchDomainResult {
	at := s.now().UTC()
	result := schema.CollectiveFetchDomainResult{Domain: schema.CollectiveFetchDomainAssociations, Requested: true, State: schema.CollectiveFetchStateComplete, Commit: schema.CollectiveFetchCommitUnchanged, StartedAt: &started, LastAttemptAt: &at, CompletedAt: &at, LastCompletedAt: prior.LastCompletedAt, LastInstalledRevision: prior.LastInstalledRevision, Coverage: schema.CollectiveEvidenceCoverageAvailable, SourceOutcomes: []schema.CollectiveFetchSourceResult{}, Associations: &schema.CollectiveFetchAssociationResults{Scopes: []schema.CollectiveFetchAssociationScopeResult{}}}
	err := inventoryErr
	if err == nil && len(ids) > schema.MaxCollectiveFetchItems {
		err = fmt.Errorf("local transcript inventory exceeds metadata invocation bound; no prefix was selected; narrow the held inventory before retrying")
	}
	if err != nil {
		issue := operationIssue(persistenceError(err), schema.CollectiveOperationFetch)
		result.Issue = &issue
		result.State = failureState(issue)
		result.CompletedAt = nil
		result.Commit = schema.CollectiveFetchCommitRetained
	} else {
		seen := map[schema.TranscriptID]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			result.Associations.Requested++
			metadata, err := s.observeAssociations(ctx, snapshot, source, invocation, id)
			item := schema.CollectiveFetchAssociationScopeResult{TranscriptID: id, State: schema.CollectiveFetchStateComplete, Commit: schema.CollectiveFetchCommitUpdated, ObservedAt: metadata.ObservedAt, CompletedAt: metadata.ObservedAt}
			if err != nil {
				issue := operationIssue(err, schema.CollectiveOperationFetch)
				item.State = failureState(issue)
				item.Commit = schema.CollectiveFetchCommitRetained
				item.CompletedAt = nil
				item.Issue = &issue
				result.Associations.Failed++
				result.State = failureState(issue)
				result.CompletedAt = nil
				if result.Issue == nil {
					result.Issue = &issue
				}
			} else {
				result.Associations.Completed++
				result.Commit = schema.CollectiveFetchCommitUpdated
			}
			result.Associations.Scopes = append(result.Associations.Scopes, item)
		}
		if result.Associations.Failed > 0 {
			result.Coverage = prior.Coverage
			if result.Associations.Completed == 0 {
				result.Commit = schema.CollectiveFetchCommitRetained
			}
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	saved, err := s.store.RecordCollectiveAssociationDomain(cleanup, invocation, result)
	if err != nil {
		issue := operationIssue(persistenceError(err), schema.CollectiveOperationFetch)
		result.State = failureState(issue)
		result.Issue = &issue
		result.CompletedAt = nil
		if result.Associations.Completed == 0 {
			result.Commit = schema.CollectiveFetchCommitRetained
		}
		return result
	}
	return saved
}
