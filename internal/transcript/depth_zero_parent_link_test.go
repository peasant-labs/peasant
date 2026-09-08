package transcript

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/depth_zero_parent_link.yaml
var depthZeroParentLinkFixtureYAML []byte

//go:embed testdata/depth_zero_parent_link.manifest.yaml
var depthZeroParentLinkManifestYAML []byte

type depthZeroParentLinkEntry struct {
	Index         int    `yaml:"index"`
	Role          string `yaml:"role"`
	Depth         int    `yaml:"depth"`
	ParentIndex   *int   `yaml:"parentIndex,omitempty"`
	ParentEntryID string `yaml:"parentEntryId,omitempty"`
	EntryType     string `yaml:"entryType,omitempty"`
	ToolCallID    string `yaml:"toolCallId,omitempty"`
	Content       string `yaml:"content,omitempty"`
	ToolOutput    string `yaml:"toolOutput,omitempty"`
	IsError       bool   `yaml:"isError,omitempty"`
}

type depthZeroParentLinkTurn struct {
	Index     int    `yaml:"index"`
	Role      string `yaml:"role"`
	ToolCalls int    `yaml:"toolCalls"`
}

type depthZeroParentLinkCase struct {
	Name                   string                     `yaml:"name"`
	Entries                []depthZeroParentLinkEntry `yaml:"entries"`
	ExpectedTurns          []depthZeroParentLinkTurn  `yaml:"expectedTurns,omitempty"`
	ExpectedOverlay        map[int]string             `yaml:"expectedOverlay,omitempty"`
	ExpectedMissingOverlay []int                      `yaml:"expectedMissingOverlay,omitempty"`
	ExpectedToolOutput     map[string]string          `yaml:"expectedToolOutput,omitempty"`
	ExpectedToolErrors     map[string]bool            `yaml:"expectedToolErrors,omitempty"`
}

type depthZeroParentLinkFixture struct {
	Cases []depthZeroParentLinkCase `yaml:"cases"`
}

func loadDepthZeroParentLinkFixture(t *testing.T) depthZeroParentLinkFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(depthZeroParentLinkFixtureYAML))
	decoder.KnownFields(true)
	var fixture depthZeroParentLinkFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode depth-zero parent link fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("depth-zero parent link fixture must contain exactly one YAML document: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(depthZeroParentLinkManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || names[fixtureCase.Name] || len(fixtureCase.Entries) == 0 {
			t.Fatalf("depth-zero parent link fixture case %q is incomplete", fixtureCase.Name)
		}
		names[fixtureCase.Name] = true
		if len(fixtureCase.ExpectedTurns) == 0 && len(fixtureCase.ExpectedOverlay) == 0 {
			t.Fatalf("depth-zero parent link fixture case %q asserts nothing", fixtureCase.Name)
		}
		for _, entry := range fixtureCase.Entries {
			if !schema.Role(entry.Role).IsValid() {
				t.Fatalf("depth-zero parent link fixture case %q has invalid role %q", fixtureCase.Name, entry.Role)
			}
		}
	}
	if err := testutil.RequireFixtureNames("depth-zero parent link", "case", manifest.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func depthZeroParentLinkEntries(fixtureCase depthZeroParentLinkCase) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, 0, len(fixtureCase.Entries))
	for _, source := range fixtureCase.Entries {
		entry := schema.SessionEntry{
			EntryIndex:  source.Index,
			Role:        schema.Role(source.Role),
			Depth:       source.Depth,
			ParentIndex: source.ParentIndex,
			EntryType:   schema.EntryType(source.EntryType),
			IsError:     source.IsError,
		}
		if source.ParentEntryID != "" {
			parentEntryID := source.ParentEntryID
			entry.ParentEntryID = &parentEntryID
		}
		if entry.EntryType == "" {
			entry.EntryType = schema.EntryTypeText
		}
		if source.Content != "" {
			content := source.Content
			entry.ContentPreview = &content
		}
		if source.ToolCallID != "" {
			id := source.ToolCallID
			entry.ToolCallID = &id
			entry.HasToolUse = entry.EntryType == schema.EntryTypeToolUse
		}
		if source.ToolOutput != "" {
			output := source.ToolOutput
			entry.ToolOutput = &output
		}
		entries = append(entries, entry)
	}
	return entries
}

// TestDepthZeroParentLinkIsNotAChild proves the parent-link contract: a depth-0
// entry that carries a ParentEntryID (a harness message graph) with a nil
// ParentIndex still folds as its own top-level turn, its depth-1 tool entries
// still fold into it, and the content overlay never adopts it as a child of the
// entry it points at.
func TestDepthZeroParentLinkIsNotAChild(t *testing.T) {
	fixture := loadDepthZeroParentLinkFixture(t)
	for _, fixtureCase := range fixture.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			entries := depthZeroParentLinkEntries(fixtureCase)
			if len(fixtureCase.ExpectedTurns) > 0 {
				turns := EntriesToTurns(entries)
				if len(turns) != len(fixtureCase.ExpectedTurns) {
					t.Fatalf("turn count = %d, want %d", len(turns), len(fixtureCase.ExpectedTurns))
				}
				for i, want := range fixtureCase.ExpectedTurns {
					if turns[i].Index != want.Index || string(turns[i].Role) != want.Role || len(turns[i].ToolCalls) != want.ToolCalls {
						t.Errorf("turn %d = (index %d, role %s, tool calls %d), want (index %d, role %s, tool calls %d)",
							i, turns[i].Index, turns[i].Role, len(turns[i].ToolCalls), want.Index, want.Role, want.ToolCalls)
					}
				}
				for id, want := range fixtureCase.ExpectedToolOutput {
					found := false
					for _, turn := range turns {
						for _, call := range turn.ToolCalls {
							if call.ID == id {
								found = true
								if call.Result != want || call.IsError != fixtureCase.ExpectedToolErrors[id] || call.DurationMs != nil {
									t.Errorf("tool %s lost untimed output/error: %+v", id, call)
								}
							}
						}
					}
					if !found {
						t.Errorf("missing tool %s", id)
					}
				}
			}
			if len(fixtureCase.ExpectedOverlay) > 0 || len(fixtureCase.ExpectedMissingOverlay) > 0 {
				overlay := contentOverlayFromEntries(entries)
				for index, want := range fixtureCase.ExpectedOverlay {
					if got, ok := overlay[index]; !ok || got != want {
						t.Errorf("overlay[%d] = %q (present=%v), want %q", index, got, ok, want)
					}
				}
				for _, index := range fixtureCase.ExpectedMissingOverlay {
					if got, ok := overlay[index]; ok {
						t.Errorf("overlay[%d] = %q, want no overlay: a linked depth-0 entry is not a child", index, got)
					}
				}
			}
		})
	}
}
