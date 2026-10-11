package ingest

import (
	"context"
	"fmt"
)

// stageAndCommitNativeBatches keeps CPU preparation parallel while every stage
// and activation transaction runs on the serial writer lane. Both windows use
// the common splitter, with their own configured session caps.
func (p *Pipeline) stageAndCommitNativeBatches(ctx context.Context, results []indexParseResult, positions []int, outcome IndexOutcome, prefix string, lane *storeWriteLane, record func(int, indexedMeta, IndexLogEntry, IndexProfileSession), stager NativeGenerationBatchStager, activator NativeGenerationBatchActivator) {
	ctx = WithWriteAdvisoryReporter(ctx, p.reportWriteDiagnostic)
	jobs := runParallel(func() error {
		return nil
	}, positions, parallelWorkers(p.config), func(position int) nativeGenerationCommitJob {
		return nativeGenerationCommitJob{position: position, commit: p.prepareNativeGenerationResult(results[position], outcome, prefix)}
	})
	var ready []nativeGenerationCommitJob
	for _, job := range jobs {
		if job.commit.ready {
			ready = append(ready, job)
		} else if record != nil {
			record(job.position, job.commit.im, job.commit.logEntry, job.commit.profile)
		}
	}
	cfg := p.writeConfig()
	// Stage windows are independent of activation windows. Handles let a stage
	// batch feed several smaller activation transactions without re-staging.
	for start := 0; start < len(ready); {
		end := start
		var bytes int64
		for end < len(ready) {
			n := nativeCandidateWriteBytes(ready[end].commit.prepared.result.nativeCandidate)
			if ExceedsWriteBudget(cfg, end-start, bytes, n, cfg.BatchSessions) {
				break
			}
			bytes += n
			end++
		}
		window := ready[start:end]
		activations := make([]NativeGenerationActivation, len(window))
		for i, job := range window {
			activations[i] = job.commit.prepared.activation
		}
		var staged []NativeGenerationStaged
		p.runStoreWrite(lane, func() {
			staged, _ = stager.StageNativeGenerations(ctx, activations)
		})
		for i := range window {
			if i < len(staged) {
				window[i].commit.prepared.staged = staged[i]
			}
		}
		for a := 0; a < len(window); {
			b := a
			bytes = 0
			for b < len(window) {
				n := nativeCandidateWriteBytes(window[b].commit.prepared.result.nativeCandidate)
				if ExceedsWriteBudget(cfg, b-a, bytes, n, cfg.ActivationSessions) {
					break
				}
				bytes += n
				b++
			}
			p.commitNativeBatch(ctx, window[a:b], outcome, prefix, lane, record, activator)
			a = b
		}
		start = end
	}
}

func (p *Pipeline) commitNativeBatch(ctx context.Context, jobs []nativeGenerationCommitJob, outcome IndexOutcome, prefix string, lane *storeWriteLane, record func(int, indexedMeta, IndexLogEntry, IndexProfileSession), activator NativeGenerationBatchActivator) {
	var admitted []nativeGenerationCommitJob
	var activations []NativeGenerationActivation
	var staged []NativeGenerationStaged
	var outcomes []NativeActivationResult
	p.runStoreWrite(lane, func() {
		// Hold each input guard through the batch call. A refusal excludes only
		// that candidate; the store still applies its own per-session C0 CAS.
		var guard func(int)
		guard = func(i int) {
			if i == len(jobs) {
				if len(activations) > 0 {
					p.config.IndexProfiler.RecordActivationSize(len(activations))
					outcomes = activator.ActivateStagedNativeGenerations(ctx, activations, staged)
				}
				return
			}
			job := jobs[i]
			err := p.withCurrentIndexInput(ctx, job.commit.prepared.result.input, func() error {
				admitted = append(admitted, job)
				activations = append(activations, job.commit.prepared.activation)
				staged = append(staged, job.commit.prepared.staged)
				guard(i + 1)
				return nil
			})
			if err != nil {
				im, log, profile := p.finishNativeGenerationResult(ctx, job.commit.prepared, outcome, prefix, ActivationOutcome{}, err)
				if record != nil {
					record(job.position, im, log, profile)
				}
				guard(i + 1)
			}
		}
		guard(0)
	})
	for i, job := range admitted {
		r := NativeActivationResult{Err: fmt.Errorf("%s: activation batch returned %d results for %d candidates; retry harvest to reconcile the committed index", prefix, len(outcomes), len(admitted))}
		if len(outcomes) == len(admitted) {
			r = outcomes[i]
		}
		im, log, profile := p.finishNativeGenerationResult(ctx, job.commit.prepared, outcome, prefix, r.Outcome, r.Err)
		if record != nil {
			record(job.position, im, log, profile)
		}
	}
}
