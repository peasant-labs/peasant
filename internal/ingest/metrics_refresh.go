package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func (p *Pipeline) hasStoredMetricRefresh() bool {
	_, sessions := p.metricsStore.(MetricSessionReader)
	_, metrics := p.analyzer.(SessionMetricsEnsurer)
	return !p.config.DryRun && sessions && metrics
}

// refreshStoredMetrics runs after the index writers have drained. Paging keeps
// the cohort bounded; only explicit invocation filters scope stored maintenance.
func (p *Pipeline) refreshStoredMetrics(ctx context.Context) (computed, checked, annotated int, days map[string]bool) {
	days = make(map[string]bool)
	reader := p.metricsStore.(MetricSessionReader)
	engine := p.analyzer.(SessionMetricsEnsurer)
	var after SessionID
	for ctx.Err() == nil {
		page, err := reader.ListMetricSessions(ctx, after, 256)
		if err != nil {
			p.reportMetricFailure("stored session page", err)
			break
		}
		if len(page) == 0 {
			break
		}
		var ready []SessionID
		for _, session := range page {
			after = session.SessionID
			if ctx.Err() != nil {
				break
			}
			if p.config.Harness != nil && session.Harness != *p.config.Harness ||
				p.config.AllowedSessionIDs != nil && !p.config.AllowedSessionIDs[session.SessionID] ||
				p.config.Since != nil && time.UnixMilli(session.StartMS).Before(*p.config.Since) {
				continue
			}
			checked++
			if err := p.checkStoredMetadataVersion(ctx, session.SessionID); err != nil {
				p.reportMetadataRefusal(string(session.SessionID), err)
				continue
			}
			// A session whose stored producer or index format is newer than this
			// build is refused for indexing; its derived rows are left alone too,
			// so a future producer's output is never reinterpreted by an older
			// metrics algorithm. The refusal is the same visible diagnostic the
			// index path reports, deduplicated when both paths see the session.
			if reader, ok := p.metricsStore.(SessionIndexStateReader); ok {
				state, err := reader.ReadIndexState(ctx, session.SessionID)
				if err != nil {
					p.reportMetricFailure(string(session.SessionID), err)
					continue
				}
				if state != nil {
					if err := p.checkIndexProducer(state); err != nil {
						p.reportIndexRefusal(session.SessionID, err)
						continue
					}
				}
			}
			changed, current, err := engine.EnsureSessionMetrics(ctx, session.SessionID)
			if err != nil {
				p.reportMetricFailure(string(session.SessionID), err)
				continue
			}
			if changed {
				computed++
				if session.StartMS > 0 {
					days[time.UnixMilli(session.StartMS).UTC().Format("2006-01-02")] = true
				}
			}
			if !current || p.classifier == nil {
				continue
			}
			ready = append(ready, session.SessionID)
		}
		// Classifier preparation independently revalidates the captured input;
		// a metrics failure cannot authorize classification of last-good values.
		if len(ready) > 0 && ctx.Err() == nil {
			if err := p.stageAnnotate(ctx, ready, nil); err != nil {
				slog.Warn("harvest: annotate stored sessions", "error", err)
			}
			annotated += len(ready)
		}
	}
	return
}

func (p *Pipeline) reportMetricFailure(location string, err error) {
	p.reportDiagnostic(DiagnosticEntry{
		ErrorType: "metrics_incomplete", Location: location, Message: err.Error(),
		Remediation: "Resolve the reported cause, then run peasant harvest again to retry this session's metrics and annotations.",
	})
	slog.Warn("harvest: stored metrics remain retryable", "location", location, "error", err)
}

// computeReadyMetrics retains injected analyzer compatibility while production
// engines report a separate confirmed-current result for every requested ID.
func (p *Pipeline) computeReadyMetrics(ctx context.Context, ids []SessionID) (int, []SessionID, error) {
	if engine, ok := p.analyzer.(SessionMetricsEnsurer); ok {
		var computed int
		var ready []SessionID
		for _, sid := range ids {
			changed, current, err := engine.EnsureSessionMetrics(ctx, sid)
			if err != nil {
				p.reportMetricFailure(string(sid), err)
				continue
			}
			if changed {
				computed++
			}
			if current {
				ready = append(ready, sid)
			}
		}
		return computed, ready, nil
	}
	n, err := p.computeIndexedMetrics(ctx, ids)
	// Legacy injected analyzers have no input-proof contract. Their classifier
	// must establish its own freshness before writing, so a failed batch does
	// not withdraw its sessions from annotation: withdrawing them would both
	// take the freshness decision away from the classifier that owns it and
	// leave the ANNOTATE stage reporting no work for sessions it accounted for.
	// The failure is still reported and no session counts as computed.
	if err != nil {
		return n, ids, fmt.Errorf("metrics prerequisite was not confirmed: %w", err)
	}
	return n, ids, nil
}

// computeIndexedMetrics refreshes this invocation's successful index targets.
// Ordinary maintenance retains the analyzer's existing version-skip behavior.
// Reporting is retained independently from logging for interactive callers.
func (p *Pipeline) computeIndexedMetrics(ctx context.Context, sessionIDs []SessionID) (int, error) {
	var computed int
	var err error
	if recomputer, ok := p.analyzer.(MetricsRecomputer); ok {
		computed, err = recomputer.RecomputeMetrics(ctx, sessionIDs)
	} else {
		computed, err = p.analyzer.ComputeMetrics(ctx, sessionIDs)
	}
	if err != nil {
		p.reportDiagnostic(DiagnosticEntry{
			ErrorType:   "metrics_incomplete",
			Location:    "metrics after indexing",
			Message:     err.Error(),
			Remediation: "Resolve the reported cause, then run peasant harvest index --force for the affected sessions to retry computation.",
		})
	}
	return computed, err
}
