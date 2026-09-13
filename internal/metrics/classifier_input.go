package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// Classifiers consume a single database view. The write transaction validates
// that view again before publishing outputs and marking the pass complete.
func (ca *ClassifierAnnotator) prepareCapturedAnnotations(ctx context.Context, backing ingest.MetricInputStore, sid ingest.SessionID, stats *ingest.AnnotationProfileStats) (ingest.SessionAnnotationBatch, error) {
	batch := ingest.SessionAnnotationBatch{SessionID: sid}
	if _, ok := ca.annotationStore.(ingest.ClassifierAnnotationSessionBatchStore); !ok {
		return batch, fmt.Errorf("classify session %s: annotation store cannot save captured input atomically; use the persistent Store before retrying", sid)
	}
	started := annotationProfileStart(stats)
	input, err := backing.ReadMetricInput(ctx, sid, true)
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
		s.GetMetricsCount++
		s.ListEntriesCount++
		s.ListEntriesTime += time.Since(started)
	})
	if err != nil {
		return batch, err
	}
	if !MetricsCurrentForInput(input) {
		return batch, fmt.Errorf("classify session %s: metrics do not prove the captured input current; prior annotations were preserved; refresh metrics before retrying", sid)
	}
	if input.IndexState == nil || input.IndexState.SessionEntriesHash == nil || *input.IndexState.SessionEntriesHash == "" {
		return batch, fmt.Errorf("classify session %s: captured entries lack index completion evidence; index the session before retrying", sid)
	}
	entriesHash := *input.IndexState.SessionEntriesHash
	outputHash, err := ingest.MetricOutputHash(input.Existing)
	if err != nil {
		return batch, err
	}
	state := classifierAnnotationRunState(sid, entriesHash, *input.Existing.ComputeVersion)
	state.MetricsOutputHash = outputHash
	if stateStore, ok := ca.metricsStore.(ingest.AnnotationRunStateStore); ok {
		prior, err := stateStore.GetAnnotationRunState(ctx, sid)
		if err != nil {
			return batch, err
		}
		if prior != nil && prior.ClassifierVersion > CurrentClassifierAnnotationVersion {
			return batch, fmt.Errorf("classify session %s: stored classifier version is newer; upgrade Peasant before retrying", sid)
		}
		if prior != nil && prior.SessionEntriesHash == entriesHash && prior.MetricsOutputHash == outputHash && prior.ComputeVersion == state.ComputeVersion && prior.ClassifierVersion == state.ClassifierVersion {
			batch.Skipped = true
			addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) { s.StateSkipCount++ })
			return batch, nil
		}
	}
	// Resolve every configured owner before running, including classifiers that
	// will return no output. Preparation failures must not retire prior results.
	addOwner := func(typeID string) error {
		ids, err := ca.resolveClassifierIDs(ctx, typeID, stats)
		if err != nil {
			return err
		}
		batch.Owners = append(batch.Owners, ingest.ClassifierAnnotationOwner{AnnotatorID: ids.annotatorID, AnnotationTypeID: ids.annotationTypeID})
		return nil
	}
	for _, classifier := range ca.engine.classifiers {
		if err := addOwner(classifier.name); err != nil {
			return batch, err
		}
	}
	for _, classifier := range ca.engine.entryClassifiers {
		if err := addOwner(classifier.name); err != nil {
			return batch, err
		}
	}
	started = annotationProfileStart(stats)
	results, times := ca.runClassifiers(ctx, sid, input.Entries, input.Existing, stats != nil)
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
		s.ClassifierRunCount++
		s.ClassifierRunTime += time.Since(started)
		s.ResultCount += len(results)
	})
	for _, result := range results {
		write, err := ca.classifierAnnotationWrite(ctx, sid, result, stats)
		if err != nil {
			return batch, err
		}
		batch.Writes = append(batch.Writes, ingest.SessionAnnotationWrite{Write: write, TypeID: result.TypeID, Value: result.Value, TargetKind: classifierResultTargetKind(result), ClassifierTime: classifierResultProfileTime(times, result)})
	}
	if err := ctx.Err(); err != nil {
		return batch, err
	}
	batch.Input, batch.RunState = input, &state
	return batch, nil
}
