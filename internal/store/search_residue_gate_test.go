package store

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store/storetest/residuefixture"
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

// seedSearchResidueDB builds the minimal store the helper reads from one
// fixture case through the shared residue fixture: a stamped user_version
// plus the two FTS docsize tables. Only what NeedsSearchResidueRebuild
// inspects exists, so the cases pin the gate and nothing else.
func seedSearchResidueDB(t *testing.T, path string, c searchResidueGateCase) {
	t.Helper()
	convert := func(terms []searchResidueGateTerm) []residuefixture.ResidueTerm {
		out := make([]residuefixture.ResidueTerm, 0, len(terms))
		for _, term := range terms {
			out = append(out, residuefixture.ResidueTerm{ID: term.ID, Text: term.Text})
		}
		return out
	}
	residuefixture.Seed(t, path, c.Version, convert(c.UnionTerms), convert(c.LegacyTerms))
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

// TestNeedsSearchResidueRebuildWALPresent drives the -wal-present branch of
// the read-only open on a live WAL store. A writer connection stays open with
// automatic checkpoints off and commits, into the -wal only, the change that
// makes the store affected: user_version 74 -> 75 and a union-only posting.
// The database file alone is a clean v74 store, so both checks can only
// report 75 and residue by reading the uncheckpointed -wal. The database
// file and the -wal are byte-identical afterwards. The -shm is not compared:
// every reader of a live WAL store records its read mark there.
func TestNeedsSearchResidueRebuildWALPresent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "residue-wal.db")
	residuefixture.Seed(t, path, 74, residuefixture.CleanUnion, residuefixture.CleanLegacy)
	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Errorf("close the WAL writer: %v", err)
		}
	})
	for _, pragma := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`} {
		if err := sqlitex.ExecuteTransient(writer, pragma, nil); err != nil {
			t.Fatalf("%s on the fixture writer: %v", pragma, err)
		}
	}
	ghost := residuefixture.AffectedUnion[0]
	if err := sqlitex.ExecuteScript(writer, fmt.Sprintf(`INSERT INTO session_search_fts(rowid, t) VALUES (%d, '%s');
PRAGMA user_version = 75;`, ghost.ID+100, ghost.Text), nil); err != nil {
		t.Fatalf("commit the affected state into the -wal: %v", err)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("the -wal is missing before the check: %v; the WAL-present branch is not exercised", err)
	}
	dbBefore := fileHash(t, path)
	walBefore := fileHash(t, path+"-wal")

	version, err := SchemaVersionAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if version != 75 {
		t.Errorf("SchemaVersionAt = %d, want the uncheckpointed 75 from the -wal", version)
	}
	got, err := NeedsSearchResidueRebuild(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("NeedsSearchResidueRebuild = false, want the residue committed to the -wal")
	}
	if after := fileHash(t, path); string(after) != string(dbBefore) {
		t.Error("the WAL-present check modified the database file")
	}
	if after := fileHash(t, path+"-wal"); string(after) != string(walBefore) {
		t.Error("the WAL-present check modified the -wal")
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
