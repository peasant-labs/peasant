package store

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_residue_gate.yaml
var searchResidueGateYAML []byte

//go:embed testdata/search_residue_gate.manifest.yaml
var searchResidueGateManifestYAML []byte

// searchResidueGateTerm is one indexed row the fixture seeds into a docsize
// table: the id/rowid and the text whose token count the predicate compares.
type searchResidueGateTerm struct {
	ID   int64  `yaml:"id"`
	Text string `yaml:"text"`
}

// searchResidueGateCase is one read-only helper case: the user_version to
// stamp, the union and legacy postings to seed, and the expected verdict.
// A missing file proves a fresh install proceeds.
type searchResidueGateCase struct {
	Name        string                  `yaml:"name"`
	Version     int                     `yaml:"version"`
	Missing     bool                    `yaml:"missing,omitempty"`
	UnionTerms  []searchResidueGateTerm `yaml:"unionTerms"`
	LegacyTerms []searchResidueGateTerm `yaml:"legacyTerms"`
	WantResidue bool                    `yaml:"wantResidue"`
}

type searchResidueGateFixtures struct {
	Cases              []searchResidueGateCase `yaml:"cases"`
	MessageMustContain []string                `yaml:"messageMustContain"`
}

func loadSearchResidueGateFixtures(t *testing.T) searchResidueGateFixtures {
	t.Helper()
	var decoded searchResidueGateFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(searchResidueGateYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode search_residue_gate.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(searchResidueGateManifestYAML)
	if err != nil {
		t.Fatalf("decode search_residue_gate manifest: %v", err)
	}
	actual := make([]string, 0, len(decoded.Cases))
	for _, c := range decoded.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "search residue gate"); err != nil {
		t.Fatal(err)
	}
	if len(decoded.MessageMustContain) == 0 {
		t.Fatal("search_residue_gate.yaml carries no messageMustContain fragments; the refusal message is unpinned")
	}
	return searchResidueGateFixtures{Cases: decoded.Cases, MessageMustContain: decoded.MessageMustContain}
}

// seedSearchResidueDB builds the minimal store the helper reads: a stamped
// user_version plus the two FTS docsize tables seeded from the fixture rows.
// Only what NeedsSearchResidueRebuild inspects exists, so the cases pin the
// gate and nothing else.
func seedSearchResidueDB(t *testing.T, path string, c searchResidueGateCase) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close residue fixture database: %v", err)
		}
	}()
	seedTerms := func(table string, terms []searchResidueGateTerm) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, `CREATE VIRTUAL TABLE `+table+` USING fts5(t)`, nil); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
		for _, term := range terms {
			if err := sqlitex.ExecuteTransient(conn, `INSERT INTO `+table+`(rowid, t) VALUES (?, ?)`, &sqlitex.ExecOptions{
				Args: []any{term.ID, term.Text},
			}); err != nil {
				t.Fatalf("seed %s row %d: %v", table, term.ID, err)
			}
		}
	}
	seedTerms("session_search_fts", c.UnionTerms)
	seedTerms("session_entries_fts", c.LegacyTerms)
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf(`PRAGMA user_version = %d`, c.Version), nil); err != nil {
		t.Fatalf("stamp user_version %d: %v", c.Version, err)
	}
}

// fileHash returns the SHA-256 of the named file for the never-writes proof.
func fileHash(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return sum[:]
}

// TestNeedsSearchResidueRebuild pins the read-only pre-open check against the
// vendored SQLite: the version gate first (74, 76, and missing files proceed
// without touching the predicate), then the residue verdict per window. Every
// case also proves the helper never writes: the database file hashes
// identically before and after, and no -wal/-shm sidecars appear (the minimal
// fixtures use rollback journals).
func TestNeedsSearchResidueRebuild(t *testing.T) {
	t.Parallel()
	fixtures := loadSearchResidueGateFixtures(t)
	for _, c := range fixtures.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.Missing {
				path := filepath.Join(t.TempDir(), "absent.db")
				got, err := NeedsSearchResidueRebuild(path)
				if err != nil {
					t.Fatal(err)
				}
				if got {
					t.Error("missing database reports residue; a fresh install must proceed")
				}
				return
			}
			path := filepath.Join(t.TempDir(), "residue.db")
			seedSearchResidueDB(t, path, c)
			before := fileHash(t, path)
			got, err := NeedsSearchResidueRebuild(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.WantResidue {
				t.Errorf("NeedsSearchResidueRebuild = %v, want %v", got, c.WantResidue)
			}
			if after := fileHash(t, path); string(after) != string(before) {
				t.Error("the residue check modified the database file; the read-only probe must never write")
			}
			for _, sidecar := range []string{path + "-wal", path + "-shm"} {
				if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
					t.Errorf("read-only check left %s behind; immutable=1 must prevent sidecars", sidecar)
				}
			}
		})
	}
}

// TestNeedsSearchResidueRebuildWALMode proves the read-only check leaves WAL
// sidecars untouched: real stores run in WAL mode, so the fixture switches a
// seeded affected store to WAL, records both sidecars, runs the check, and
// asserts residue true with byte-identical file and sidecars.
func TestNeedsSearchResidueRebuildWALMode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "residue-wal.db")
	seedSearchResidueDB(t, path, searchResidueGateCase{
		Name:        "wal-affected",
		Version:     75,
		UnionTerms:  []searchResidueGateTerm{{ID: 1, Text: "kangaroo ghost"}, {ID: 2, Text: "keep me"}},
		LegacyTerms: []searchResidueGateTerm{{ID: 2, Text: "keep me"}},
		WantResidue: true,
	})
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA journal_mode=WAL`, nil); err != nil {
		_ = conn.Close()
		t.Fatalf("switch fixture to WAL: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileHash(t, path)
	var walBefore, shmBefore []byte
	walPresent, shmPresent := false, false
	if data, err := os.ReadFile(path + "-wal"); err == nil {
		walBefore, walPresent = data, true
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path + "-shm"); err == nil {
		shmBefore, shmPresent = data, true
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	got, err := NeedsSearchResidueRebuild(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("WAL affected store reports no residue; want true")
	}
	if after := fileHash(t, path); string(after) != string(before) {
		t.Error("WAL check modified the database file")
	}
	if data, err := os.ReadFile(path + "-wal"); err == nil {
		if !walPresent || string(data) != string(walBefore) {
			t.Error("WAL check modified the -wal sidecar")
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	} else if walPresent {
		t.Error("WAL check removed the -wal sidecar")
	}
	if data, err := os.ReadFile(path + "-shm"); err == nil {
		if !shmPresent || string(data) != string(shmBefore) {
			t.Error("WAL check modified the -shm sidecar")
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	} else if shmPresent {
		t.Error("WAL check removed the -shm sidecar")
	}
}

// TestSearchResidueRefusalMessage pins the six-part refusal every gated entry
// point prints or returns: what failed, why, where, when, meaning, and the
// terminal fix. The fragments live in the fixture so the message stays
// reviewable as data.
func TestSearchResidueRefusalMessage(t *testing.T) {
	t.Parallel()
	fixtures := loadSearchResidueGateFixtures(t)
	msg := (&SearchResidueRebuildError{DBPath: "/synthetic/peasant.db"}).Error()
	for _, fragment := range fixtures.MessageMustContain {
		if !strings.Contains(msg, fragment) {
			t.Errorf("refusal message misses %q:\n%s", fragment, msg)
		}
	}
}
