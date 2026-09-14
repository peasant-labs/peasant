package ingest

import (
	"context"
	"fmt"
)

// IndexCoverage splits the sessions whose index attempt ended in error with
// no completion into the sessions that hold no stored entries and the
// sessions that kept their previous entries.
//
// A nil *IndexCoverage means unavailable, never zero: the store could not
// answer, or a membership chunk failed. Whenever coverage is non-nil,
// FailedAttempts equals Empty plus FailedRetained.
type IndexCoverage struct {
	// FailedAttempts counts the sessions whose attempt ended in error with
	// no completion (FailedIndexSessions).
	FailedAttempts int `json:"failedAttempts"`
	// Empty counts the failed sessions with no session_entries rows.
	Empty int `json:"empty"`
	// FailedRetained counts the failed sessions that still hold entries.
	FailedRetained int `json:"failedRetained"`
}

// ComputeIndexCoverage folds the failed sessions against the store's
// entries-membership answer. The failed set must already be classified and
// distinct (FailedIndexSessions); an empty set answers a measured zero
// without consulting the store.
//
// A nil reader or any membership error leaves the answer unavailable (nil,
// never zero) with an actionable error: the caller reports one nonfatal
// diagnostic and prints no partial count. A session the answer does not name
// counts as retained: emptiness is only ever reported on the store's explicit
// word, never on an absence.
func ComputeIndexCoverage(ctx context.Context, reader IndexCoverageReader, failed []SessionID) (*IndexCoverage, error) {
	coverage := &IndexCoverage{FailedAttempts: len(failed)}
	if len(failed) == 0 {
		return coverage, nil
	}
	if reader == nil {
		return nil, fmt.Errorf("index coverage unavailable: the store cannot report which of %d failed session(s) kept their entries (no entries-membership capability); when=finalize; impact=the summary omits the empty/retained breakdown instead of guessing; fix=run the harvest with the standard analytics store, which answers stored-entry membership", len(failed))
	}
	without, err := reader.SessionsWithoutEntries(ctx, failed)
	if err != nil {
		return nil, fmt.Errorf("index coverage unavailable: could not check stored entries for %d failed session(s): %w; when=finalize; impact=the summary omits the empty/retained breakdown instead of guessing; fix=re-run the harvest, and if the failure persists check the analytics store is readable", len(failed), err)
	}
	for _, id := range failed {
		if without[id] {
			coverage.Empty++
		} else {
			coverage.FailedRetained++
		}
	}
	return coverage, nil
}

// indexCoverageReader returns the entries-membership capability the pipeline
// finalizes through: the metrics store owns the session_entries rows, so it
// answers first, and the session store covers test doubles that carry the
// capability on the other handle. Nil means no handle answers.
func (p *Pipeline) indexCoverageReader() IndexCoverageReader {
	if reader, ok := p.metricsStore.(IndexCoverageReader); ok {
		return reader
	}
	if reader, ok := p.store.(IndexCoverageReader); ok {
		return reader
	}
	return nil
}

// resolveIndexCoverage computes the run's coverage from its index log, or
// reports the one actionable nonfatal diagnostic and answers nil when the
// store cannot answer. It never returns a partial count.
func (p *Pipeline) resolveIndexCoverage(ctx context.Context, log []IndexLogEntry, logPrefix string) *IndexCoverage {
	coverage, err := ComputeIndexCoverage(ctx, p.indexCoverageReader(), FailedIndexSessions(log))
	if err != nil {
		p.reportDiagnostic(DiagnosticEntry{
			ErrorType:   "index_coverage_unavailable",
			Location:    logPrefix + " finalize",
			Message:     err.Error(),
			Remediation: "Re-run the harvest; if the warning persists, check the analytics store is readable. The failed sessions are still listed per session with their reasons in the JSON output.",
		})
		return nil
	}
	return coverage
}
