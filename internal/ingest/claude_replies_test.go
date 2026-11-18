package ingest_test

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/claude_replies.yaml
var claudeRepliesYAML []byte

//go:embed testdata/claude_replies.manifest.yaml
var claudeRepliesManifestYAML []byte

type claudeReplyEntry struct {
	Index      int    `yaml:"index"`
	Depth      int    `yaml:"depth"`
	Parent     int    `yaml:"parent"`
	Role       string `yaml:"role"`
	EntryType  string `yaml:"entryType"`
	Content    string `yaml:"content"`
	ToolNames  string `yaml:"toolNames"`
	TokensOut  int    `yaml:"tokensOut"`
	StopReason string `yaml:"stopReason"`
}

type claudeReplyCase struct {
	Name     string   `yaml:"name"`
	JSONL    []string `yaml:"jsonl"`
	RawBytes []struct {
		Entry int `yaml:"entry"`
		Line  int `yaml:"line"`
	} `yaml:"rawBytes"`
	Entries []claudeReplyEntry `yaml:"entries"`
}

// TestClaudeRepliesGroupByMessage runs the stored indexer over records that
// share an API message id and asserts every stored entry as literals.
func TestClaudeRepliesGroupByMessage(t *testing.T) {
	t.Parallel()
	var document struct {
		Cases []claudeReplyCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(claudeRepliesYAML, &document); err != nil {
		t.Fatalf("decode claude replies fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(claudeRepliesManifestYAML, "claude replies")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "claude replies"); err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			data := []byte(strings.Join(row.JSONL, "\n") + "\n")
			idx := ingest.NewClaudeIndexer(testutil.NewMemFS(), ingest.WithClaudeFullDepth(true), ingest.WithClaudeFullContent(true))
			entries, err := idx.IndexTranscriptBytes(t.Context(), ingest.DiscoveredSession{SessionID: ingest.SessionID(testutil.TestSessionUUID), Harness: ingest.HarnessClaudeCode}, data)
			if err != nil {
				t.Fatalf("IndexTranscriptBytes: %v", err)
			}
			if len(entries) != len(row.Entries) {
				for _, e := range entries {
					t.Logf("got entry %d depth %d role %s type %s", e.EntryIndex, e.Depth, e.Role, e.EntryType)
				}
				t.Fatalf("stored %d entries, want %d", len(entries), len(row.Entries))
			}
			for i, want := range row.Entries {
				got := entries[i]
				parent := -1
				if got.ParentIndex != nil {
					parent = *got.ParentIndex
				}
				content, toolNames, stopReason, tokensOut := "", "", "", 0
				if got.ContentPreview != nil {
					content = *got.ContentPreview
				}
				if got.ToolNamesCSV != nil {
					toolNames = *got.ToolNamesCSV
				}
				if got.StopReason != nil {
					stopReason = string(*got.StopReason)
				}
				if got.TokensOut != nil {
					tokensOut = *got.TokensOut
				}
				actual := claudeReplyEntry{Index: got.EntryIndex, Depth: got.Depth, Parent: parent, Role: string(got.Role), EntryType: string(got.EntryType), Content: content, ToolNames: toolNames, TokensOut: tokensOut, StopReason: stopReason}
				if actual != want {
					t.Errorf("entry[%d] = %+v, want %+v", i, actual, want)
				}
			}
			for _, sized := range row.RawBytes {
				var reply *int
				for i := range entries {
					if entries[i].EntryIndex == sized.Entry {
						reply = entries[i].RawByteLength
					}
				}
				if want := claudeReplySourceBytes(row.JSONL, sized.Line); reply == nil || *reply != want {
					t.Errorf("entry %d raw byte length = %v, want the summed record size %d", sized.Entry, reply, want)
				}
			}
		})
	}
}

// claudeReplySourceBytes sums the trimmed line sizes of the records that share
// the message id of the record on line first.
func claudeReplySourceBytes(lines []string, line int) int {
	first := lines[line]
	start := strings.Index(first, `"id":"msg_`)
	if start < 0 {
		return len(bytes.TrimSpace([]byte(first)))
	}
	end := strings.Index(first[start+6:], `"`) + start + 6
	marker := first[start : end+1]
	total := 0
	for _, line := range lines {
		if strings.Contains(line, marker) {
			total += len(bytes.TrimSpace([]byte(line)))
		}
	}
	return total
}
