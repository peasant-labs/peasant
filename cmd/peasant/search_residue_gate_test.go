package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/store/storetest/residuefixture"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_residue_gate.yaml
var searchResidueGateYAML []byte

//go:embed testdata/search_residue_gate.manifest.yaml
var searchResidueGateManifestYAML []byte

// searchResidueGateCase is one gate case: the terminal shape and the fixture
// store to open against. affected is a version-75 store with union residue,
// clean is version-75 without residue, current is already migrated, missing
// is a fresh install with no file.
type searchResidueGateCase struct {
	Name       string `yaml:"name"`
	IsTTY      bool   `yaml:"isTTY"`
	DB         string `yaml:"db"`
	WantRefuse bool   `yaml:"wantRefuse"`
}

// searchResiduePushCase is one push caller on an affected store; caller
// selects the argument list the runner builds.
type searchResiduePushCase struct {
	Name   string `yaml:"name"`
	Caller string `yaml:"caller"`
}

type searchResidueGateFixtures struct {
	Cases     []searchResidueGateCase `yaml:"cases"`
	PushCases []searchResiduePushCase `yaml:"pushCases"`
}

func loadSearchResidueGateCases(t *testing.T) []searchResidueGateCase {
	t.Helper()
	return loadSearchResidueGateFixtures(t).Cases
}

func loadSearchResidueGateFixtures(t *testing.T) searchResidueGateFixtures {
	t.Helper()
	var fixtures searchResidueGateFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(searchResidueGateYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode search_residue_gate.yaml: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decoder = yaml.NewDecoder(strings.NewReader(string(searchResidueGateManifestYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode search_residue_gate manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases)+len(fixtures.PushCases))
	actualSeen := map[string]bool{}
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
		actualSeen[c.Name] = true
		switch c.DB {
		case "affected", "clean", "current", "missing":
		default:
			t.Fatalf("search_residue_gate.yaml: case %q names store %q; add it to the runner", c.Name, c.DB)
		}
	}
	for _, c := range fixtures.PushCases {
		actual = append(actual, c.Name)
		actualSeen[c.Name] = true
		switch c.Caller {
		case "hook-with-rule", "hook-without-rule", "script":
		default:
			t.Fatalf("search_residue_gate.yaml: push case %q names caller %q; add it to the runner", c.Name, c.Caller)
		}
	}
	for _, name := range manifest.RequiredNames {
		if !actualSeen[name] {
			t.Fatalf("search residue gate required case %q is missing from the fixture", name)
		}
	}
	manifestSeen := map[string]bool{}
	for _, name := range manifest.RequiredNames {
		manifestSeen[name] = true
	}
	for _, name := range actual {
		if !manifestSeen[name] {
			t.Fatalf("search residue gate case %q is missing from the required-names manifest", name)
		}
	}
	return fixtures
}

// seedGateFixtureStore builds the fixture store the gate matrix reads: a
// version-75 union/retired pair with or without residue, or a current-schema
// minimal store. It reuses the shared residue fixture so the gate tests pin
// the same predicate the store tests pin.
func seedGateFixtureStore(t *testing.T, kind string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate.db")
	switch kind {
	case "affected":
		residuefixture.SeedAffected(t, path)
	case "clean":
		residuefixture.SeedClean(t, path)
	case "current":
		residuefixture.Seed(t, path, store.CurrentSchemaVersion(), nil, nil)
	case "missing":
	default:
		t.Fatalf("unknown gate fixture store %q", kind)
	}
	return path
}

// TestSearchResidueGate drives the non-interactive guard every migrating open
// funnels through: a terminal always proceeds, a non-terminal proceeds
// unless the store carries v75 residue, and a missing store proceeds. The
// TTY detector is stubbed; production uses stdin.
func TestSearchResidueGate(t *testing.T) {
	for _, c := range loadSearchResidueGateCases(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			previous := stdinIsTerminal
			stdinIsTerminal = func() bool { return c.IsTTY }
			t.Cleanup(func() { stdinIsTerminal = previous })
			path := seedGateFixtureStore(t, c.DB)
			err := refuseStaleSearchIndex(path)
			if !c.WantRefuse {
				if err != nil {
					t.Fatalf("gate refused: %v", err)
				}
				return
			}
			var refusal *store.SearchResidueRebuildError
			if !errors.As(err, &refusal) {
				t.Fatalf("gate error = %v; want the actionable residue refusal", err)
			}
			if refusal.DBPath != path {
				t.Errorf("refusal names %q, want the store that was checked", refusal.DBPath)
			}
			for _, fragment := range residuefixture.RefusalFragments {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("refusal message misses %q:\n%s", fragment, err)
				}
			}
		})
	}
}

// TestSearchResidueGateDoesNotWrite proves the gate never modifies the store
// it inspects: the database file hashes identically before and after every
// decision in the matrix.
func TestSearchResidueGateDoesNotWrite(t *testing.T) {
	previous := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = previous })
	for _, kind := range []string{"affected", "clean", "current"} {
		path := seedGateFixtureStore(t, kind)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_ = refuseStaleSearchIndex(path)
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Errorf("gate modified the %s store; the read-only check must never write", kind)
		}
	}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestOpenRunStoreRefusesResidue proves harvest's and push's shared opener
// refuses an affected store instead of migrating it: the typed refusal
// leaves the file byte-identical, a clean store opens as usual, and a
// terminal proceeds (migrating the affected store visibly, as the store
// tests pin end to end).
func TestOpenRunStoreRefusesResidue(t *testing.T) {
	newDataCmd := func(dir string) *cobra.Command {
		t.Helper()
		cmd := &cobra.Command{}
		cmd.Flags().String("data-dir", "", "")
		if err := cmd.Flags().Set("data-dir", dir); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	openAt := func(t *testing.T, dir string, tty bool) (*store.Store, error) {
		t.Helper()
		previous := stdinIsTerminal
		stdinIsTerminal = func() bool { return tty }
		t.Cleanup(func() { stdinIsTerminal = previous })
		ownedRoot := t.TempDir()
		return openRunStore(newDataCmd(dir), false, ownedRoot, ingest.DefaultWriteConfig(1))
	}
	resolveDB := func(dir string) string {
		return string(defaults.ResolveDBFilePathWith(dir))
	}

	t.Run("affected-nonterminal-refuses-unchanged", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, string(defaults.AppName)), 0o700); err != nil {
			t.Fatal(err)
		}
		residuefixture.SeedAffected(t, resolveDB(dir))
		before := fileHash(t, resolveDB(dir))
		_, err := openAt(t, dir, false)
		var refusal *store.SearchResidueRebuildError
		if !errors.As(err, &refusal) {
			t.Fatalf("openRunStore err = %v; want the residue refusal", err)
		}
		if got := fileHash(t, resolveDB(dir)); got != before {
			t.Error("refused open modified the database file")
		}
	})

	t.Run("clean-nonterminal-opens", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, string(defaults.AppName)), 0o700); err != nil {
			t.Fatal(err)
		}
		// A current-schema store opens as usual; the refusal never fires.
		storetest.CopyGoldenTo(t, resolveDB(dir))
		db, err := openAt(t, dir, false)
		if err != nil {
			t.Fatalf("clean store refused: %v", err)
		}
		_ = db.Close()
	})

	t.Run("current-terminal-opens", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, string(defaults.AppName)), 0o700); err != nil {
			t.Fatal(err)
		}
		storetest.CopyGoldenTo(t, resolveDB(dir))
		db, err := openAt(t, dir, true)
		if err != nil {
			t.Fatalf("terminal open of clean store failed: %v", err)
		}
		_ = db.Close()
	})
}

// pushResidueArgs builds the argument list one push caller runs. The hook
// callers come from the real hook generator, so the test runs exactly the
// flags a generated hook passes; the leading program and subcommand words
// are dropped because the test root mounts the push command directly.
func pushResidueArgs(t *testing.T, caller string) []string {
	t.Helper()
	var binding githooks.Binding
	switch caller {
	case "script":
		return []string{"--non-interactive"}
	case "hook-with-rule":
		binding = githooks.Binding{RequireAutoPublishRule: true, AutoPublishEvent: githooks.EventPostCommit}
	case "hook-without-rule":
		binding = githooks.Binding{}
	default:
		t.Fatalf("unknown push caller %q", caller)
	}
	argv := githooks.RepositoryArgv("", binding)
	if len(argv) < 3 || argv[0] != "peasant" || argv[1] != "village" || argv[2] != "push" {
		t.Fatalf("generated hook argv = %q; want it to start with peasant village push", argv)
	}
	return argv[3:]
}

// TestPushResidueRefusalIsNothingAttempted drives every push caller on an
// affected store: a generated hook with an auto-publish rule, a generated
// hook without one, and a script. Each is refused before any village
// request with the residue refusal wrapped as nothing attempted, so the
// process exits with the nothing-attempted status a generated hook maps to
// "nothing reached peasant online". Credentials are stubbed (the gate fires
// before any request), no network is touched, the database file is
// byte-identical, and the store stays at version 75. The TTY detector is
// stubbed false so the test is TTY-independent.
func TestPushResidueRefusalIsNothingAttempted(t *testing.T) {
	previous := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = previous })
	for _, c := range loadSearchResidueGateFixtures(t).PushCases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, string(defaults.AppName)), 0o700); err != nil {
				t.Fatal(err)
			}
			dbPath := string(defaults.ResolveDBFilePathWith(dir))
			residuefixture.SeedAffected(t, dbPath)
			writeTestCredentials(t, dir)
			before := fileHash(t, dbPath)
			_, _, err := executeWithDataDirStreams(t, BuildPushCommand(), dir, pushResidueArgs(t, c.Caller))
			if err == nil {
				t.Fatal("push on an affected store succeeded; want the residue refusal")
			}
			var nothingAttempted *pushNothingAttemptedError
			if !errors.As(err, &nothingAttempted) {
				t.Fatalf("push err = %v; want it marked nothing attempted", err)
			}
			var refusal *store.SearchResidueRebuildError
			if !errors.As(err, &refusal) {
				t.Fatalf("push err = %v; want the residue refusal as its cause", err)
			}
			if got := exitCodeFor(err); got != defaults.ExitNothingAttempted {
				t.Errorf("exit code = %d, want the nothing-attempted status %d", got, defaults.ExitNothingAttempted)
			}
			for _, fragment := range residuefixture.RefusalFragments {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("refusal misses %q:\n%s", fragment, err)
				}
			}
			if got := fileHash(t, dbPath); got != before {
				t.Error("refused push modified the database file")
			}
			if version, verr := store.SchemaVersionAt(dbPath); verr != nil || version != 75 {
				t.Errorf("store version = %d, err = %v; want the un-migrated 75", version, verr)
			}
		})
	}
}

// TestMaybePreopenStoreForWebStart covers the `web start` parent contract:
// on a terminal it pre-opens (migrating an affected store visibly before
// forking the background child), while a non-interactive start refuses an
// affected store instead of forking a server that could never start.
func TestMaybePreopenStoreForWebStart(t *testing.T) {
	seedAffected := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, string(defaults.AppName)), 0o700); err != nil {
			t.Fatal(err)
		}
		dbPath := string(defaults.ResolveDBFilePathWith(dir))
		residuefixture.SeedAffected(t, dbPath)
		return dbPath
	}

	t.Run("terminal-affected-attempts-open", func(t *testing.T) {
		dbPath := seedAffected(t)
		// The parent always opens on a terminal; the open itself (and the
		// visible rebuild of a real v75 store) is proven in the store
		// package. Here the minimal fixture cannot migrate, so the
		// assertion is that the parent does not refuse: it attempts the
		// open and warns instead of skipping.
		if err := maybePreopenStoreForWebStart(true, dbPath); err != nil {
			t.Fatalf("terminal pre-open refused: %v", err)
		}
	})

	t.Run("nonterminal-affected-refuses", func(t *testing.T) {
		dbPath := seedAffected(t)
		before := fileHash(t, dbPath)
		err := maybePreopenStoreForWebStart(false, dbPath)
		var refusal *store.SearchResidueRebuildError
		if !errors.As(err, &refusal) {
			t.Fatalf("non-terminal pre-open err = %v; want the residue refusal", err)
		}
		if got := fileHash(t, dbPath); got != before {
			t.Error("refused pre-open modified the database file")
		}
	})

	t.Run("nonterminal-clean-proceeds", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, string(defaults.AppName)), 0o700); err != nil {
			t.Fatal(err)
		}
		dbPath := string(defaults.ResolveDBFilePathWith(dir))
		residuefixture.SeedClean(t, dbPath)
		if err := maybePreopenStoreForWebStart(false, dbPath); err != nil {
			t.Fatalf("clean pre-open refused: %v", err)
		}
	})
}
