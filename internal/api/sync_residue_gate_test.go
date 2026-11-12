package api

import (
	"bytes"
	"errors"
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

// TestSyncIngestRefusesResidue proves a web-triggered ingest never runs the
// v76 whole-index rebuild inside the server: on an affected store the
// pipeline constructor refuses with the actionable fix instead of opening
// the database, and the database file is byte-identical afterwards.
func TestSyncIngestRefusesResidue(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	dbPath := defaults.ResolveDBFilePathWith(dataHome)
	if err := os.MkdirAll(filepath.Dir(string(dbPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	seedSyncResidueDB(t, string(dbPath), true)
	before, err := os.ReadFile(string(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	h := syncResidueHandler(t, dataHome, nil)
	built, err := h.newIngestPipeline(ingest.NewProgressState(), nil)
	if built != nil {
		// A pipeline was built, so the store was opened: release it before failing.
		built.release()
		t.Fatal("web ingest built a pipeline over an affected store; it must refuse before opening the database")
	}
	if err == nil {
		t.Fatal("web ingest opened an affected store; it must refuse instead of rebuilding in the server")
	}
	var refusal *store.SearchResidueRebuildError
	if !errors.As(err, &refusal) {
		t.Fatalf("web ingest error = %v; want the actionable residue refusal", err)
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
}

// TestSyncIngestProceedsWithoutResidue proves a clean store passes the gate:
// the constructor gets past the residue check (any later failure is
// unrelated to it).
func TestSyncIngestProceedsWithoutResidue(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	dbPath := defaults.ResolveDBFilePathWith(dataHome)
	if err := os.MkdirAll(filepath.Dir(string(dbPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	seedSyncResidueDB(t, string(dbPath), false)
	h := syncResidueHandler(t, dataHome, nil)
	built, err := h.newIngestPipeline(ingest.NewProgressState(), nil)
	if built != nil {
		defer built.release()
	}
	var refusal *store.SearchResidueRebuildError
	if errors.As(err, &refusal) {
		t.Fatalf("clean store refused: %v", err)
	}
}
