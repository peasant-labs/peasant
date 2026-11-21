package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/annotate_session_outcomes.yaml
var annotateSessionOutcomesYAML []byte

// annotateOutcomeBehavior is what the classifier double does for one session.
type annotateOutcomeBehavior string

const (
	behaviorAnnotatedRows                   annotateOutcomeBehavior = "annotated-rows"
	behaviorAnnotatedNoRows                 annotateOutcomeBehavior = "annotated-no-rows"
	behaviorSkipped                         annotateOutcomeBehavior = "skipped"
	behaviorPrepareError                    annotateOutcomeBehavior = "prepare-error"
	behaviorPrepareWriteFailure             annotateOutcomeBehavior = "prepare-write-failure"
	behaviorPrepareWriteFailurePersistError annotateOutcomeBehavior = "prepare-write-failure-persist-error"
	behaviorPersistWriteError               annotateOutcomeBehavior = "persist-write-error"
	behaviorPersistBatchError               annotateOutcomeBehavior = "persist-batch-error"
	behaviorCancelInPrepare                 annotateOutcomeBehavior = "cancel-in-prepare"
	behaviorCancelInFlush                   annotateOutcomeBehavior = "cancel-in-flush"
)

var annotateOutcomeBehaviors = map[annotateOutcomeBehavior]bool{
	behaviorAnnotatedRows: true, behaviorAnnotatedNoRows: true, behaviorSkipped: true,
	behaviorPrepareError: true, behaviorPrepareWriteFailure: true, behaviorPrepareWriteFailurePersistError: true,
	behaviorPersistWriteError: true, behaviorPersistBatchError: true,
	behaviorCancelInPrepare: true, behaviorCancelInFlush: true,
}

func (b *annotateOutcomeBehavior) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	behavior := annotateOutcomeBehavior(raw)
	if !annotateOutcomeBehaviors[behavior] {
		return fmt.Errorf("line %d: unknown session behavior %q", node.Line, raw)
	}
	*b = behavior
	return nil
}

// annotateOutcomeClassifierKind selects which classifier interface the double
// implements.
type annotateOutcomeClassifierKind string

const (
	annotateClassifierBuffered   annotateOutcomeClassifierKind = "buffered"
	annotateClassifierUnbuffered annotateOutcomeClassifierKind = "unbuffered"
)

func (k *annotateOutcomeClassifierKind) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	switch kind := annotateOutcomeClassifierKind(raw); kind {
	case annotateClassifierBuffered, annotateClassifierUnbuffered:
		*k = kind
		return nil
	}
	return fmt.Errorf("line %d: unknown classifier kind %q", node.Line, raw)
}

type annotateOutcomeCase struct {
	Name       string                        `yaml:"name"`
	Classifier annotateOutcomeClassifierKind `yaml:"classifier"`
	Sessions   []annotateOutcomeBehavior     `yaml:"sessions"`
	Want       struct {
		Candidates        int `yaml:"candidates"`
		Annotated         int `yaml:"annotated"`
		Skipped           int `yaml:"skipped"`
		PreparationFailed int `yaml:"preparationFailed"`
		PersistenceFailed int `yaml:"persistenceFailed"`
		Unfinished        int `yaml:"unfinished"`
	} `yaml:"want"`
}

func loadAnnotateOutcomeCases(t *testing.T) []annotateOutcomeCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string              `yaml:"requiredNames"`
		Cases         []annotateOutcomeCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(annotateSessionOutcomesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode annotate_session_outcomes.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("annotate_session_outcomes.yaml must contain exactly one document: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if present[c.Name] {
			t.Fatalf("annotate_session_outcomes.yaml repeats case %q", c.Name)
		}
		present[c.Name] = true
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("annotate_session_outcomes.yaml declares no required case names")
	}
	for _, name := range fixture.RequiredNames {
		if !present[name] {
			t.Fatalf("annotate_session_outcomes.yaml is missing required case %q", name)
		}
	}
	return fixture.Cases
}

var errAnnotateOutcomeDouble = errors.New("annotate outcome double failure")

// outcomeClassifier plays each session's fixture behavior. After the run is
// cancelled every call fails with the context error, as a store call would.
type outcomeClassifier struct {
	behaviors map[SessionID]annotateOutcomeBehavior
	cancel    context.CancelFunc
}

func (c *outcomeClassifier) Annotate(ctx context.Context, sid SessionID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch c.behaviors[sid] {
	case behaviorCancelInPrepare, behaviorCancelInFlush:
		c.cancel()
		return ctx.Err()
	case behaviorAnnotatedRows, behaviorAnnotatedNoRows, behaviorSkipped:
		return nil
	default:
		return errAnnotateOutcomeDouble
	}
}

func outcomeWrites(n int) []SessionAnnotationWrite {
	writes := make([]SessionAnnotationWrite, n)
	for i := range writes {
		writes[i] = SessionAnnotationWrite{TypeID: "test.annotation", Value: "prepared", TargetKind: AnnotationProfileTargetEntry}
	}
	return writes
}

// bufferedOutcomeClassifier adds the buffered prepare/flush split.
type bufferedOutcomeClassifier struct {
	outcomeClassifier
}

var (
	_ SessionClassifier         = (*outcomeClassifier)(nil)
	_ BufferedSessionClassifier = (*bufferedOutcomeClassifier)(nil)
)

func (c *bufferedOutcomeClassifier) PrepareAnnotations(ctx context.Context, sid SessionID, _ *IndexProfiler) (SessionAnnotationBatch, error) {
	batch := SessionAnnotationBatch{SessionID: sid}
	if err := ctx.Err(); err != nil {
		return batch, err
	}
	state := &AnnotationRunState{SessionID: sid}
	switch c.behaviors[sid] {
	case behaviorAnnotatedNoRows:
		return batch, nil
	case behaviorSkipped:
		batch.Skipped = true
		return batch, nil
	case behaviorPrepareError:
		return batch, errAnnotateOutcomeDouble
	case behaviorCancelInPrepare:
		c.cancel()
		return batch, ctx.Err()
	case behaviorPrepareWriteFailure, behaviorPrepareWriteFailurePersistError:
		batch.Writes = outcomeWrites(2)
		batch.PreparationFailed = true
		return batch, nil
	default:
		batch.Writes = outcomeWrites(3)
		batch.RunState = state
		return batch, nil
	}
}

func (c *bufferedOutcomeClassifier) FlushAnnotationBatches(ctx context.Context, batches []SessionAnnotationBatch, _ *IndexProfiler) []SessionAnnotationBatchResult {
	for _, batch := range batches {
		if c.behaviors[batch.SessionID] == behaviorCancelInFlush {
			c.cancel()
		}
	}
	results := make([]SessionAnnotationBatchResult, len(batches))
	for i, batch := range batches {
		results[i].SessionID = batch.SessionID
		results[i].Results = make([]ClassifierAnnotationWriteResult, len(batch.Writes))
		for j := range results[i].Results {
			results[i].Results[j].Dedup = DedupCreate
		}
		if err := ctx.Err(); err != nil {
			results[i].Err = err
			continue
		}
		switch c.behaviors[batch.SessionID] {
		case behaviorPersistWriteError, behaviorPrepareWriteFailurePersistError:
			results[i].Results[len(results[i].Results)-1].Err = errAnnotateOutcomeDouble
		case behaviorPersistBatchError:
			results[i].Err = errAnnotateOutcomeDouble
		}
	}
	return results
}

// annotateOutcomeEntry runs the ANNOTATE stage through one production entry.
type annotateOutcomeEntry struct {
	name string
	run  func(ctx context.Context, p *Pipeline, ids []SessionID)
}

var annotateOutcomeEntries = []annotateOutcomeEntry{
	{name: "streamed", run: func(ctx context.Context, p *Pipeline, ids []SessionID) {
		ch := make(chan indexedMeta, len(ids))
		for _, sid := range ids {
			ch <- indexedMeta{session: DiscoveredSession{SessionID: sid}, indexed: true}
		}
		close(ch)
		p.runStreamedDownstream(ctx, ch, nil, len(ids), "test", nil)
	}},
	{name: "stage", run: func(ctx context.Context, p *Pipeline, ids []SessionID) {
		_ = p.stageAnnotate(ctx, ids, nil)
	}},
}

func TestAnnotateSessionOutcomesCountEachCandidateOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range loadAnnotateOutcomeCases(t) {
		for _, entry := range annotateOutcomeEntries {
			t.Run(tc.Name+"/"+entry.name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				base := outcomeClassifier{behaviors: make(map[SessionID]annotateOutcomeBehavior, len(tc.Sessions)), cancel: cancel}
				ids := make([]SessionID, len(tc.Sessions))
				for i, behavior := range tc.Sessions {
					ids[i] = SessionID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))
					base.behaviors[ids[i]] = behavior
				}
				var classifier SessionClassifier = &base
				if tc.Classifier == annotateClassifierBuffered {
					classifier = &bufferedOutcomeClassifier{outcomeClassifier: base}
				}
				profiler := &IndexProfiler{}
				p := &Pipeline{config: PipelineConfig{Parallelism: 1, IndexProfiler: profiler}, classifier: classifier}
				entry.run(ctx, p, ids)

				got := profiler.Snapshot().AnnotationSessions
				want := AnnotationSessionOutcomes{
					Candidates:        tc.Want.Candidates,
					Annotated:         tc.Want.Annotated,
					Skipped:           tc.Want.Skipped,
					PreparationFailed: tc.Want.PreparationFailed,
					PersistenceFailed: tc.Want.PersistenceFailed,
				}
				if got != want || got.Unfinished() != tc.Want.Unfinished {
					t.Fatalf("annotation sessions = %+v unfinished=%d, want %+v", got, got.Unfinished(), tc.Want)
				}
				if got.Finished()+got.Unfinished() != got.Candidates {
					t.Fatalf("finished %d + unfinished %d != candidates %d", got.Finished(), got.Unfinished(), got.Candidates)
				}
			})
		}
	}
}
