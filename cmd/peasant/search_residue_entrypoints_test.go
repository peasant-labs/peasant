package main

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/store/storetest/residuefixture"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_residue_entrypoints.yaml
var searchResidueEntrypointsYAML []byte

//go:embed testdata/search_residue_entrypoints.manifest.yaml
var searchResidueEntrypointsManifestYAML []byte

// searchResidueEntryCase is one gated entry point × store-state case: the
// opener to drive and the residue verdict it must reach. affected is a
// version-75 store with union residue (the open must be refused with the
// typed refusal; the database file and any sidecars stay byte-identical);
// clean is version-75 without residue (the gate must pass; a later open may
// still fail for unrelated missing-schema reasons on the minimal fixture,
// but never with the residue refusal); current is a real head-schema store
// (the open proceeds as usual); real-v75 is a real clean version-75 store
// (the open must succeed and migrate it to the build schema). confirm is the
// --confirm flag for the migrate and reclaim openers, which skip the gate
// when it is set. wantOpened requires err == nil and the build schema
// version afterwards.
type searchResidueEntryCase struct {
	Name       string `yaml:"name"`
	Entry      string `yaml:"entry"`
	Store      string `yaml:"store"`
	Confirm    bool   `yaml:"confirm,omitempty"`
	WantRefuse bool   `yaml:"wantRefuse"`
	WantOpened bool   `yaml:"wantOpened,omitempty"`
}

func loadSearchResidueEntryCases(t *testing.T) []searchResidueEntryCase {
	t.Helper()
	var fixtures struct {
		Cases []searchResidueEntryCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(searchResidueEntrypointsYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode search_residue_entrypoints.yaml: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decoder = yaml.NewDecoder(strings.NewReader(string(searchResidueEntrypointsManifestYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode search_residue_entrypoints manifest: %v", err)
	}
	seen := map[string]bool{}
	actualSeen := map[string]bool{}
	for _, c := range fixtures.Cases {
		actualSeen[c.Name] = true
		switch c.Entry {
		case "openDB", "openRunStore", "openVerifyContentStore", "openKickstartStore", "webPreopen", "runWebForeground", "ftueRunner", "runVerify", "openMigrateStore", "openReclaimStore":
		default:
			t.Fatalf("search_residue_entrypoints.yaml: case %q names entry %q; add it to the runner", c.Name, c.Entry)
		}
		switch c.Store {
		case "affected", "clean", "current", "real-v75":
		default:
			t.Fatalf("search_residue_entrypoints.yaml: case %q names store %q; add it to the runner", c.Name, c.Store)
		}
	}
	for _, name := range manifest.RequiredNames {
		seen[name] = true
		if !actualSeen[name] {
			t.Fatalf("search residue entrypoint required case %q is missing from the fixture", name)
		}
	}
	for name := range actualSeen {
		if !seen[name] {
			t.Fatalf("search residue entrypoint %q is missing from the required-names manifest", name)
		}
	}
	return fixtures.Cases
}

// seedEntrypointStore materializes the fixture store the matrix reads: a
// minimal v75 union/retired pair with (affected) or without (clean) residue,
// a real head-schema store (current), or a real clean v75 store (real-v75).
func seedEntrypointStore(t *testing.T, dir, kind string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, string(defaults.AppName)), 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	switch kind {
	case "affected":
		residuefixture.SeedAffected(t, dbPath)
	case "clean":
		residuefixture.SeedClean(t, dbPath)
	case "real-v75":
		storetest.BuildCleanV75Store(t, dbPath)
	case "current":
		db, err := store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown entrypoint fixture store %q", kind)
	}
	return dbPath
}

func entrypointTestCmd(t *testing.T, dir string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("data-dir", "", "")
	if err := cmd.Flags().Set("data-dir", dir); err != nil {
		t.Fatal(err)
	}
	cmd.SetContext(context.Background())
	return cmd
}

// sidecarSnapshot records the -wal/-shm sidecars beside dbPath so the matrix
// can prove a refused open leaves them absent or unchanged (real stores run
// in WAL mode; the minimal fixtures use rollback journals and carry none).
func sidecarSnapshot(t *testing.T, dbPath string) (wal, shm []byte, walPresent, shmPresent bool) {
	t.Helper()
	if data, err := os.ReadFile(dbPath + "-wal"); err == nil {
		wal, walPresent = data, true
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(dbPath + "-shm"); err == nil {
		shm, shmPresent = data, true
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return wal, shm, walPresent, shmPresent
}

func assertSidecarsUnchanged(t *testing.T, dbPath string, wal, shm []byte, walPresent, shmPresent bool) {
	t.Helper()
	afterWal, afterShm, afterWalPresent, afterShmPresent := sidecarSnapshot(t, dbPath)
	if afterWalPresent != walPresent {
		t.Errorf("refused open changed -wal presence (was %v, now %v)", walPresent, afterWalPresent)
	}
	if afterShmPresent != shmPresent {
		t.Errorf("refused open changed -shm presence (was %v, now %v)", shmPresent, afterShmPresent)
	}
	if walPresent && string(afterWal) != string(wal) {
		t.Error("refused open modified the -wal sidecar")
	}
	if shmPresent && string(afterShm) != string(shm) {
		t.Error("refused open modified the -shm sidecar")
	}
}

// TestGatedEntryPointsRefuseResidue drives every migrating open through the
// residue gate: on an affected store the open is refused before any migration
// and the database file plus sidecars are byte-identical afterwards;
// otherwise the gate lets the open through (a later failure, if any, is never
// the residue refusal). The TTY detector is stubbed false so the matrix is
// TTY-independent; terminal proceeds is covered by the gate unit tests plus
// the store-level Open tests that migrate real v75 stores.
func TestGatedEntryPointsRefuseResidue(t *testing.T) {
	previous := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = previous })
	for _, c := range loadSearchResidueEntryCases(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := seedEntrypointStore(t, dir, c.Store)
			var before []byte
			var wal, shm []byte
			var walPresent, shmPresent bool
			if c.WantRefuse {
				var err error
				before, err = os.ReadFile(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				wal, shm, walPresent, shmPresent = sidecarSnapshot(t, dbPath)
			}
			var err error
			switch c.Entry {
			case "openDB":
				var db *store.Store
				var closeDB func()
				db, closeDB, err = openDB(entrypointTestCmd(t, dir))
				if err == nil {
					closeDB()
					_ = db
				}
			case "openRunStore":
				var db *store.Store
				ownedRoot := t.TempDir()
				db, err = openRunStore(entrypointTestCmd(t, dir), false, ownedRoot, ingest.DefaultWriteConfig(1))
				if err == nil {
					_ = db.Close()
				}
			case "openVerifyContentStore":
				ownedRoot := t.TempDir()
				var closeDB func()
				_, closeDB, err = openVerifyContentStore(entrypointTestCmd(t, dir), ownedRoot, dbPath, false)
				if err == nil {
					closeDB()
				}
			case "openKickstartStore":
				ownedRoot := t.TempDir()
				var db *store.Store
				var closeDB func()
				db, closeDB, err = openKickstartStoreWithError(entrypointTestCmd(t, dir), ownedRoot)
				if err == nil && db != nil {
					closeDB()
				}
			case "webPreopen":
				err = maybePreopenStoreForWebStart(false, dbPath)
			case "runWebForeground":
				cfgPath := writeTestConfigFile(t, dir)
				cmd := entrypointTestCmd(t, dir)
				// Every case is bounded: a passing gate starts the server
				// (blocking) after the minimal-fixture open warns, and a
				// broken gate on the affected case would do the same, so
				// the short context turns either into a prompt return
				// instead of a hang until the test binary's deadline.
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				cmd.SetContext(ctx)
				err = runWebForeground(cmd, cfgPath, 0, false, "", false)
			case "ftueRunner":
				cfgPath := writeTestConfigFile(t, dir)
				runner, _ := buildFTUEIngestRunnerWithProgress(entrypointTestCmd(t, dir), cfgPath)
				_, err = runner(context.Background(), ftue.WizardAnswers{})
			case "runVerify":
				err = runVerify(entrypointTestCmd(t, dir), false, false, false)
			case "openMigrateStore":
				var closeDB func()
				_, closeDB, err = openMigrateStore(entrypointTestCmd(t, dir), false, c.Confirm, t.TempDir(), dbPath)
				if err == nil {
					closeDB()
				}
			case "openReclaimStore":
				var closeDB func()
				_, closeDB, err = openReclaimStore(entrypointTestCmd(t, dir), false, c.Confirm, t.TempDir(), dbPath)
				if err == nil {
					closeDB()
				}
			}
			var refusal *store.SearchResidueRebuildError
			if c.WantRefuse {
				if !errors.As(err, &refusal) {
					t.Fatalf("%s on affected store: err = %v; want the residue refusal", c.Entry, err)
				}
				after, readErr := os.ReadFile(dbPath)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if string(after) != string(before) {
					t.Errorf("%s refused but modified the database file", c.Entry)
				}
				assertSidecarsUnchanged(t, dbPath, wal, shm, walPresent, shmPresent)
				return
			}
			if errors.As(err, &refusal) {
				t.Fatalf("%s on %s store refused: %v", c.Entry, c.Store, err)
			}
			if c.WantOpened {
				if err != nil {
					t.Fatalf("%s on %s store: err = %v; want a clean open", c.Entry, c.Store, err)
				}
				version, verr := store.SchemaVersionAt(dbPath)
				if verr != nil {
					t.Fatal(verr)
				}
				if version != store.CurrentSchemaVersion() {
					t.Errorf("%s on %s store left user_version %d, want the build schema %d", c.Entry, c.Store, version, store.CurrentSchemaVersion())
				}
			}
		})
	}
}
