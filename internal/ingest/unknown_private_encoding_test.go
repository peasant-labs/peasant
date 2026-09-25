package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/unknown_private_encoding.yaml
var unknownPrivateEncodingYAML []byte

func TestUnknownPrivateEncoding(t *testing.T) {
	t.Parallel()
	var doc struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name          string `yaml:"name"`
			Extra         string `yaml:"extra"`
			Payload       string `yaml:"payload"`
			MissingPublic bool   `yaml:"missing_public"`
			Error         bool   `yaml:"error"`
			Count         int    `yaml:"count"`
			NoEcho        string `yaml:"no_echo"`
			Field         string `yaml:"field"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(unknownPrivateEncodingYAML))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing fixture document: %v", err)
	}
	names := map[string]bool{}
	var actualNames []string
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("duplicate or empty fixture name")
		}
		names[c.Name] = true
		actualNames = append(actualNames, c.Name)
	}
	if err := testutil.RequireFixtureNames("unknown private encoding", "case", doc.Required, names); err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: doc.Required}, actualNames, "unknown private encoding"); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			entry := schema.SessionEntry{Harness: schema.HarnessClaudeCode, Extra: &c.Extra}
			records, err := ingest.RetainedUnknownOf(entry)
			if c.Error {
				if err == nil {
					t.Fatal("malformed private encoding accepted")
				}
				var integrity *ingest.EvidenceIntegrityError
				if !errors.As(err, &integrity) {
					t.Fatalf("corrupt evidence did not surface the typed integrity error: %v", err)
				}
				if errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
					t.Fatalf("corruption masked as legacy missing-position compatibility: %v", err)
				}
				if c.Field == "" {
					t.Fatal("refusal fixture missing owned field-path oracle")
				}
				if integrity.Field != c.Field {
					t.Fatalf("integrity error named %q want owned field %q", integrity.Field, c.Field)
				}
				if c.NoEcho != "" && strings.Contains(err.Error(), c.NoEcho) {
					t.Fatal("integrity error echoed raw source bytes")
				}
				return
			}
			want := c.Count
			if want == 0 {
				want = 1
			}
			if err != nil || len(records) != want {
				t.Fatalf("decoded evidence changed: %d records %+v", len(records), err)
			}
			if c.Payload != "" {
				if want != 1 || string(records[0].Payload) != c.Payload {
					t.Fatalf("decoded payload changed: %+v", records)
				}
			}
			entry.Extra = nil
			if err := ingest.AttachRetainedUnknown(&entry, records); err != nil {
				t.Fatal(err)
			}
			roundtrip, err := ingest.RetainedUnknownOf(entry)
			if err != nil || len(roundtrip) != want {
				t.Fatalf("rewriting changed the evidence set: %+v %v", roundtrip, err)
			}
			if c.Payload != "" && string(roundtrip[0].Payload) != c.Payload {
				t.Fatalf("rewriting normalized source JSON: %+v", roundtrip)
			}
			projected, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessClaudeCode)
			if c.MissingPublic {
				if !errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
					t.Fatalf("missing coordinates fabricated: %v", err)
				}
				var legacy *ingest.LegacyPositionUnavailableError
				if !errors.As(err, &legacy) {
					t.Fatalf("legacy absence did not surface the typed legacy error: %v", err)
				}
				assertCodecStoreRoundTrip(t, c.Extra, want, c.Payload, true)
				return
			}
			if err != nil || len(projected) != want {
				t.Fatalf("valid evidence refused export certification: %+v %v", projected, err)
			}
			if c.Payload != "" && (projected[0].RecordIndex != 0 || projected[0].Position != 0 || projected[0].Payload != c.Payload) {
				t.Fatalf("zero source coordinates/payload lost: %+v", projected)
			}
			assertCodecStoreRoundTrip(t, c.Extra, want, c.Payload, false)
		})
	}
}

// assertCodecStoreRoundTrip proves the fixture bytes survive a real durable
// write, a close/reopen cycle, and read-side revalidation: the reopened row
// decodes to the same payload and still passes export certification. Legacy
// rows (wantLegacy) must still refuse export certification with the typed
// legacy error after reopen instead of certifying.
func assertCodecStoreRoundTrip(t *testing.T, extra string, want int, payload string, wantLegacy bool) {
	t.Helper()
	ctx := t.Context()
	sid := schema.SessionID(testutil.TestSessionUUID)
	path := t.TempDir() + "/codec.db"
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	storetest.SeedSession(t, db, string(sid))
	entry := schema.SessionEntry{
		SessionID:  sid,
		EntryIndex: 0,
		Harness:    schema.HarnessClaudeCode,
		Role:       schema.RoleSystem,
		EntryType:  schema.EntryTypeSystem,
		Extra:      &extra,
	}
	// Retained evidence certifies as incomplete/full with the unknown-data
	// failure code: claiming Complete here would (correctly) trip the store's
	// capture/evidence agreement check, so declare the honest tuple.
	versions, registered := ingest.HarvesterVersionRegistry[schema.HarnessClaudeCode]
	if !registered {
		t.Fatal("claude-code missing from HarvesterVersionRegistry")
	}
	write := ingest.SessionEntryWrite{
		SessionID:          sid,
		Result:             indexformat.V1{Entries: []schema.SessionEntry{entry}},
		IndexVersion:       versions.IndexVersion,
		RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:          ingest.ContentCaptureIncomplete,
			SourceAuthority: ingest.ContentSourceNewIngest,
			CaptureFormat:   ingest.ContentCaptureFormatFull,
			FailureCode:     ingest.ContentCaptureUnknownDataRetained,
		},
	}
	if wantLegacy {
		// Legacy evidence carries no traversal coordinates, so it never
		// certifies as full content: store it through the honest preview
		// path, which skips the full-content evidence certificate.
		write.RequireFullContent = false
		write.ContentCapture = ingest.SessionContentCaptureWrite{
			Status:          ingest.ContentCaptureIncomplete,
			SourceAuthority: ingest.ContentSourceNewIngest,
			CaptureFormat:   ingest.ContentCaptureFormatLegacyPreviewOnly,
			FailureCode:     ingest.ContentCaptureLegacyPreviewOnly,
		}
	}
	results := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{write})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("real store write refused valid evidence: %+v", results)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reopened, err := db.ListEntries(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	var found []schema.SessionEntry
	for _, row := range reopened {
		if row.Extra != nil {
			found = append(found, row)
		}
	}
	if len(found) == 0 {
		t.Fatal("stored evidence row missing after reopen")
	}
	var records int
	for _, row := range found {
		decoded, err := ingest.RetainedUnknownOf(row)
		if err != nil {
			t.Fatal(err)
		}
		records += len(decoded)
		if payload != "" {
			for _, record := range decoded {
				if string(record.Payload) != payload {
					t.Fatalf("reopened payload changed: %.120s", record.Payload)
				}
			}
		}
	}
	if records != want {
		t.Fatalf("reopened evidence count %d want %d", records, want)
	}
	projected, err := ingest.ProjectRetainedUnknown(found, schema.HarnessClaudeCode)
	if wantLegacy {
		if !errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
			t.Fatalf("reopened legacy evidence certified export: %+v %v", projected, err)
		}
		var legacy *ingest.LegacyPositionUnavailableError
		if !errors.As(err, &legacy) {
			t.Fatalf("reopened legacy absence did not surface the typed legacy error: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("reopened evidence refused export certification: %v", err)
	}
}
