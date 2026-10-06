package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/claude_thinking_levels.yaml
var claudeThinkingLevelsYAML []byte

//go:embed testdata/claude_thinking_levels.manifest.yaml
var claudeThinkingLevelsManifestYAML []byte

//go:embed testdata/pi_thinking_levels.yaml
var piThinkingLevelsYAML []byte

//go:embed testdata/pi_thinking_levels.manifest.yaml
var piThinkingLevelsManifestYAML []byte

//go:embed testdata/codex_thinking_levels.yaml
var codexThinkingLevelsYAML []byte

//go:embed testdata/codex_thinking_levels.manifest.yaml
var codexThinkingLevelsManifestYAML []byte

//go:embed testdata/opencode_thinking_levels.yaml
var openCodeThinkingLevelsYAML []byte

//go:embed testdata/opencode_thinking_levels_retained.manifest.yaml
var openCodeThinkingLevelsRetainedManifestYAML []byte

// expectedThinkingLevel is one expected observation; both fields empty means
// absent (unknown), which is never the same as off.
type expectedThinkingLevel struct {
	Level string `yaml:"level"`
	Raw   string `yaml:"raw"`
}

func (e *expectedThinkingLevel) pair() expectedThinkingLevel {
	if e == nil {
		return expectedThinkingLevel{}
	}
	return *e
}

// thinkingLevelTranscriptCase is one retained-transcript case shared by the
// Claude Code, Pi and Codex corpora. BuilderEffort (Codex only) drives the
// representative rollout builder instead of an inline transcript.
type thinkingLevelTranscriptCase struct {
	Name          string                  `yaml:"name"`
	Transcript    string                  `yaml:"transcript"`
	BuilderEffort string                  `yaml:"builderEffort"`
	Seed          *expectedThinkingLevel  `yaml:"seed"`
	History       []expectedThinkingLevel `yaml:"history"`
	Turns         []expectedThinkingLevel `yaml:"turns"`
	Diagnostic    string                  `yaml:"diagnostic"`
}

type thinkingLevelTranscriptCorpus struct {
	Cases []thinkingLevelTranscriptCase `yaml:"cases"`
}

func loadThinkingLevelTranscriptCorpus(t *testing.T, label string, corpusYAML, manifestYAML []byte) []thinkingLevelTranscriptCase {
	t.Helper()
	cases, err := decodeThinkingLevelTranscriptCorpus(label, corpusYAML, manifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

func decodeThinkingLevelTranscriptCorpus(label string, corpusYAML, manifestYAML []byte) ([]thinkingLevelTranscriptCase, error) {
	var corpus thinkingLevelTranscriptCorpus
	if err := testutil.DecodeFixtureYAML(corpusYAML, &corpus); err != nil {
		return nil, fmt.Errorf("decode %s corpus: %w", label, err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(manifestYAML, label)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(corpus.Cases))
	for _, c := range corpus.Cases {
		if (c.Transcript == "") == (c.BuilderEffort == "") {
			return nil, fmt.Errorf("%s case %q must set exactly one of transcript or builderEffort", label, c.Name)
		}
		if len(c.Turns) == 0 {
			return nil, fmt.Errorf("%s case %q lists no assistant turns", label, c.Name)
		}
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, label); err != nil {
		return nil, err
	}
	return corpus.Cases, nil
}

func TestThinkingLevelCorpusManifestsGuardRequiredNames(t *testing.T) {
	check := func(label string, corpus, manifestYAML []byte) {
		t.Helper()
		manifest, err := testutil.DecodeRequiredNamesManifest(manifestYAML, label)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range manifest.RequiredNames {
			mutated := bytes.Replace(corpus, []byte("name: "+required+"\n"), []byte("name: renamed_case\n"), 1)
			if _, err := decodeThinkingLevelTranscriptCorpus(label, mutated, manifestYAML); err == nil {
				t.Errorf("%s: renaming required case %q unexpectedly validated", label, required)
			}
		}
	}
	check("Claude thinking levels", claudeThinkingLevelsYAML, claudeThinkingLevelsManifestYAML)
	check("Pi thinking levels", piThinkingLevelsYAML, piThinkingLevelsManifestYAML)
	check("Codex thinking levels", codexThinkingLevelsYAML, codexThinkingLevelsManifestYAML)
	for _, required := range mustDecodeManifest(t, openCodeThinkingLevelsRetainedManifestYAML).RequiredNames {
		mutated := bytes.Replace(openCodeThinkingLevelsYAML, []byte("name: "+required+"\n"), []byte("name: renamed_case\n"), 1)
		if _, err := decodeOpenCodeRetainedThinkingLevelCorpus(mutated); err == nil {
			t.Errorf("OpenCode retained thinking levels: renaming required case %q unexpectedly validated", required)
		}
	}
}

func mustDecodeManifest(t *testing.T, data []byte) testutil.RequiredNamesManifest {
	t.Helper()
	manifest, err := testutil.DecodeRequiredNamesManifest(data, "OpenCode retained thinking levels")
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestClaudeThinkingLevelExtraction(t *testing.T) {
	for _, c := range loadThinkingLevelTranscriptCorpus(t, "Claude thinking levels", claudeThinkingLevelsYAML, claudeThinkingLevelsManifestYAML) {
		t.Run(c.Name, func(t *testing.T) {
			runThinkingLevelTranscriptCase(t, ingest.HarnessClaudeCode, []byte(c.Transcript), c, ingest.NewClaudeIndexer(nil))
		})
	}
}

func TestPiThinkingLevelExtraction(t *testing.T) {
	for _, c := range loadThinkingLevelTranscriptCorpus(t, "Pi thinking levels", piThinkingLevelsYAML, piThinkingLevelsManifestYAML) {
		t.Run(c.Name, func(t *testing.T) {
			runThinkingLevelTranscriptCase(t, ingest.HarnessPi, []byte(c.Transcript), c, ingest.NewPiIndexer(nil))
		})
	}
}

func TestCodexThinkingLevelExtraction(t *testing.T) {
	for _, c := range loadThinkingLevelTranscriptCorpus(t, "Codex thinking levels", codexThinkingLevelsYAML, codexThinkingLevelsManifestYAML) {
		t.Run(c.Name, func(t *testing.T) {
			data := []byte(c.Transcript)
			if c.BuilderEffort != "" {
				opts := defaultCodexFixtureOpts()
				opts.effort = c.BuilderEffort
				data = codexRolloutJSONL(opts)
			}
			runThinkingLevelTranscriptCase(t, ingest.HarnessCodex, data, c, ingest.NewCodexIndexer(nil))
		})
	}
}

type thinkingLevelBytesIndexer interface {
	IndexTranscriptBytes(ctx context.Context, session ingest.DiscoveredSession, data []byte) ([]schema.SessionEntry, error)
}

// runThinkingLevelTranscriptCase runs the retained metadata extraction (the
// adapter refresh path, which also proves the adapter copies the parsed seed)
// and the indexer over the same bytes, then compares the seed and the
// per-turn evidence of each depth-0 assistant text entry in order.
func runThinkingLevelTranscriptCase(t *testing.T, harness ingest.Harness, data []byte, c thinkingLevelTranscriptCase, indexer thinkingLevelBytesIndexer) {
	t.Helper()
	original := makeReindexMeta(t, testSessionID, "/missing/native/session.jsonl")
	original.ModelHarness = harness
	// A stale seed on the original must be replaced by the re-extracted one,
	// never kept and never blanked when the transcript carries evidence.
	original.ThinkingLevel = schema.ThinkingLevelMinimal
	original.ThinkingLevelRaw = "stale-original"
	adapter := ingest.DefaultAdapterRegistry[harness](nil, nil, salt.Salt{})
	extractor, ok := adapter.(ingest.TranscriptMetadataExtractor)
	if !ok {
		t.Fatalf("%s adapter has no retained transcript extraction", harness)
	}
	metadata, err := extractor.ExtractMetadataFromTranscript(t.Context(), data, original)
	if err != nil {
		t.Fatalf("extract retained metadata: %v", err)
	}
	gotSeed := expectedThinkingLevel{Level: string(metadata.ThinkingLevel), Raw: string(metadata.ThinkingLevelRaw)}
	if gotSeed != c.Seed.pair() {
		t.Errorf("session seed = %+v, want %+v", gotSeed, c.Seed.pair())
	}
	var gotHistory []expectedThinkingLevel
	for _, change := range metadata.ThinkingLevelHistory {
		gotHistory = append(gotHistory, expectedThinkingLevel{Level: string(change.Level), Raw: string(change.Raw)})
	}
	if !slices.Equal(gotHistory, c.History) {
		t.Errorf("session history = %+v, want %+v", gotHistory, c.History)
	}
	if c.Diagnostic != "" {
		found := false
		for _, warning := range metadata.Diagnostics.Warnings {
			found = found || warning.ErrorType == c.Diagnostic
		}
		if !found {
			t.Errorf("missing diagnostic %q", c.Diagnostic)
		}
	}

	sessionID, err := ingest.NewSessionID(testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := indexer.IndexTranscriptBytes(t.Context(), ingest.DiscoveredSession{SessionID: sessionID, Harness: harness, SourceFormat: ingest.SourceFormatJSONL}, data)
	if err != nil {
		t.Fatalf("index transcript: %v", err)
	}
	var turns []expectedThinkingLevel
	for _, entry := range entries {
		if entry.Role != ingest.RoleAssistant {
			assertNoThinkingLevelEvidence(t, entry)
			continue
		}
		if entry.Depth != 0 || entry.EntryType != ingest.EntryTypeText {
			continue
		}
		turns = append(turns, thinkingLevelEvidence(t, entry))
	}
	if !slices.Equal(turns, c.Turns) {
		t.Errorf("assistant turn evidence = %+v, want %+v", turns, c.Turns)
	}
}

// thinkingLevelEvidence reads the per-turn Extra keys and checks that both
// halves are valid contract values.
func thinkingLevelEvidence(t *testing.T, entry schema.SessionEntry) expectedThinkingLevel {
	t.Helper()
	if entry.Extra == nil {
		return expectedThinkingLevel{}
	}
	var extra struct {
		Level *string `json:"thinking_level"`
		Raw   *string `json:"thinking_level_raw"`
	}
	if err := json.Unmarshal([]byte(*entry.Extra), &extra); err != nil {
		t.Fatalf("entry %d extra is not JSON: %v", entry.EntryIndex, err)
	}
	var got expectedThinkingLevel
	if extra.Level != nil {
		got.Level = *extra.Level
		if got.Level == "" || !schema.ThinkingLevel(got.Level).IsValid() {
			t.Errorf("entry %d carries invalid thinking_level %q", entry.EntryIndex, got.Level)
		}
	}
	if extra.Raw != nil {
		got.Raw = *extra.Raw
		if !schema.ThinkingLevelRaw(got.Raw).IsValid() {
			t.Errorf("entry %d carries invalid thinking_level_raw %q", entry.EntryIndex, got.Raw)
		}
	}
	return got
}

func assertNoThinkingLevelEvidence(t *testing.T, entry schema.SessionEntry) {
	t.Helper()
	if got := thinkingLevelEvidence(t, entry); got != (expectedThinkingLevel{}) {
		t.Errorf("non-assistant entry %d (role %s) carries thinking-level evidence %+v", entry.EntryIndex, entry.Role, got)
	}
}

// --- OpenCode retained indexer ---

type openCodeThinkingLevelMessage struct {
	ID      string `yaml:"id"`
	Role    string `yaml:"role"`
	Variant string `yaml:"variant"`
}

type openCodeRetainedThinkingLevelCase struct {
	Name     string                         `yaml:"name"`
	Layout   string                         `yaml:"layout"`
	Messages []openCodeThinkingLevelMessage `yaml:"messages"`
	Turns    []expectedThinkingLevel        `yaml:"turns"`
}

// openCodeThinkingLevelCorpus decodes the retained-indexer section strictly;
// other sections of the shared corpus belong to other paths' tests.
type openCodeThinkingLevelCorpus struct {
	RetainedIndexerCases []openCodeRetainedThinkingLevelCase `yaml:"retainedIndexerCases"`
	OtherSections        map[string]any                      `yaml:",inline"`
}

func decodeOpenCodeRetainedThinkingLevelCorpus(data []byte) ([]openCodeRetainedThinkingLevelCase, error) {
	var corpus openCodeThinkingLevelCorpus
	if err := testutil.DecodeFixtureYAML(data, &corpus); err != nil {
		return nil, fmt.Errorf("decode OpenCode thinking-level corpus: %w", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(openCodeThinkingLevelsRetainedManifestYAML, "OpenCode retained thinking levels")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(corpus.RetainedIndexerCases))
	for _, c := range corpus.RetainedIndexerCases {
		switch c.Layout {
		case "current", "legacy", "file":
		default:
			return nil, fmt.Errorf("OpenCode retained case %q names unknown layout %q", c.Name, c.Layout)
		}
		if len(c.Messages) == 0 || len(c.Messages) != len(c.Turns) {
			return nil, fmt.Errorf("OpenCode retained case %q must list one expected turn per message", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "OpenCode retained thinking levels"); err != nil {
		return nil, err
	}
	return corpus.RetainedIndexerCases, nil
}

func TestOpenCodeRetainedIndexerThinkingLevel(t *testing.T) {
	cases, err := decodeOpenCodeRetainedThinkingLevelCorpus(openCodeThinkingLevelsYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			entries := indexOpenCodeThinkingLevelCase(t, c)
			var turns []expectedThinkingLevel
			for _, entry := range entries {
				if entry.Depth != 0 {
					continue
				}
				got := thinkingLevelEvidence(t, entry)
				if entry.Role != ingest.RoleAssistant && got != (expectedThinkingLevel{}) {
					t.Errorf("non-assistant entry %d carries evidence %+v", entry.EntryIndex, got)
				}
				turns = append(turns, got)
			}
			if !slices.Equal(turns, c.Turns) {
				t.Errorf("message evidence = %+v, want %+v", turns, c.Turns)
			}
		})
	}
}

func openCodeThinkingLevelMessageData(sessionID string, index int, message openCodeThinkingLevelMessage) json.RawMessage {
	data := map[string]any{
		"id":        message.ID,
		"sessionID": sessionID,
		"role":      message.Role,
		"content":   "fixture content",
		"time":      map[string]any{"created": 1700000000000 + int64(index)},
	}
	if message.Variant != "" {
		data["model"] = map[string]any{"id": "fixture-model", "variant": message.Variant}
	}
	encoded, _ := json.Marshal(data)
	return encoded
}

func indexOpenCodeThinkingLevelCase(t *testing.T, c openCodeRetainedThinkingLevelCase) []schema.SessionEntry {
	t.Helper()
	sessionID := testutil.TestOpenCodeSesID
	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Layout == "file" {
		root := t.TempDir()
		sessionPath := filepath.Join(root, "storage", "session", "project-fixture", sessionID+".json")
		writeThinkingLevelFile(t, sessionPath, []byte(fmt.Sprintf(`{"id":%q,"version":"0.1.0","projectID":"project-fixture","directory":"/home/test/project","time":{"created":1700000000000,"updated":1700000060000}}`, sessionID)))
		for index, message := range c.Messages {
			writeThinkingLevelFile(t, filepath.Join(root, "storage", "message", sessionID, message.ID+".json"), openCodeThinkingLevelMessageData(sessionID, index, message))
		}
		session := ingest.DiscoveredSession{SessionID: sid, Harness: defaults.HarnessOpenCode, SourcePath: ingest.ResolvedPath(sessionPath), SourceFormat: ingest.SourceFormatJSON, OriginalRoot: ingest.ResolvedPath(root)}
		entries, err := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}).IndexTranscript(t.Context(), session)
		if err != nil {
			t.Fatalf("index message files: %v", err)
		}
		return entries
	}
	format, origin := "peasant.opencode.current-sqlite", ingest.TranscriptOriginOpenCodeCurrentSQLite
	if c.Layout == "legacy" {
		format, origin = "peasant.opencode.legacy-sqlite", ingest.TranscriptOriginOpenCodeLegacySQLite
	}
	messages := make([]map[string]any, 0, len(c.Messages))
	for index, message := range c.Messages {
		created := 1700000000000 + int64(index)
		messages = append(messages, map[string]any{
			"id": message.ID, "session_id": sessionID, "time_created": created, "time_updated": created,
			"data": openCodeThinkingLevelMessageData(sessionID, index, message), "parts": []any{},
		})
	}
	projection, err := json.Marshal(map[string]any{"format": format, "version": 3, "session_id": sessionID, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}).IndexTranscriptBytes(t.Context(), ingest.DiscoveredSession{SessionID: sid, Harness: defaults.HarnessOpenCode, SourceFormat: ingest.SourceFormatJSON, TranscriptOrigin: origin}, projection)
	if err != nil {
		t.Fatalf("index %s projection: %v", c.Layout, err)
	}
	return entries
}

func writeThinkingLevelFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
