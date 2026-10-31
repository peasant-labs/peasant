package defaults

import (
	_ "embed"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/session_workflow_facets.yaml
var sessionWorkflowFacetsYAML []byte

type sessionWorkflowFacetCase struct {
	Name    string   `yaml:"name"`
	Set     []string `yaml:"set"`
	Refused *string  `yaml:"refused"`
}

type sessionWorkflowFacetFixture struct {
	RequiredNames []string                   `yaml:"requiredNames"`
	Cases         []sessionWorkflowFacetCase `yaml:"cases"`
}

func loadSessionWorkflowFacetFixture(t *testing.T) sessionWorkflowFacetFixture {
	t.Helper()
	var fixture sessionWorkflowFacetFixture
	if err := yaml.Unmarshal(sessionWorkflowFacetsYAML, &fixture); err != nil {
		t.Fatalf("decode session workflow facet fixture: %v", err)
	}
	for _, name := range fixture.RequiredNames {
		if !slices.ContainsFunc(fixture.Cases, func(c sessionWorkflowFacetCase) bool { return c.Name == name }) {
			t.Errorf("session_workflow_facets.yaml lost the required case %q", name)
		}
	}
	return fixture
}

func TestSessionWorkflowFacets(t *testing.T) {
	t.Parallel()
	for _, tc := range loadSessionWorkflowFacetFixture(t).Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			if tc.Set != nil {
				got := make([]string, len(AllSessionWorkflowFacets))
				for i, facet := range AllSessionWorkflowFacets {
					got[i] = facet.String()
					accepted, err := NewSessionWorkflowFacet(facet.String())
					if err != nil || accepted != facet {
						t.Errorf("NewSessionWorkflowFacet(%q) = %q, %v", facet, accepted, err)
					}
				}
				if !slices.Equal(got, tc.Set) {
					t.Fatalf("AllSessionWorkflowFacets = %v, want %v", got, tc.Set)
				}
			}
			if tc.Refused != nil {
				_, err := NewSessionWorkflowFacet(*tc.Refused)
				if err == nil {
					t.Fatalf("NewSessionWorkflowFacet(%q) was accepted", *tc.Refused)
				}
				for _, facet := range AllSessionWorkflowFacets {
					if !strings.Contains(err.Error(), facet.String()) {
						t.Errorf("refusal %q does not name %q", err, facet)
					}
				}
			}
		})
	}
}
