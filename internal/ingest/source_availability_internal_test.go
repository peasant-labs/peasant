package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/source_unavailable.yaml
var sourceUnavailableFixtureData []byte

//go:embed testdata/source_unavailable_cause.yaml
var sourceUnavailableCauseFixtureData []byte

//go:embed testdata/source_settle_rereport.yaml
var sourceSettleRereportFixtureData []byte

//go:embed testdata/source_diagnostic_lanes.yaml
var sourceDiagnosticLanesFixtureData []byte

type sourceUnavailableFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name string `yaml:"name"`
		Kind string `yaml:"kind"`
		Want bool   `yaml:"want"`
	} `yaml:"cases"`
}

// decodeOneFixture reads a single-document fixture with unknown fields
// refused, so a renamed or removed key is a failure rather than a silently
// ignored line.
func decodeOneFixture(t *testing.T, data []byte, dest any, label string) {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(dest); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("%s must be one document: %v", label, err)
	}
}

// requireNamedCases fails when the manifest is empty, blank, duplicated, or
// names a case the fixture does not contain. An empty list would otherwise
// accept a fixture that deleted every row.
func requireNamedCases(t *testing.T, label string, required []string, present map[string]bool) {
	t.Helper()
	if len(required) == 0 {
		t.Fatalf("%s declares no required names; list every name the fixture must retain", label)
	}
	seen := make(map[string]bool, len(required))
	for _, name := range required {
		if strings.TrimSpace(name) == "" {
			t.Fatalf("%s required names has a blank entry", label)
		}
		if seen[name] {
			t.Fatalf("%s required names repeats %q", label, name)
		}
		seen[name] = true
		if !present[name] {
			t.Fatalf("%s is missing required name %q", label, name)
		}
	}
}

// TestSourceUnavailableClassifiesOnlyTheSourceBytes pins the boundary the
// settle depends on: the bytes on disk can settle, because they are the problem
// the user has to fix whether they are missing or unusable, while a store,
// locator, or write fault must keep reporting until it clears on its own.
func TestSourceUnavailableClassifiesOnlyTheSourceBytes(t *testing.T) {
	var fixtures sourceUnavailableFixtures
	decodeOneFixture(t, sourceUnavailableFixtureData, &fixtures, "source unavailable fixture")
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate source unavailable fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	// This white-box test cannot import internal/testutil (it would close an
	// import cycle through this package), so it checks the required names here.
	// That is why this small validator has a second copy here rather than a
	// shared leaf package: the check is the same one RequireFixtureNames makes,
	// and moving it would mean a new package both owners import.
	requireNamedCases(t, "source unavailable fixture", fixtures.Required, names)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var err error
			switch fixture.Kind {
			case "not_exist":
				err = fs.ErrNotExist
			case "wrapped_not_exist":
				err = fmt.Errorf("capture transcript source: %w", fs.ErrNotExist)
			case "malformed":
				err = malformedSourceRecord{}
			case "malformed_with_cause":
				err = fmt.Errorf("capture transcript source: %w", malformedSourceRecord{cause: errors.New("required message content missing")})
			case "locator":
				err = errors.New("synthetic locator lookup failure")
			case "store":
				err = fmt.Errorf("commit session entries: %w", errors.New("database is locked"))
			default:
				t.Fatalf("case %s names unknown kind %q", fixture.Name, fixture.Kind)
			}
			if got := sourceUnavailable(err); got != fixture.Want {
				t.Fatalf("sourceUnavailable(%v) = %v, want %v", err, got, fixture.Want)
			}
		})
	}
}

func TestSourceUnavailableCauseClassifiesTheClosedSet(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name      string `yaml:"name"`
			Kind      string `yaml:"kind"`
			WantCause string `yaml:"want_cause"`
			WantOK    bool   `yaml:"want_ok"`
		} `yaml:"cases"`
	}
	decodeOneFixture(t, sourceUnavailableCauseFixtureData, &fixtures, "source cause fixture")
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate source-cause fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	requireNamedCases(t, "source cause fixture", fixtures.Required, names)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var err error
			switch fixture.Kind {
			case "nil":
				err = nil
			case "not_exist":
				err = fs.ErrNotExist
			case "wrapped_not_exist":
				err = fmt.Errorf("capture transcript source: %w", fs.ErrNotExist)
			case "malformed":
				err = malformedSourceRecord{}
			case "malformed_with_cause":
				err = fmt.Errorf("capture transcript source: %w", malformedSourceRecord{cause: errors.New("required message content missing")})
			case "locator":
				err = errors.New("synthetic locator lookup failure")
			case "store":
				err = fmt.Errorf("commit session entries: %w", errors.New("database is locked"))
			default:
				t.Fatalf("case %s names unknown kind %q", fixture.Name, fixture.Kind)
			}
			got, ok := sourceUnavailableCause(err)
			if ok != fixture.WantOK || (ok && string(got) != fixture.WantCause) {
				t.Fatalf("sourceUnavailableCause(%v) = %q, %v; want %q, %v", err, got, ok, fixture.WantCause, fixture.WantOK)
			}
		})
	}
}

// recordingAvailabilityStore is the availability dependency the settle calls.
// It is not the code under test.
type recordingAvailabilityStore struct {
	MetricsStore

	mu      sync.Mutex
	reasons map[availabilityKey]SourceUnavailabilityReason
	records int
}

var _ SourceAvailabilityStore = (*recordingAvailabilityStore)(nil)

type availabilityKey struct {
	session SessionID
	lane    SourceUnavailabilityLane
}

func (s *recordingAvailabilityStore) ReadSourceUnavailabilityForLane(_ context.Context, sid SessionID, lane SourceUnavailabilityLane) (*SourceUnavailabilityReason, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reason, ok := s.reasons[availabilityKey{session: sid, lane: lane}]
	if !ok {
		return nil, nil
	}
	return &reason, nil
}

func (s *recordingAvailabilityStore) RecordSourceUnavailable(_ context.Context, sid SessionID, lane SourceUnavailabilityLane, reason SourceUnavailabilityReason) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records++
	if s.reasons == nil {
		s.reasons = make(map[availabilityKey]SourceUnavailabilityReason)
	}
	key := availabilityKey{session: sid, lane: lane}
	if previous, ok := s.reasons[key]; ok && previous != reason.SupersededReason() {
		return false, nil
	}
	s.reasons[key] = reason
	return true, nil
}

func (s *recordingAvailabilityStore) stored(sid SessionID, lane SourceUnavailabilityLane) (SourceUnavailabilityReason, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reason, ok := s.reasons[availabilityKey{session: sid, lane: lane}]
	return reason, ok
}

// TestSettleUnavailableSourceRereportsForceAndCauseChange holds the report
// owed by one session across harvests. A routine repeat of the stored cause
// is quiet. An explicit retry of that same cause reports again. A cause that
// replaces the stored one reports once, then goes quiet.
func TestSettleUnavailableSourceRereportsForceAndCauseChange(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Steps    []struct {
			Name       string `yaml:"name"`
			Force      bool   `yaml:"force"`
			Cause      string `yaml:"cause"`
			WantReport bool   `yaml:"want_report"`
			WantStored string `yaml:"want_stored"`
		} `yaml:"steps"`
	}
	decodeOneFixture(t, sourceSettleRereportFixtureData, &fixtures, "source settle re-report fixture")
	if len(fixtures.Steps) == 0 {
		t.Fatal("source settle re-report fixture has no steps")
	}
	names := make(map[string]bool)
	for _, step := range fixtures.Steps {
		if step.Name == "" || names[step.Name] {
			t.Fatalf("empty or duplicate source settle step %q", step.Name)
		}
		names[step.Name] = true
		if _, err := NewSourceUnavailabilityReason(step.Cause); err != nil {
			t.Fatalf("step %s names unknown cause %q", step.Name, step.Cause)
		}
		if _, err := NewSourceUnavailabilityReason(step.WantStored); err != nil {
			t.Fatalf("step %s names unknown stored cause %q", step.Name, step.WantStored)
		}
	}
	requireNamedCases(t, "source settle re-report fixture", fixtures.Required, names)

	store := &recordingAvailabilityStore{}
	sid := SessionID("11111111-1111-4111-8111-111111111111")
	ctx := context.Background()
	for _, step := range fixtures.Steps {
		cause, err := NewSourceUnavailabilityReason(step.Cause)
		if err != nil {
			t.Fatal(err)
		}
		// A new pipeline is a later harvest: it starts with no per-run claims
		// and uses the durable lane state to settle routine repeats.
		pipeline := &Pipeline{config: PipelineConfig{Force: step.Force}, metricsStore: store}
		got := pipeline.settleUnavailableSource(ctx, sid, SourceLaneIndex, cause, nil, StageIndex)
		if got != step.WantReport {
			t.Fatalf("step %s report = %v, want %v", step.Name, got, step.WantReport)
		}
		stored, ok := store.stored(sid, SourceLaneIndex)
		if !ok || string(stored) != step.WantStored {
			t.Fatalf("step %s stored %q, %v; want %q", step.Name, stored, ok, step.WantStored)
		}
	}
}

// TestSettleUnavailableSourceUsesCallerWriteLane holds the index-failure
// path's contract: the settle record runs on the lane the caller already
// holds. The fallback mutex is held for the whole call. A dropped lane would
// lock it and miss the deadline.
func TestSettleUnavailableSourceUsesCallerWriteLane(t *testing.T) {
	store := &recordingAvailabilityStore{}
	pipeline := &Pipeline{metricsStore: store}
	lane := newStoreWriteLane(1)
	defer lane.close()
	pipeline.storeWriteMu.Lock()
	defer pipeline.storeWriteMu.Unlock()

	done := make(chan bool, 1)
	go func() {
		done <- pipeline.settleUnavailableSource(context.Background(), SessionID("11111111-1111-4111-8111-111111111111"), SourceLaneIndex, SourceUnavailableRecordUnusable, lane, StageIndex)
	}()
	// The lane completion is the wake. The deadline is the failure bound: a
	// write on the caller's lane finishes in-process, and a fallback lock
	// would still be waiting when it expires.
	select {
	case reported := <-done:
		if !reported {
			t.Fatal("the first source refusal was not owed")
		}
	case <-time.After(time.Second):
		t.Fatal("settle did not finish while the fallback mutex was held; the caller's write lane was not used")
	}
	store.mu.Lock()
	records := store.records
	store.mu.Unlock()
	if records != 1 {
		t.Fatalf("records = %d, want the lane to run the write", records)
	}
}

// TestSourceDiagnosticLanes exercises the same reporting helpers used by
// harvest. Both diagnostics can be observed in one run even when they report
// the same source cause; their stored causes remain independent across runs.
func TestSourceDiagnosticLanes(t *testing.T) {
	var fixture struct {
		Required []string `yaml:"required_names"`
		Runs     []struct {
			Name   string `yaml:"name"`
			Force  bool   `yaml:"force"`
			Events []struct {
				Lane       string `yaml:"lane"`
				Cause      string `yaml:"cause"`
				WantReport bool   `yaml:"want_report"`
				WantStored string `yaml:"want_stored"`
			} `yaml:"events"`
		} `yaml:"runs"`
	}
	decodeOneFixture(t, sourceDiagnosticLanesFixtureData, &fixture, "source diagnostic lanes")
	present := make(map[string]bool, len(fixture.Runs))
	for _, run := range fixture.Runs {
		if run.Name == "" || present[run.Name] || len(run.Events) == 0 {
			t.Fatalf("invalid diagnostic-lane run %q", run.Name)
		}
		present[run.Name] = true
	}
	requireNamedCases(t, "source diagnostic lanes", fixture.Required, present)

	store := &recordingAvailabilityStore{}
	sid := SessionID("11111111-1111-4111-8111-111111111111")
	session := DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode}
	decision := RefreshDecision{Reason: RefreshReasonPublicationCaptureMissing, Lane: RefreshLaneNative}
	for _, run := range fixture.Runs {
		t.Run(run.Name, func(t *testing.T) {
			pipeline := &Pipeline{config: PipelineConfig{Force: run.Force}, metricsStore: store}
			for _, event := range run.Events {
				lane, err := NewSourceUnavailabilityLane(event.Lane)
				if err != nil {
					t.Fatal(err)
				}
				cause, err := NewSourceUnavailabilityReason(event.Cause)
				if err != nil {
					t.Fatal(err)
				}
				var sourceErr error
				switch cause {
				case SourceUnavailableNoSavedCopy:
					sourceErr = fs.ErrNotExist
				case SourceUnavailableRecordUnusable:
					sourceErr = malformedSourceRecord{}
				}
				before := pipeline.snapshotDiagnostics()
				var wantType string
				switch lane {
				case SourceLanePairRepair:
					wantType = OutcomeReasonPairRepairUnavailable.Code()
					pipeline.reportPairRepairUnavailable(t.Context(), sid, "/synthetic/source", sourceErr, nil)
				case SourceLanePublicationCapture:
					wantType = OutcomeReasonAdapterRefreshUnavailable.Code()
					pipeline.reportAdapterAcquisitionUnavailable(t.Context(), session, decision, &adapterAcquisitionError{cause: sourceErr}, nil)
				default:
					t.Fatalf("fixture lane %q does not name a diagnostic producer", lane)
				}
				diagnostics := pipeline.snapshotDiagnostics()
				reported := len(diagnostics) > len(before)
				if reported != event.WantReport {
					t.Fatalf("%s/%s report=%t, want %t; diagnostics=%+v", lane, cause, reported, event.WantReport, diagnostics)
				}
				beforeType, afterType := 0, 0
				for _, diagnostic := range before {
					if diagnostic.ErrorType == wantType {
						beforeType++
					}
				}
				for _, diagnostic := range diagnostics {
					if diagnostic.ErrorType == wantType {
						afterType++
					}
				}
				if reported && (len(diagnostics) != len(before)+1 || afterType != beforeType+1) {
					t.Fatalf("%s/%s emitted the wrong diagnostic: before=%+v after=%+v", lane, cause, before, diagnostics)
				}
				stored, ok := store.stored(sid, lane)
				if !ok || string(stored) != event.WantStored {
					t.Fatalf("%s stored %q, %t; want %q", lane, stored, ok, event.WantStored)
				}
			}
		})
	}
}
