package village_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/legacy_people.yaml
var legacyPeopleYAML []byte

// Collective reads accept a village before Village API 0.29.0: an account id
// or unreadable person reference decodes as anonymous, a profile is kept, and
// the decoded value re-encodes.
func TestCollectiveReadsAcceptLegacyPeopleFixtures(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name                string                          `yaml:"name"`
			Surface             string                          `yaml:"surface"`
			Body                string                          `yaml:"body"`
			Want                []schema.VillagePublicOwnerKind `yaml:"want"`
			WantRepositoryOwner string                          `yaml:"wantRepositoryOwner"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(legacyPeopleYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	const collective = schema.VillageUUID("11111111-1111-4111-8111-111111111111")
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, strings.ReplaceAll(c.Body, "COLLECTIVE", collective.String()))
			}))
			t.Cleanup(srv.Close)
			client := village.NewVillageClient(srv.URL, testAPIKey, nil)

			var people []schema.VillagePublicOwner
			var decoded any
			var err error
			switch c.Surface {
			case "collectives":
				var rows []schema.VillageUserCollective
				rows, err = client.ListCollectives(context.Background())
				for _, row := range rows {
					people = append(people, row.Creator)
				}
				decoded = rows
			case "repositories":
				var rows []schema.VillageLinkedRepository
				rows, err = client.ListCollectiveRepositories(context.Background(), collective)
				for _, row := range rows {
					people = append(people, row.LinkedBy)
					if row.Owner != c.WantRepositoryOwner {
						t.Errorf("repository owner = %q, want %q (a GitHub login, not a person)", row.Owner, c.WantRepositoryOwner)
					}
				}
				decoded = rows
			case "detail":
				var row schema.VillageCollectiveDetailRecord
				row, err = client.ReadObservationCollective(context.Background(), collective)
				people = append(people, row.Creator)
				decoded = row
			default:
				t.Fatalf("unknown surface %q", c.Surface)
			}
			if err != nil {
				t.Fatalf("read %s: %v", c.Surface, err)
			}
			if len(people) != len(c.Want) {
				t.Fatalf("read %d people, want %d", len(people), len(c.Want))
			}
			for i, want := range c.Want {
				if people[i].Kind != want {
					t.Errorf("person %d kind = %q, want %q", i, people[i].Kind, want)
				}
			}
			if _, err := json.Marshal(decoded); err != nil {
				t.Errorf("decoded %s does not re-encode: %v", c.Surface, err)
			}
		})
	}
}
