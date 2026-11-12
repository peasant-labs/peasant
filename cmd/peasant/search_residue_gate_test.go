package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
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

func loadSearchResidueGateCases(t *testing.T) []searchResidueGateCase {
	t.Helper()
	var fixtures struct {
		Cases []searchResidueGateCase `yaml:"cases"`
	}
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
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
		switch c.DB {
		case "affected", "clean", "current", "missing":
		default:
			t.Fatalf("search_residue_gate.yaml: case %q names store %q; add it to the runner", c.Name, c.DB)
		}
	}
	seen := map[string]bool{}
	for _, name := range manifest.RequiredNames {
		seen[name] = true
	}
	if len(actual) != len(manifest.RequiredNames) {
		t.Fatalf("search residue gate manifest lists %d names but the fixture carries %d", len(manifest.RequiredNames), len(actual))
	}
	for _, name := range actual {
		if !seen[name] {
			t.Fatalf("search residue gate case %q is missing from the required-names manifest", name)
		}
	}
	return fixtures.Cases
}

// seedGateFixtureStore builds the fixture store the gate matrix reads: a
// version-75 union/retired pair with or without residue, or a current-schema
// store. It reuses the shared storetest residue builder so the gate tests
// pin the same predicate the store tests pin.
func seedGateFixtureStore(t *testing.T, kind string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate.db")
	var version int
	var union, legacy []storetest.ResidueTerm
	switch kind {
	case "affected":
		version = 75
		union = []storetest.ResidueTerm{{ID: 1, Text: "kangaroo ghost"}, {ID: 2, Text: "keep me"}}
		legacy = []storetest.ResidueTerm{{ID: 2, Text: "keep me"}}
	case "clean":
		version = 75
		union = []storetest.ResidueTerm{{ID: 1, Text: "keep me"}, {ID: 2, Text: "steady state"}}
		legacy = []storetest.ResidueTerm{{ID: 1, Text: "keep me"}, {ID: 2, Text: "steady state"}}
	case "current":
		version = 76
	case "missing":
		return path
	default:
		t.Fatalf("unknown gate fixture store %q", kind)
	}
	storetest.SeedResidueDB(t, path, version, union, legacy)
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
			for _, fragment := range []string{"peasant harvest", "web start", "0.16.1"} {
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
		if err := os.MkdirAll(filepath.Join(dir, "peasant"), 0o700); err != nil {
			t.Fatal(err)
		}
		union := []storetest.ResidueTerm{{ID: 1, Text: "kangaroo ghost"}, {ID: 2, Text: "keep me"}}
		legacy := []storetest.ResidueTerm{{ID: 2, Text: "keep me"}}
		storetest.SeedResidueDB(t, resolveDB(dir), 75, union, legacy)
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
		if err := os.MkdirAll(filepath.Join(dir, "peasant"), 0o700); err != nil {
			t.Fatal(err)
		}
		// A current-schema store opens as usual; the refusal never fires.
		seed, err := store.Open(resolveDB(dir))
		if err != nil {
			t.Fatal(err)
		}
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := openAt(t, dir, false)
		if err != nil {
			t.Fatalf("clean store refused: %v", err)
		}
		_ = db.Close()
	})

	t.Run("current-terminal-opens", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "peasant"), 0o700); err != nil {
			t.Fatal(err)
		}
		seed, err := store.Open(resolveDB(dir))
		if err != nil {
			t.Fatal(err)
		}
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := openAt(t, dir, true)
		if err != nil {
			t.Fatalf("terminal open of clean store failed: %v", err)
		}
		_ = db.Close()
	})
}

// TestPushSkipsResidueWithExitZero drives the managed-hook path end to end:
// `village push --non-interactive` on an affected store skips the publish
// with the actionable message and exits 0 instead of blocking the push.
// Credentials are stubbed (the gate fires before any village request), no
// network is touched, and the database file is byte-identical afterwards.
func TestPushSkipsResidueWithExitZero(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "peasant"), 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	union := []storetest.ResidueTerm{{ID: 1, Text: "kangaroo ghost"}, {ID: 2, Text: "keep me"}}
	legacy := []storetest.ResidueTerm{{ID: 2, Text: "keep me"}}
	storetest.SeedResidueDB(t, dbPath, 75, union, legacy)
	writeTestCredentials(t, dir)
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, stderr, err := executeWithDataDirStreams(t, BuildPushCommand(), dir, []string{"--non-interactive"})
	if err != nil {
		t.Fatalf("hook push on affected store failed: %v\nstderr: %s", err, stderr)
	}
	for _, fragment := range []string{"0.16.1", "peasant harvest", "no files were changed"} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("hook output misses %q:\n%s", fragment, stderr)
		}
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("skipped hook push modified the database file")
	}
	if version, err := store.SchemaVersionAt(dbPath); err != nil || version != 75 {
		t.Errorf("store version = %d, err = %v; want the un-migrated 75", version, err)
	}
}

// TestPushSkipForSearchResidue maps the residue refusal to the hook contract:
// skip the publish with the actionable message and exit 0. Any other error
// passes through unchanged.
func TestPushSkipForSearchResidue(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	if skipped := pushSkipForSearchResidue(cmd, &store.SearchResidueRebuildError{DBPath: "/tmp/x.db"}); !skipped {
		t.Fatal("residue refusal must skip the push")
	}
	if !strings.Contains(errBuf.String(), "peasant harvest") {
		t.Errorf("skip must print the fix; got: %s", errBuf.String())
	}
	sentinel := errors.New("boom")
	if skipped := pushSkipForSearchResidue(cmd, sentinel); skipped {
		t.Fatal("ordinary errors must not skip the push")
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
		if err := os.MkdirAll(filepath.Join(dir, "peasant"), 0o700); err != nil {
			t.Fatal(err)
		}
		dbPath := string(defaults.ResolveDBFilePathWith(dir))
		union := []storetest.ResidueTerm{{ID: 1, Text: "kangaroo ghost"}, {ID: 2, Text: "keep me"}}
		legacy := []storetest.ResidueTerm{{ID: 2, Text: "keep me"}}
		storetest.SeedResidueDB(t, dbPath, 75, union, legacy)
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
		if err := os.MkdirAll(filepath.Join(dir, "peasant"), 0o700); err != nil {
			t.Fatal(err)
		}
		dbPath := string(defaults.ResolveDBFilePathWith(dir))
		union := []storetest.ResidueTerm{{ID: 1, Text: "keep me"}}
		storetest.SeedResidueDB(t, dbPath, 75, union, union)
		if err := maybePreopenStoreForWebStart(false, dbPath); err != nil {
			t.Fatalf("clean pre-open refused: %v", err)
		}
	})
}
