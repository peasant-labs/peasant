package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_unknown.yaml
var openCodeUnknownYAML []byte

func TestOpenCodeUnknownNativeAndRetainedPersistence(t *testing.T) {
	var fixture struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name        string      `yaml:"name"`
			Rows        []ocProvRow `yaml:"rows"`
			Namespace   string      `yaml:"namespace"`
			Kind        string      `yaml:"kind"`
			SourceID    string      `yaml:"source_id"`
			Pointer     string      `yaml:"pointer"`
			Position    int64       `yaml:"position"`
			RecordIndex int64       `yaml:"record_index"`
			Error       bool        `yaml:"error"`
			Texts       []string    `yaml:"texts"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeUnknownYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("trailing fixture document", err)
	}
	names := []string{}
	if len(fixture.Required) == 0 {
		t.Fatal("missing required-name manifest")
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatal("duplicate or empty fixture name")
		}
		seen[row.Name] = true
		names = append(names, row.Name)
	}
	slices.Sort(names)
	slices.Sort(fixture.Required)
	if !slices.Equal(names, fixture.Required) {
		t.Fatal("fixture membership differs from required names")
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			const sid schema.SessionID = "ses_opencodeUnknownFixture"
			secret := "ghp_" + strings.Repeat("A", 36)
			payload := strings.Repeat("synthetic-padding-", 1024) + " " + secret + " FULL_UNKNOWN_TAIL"
			for i := range row.Rows {
				row.Rows[i].Data = strings.ReplaceAll(row.Rows[i].Data, "PAYLOAD", payload)
			}
			source := testfixture.MaterializeByName(t, "native-current-rows")
			seedOpenCodeProvenanceCase(t, source, ocProvCase{Scope: ocProvScope{SessionID: string(sid), ParentNullProven: true}, Rows: row.Rows})
			adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
			session := ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessOpenCode, SourcePath: ingest.ResolvedPath(source.Path), TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite}
			nativeIndexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(openCodeNativeProvenanceConfig(source, string(sid), ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}, nil)))
			candidate, nativeErr := nativeIndexer.BuildNativeGeneration(t.Context(), session)
			managed, managedErr := adapter.MaterializeTranscript(t.Context(), session)
			if row.Error {
				if nativeErr == nil || managedErr == nil {
					t.Fatalf("corrupt source accepted: native=%v retained=%v", nativeErr, managedErr)
				}
				return
			}
			if nativeErr != nil {
				t.Fatal(nativeErr)
			}
			if managedErr != nil {
				t.Fatal(managedErr)
			}
			check := func(entries []schema.SessionEntry) {
				var visible strings.Builder
				for _, turn := range transcript.EntriesToTurns(entries) {
					visible.WriteString(turn.Content)
					visible.WriteByte('\n')
				}
				for _, text := range row.Texts {
					if !strings.Contains(visible.String(), text) {
						t.Fatalf("known content %q was lost", text)
					}
				}
				var records []ingest.RetainedUnknown
				for _, entry := range entries {
					evidence, err := ingest.RetainedUnknownOf(entry)
					if err != nil {
						t.Fatal(err)
					}
					records = append(records, evidence...)
				}
				if row.Kind == "" {
					if len(records) != 0 {
						t.Fatal("additive field incorrectly classified unknown")
					}
					return
				}
				if len(records) != 1 {
					t.Fatalf("retained evidence: %+v", records)
				}
				got := records[0]
				if got.Namespace != row.Namespace || got.Kind != row.Kind || got.Position.SourceID != row.SourceID || got.Position.JSONPointer != row.Pointer {
					t.Fatalf("source identity lost: %+v", got)
				}
				if public := got.Position.Public; public == nil || public.Position != row.Position || public.RecordIndex != row.RecordIndex {
					t.Fatalf("source traversal lost: %+v", public)
				}
				if len(got.Payload) < 8192 || !bytes.Contains(got.Payload, []byte("FULL_UNKNOWN_TAIL")) || !bytes.Contains(got.Payload, []byte(secret)) || !bytes.Contains(got.Payload, []byte("9007199254740993")) {
					t.Errorf("stored evidence is not raw source bytes: length=%d tail=%t secret=%t precision=%t", len(got.Payload), bytes.Contains(got.Payload, []byte("FULL_UNKNOWN_TAIL")), bytes.Contains(got.Payload, []byte(secret)), bytes.Contains(got.Payload, []byte("9007199254740993")))
				}
				for _, turn := range transcript.EntriesToTurns(entries) {
					if strings.Contains(turn.Content, "FULL_UNKNOWN_TAIL") {
						t.Fatal("unknown payload displayed")
					}
					for _, entry := range entries {
						if ingest.IsRetainedUnknownCarrier(entry) && entry.EntryIndex == turn.Index {
							t.Fatal("opaque carrier displayed")
						}
					}
				}
			}
			retainedIndexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullContent(true), ingest.WithOpenCodeFullDepth(true))
			check(candidate.Result.Generation.Main.Entries)
			retained, err := retainedIndexer.IndexTranscriptBytesForCapture(t.Context(), session, managed.Data)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("check retained parse")
			check(retained.Entries)
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "unknown.db")
			root := filepath.Join(dir, "artifacts")
			db := nativeRepairStore(t, dbPath, root)
			seedOpenCodeRepairSession(t, db, string(sid), sid)
			if err := db.IndexSessionEntries(t.Context(), sid, retained.Entries); err != nil {
				t.Fatal(err)
			}
			stored, err := db.ListEntries(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("check stored retained entries")
			check(stored)
			activation := ingest.NativeGenerationActivation{Generation: candidate.Result, Blobs: candidate.Blobs, PriorEvidence: candidate.PriorEvidence, IndexerVersion: 18, IndexedAtMs: 1, ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, FailureCode: ingest.ContentCaptureUnknownDataRetained, SourceAuthority: ingest.ContentSourceProviderSource, TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite, CaptureFormat: ingest.ContentCaptureFormatPreviewOnly, CapturedAtMs: 1}}
			if _, err := db.ActivateNativeGeneration(t.Context(), activation); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = nativeRepairStore(t, dbPath, root)
			defer db.Close()
			verify := func(snapshot indexformat.ReadSnapshot) error {
				t.Log("check native snapshot")
				check(snapshot.Main.Entries)
				return nil
			}
			if err := db.WithSessionSnapshot(t.Context(), sid, verify); err != nil {
				t.Fatal(err)
			}
			broken := activation
			broken.Generation.Generation.ID = "failed-unknown-generation"
			broken.Blobs = map[schema.SourceEntryRef][]byte{}
			if _, err := db.ActivateNativeGeneration(t.Context(), broken); err == nil {
				t.Fatal("missing blobs authorized replacement")
			}
			if err := db.WithSessionSnapshot(t.Context(), sid, verify); err != nil {
				t.Fatal(err)
			}
		})
	}
}
