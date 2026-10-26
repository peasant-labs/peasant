package main

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/collective_refs.yaml
var collectiveRefsYAML []byte

// TestCollectiveRefsFixtures drives every accepted and refused form of a
// --collective value against the village double's collective list. The refusals
// matter as much as the resolutions: a value that names nothing, names two
// collectives, or names a collective the caller does not belong to must stop the
// command before it lists or pulls anything.
func TestCollectiveRefsFixtures(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Collectives   []struct {
			ID     schema.VillageUUID `yaml:"id"`
			Name   string             `yaml:"name"`
			Member bool               `yaml:"member"`
		} `yaml:"collectives"`
		Cases []struct {
			Name          string `yaml:"name"`
			Raw           string `yaml:"raw"`
			ID            string `yaml:"id"`
			ErrorContains string `yaml:"errorContains"`
			ErrorMentions string `yaml:"errorMentions"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(collectiveRefsYAML, &fixture); err != nil {
		t.Fatal(err)
	}

	collectives := make([]testutil.VillageCollective, 0, len(fixture.Collectives))
	for _, collective := range fixture.Collectives {
		collectives = append(collectives, testutil.VillageCollective{
			ID: collective.ID, Name: collective.Name, Member: collective.Member,
		})
	}

	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()

			server := testutil.NewCollectiveVillage(t, collectives...)
			client := village.NewVillageClient(server.URL(), "test-key", nil)

			ids, names, err := resolveCollectiveRefs(t.Context(), client, []string{c.Raw})

			if c.ErrorContains != "" {
				if err == nil {
					t.Fatalf("resolve %q: got ids=%v names=%v, want an error containing %q", c.Raw, ids, names, c.ErrorContains)
				}
				if !strings.Contains(err.Error(), c.ErrorContains) {
					t.Fatalf("resolve %q: error %q does not contain %q", c.Raw, err, c.ErrorContains)
				}
				if c.ErrorMentions != "" && !strings.Contains(err.Error(), c.ErrorMentions) {
					t.Fatalf("resolve %q: error %q does not mention %q", c.Raw, err, c.ErrorMentions)
				}
				if len(ids) != 0 {
					t.Fatalf("resolve %q: refused but returned ids %v", c.Raw, ids)
				}
				return
			}

			if err != nil {
				t.Fatalf("resolve %q: %v", c.Raw, err)
			}
			if len(ids) != 1 || ids[0].String() != c.ID {
				t.Fatalf("resolve %q: got ids=%v, want exactly [%s]", c.Raw, ids, c.ID)
			}
			if len(names) != 1 || names[0] == "" {
				t.Fatalf("resolve %q: got names=%v, want one non-empty name", c.Raw, names)
			}
		})
	}
}

// TestResolveCollectiveRefsCollapsesDuplicates checks the repeatable flag's
// separate concern: the same collective named twice, in two different forms, is
// resolved once. Downloading it twice would be the visible symptom.
func TestResolveCollectiveRefsCollapsesDuplicates(t *testing.T) {
	t.Parallel()
	const id schema.VillageUUID = "11111111-1111-4111-8111-111111111111"
	server := testutil.NewCollectiveVillage(t, testutil.VillageCollective{ID: id, Name: "platform-team", Member: true})
	client := village.NewVillageClient(server.URL(), "test-key", nil)

	ids, names, err := resolveCollectiveRefs(t.Context(), client, []string{
		"platform-team",
		id.String(),
		"https://village.example.com/groups/" + id.String(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("got ids=%v, want exactly one entry for %s", ids, id)
	}
	if len(names) != 1 {
		t.Fatalf("got names=%v, want exactly one name", names)
	}
}
