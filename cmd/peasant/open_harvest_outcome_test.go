package main

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/open-harvest-outcome.yaml
var openHarvestOutcomeYAML []byte

// openHarvestOutcomeOtherSessionID is the other session a synthetic run
// processed besides the one open asked for.
const openHarvestOutcomeOtherSessionID = openTestUnknownSessionID

// openHarvestKind is the completion kind a synthetic execution carries, as the
// open-harvest-outcome fixture spells it.
type openHarvestKind string

const (
	openHarvestSucceeded openHarvestKind = "succeeded"
	openHarvestCanceled  openHarvestKind = "canceled"
	openHarvestFailed    openHarvestKind = "failed"
	openHarvestUnknown   openHarvestKind = "unknown"
)

// openHarvestSessionRef names which session a fixture row belongs to.
type openHarvestSessionRef string

const (
	openHarvestSelf  openHarvestSessionRef = "self"
	openHarvestOther openHarvestSessionRef = "other"
)

type openHarvestOutcomeFixture struct {
	RequiredNames []string                 `yaml:"requiredNames"`
	Cases         []openHarvestOutcomeCase `yaml:"cases"`
}

type openHarvestOutcomeCase struct {
	Name          string          `yaml:"name"`
	Kind          openHarvestKind `yaml:"kind"`
	Result        bool            `yaml:"result"`
	UIError       string          `yaml:"ui_error"`
	RunError      string          `yaml:"run_error"`
	StoreError    string          `yaml:"store_error"`
	SessionErrors []struct {
		Session openHarvestSessionRef `yaml:"session"`
		Error   string                `yaml:"error"`
	} `yaml:"session_errors"`
	IndexLog []struct {
		Session openHarvestSessionRef `yaml:"session"`
		Outcome ingest.IndexOutcome   `yaml:"outcome"`
	} `yaml:"index_log"`
	WantError string `yaml:"want_error"`
}

func loadOpenHarvestOutcomeFixture(t *testing.T) openHarvestOutcomeFixture {
	t.Helper()
	var fixture openHarvestOutcomeFixture
	if err := testutil.DecodeNamedFixtureYAML(openHarvestOutcomeYAML, &fixture); err != nil {
		t.Fatalf("decode open harvest outcome fixture: %v", err)
	}
	return fixture
}

func (r openHarvestSessionRef) id(t *testing.T) ingest.SessionID {
	t.Helper()
	switch r {
	case openHarvestSelf:
		return openTestSessionID
	case openHarvestOther:
		return openHarvestOutcomeOtherSessionID
	}
	t.Fatalf("unknown session %q", r)
	return ""
}

// execution builds the synthetic harvestExecution the case describes.
func (tc openHarvestOutcomeCase) execution(t *testing.T) harvestExecution {
	t.Helper()
	var execution harvestExecution
	switch tc.Kind {
	case openHarvestSucceeded:
		execution.kind = harvestCompletionSucceeded
	case openHarvestCanceled:
		execution.kind = harvestCompletionCanceled
		execution.ctxErr = context.Canceled
	case openHarvestFailed:
		execution.kind = harvestCompletionFailed
	case openHarvestUnknown:
		// One past the last kind the harvest defines.
		execution.kind = harvestCompletionFailed + 1
	default:
		t.Fatalf("case %q has an unknown kind %q", tc.Name, tc.Kind)
	}
	if tc.UIError != "" {
		execution.uiErr = errors.New(tc.UIError)
	}
	if tc.RunError != "" {
		execution.runErr = errors.New(tc.RunError)
	}
	if !tc.Result {
		if tc.StoreError != "" || len(tc.SessionErrors) != 0 || len(tc.IndexLog) != 0 {
			t.Fatalf("case %q describes a result it does not carry", tc.Name)
		}
		return execution
	}
	result := &ingest.PipelineResult{}
	if tc.StoreError != "" {
		result.Summary.StoreError = errors.New(tc.StoreError)
	}
	for _, failure := range tc.SessionErrors {
		result.Sessions = append(result.Sessions, ingest.SessionResult{SessionID: failure.Session.id(t), Error: errors.New(failure.Error)})
	}
	for _, entry := range tc.IndexLog {
		if !entry.Outcome.IsValid() {
			t.Fatalf("case %q has an unknown index outcome %q", tc.Name, entry.Outcome)
		}
		result.IndexLog = append(result.IndexLog, ingest.IndexLogEntry{SessionID: entry.Session.id(t), Outcome: entry.Outcome})
	}
	execution.result = result
	return execution
}

// TestOpenHarvestOutcome checks every branch of the reducer that turns a
// harvest execution into the result of open's harvest step.
func TestOpenHarvestOutcome(t *testing.T) {
	t.Parallel()
	fixture := loadOpenHarvestOutcomeFixture(t)
	replacer := strings.NewReplacer("{session_id}", openTestSessionID)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			err := openHarvestOutcome(tc.execution(t), openTestSessionID)
			want := replacer.Replace(tc.WantError)
			switch {
			case want == "" && err != nil:
				t.Fatalf("openHarvestOutcome = %v, want the session recorded", err)
			case want != "" && (err == nil || err.Error() != want):
				t.Fatalf("openHarvestOutcome = %v, want %q", err, want)
			}
		})
	}
}
