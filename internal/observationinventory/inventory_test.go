package observationinventory_test

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/observationinventory"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/held.yaml
var heldYAML []byte

func TestHeldInventoryUsesFullManifestScopeWithoutContentReads(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name        string `yaml:"name"`
			URL         string `yaml:"url"`
			Mismatch    bool   `yaml:"mismatch"`
			WantPresent bool   `yaml:"wantPresent"`
			WantError   bool   `yaml:"wantError"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(heldYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if names[c.Name] {
			t.Fatal("duplicate held inventory case")
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("held inventory case %s missing", name)
		}
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			db := storetest.Open(t)
			dir := t.TempDir()
			const id schema.TranscriptID = "22222222-2222-4222-8222-222222222222"
			manifest := pull.PullManifest{ManifestVersion: pull.PullManifestVersion, VillageURL: c.URL, VillageHost: "village.invalid", TranscriptID: id}
			if c.Mismatch {
				manifest.TranscriptID = "33333333-3333-4333-8333-333333333333"
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, pull.ManifestFilename), raw, 0600); err != nil {
				t.Fatal(err)
			}
			// This body cannot be read as a file. Metadata inventory must not need it.
			if err := os.Mkdir(filepath.Join(dir, pull.TranscriptFilename), 0700); err != nil {
				t.Fatal(err)
			}
			if err := db.CommitPull(t.Context(), store.PullCommit{Transcript: store.PulledTranscriptRow{VillageHost: "village.invalid", TranscriptID: id, OwnerUserID: "44444444-4444-4444-8444-444444444444", OwnerUsername: "fixture", Harness: schema.HarnessClaudeCode, ContentHash: strings.Repeat("a", 64), Visibility: schema.VisibilityPublic, PullDir: dir, FirstPulledAt: 1, LastPulledAt: 1}}); err != nil {
				t.Fatal(err)
			}
			scope, err := schema.NormalizeCollectiveObservationScope("https://village.invalid/one", "11111111-1111-4111-8111-111111111111")
			if err != nil {
				t.Fatal(err)
			}
			ids, err := observationinventory.Held(db)(t.Context(), scope)
			if (err != nil) != c.WantError {
				t.Fatalf("inventory err=%v wantError=%v", err, c.WantError)
			}
			if err == nil {
				if c.WantPresent && (len(ids) != 1 || ids[0] != id) {
					t.Fatalf("held ID missing: %v", ids)
				}
				if !c.WantPresent && len(ids) != 0 {
					t.Fatalf("cross-installation ID leaked: %v", ids)
				}
			}
		})
	}
}
