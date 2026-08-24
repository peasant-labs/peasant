package ingest

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type openCodeGraphCycleFixture struct {
	RequiredCases []string                 `yaml:"required_cases"`
	RepeatRuns    int                      `yaml:"repeat_runs"`
	Cases         []openCodeGraphCycleCase `yaml:"cases"`
}

type openCodeGraphCycleCase struct {
	Name                       string                      `yaml:"name"`
	Messages                   []openCodeGraphCycleMessage `yaml:"messages"`
	ExpectedLinks              []openCodeGraphCycleLink    `yaml:"expected_links"`
	ExpectedCycleDiagnostics   []string                    `yaml:"expected_cycle_diagnostics"`
	ExpectedMissingDiagnostics []string                    `yaml:"expected_missing_diagnostics"`
}

type openCodeGraphCycleMessage struct {
	ID       string `yaml:"id"`
	ParentID string `yaml:"parent_id"`
	Role     string `yaml:"role"`
}

type openCodeGraphCycleLink struct {
	ID     string `yaml:"id"`
	Parent string `yaml:"parent"`
}

//go:embed testdata/opencode_graph_cycles.yaml
var openCodeGraphCycleYAML []byte

func loadOpenCodeGraphCycleFixture(data []byte) (openCodeGraphCycleFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture openCodeGraphCycleFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode OpenCode graph cycle fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, errors.New("OpenCode graph cycle fixture must contain exactly one YAML document")
	}
	if len(fixture.RequiredCases) == 0 || fixture.RepeatRuns < 2 {
		return fixture, errors.New("OpenCode graph cycle fixture guard failed")
	}
	presentCycle := make(map[string]struct{}, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		presentCycle[testCase.Name] = struct{}{}
	}
	for _, name := range fixture.RequiredCases {
		if _, ok := presentCycle[name]; !ok {
			return fixture, fmt.Errorf("OpenCode graph cycle fixture is missing required case %q", name)
		}
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || seen[testCase.Name] || len(testCase.Messages) == 0 || len(testCase.ExpectedLinks) != len(testCase.Messages) {
			return fixture, fmt.Errorf("OpenCode graph cycle fixture has an incomplete or duplicate case %+v", testCase)
		}
		seen[testCase.Name] = true
		ids := make(map[string]bool, len(testCase.Messages))
		for _, message := range testCase.Messages {
			if message.ID == "" || ids[message.ID] || !Role(message.Role).IsValid() {
				return fixture, fmt.Errorf("OpenCode graph cycle case %q has an invalid message %+v", testCase.Name, message)
			}
			ids[message.ID] = true
		}
		for _, link := range testCase.ExpectedLinks {
			if !ids[link.ID] || (link.Parent != "" && !ids[link.Parent]) {
				return fixture, fmt.Errorf("OpenCode graph cycle case %q has an expected link to an unknown message %+v", testCase.Name, link)
			}
		}
		for _, id := range testCase.ExpectedCycleDiagnostics {
			if !ids[id] {
				return fixture, fmt.Errorf("OpenCode graph cycle case %q expects a cycle diagnostic for unknown message %q", testCase.Name, id)
			}
		}
		for _, id := range testCase.ExpectedMissingDiagnostics {
			if !ids[id] {
				return fixture, fmt.Errorf("OpenCode graph cycle case %q expects a missing-parent diagnostic for unknown message %q", testCase.Name, id)
			}
		}
	}
	return fixture, nil
}

func openCodeGraphCycleMessages(t testing.TB, fixtures []openCodeGraphCycleMessage) []openCodeSemanticMessage {
	t.Helper()
	messages := make([]openCodeSemanticMessage, 0, len(fixtures))
	for index, fixture := range fixtures {
		raw := fmt.Sprintf(`{"id":%q,"sessionID":"ses_3cd91f52effeXd3QAJ54jOyzv5","role":%q,"parentID":%q,"time":{"created":%d}}`, fixture.ID, fixture.Role, fixture.ParentID, 1000+index)
		message, err := parseOpenCodeSemanticMessage(fixture.ID, 0, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		part, err := parseOpenCodeSemanticPart("part_"+fixture.ID, 0, []byte(fmt.Sprintf(`{"id":%q,"type":"text","text":"text for %s"}`, "part_"+fixture.ID, fixture.ID)))
		if err != nil {
			t.Fatal(err)
		}
		message.Parts = append(message.Parts, part)
		messages = append(messages, message)
	}
	return messages
}

// TestOpenCodeIndexerGraphLinksAreDeterministicAndKeepDepthContract proves
// that parent cycles resolve the same way on every run and that messages keep
// Depth 0 while parts keep Depth 1 regardless of the parent chain length.
func TestOpenCodeIndexerGraphLinksAreDeterministicAndKeepDepthContract(t *testing.T) {
	fixture, err := loadOpenCodeGraphCycleFixture(openCodeGraphCycleYAML)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	if err != nil {
		t.Fatal(err)
	}
	indexer := NewOpenCodeIndexer(&OSFileSystem{}, WithOpenCodeFullDepth(true))
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			messages := openCodeGraphCycleMessages(t, testCase.Messages)
			first := indexer.indexSemanticMessages(sessionID, messages)
			for run := 1; run < fixture.RepeatRuns; run++ {
				again := indexer.indexSemanticMessages(sessionID, messages)
				if !reflect.DeepEqual(first, again) {
					t.Fatalf("run %d produced a different graph:\nfirst=%s\nagain=%s", run, describeOpenCodeGraph(first), describeOpenCodeGraph(again))
				}
			}
			indexes := make(map[string]int, len(first))
			for index, entry := range first {
				if entry.EntryID != nil {
					indexes[*entry.EntryID] = index
				}
				wantDepth := 0
				if entry.PartType != nil {
					wantDepth = 1
				}
				if entry.Depth != wantDepth {
					t.Fatalf("entry %+v has Depth %d, want %d: messages stay at 0 and parts at 1", entry, entry.Depth, wantDepth)
				}
				if entry.Depth == 0 && entry.ParentIndex != nil {
					t.Fatalf("depth-0 message %+v carries ParentIndex %d; the contract keeps depth-0 ParentIndex nil", entry, *entry.ParentIndex)
				}
			}
			for _, link := range testCase.ExpectedLinks {
				entry := first[indexes[link.ID]]
				if link.Parent == "" {
					if entry.ParentEntryID != nil {
						t.Fatalf("message %q should stay at the root, got parent link %q", link.ID, *entry.ParentEntryID)
					}
					continue
				}
				if entry.ParentEntryID == nil || *entry.ParentEntryID != link.Parent {
					t.Fatalf("message %q parent link=%v, want %q", link.ID, entry.ParentEntryID, link.Parent)
				}
			}
		})
	}
}

func describeOpenCodeGraph(entries []schema.SessionEntry) string {
	var buffer bytes.Buffer
	for _, entry := range entries {
		id := ""
		if entry.EntryID != nil {
			id = *entry.EntryID
		}
		parent := "root"
		if entry.ParentEntryID != nil {
			parent = *entry.ParentEntryID
		}
		fmt.Fprintf(&buffer, "%d:%s depth=%d parent=%s; ", entry.EntryIndex, id, entry.Depth, parent)
	}
	return buffer.String()
}

// TestOpenCodeCycleDiagnosticNamesOnlyTheDroppedLink proves that a parent cycle
// yields exactly one cycle diagnostic, for the single member whose link
// normalization dropped, not one diagnostic per node on the cycle. It also
// checks that an absent parent still yields its own missing-parent diagnostic.
func TestOpenCodeCycleDiagnosticNamesOnlyTheDroppedLink(t *testing.T) {
	fixture, err := loadOpenCodeGraphCycleFixture(openCodeGraphCycleYAML)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	if err != nil {
		t.Fatal(err)
	}
	sourcePath, err := NewResolvedPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := DiscoveredSession{SessionID: sessionID, Harness: HarnessOpenCode, SourcePath: sourcePath}

	const (
		cycleMarker   = "linking it would close a parent cycle"
		missingMarker = "absent from the selected canonical representation"
	)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			messages := openCodeGraphCycleMessages(t, testCase.Messages)
			diagnostics := missingOpenCodeParentDiagnostics(session, messages)

			cycleIDs := make(map[string]int)
			missingIDs := make(map[string]int)
			for _, diagnostic := range diagnostics {
				switch {
				case strings.Contains(diagnostic.Message, cycleMarker):
					cycleIDs[messageIDFromCycleDiagnostic(t, testCase.Messages, diagnostic.Message)]++
				case strings.Contains(diagnostic.Message, missingMarker):
					missingIDs[messageIDFromCycleDiagnostic(t, testCase.Messages, diagnostic.Message)]++
				default:
					t.Fatalf("diagnostic wording is neither cycle nor missing-parent: %q", diagnostic.Message)
				}
			}

			wantCycle := countedIDSet(testCase.ExpectedCycleDiagnostics)
			wantMissing := countedIDSet(testCase.ExpectedMissingDiagnostics)
			if !reflect.DeepEqual(cycleIDs, wantCycle) {
				t.Fatalf("cycle diagnostics = %v, want exactly %v", cycleIDs, wantCycle)
			}
			if !reflect.DeepEqual(missingIDs, wantMissing) {
				t.Fatalf("missing-parent diagnostics = %v, want exactly %v", missingIDs, wantMissing)
			}
			if got, want := len(diagnostics), len(testCase.ExpectedCycleDiagnostics)+len(testCase.ExpectedMissingDiagnostics); got != want {
				t.Fatalf("total diagnostics = %d, want %d for case %q", got, want, testCase.Name)
			}
		})
	}
}

// messageIDFromCycleDiagnostic recovers which fixture message a diagnostic names
// by finding the one message id its wording mentions. The message id is unique
// within a case, so exactly one matches.
func messageIDFromCycleDiagnostic(t testing.TB, messages []openCodeGraphCycleMessage, text string) string {
	t.Helper()
	match := ""
	for _, message := range messages {
		if strings.Contains(text, "message "+message.ID+" references") {
			if match != "" {
				t.Fatalf("diagnostic %q names more than one message", text)
			}
			match = message.ID
		}
	}
	if match == "" {
		t.Fatalf("diagnostic %q names no known message", text)
	}
	return match
}

func countedIDSet(ids []string) map[string]int {
	set := make(map[string]int)
	for _, id := range ids {
		set[id]++
	}
	return set
}
