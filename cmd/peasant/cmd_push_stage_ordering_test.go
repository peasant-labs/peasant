package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/push"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/push_stage_ordering.yaml
var pushStageOrderingYAML []byte

type pushStageAction string

const (
	pushStageActionSuccess             pushStageAction = "success"
	pushStageActionAssociationSuccess  pushStageAction = "association_success"
	pushStageActionNoAssociation       pushStageAction = "no_association"
	pushStageActionFailure             pushStageAction = "failure"
	pushStageActionCancel              pushStageAction = "cancel"
	pushStageActionObserveCancellation pushStageAction = "observe_cancellation"
)

type pushStageEvent string

const (
	pushStageEventTranscriptStart pushStageEvent = "transcript-start"
	pushStageEventTranscriptEnd   pushStageEvent = "transcript-end"
	pushStageEventAnnotationStart pushStageEvent = "annotation-start"
	pushStageEventAnnotationEnd   pushStageEvent = "annotation-end"
)

type pushStageErrorKind string

const (
	pushStageErrorNone     pushStageErrorKind = "none"
	pushStageErrorFailure  pushStageErrorKind = "failure"
	pushStageErrorCanceled pushStageErrorKind = "canceled"
)

type pushStageErrorSource string

const (
	pushStageErrorSourceNone       pushStageErrorSource = "none"
	pushStageErrorSourceTranscript pushStageErrorSource = "transcript"
	pushStageErrorSourceAnnotation pushStageErrorSource = "annotation"
)

type pushStageOrderingFixtureFile struct {
	Cases []pushStageOrderingFixture `yaml:"cases"`
}

type pushStageOrderingFixture struct {
	Name       string               `yaml:"name"`
	Transcript pushStageAction      `yaml:"transcript"`
	Annotation pushStageAction      `yaml:"annotation"`
	Expected   pushStageExpectation `yaml:"expected"`
}

type pushStageExpectation struct {
	EventOrder        []pushStageEvent     `yaml:"event_order"`
	AnnotationRan     bool                 `yaml:"annotation_ran"`
	TranscriptResult  bool                 `yaml:"transcript_result"`
	AnnotationSummary bool                 `yaml:"annotation_summary"`
	AnnotationTotal   *int                 `yaml:"annotation_total"`
	TranscriptError   pushStageErrorKind   `yaml:"transcript_error"`
	AnnotationError   pushStageErrorKind   `yaml:"annotation_error"`
	ReturnedError     pushStageErrorSource `yaml:"returned_error"`
}

type pushStageRunOutcome struct {
	result            *push.PushResult
	transcriptErr     error
	annotationSummary *push.AnnotationPushSummary
	annotationErr     error
}

var (
	errPushStageTranscript = errors.New("transcript stage failure")
	errPushStageAnnotation = errors.New("annotation stage failure")
)

const expectedPushStageOrderingCaseCount = 4

const pushStageOrderingFixturePath = "cmd/peasant/testdata/push_stage_ordering.yaml"

func loadPushStageOrderingFixtures(data []byte) ([]pushStageOrderingFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture pushStageOrderingFixtureFile
	if err := decoder.Decode(&fixture); err != nil {
		return nil, fmt.Errorf("decode push stage ordering fixture in %s: %w", pushStageOrderingFixturePath, err)
	}
	var extra struct{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("push stage ordering fixture in %s must contain exactly one YAML document; remediation: keep a single YAML document with one cases list", pushStageOrderingFixturePath)
		}
		return nil, fmt.Errorf("decode extra push stage ordering fixture document in %s: %w", pushStageOrderingFixturePath, err)
	}

	if len(fixture.Cases) != expectedPushStageOrderingCaseCount {
		return nil, fmt.Errorf("push stage ordering fixture in %s has %d cases, want exactly %d; remediation: keep the required ordering scenarios in the cases list", pushStageOrderingFixturePath, len(fixture.Cases), expectedPushStageOrderingCaseCount)
	}
	requiredNames := map[string]struct{}{
		"association_success":                          {},
		"no_association":                               {},
		"transcript_failure_annotation_still_runs":     {},
		"cancellation_same_context_reaches_annotation": {},
	}
	seenNames := make(map[string]struct{}, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" {
			return nil, fmt.Errorf("push stage ordering fixture in %s contains a case without a name; remediation: add a unique name to every case", pushStageOrderingFixturePath)
		}
		if _, ok := requiredNames[fixtureCase.Name]; !ok {
			return nil, fmt.Errorf("push stage ordering fixture in %s contains unexpected case %q; remediation: use one of the required case names", pushStageOrderingFixturePath, fixtureCase.Name)
		}
		if _, duplicate := seenNames[fixtureCase.Name]; duplicate {
			return nil, fmt.Errorf("push stage ordering fixture in %s contains duplicate case %q; remediation: give each case a unique name", pushStageOrderingFixturePath, fixtureCase.Name)
		}
		seenNames[fixtureCase.Name] = struct{}{}
		if err := validatePushStageOrderingFixture(fixtureCase); err != nil {
			return nil, err
		}
	}
	for name := range requiredNames {
		if _, ok := seenNames[name]; !ok {
			return nil, fmt.Errorf("push stage ordering fixture in %s is missing required case %q; remediation: restore the required ordering scenario", pushStageOrderingFixturePath, name)
		}
	}
	return fixture.Cases, nil
}

func validatePushStageOrderingFixture(fixture pushStageOrderingFixture) error {
	if !validPushStageAction(fixture.Transcript) {
		return fmt.Errorf("fixture %q in fixture path %s has invalid transcript action %q; remediation: use a supported transcript action", fixture.Name, pushStageOrderingFixturePath, fixture.Transcript)
	}
	if !validPushStageAction(fixture.Annotation) {
		return fmt.Errorf("fixture %q in fixture path %s has invalid annotation action %q; remediation: use a supported annotation action", fixture.Name, pushStageOrderingFixturePath, fixture.Annotation)
	}
	if len(fixture.Expected.EventOrder) == 0 {
		return fmt.Errorf("fixture %q in fixture path %s has no expected event order; remediation: add the expected callback event sequence", fixture.Name, pushStageOrderingFixturePath)
	}
	for _, event := range fixture.Expected.EventOrder {
		if !validPushStageEvent(event) {
			return fmt.Errorf("fixture %q in fixture path %s has invalid event %q; remediation: use a supported stage event", fixture.Name, pushStageOrderingFixturePath, event)
		}
	}
	if !validPushStageErrorKind(fixture.Expected.TranscriptError) {
		return fmt.Errorf("fixture %q in fixture path %s has invalid transcript error kind %q; remediation: use a supported error kind", fixture.Name, pushStageOrderingFixturePath, fixture.Expected.TranscriptError)
	}
	if !validPushStageErrorKind(fixture.Expected.AnnotationError) {
		return fmt.Errorf("fixture %q in fixture path %s has invalid annotation error kind %q; remediation: use a supported error kind", fixture.Name, pushStageOrderingFixturePath, fixture.Expected.AnnotationError)
	}
	if !validPushStageErrorSource(fixture.Expected.ReturnedError) {
		return fmt.Errorf("fixture %q in fixture path %s has invalid returned error source %q; remediation: use a supported returned error source", fixture.Name, pushStageOrderingFixturePath, fixture.Expected.ReturnedError)
	}
	if err := validatePushStageAnnotationTotal(fixture.Name, fixture.Expected.AnnotationTotal); err != nil {
		return err
	}
	return nil
}

func validatePushStageAnnotationTotal(fixtureName string, annotationTotal *int) error {
	if annotationTotal == nil {
		return fmt.Errorf(
			"fixture %q is missing expected.annotation_total in fixture path %s; omission impact: the test cannot verify the returned annotation count; remediation: add a non-negative expected.annotation_total value",
			fixtureName,
			pushStageOrderingFixturePath,
		)
	}
	if *annotationTotal < 0 {
		return fmt.Errorf(
			"fixture %q has negative expected.annotation_total=%d in fixture path %s; validation impact: an invalid count makes the annotation-total assertion untrustworthy; remediation: replace it with the expected non-negative annotation count",
			fixtureName,
			*annotationTotal,
			pushStageOrderingFixturePath,
		)
	}
	return nil
}

func validPushStageAction(action pushStageAction) bool {
	switch action {
	case pushStageActionSuccess, pushStageActionAssociationSuccess, pushStageActionNoAssociation,
		pushStageActionFailure, pushStageActionCancel, pushStageActionObserveCancellation:
		return true
	default:
		return false
	}
}

func validPushStageEvent(event pushStageEvent) bool {
	switch event {
	case pushStageEventTranscriptStart, pushStageEventTranscriptEnd,
		pushStageEventAnnotationStart, pushStageEventAnnotationEnd:
		return true
	default:
		return false
	}
}

func validPushStageErrorKind(kind pushStageErrorKind) bool {
	switch kind {
	case pushStageErrorNone, pushStageErrorFailure, pushStageErrorCanceled:
		return true
	default:
		return false
	}
}

func validPushStageErrorSource(source pushStageErrorSource) bool {
	switch source {
	case pushStageErrorSourceNone, pushStageErrorSourceTranscript, pushStageErrorSourceAnnotation:
		return true
	default:
		return false
	}
}

func TestPushStageOrderingFixtures_RejectMissingAnnotationTotal(t *testing.T) {
	annotationTotalField := []byte("annotation_total: 1")
	if !bytes.Contains(pushStageOrderingYAML, annotationTotalField) {
		t.Fatal("push stage ordering fixture no longer contains the association_success annotation_total row to omit")
	}
	malformedYAML := bytes.Replace(pushStageOrderingYAML, annotationTotalField, nil, 1)

	_, err := loadPushStageOrderingFixtures(malformedYAML)
	if err == nil {
		t.Fatal("missing expected.annotation_total was accepted")
	}
	for _, want := range []string{
		"association_success",
		"expected.annotation_total",
		pushStageOrderingFixturePath,
		"remediation:",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing annotation_total error = %v, want %q", err, want)
		}
	}
}

func TestPushStageOrderingFixtures_RejectNegativeAnnotationTotal(t *testing.T) {
	annotationTotalField := []byte("annotation_total: 1")
	negativeAnnotationTotalField := []byte("annotation_total: -1")
	if !bytes.Contains(pushStageOrderingYAML, annotationTotalField) {
		t.Fatal("push stage ordering fixture no longer contains the association_success annotation_total row to make negative")
	}
	malformedYAML := bytes.Replace(pushStageOrderingYAML, annotationTotalField, negativeAnnotationTotalField, 1)

	_, err := loadPushStageOrderingFixtures(malformedYAML)
	if err == nil {
		t.Fatal("negative expected.annotation_total was accepted")
	}
	for _, want := range []string{
		"association_success",
		"expected.annotation_total",
		pushStageOrderingFixturePath,
		"remediation:",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("negative annotation_total error = %v, want %q", err, want)
		}
	}
}

// TestRunPushStages proves the production stage seam, rather than the concrete
// network clients: transcript completion is a barrier, both stage outcomes are
// retained, transcript errors keep precedence, and cancellation reaches the
// annotation stage through the same context.
func TestRunPushStages(t *testing.T) {
	t.Parallel()
	fixtures, err := loadPushStageOrderingFixtures(pushStageOrderingYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()

			stageCtx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var (
				eventsMu          sync.Mutex
				events            []pushStageEvent
				annotationCalled  atomic.Bool
				contextMismatched atomic.Bool
				boundaryOnce      sync.Once
				annotationOnce    sync.Once
				releaseOnce       sync.Once
			)
			transcriptBoundary := make(chan struct{})
			transcriptReturned := make(chan struct{})
			annotationStarted := make(chan struct{})
			releaseTranscript := make(chan struct{})
			runDone := make(chan struct{})
			runResult := make(chan pushStageRunOutcome, 1)
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(releaseTranscript) })
				<-runDone
			})
			record := func(event pushStageEvent) {
				eventsMu.Lock()
				events = append(events, event)
				eventsMu.Unlock()
			}

			transcriptStage := func(ctx context.Context) (*push.PushResult, error) {
				if ctx != stageCtx {
					contextMismatched.Store(true)
				}
				record(pushStageEventTranscriptStart)
				var result *push.PushResult
				var err error
				switch fixture.Transcript {
				case pushStageActionSuccess:
					result = &push.PushResult{New: 1}
				case pushStageActionFailure:
					result = &push.PushResult{Errors: 1}
					err = errPushStageTranscript
				case pushStageActionCancel:
					cancel()
					result = &push.PushResult{Errors: 1}
					err = fmt.Errorf("transcript stage canceled: %w", context.Canceled)
				default:
					err = fmt.Errorf("unsupported transcript action %q", fixture.Transcript)
				}
				record(pushStageEventTranscriptEnd)
				boundaryOnce.Do(func() { close(transcriptBoundary) })
				<-releaseTranscript
				return result, err
			}
			transcriptStageWithReturnSignal := func(ctx context.Context) (*push.PushResult, error) {
				result, err := transcriptStage(ctx)
				close(transcriptReturned)
				return result, err
			}

			annotationStage := func(ctx context.Context) (*push.AnnotationPushSummary, error) {
				select {
				case <-transcriptReturned:
				default:
					t.Errorf("annotation started before transcript callback returned")
				}
				annotationOnce.Do(func() { close(annotationStarted) })
				annotationCalled.Store(true)
				if ctx != stageCtx {
					contextMismatched.Store(true)
				}
				record(pushStageEventAnnotationStart)

				var summary *push.AnnotationPushSummary
				var err error
				switch fixture.Annotation {
				case pushStageActionAssociationSuccess:
					summary = &push.AnnotationPushSummary{Total: 1, Created: 1}
				case pushStageActionNoAssociation:
					summary = &push.AnnotationPushSummary{}
				case pushStageActionFailure:
					summary = &push.AnnotationPushSummary{Total: 1, Errors: 1}
					err = errPushStageAnnotation
				case pushStageActionObserveCancellation:
					if ctx.Err() != context.Canceled {
						err = fmt.Errorf("annotation stage did not receive canceled context: %w", errPushStageAnnotation)
					} else {
						err = fmt.Errorf("annotation stage canceled: %w", context.Canceled)
					}
				default:
					err = fmt.Errorf("unsupported annotation action %q", fixture.Annotation)
				}
				record(pushStageEventAnnotationEnd)
				return summary, err
			}

			go func() {
				defer close(runDone)
				result, transcriptErr, annotationSummary, annotationErr := runPushStages(stageCtx, transcriptStageWithReturnSignal, annotationStage)
				runResult <- pushStageRunOutcome{
					result:            result,
					transcriptErr:     transcriptErr,
					annotationSummary: annotationSummary,
					annotationErr:     annotationErr,
				}
			}()

			<-transcriptBoundary
			select {
			case <-annotationStarted:
				t.Fatal("annotation stage began while transcript callback was blocked before returning")
			default:
			}
			releaseOnce.Do(func() { close(releaseTranscript) })
			outcome := <-runResult
			result, transcriptErr, annotationSummary, annotationErr := outcome.result, outcome.transcriptErr, outcome.annotationSummary, outcome.annotationErr

			if annotationCalled.Load() != fixture.Expected.AnnotationRan {
				t.Errorf("annotation stage called = %v, want %v", annotationCalled.Load(), fixture.Expected.AnnotationRan)
			}
			if (result != nil) != fixture.Expected.TranscriptResult {
				t.Errorf("transcript result present = %v, want %v", result != nil, fixture.Expected.TranscriptResult)
			}
			if (annotationSummary != nil) != fixture.Expected.AnnotationSummary {
				t.Errorf("annotation summary present = %v, want %v", annotationSummary != nil, fixture.Expected.AnnotationSummary)
			}
			if annotationSummary != nil && annotationSummary.Total != *fixture.Expected.AnnotationTotal {
				t.Errorf("annotation total = %d, want %d", annotationSummary.Total, *fixture.Expected.AnnotationTotal)
			}
			assertPushStageError(t, "transcript", transcriptErr, fixture.Expected.TranscriptError, errPushStageTranscript)
			assertPushStageError(t, "annotation", annotationErr, fixture.Expected.AnnotationError, errPushStageAnnotation)

			returnedErr := firstPushStageError(transcriptErr, annotationErr)
			switch fixture.Expected.ReturnedError {
			case pushStageErrorSourceNone:
				if returnedErr != nil {
					t.Errorf("returned error = %v, want nil", returnedErr)
				}
			case pushStageErrorSourceTranscript:
				if returnedErr != transcriptErr {
					t.Errorf("returned error = %v, want transcript error %v", returnedErr, transcriptErr)
				}
			case pushStageErrorSourceAnnotation:
				if returnedErr != annotationErr {
					t.Errorf("returned error = %v, want annotation error %v", returnedErr, annotationErr)
				}
			}

			if contextMismatched.Load() {
				t.Errorf("transcript and annotation stages did not receive the same context")
			}
			eventsMu.Lock()
			gotEvents := append([]pushStageEvent(nil), events...)
			eventsMu.Unlock()
			if !reflect.DeepEqual(gotEvents, fixture.Expected.EventOrder) {
				t.Errorf("stage event order = %v, want %v", gotEvents, fixture.Expected.EventOrder)
			}
		})
	}
}

func assertPushStageError(t *testing.T, stage string, got error, want pushStageErrorKind, sentinel error) {
	t.Helper()
	switch want {
	case pushStageErrorNone:
		if got != nil {
			t.Errorf("%s error = %v, want nil", stage, got)
		}
	case pushStageErrorFailure:
		if !errors.Is(got, sentinel) {
			t.Errorf("%s error = %v, want errors.Is(..., %v)", stage, got, sentinel)
		}
	case pushStageErrorCanceled:
		if !errors.Is(got, context.Canceled) {
			t.Errorf("%s error = %v, want cancellation", stage, got)
		}
	}
}
