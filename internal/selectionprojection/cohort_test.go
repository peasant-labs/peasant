package selectionprojection_test

import (
	_ "embed"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/cohorts.yaml
var cohortFixtureYAML []byte

type cohortFixture struct {
	ExpectedLabelCaseCount        int                      `yaml:"expectedLabelCaseCount"`
	LabelCases                    []cohortLabelCase        `yaml:"labelCases"`
	ExpectedMultiplicityCaseCount int                      `yaml:"expectedMultiplicityCaseCount"`
	MultiplicityCases             []cohortMultiplicityCase `yaml:"multiplicityCases"`
}
type cohortLabelCase struct {
	Name     string
	Evidence []cohortLabelEvidence
	Expected []string
}
type cohortLabelEvidence struct{ Group, Path, Cohort string }
type cohortMultiplicityCase struct {
	Name     string
	Evidence []cohortMultiplicityEvidence
	Expected []string
}
type cohortMultiplicityEvidence struct{ Harness, Text, Cohort string }

func loadCohortFixture(t *testing.T) cohortFixture {
	t.Helper()
	var fixture cohortFixture
	if err := yaml.Unmarshal(cohortFixtureYAML, &fixture); err != nil {
		t.Fatalf("decode cohort fixture: %v", err)
	}
	if len(fixture.LabelCases) != fixture.ExpectedLabelCaseCount || len(fixture.MultiplicityCases) != fixture.ExpectedMultiplicityCaseCount {
		t.Fatalf("cohort fixture row guards do not match: labels=%d multiplicities=%d", len(fixture.LabelCases), len(fixture.MultiplicityCases))
	}
	return fixture
}

func TestDistinctLocationLabelsFixtures(t *testing.T) {
	for _, testCase := range loadCohortFixture(t).LabelCases {
		t.Run(testCase.Name, func(t *testing.T) {
			evidence := make([]selectionprojection.LocationLabelEvidence, len(testCase.Evidence))
			for i, item := range testCase.Evidence {
				evidence[i] = selectionprojection.LocationLabelEvidence{PresentationGroup: item.Group, ClonePath: ingest.ClonePath(item.Path), CohortKey: ingest.RepositoryCohortKey(item.Cohort)}
			}
			if got := selectionprojection.DistinctLocationLabels(evidence); !reflect.DeepEqual(got, testCase.Expected) {
				t.Fatalf("labels=%v, want %v", got, testCase.Expected)
			}
		})
	}
}

func TestCohortMultiplicitiesFixtures(t *testing.T) {
	for _, testCase := range loadCohortFixture(t).MultiplicityCases {
		t.Run(testCase.Name, func(t *testing.T) {
			evidence := make([]selectionprojection.CohortEvidence, len(testCase.Evidence))
			for i, item := range testCase.Evidence {
				evidence[i] = selectionprojection.CohortEvidence{Harness: ingest.Harness(item.Harness), Text: item.Text, CohortKey: ingest.RepositoryCohortKey(item.Cohort)}
			}
			got := selectionprojection.CohortMultiplicities(evidence)
			actual := make([]string, len(got))
			for i := range got {
				switch got[i] {
				case ingest.DiscoveryIdentityUnique:
					actual[i] = "unique"
				case ingest.DiscoveryIdentityAmbiguous:
					actual[i] = "ambiguous"
				default:
					t.Fatalf("unknown multiplicity %d", got[i])
				}
			}
			if !reflect.DeepEqual(actual, testCase.Expected) {
				t.Fatalf("multiplicities=%v, want %v", actual, testCase.Expected)
			}
		})
	}
}
