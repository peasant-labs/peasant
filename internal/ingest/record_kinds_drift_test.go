package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

//go:embed testdata/record_kinds_capture.yaml
var recordKindsCaptureYAML []byte

type recordKindsCaptureFixture struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name            string  `yaml:"name"`
		Harness         Harness `yaml:"harness"`
		Transcript      string  `yaml:"transcript"`
		WantKind        string  `yaml:"want_kind"`
		WantNamespace   string  `yaml:"want_namespace"`
		WantPayload     string  `yaml:"want_payload"`
		WantPointer     string  `yaml:"want_pointer"`
		WantRecordIndex int64   `yaml:"want_record_index"`
		WantPosition    int64   `yaml:"want_position"`
		WantError       bool    `yaml:"want_error"`
	} `yaml:"cases"`
}

func TestRecordKindsRegistryLoads(t *testing.T) {
	if _, err := LoadRecordKindRegistry(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordKindsYAMLCurrent(t *testing.T) {
	generated, err := GenerateRecordKindRegistryYAML()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated, recordKindsYAML) {
		t.Fatal("internal/ingest/record_kinds.yaml is stale; run go generate ./internal/ingest/")
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
	if string(raw) != registry.Document() {
		t.Fatal("docs/record-kinds.md is stale; run go generate ./internal/ingest/")
	}
}

func TestRecordKindsClaudePreviewParity(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range registry.Harnesses[HarnessClaudeCode].Kinds {
		probe := kind.Kind
		if kind.Match == RecordKindPrefix {
			probe += "probe"
		}
		if kind.Namespace == "system_subtype" && probe == "compact_boundary" {
			probe = "compact-boundary"
		} else if kind.Namespace != "record" || !isClaudeControlRecordType(probe) {
			continue
		}
		hasPreview := claudeControlPreview(probe, map[string]json.RawMessage{}) != ""
		if hasPreview != (kind.Preview == RecordKindPreviewYes) {
			t.Errorf("%+v preview differs from actual control projection", kind.Key())
		}
	}
}

// Exercise the real authoritative capture boundary for every registered
// harness. Storage/publication proofs live in their respective integration suites.
func TestRecordKindsCaptureContract(t *testing.T) {
	var fixtures recordKindsCaptureFixture
	decodeRegistryFixture(t, recordKindsCaptureYAML, &fixtures)
	names := map[string]bool{}
	harnesses := map[Harness]bool{}
	for _, row := range fixtures.Cases {
		if names[row.Name] {
			t.Fatal("duplicate fixture", row.Name)
		}
		names[row.Name] = true
		if !row.WantError {
			harnesses[row.Harness] = true
		}
	}
	checkRegistryFixtureNames(t, names, fixtures.RequiredNames)
	for harness := range DefaultAdapterRegistry {
		if !harnesses[harness] {
			t.Errorf("missing capture fixture for supported harness %s", harness)
		}
		delete(harnesses, harness)
	}
	for harness := range harnesses {
		t.Errorf("capture fixture for unsupported harness %s", harness)
	}
	sid, err := NewSessionID("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range fixtures.Cases {
		t.Run(row.Name, func(t *testing.T) {
			session := DiscoveredSession{SessionID: sid, Harness: row.Harness}
			if row.Harness == HarnessOpenCode {
				session.TranscriptOrigin = TranscriptOriginOpenCodeLegacySQLite
			}
			var indexer AuthoritativeTranscriptIndexer
			fs := &OSFileSystem{}
			switch row.Harness {
			case HarnessClaudeCode:
				indexer = NewClaudeIndexer(fs)
			case HarnessCodex:
				indexer = NewCodexIndexer(fs)
			case HarnessCursor:
				indexer = NewCursorIndexer(fs)
			case HarnessStrike:
				indexer = NewStrikeIndexer(fs)
			case HarnessOpenCode:
				indexer = NewOpenCodeIndexer(fs)
			case HarnessPi:
				indexer = NewPiIndexer(fs)
			default:
				t.Fatalf("unsupported fixture harness %s", row.Harness)
			}
			capture, err := indexer.IndexTranscriptBytesForCapture(context.Background(), session, []byte(row.Transcript))
			if row.WantError {
				if err == nil {
					t.Fatal("malformed known content certified after unknown")
				}
				return
			}
			if err != nil || len(capture.RetainedUnknown) != 1 {
				t.Fatalf("capture evidence %+v: %v", capture.RetainedUnknown, err)
			}
			got := capture.RetainedUnknown[0]
			if got.Kind != row.WantKind || got.Harness != row.Harness || got.Namespace != row.WantNamespace || string(got.Payload) != row.WantPayload || got.Position.JSONPointer != row.WantPointer {
				t.Fatalf("capture evidence differs: %+v payload=%s", got, got.Payload)
			}
			public := got.Position.Public
			if public == nil || public.SourceRef == "" || public.RecordIndex != row.WantRecordIndex || public.Position != row.WantPosition {
				t.Fatalf("source coordinates %+v, want record %d position %d", public, row.WantRecordIndex, row.WantPosition)
			}
			stored, err := retainedUnknownEntries(capture.Entries)
			if err != nil || !reflect.DeepEqual(stored, capture.RetainedUnknown) {
				t.Fatalf("entry evidence differs from capture: %+v %v", stored, err)
			}
		})
	}
}
