package ingest_test

import (
	_ "embed"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
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
	UUID       string `yaml:"uuid"`
	Timestamp  string `yaml:"timestamp"`
	Role       string `yaml:"role"`
	EntryType  string `yaml:"entryType"`
	Content    string `yaml:"content"`
	ContentAny bool   `yaml:"contentAny"`
	ToolNames  string `yaml:"toolNames"`
	TokensIn   int    `yaml:"tokensIn"`
	TokensOut  int    `yaml:"tokensOut"`
	StopReason string `yaml:"stopReason"`
	RawBytes   int    `yaml:"rawBytes"`
}

type claudeReplyCase struct {
	Name          string   `yaml:"name"`
	FullDepthOff  bool     `yaml:"fullDepthOff"`
	JSONL         []string `yaml:"jsonl"`
	SessionTokens *struct {
		TokensIn  int `yaml:"tokensIn"`
		TokensOut int `yaml:"tokensOut"`
	} `yaml:"sessionTokens"`
	Keys    []string           `yaml:"keys"`
	Entries []claudeReplyEntry `yaml:"entries"`
}

// TestClaudeRepliesGroupByMessage runs the stored indexer, the managed
// producer and the metadata extraction over records that share an API message
// id, and asserts every stored entry, every native key and the session token
// totals as literals.
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
			idx := ingest.NewClaudeIndexer(testutil.NewMemFS(), ingest.WithClaudeFullDepth(!row.FullDepthOff), ingest.WithClaudeFullContent(true))
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
			replyIn, replyOut := 0, 0
			for i, want := range row.Entries {
				got := entries[i]
				actual := claudeReplyEntry{Index: got.EntryIndex, Depth: got.Depth, Parent: -1, Role: string(got.Role), EntryType: string(got.EntryType), ContentAny: want.ContentAny}
				if got.ParentIndex != nil {
					actual.Parent = *got.ParentIndex
				}
				if got.EntryID != nil {
					actual.UUID = *got.EntryID
				}
				if got.TimestampMs != nil {
					actual.Timestamp = time.UnixMilli(*got.TimestampMs).UTC().Format(time.RFC3339)
				}
				if got.ContentPreview != nil && !want.ContentAny {
					actual.Content = *got.ContentPreview
				}
				if got.ToolNamesCSV != nil {
					actual.ToolNames = *got.ToolNamesCSV
				}
				if got.TokensIn != nil {
					actual.TokensIn = *got.TokensIn
				}
				if got.TokensOut != nil {
					actual.TokensOut = *got.TokensOut
				}
				if got.StopReason != nil {
					actual.StopReason = string(*got.StopReason)
				}
				if got.Depth == 0 && got.RawByteLength != nil {
					actual.RawBytes = *got.RawByteLength
				}
				if actual != want {
					t.Errorf("entry[%d] = %+v, want %+v", i, actual, want)
				}
				replyIn += actual.TokensIn
				replyOut += actual.TokensOut
			}
			if row.Keys != nil {
				if got := claudeReplyNativeKeys(t, row.JSONL); !reflect.DeepEqual(got, row.Keys) {
					t.Errorf("managed native keys = %v, want %v", got, row.Keys)
				}
			}
			if row.SessionTokens != nil {
				gotIn, gotOut := claudeReplySessionTokens(t, data)
				if gotIn != row.SessionTokens.TokensIn || gotOut != row.SessionTokens.TokensOut {
					t.Errorf("session tokens = (%d, %d), want (%d, %d)", gotIn, gotOut, row.SessionTokens.TokensIn, row.SessionTokens.TokensOut)
				}
				if gotIn != replyIn || gotOut != replyOut {
					t.Errorf("session tokens (%d, %d) differ from the sum of the reply entries (%d, %d)", gotIn, gotOut, replyIn, replyOut)
				}
			}
		})
	}
}

// claudeReplyNativeKeys builds the managed generation over the case's records
// and returns every main entry's native key, in entry order.
func claudeReplyNativeKeys(t *testing.T, lines []string) []string {
	t.Helper()
	var captured strings.Builder
	for _, line := range lines {
		var record map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("fixture record is not JSON: %v", err)
		}
		record["sessionId"], _ = json.Marshal(testutil.TestSessionUUID)
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		captured.Write(encoded)
		captured.WriteByte('\n')
	}
	candidate := buildClaudeStabilityCandidate(t, "gen-claude-replies", captured.String(), ingest.ProjectionPriorState{})
	keyByRef := make(map[string]string)
	for _, alias := range candidate.Result.Generation.Aliases {
		if key, block := strings.CutPrefix(alias.NativeKey, "block:"); block {
			keyByRef[string(alias.Ref)] = key
		}
	}
	keys := make([]string, 0, len(candidate.Result.Generation.Main.Entries))
	for _, entry := range candidate.Result.Generation.Main.Entries {
		keys = append(keys, keyByRef[string(entry.SourceEntryRef)])
	}
	return keys
}

// claudeReplySessionTokens extracts the session metadata over the case's
// records through the production adapter and returns its token totals.
func claudeReplySessionTokens(t *testing.T, data []byte) (int, int) {
	t.Helper()
	mfs := testutil.NewMemFS()
	path := setupClaudeFS(t, mfs, "/home/test/.claude/projects", "-home-test-project", testutil.TestSessionUUID, data)
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	adapter := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := adapter.ExtractMetadata(t.Context(), ingest.DiscoveredSession{
		SessionID: sid, Harness: ingest.HarnessClaudeCode, SourcePath: ingest.ResolvedPath(path), SourceFormat: ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{}, DebugPaths: []ingest.ResolvedPath{},
	})
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	return meta.Stats.TokensIn, meta.Stats.TokensOut
}
