package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/village_user_filter.yaml
var villageUserFilterYAML []byte

// userFilterWorld is the seeded world every case selects from: the caller's
// member collectives and the transcripts their contributors shared into them.
type userFilterWorld struct {
	Collectives []struct {
		ID     schema.VillageUUID `yaml:"id"`
		Name   string             `yaml:"name"`
		Member bool               `yaml:"member"`
	} `yaml:"collectives"`
	Transcripts []struct {
		ID          schema.TranscriptID `yaml:"id"`
		Owner       string              `yaml:"owner"`
		Collectives []string            `yaml:"collectives"`
	} `yaml:"transcripts"`
}

// userFilterCase is one row of testdata/village_user_filter.yaml.
type userFilterCase struct {
	Name            string                `yaml:"name"`
	Command         string                `yaml:"command"`
	Collective      string                `yaml:"collective"`
	User            string                `yaml:"user"`
	Limit           int                   `yaml:"limit"`
	Arg             string                `yaml:"arg"`
	ErrorContains   string                `yaml:"errorContains"`
	Expect          []schema.TranscriptID `yaml:"expect"`
	ExpectTotal     int                   `yaml:"expectTotal"`
	ExpectListCalls int                   `yaml:"expectListCalls"`
}

// args renders the case as the production command line it exercises, so a case
// reads as the invocation a person would type.
func (c userFilterCase) args() []string {
	args := []string{"transcripts"}
	switch c.Command {
	case "list":
		args = append(args, "list", "--json")
	case "list-local":
		args = append(args, "list", "--local", "--json")
	case "list-local-table":
		args = append(args, "list", "--local")
	default:
		args = append(args, "pull")
	}
	if c.Arg != "" {
		args = append(args, c.Arg)
	}
	if c.Collective != "" {
		args = append(args, "--collective", c.Collective)
	}
	if c.User != "" {
		args = append(args, "--user", c.User)
	}
	if c.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(c.Limit))
	}
	return args
}

// TestVillageUserFilterFixtures drives the --user contributor filter over the
// village double's pull surface, on the listing AND on the bulk pull. Each
// fixture case names one behaviour and the command that shows it; the test seeds
// the fixture's world, runs the production command, and asserts the observable
// outcome: which rows were selected, the total the listing reports, and how many
// listing reads the run made. A case that names rows rather than a count cannot
// pass by selecting the right NUMBER of the wrong rows.
func TestVillageUserFilterFixtures(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string         `yaml:"requiredNames"`
		World         userFilterWorld  `yaml:"world"`
		Cases         []userFilterCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(villageUserFilterYAML, &fixture); err != nil {
		t.Fatal(err)
	}

	collectives := make([]testutil.VillageCollective, 0, len(fixture.World.Collectives))
	byName := make(map[string]schema.VillageUUID, len(fixture.World.Collectives))
	for _, collective := range fixture.World.Collectives {
		collectives = append(collectives, testutil.VillageCollective{
			ID: collective.ID, Name: collective.Name, Member: collective.Member,
		})
		byName[collective.Name] = collective.ID
	}

	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			server := testutil.NewCollectiveVillage(t, collectives...)
			server.SetAuthenticatedUser(schema.VillageUUID(stubOwnerUserID), "owneruser")
			for _, row := range fixture.World.Transcripts {
				// Each contributor is a distinct village account, so the filter has
				// more than one account to choose between.
				server.SetPullTranscriptOwnedBy(row.ID, pullCollectiveTestBlob(t, row.ID), nil,
					"user-"+row.Owner+"-001", row.Owner)
				for _, name := range row.Collectives {
					server.Share(row.ID, byName[name], schema.VillageShareStatusApproved)
				}
			}
			writeCredsAt(t, dir, server.URL())
			if strings.HasPrefix(c.Command, "list-local") {
				if out, err := runVillage(t, dir, "transcripts", "pull", "--collective", pullCollectiveAName, "--collective", pullCollectiveBName); err != nil {
					t.Fatalf("seed local inventory: %v (out=%q)", err, out)
				}
				server.Close()
				if err := os.Remove(filepath.Join(string(defaults.ResolveConfigDirPathWith(dir)), string(defaults.CredentialsFile))); err != nil {
					t.Fatal(err)
				}
			}

			out, err := runVillage(t, dir, c.args()...)

			if c.ErrorContains != "" {
				if err == nil {
					t.Fatalf("%s must fail; out=%q", c.Name, out)
				}
				if !strings.Contains(err.Error(), c.ErrorContains) {
					t.Fatalf("%s: error %q does not contain %q", c.Name, err, c.ErrorContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s must exit 0, got err=%v out=%q", c.Name, err, out)
			}

			switch c.Command {
			case "list-local":
				var rows []jsonLocalListRow
				if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
					t.Fatalf("decode offline listing: %v (out=%q)", err, out)
				}
				got := make([]schema.TranscriptID, 0, len(rows))
				for _, row := range rows {
					id, err := schema.NewTranscriptID(row.TranscriptID)
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, id)
				}
				if !sameTranscriptSet(got, c.Expect) {
					t.Errorf("offline listing = %v, want %v", got, c.Expect)
				}
			case "list-local-table":
				for _, row := range fixture.World.Transcripts {
					if strings.Contains(out, row.ID.String()) != slices.Contains(c.Expect, row.ID) {
						t.Errorf("unexpected presence of transcript %s in %q", row.ID, out)
					}
				}
				if len(c.Expect) == 0 && !strings.Contains(out, "No transcripts pulled yet.") {
					t.Errorf("missing empty inventory message: %q", out)
				}
			case "list":
				var response schema.PullListResponse
				if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &response); err != nil {
					t.Fatalf("%s: decode the listing JSON: %v (out=%q)", c.Name, err, out)
				}
				if got := transcriptIDs(response.Transcripts); !sameTranscriptSet(got, c.Expect) {
					t.Errorf("%s: selected %v, want %v", c.Name, got, c.Expect)
				}
				if response.Total != c.ExpectTotal {
					t.Errorf("%s: reported total %d, want %d", c.Name, response.Total, c.ExpectTotal)
				}
				if c.ExpectListCalls > 0 {
					if got := server.PullListCalls(); got != c.ExpectListCalls {
						t.Errorf("%s: made %d listing reads, want %d", c.Name, got, c.ExpectListCalls)
					}
				}
			case "pull":
				if got := pulledTranscriptIDs(t, dir); !sameTranscriptSet(got, c.Expect) {
					t.Errorf("%s: pulled %v, want %v", c.Name, got, c.Expect)
				}
			default:
				t.Fatalf("case %q has no assertion for command %q", c.Name, c.Command)
			}
		})
	}
}

// transcriptIDs projects the listing rows to their identifiers.
func transcriptIDs(rows []schema.PullTranscriptInfo) []schema.TranscriptID {
	out := make([]schema.TranscriptID, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].TranscriptID)
	}
	return out
}

// sameTranscriptSet reports whether two lists name the same transcripts, in any
// order. Selection is order-independent, so a case asserts the set.
func sameTranscriptSet(got, want []schema.TranscriptID) bool {
	got = append([]schema.TranscriptID(nil), got...)
	want = append([]schema.TranscriptID(nil), want...)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

// pulledTranscriptIDs reads the local pulled inventory, so a pull case asserts
// the transcripts that actually landed rather than the ones it meant to pull.
func pulledTranscriptIDs(t *testing.T, dir string) []schema.TranscriptID {
	t.Helper()
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	rows, err := db.ListPulledTranscripts(t.Context())
	if err != nil {
		t.Fatalf("list pulled transcripts: %v", err)
	}
	out := make([]schema.TranscriptID, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].TranscriptID)
	}
	return out
}
