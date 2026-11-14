package api

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/store/storetest/residuefixture"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/server_behind_store.yaml
var serverBehindStoreYAML []byte

// seedServerBehindStore builds the named fixture store: a minimal schema-75
// store with (long) or without (quick) stale search results, or a real store
// at the head schema that still owes the turn count recount.
func seedServerBehindStore(t *testing.T, dbPath, kind string) {
	t.Helper()
	switch kind {
	case "long":
		residuefixture.SeedAffected(t, dbPath)
	case "quick":
		residuefixture.SeedClean(t, dbPath)
	case "owed-recount":
		storetest.BuildRecountOwedHeadStore(t, dbPath)
	default:
		t.Fatalf("server_behind_store.yaml names store %q; add it to seedServerBehindStore", kind)
	}
}

func syncResidueHandler(t *testing.T, dataHome string, cfg *config.Config) *syncHandler {
	t.Helper()
	if cfg == nil {
		cfg = config.BaseConfig()
	}
	return &syncHandler{config: newLiveConfig(cfg), dataHome: dataHome}
}

// TestServerImportsNeverUpgradeABehindStore runs every case of
// server_behind_store.yaml.
func TestServerImportsNeverUpgradeABehindStore(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name  string `yaml:"name"`
			Entry string `yaml:"entry"`
			Store string `yaml:"store"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(serverBehindStoreYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("server_behind_store.yaml", "cases", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dataHome := t.TempDir()
			dbPath := string(defaults.ResolveDBFilePathWith(dataHome))
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
				t.Fatal(err)
			}
			seedServerBehindStore(t, dbPath, c.Store)
			before, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			h := syncResidueHandler(t, dataHome, nil)
			switch c.Entry {
			case "pipeline":
				built, buildErr := h.newIngestPipeline(ingest.NewProgressState(), nil)
				if built != nil {
					// A pipeline was built, so the store was opened: release it before failing.
					built.release()
					t.Fatal("web ingest built a pipeline over a behind store; a server must refuse instead of upgrading")
				}
				err = buildErr
			case "kickstart":
				start, launch, claimErr := h.reserveIngest(true)
				if launch != nil || start.AlreadyRunning {
					t.Fatalf("the kickstart claimed an import over a behind store: start=%+v", start)
				}
				h.mu.Lock()
				status := h.ingestStatus
				h.mu.Unlock()
				if status == "running" {
					t.Fatal("a refused kickstart import left the import marked running")
				}
				err = claimErr
			default:
				t.Fatalf("server_behind_store.yaml: case %q names entry %q; add it to the runner", c.Name, c.Entry)
			}
			if !store.IsSchemaBehind(err) {
				t.Fatalf("%s on the %s store: error = %v; want the behind-store refusal", c.Entry, c.Store, err)
			}
			for _, fragment := range residuefixture.RefusalFragments {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("refusal message misses %q:\n%s", fragment, err)
				}
			}
			after, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("the refused server import modified the database file")
			}
			for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
				if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
					t.Errorf("the refused server import left %s behind", sidecar)
				}
			}
		})
	}
}
