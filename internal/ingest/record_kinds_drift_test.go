package ingest

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/record_kinds_refusal.yaml
var recordKindsRefusalYAML []byte

type recordKindsRefusalFixture struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name       string `yaml:"name"`
		Harness    string `yaml:"harness"`
		Transcript string `yaml:"transcript"`
		WantKind   string `yaml:"want_kind"`
	} `yaml:"cases"`
}

func loadRecordKindsRefusalFixtures(t *testing.T) recordKindsRefusalFixture {
	t.Helper()
	var fixture recordKindsRefusalFixture
	if err := yaml.Unmarshal(recordKindsRefusalYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid record-kind refusal fixture %q", row.Name)
		}
		seen[row.Name] = true
		if row.Harness == "" || row.Transcript == "" || row.WantKind == "" {
			t.Fatalf("record-kind refusal fixture %q is incomplete", row.Name)
		}
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing record-kind refusal fixture %q", name)
		}
	}
	return fixture
}

// recordKindsCodeSets returns the kind names each harness strict path accepts,
// walked from the production vocabulary structures. A parser change that adds
// or removes a kind must update both its vocabulary and the registry, or the
// drift tests below fail.
func recordKindsCodeSets() map[Harness][]string {
	concat := func(sets ...[]string) []string {
		var out []string
		for _, set := range sets {
			out = append(out, set...)
		}
		return out
	}
	strike := make([]string, 0, len(knownStrikeEventKinds))
	for _, kind := range knownStrikeEventKinds {
		strike = append(strike, string(kind))
	}
	return map[Harness][]string{
		HarnessClaudeCode: concat(
			claudeStrictRecordKinds(),
			claudeStrictSystemSubtypes(),
			claudeStrictBlockKinds(),
			claudeControlKindLabels(),
		),
		HarnessCodex: concat(
			codexStrictEnvelopeKinds(),
			codexStrictEventMsgKinds(),
			codexStrictResponsePayloadKinds(),
			codexStrictMessageBlockKinds(),
			// The unrepresentable response shape refuses under the envelope
			// kind name; the registry tracks it under this qualified name.
			[]string{"response_item-unrepresentable"},
		),
		HarnessStrike: strike,
		HarnessOpenCode: concat(
			openCodeCaptureControlKinds(),
			knownOpenCodeSemanticPartKinds(),
		),
		HarnessCursor: concat(
			cursorStrictRoleKinds(),
			[]string{"turn_ended"},
			cursorStrictBlockKinds(),
		),
	}
}

func TestRecordKindsRegistryLoads(t *testing.T) {
	if _, err := LoadRecordKindRegistry(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordKindsDocCurrent(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../docs/record-kinds.md")
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	const beginMarker = "<!-- BEGIN GENERATED RECORD KINDS: do not hand-edit; run go generate ./internal/ingest/ -->"
	const endMarker = "<!-- END GENERATED RECORD KINDS -->"
	begin := strings.Index(document, beginMarker)
	end := strings.Index(document, endMarker)
	if begin < 0 || end < 0 || end < begin {
		t.Fatal("docs/record-kinds.md lacks the generated-table markers")
	}
	want := document[:begin+len(beginMarker)] + "\n\n" + registry.Markdown() + document[end:]
	if document != want {
		t.Fatal("docs/record-kinds.md table differs from the registry; run go generate ./internal/ingest/")
	}
}

func TestRecordKindsVersionsMatchBaseline(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for harness, section := range registry.Harnesses {
		target, ok := HarvesterVersionRegistry[harness]
		if !ok {
			t.Errorf("registry harness %q has no harvester version declaration", string(harness))
			continue
		}
		if section.AdapterVersion != target.AdapterVersion || section.IndexerVersion != target.IndexerVersion {
			t.Errorf("registry harness %q declares adapter %d indexer %d, baseline is adapter %d indexer %d; re-verify the mapping and update the registry",
				string(harness), section.AdapterVersion, section.IndexerVersion, target.AdapterVersion, target.IndexerVersion)
		}
	}
}

func TestRecordKindsCoverCodeVocabulary(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for harness, names := range recordKindsCodeSets() {
		section, ok := registry.Harnesses[harness]
		if !ok {
			t.Errorf("registry has no section for harness %q", string(harness))
			continue
		}
		byName := section.KindsByName()
		for _, name := range names {
			if _, ok := byName[name]; !ok {
				t.Errorf("harness %q: production vocabulary kind %q has no registry entry", string(harness), name)
			}
		}
	}
}

func TestRecordKindsEntriesMatchCode(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for harness, names := range recordKindsCodeSets() {
		section, ok := registry.Harnesses[harness]
		if !ok {
			continue
		}
		inCode := make(map[string]bool, len(names))
		for _, name := range names {
			inCode[name] = true
		}
		for _, kind := range section.Kinds {
			if strings.HasPrefix(kind.Source, "census") {
				// Census-observed refused kinds are documentary: the strict
				// default branch refuses them precisely because code names no
				// such kind. They must stay refused with a reason.
				if kind.Status != RecordKindRefused {
					t.Errorf("harness %q: census kind %q must stay refused, got %q", string(harness), kind.Kind, string(kind.Status))
				}
				continue
			}
			if !inCode[kind.Kind] {
				t.Errorf("harness %q: registry kind %q is not in the production vocabulary; remove it or map the new parser kind", string(harness), kind.Kind)
			}
		}
	}
}

func TestRecordKindsClaudePreviewParity(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	section, ok := registry.Harnesses[HarnessClaudeCode]
	if !ok {
		t.Fatal("registry has no claude-code section")
	}
	control := make(map[string]bool, len(claudeControlKindLabels()))
	for _, name := range claudeControlKindLabels() {
		control[name] = true
	}
	for _, kind := range section.Kinds {
		if !control[kind.Kind] {
			continue
		}
		probe := kind.Kind
		if probe == "artifact-" {
			probe = "artifact-probe"
		}
		if probe == "compact_boundary" {
			probe = "compact-boundary"
		}
		hasPreview := claudeControlPreview(probe, map[string]json.RawMessage{}) != ""
		wantPreview := kind.Preview == RecordKindPreviewYes
		if hasPreview != wantPreview {
			t.Errorf("claude-code kind %q: production preview present=%v, registry preview=%q", kind.Kind, hasPreview, string(kind.Preview))
		}
	}
}

func TestRecordKindsUnmappedKindsRefuse(t *testing.T) {
	fixture := loadRecordKindsRefusalFixtures(t)
	sessionID, err := NewSessionID("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			var harness Harness
			switch row.Harness {
			case "claude-code":
				harness = HarnessClaudeCode
			case "codex":
				harness = HarnessCodex
			case "strike":
				harness = HarnessStrike
			case "opencode":
				harness = HarnessOpenCode
			case "cursor":
				harness = HarnessCursor
			default:
				t.Fatalf("unknown harness %q", row.Harness)
			}
			session := DiscoveredSession{SessionID: sessionID, Harness: harness}
			if harness == HarnessOpenCode {
				session.TranscriptOrigin = TranscriptOriginOpenCodeLegacySQLite
			}
			var indexer AuthoritativeTranscriptIndexer
			fs := &OSFileSystem{}
			switch harness {
			case HarnessClaudeCode:
				indexer = NewClaudeIndexer(fs)
			case HarnessCodex:
				indexer = NewCodexIndexer(fs)
			case HarnessStrike:
				indexer = NewStrikeIndexer(fs)
			case HarnessOpenCode:
				indexer = NewOpenCodeIndexer(fs)
			case HarnessCursor:
				indexer = NewCursorIndexer(fs)
			}
			_, err := indexer.IndexTranscriptBytesForCapture(context.Background(), session, []byte(row.Transcript))
			var unrepresented *UnrepresentedRecordError
			if !errors.As(err, &unrepresented) {
				t.Fatalf("expected UnrepresentedRecordError, got %v", err)
			}
			if unrepresented.Kind != row.WantKind {
				t.Errorf("refusal kind %q, want %q", unrepresented.Kind, row.WantKind)
			}
			if unrepresented.Harness != harness {
				t.Errorf("refusal harness %q, want %q", string(unrepresented.Harness), string(harness))
			}
		})
	}
}
