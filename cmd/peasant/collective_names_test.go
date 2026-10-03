package main

import (
	_ "embed"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/collective_names.yaml
var collectiveNamesYAML []byte

func TestCollectiveNamesFixtures(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name       string `yaml:"name"`
			Known      string `yaml:"known"`
			Membership string `yaml:"membership"`
			Status     int    `yaml:"status"`
			Expected   string `yaml:"expected"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(collectiveNamesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	const id schema.VillageUUID = "11111111-1111-4111-8111-111111111111"
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.Membership == "" || c.Expected == "" {
				t.Fatal("a case needs a membership name and expected name")
			}
			server := testutil.NewCollectiveVillage(t, testutil.VillageCollective{ID: id, Name: c.Membership, Member: true})
			server.AnswerCollectives(c.Status)
			client := village.NewVillageClient(server.URL(), "test-key", nil)
			got := collectiveNames(t.Context(), client, []schema.VillageUUID{id}, map[schema.VillageUUID]string{id: c.Known})
			if !slices.Equal(got, []string{c.Expected}) {
				t.Errorf("names = %v, want %q", got, c.Expected)
			}
		})
	}
}
