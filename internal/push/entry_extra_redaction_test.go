package push_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/entry_extra_redaction.yaml
var entryExtraRedactionYAML []byte

//go:embed testdata/entry_extra_redaction_manifest.yaml
var entryExtraRedactionManifestYAML []byte

type entryExtraRedactionCase struct {
	Name      string `yaml:"name"`
	Extra     string `yaml:"extra"`
	Want      string `yaml:"want"`
	SameBytes bool   `yaml:"sameBytes"`
	Absent    string `yaml:"absent"`
	Refused   bool   `yaml:"refused"`
}

func loadEntryExtraRedactionCases(t *testing.T) []entryExtraRedactionCase {
	t.Helper()
	var fixture struct {
		Cases []entryExtraRedactionCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(entryExtraRedactionYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode entry_extra_redaction.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(entryExtraRedactionManifestYAML, "entry extra redaction")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "entry extra redaction"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// TestEntryExtraRedactedAsDocument proves publication redaction keeps a stored
// Extra valid JSON when a rule matches next to one of its escapes, on both
// seams that carry entries, and that every later reader still accepts it.
func TestEntryExtraRedactedAsDocument(t *testing.T) {
	t.Parallel()
	for _, tc := range loadEntryExtraRedactionCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			extra, preview := tc.Extra, "x"
			entry := schema.SessionEntry{
				SessionID: "11111111-2222-3333-4444-555555555555", Harness: schema.HarnessClaudeCode,
				EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: &preview, Extra: &extra,
			}
			entries, err := push.RedactEntries(redactor, []schema.SessionEntry{entry})
			if tc.Refused {
				if err == nil {
					t.Fatalf("RedactEntries published the entry, want it refused; Extra %s", *entries[0].Extra)
				}
				return
			}
			if err != nil {
				t.Fatalf("RedactEntries: %v", err)
			}
			assertExtra(t, "RedactEntries", entries[0].Extra, tc)

			meta := ingest.NewUnifiedMetadata()
			meta.SessionID, meta.SchemaVersion = "11111111-2222-3333-4444-555555555555", 9
			meta.ModelHarness, meta.Model = schema.HarnessClaudeCode, "claude-opus-4-6"
			meta.Timestamp.Start, meta.Timestamp.End = 1, 2
			meta.Source.Format = schema.SourceFormatJSONL
			meta.Project.Hash, meta.Project.Name = schema.ProjectHash(strings.Repeat("a", 64)), "widgets"
			payload, err := push.MapMetadata(push.MapOptions{
				Meta: &meta, Entries: entries, Fields: config.PushFieldVisibility{}.Resolve(), Redactor: redactor,
			})
			if err != nil {
				t.Fatalf("MapMetadata: %v", err)
			}
			var request struct {
				Entries []schema.SessionEntry `json:"entries"`
			}
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Fatal(err)
			}
			if len(request.Entries) != 1 {
				t.Fatalf("request carries %d entries, want 1", len(request.Entries))
			}
			assertExtra(t, "MapMetadata", request.Entries[0].Extra, tc)
		})
	}
}

func assertExtra(t *testing.T, seam string, got *string, tc entryExtraRedactionCase) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s dropped Extra", seam)
	}
	if _, _, err := ingest.DecodePiExtra(got); err != nil {
		t.Fatalf("%s left an Extra the entry reader refuses: %v\n%s", seam, err, *got)
	}
	if tc.Absent != "" && strings.Contains(*got, tc.Absent) {
		t.Fatalf("%s published %q: %s", seam, tc.Absent, *got)
	}
	if tc.Want == "" && !tc.SameBytes {
		return
	}
	if tc.SameBytes {
		if *got != tc.Extra {
			t.Fatalf("%s changed an Extra no rule matched:\n got %s\nwant %s", seam, *got, tc.Extra)
		}
		return
	}
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(*got), &gotValue); err != nil {
		t.Fatalf("%s Extra is not JSON: %v\n%s", seam, err, *got)
	}
	if err := json.Unmarshal([]byte(tc.Want), &wantValue); err != nil {
		t.Fatalf("fixture want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s Extra = %s, want %s", seam, *got, tc.Want)
	}
}
