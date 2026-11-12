package api

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest/residuefixture"
)

// seedSyncResidueDB builds the fixture store the web ingest gate reads: a
// version-75 union/retired pair with residue when affected is true, and the
// same pair with identical postings otherwise. It reuses the shared residue
// fixture so the sync tests pin the same predicate the store tests pin.
func seedSyncResidueDB(t *testing.T, dbPath string, affected bool) {
	t.Helper()
	if affected {
		residuefixture.SeedAffected(t, dbPath)
		return
	}
	residuefixture.SeedClean(t, dbPath)
}

func syncResidueHandler(t *testing.T, dataHome string, cfg *config.Config) *syncHandler {
	t.Helper()
	if cfg == nil {
		cfg = config.BaseConfig()
	}
	return &syncHandler{config: newLiveConfig(cfg), dataHome: dataHome}
}

// TestSyncIngestNeverUpgradesABehindStore proves a server-triggered import
// never upgrades the store: on a behind store, with or without stale search
// results, the pipeline constructor refuses with the command that runs the
// upgrade, and the database file is byte-identical afterwards.
func TestSyncIngestNeverUpgradesABehindStore(t *testing.T) {
	t.Parallel()
	t.Run("long-step", func(t *testing.T) { assertSyncIngestRefusesBehindStore(t, true) })
	t.Run("quick-step", func(t *testing.T) { assertSyncIngestRefusesBehindStore(t, false) })
}

func assertSyncIngestRefusesBehindStore(t *testing.T, affected bool) {
	t.Helper()
	t.Parallel()
	dataHome := t.TempDir()
	dbPath := defaults.ResolveDBFilePathWith(dataHome)
	if err := os.MkdirAll(filepath.Dir(string(dbPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	seedSyncResidueDB(t, string(dbPath), affected)
	before, err := os.ReadFile(string(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	h := syncResidueHandler(t, dataHome, nil)
	built, err := h.newIngestPipeline(ingest.NewProgressState(), nil)
	if built != nil {
		// A pipeline was built, so the store was opened: release it before failing.
		built.release()
		t.Fatal("web ingest built a pipeline over a behind store; a server must refuse instead of upgrading")
	}
	if err == nil {
		t.Fatal("web ingest opened a behind store; a server must refuse instead of upgrading")
	}
	if !store.IsSchemaBehind(err) {
		t.Fatalf("web ingest error = %v; want the behind-store refusal", err)
	}
	for _, fragment := range residuefixture.RefusalFragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("refusal message misses %q:\n%s", fragment, err)
		}
	}
	after, err := os.ReadFile(string(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("refused web ingest modified the database file")
	}
	for _, sidecar := range []string{string(dbPath) + "-wal", string(dbPath) + "-shm"} {
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Errorf("refused web ingest left %s behind", sidecar)
		}
	}
}
