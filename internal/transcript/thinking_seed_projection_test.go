package transcript

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/thinking_seed_projection.yaml
var thinkingSeedProjectionFixtureYAML []byte

//go:embed testdata/thinking_seed_projection.manifest.yaml
var thinkingSeedProjectionManifestYAML []byte

type thinkingLevelPair struct {
	Level string `yaml:"level,omitempty"`
	Raw   string `yaml:"raw,omitempty"`
}

// thinkingLevelChanges converts a fixture history into wire history entries.
// An empty fixture list yields nil, which is how an absent history is
// represented.
func thinkingLevelChanges(pairs []thinkingLevelPair) []schema.ThinkingLevelChange {
	if len(pairs) == 0 {
		return nil
	}
	changes := make([]schema.ThinkingLevelChange, len(pairs))
	for i, pair := range pairs {
		changes[i] = schema.ThinkingLevelChange{Level: schema.ThinkingLevel(pair.Level), Raw: schema.ThinkingLevelRaw(pair.Raw)}
	}
	return changes
}

type thinkingSeedEntry struct {
	Index       int    `yaml:"index"`
	Role        string `yaml:"role"`
	Depth       int    `yaml:"depth"`
	ParentIndex *int   `yaml:"parentIndex,omitempty"`
	Content     string `yaml:"content"`
	Level       string `yaml:"level,omitempty"`
	Raw         string `yaml:"raw,omitempty"`
}

type thinkingSeedCase struct {
	Name            string              `yaml:"name"`
	StoredSeed      thinkingLevelPair   `yaml:"storedSeed"`
	StoredHistory   []thinkingLevelPair `yaml:"storedHistory"`
	Entries         []thinkingSeedEntry `yaml:"entries"`
	ExpectedSeed    thinkingLevelPair   `yaml:"expectedSeed"`
	ExpectedHistory []thinkingLevelPair `yaml:"expectedHistory"`
	ExpectedTurns   []thinkingLevelPair `yaml:"expectedTurns"`
}

type thinkingSeedFixture struct {
	Cases                 []thinkingSeedCase `yaml:"cases"`
	NegativeSeedFailures  []string           `yaml:"negativeSeedFailures"`
	NegativeScopeFailures []string           `yaml:"negativeScopeFailures"`
}

func loadThinkingSeedFixture(t *testing.T) thinkingSeedFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(thinkingSeedProjectionFixtureYAML))
	decoder.KnownFields(true)
	var fixture thinkingSeedFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode thinking seed projection fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("thinking seed fixture must contain one document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(thinkingSeedProjectionManifestYAML, "thinking seed projection")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		if len(fixtureCase.Entries) == 0 {
			t.Fatalf("thinking seed projection case %q has no entries", fixtureCase.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "thinking seed projection"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func runThinkingSeedFixture(fixture thinkingSeedFixture) map[string][]string {
	failures := make(map[string][]string, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		entries := make([]schema.SessionEntry, len(fixtureCase.Entries))
		for index, source := range fixtureCase.Entries {
			content := source.Content
			entry := schema.SessionEntry{EntryIndex: source.Index, Role: schema.Role(source.Role), Depth: source.Depth, ParentIndex: source.ParentIndex, EntryType: schema.EntryTypeText, ContentPreview: &content}
			extra := map[string]string{}
			if source.Level != "" {
				extra["thinking_level"] = source.Level
			}
			if source.Raw != "" {
				extra["thinking_level_raw"] = source.Raw
			}
			if len(extra) > 0 {
				encoded, _ := json.Marshal(extra)
				value := string(encoded)
				entry.Extra = &value
			}
			entries[index] = entry
		}
		turns := EntriesToTurns(entries)
		session := &ingest.Session{
			ThinkingLevel:        schema.ThinkingLevel(fixtureCase.StoredSeed.Level),
			ThinkingLevelRaw:     schema.ThinkingLevelRaw(fixtureCase.StoredSeed.Raw),
			ThinkingLevelHistory: thinkingLevelChanges(fixtureCase.StoredHistory),
			Turns:                turns,
		}
		detail := SessionToDetail(session)
		var caseFailures []string
		if got := (thinkingLevelPair{Level: string(detail.ThinkingLevel), Raw: string(detail.ThinkingLevelRaw)}); got != fixtureCase.ExpectedSeed {
			caseFailures = append(caseFailures, fmt.Sprintf("seed=%+v, want %+v", got, fixtureCase.ExpectedSeed))
		}
		var gotHistory []thinkingLevelPair
		for _, change := range detail.ThinkingLevelHistory {
			gotHistory = append(gotHistory, thinkingLevelPair{Level: string(change.Level), Raw: string(change.Raw)})
		}
		if fmt.Sprint(gotHistory) != fmt.Sprint(fixtureCase.ExpectedHistory) {
			caseFailures = append(caseFailures, fmt.Sprintf("history=%+v, want %+v", gotHistory, fixtureCase.ExpectedHistory))
		}
		if len(detail.Turns) != len(fixtureCase.ExpectedTurns) {
			caseFailures = append(caseFailures, fmt.Sprintf("turn count=%d, want %d", len(detail.Turns), len(fixtureCase.ExpectedTurns)))
		} else {
			for index, want := range fixtureCase.ExpectedTurns {
				got := thinkingLevelPair{Level: string(detail.Turns[index].ThinkingLevel), Raw: string(detail.Turns[index].ThinkingLevelRaw)}
				if got != want {
					caseFailures = append(caseFailures, fmt.Sprintf("turn %d thinking level=%+v, want %+v", index, got, want))
				}
			}
		}
		failures[fixtureCase.Name] = caseFailures
	}
	return failures
}

func TestThinkingSeedProjectionFixtureGuards(t *testing.T) {
	t.Parallel()
	loadThinkingSeedFixture(t)
	manifest, err := testutil.DecodeRequiredNamesManifest(thinkingSeedProjectionManifestYAML, "thinking seed projection")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(thinkingSeedProjectionFixtureYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		var fixture thinkingSeedFixture
		if err := yaml.Unmarshal(mutated, &fixture); err != nil {
			t.Fatalf("decode renamed fixture: %v", err)
		}
		names := make([]string, len(fixture.Cases))
		for index, fixtureCase := range fixture.Cases {
			names[index] = fixtureCase.Name
		}
		if err := testutil.ValidateRequiredNames(manifest, names, "thinking seed projection"); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}
