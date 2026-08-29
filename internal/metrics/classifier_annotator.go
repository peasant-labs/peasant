package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// Annotator names — must match seed data in internal/store/schema.go seedAnnotators().
const (
	// CurrentClassifierAnnotationVersion tracks classifier annotation logic. Bump
	// this when classifier results or persistence semantics change so unchanged
	// entry projections are annotated again with the new rules.
	CurrentClassifierAnnotationVersion = 1

	annotatorNameOutcome     = "outcome-classifier"
	annotatorNameFrustration = "frustration-classifier"
	annotatorNameScope       = "scope-classifier"

	// Entry-level classifier annotator names (V18 migration).
	annotatorNameFrustrationSignal  = "frustration-signal-classifier"
	annotatorNameResolutionEvidence = "resolution-evidence-classifier"
)

// typeAnnotatorNames maps each classifier's TypeID to its seeded annotator name.
// These names match the rows seeded by the V13/V18 migrations in store/schema.go.
var typeAnnotatorNames = map[string]string{
	typeIDSessionOutcome:     annotatorNameOutcome,
	typeIDUserFrustration:    annotatorNameFrustration,
	typeIDSessionScope:       annotatorNameScope,
	typeIDFrustrationSignal:  annotatorNameFrustrationSignal,
	typeIDResolutionEvidence: annotatorNameResolutionEvidence,
}

// ClassifierAnnotator implements ingest.SessionClassifier.
// It runs the ClassifierEngine over a session's entries and metrics, then
// persists non-nil results as session annotations via AnnotationStore.
//
// Annotate is best-effort: per-result errors are logged and skipped.
type ClassifierAnnotator struct {
	metricsStore    ingest.MetricsStore
	annotationStore ingest.AnnotationStore
	engine          *ClassifierEngine
	idsMu           sync.Mutex
	idsByType       map[string]classifierAnnotationIDs
}

type classifierAnnotationIDs struct {
	annotatorID      string
	annotationTypeID string
}

// Compile-time guard: *ClassifierAnnotator must satisfy ingest.SessionClassifier.
var _ ingest.SessionClassifier = (*ClassifierAnnotator)(nil)
var _ ingest.ProfiledSessionClassifier = (*ClassifierAnnotator)(nil)

// NewClassifierAnnotator constructs a ClassifierAnnotator with the given stores.
func NewClassifierAnnotator(
	ms ingest.MetricsStore,
	as ingest.AnnotationStore,
) *ClassifierAnnotator {
	return &ClassifierAnnotator{
		metricsStore:    ms,
		annotationStore: as,
		engine:          NewClassifierEngine(),
		idsByType:       make(map[string]classifierAnnotationIDs, len(typeAnnotatorNames)),
	}
}

// Annotate runs all classifiers for sessionID and persists non-nil results.
// Entry/metrics load errors are returned; per-result errors are logged and skipped.
func (ca *ClassifierAnnotator) Annotate(ctx context.Context, sessionID ingest.SessionID) error {
	return ca.annotate(ctx, sessionID, nil)
}

// AnnotateWithProfile runs all classifiers for sessionID and records aggregate
// ANNOTATE detail into profiler. A nil profiler preserves Annotate behavior.
func (ca *ClassifierAnnotator) AnnotateWithProfile(ctx context.Context, sessionID ingest.SessionID, profiler *ingest.IndexProfiler) error {
	if profiler == nil {
		return ca.Annotate(ctx, sessionID)
	}
	var stats ingest.AnnotationProfileStats
	defer func() {
		profiler.RecordAnnotation(stats)
	}()
	return ca.annotate(ctx, sessionID, &stats)
}

func (ca *ClassifierAnnotator) annotate(ctx context.Context, sessionID ingest.SessionID, stats *ingest.AnnotationProfileStats) error {
	stateInputs, hasCombinedInputs := ca.combinedAnnotationRunInputs(ctx, sessionID)
	if hasCombinedInputs && annotationRunInputsCurrent(stateInputs) {
		addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) { s.StateSkipCount++ })
		return nil
	}

	metricsStarted := annotationProfileStart(stats)
	metrics, err := ca.metricsStore.GetMetrics(ctx, sessionID)
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
		s.GetMetricsCount++
		s.GetMetricsTime += time.Since(metricsStarted)
	})
	if err != nil {
		return fmt.Errorf("ClassifierAnnotator.Annotate: get metrics for %s: %w", sessionID, err)
	}

	entriesHash, computeVersion, stateUsable := ca.annotationRunInputs(ctx, sessionID, metrics, stateInputs, hasCombinedInputs)
	if !hasCombinedInputs && stateUsable && ca.annotationStateCurrent(ctx, sessionID, entriesHash, computeVersion) {
		addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) { s.StateSkipCount++ })
		return nil
	}

	listStarted := annotationProfileStart(stats)
	entries, err := ca.metricsStore.ListEntries(ctx, sessionID)
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
		s.ListEntriesCount++
		s.ListEntriesTime += time.Since(listStarted)
	})
	if err != nil {
		return fmt.Errorf("ClassifierAnnotator.Annotate: list entries for %s: %w", sessionID, err)
	}

	runStarted := annotationProfileStart(stats)
	results := ca.engine.Run(ctx, sessionID, entries, metrics)
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
		s.ClassifierRunCount++
		s.ClassifierRunTime += time.Since(runStarted)
		s.ResultCount += len(results)
		for _, result := range results {
			if result.Target == nil {
				s.SessionResultCount++
			} else {
				s.EntryResultCount++
			}
		}
	})

	persistFailed := ca.persistResults(ctx, sessionID, results, stats)
	if stateUsable && !persistFailed {
		ca.saveAnnotationRunState(ctx, sessionID, entriesHash, computeVersion)
	}
	return nil
}

func (ca *ClassifierAnnotator) persistResults(ctx context.Context, sessionID ingest.SessionID, results []*ClassifierResult, stats *ingest.AnnotationProfileStats) bool {
	if batchStore, ok := ca.annotationStore.(ingest.ClassifierAnnotationBatchStore); ok {
		return ca.persistResultsBatch(ctx, sessionID, results, stats, batchStore)
	}

	persistFailed := false
	for _, r := range results {
		if err := ca.persistResult(ctx, sessionID, r, stats); err != nil {
			persistFailed = true
			slog.Warn("ClassifierAnnotator.Annotate: persist result",
				"session_id", sessionID,
				"type_id", r.TypeID,
				"error", err,
			)
		}
	}
	return persistFailed
}

func (ca *ClassifierAnnotator) persistResultsBatch(ctx context.Context, sessionID ingest.SessionID, results []*ClassifierResult, stats *ingest.AnnotationProfileStats, batchStore ingest.ClassifierAnnotationBatchStore) bool {
	writes := make([]ingest.ClassifierAnnotationWrite, 0, len(results))
	writeProfiles := make([]classifierAnnotationProfileKey, 0, len(results))
	persistFailed := false
	for _, r := range results {
		write, err := ca.classifierAnnotationWrite(ctx, sessionID, r, stats)
		if err != nil {
			persistFailed = true
			slog.Warn("ClassifierAnnotator.Annotate: prepare classifier annotation write",
				"session_id", sessionID,
				"type_id", r.TypeID,
				"error", err,
			)
			continue
		}
		writes = append(writes, write)
		writeProfiles = append(writeProfiles, classifierAnnotationProfileKey{
			typeID:     r.TypeID,
			value:      r.Value,
			targetKind: classifierResultTargetKind(r),
		})
	}
	if len(writes) == 0 {
		return persistFailed
	}
	writeStarted := annotationProfileStart(stats)
	var batchResults []ingest.ClassifierAnnotationWriteResult
	if profiled, ok := batchStore.(ingest.ProfiledClassifierAnnotationBatchStore); ok {
		batchResults = profiled.ApplyClassifierAnnotationsWithProfile(ctx, writes, stats)
	} else {
		batchResults = batchStore.ApplyClassifierAnnotations(ctx, writes)
	}
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchWriteCount++
		s.BatchWriteTime += time.Since(writeStarted)
		s.BatchResultCount += len(batchResults)
	})
	for i, result := range batchResults {
		if i < len(writeProfiles) {
			profile := writeProfiles[i]
			addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
				s.RecordAnnotationResult(profile.typeID, profile.value, profile.targetKind, result.Dedup, result.Err != nil)
			})
		}
		addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
			addAnnotationDedupDecision(s, result.Dedup)
			if result.Err != nil {
				s.BatchErrorCount++
			}
		})
		if result.Err != nil {
			persistFailed = true
			typeID := ""
			if i < len(writeProfiles) {
				typeID = writeProfiles[i].typeID
			}
			slog.Warn("ClassifierAnnotator.Annotate: persist batch result",
				"session_id", sessionID,
				"type_id", typeID,
				"error", result.Err,
			)
		}
	}
	return persistFailed
}

type classifierAnnotationProfileKey struct {
	typeID     string
	value      string
	targetKind ingest.AnnotationProfileTargetKind
}

func classifierResultTargetKind(r *ClassifierResult) ingest.AnnotationProfileTargetKind {
	if r == nil {
		return ingest.AnnotationProfileTargetUnknown
	}
	if r.Target != nil {
		return ingest.AnnotationProfileTargetEntry
	}
	return ingest.AnnotationProfileTargetSession
}

func addAnnotationTiming(stats *ingest.AnnotationProfileStats, add func(*ingest.AnnotationProfileStats)) {
	if stats != nil {
		add(stats)
	}
}

func annotationProfileStart(stats *ingest.AnnotationProfileStats) time.Time {
	if stats == nil {
		return time.Time{}
	}
	return time.Now()
}

func (ca *ClassifierAnnotator) combinedAnnotationRunInputs(ctx context.Context, sessionID ingest.SessionID) (*ingest.AnnotationRunInputs, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	inputStore, ok := ca.metricsStore.(ingest.AnnotationRunInputStore)
	if !ok {
		return nil, false
	}
	inputs, err := inputStore.GetAnnotationRunInputs(ctx, sessionID)
	if err != nil {
		slog.Warn("ClassifierAnnotator.Annotate: combined annotation state lookup failed, continuing with annotation",
			"session_id", sessionID,
			"error", err,
		)
		return nil, false
	}
	return inputs, inputs != nil
}

func annotationRunInputsCurrent(inputs *ingest.AnnotationRunInputs) bool {
	if inputs == nil || !inputs.HasSessionEntriesHash || inputs.SessionEntriesHash == "" || !inputs.HasComputeVersion || inputs.State == nil {
		return false
	}
	return inputs.State.SessionEntriesHash == inputs.SessionEntriesHash &&
		inputs.State.ComputeVersion == inputs.ComputeVersion &&
		inputs.State.ClassifierVersion == CurrentClassifierAnnotationVersion
}

func (ca *ClassifierAnnotator) annotationRunInputs(ctx context.Context, sessionID ingest.SessionID, metrics *ingest.SessionMetrics, combined *ingest.AnnotationRunInputs, hasCombined bool) (string, int, bool) {
	if hasCombined {
		if combined == nil || !combined.HasSessionEntriesHash || combined.SessionEntriesHash == "" || metrics == nil || metrics.ComputeVersion == nil {
			return "", 0, false
		}
		return combined.SessionEntriesHash, *metrics.ComputeVersion, true
	}
	if metrics == nil || metrics.ComputeVersion == nil {
		return "", 0, false
	}
	stateStore, ok := ca.metricsStore.(ingest.AnnotationRunStateStore)
	if !ok {
		return "", 0, false
	}
	hash, hashOK, err := stateStore.GetCurrentSessionEntriesHash(ctx, sessionID)
	if err != nil {
		slog.Warn("ClassifierAnnotator.Annotate: annotation state hash lookup failed, continuing with annotation",
			"session_id", sessionID,
			"error", err,
		)
		return "", 0, false
	}
	if !hashOK || hash == "" {
		return "", 0, false
	}
	return hash, *metrics.ComputeVersion, true
}

func (ca *ClassifierAnnotator) annotationStateCurrent(ctx context.Context, sessionID ingest.SessionID, entriesHash string, computeVersion int) bool {
	if ctx.Err() != nil {
		return false
	}
	stateStore, ok := ca.metricsStore.(ingest.AnnotationRunStateStore)
	if !ok {
		return false
	}
	state, err := stateStore.GetAnnotationRunState(ctx, sessionID)
	if err != nil {
		slog.Warn("ClassifierAnnotator.Annotate: annotation state lookup failed, continuing with annotation",
			"session_id", sessionID,
			"error", err,
		)
		return false
	}
	return state != nil &&
		state.SessionEntriesHash == entriesHash &&
		state.ComputeVersion == computeVersion &&
		state.ClassifierVersion == CurrentClassifierAnnotationVersion
}

func (ca *ClassifierAnnotator) saveAnnotationRunState(ctx context.Context, sessionID ingest.SessionID, entriesHash string, computeVersion int) {
	stateStore, ok := ca.metricsStore.(ingest.AnnotationRunStateStore)
	if !ok {
		return
	}
	state := ingest.AnnotationRunState{
		SessionID:          sessionID,
		SessionEntriesHash: entriesHash,
		ComputeVersion:     computeVersion,
		ClassifierVersion:  CurrentClassifierAnnotationVersion,
		AnnotatedAt:        time.Now(),
	}
	if err := stateStore.SaveAnnotationRunState(ctx, state); err != nil {
		slog.Warn("ClassifierAnnotator.Annotate: save annotation state failed",
			"session_id", sessionID,
			"error", err,
		)
	}
}

func (ca *ClassifierAnnotator) classifierAnnotationWrite(
	ctx context.Context,
	sessionID ingest.SessionID,
	r *ClassifierResult,
	stats *ingest.AnnotationProfileStats,
) (ingest.ClassifierAnnotationWrite, error) {
	ids, err := ca.resolveClassifierIDs(ctx, r.TypeID, stats)
	if err != nil {
		return ingest.ClassifierAnnotationWrite{}, err
	}

	var confidence *float64
	if r.Confidence > 0 {
		confidence = &r.Confidence
	}
	var reason *string
	if r.Reason != "" {
		reason = &r.Reason
	}

	sid := string(sessionID)
	findParams := ingest.FindAnnotationParams{
		AnnotationTypeID: ids.annotationTypeID,
		AnnotatorID:      ids.annotatorID,
		SessionID:        &sid,
	}
	createParams := ingest.CreateAnnotationParams{
		AnnotatorID:      ids.annotatorID,
		AnnotationTypeID: ids.annotationTypeID,
		Value:            r.Value,
		Confidence:       confidence,
		Reason:           reason,
		Provenance:       r.Provenance,
	}

	var entryIndex *int
	var endIndex *int
	if r.Target != nil {
		findParams.EntryIndex = &r.Target.EntryIndex
		entryIndex = &r.Target.EntryIndex
		if r.Target.EndIndex != 0 {
			endIndex = &r.Target.EndIndex
		}
		createParams.EntryTarget = &ingest.EntryTarget{
			SessionID:  sid,
			EntryIndex: r.Target.EntryIndex,
			EndIndex:   r.Target.EndIndex,
		}
	} else {
		createParams.SessionID = &sid
	}

	contentHash := schema.ComputeAnnotationHash(
		ids.annotationTypeID, ids.annotatorID, r.Value,
		&sid, entryIndex, endIndex,
		confidence, reason, r.Provenance,
	)

	return ingest.ClassifierAnnotationWrite{
		Create:      createParams,
		Find:        findParams,
		ContentHash: contentHash,
	}, nil
}

// persistResult resolves annotator/type IDs and writes one ClassifierResult to the store.
func (ca *ClassifierAnnotator) persistResult(
	ctx context.Context,
	sessionID ingest.SessionID,
	r *ClassifierResult,
	stats *ingest.AnnotationProfileStats,
) error {
	write, err := ca.classifierAnnotationWrite(ctx, sessionID, r, stats)
	if err != nil {
		return err
	}

	findStarted := annotationProfileStart(stats)
	existing, err := ca.annotationStore.FindExistingAnnotation(ctx, write.Find)
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
		s.DedupLookupCount++
		s.DedupLookupTime += time.Since(findStarted)
	})
	if err != nil {
		slog.Warn("ClassifierAnnotator.persistResult: dedup check failed, proceeding with create",
			"type_id", r.TypeID, "error", err)
	}

	dedupResult := ca.decideDedupAction(existing, write.ContentHash)
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
		addAnnotationDedupDecision(s, dedupResult)
		s.RecordAnnotationResult(r.TypeID, r.Value, classifierResultTargetKind(r), dedupResult, false)
	})

	switch dedupResult {
	case ingest.DedupSkip:
		slog.Debug("ClassifierAnnotator.persistResult: skip (same content hash)",
			"type_id", r.TypeID, "session_id", sessionID, "hash", write.ContentHash)
		return nil
	case ingest.DedupSupersede:
		newID, createErr := ca.createAnnotationFromParams(ctx, r.TypeID, write.Create, stats)
		if createErr != nil {
			return createErr
		}
		hashStarted := annotationProfileStart(stats)
		if hashErr := ca.annotationStore.UpdateContentHash(ctx, newID, write.ContentHash); hashErr != nil {
			addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
				s.UpdateContentHashCount++
				s.UpdateContentHashTime += time.Since(hashStarted)
			})
			slog.Warn("ClassifierAnnotator.persistResult: update content hash", "annotation_id", newID, "error", hashErr)
			return hashErr
		}
		addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
			s.UpdateContentHashCount++
			s.UpdateContentHashTime += time.Since(hashStarted)
		})
		supersedeStarted := annotationProfileStart(stats)
		if supErr := ca.annotationStore.SupersedeAnnotation(ctx, existing.ID, newID); supErr != nil {
			addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
				s.SupersedeCount++
				s.SupersedeTime += time.Since(supersedeStarted)
			})
			slog.Warn("ClassifierAnnotator.persistResult: supersede old annotation", "old_id", existing.ID, "new_id", newID, "error", supErr)
			return supErr
		}
		addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
			s.SupersedeCount++
			s.SupersedeTime += time.Since(supersedeStarted)
		})
		return nil
	default:
		newID, createErr := ca.createAnnotationFromParams(ctx, r.TypeID, write.Create, stats)
		if createErr != nil {
			return createErr
		}
		hashStarted := annotationProfileStart(stats)
		if hashErr := ca.annotationStore.UpdateContentHash(ctx, newID, write.ContentHash); hashErr != nil {
			addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
				s.UpdateContentHashCount++
				s.UpdateContentHashTime += time.Since(hashStarted)
			})
			slog.Warn("ClassifierAnnotator.persistResult: update content hash", "annotation_id", newID, "error", hashErr)
			return hashErr
		}
		addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
			s.UpdateContentHashCount++
			s.UpdateContentHashTime += time.Since(hashStarted)
		})
		return nil
	}
}

func addAnnotationDedupDecision(s *ingest.AnnotationProfileStats, dedup ingest.AnnotationDedupResult) {
	switch dedup {
	case ingest.DedupSkip:
		s.DedupSkipCount++
	case ingest.DedupSupersede:
		s.DedupSupersedeCount++
	default:
		s.DedupCreateCount++
	}
}

func (ca *ClassifierAnnotator) createAnnotationFromParams(ctx context.Context, typeID string, p ingest.CreateAnnotationParams, stats *ingest.AnnotationProfileStats) (string, error) {
	if p.EntryTarget != nil {
		ep := ingest.EntryAnnotationParams{
			SessionID:        p.EntryTarget.SessionID,
			EntryIndex:       p.EntryTarget.EntryIndex,
			EndIndex:         p.EntryTarget.EndIndex,
			AnnotatorID:      p.AnnotatorID,
			AnnotationTypeID: p.AnnotationTypeID,
			Value:            p.Value,
			Confidence:       p.Confidence,
			Reason:           p.Reason,
			Provenance:       p.Provenance,
		}
		createStarted := annotationProfileStart(stats)
		newID, err := ca.annotationStore.CreateEntryAnnotation(ctx, ep)
		addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
			s.CreateEntryCount++
			s.CreateEntryTime += time.Since(createStarted)
		})
		if err != nil {
			return "", fmt.Errorf("create entry annotation type=%q value=%q entry=%d: %w", typeID, p.Value, p.EntryTarget.EntryIndex, err)
		}
		return newID, nil
	}
	if p.SessionID == nil {
		return "", fmt.Errorf("create classifier annotation type=%q value=%q: missing session or entry target", typeID, p.Value)
	}
	sp := ingest.SessionAnnotationParams{
		SessionID:        *p.SessionID,
		AnnotatorID:      p.AnnotatorID,
		AnnotationTypeID: p.AnnotationTypeID,
		Value:            p.Value,
		Confidence:       p.Confidence,
		Reason:           p.Reason,
		Provenance:       p.Provenance,
	}
	createStarted := annotationProfileStart(stats)
	newID, err := ca.annotationStore.CreateSessionAnnotation(ctx, sp)
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) {
		s.CreateSessionCount++
		s.CreateSessionTime += time.Since(createStarted)
	})
	if err != nil {
		return "", fmt.Errorf("create annotation type=%q value=%q: %w", typeID, p.Value, err)
	}
	return newID, nil
}

func (ca *ClassifierAnnotator) resolveClassifierIDs(ctx context.Context, typeID string, stats *ingest.AnnotationProfileStats) (classifierAnnotationIDs, error) {
	ca.idsMu.Lock()
	defer ca.idsMu.Unlock()

	if ids, ok := ca.idsByType[typeID]; ok {
		addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) { s.IDCacheHits++ })
		return ids, nil
	}
	addAnnotationTiming(stats, func(s *ingest.AnnotationProfileStats) { s.IDCacheMisses++ })
	if ca.idsByType == nil {
		ca.idsByType = make(map[string]classifierAnnotationIDs, len(typeAnnotatorNames))
	}

	annotatorName, ok := typeAnnotatorNames[typeID]
	if !ok {
		return classifierAnnotationIDs{}, fmt.Errorf("no seeded annotator for type %q", typeID)
	}

	annotatorID, err := ca.annotationStore.GetAnnotatorIDByName(ctx, annotatorName)
	if err != nil {
		return classifierAnnotationIDs{}, fmt.Errorf("get annotator %q: %w", annotatorName, err)
	}
	if annotatorID == "" {
		return classifierAnnotationIDs{}, fmt.Errorf("annotator %q not found in DB (V13 migration required)", annotatorName)
	}

	annotationTypeID, err := ca.annotationStore.GetAnnotationTypeID(ctx, typeID)
	if err != nil {
		return classifierAnnotationIDs{}, fmt.Errorf("get annotation type %q: %w", typeID, err)
	}
	if annotationTypeID == "" {
		return classifierAnnotationIDs{}, fmt.Errorf("annotation type %q not found in DB (V13 migration required)", typeID)
	}

	ids := classifierAnnotationIDs{annotatorID: annotatorID, annotationTypeID: annotationTypeID}
	ca.idsByType[typeID] = ids
	return ids, nil
}

// decideDedupAction determines whether to skip, supersede, or create a new annotation
// based on the existing annotation and the new content hash.
func (ca *ClassifierAnnotator) decideDedupAction(existing *ingest.ExistingAnnotation, newHash string) ingest.AnnotationDedupResult {
	if existing == nil {
		return ingest.DedupCreate
	}
	if existing.ContentHash == newHash {
		return ingest.DedupSkip
	}
	return ingest.DedupSupersede
}
