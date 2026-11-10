package api

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/grouped_row_titles.yaml
var groupedRowTitlesYAML []byte

type groupedRowTitleCase struct {
	Name                string              `yaml:"name"`
	Variant             GroupedRouteVariant `yaml:"variant"`
	Title               string              `yaml:"title"`
	HelperTitle         string              `yaml:"helperTitle"`
	ExpectedTitle       string              `yaml:"expectedTitle"`
	ExpectedHelperTitle string              `yaml:"expectedHelperTitle"`
}

type groupedRowTitleFixture struct {
	Required []string              `yaml:"requiredNames"`
	Cases    []groupedRowTitleCase `yaml:"cases"`
}

func loadGroupedRowTitleFixture(t *testing.T) groupedRowTitleFixture {
	t.Helper()
	var fixture groupedRowTitleFixture
	if err := testutil.DecodeFixtureYAML(groupedRowTitlesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("testdata/grouped_row_titles.yaml", "grouped row titles", fixture.Required, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestGroupedRowsCarryStoredTitles pins the server half of the grouped-title
// contract: the sessions and sync variants set the stored title on every row a
// client renders, the search variant omits it, and a helper member carries its
// own stored title. Titles come from the bounded row read, never from the
// retired full sessions channel.
func TestGroupedRowsCarryStoredTitles(t *testing.T) {
	t.Parallel()
	for _, c := range loadGroupedRowTitleFixture(t).Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			filters := GroupedFilters{Variant: c.Variant}
			owner := GroupedCandidate{
				Summary:  schema.SessionSummary{ID: "agent-owner"},
				Title:    c.Title,
				StartMs:  500,
				StableID: "agent-owner",
				Harness:  "codex",
				Purpose:  schema.SessionPurposeInteraction,
			}
			if c.Variant == GroupedRouteSearch {
				owner.Matches = []schema.SearchResult{{
					SessionID:  "agent-owner",
					EntryIndex: 0,
					Score:      1,
					Snippet:    "needle",
				}}
			}
			candidates := []GroupedCandidate{owner}
			if c.HelperTitle != "" {
				candidates = append(candidates, GroupedCandidate{
					Summary:            schema.SessionSummary{ID: "agent-helper"},
					Title:              c.HelperTitle,
					StartMs:            490,
					StableID:           "agent-helper",
					Harness:            "codex",
					Purpose:            schema.SessionPurposeHelperReview,
					OwnerState:         schema.RelationshipTargetKnown,
					OwnerTargetLocalID: "agent-owner",
				})
			}

			issuer := groupedScopeIssuerFunc(func(string, GroupedRouteVariant, GroupedFilters) (string, error) {
				return "scope-stub", nil
			})
			payload, err := buildGroupedListPayload(filters, candidates, 1, schema.PageSize25, "7", issuer)
			if err != nil {
				t.Fatalf("build grouped list: %v", err)
			}
			ownerItem := groupedTranscriptItem(t, payload.Items, "agent-owner")
			if ownerItem.Transcript.Title != c.ExpectedTitle {
				t.Fatalf("owner row title = %q, want %q", ownerItem.Transcript.Title, c.ExpectedTitle)
			}
			if len(ownerItem.HelperGroups) == 0 {
				return
			}
			members, err := buildGroupedMembersPayload(ownerItem.HelperGroups[0].GroupID, filters, candidates, 1, schema.PageSize25, "7", issuer)
			if err != nil {
				t.Fatalf("build grouped members: %v", err)
			}
			helperItem := groupedTranscriptItem(t, members.Members, "agent-helper")
			if helperItem.Transcript.Title != c.ExpectedHelperTitle {
				t.Fatalf("helper row title = %q, want %q", helperItem.Transcript.Title, c.ExpectedHelperTitle)
			}
		})
	}
}

func groupedTranscriptItem(t *testing.T, items []schema.LocalSessionListItem, sessionID string) *schema.LocalSessionListItem {
	t.Helper()
	for i := range items {
		row := items[i].Transcript
		if row != nil && row.Session.ID == sessionID {
			return &items[i]
		}
	}
	t.Fatalf("grouped payload carries no transcript row for %s", sessionID)
	return nil
}
