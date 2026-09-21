package ingest

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

//go:embed testdata/record_kinds_refusal.yaml
var recordKindsRefusalYAML []byte

type recordKindsCaptureFixture struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name         string  `yaml:"name"`
		Harness      Harness `yaml:"harness"`
		Transcript   string  `yaml:"transcript"`
		WantKind     string  `yaml:"want_kind"`
		WantRetained bool    `yaml:"want_retained"`
	} `yaml:"cases"`
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

// This transitional capture fixture keeps previous failure-vs-retention
// assertions intact until the native producers are integrated. It is not a
// claim that a refusal satisfies the open fallback policy.
func TestRecordKindsCaptureContract(t *testing.T) {
	var fixtures recordKindsCaptureFixture
	decodeRegistryFixture(t, recordKindsRefusalYAML, &fixtures)
	names := map[string]bool{}
	for _, row := range fixtures.Cases {
		if names[row.Name] {
			t.Fatal("duplicate fixture", row.Name)
		}
		names[row.Name] = true
	}
	checkRegistryFixtureNames(t, names, fixtures.RequiredNames)
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
			default:
				t.Fatalf("unsupported fixture harness %s", row.Harness)
			}
			capture, err := indexer.IndexTranscriptBytesForCapture(context.Background(), session, []byte(row.Transcript))
			if row.WantRetained {
				if err != nil || len(capture.RetainedUnknown) != 1 || capture.RetainedUnknown[0].Kind != row.WantKind || capture.RetainedUnknown[0].Harness != row.Harness {
					t.Fatalf("capture evidence %+v: %v", capture.RetainedUnknown, err)
				}
				return
			}
			var refused *UnrepresentedRecordError
			if !errors.As(err, &refused) || refused.Kind != row.WantKind || refused.Harness != row.Harness {
				t.Fatalf("capture refusal: %v", err)
			}
		})
	}
}
