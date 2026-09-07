package ingest

import "context"

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
		p.appendDiagnostic(DiagnosticEntry{
			ErrorType:   "metrics_incomplete",
			Location:    "metrics after indexing",
			Message:     err.Error(),
			Remediation: "Resolve the reported cause, then run peasant harvest index --force for the affected sessions to retry computation.",
		})
	}
	return computed, err
}
