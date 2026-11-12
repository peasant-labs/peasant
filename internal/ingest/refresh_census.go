package ingest

import (
	"cmp"
	"fmt"
	"slices"
)

// RefreshLane names the path a refresh takes. Each refresh reason maps to
// exactly one lane, so the lane is derived and never stored.
type RefreshLane string

const (
	RefreshLaneNative         RefreshLane = "native"
	RefreshLaneStoredMetadata RefreshLane = "stored-metadata"
	RefreshLaneStoredIndex    RefreshLane = "stored-index"
)

// AllRefreshLanes lists every lane a refresh can take.
var AllRefreshLanes = []RefreshLane{RefreshLaneNative, RefreshLaneStoredMetadata, RefreshLaneStoredIndex}

// NewRefreshLane validates a raw lane name.
func NewRefreshLane(raw string) (RefreshLane, error) {
	if lane := RefreshLane(raw); slices.Contains(AllRefreshLanes, lane) {
		return lane, nil
	}
	return "", fmt.Errorf("ingest: read refresh lane %q: this build defines no such lane; nothing was recorded; use native, stored-metadata or stored-index", raw)
}

// RefreshLane returns the lane of a refresh reason, or "" when the reason is
// not a refresh.
func (r OutcomeReason) RefreshLane() RefreshLane {
	switch r {
	case OutcomeReasonRefreshNew, OutcomeReasonRefreshSourceChanged, OutcomeReasonRefreshPairMissing,
		OutcomeReasonRefreshMetadataSchema, OutcomeReasonRefreshForced, OutcomeReasonRefreshAdapterBehindNativeOnly:
		return RefreshLaneNative
	case OutcomeReasonRefreshAdapterBehind, OutcomeReasonRefreshAdapterStampAbsent:
		return RefreshLaneStoredMetadata
	case OutcomeReasonRefreshIndexerBehind, OutcomeReasonRefreshRefusalReopened, OutcomeReasonRefreshPublicationUnbound:
		return RefreshLaneStoredIndex
	default:
		return ""
	}
}

// RefreshCensusRow counts the sessions one run refreshed for one reason.
// SourceBytes sums the bytes the indexer parsed; it is zero for a refresh
// that did not index.
type RefreshCensusRow struct {
	Reason      OutcomeReason
	Lane        RefreshLane
	Sessions    int
	SourceBytes int64
}

// RefreshCensus is the per-reason count of the sessions a run refreshed. An
// unchanged session is not in it.
type RefreshCensus struct {
	Rows []RefreshCensusRow
}

// Sessions returns the number of refreshed sessions across every reason.
func (c RefreshCensus) Sessions() int {
	total := 0
	for _, row := range c.Rows {
		total += row.Sessions
	}
	return total
}

// markRefreshReason names why a session is refreshed where the pipeline knows
// more than the session's final status tells. The first name wins.
func (p *Pipeline) markRefreshReason(sid SessionID, reason OutcomeReason) {
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	if p.refreshReasons == nil {
		p.refreshReasons = make(map[SessionID]OutcomeReason)
	}
	if _, named := p.refreshReasons[sid]; !named {
		p.refreshReasons[sid] = reason
	}
}

// refreshReasonFor names the reason of one processed session from the facts
// the pipeline holds today: the session's status, the run's mode, and the
// maintenance pass that queued it.
func (p *Pipeline) refreshReasonFor(result SessionResult, named map[SessionID]OutcomeReason) OutcomeReason {
	switch {
	case p.config.Force:
		// A forced run reports every session it processes as new, so the
		// status cannot tell a first import from a forced refresh.
		return OutcomeReasonRefreshForced
	case result.Status == DiffNew:
		return OutcomeReasonRefreshNew
	case named[result.SessionID] != 0:
		return named[result.SessionID]
	case p.config.Reindex:
		return OutcomeReasonRefreshIndexerBehind
	default:
		return OutcomeReasonRefreshSourceChanged
	}
}

// refreshOutcomes returns one refresh row per refreshed session: every
// processed session, and every session whose stored index was rewritten
// without a native pass. An unchanged or excluded session has no row.
func (p *Pipeline) refreshOutcomes(results []SessionResult, indexLog []IndexLogEntry, createdAt int64) []RunOutcome {
	p.diagnosticsMu.Lock()
	named := p.refreshReasons
	p.diagnosticsMu.Unlock()
	seen := make(map[SessionID]bool, len(results))
	var outcomes []RunOutcome
	for _, result := range results {
		if result.SessionID == "" || result.ExcludedBySelection || result.Status == DiffUnchanged || seen[result.SessionID] {
			continue
		}
		seen[result.SessionID] = true
		outcomes = append(outcomes, RunOutcome{SessionID: result.SessionID, Reason: p.refreshReasonFor(result, named), CreatedAt: createdAt})
	}
	for _, entry := range indexLog {
		if entry.SessionID == "" || seen[entry.SessionID] {
			continue
		}
		if entry.Outcome != IndexOutcomeIndexed && entry.Outcome != IndexOutcomeReindexed && entry.Outcome != IndexOutcomeFallback {
			continue
		}
		seen[entry.SessionID] = true
		reason := OutcomeReasonRefreshIndexerBehind
		if p.config.Force {
			reason = OutcomeReasonRefreshForced
		}
		outcomes = append(outcomes, RunOutcome{SessionID: entry.SessionID, Reason: reason, CreatedAt: createdAt})
	}
	return outcomes
}

// refreshCensus counts the refresh rows of a run by reason. It reads the same
// rows the audit stores, so the printed census and the stored one agree.
func refreshCensus(outcomes []RunOutcome, indexLog []IndexLogEntry) RefreshCensus {
	parsed := make(map[SessionID]int64, len(indexLog))
	for _, entry := range indexLog {
		if entry.Bytes != nil {
			parsed[entry.SessionID] += *entry.Bytes
		}
	}
	rows := make(map[OutcomeReason]*RefreshCensusRow)
	for _, outcome := range outcomes {
		if outcome.Reason.Kind() != OutcomeKindRefresh {
			continue
		}
		row := rows[outcome.Reason]
		if row == nil {
			row = &RefreshCensusRow{Reason: outcome.Reason, Lane: outcome.Reason.RefreshLane()}
			rows[outcome.Reason] = row
		}
		row.Sessions++
		row.SourceBytes += parsed[outcome.SessionID]
	}
	census := RefreshCensus{Rows: make([]RefreshCensusRow, 0, len(rows))}
	for _, row := range rows {
		census.Rows = append(census.Rows, *row)
	}
	slices.SortFunc(census.Rows, func(a, b RefreshCensusRow) int { return cmp.Compare(a.Reason, b.Reason) })
	return census
}
