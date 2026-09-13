package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/web_harvest_registry.yaml
var webHarvestRegistryYAML []byte

// This drives the same runner the mounted ingest handler dispatches, with real
// synthetic Codex input and SQLite. Codex was absent from the old web wiring.
func TestWebHarvestCanonicalRegistry(t *testing.T) {
	var fixture struct {
		Harnesses  []ingest.Harness `yaml:"harnesses"`
		SessionID  ingest.SessionID `yaml:"sessionID"`
		Filename   string           `yaml:"filename"`
		Transcript string           `yaml:"transcript"`
		StatusKeys []string         `yaml:"statusKeys"`
	}
	if err := yaml.Unmarshal(webHarvestRegistryYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv(defaults.EnvXDGDataHome.String(), filepath.Join(root, "data"))
	t.Setenv(defaults.EnvXDGConfigHome.String(), filepath.Join(root, "config"))
	t.Setenv(defaults.EnvXDGStateHome.String(), filepath.Join(root, "state"))
	cfg := config.BaseConfig()
	cfg.Output.BasePath = filepath.Join(root, "managed")
	for _, harness := range fixture.Harnesses {
		provider, ok := cfg.Sources.Provider(harness)
		if !ok {
			t.Fatalf("missing configured harness %s", harness)
		}
		provider.Enabled = true
		provider.Paths = []string{filepath.Join(root, string(harness))}
	}
	if got := slices.Sorted(maps.Keys(buildWebSourceConfigs(cfg))); !slices.Equal(got, fixture.Harnesses) {
		t.Fatalf("web source registry = %v, want %v", got, fixture.Harnesses)
	}
	for _, harness := range fixture.Harnesses {
		provider, _ := cfg.Sources.Provider(harness)
		provider.Enabled = harness == ingest.HarnessCodex
	}
	source := filepath.Join(root, string(ingest.HarnessCodex))
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "2024", "02", "19", fixture.Filename)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fixture.Transcript), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1708300800, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	handler := &syncHandler{config: cfg}
	handler.runIngestPipeline(ingest.NewProgressState())
	if handler.ingestError != nil {
		t.Fatalf("web ingest: %s", handler.ingestError)
	}
	if handler.ingestResult == nil || handler.ingestResult.Summary.Indexed != 1 || !maps.Equal(handler.ingestResult.Summary.HarvesterVersions, ingest.HarvesterVersionRegistry) {
		t.Fatalf("populated web ingest result = %+v", handler.ingestResult)
	}
	db, err := store.Open(string(defaults.ResolveDBFilePath()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	entries, err := db.ListEntries(t.Context(), fixture.SessionID)
	if err != nil || len(entries) == 0 {
		t.Fatalf("real web index entries=%d error=%v", len(entries), err)
	}
	response := httptest.NewRecorder()
	handler.handleSyncIngestStatus(response, httptest.NewRequest("GET", "/api/v1/sync/ingest/status", nil))
	var status struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(status.Result)); !slices.Equal(got, fixture.StatusKeys) {
		t.Fatalf("HTTP result keys = %v, want unchanged %v", got, fixture.StatusKeys)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, []byte(fixture.Transcript)) {
		t.Fatalf("native web fixture changed: %v", err)
	}
}
