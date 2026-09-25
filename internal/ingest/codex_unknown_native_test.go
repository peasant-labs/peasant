package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/codex_unknown.yaml
var codexUnknownNativeYAML []byte

func TestCodexUnknownNativePersistence(t *testing.T) {
	var fixture struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name           string `yaml:"name"`
			Record         string `yaml:"record"`
			Namespace      string `yaml:"namespace"`
			Kind           string `yaml:"kind"`
			Pointer        string `yaml:"pointer"`
			Error          bool   `yaml:"error"`
			NativeOnly     bool   `yaml:"native_only"`
			CopyBoundary   *int64 `yaml:"copy_boundary"`
			Reverted       bool   `yaml:"reverted"`
			Position       int64  `yaml:"position"`
			NativePosition int64  `yaml:"native_position"`
			PrefixBlank    bool   `yaml:"prefix_blank"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(codexUnknownNativeYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("trailing YAML", err)
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
		t.Fatal("required fixture membership differs")
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			const sid schema.SessionID = "11111111-1111-4111-8111-111111111111"
			dir := t.TempDir()
			source := filepath.Join(dir, "rollout.jsonl")
			known := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"known"}]}}`
			header := `{"type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","history_mode":"legacy"}}`
			payload := strings.Repeat("synthetic-padding-", 1024) + " FULL_UNKNOWN_TAIL"
			var raw map[string]json.RawMessage
			// Add a large field to the unknown value itself, not to its known owner.
			record := row.Record
			if row.Name == "envelope" {
				if err := json.Unmarshal([]byte(row.Record), &raw); err != nil {
					t.Fatal(err)
				}
				raw["large_padding"], _ = json.Marshal(payload)
				encoded, _ := json.Marshal(raw)
				record = string(encoded)
			}
			middle := record + "\n"
			line := 3
			if row.PrefixBlank {
				middle = "\n" + middle
				line++
			}
			if row.Reverted {
				middle += `{"type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":1}}` + "\n"
			}
			data := header + "\n" + known + "\n" + middle + known + "\n"
			if err := os.WriteFile(source, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			session := ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessCodex, SourcePath: ingest.ResolvedPath(source)}
			idx := ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexProvenanceCapture(ingest.CodexProvenanceIndexerConfig{Enabled: true, GenerationID: func(ingest.DiscoveredSession) string { return "unknown-native" }}))
			candidate, err := idx.BuildNativeGeneration(t.Context(), session)
			if row.CopyBoundary != nil {
				source := &fixtureCodexSource{authority: ingest.CodexSourceAuthority{StableThreadID: string(sid), Kind: ingest.CodexAuthorityDetachedFile, CurrentPointer: "current", PhysicalSourceID: "synthetic-stream", HistoryMode: json.RawMessage(`"legacy"`), CopyBoundary: row.CopyBoundary, OriginalOwnershipProven: true}, sources: map[string][]byte{"current": []byte(data)}}
				history, captureErr := ingest.CaptureCodexHistoryWithRetry(t.Context(), source, session, nil)
				if captureErr != nil {
					t.Fatal(captureErr)
				}
				built, buildErr := ingest.BuildCodexCandidate(ingest.CodexCandidateInput{History: history, Base: schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: sid, ModelHarness: ingest.HarnessCodex}, GenerationID: "unknown-native"})
				candidate = ingest.NativeGenerationCandidate{Result: built.V2, Blobs: built.Content}
				err = buildErr
			}
			if row.Error {
				if err == nil {
					t.Fatal("invalid known field authorized replacement")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(dir, "store.db")
			root := filepath.Join(dir, "artifacts")
			db := nativeRepairStore(t, dbPath, root)
			seedOpenCodeRepairSession(t, db, string(sid), sid)
			conn, err := db.Pool().Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET model_harness = 'codex' WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sid)}})
			db.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			activation := ingest.NativeGenerationActivation{Generation: candidate.Result, Blobs: candidate.Blobs, IndexerVersion: 18, IndexedAtMs: 1, ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, FailureCode: ingest.ContentCaptureUnknownDataRetained, SourceAuthority: ingest.ContentSourceProviderSource, TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatPreviewOnly, CapturedAtMs: 1}}
			if _, err := db.ActivateNativeGeneration(t.Context(), activation); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = nativeRepairStore(t, dbPath, root)
			defer db.Close()
			check := func(snapshot indexformat.ReadSnapshot) error {
				var evidence []ingest.RetainedUnknown
				for _, entry := range snapshot.Main.Entries {
					records, err := ingest.RetainedUnknownOf(entry)
					if err != nil {
						return err
					}
					evidence = append(evidence, records...)
				}
				if row.Reverted {
					if len(evidence) != 0 {
						t.Fatal("reverted evidence survived")
					}
					return nil
				}
				if len(evidence) != 1 {
					t.Fatalf("persisted evidence: %+v", evidence)
				}
				got := evidence[0]
				if public := got.Position.Public; public == nil || public.RecordIndex != int64(line-1) || public.Position != row.NativePosition || !strings.HasPrefix(public.SourceRef, "src_") {
					t.Fatalf("wrong source traversal: %+v", public)
				}
				if got.Namespace != row.Namespace || got.Kind != row.Kind || got.Position.Line != line || got.Position.JSONPointer != row.Pointer {
					t.Fatalf("coordinates: %+v", got)
				}
				if strings.Contains(row.Record, "ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD") && !bytes.Contains(got.Payload, []byte("ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD")) {
					t.Fatal("stored evidence is not raw source bytes")
				}
				if row.Name == "envelope" && (!bytes.Contains(got.Payload, []byte(payload)) || !bytes.Contains(got.Payload, []byte("9007199254740993"))) {
					t.Fatal("large payload or precise number lost")
				}
				for _, turn := range transcript.EntriesToTurns(snapshot.Main.Entries) {
					for _, entry := range snapshot.Main.Entries {
						if ingest.IsRetainedUnknownCarrier(entry) && entry.EntryIndex == turn.Index {
							t.Fatal("evidence carrier displayed")
						}
					}
				}
				return nil
			}
			if err := db.WithSessionSnapshot(t.Context(), sid, check); err != nil {
				t.Fatal(err)
			}
			// A real blob integrity failure cannot replace the previous generation.
			broken := activation
			broken.Generation.Generation.ID = "broken-unknown-native"
			broken.Blobs = map[schema.SourceEntryRef][]byte{}
			if _, err := db.ActivateNativeGeneration(t.Context(), broken); err == nil {
				t.Fatal("missing blobs authorized replacement")
			}
			if err := db.WithSessionSnapshot(t.Context(), sid, check); err != nil {
				t.Fatal(err)
			}
		})
	}
}
