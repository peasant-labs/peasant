package store_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/entry_extra_precision.yaml
var entryExtraPrecisionYAML []byte

type entryExtraPrecisionFixture struct {
	Name           string            `yaml:"name"`
	Extra          string            `yaml:"extra"`
	Payload        string            `yaml:"payload"`
	ExpectedFields map[string]string `yaml:"expected_fields"`
}

func loadEntryExtraPrecisionFixtures(t *testing.T) []entryExtraPrecisionFixture {
	t.Helper()
	var corpus struct {
		Required []string                     `yaml:"required_names"`
		Cases    []entryExtraPrecisionFixture `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(entryExtraPrecisionYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&corpus); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("expected one extra-precision fixture document")
	}
	names := make(map[string]bool)
	for _, row := range corpus.Cases {
		if row.Name == "" || names[row.Name] || row.Extra == "" || row.Payload == "" || len(row.ExpectedFields) == 0 {
			t.Fatal("duplicate or incomplete extra-precision fixture")
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("entry extra precision", "case", corpus.Required, names); err != nil {
		t.Fatal(err)
	}
	return corpus.Cases
}

func TestEntryExtraExtensionPreservesJSONNumbers(t *testing.T) {
	for _, row := range loadEntryExtraPrecisionFixtures(t) {
		t.Run(row.Name, func(t *testing.T) {
			db := storetest.Open(t)
			storetest.SeedSession(t, db, testutil.TestSessionUUID)
			sid := schema.SessionID(testutil.TestSessionUUID)
			entry := schema.SessionEntry{SessionID: sid, Harness: schema.HarnessOpenCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, Extra: &row.Extra}
			if err := db.IndexSessionEntries(t.Context(), sid, []schema.SessionEntry{entry}); err != nil {
				t.Fatal(err)
			}
			check := func(t *testing.T, entries []schema.SessionEntry, err error) {
				t.Helper()
				if err != nil || len(entries) != 1 || entries[0].Extra == nil {
					t.Fatalf("read one stored entry: %v, rows=%d", err, len(entries))
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(*entries[0].Extra), &fields); err != nil {
					t.Fatal(err)
				}
				for key, expected := range row.ExpectedFields {
					if string(fields[key]) != expected {
						t.Errorf("extension merge changed %s: got %s, want %s", key, fields[key], expected)
					}
				}
				records, err := ingest.RetainedUnknownOf(entries[0])
				if err != nil || len(records) != 1 || string(records[0].Payload) != row.Payload {
					t.Fatalf("extension merge changed retained payload: %v, records=%+v", err, records)
				}
			}
			t.Run("all-entries", func(t *testing.T) {
				entries, err := db.ListEntries(t.Context(), sid)
				check(t, entries, err)
			})
			t.Run("entry-range", func(t *testing.T) {
				entries, err := db.ListEntriesRange(t.Context(), sid, 0, 0)
				check(t, entries, err)
			})
		})
	}
}
