package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/harvest_metadata_diagnostics.yaml
var harvestMetadataDiagnosticsYAML []byte

// This mounts harvest's actual TTY completion branch. It changes process-global
// stderr/logger only in nonparallel tests, and captures final command stderr
// separately from the progress terminal. No guided TUI layout is changed/tested.
func TestHarvestMetadataDiagnosticsTTY(t *testing.T) {
	var fixtures struct {
		RequiredNames    []string         `yaml:"requiredNames"`
		ExpectedRootKeys []string         `yaml:"expectedRootKeys"`
		SessionID        ingest.SessionID `yaml:"sessionID"`
		Transcript       string           `yaml:"transcript"`
		Cases            []struct {
			Name                string `yaml:"name"`
			SchemaVersion       int    `yaml:"schemaVersion"`
			StoredSchemaVersion int    `yaml:"storedSchemaVersion"`
			Force               bool   `yaml:"force"`
			Reindex             bool   `yaml:"reindex"`
			WantWarning         bool   `yaml:"wantWarning"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(harvestMetadataDiagnosticsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "managed")
			dbPath := defaults.ResolveDBFilePathWith(dir).String()
			if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			files := make(map[string][]byte)
			seedHarvestIndexSession(t, db, output, harvestIndexSessionFixture{Name: fixture.Name, ID: fixtures.SessionID, Harness: ingest.HarnessClaudeCode, Age: "24h", Transcript: fixtures.Transcript}, files)
			metaPath := filepath.Join(output, testutil.TestHostSlug, string(fixtures.SessionID), string(fixtures.SessionID)+defaults.MetadataSuffix)
			var meta ingest.UnifiedMetadata
			if err := json.Unmarshal(files[metaPath], &meta); err != nil {
				t.Fatal(err)
			}
			meta.SchemaVersion = fixture.SchemaVersion
			meta.MetadataHash = schema.ComputeMetadataHash(&meta)
			files[metaPath], err = json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(metaPath, files[metaPath], 0600); err != nil {
				t.Fatal(err)
			}
			if fixture.SchemaVersion <= ingest.CurrentSchemaVersion {
				// Settle the compatible artifact change before recording the
				// no-op baseline or injecting a future stored-schema refusal.
				reconcileHarvestIndexMetadata(t, db, output, fixtures.SessionID, metaPath, files)
			}
			if fixture.StoredSchemaVersion > 0 {
				conn, err := db.Pool().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET schema_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{fixture.StoredSchemaVersion, string(fixtures.SessionID)}})
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := readHarvestIndexSnapshot(t, db, fixtures.SessionID)
			source := filepath.Join(dir, "native")
			nativePath := filepath.Join(source, "-synthetic-project", string(fixtures.SessionID)+".jsonl")
			if err := os.MkdirAll(filepath.Dir(nativePath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(nativePath, []byte(fixtures.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			old := time.UnixMilli(1700000000000)
			if err := os.Chtimes(nativePath, old, old); err != nil {
				t.Fatal(err)
			}
			files[nativePath] = []byte(fixtures.Transcript)
			configPath := writeTestConfigFile(t, dir)
			master, terminal := openTestTerminal(t)
			drained := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, master); close(drained) }()
			t.Cleanup(func() { _ = terminal.Close(); _ = master.Close(); <-drained })
			oldStderr, oldLogger := os.Stderr, slog.Default()
			os.Stderr = terminal
			var logOutput bytes.Buffer
			slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, nil)))
			defer func() { os.Stderr = oldStderr; slog.SetDefault(oldLogger) }()
			root := buildRootCommand()
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			args := []string{"--config", configPath, "--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "harvest"}
			if fixture.Reindex {
				args = append(args, "index")
			}
			args = append(args, "--source-harness=claude-code", "--output="+output, "--json")
			if !fixture.Reindex {
				args = append(args, "--source-path="+source)
			}
			if fixture.Force {
				args = append(args, "--force")
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("metadata refusal must stay nonfatal: %v; stderr=%s", err, &stderr)
			}
			if logOutput.Len() != 0 {
				t.Fatalf("TTY run did not suppress structured logs: %s", &logOutput)
			}
			if fixture.WantWarning {
				if !strings.Contains(stderr.String(), string(fixtures.SessionID)) || !strings.Contains(stderr.String(), "999") || !strings.Contains(stderr.String(), "upgrade Peasant") {
					t.Fatalf("missing actionable post-render warning: %q", stderr.String())
				}
			} else if stderr.Len() != 0 {
				t.Fatalf("compatible input produced false warning: %q", stderr.String())
			}
			var result map[string]json.RawMessage
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("JSON stdout changed: %v; %s", err, &stdout)
			}
			if keys := slices.Sorted(maps.Keys(result)); !slices.Equal(keys, fixtures.ExpectedRootKeys) {
				t.Fatalf("JSON root keys=%v want=%v", keys, fixtures.ExpectedRootKeys)
			}
			var summary ingest.PipelineSummary
			if err := json.Unmarshal(result["summary"], &summary); err != nil || summary.Errors != 0 {
				t.Fatalf("refusal changed nonfatal summary: %+v %v", summary, err)
			}
			after := readHarvestIndexSnapshot(t, db, fixtures.SessionID)
			if after.IndexerVersion != before.IndexerVersion || after.IndexedAt != before.IndexedAt || !reflect.DeepEqual(after.Entries, before.Entries) {
				t.Fatal("metadata refusal/current no-op changed last-good index")
			}
			for path, beforeBytes := range files {
				afterBytes, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(afterBytes, beforeBytes) {
					t.Fatalf("artifact/source %s changed: %v", path, err)
				}
			}
		})
	}
	if err := testutil.RequireFixtureNames("harvest metadata diagnostics", "case", fixtures.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
}
