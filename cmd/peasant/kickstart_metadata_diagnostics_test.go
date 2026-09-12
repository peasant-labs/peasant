package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/kickstart_metadata_diagnostics.yaml
var kickstartMetadataDiagnosticsYAML []byte

func TestKickstartLocalIngestForwardsMetadataDiagnostics(t *testing.T) {
	var fixture struct {
		RequiredNames []string                     `yaml:"requiredNames"`
		Session       selectionRunnerSourceSession `yaml:"session"`
		Cases         []struct {
			Name           string `yaml:"name"`
			SidecarVersion int    `yaml:"sidecarVersion"`
			StoreVersion   int    `yaml:"storeVersion"`
			WantWarning    bool   `yaml:"wantWarning"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(kickstartMetadataDiagnosticsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("diagnostic fixture must contain exactly one document")
	}
	required := []string{"future-sidecar", "future-store", "compatible-control"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("required diagnostic manifest changed")
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid diagnostic case %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "native")
			writeSelectionRunnerSources(t, source, []selectionRunnerSourceSession{fixture.Session})
			cfg := config.BaseConfig()
			cfg.Selection.Mode = config.SelectionModeAll
			cfg.Sources = config.SourcesConfig{ClaudeCode: config.SourceProviderConfig{Enabled: true, Paths: []string{source}}}
			cfg.Output.BasePath = filepath.Join(root, "managed")
			configPath := filepath.Join(root, "config.yaml")
			if err := config.SaveAtomic(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			cmd := &cobra.Command{Use: "kickstart"}
			cmd.Flags().String("data-dir", root, "")
			run, _ := kickstartLocalIngest(cmd, configPath, selectionRunnerListings([]selectionRunnerSourceSession{fixture.Session}))
			first, err := run(t.Context())
			if err != nil || first == nil || first.New != 1 {
				t.Fatalf("seed real local ingestion: %+v %v", first, err)
			}
			db, err := store.Open(defaults.ResolveDBFilePathWith(root).String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sid, err := ingest.NewSessionID(fixture.Session.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			locations, err := db.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{sid})
			if err != nil {
				t.Fatal(err)
			}
			metadataPath := filepath.Join(cfg.Output.BasePath, locations[sid].HostSlug, string(sid), string(sid)+defaults.MetadataSuffix)
			original, err := os.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			if row.SidecarVersion != 0 {
				var metadata ingest.UnifiedMetadata
				if err := json.Unmarshal(original, &metadata); err != nil {
					t.Fatal(err)
				}
				metadata.SchemaVersion = row.SidecarVersion
				original, err = json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(metadataPath, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if row.StoreVersion != 0 {
				conn, err := db.Pool().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET schema_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{row.StoreVersion, string(sid)}})
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := readHarvestIndexSnapshot(t, db, sid)
			var logs bytes.Buffer
			old := slog.Default()
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			slog.SetDefault(logger)
			defer slog.SetDefault(old)
			result, err := run(t.Context())
			if err != nil || result == nil || result.Errors != 0 || result.New != 0 || result.Updated != 0 || result.Unchanged != 1 {
				t.Fatalf("nonfatal result changed: %+v %v", result, err)
			}
			if slog.Default() != logger || logs.Len() != 0 {
				t.Fatal("runner did not preserve suppressed logging boundary")
			}
			if got := len(result.Diagnostics) > 0; got != row.WantWarning {
				t.Fatalf("diagnostics=%+v want warning=%t", result.Diagnostics, row.WantWarning)
			}
			if row.WantWarning {
				encoded, _ := json.Marshal(result.Diagnostics)
				if !strings.Contains(string(encoded), "999") || !strings.Contains(string(encoded), "upgrade Peasant") || !strings.Contains(string(encoded), string(sid)) {
					t.Fatalf("warning lost actionable evidence: %s", encoded)
				}
			}
			after := readHarvestIndexSnapshot(t, db, sid)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("warning/current no-op changed last-good index")
			}
			currentLocations, err := db.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{sid})
			wantVersion := locations[sid].SchemaVersion
			if row.StoreVersion != 0 {
				wantVersion = row.StoreVersion
			}
			if err != nil || currentLocations[sid].SchemaVersion != wantVersion {
				t.Fatalf("retained store version changed: %+v %v", currentLocations, err)
			}
			current, err := os.ReadFile(metadataPath)
			if err != nil || !bytes.Equal(current, original) {
				t.Fatal("warning/current no-op replaced metadata")
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing diagnostic fixture %q", name)
		}
	}
}
