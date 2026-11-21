package ingest

import "context"

// preparedAnnotationOutcome is the terminal outcome of a prepared batch that
// needs no flush. It reports false when the batch must flush first.
func preparedAnnotationOutcome(batch SessionAnnotationBatch) (AnnotationSessionOutcome, bool) {
	switch {
	case batch.Skipped:
		return AnnotationSessionSkipped, true
	case len(batch.Writes) == 0 && batch.RunState == nil:
		if batch.PreparationFailed {
			return AnnotationSessionPreparationFailed, true
		}
		return AnnotationSessionAnnotated, true
	}
	return 0, false
}

// flushedAnnotationOutcome is the terminal outcome of a flushed batch. A
// preparation failure takes precedence over a flush failure, so a session that
// failed in both phases counts once.
func flushedAnnotationOutcome(batch SessionAnnotationBatch, result SessionAnnotationBatchResult) AnnotationSessionOutcome {
	if batch.PreparationFailed {
		return AnnotationSessionPreparationFailed
	}
	if result.Err != nil {
		return AnnotationSessionPersistenceFailed
	}
	for _, write := range result.Results {
		if write.Err != nil {
			return AnnotationSessionPersistenceFailed
		}
	}
	return AnnotationSessionAnnotated
}

// unbufferedAnnotationOutcome is the terminal outcome of a classifier that
// reports one error for the whole session.
func unbufferedAnnotationOutcome(err error) AnnotationSessionOutcome {
	if err != nil {
		return AnnotationSessionPreparationFailed
	}
	return AnnotationSessionAnnotated
}

// recordAnnotationOutcome records one candidate's terminal outcome. A failure
// that ends an attempt after the run was cancelled is not recorded: the
// candidate stays unfinished rather than counting as failed work.
func (p *Pipeline) recordAnnotationOutcome(ctx context.Context, outcome AnnotationSessionOutcome) {
	failed := outcome == AnnotationSessionPreparationFailed || outcome == AnnotationSessionPersistenceFailed
	if failed && ctx.Err() != nil {
		return
	}
	p.config.IndexProfiler.RecordAnnotationOutcome(outcome)
}
