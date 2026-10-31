package transcript_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"reflect"
	"strings"
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
			Name           string   `yaml:"name"`
			Source         string   `yaml:"source"`
			Content        []string `yaml:"content"`
			Depths         []int    `yaml:"depths"`
			ToolCalls      int      `yaml:"toolCalls"`
			FirstModel     string   `yaml:"firstModel"`
			FirstTimestamp string   `yaml:"firstTimestamp"`
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
			path := "/fixture/" + testutil.TestSessionUUID + ".jsonl"
			sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true), ingest.WithClaudeFullContent(true))
			var captured bytes.Buffer
			for _, line := range strings.Split(strings.TrimSpace(c.Source), "\n") {
				var record map[string]json.RawMessage
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				record["sessionId"], _ = json.Marshal(string(sid))
				encoded, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				captured.Write(encoded)
				captured.WriteByte('\n')
			}
			metadata := ingest.NewUnifiedMetadata()
			metadata.SessionID = sid
			metadata.ModelHarness = ingest.HarnessClaudeCode
			metadata.HostSlug = testutil.TestHostSlug
			metadata.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "myapp", FilePath: "/home/test/myapp"}
			metadata.CWD = "/home/test/myapp"
			metadata.Source = ingest.SourceInfo{FilePath: path, Format: ingest.SourceFormatJSONL}
			metadata.ContentHash = schema.ComputeTranscriptHash(captured.Bytes())
			candidate, err := indexer.BuildGeneration(t.Context(), ingest.GenerationBuildInput{
				Session:      ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessClaudeCode, SourcePath: ingest.ResolvedPath(path), SourceFormat: ingest.SourceFormatJSONL},
				GenerationID: "gen-claude-text", RecordedMetadata: &metadata, CapturedTranscript: captured.Bytes(),
			})
			if err != nil {
				t.Fatal(err)
			}
			entries := candidate.Result.Generation.Main.Entries
			before, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			turns := transcript.EntriesToTurns(entries)
			if len(turns) != len(c.Content) {
				t.Fatalf("turn count=%d, want %d", len(turns), len(c.Content))
			}
			after, err := json.Marshal(entries)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("folding mutated stored entries")
			}
			if !reflect.DeepEqual(turns, transcript.EntriesToTurns(entries)) {
				t.Fatal("folding is not repeatable")
			}
			if c.FirstModel != "" && string(turns[0].ObservedModel) != c.FirstModel {
				t.Errorf("model = %q, want %q", turns[0].ObservedModel, c.FirstModel)
			}
			if c.FirstTimestamp != "" && turns[0].Timestamp.UTC().Format("2006-01-02T15:04:05Z07:00") != c.FirstTimestamp {
				t.Errorf("timestamp = %v, want %s", turns[0].Timestamp, c.FirstTimestamp)
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
