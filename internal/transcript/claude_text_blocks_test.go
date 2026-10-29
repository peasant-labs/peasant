package transcript_test

import (
	_ "embed"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/claude_text_blocks.yaml
var claudeTextBlocksYAML []byte

func TestClaudeTextBlocksProjection(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name      string   `yaml:"name"`
			Source    string   `yaml:"source"`
			Content   []string `yaml:"content"`
			Depths    []int    `yaml:"depths"`
			ToolCalls int      `yaml:"toolCalls"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(claudeTextBlocksYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] || len(c.Content) != len(c.Depths) {
			t.Fatalf("invalid Claude text fixture %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			const path = "/fixture/transcript.jsonl"
			if err := fs.WriteFile(path, []byte(c.Source), 0644); err != nil {
				t.Fatal(err)
			}
			sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true), ingest.WithClaudeFullContent(true))
			entries, err := indexer.IndexTranscript(t.Context(), ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessClaudeCode, SourcePath: path, SourceFormat: ingest.SourceFormatJSONL})
			if err != nil {
				t.Fatal(err)
			}
			// Managed captures install a durable ref on every decomposed entry.
			// Preserve that property at the fold boundary: reference-free legacy
			// rows would let consecutive dedup hide this projection regression.
			for i := range entries {
				entries[i].SourceEntryRef = schema.SourceEntryRef(fmt.Sprintf("claude-fixture:%d", entries[i].EntryIndex))
			}
			turns := transcript.EntriesToTurns(entries)
			if len(turns) != len(c.Content) {
				t.Fatalf("turn count=%d, want %d", len(turns), len(c.Content))
			}
			toolCalls := 0
			for i, turn := range turns {
				if turn.Content != c.Content[i] || turn.Depth != c.Depths[i] {
					t.Errorf("turn %d = (%q, depth %d), want (%q, depth %d)", i, turn.Content, turn.Depth, c.Content[i], c.Depths[i])
				}
				if turn.SourceEntryRef == "" {
					t.Error("projection lost durable source reference")
				}
				toolCalls += len(turn.ToolCalls)
			}
			if toolCalls != c.ToolCalls {
				t.Errorf("tool calls=%d, want %d", toolCalls, c.ToolCalls)
			}
		})
	}
	if err := testutil.RequireFixtureNames("Claude text blocks", "case", fixture.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
}
