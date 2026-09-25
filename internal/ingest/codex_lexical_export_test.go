package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/codex_unknown_lexical.yaml
var codexLexicalExportYAML []byte

type codexLexicalExportCase struct {
	Name             string   `yaml:"name"`
	Kind             string   `yaml:"kind"`
	Namespace        string   `yaml:"namespace"`
	Pointer          string   `yaml:"pointer"`
	Record           string   `yaml:"record"`
	ExpectedPayload  string   `yaml:"expected_payload"`
	ExpectedSiblings int      `yaml:"expected_siblings"`
	ExpectedKnown    []string `yaml:"expected_known"`
	ExpectedPosition int64    `yaml:"expected_position"`
	WideSiblings     int      `yaml:"wide_siblings"`
	LeafSize         int      `yaml:"leaf_size"`
	UnknownEvery     int      `yaml:"unknown_every"`
}

// TestCodexLexicalReopenExport is the SLICE-5-L4 consolidated pass: lexical +
// wide bytes through a real SQLite close/reopen and export. The prepare path
// (pointer rebasing, sibling alignment) is pinned in TestCodexLexicalFidelity;
// this test pins that the same bytes survive the store boundary and the
// export-time baseline egress byte-exact (lexical payloads carry no secrets,
// so baseline redaction must leave them unchanged). Positions, pointers,
// siblings, and payloads hold in exported bytes.
func TestCodexLexicalReopenExport(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Required []string                 `yaml:"required_names"`
		Cases    []codexLexicalExportCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(codexLexicalExportYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("trailing fixture document")
	}
	// The manifest owns the case list: deleting or renaming a fixture case
	// fails here, and an undeclared row fails the same way. The fidelity test
	// enforces the same manifest on the prepare path; this test enforces it
	// on the store-boundary + egress path.
	actualNames := make([]string, 0, len(fixture.Cases))
	seenNames := map[string]bool{}
	for _, row := range fixture.Cases {
		if row.Name == "" || seenNames[row.Name] {
			t.Fatalf("invalid lexical fixture %q", row.Name)
		}
		seenNames[row.Name] = true
		actualNames = append(actualNames, row.Name)
	}
	if err := testutil.RequireFixtureNames("codex lexical export", "case", fixture.Required, seenNames); err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.Required}, actualNames, "codex lexical export"); err != nil {
		t.Fatal(err)
	}
	// Non-wide lexical cases: exact payload/position/siblings through
	// close/reopen + export.
	for _, row := range fixture.Cases {
		if row.WideSiblings > 0 {
			continue
		}
		// Oracle-field integrity: the prepare path is pinned in
		// TestCodexLexicalFidelity, but these decoded fields must still mean
		// something here — a shrunk or corrupted fixture row fails before any
		// store work starts.
		if !json.Valid([]byte(row.Record)) || !strings.Contains(row.Record, `"future"`) {
			t.Fatalf("lexical fixture %q carries no future-bearing record", row.Name)
		}
		if !json.Valid([]byte(row.ExpectedPayload)) {
			t.Fatalf("lexical fixture %q expected_payload is not JSON", row.Name)
		}
		if row.ExpectedSiblings != len(row.ExpectedKnown) {
			t.Fatalf("lexical fixture %q expected_siblings=%d disagrees with %d known siblings", row.Name, row.ExpectedSiblings, len(row.ExpectedKnown))
		}
		for _, known := range row.ExpectedKnown {
			if !json.Valid([]byte(known)) {
				t.Fatalf("lexical fixture %q expected_known entry is not JSON: %.80s", row.Name, known)
			}
		}
		if row.Pointer == "" || row.Pointer[0] != '/' {
			t.Fatalf("lexical fixture %q pointer is not a JSON pointer: %q", row.Name, row.Pointer)
		}
		if row.Kind == "" || row.Namespace == "" {
			t.Fatalf("lexical fixture %q names no kind or namespace", row.Name)
		}
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dbPath := storetest.CopyGoldenDB(t)
			db, err := store.Open(dbPath, store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			storetest.SeedSession(t, db, sid.String())
			position := ingest.UnknownSourcePosition{
				Line: 7, SourceID: "lexical-stream",
				Public: &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: 0, Position: row.ExpectedPosition},
			}
			record, err := ingest.NewRetainedUnknown(ingest.HarnessClaudeCode, row.Namespace, row.Kind, position, json.RawMessage(row.ExpectedPayload))
			if err != nil {
				t.Fatal(err)
			}
			// Preserve the fixture pointer for export assertion.
			record.Position.JSONPointer = row.Pointer
			carrier, err := ingest.RetainedUnknownEntry(sid, 1, record)
			if err != nil {
				t.Fatal(err)
			}
			versions := testutil.HarvesterVersionsForSeed(t, ingest.HarnessClaudeCode)
			written := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
				SessionID: sid, Result: indexformat.V1{Entries: []schema.SessionEntry{carrier}},
				IndexVersion: versions.IndexVersion, IndexerVersion: versions.IndexerVersion,
				CaptureRevision: 1, RequireFullContent: true,
				ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, FailureCode: ingest.ContentCaptureUnknownDataRetained, CaptureFormat: ingest.ContentCaptureFormatFull, SourceAuthority: ingest.ContentSourceNewIngest},
			}})
			if len(written) != 1 || written[0].Err != nil {
				t.Fatalf("store write: %+v", written)
			}
			// Real SQLite close/reopen: committed bytes must survive.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db2, err := store.Open(dbPath, store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db2.Close() })
			entries, err := db2.ListEntries(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			var evidence []ingest.RetainedUnknown
			for _, entry := range entries {
				records, err := ingest.RetainedUnknownOf(entry)
				if err != nil {
					t.Fatal(err)
				}
				evidence = append(evidence, records...)
			}
			if len(evidence) != 1 || string(evidence[0].Payload) != row.ExpectedPayload {
				t.Fatalf("reopened payload mismatch: %+v", evidence)
			}
			exported, err := export.ExportSession(ctx, db2, testutil.NewMemFS(), sid.String())
			if err != nil {
				t.Fatalf("export: %v", err)
			}
			if len(exported.RetainedUnknown) != 1 {
				t.Fatalf("exported retained count: %d", len(exported.RetainedUnknown))
			}
			got := exported.RetainedUnknown[0]
			if got.Payload != row.ExpectedPayload || got.Pointer != row.Pointer ||
				got.Position != row.ExpectedPosition || got.Namespace != row.Namespace || got.Kind != row.Kind {
				t.Fatalf("exported lexical evidence differs: %+v want payload %q pointer %q position %d",
					got, row.ExpectedPayload, row.Pointer, row.ExpectedPosition)
			}
		})
	}
}

// TestCodexWideReopenExport carries a wide sibling set through close/reopen +
// export, asserting every retained leaf's payload, pointer, and position
// survive. The sibling recipe comes from the wide_128 fixture row (the 512/2048
// recipes stay in the fidelity/performance gate); this pass proves wide-count
// egress without repeating the long trio.
func TestCodexWideReopenExport(t *testing.T) {
	t.Parallel()
	// The YAML-owned recipe: hardcoded counts would drift from the fixture.
	var wideFixture struct {
		Required []string                 `yaml:"required_names"`
		Cases    []codexLexicalExportCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(codexLexicalExportYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&wideFixture); err != nil {
		t.Fatal(err)
	}
	var wide *codexLexicalExportCase
	for i := range wideFixture.Cases {
		if wideFixture.Cases[i].Name == "wide_128" {
			wide = &wideFixture.Cases[i]
			break
		}
	}
	if wide == nil {
		t.Fatal("wide_128 recipe missing from codex lexical fixture")
	}
	if wide.WideSiblings <= 0 || wide.UnknownEvery <= 0 || wide.LeafSize <= 0 {
		t.Fatalf("wide_128 recipe is not a positive recipe: %+v", *wide)
	}
	ctx := context.Background()
	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	storetest.SeedSession(t, db, sid.String())
	siblings := wide.WideSiblings
	every := wide.UnknownEvery
	pad := strings.Repeat("x", wide.LeafSize/4)
	var carriers []schema.SessionEntry
	var want []struct {
		payload  string
		pointer  string
		position int64
	}
	idx := 0
	for i := 0; i < siblings; i++ {
		if i%every != 0 {
			continue
		}
		payload := fmt.Sprintf(`{"type":"future","leaf":%d,"pad":"%s"}`, i, pad)
		pointer := fmt.Sprintf("/payload/item/content/%d", i)
		position := ingest.UnknownSourcePosition{
			Line: 11 + i, SourceID: "lexical-wide-stream",
			Public: &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: int64(i), Position: int64(3 + i)},
		}
		record, err := ingest.NewRetainedUnknown(ingest.HarnessClaudeCode, "message_block", "future", position, json.RawMessage(payload))
		if err != nil {
			t.Fatal(err)
		}
		record.Position.JSONPointer = pointer
		carrier, err := ingest.RetainedUnknownEntry(sid, idx, record)
		if err != nil {
			t.Fatal(err)
		}
		carriers = append(carriers, carrier)
		want = append(want, struct {
			payload  string
			pointer  string
			position int64
		}{payload, pointer, int64(3 + i)})
		idx++
	}
	versions := testutil.HarvesterVersionsForSeed(t, ingest.HarnessClaudeCode)
	written := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: sid, Result: indexformat.V1{Entries: carriers},
		IndexVersion: versions.IndexVersion, IndexerVersion: versions.IndexerVersion,
		CaptureRevision: 1, RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, FailureCode: ingest.ContentCaptureUnknownDataRetained, CaptureFormat: ingest.ContentCaptureFormatFull, SourceAuthority: ingest.ContentSourceNewIngest},
	}})
	if len(written) != 1 || written[0].Err != nil {
		t.Fatalf("store write: %+v", written)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	exported, err := export.ExportSession(ctx, db2, testutil.NewMemFS(), sid.String())
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exported.RetainedUnknown) != len(want) {
		t.Fatalf("exported wide count = %d, want %d", len(exported.RetainedUnknown), len(want))
	}
	byPointer := map[string]schema.RetainedUnknownRecord{}
	for _, record := range exported.RetainedUnknown {
		byPointer[record.Pointer] = record
	}
	for _, w := range want {
		got, ok := byPointer[w.pointer]
		if !ok {
			t.Fatalf("missing exported wide leaf %s", w.pointer)
		}
		if got.Payload != w.payload || got.Position != w.position {
			t.Fatalf("wide leaf %s differs: %+v want payload %q position %d", w.pointer, got, w.payload, w.position)
		}
	}
}
