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

// TestRefreshReasonsUseTheOutcomeVocabulary pins exact membership and lane
// mapping, including the decisions that do not write an outcome.
func TestRefreshReasonsUseTheOutcomeVocabulary(t *testing.T) {
	var fixture struct {
		Reasons []struct {
			Code      string `yaml:"code"`
			Lane      string `yaml:"lane"`
			NoOutcome bool   `yaml:"no_outcome"`
		} `yaml:"reasons"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(refreshReasonsYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	want := make(map[ingest.RefreshReason]bool)
	for _, row := range fixture.Reasons {
		reason, err := ingest.NewRefreshReason(row.Code)
		if err != nil {
			t.Fatal(err)
		}
		if want[reason] {
			t.Fatalf("fixture repeats reason %q", row.Code)
		}
		want[reason] = true
		lane, err := ingest.NewRefreshLane(row.Lane)
		if err != nil {
			t.Fatal(err)
		}
		outcome := reason.OutcomeReason()
		if row.NoOutcome {
			if outcome != 0 {
				t.Errorf("%q unexpectedly writes outcome %s", reason, outcome.Code())
			}
		} else {
			if !outcome.Defined() || outcome.Code() != row.Code {
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
		if outcome.Kind() == ingest.OutcomeKindRefresh && !want[ingest.RefreshReason(outcome.Code())] {
			t.Errorf("refresh outcome %q is absent from the fixture", outcome.Code())
		}
	}
}
