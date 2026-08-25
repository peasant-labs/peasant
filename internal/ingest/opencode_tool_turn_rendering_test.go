package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/transcript"
)

//go:embed testdata/opencode_tool_turn_rendering.yaml
var openCodeToolTurnRenderingData []byte

// openCodeToolCallExpectation is one folded tool call as a reader sees it.
type openCodeToolCallExpectation struct {
	Name   string `yaml:"name"`
	Input  string `yaml:"input"`
	Output string `yaml:"output"`
}

// openCodeTurnExpectation is one rendered turn of the folded message.
type openCodeTurnExpectation struct {
	Role      string                        `yaml:"role"`
	EntryType string                        `yaml:"entry_type"`
	Content   string                        `yaml:"content"`
	Tools     []openCodeToolCallExpectation `yaml:"tools"`
}

type openCodeToolTurnRenderingCase struct {
	Name                 string                    `yaml:"name"`
	Role                 string                    `yaml:"role"`
	Parts                []string                  `yaml:"parts"`
	ContentRenderedOnce  string                    `yaml:"content_rendered_once"`
	WantMessageEntryType string                    `yaml:"want_message_entry_type"`
	WantTurns            []openCodeTurnExpectation `yaml:"want_turns"`
}

type openCodeToolTurnRenderingDoc struct {
	RequiredCases []string                        `yaml:"required_cases"`
	Cases         []openCodeToolTurnRenderingCase `yaml:"cases"`
}

func loadOpenCodeToolTurnRenderingDoc(t *testing.T) openCodeToolTurnRenderingDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeToolTurnRenderingData))
	decoder.KnownFields(true)
	var doc openCodeToolTurnRenderingDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode tool-turn rendering fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("tool-turn rendering fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("tool-turn rendering fixture declares no required cases")
	}
	present := make(map[string]struct{}, len(doc.Cases))
	for _, testCase := range doc.Cases {
		if testCase.Name == "" || testCase.Role == "" || testCase.WantMessageEntryType == "" || len(testCase.Parts) == 0 || len(testCase.WantTurns) == 0 {
			t.Fatalf("tool-turn rendering fixture has an incomplete case: %+v", testCase)
		}
		for _, part := range testCase.Parts {
			if !json.Valid([]byte(part)) {
				t.Fatalf("tool-turn rendering case %q holds a part that is not valid JSON: %s", testCase.Name, part)
			}
		}
		if _, duplicate := present[testCase.Name]; duplicate {
			t.Fatalf("tool-turn rendering fixture has a duplicate case name %q", testCase.Name)
		}
		present[testCase.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := present[name]; !ok {
			t.Fatalf("tool-turn rendering fixture is missing required case %q", name)
		}
	}
	return doc
}

// managedProjectionWithParts builds the managed legacy projection bytes for one
// message carrying the given stored part rows. It is the artifact the
// materializer writes, so the assertion runs the production indexing and fold
// path rather than a helper's shortcut.
func managedProjectionWithParts(t *testing.T, sessionID, role string, parts []string) []byte {
	t.Helper()
	messageData, err := json.Marshal(map[string]any{"id": "msg_render", "role": role, "time": map[string]any{"created": 1}})
	if err != nil {
		t.Fatalf("encode synthetic message: %v", err)
	}
	rows := make([]any, 0, len(parts))
	for index, part := range parts {
		var identity struct {
			ID string `json:"id"`
		}
		if unmarshalErr := json.Unmarshal([]byte(part), &identity); unmarshalErr != nil {
			t.Fatalf("read the identity of synthetic part %d: %v", index, unmarshalErr)
		}
		rows = append(rows, map[string]any{
			"id": identity.ID, "message_id": "msg_render", "session_id": sessionID,
			"time_created": int64(index + 1), "time_updated": int64(index + 1),
			"data": json.RawMessage(part),
		})
	}
	projection, err := json.Marshal(map[string]any{
		"format":     "peasant.opencode.legacy-sqlite",
		"version":    2,
		"session_id": sessionID,
		"messages": []any{map[string]any{
			"id": "msg_render", "session_id": sessionID, "time_created": 1, "time_updated": 1,
			"data":  json.RawMessage(messageData),
			"parts": rows,
		}},
	})
	if err != nil {
		t.Fatalf("encode synthetic projection: %v", err)
	}
	return projection
}

// TestOpenCodeToolTurnRendering proves that one OpenCode message folds to the
// turns a reader sees: a tool turn names its tool and carries the tool's own
// output, and a message's prose renders exactly once.
//
// Mutation proof, one guard per defect. Dropping the "tool" fallback in
// openCodeSemanticToolName makes tool-name-comes-from-the-tool-field fail with
// an empty name. Dropping State.Output from the output precedence makes
// tool-output-comes-from-state-output and output-wins-over-result-error-and-content
// fail with an empty or aliased output. Restoring the RoleUser condition on the
// duplicate text-part drop makes a-trailing-assistant-text-part-renders-once
// fail with the report on two turns. Counting "reasoning" as a tool part in
// inspectOpenCodeSemanticParts makes a-reasoning-part-does-not-make-a-tool-turn
// fail with a tool_use turn.
func TestOpenCodeToolTurnRendering(t *testing.T) {
	t.Parallel()
	doc := loadOpenCodeToolTurnRenderingDoc(t)
	for _, testCase := range doc.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			sessionID, err := ingest.NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
			if err != nil {
				t.Fatalf("build session identifier: %v", err)
			}
			session := ingest.DiscoveredSession{
				SessionID:        sessionID,
				Harness:          ingest.HarnessOpenCode,
				SourcePath:       ingest.ResolvedPath("/synthetic/opencode.db"),
				SourceFormat:     ingest.SourceFormatJSON,
				TranscriptOrigin: ingest.TranscriptOriginOpenCodeLegacySQLite,
			}
			indexer, ok := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{FullContent: true})[ingest.HarnessOpenCode]
			if !ok {
				t.Fatal("the registry holds no OpenCode indexer")
			}
			data := managedProjectionWithParts(t, string(sessionID), testCase.Role, testCase.Parts)
			entries, err := indexer.IndexTranscriptBytes(t.Context(), session, data)
			if err != nil {
				t.Fatalf("index the synthetic managed projection: %v", err)
			}
			if len(entries) == 0 {
				t.Fatal("the indexed projection carried no entries")
			}
			if string(entries[0].EntryType) != testCase.WantMessageEntryType {
				t.Errorf("message entry type = %q, want %q", entries[0].EntryType, testCase.WantMessageEntryType)
			}
			turns := transcript.EntriesToTurns(entries)
			if len(turns) != len(testCase.WantTurns) {
				for index, turn := range turns {
					t.Logf("turn[%d] role=%s type=%s tools=%d content=%q", index, turn.Role, turn.EntryType, len(turn.ToolCalls), turn.Content)
				}
				t.Fatalf("turn count = %d, want %d", len(turns), len(testCase.WantTurns))
			}
			for index, want := range testCase.WantTurns {
				got := turns[index]
				if string(got.Role) != want.Role {
					t.Errorf("turn[%d] role = %q, want %q", index, got.Role, want.Role)
				}
				if string(got.EntryType) != want.EntryType {
					t.Errorf("turn[%d] entry type = %q, want %q", index, got.EntryType, want.EntryType)
				}
				if got.Content != want.Content {
					t.Errorf("turn[%d] content = %q, want %q", index, got.Content, want.Content)
				}
				if len(got.ToolCalls) != len(want.Tools) {
					t.Fatalf("turn[%d] tool call count = %d, want %d", index, len(got.ToolCalls), len(want.Tools))
				}
				for callIndex, wantCall := range want.Tools {
					gotCall := got.ToolCalls[callIndex]
					if gotCall.Name != wantCall.Name {
						t.Errorf("turn[%d] tool[%d] name = %q, want %q", index, callIndex, gotCall.Name, wantCall.Name)
					}
					if gotCall.Arguments != wantCall.Input {
						t.Errorf("turn[%d] tool[%d] input = %q, want %q", index, callIndex, gotCall.Arguments, wantCall.Input)
					}
					if gotCall.Result != wantCall.Output {
						t.Errorf("turn[%d] tool[%d] output = %q, want %q", index, callIndex, gotCall.Result, wantCall.Output)
					}
				}
			}
			if testCase.ContentRenderedOnce != "" {
				rendered := 0
				for _, turn := range turns {
					if strings.Contains(turn.Content, testCase.ContentRenderedOnce) {
						rendered++
					}
				}
				if rendered != 1 {
					t.Errorf("content %q renders on %d turns, want exactly 1", testCase.ContentRenderedOnce, rendered)
				}
			}
		})
	}
}
