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
	reason  *SourceUnavailabilityReason
	records int
}

func (s *recordingAvailabilityStore) ReadSourceUnavailability(context.Context, SessionID) (*SourceUnavailabilityReason, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason == nil {
		return nil, nil
	}
	copied := *s.reason
	return &copied, nil
}

func (s *recordingAvailabilityStore) RecordSourceUnavailable(_ context.Context, _ SessionID, reason SourceUnavailabilityReason) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records++
	if s.reason != nil && *s.reason == reason {
		return false, nil
	}
	copied := reason
	s.reason = &copied
	return true, nil
}

func (s *recordingAvailabilityStore) stored() (SourceUnavailabilityReason, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason == nil {
		return "", false
	}
	return *s.reason, true
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
		// A new pipeline is a later harvest. The same pipeline would still
		// owe a report it recorded earlier in that run.
		pipeline := &Pipeline{config: PipelineConfig{Force: step.Force}, metricsStore: store}
		got := pipeline.settleUnavailableSource(ctx, sid, cause, nil)
		if got != step.WantReport {
			t.Fatalf("step %s report = %v, want %v", step.Name, got, step.WantReport)
		}
		stored, ok := store.stored()
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
		done <- pipeline.settleUnavailableSource(context.Background(), SessionID("11111111-1111-4111-8111-111111111111"), SourceUnavailableRecordUnusable, lane)
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
