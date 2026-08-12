package selectionprojection_test

import (
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/selectionprojection/testfixture"
)

func TestEffectiveProjects_FixtureCorpus(t *testing.T) {
	t.Parallel()

	fixtures, err := testfixture.LoadEffectiveProjectFixtures()
	if err != nil {
		t.Fatalf("load effective-project fixtures: %v", err)
	}
	for _, fixture := range fixtures.ProjectionCases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()

			candidates, err := fixture.ProductionCandidates()
			if err != nil {
				t.Fatalf("build production candidates: %v", err)
			}
			want, err := fixture.ExpectedProjects()
			if err != nil {
				t.Fatalf("build expected projects: %v", err)
			}

			var matcher *ingest.SelectionMatcher
			if fixture.Selection.Mode != config.SelectionModeAll {
				compiled := config.CompileSelectionMatcher(fixture.Selection)
				matcher = &compiled
			}
			got := selectionprojection.EffectiveProjects(matcher, fixture.Selection, candidates)
			if len(got) != len(want) {
				t.Fatalf("effective project count = %d, want %d\ngot:  %+v\nwant: %+v", len(got), len(want), got, want)
			}
			for index := range want {
				if !reflect.DeepEqual(got[index], want[index]) {
					t.Errorf("effective project %d differs\ngot:  %+v\nwant: %+v", index, got[index], want[index])
				}
			}
		})
	}
}

func TestEffectiveProjectFixture_GateRowsReuseProjectionInputs(t *testing.T) {
	t.Parallel()

	fixtures, err := testfixture.LoadEffectiveProjectFixtures()
	if err != nil {
		t.Fatalf("load effective-project fixtures: %v", err)
	}
	for _, gateCase := range fixtures.GateCases {
		if _, ok := fixtures.ProjectionCaseByName(gateCase.ProjectionCase); !ok {
			t.Errorf("gate fixture %q does not resolve projection case %q", gateCase.Name, gateCase.ProjectionCase)
		}
	}
}
