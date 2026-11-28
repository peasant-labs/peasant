package ingest_test

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_reasons.yaml
var refreshReasonsYAML []byte

//go:embed testdata/refresh_reasons.manifest.yaml
var refreshReasonsManifestYAML []byte

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
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decoder = yaml.NewDecoder(strings.NewReader(string(refreshReasonsManifestYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode refresh reasons manifest: %v", err)
	}
	required := make(map[string]bool)
	for _, name := range manifest.RequiredNames {
		if name == "" || required[name] {
			t.Fatalf("empty or duplicate required refresh reason %q", name)
		}
		required[name] = true
	}
	actual := make(map[string]bool)
	for _, row := range fixture.Reasons {
		name := string(row.Code)
		if !required[name] || actual[name] {
			t.Fatalf("refresh reason %q is not in the manifest or is duplicated", name)
		}
		actual[name] = true
		if _, err := ingest.NewRefreshReason(string(row.Code)); err != nil {
			t.Fatalf("refresh_reasons.yaml code: %v", err)
		}
		if _, err := ingest.NewRefreshLane(string(row.Lane)); err != nil {
			t.Fatalf("refresh_reasons.yaml code %s lane: %v", row.Code, err)
		}
	}
	for name := range required {
		if !actual[name] {
			t.Fatalf("required refresh reason %q missing from fixture", name)
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

//go:embed testdata/refresh_decision_adapter_matrix.yaml
var refreshDecisionAdapterMatrixYAML []byte

//go:embed testdata/refresh_decision_adapter_matrix.manifest.yaml
var refreshDecisionAdapterMatrixManifestYAML []byte

type refreshDecisionAdapterMatrixFixture struct {
	DecideRefreshCases []struct {
		Name                  string               `yaml:"name"`
		AdapterCanExtract     bool                 `yaml:"adapterCanExtract"`
		AdapterUndeclared     bool                 `yaml:"adapterUndeclared"`
		AdapterStampPresent   bool                 `yaml:"adapterStampPresent"`
		WantReason            ingest.RefreshReason `yaml:"wantReason"`
		WantLane              ingest.RefreshLane   `yaml:"wantLane"`
		WantAdapterUndeclared bool                 `yaml:"wantAdapterUndeclared"`
	} `yaml:"decideRefreshCases"`
	LaneProfiles []struct {
		Name                   string                 `yaml:"name"`
		AdapterCanExtract      bool                   `yaml:"adapterCanExtract"`
		IndexerHasMetadataHalf bool                   `yaml:"indexerHasMetadataHalf"`
		AcceptedReasons        []ingest.RefreshReason `yaml:"acceptedReasons"`
	} `yaml:"laneProfiles"`
}

func TestDecideRefreshAdapterMatrix(t *testing.T) {
	var fixture refreshDecisionAdapterMatrixFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(refreshDecisionAdapterMatrixYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode refresh_decision_adapter_matrix.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(refreshDecisionAdapterMatrixManifestYAML, "refresh decision adapter matrix")
	if err != nil {
		t.Fatal(err)
	}
	var actualNames []string
	for _, c := range fixture.DecideRefreshCases {
		actualNames = append(actualNames, c.Name)
	}
	for _, p := range fixture.LaneProfiles {
		actualNames = append(actualNames, p.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, actualNames, "refresh decision adapter matrix"); err != nil {
		t.Fatal(err)
	}
	if len(fixture.DecideRefreshCases) != 8 {
		t.Fatalf("decideRefreshCases count = %d, want 8", len(fixture.DecideRefreshCases))
	}
	for _, c := range fixture.DecideRefreshCases {
		t.Run(c.Name, func(t *testing.T) {
			got := ingest.DecideRefresh(ingest.RefreshEvidence{
				StoredRow:           true,
				AdapterVersion:      4,
				AdapterTarget:       5,
				AdapterCanExtract:   c.AdapterCanExtract,
				AdapterUndeclared:   c.AdapterUndeclared,
				AdapterStampPresent: c.AdapterStampPresent,
			})
			want := ingest.RefreshDecision{
				Reason:            c.WantReason,
				Lane:              c.WantLane,
				AdapterUndeclared: c.WantAdapterUndeclared,
			}
			if got != want {
				t.Fatalf("DecideRefresh() = %+v, want %+v", got, want)
			}
		})
	}
}
