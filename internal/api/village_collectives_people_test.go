package api

import (
	_ "embed"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/village_collectives_people.yaml
var villageCollectivesPeopleYAML []byte

// TestVillageCollectivesRouteReadsEitherPersonShape mounts the popup's
// collectives route against a village double serving each person shape and
// asserts a valid body with the repository and organization suggestions.
func TestVillageCollectivesRouteReadsEitherPersonShape(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string `yaml:"name"`
			LegacyPeople bool   `yaml:"legacyPeople"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(villageCollectivesPeopleYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			world := newPublishingWorld(t)
			world.village.ServeLegacyCollectivePeople(c.LegacyPeople)
			status, body := world.request(t, http.MethodGet, defaults.RouteVillageCollectives.String()+"?sessionId="+insideSessionID, nil)
			var response schema.LocalVillageCollectivesResponse
			decodeContract(t, status, body, &response)
			suggestions := map[string]*schema.LocalCollectiveSuggestion{}
			for _, collective := range response.Collectives {
				suggestions[collective.Group.Name] = collective.Suggestion
			}
			if len(suggestions) != 3 {
				t.Fatalf("collectives = %v; want the three the user belongs to", suggestions)
			}
			if s := suggestions["platform"]; s == nil || s.Reason != schema.LocalCollectiveSuggestionLinkedRepository || s.Match != "github.com:acme/tools" {
				t.Errorf("platform suggestion = %+v; it links the session's repository", s)
			}
			if s := suggestions["research"]; s == nil || s.Reason != schema.LocalCollectiveSuggestionLinkedGithubOrg || s.Match != acmeOrg {
				t.Errorf("research suggestion = %+v; it links the session's organization", s)
			}
		})
	}
}
