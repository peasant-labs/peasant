package ingest_test

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_reasons.yaml
var refreshReasonsYAML []byte

type refreshReasonsFixture struct {
	Reasons []struct {
		Code      ingest.RefreshReason `yaml:"code"`
		Lane      ingest.RefreshLane   `yaml:"lane"`
		NoOutcome bool                 `yaml:"no_outcome"`
	} `yaml:"reasons"`
}

func loadRefreshReasonsFixture(t *testing.T) refreshReasonsFixture {
	t.Helper()
	var fixture refreshReasonsFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(refreshReasonsYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode refresh_reasons.yaml: %v", err)
	}
	for _, row := range fixture.Reasons {
		if _, err := ingest.NewRefreshReason(string(row.Code)); err != nil {
			t.Fatalf("refresh_reasons.yaml code: %v", err)
		}
		if _, err := ingest.NewRefreshLane(string(row.Lane)); err != nil {
			t.Fatalf("refresh_reasons.yaml code %s lane: %v", row.Code, err)
		}
	}
	return fixture
}

// TestRefreshReasonsUseTheOutcomeVocabulary pins exact membership and lane
// mapping, including the decisions that do not write an outcome.
func TestRefreshReasonsUseTheOutcomeVocabulary(t *testing.T) {
	fixture := loadRefreshReasonsFixture(t)
	want := make(map[ingest.RefreshReason]bool)
	for _, row := range fixture.Reasons {
		reason, lane := row.Code, row.Lane
		if want[reason] {
			t.Fatalf("fixture repeats reason %q", row.Code)
		}
		want[reason] = true
		outcome := reason.OutcomeReason()
		if row.NoOutcome {
			if outcome != 0 {
				t.Errorf("%q unexpectedly writes outcome %s", reason, outcome.Code())
			}
		} else {
			if !outcome.Defined() || outcome.Code() != string(row.Code) {
				t.Errorf("%q maps to outcome %s, want its own outcome", reason, outcome.Code())
			}
			if outcome.Kind() != ingest.OutcomeKindRefresh || outcome.RefreshLane() != lane {
				t.Errorf("%q maps to kind %s lane %s, want refresh in lane %s", reason, outcome.Kind(), outcome.RefreshLane(), lane)
			}
		}
	}
	seen := make(map[ingest.RefreshReason]bool)
	for _, reason := range ingest.AllRefreshReasons {
		if seen[reason] || !want[reason] {
			t.Errorf("decision reason %q is repeated or absent from the fixture", reason)
		}
		seen[reason] = true
	}
	for reason := range want {
		if !seen[reason] {
			t.Errorf("fixture reason %q is absent from AllRefreshReasons", reason)
		}
	}
	for _, outcome := range ingest.AllOutcomeReasons {
		if outcome.Kind() == ingest.OutcomeKindRefresh {
			reason, err := ingest.NewRefreshReason(outcome.Code())
			if err != nil || !want[reason] {
				t.Errorf("refresh outcome %q is absent from the fixture: %v", outcome.Code(), err)
			}
		}
	}
}
