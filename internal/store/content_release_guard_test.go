package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_release_guard.yaml
var contentReleaseGuardYAML []byte

//go:embed testdata/content_release_guard.manifest.yaml
var contentReleaseGuardManifestYAML []byte

// contentReleaseGuardCase is one content_release_guard case: the
// section-10 name plus the seed the runner builds, the tables it clears to
// isolate one failing precondition, the production phases it runs, the
// guard evaluations it expects (with the refusal-detail substrings every
// failing evaluation must carry), and the drop disposition behind the
// guard.
type contentReleaseGuardCase struct {
	Name string `yaml:"name"`
	// Seed builds the store state: none (a fresh install), file-backed (a
	// native session with projection rows, mirror rows, and native
	// full-content rows), or non-native-full (a legacy session whose
	// full-content rows belong to no native session).
	Seed string `yaml:"seed,omitempty"`
	// Clear empties projection or full-content tables after seeding, so
	// exactly the wanted preconditions fail.
	Clear []string `yaml:"clear,omitempty"`
	// Consolidate runs the production search-consolidation phase.
	Consolidate bool `yaml:"consolidate,omitempty"`
	// Migrate runs the production per-session conversion, then search
	// consolidation: the session ends converted.
	Migrate bool `yaml:"migrate,omitempty"`
	// WantFailed names the preconditions that must fail, each with a
	// positive row count and the detail substrings.
	WantFailed []string `yaml:"wantFailed,omitempty"`
	// WantPassed names the preconditions that must hold, each with an
	// empty detail.
	WantPassed []string `yaml:"wantPassed,omitempty"`
	// WantDetailContains lists the substrings every failing detail must
	// carry (the fix first of all).
	WantDetailContains []string `yaml:"wantDetailContains,omitempty"`
	// Drop proves the drop behind the guard: refused (nothing changes),
	// allowed (the retired tables go, the kept tables stay), or skip.
	Drop string `yaml:"drop,omitempty"`
	// WantFullContentKept proves the allowed drop keeps the non-native
	// full-content rows it still serves.
	WantFullContentKept bool `yaml:"wantFullContentKept,omitempty"`
	// WantRetiredGone proves the allowed drop removed the retired tables
	// while the kept tables stay.
	WantRetiredGone bool `yaml:"wantRetiredGone,omitempty"`
	// WantDualIndex proves a fresh install carries both search indexes
	// until consolidation runs.
	WantDualIndex bool `yaml:"wantDualIndex,omitempty"`
	// ThenConsolidate asserts the initial refusal, runs consolidation,
	// and then proves every precondition holds and the drop succeeds.
	ThenConsolidate bool `yaml:"thenConsolidate,omitempty"`
	// NewerSchema stamps user_version past this build and proves both
	// opens refuse with the newer-release fix; it runs no guard case.
	NewerSchema bool `yaml:"newerSchema,omitempty"`
}

type contentReleaseGuardFixtures struct {
	Cases []contentReleaseGuardCase `yaml:"cases"`
}

// loadContentReleaseGuardFixtures strictly decodes the typed release-guard
// family and enforces its required-names manifest in both directions.
func loadContentReleaseGuardFixtures(t *testing.T) []contentReleaseGuardCase {
	t.Helper()
	var fixtures contentReleaseGuardFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(contentReleaseGuardYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode content_release_guard.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentReleaseGuardManifestYAML)
	if err != nil {
		t.Fatalf("decode content release guard manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("content_release_guard.yaml: a case has a blank name")
		}
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "content release guard"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

// TestContentReleaseGuardFixtureManifest pins the release-guard case
// inventory: the loader compiles, the manifest loads, and every required
// name is present.
func TestContentReleaseGuardFixtureManifest(t *testing.T) {
	t.Parallel()
	loadContentReleaseGuardFixtures(t)
}

// TestContentReleaseGuard runs the content_release_guard cases under
// their section-10 names: each precondition branch refuses with the
// failing condition and the fix named, the allow cases pass, a fresh
// install needs consolidation, and a newer schema is refused on open.
func TestContentReleaseGuard(t *testing.T) {
	t.Parallel()
	for _, c := range loadContentReleaseGuardFixtures(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.NewerSchema {
				runReleaseGuardNewerSchema(t)
				return
			}
			runReleaseGuardCase(t, c)
		})
	}
}

// releaseGuardClearAllowlist is the exact set of tables a fixture may
// clear: the three retired projection tables plus the native full-content
// manifest (whose chunks follow through the cascade).
var releaseGuardClearAllowlist = map[string]struct{}{
	"session_projection_generations": {},
	"session_projection_entries":     {},
	"session_projection_content":     {},
	"session_entry_full_content":     {},
}

// runReleaseGuardCase seeds one guard case, runs its production phases,
// asserts its guard evaluations, and proves its drop disposition.
func runReleaseGuardCase(t *testing.T, c contentReleaseGuardCase) {
	t.Helper()
	ctx := context.Background()
	s, root := openGenerationStore(t)
	sid := migrateCaseSessionID(t, c.Name, 0)
	seedGenerationSession(t, s, string(sid))
	switch c.Seed {
	case "none", "":
	case "file-backed":
		seedReleaseGuardFileBacked(t, s, root, sid)
	case "non-native-full":
		seedMigrateNonNative(t, s, sid, true)
	default:
		t.Fatalf("unknown seed %q", c.Seed)
	}
	for _, table := range c.Clear {
		if _, ok := releaseGuardClearAllowlist[table]; !ok {
			t.Fatalf("clear of %q is outside the allowlist", table)
		}
		execMigrateSQL(t, s, `DELETE FROM `+table)
	}
	if c.Migrate {
		outcome, err := s.MigrateSession(ctx, sid)
		if err != nil {
			t.Fatalf("MigrateSession: %v", err)
		}
		if outcome != MigrateOutcomeConverted {
			t.Fatalf("MigrateSession outcome = %q, want converted", outcome)
		}
		if consolidated, err := s.migrateConsolidateSearch(ctx); err != nil || !consolidated {
			t.Fatalf("consolidate after conversion: consolidated=%v err=%v; want the retired index dropped", consolidated, err)
		}
	} else if c.Consolidate {
		if consolidated, err := s.migrateConsolidateSearch(ctx); err != nil || !consolidated {
			t.Fatalf("consolidate search: consolidated=%v err=%v; want the retired index dropped", consolidated, err)
		}
	}
	if c.WantDualIndex {
		assertReleaseGuardDualIndex(t, s)
	}
	assertReleaseGuardEvaluations(t, s, c)
	switch c.Drop {
	case "refused":
		assertReleaseGuardDropRefused(t, s, c)
	case "allowed":
		assertReleaseGuardDropAllowed(t, s, c)
	case "skip":
	default:
		t.Fatalf("unknown drop disposition %q", c.Drop)
	}
	if c.ThenConsolidate {
		if consolidated, err := s.migrateConsolidateSearch(ctx); err != nil || !consolidated {
			t.Fatalf("consolidate a fresh install: consolidated=%v err=%v; want the retired index dropped", consolidated, err)
		}
		assertReleaseGuardAllPassed(t, s)
		if err := s.DropRetiredProjectionTables(ctx); err != nil {
			t.Fatalf("drop after consolidation: %v; want the retired tables gone", err)
		}
		assertReleaseGuardTablesGone(t, s)
	}
}

// seedReleaseGuardFileBacked builds one native file-backed session with
// projection rows, mirror rows, and native full-content rows: the state
// every refuse case isolates one precondition from.
func seedReleaseGuardFileBacked(t *testing.T, s *Store, root string, sid schema.SessionID) {
	t.Helper()
	text, input, output := migrateSeedTexts("gen_release_guard")
	v2, blobs := buildTestGeneration(t, sid, "gen_release_guard", text, input, output)
	stampSeedMetadataHash(&v2)
	seedFileBackedGeneration(t, s, root, sid, v2, blobs, true)
}

// assertReleaseGuardEvaluations proves the guard order, the failing set
// with its counts and fix, and the passing set.
func assertReleaseGuardEvaluations(t *testing.T, s *Store, c contentReleaseGuardCase) {
	t.Helper()
	statuses, err := s.EvaluateRetirementPreconditions(context.Background())
	if err != nil {
		t.Fatalf("EvaluateRetirementPreconditions: %v", err)
	}
	if len(statuses) != len(AllRetirementPreconditions) {
		t.Fatalf("evaluated %d preconditions, want the %d in AllRetirementPreconditions order", len(statuses), len(AllRetirementPreconditions))
	}
	for i, status := range statuses {
		if status.Name != AllRetirementPreconditions[i] {
			t.Fatalf("evaluation %d is %q, want %q in AllRetirementPreconditions order", i, status.Name, AllRetirementPreconditions[i])
		}
	}
	byName := make(map[RetirementPrecondition]RetirementPreconditionStatus, len(statuses))
	for _, status := range statuses {
		byName[status.Name] = status
	}
	for _, name := range c.WantFailed {
		status, ok := byName[RetirementPrecondition(name)]
		if !ok {
			t.Fatalf("wantFailed names unknown precondition %q", name)
		}
		if status.Passed {
			t.Fatalf("precondition %q passed, want it to fail", name)
		}
		if status.RowCount <= 0 {
			t.Fatalf("precondition %q fails with row count %d, want a positive blocking count", name, status.RowCount)
		}
		for _, want := range c.WantDetailContains {
			if !strings.Contains(status.Detail, want) {
				t.Fatalf("precondition %q detail %q does not name %q", name, status.Detail, want)
			}
		}
	}
	for _, name := range c.WantPassed {
		status, ok := byName[RetirementPrecondition(name)]
		if !ok {
			t.Fatalf("wantPassed names unknown precondition %q", name)
		}
		if !status.Passed {
			t.Fatalf("precondition %q fails with %d rows (%s), want it to hold", name, status.RowCount, status.Detail)
		}
		if status.Detail != "" {
			t.Fatalf("precondition %q holds but carries detail %q", name, status.Detail)
		}
	}
}

// assertReleaseGuardDropRefused proves the drop changes nothing while any
// precondition fails and names the failing condition with the fix.
func assertReleaseGuardDropRefused(t *testing.T, s *Store, c contentReleaseGuardCase) {
	t.Helper()
	err := s.DropRetiredProjectionTables(context.Background())
	if err == nil {
		t.Fatal("drop with failing preconditions succeeded; want the guard to refuse")
	}
	var notReady *RetirementNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("drop error is %T (%v); want a guard refusal naming the failing conditions", err, err)
	}
	if len(notReady.Failed) != len(c.WantFailed) {
		t.Fatalf("refusal names %d failing precondition(s), want the %d in wantFailed", len(notReady.Failed), len(c.WantFailed))
	}
	for _, status := range notReady.Failed {
		if !strings.Contains(err.Error(), string(status.Name)) {
			t.Fatalf("refusal %q does not name failing precondition %q", err.Error(), status.Name)
		}
	}
	if !strings.Contains(err.Error(), "peasant migrate") {
		t.Fatalf("refusal %q does not name the fix", err.Error())
	}
	for _, table := range []string{"session_projection_generations", "session_projection_entries", "session_projection_content"} {
		if queryMigrateInt(t, s, `SELECT COUNT(*) FROM `+table) < 0 {
			t.Fatalf("refused drop left %s unreadable", table)
		}
	}
}

// assertReleaseGuardDropAllowed proves the allowed drop removes the
// retired tables while the kept tables stay.
func assertReleaseGuardDropAllowed(t *testing.T, s *Store, c contentReleaseGuardCase) {
	t.Helper()
	ctx := context.Background()
	var fullBefore int64
	if c.WantFullContentKept {
		fullBefore = queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_entry_full_content`)
		if fullBefore <= 0 {
			t.Fatal("non-native seed left no full-content rows; the allow case proves nothing")
		}
	}
	if err := s.DropRetiredProjectionTables(ctx); err != nil {
		t.Fatalf("drop with all preconditions met: %v; want the retired tables gone", err)
	}
	if c.WantFullContentKept {
		if got := queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_entry_full_content`); got != fullBefore {
			t.Fatalf("allowed drop kept %d full-content rows, want the seeded %d", got, fullBefore)
		}
		if got := queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_entry_full_content_chunks`); got <= 0 {
			t.Fatal("allowed drop left no full-content chunks; the kept tables must stay")
		}
	}
	if c.WantRetiredGone {
		assertReleaseGuardTablesGone(t, s)
	}
	// The guard stays all-passed on the retired database: a removed table
	// counts as empty, so the drop is idempotent.
	assertReleaseGuardAllPassed(t, s)
	if err := s.DropRetiredProjectionTables(ctx); err != nil {
		t.Fatalf("second drop on the retired database: %v; want idempotent success", err)
	}
}

// assertReleaseGuardTablesGone proves the retired tables are gone while
// the kept full-content tables stay.
func assertReleaseGuardTablesGone(t *testing.T, s *Store) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	for _, table := range []string{"session_projection_generations", "session_projection_entries", "session_projection_content"} {
		if tableExistsOnConn(conn, table) {
			t.Fatalf("retired table %s still exists after the allowed drop", table)
		}
	}
	for _, table := range []string{"session_entry_full_content", "session_entry_full_content_chunks"} {
		if !tableExistsOnConn(conn, table) {
			t.Fatalf("kept table %s is gone after the drop; only the retired projection tables go", table)
		}
	}
}

// assertReleaseGuardAllPassed proves every precondition holds.
func assertReleaseGuardAllPassed(t *testing.T, s *Store) {
	t.Helper()
	statuses, err := s.EvaluateRetirementPreconditions(context.Background())
	if err != nil {
		t.Fatalf("EvaluateRetirementPreconditions: %v", err)
	}
	for _, status := range statuses {
		if !status.Passed {
			t.Fatalf("precondition %q fails with %d rows (%s) after the drop; want all-passed", status.Name, status.RowCount, status.Detail)
		}
	}
}

// assertReleaseGuardDualIndex proves a fresh install carries both search
// indexes: the retired one and the harmonized one.
func assertReleaseGuardDualIndex(t *testing.T, s *Store) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	for _, table := range []string{"session_entries_fts", "session_search_fts"} {
		if !tableExistsOnConn(conn, table) {
			t.Fatalf("fresh install is missing %s; want the dual-index state until consolidation", table)
		}
	}
}

// TestEvaluateRetirementPreconditionsOnConn proves the shared-connection
// evaluation the drop runs on: one caller-held connection yields all five
// preconditions in AllRetirementPreconditions order, so the check and the
// change never span two checkouts.
func TestEvaluateRetirementPreconditionsOnConn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openGenerationStore(t)
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	statuses, err := evaluateRetirementPreconditionsOnConn(ctx, conn)
	if err != nil {
		t.Fatalf("evaluate on the caller-held connection: %v", err)
	}
	if len(statuses) != len(AllRetirementPreconditions) {
		t.Fatalf("evaluated %d preconditions, want %d", len(statuses), len(AllRetirementPreconditions))
	}
	for i, status := range statuses {
		if status.Name != AllRetirementPreconditions[i] {
			t.Fatalf("evaluation %d is %q, want %q", i, status.Name, AllRetirementPreconditions[i])
		}
	}
	// A fresh store holds the dual-index state: everything passes except
	// the retired-index guard.
	for _, status := range statuses {
		if status.Name == RetirementPreconditionSessionEntriesFTSAbsent {
			if status.Passed {
				t.Fatalf("%q passed on a fresh store; want the consolidation refusal", status.Name)
			}
			continue
		}
		if !status.Passed {
			t.Fatalf("%q fails on a fresh store (%s); want it to hold", status.Name, status.Detail)
		}
	}
}

// runReleaseGuardNewerSchema stamps user_version past this build and
// proves both the read-write and the read-only opens refuse with the
// newer-release fix, changing nothing.
func runReleaseGuardNewerSchema(t *testing.T) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "newer.db")
	opened, err := Open(dbPath, WithPoolSize(2))
	if err != nil {
		t.Fatalf("open a fresh database: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	newer := CurrentSchemaVersion() + 1
	raw, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(raw, fmt.Sprintf("PRAGMA user_version = %d;", newer), nil); err != nil {
		_ = raw.Close()
		t.Fatalf("stamp a newer schema version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if version, err := SchemaVersionAt(dbPath); err != nil || version != newer {
		t.Fatalf("SchemaVersionAt = %d, err = %v; want the stamped %d", version, err, newer)
	}
	if _, err := Open(dbPath, WithPoolSize(2)); err == nil {
		t.Fatal("open of a newer database succeeded; want the newer-schema refusal")
	} else if !strings.Contains(err.Error(), "newer Peasant") || !strings.Contains(err.Error(), "install that version or newer") {
		t.Fatalf("open refusal %q does not name the newer release and the fix", err.Error())
	}
	if _, err := OpenReadOnlyWithOptions(dbPath); err == nil {
		t.Fatal("read-only open of a newer database succeeded; want the newer-schema refusal")
	} else if !strings.Contains(err.Error(), "newer Peasant") || !strings.Contains(err.Error(), "install that version or newer") {
		t.Fatalf("read-only refusal %q does not name the newer release and the fix", err.Error())
	}
	if version, err := SchemaVersionAt(dbPath); err != nil || version != newer {
		t.Fatalf("refused opens changed the database: version = %d, err = %v", version, err)
	}
}
