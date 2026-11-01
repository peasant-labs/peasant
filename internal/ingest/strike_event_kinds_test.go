package ingest_test

import (
	_ "embed"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/strike_event_kinds.yaml
var strikeEventKindsFixtureData []byte

type strikeEventKindCase struct {
	Kind           string `yaml:"kind"`
	Classification string `yaml:"classification"`
	Record         string `yaml:"record"`
	Text           string `yaml:"text"`
}

type strikeEventKindsFixture struct {
	RequiredKinds []string              `yaml:"required_kinds"`
	Cases         []strikeEventKindCase `yaml:"cases"`
	Undeclared    string                `yaml:"undeclared"`
	Malformed     strikeEventKindCase   `yaml:"malformed"`
}

func loadStrikeEventKindsFixture(t *testing.T) strikeEventKindsFixture {
	t.Helper()
	var fixture strikeEventKindsFixture
	if err := yaml.Unmarshal(strikeEventKindsFixtureData, &fixture); err != nil {
		t.Fatalf("decode testdata/strike_event_kinds.yaml: %v", err)
	}
	if len(fixture.Cases) == 0 || fixture.Undeclared == "" || fixture.Malformed.Record == "" {
		t.Fatal("testdata/strike_event_kinds.yaml is missing its cases, undeclared or malformed record")
	}
	return fixture
}

// strikeKindsSession places one record after an ordinary user message so the
// capture has represented conversation around the event under test.
func strikeKindsSession(t *testing.T, record string) (ingest.DiscoveredSession, []byte) {
	t.Helper()
	const id = "CCCCCCCCCCCCCCCCCCCCCCCCCC"
	data := []byte(`{"type":"user.message","time":"2026-01-02T03:04:00Z","data":{"turnId":"turn-1","text":"synthetic request"}}` + "\n" + record + "\n")
	path := "/synthetic/strike/" + id + ".jsonl"
	resolved, err := ingest.NewResolvedPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return ingest.DiscoveredSession{SessionID: ingest.SessionID(id), Harness: ingest.HarnessStrike, SourcePath: resolved, SourceFormat: ingest.SourceFormatJSONL}, data
}

func strikeCaptureIndexer(t *testing.T) ingest.AuthoritativeTranscriptIndexer {
	t.Helper()
	idx, ok := ingest.NewIndexerRegistry(testutil.NewMemFS(), ingest.IndexerRegistryOptions{})[ingest.HarnessStrike].(ingest.AuthoritativeTranscriptIndexer)
	if !ok {
		t.Fatal("Strike indexer is not authoritative")
	}
	return idx
}

// TestStrikeLifecycleEventKindsCaptureComplete proves each Strike lifecycle,
// control and metadata event certifies as a complete capture: it is
// classified, retains no unknown evidence, and conversation-bearing kinds keep
// their text (#449).
func TestStrikeLifecycleEventKindsCaptureComplete(t *testing.T) {
	t.Parallel()
	fixture := loadStrikeEventKindsFixture(t)
	idx := strikeCaptureIndexer(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Kind, func(t *testing.T) {
			t.Parallel()
			session, data := strikeKindsSession(t, tc.Record)
			result, err := idx.IndexTranscriptBytesForCapture(t.Context(), session, data)
			if err != nil {
				t.Fatalf("capture refused %s: %v", tc.Kind, err)
			}
			if len(result.RetainedUnknown) != 0 {
				t.Fatalf("%s retained as unknown evidence (incomplete capture): %+v", tc.Kind, result.RetainedUnknown)
			}
			for _, entry := range result.Entries {
				if ingest.IsRetainedUnknownCarrier(entry) {
					t.Fatalf("%s produced a retained-unknown carrier entry", tc.Kind)
				}
			}
			switch tc.Classification {
			case "metadata", "control":
				want := ingest.IgnoredRecordReason(tc.Classification)
				found := false
				for _, ignored := range result.IgnoredRecords {
					if ignored.Kind == tc.Kind && ignored.Reason == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s not accounted as ignored %s: %+v", tc.Kind, want, result.IgnoredRecords)
				}
				if len(result.Entries) != 1 {
					t.Fatalf("%s is entryless but capture produced %d entries", tc.Kind, len(result.Entries))
				}
			case "text":
				found := false
				for _, entry := range result.Entries {
					if entry.ContentPreview != nil && strings.Contains(*entry.ContentPreview, tc.Text) {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s conversation text %q missing from entries", tc.Kind, tc.Text)
				}
			default:
				t.Fatalf("unknown classification %q", tc.Classification)
			}
		})
	}
}

// TestStrikeLifecycleUndeclaredAndMalformedKinds keeps the two boundaries: a
// well-formed undeclared kind still retains evidence, and malformed data on a
// known lifecycle kind still refuses with an error naming the kind.
func TestStrikeLifecycleUndeclaredAndMalformedKinds(t *testing.T) {
	t.Parallel()
	fixture := loadStrikeEventKindsFixture(t)
	idx := strikeCaptureIndexer(t)

	session, data := strikeKindsSession(t, fixture.Undeclared)
	result, err := idx.IndexTranscriptBytesForCapture(t.Context(), session, data)
	if err != nil {
		t.Fatalf("undeclared kind refused: %v", err)
	}
	if len(result.RetainedUnknown) != 1 || result.RetainedUnknown[0].Kind != "future.lifecycle" {
		t.Fatalf("undeclared kind retained = %+v, want one future.lifecycle record", result.RetainedUnknown)
	}

	session, data = strikeKindsSession(t, fixture.Malformed.Record)
	if _, err := idx.IndexTranscriptBytesForCapture(t.Context(), session, data); err == nil || !strings.Contains(err.Error(), fixture.Malformed.Kind) {
		t.Fatalf("malformed %s capture error = %v, want a refusal naming the kind", fixture.Malformed.Kind, err)
	}
}

// TestStrikeLifecycleEventKindsFixtureIsComplete keeps one case per required
// kind, so a kind dropped from the fixture cannot silently lose coverage.
func TestStrikeLifecycleEventKindsFixtureIsComplete(t *testing.T) {
	t.Parallel()
	fixture := loadStrikeEventKindsFixture(t)
	seen := make(map[string]int)
	for _, tc := range fixture.Cases {
		seen[tc.Kind]++
	}
	for _, kind := range fixture.RequiredKinds {
		if seen[kind] != 1 {
			t.Errorf("required kind %q has %d fixture cases, want 1", kind, seen[kind])
		}
		delete(seen, kind)
	}
	for kind := range seen {
		t.Errorf("fixture case %q is not listed in required_kinds", kind)
	}
}
