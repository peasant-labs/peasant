package store

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migration_v76_search_triggers.yaml
var migrationV76SearchTriggersYAML []byte

//go:embed testdata/migration_v76_search_triggers.manifest.yaml
var migrationV76SearchTriggersManifestYAML []byte

// migrationV76SearchCase is one V76 repair case: the chain under test (full
// runs V75 then V76, v75 caps the chain at V75 to pin the defect V76 repairs),
// the needs_rebuild preset the migration must preserve, and the observable
// union postings after the post-leg writes. Leg A writes land between V75
// and V76; leg B writes land after the final leg. On the full chain the
// residue predicate fires on the leg A window, so V76 rebuilds whole and
// every removed term and ghost is gone; leg B writes go through live union
// triggers with exact old values. The runner hardcodes the invariants: the
// untouched seed, both body terms, and a clean FTS integrity-check on every
// chain; the legacy-side landing of the capped chain's writes (the defect
// mechanism); and the production-path reads.
type migrationV76SearchCase struct {
	Name                      string `yaml:"name"`
	Chain                     string `yaml:"chain"`
	PresetNeedsRebuild        bool   `yaml:"presetNeedsRebuild"`
	WantInsertedInUnion       bool   `yaml:"wantInsertedInUnion"`
	WantUpdatedNewInUnion     bool   `yaml:"wantUpdatedNewInUnion"`
	WantLegAUpdatedOldInUnion bool   `yaml:"wantLegAUpdatedOldInUnion"`
	WantLegBUpdatedOldInUnion bool   `yaml:"wantLegBUpdatedOldInUnion"`
	WantLegAGhostInUnion      bool   `yaml:"wantLegAGhostInUnion"`
	WantLegBGhostInUnion      bool   `yaml:"wantLegBGhostInUnion"`
	WantLegacyAbsent          bool   `yaml:"wantLegacyAbsent"`
	WantTriggersTarget        string `yaml:"wantTriggersTarget"`
	WantNeedsRebuild          bool   `yaml:"wantNeedsRebuild"`
}

type migrationV76Fixtures struct {
	Cases          []migrationV76SearchCase    `yaml:"cases"`
	PredicateCases []migrationV76PredicateCase `yaml:"predicateCases"`
	OpenCases      []migrationV76OpenCase      `yaml:"openCases"`
}

func loadMigrationV76Fixtures(t *testing.T) migrationV76Fixtures {
	t.Helper()
	var fixtures migrationV76Fixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(migrationV76SearchTriggersYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode migration_v76_search_triggers.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(migrationV76SearchTriggersManifestYAML)
	if err != nil {
		t.Fatalf("decode migration_v76_search_triggers manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases)+len(fixtures.PredicateCases)+len(fixtures.OpenCases))
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
		if c.Chain != "full" && c.Chain != "v75" {
			t.Fatalf("migration_v76_search_triggers.yaml: case %q pins chain %q; add it to the runner", c.Name, c.Chain)
		}
		if c.WantTriggersTarget != "union" && c.WantTriggersTarget != "legacy" {
			t.Fatalf("migration_v76_search_triggers.yaml: case %q pins trigger target %q; add it to the runner", c.Name, c.WantTriggersTarget)
		}
	}
	for _, c := range fixtures.PredicateCases {
		actual = append(actual, c.Name)
		switch c.Window {
		case "none", "delete", "reuse", "insert":
		default:
			t.Fatalf("migration_v76_search_triggers.yaml: predicate case %q pins window %q; add it to the runner", c.Name, c.Window)
		}
	}
	for _, c := range fixtures.OpenCases {
		actual = append(actual, c.Name)
		switch c.Window {
		case "affected", "none", "insert":
		default:
			t.Fatalf("migration_v76_search_triggers.yaml: open case %q pins window %q; add it to the runner", c.Name, c.Window)
		}
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "v76 search triggers"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func loadMigrationV76SearchCases(t *testing.T) []migrationV76SearchCase {
	t.Helper()
	return loadMigrationV76Fixtures(t).Cases
}

// migrationV76PredicateCase is one residue-predicate case: the window applied
// to a v75 store and the predicate's verdict, evaluated against the vendored
// SQLite. The windows pin the gate: silence on a clean store and on inserts
// alone, residue on a delete and on a reused rowid.
type migrationV76PredicateCase struct {
	Name        string `yaml:"name"`
	Window      string `yaml:"window"`
	WantResidue bool   `yaml:"wantResidue"`
}

// migrationV76OpenCase is one Open-driven v76 case: the window applied to a
// v75 store, then a real Open runs the chain while the notice writer is
// captured. The affected path prints the one-line rebuild notice and heals
// the window; the crossing path stays silent and takes the cheap re-sync.
// The notice is the cheap-path probe: it fires exactly when the pre-chain
// predicate holds, and the in-migration gate re-evaluates the same predicate
// at v76's transactional point.
type migrationV76OpenCase struct {
	Name       string `yaml:"name"`
	Window     string `yaml:"window"`
	WantNotice bool   `yaml:"wantNotice"`
}

func loadMigrationV76PredicateCases(t *testing.T) []migrationV76PredicateCase {
	t.Helper()
	return loadMigrationV76Fixtures(t).PredicateCases
}

func loadMigrationV76OpenCases(t *testing.T) []migrationV76OpenCase {
	t.Helper()
	return loadMigrationV76Fixtures(t).OpenCases
}

// v76EntriesSession and v76BodiesSession seed two sessions: the entries
// session carries the UPDATE/DELETE targets and the keep row, the bodies
// session carries the pre-migration body row.
const (
	v76EntriesSession = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	v76BodiesSession  = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
)

// TestMigrationV76RepairsSearchTriggers proves V76 heals the V75 trigger
// regression: a consolidated-then-V75 store gets union-pointed triggers, a
// union entries half in which every live row is findable with no rebuild,
// and no retired index, while the rebuild flag and body search survive. The
// capped v75 case pins the defect (post-V75 entries writes land in the
// legacy index only), so removing V76 from the chain fails the healed cases.
// The residue predicate fires on the leg A window, so the full chain takes
// the rebuild path and every removed term and ghost is gone; the cheap
// re-sync path is pinned by the open-driven crossing case below.
func TestMigrationV76RepairsSearchTriggers(t *testing.T) {
	t.Parallel()
	for _, c := range loadMigrationV76SearchCases(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runMigrationV76SearchCase(t, c)
		})
	}
}

// buildV75Store opens a scratch database, migrates it to v74, simulates the
// production consolidation a pre-v75 store ran, seeds it, presets the search
// rebuild flag, and migrates it to v75. The caller owns the connection.
func buildV75Store(t *testing.T, path string, presetNeedsRebuild bool) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(74)); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	// Simulate the production consolidation a pre-V75 store ran (Phase 3 in
	// harmonized_migrate.go): entries triggers point at the union index, the
	// union is rebuilt over the view, the retired index is dropped, and the
	// flag is cleared. Post-V75 this history is unobservable — V75 drops and
	// recreates everything — but the simulation keeps the premise explicit.
	consolidate := "DROP TRIGGER IF EXISTS session_entries_ai;\n" +
		"DROP TRIGGER IF EXISTS session_entries_ad;\n" +
		"DROP TRIGGER IF EXISTS session_entries_au;\n" +
		migrateSearchTriggerAI + ";\n" +
		migrateSearchTriggerAD + ";\n" +
		migrateSearchTriggerAU + ";\n" +
		"INSERT INTO session_search_fts(session_search_fts) VALUES('rebuild');\n" +
		"DROP TABLE session_entries_fts;\n" +
		"UPDATE session_search_state SET needs_rebuild = 0 WHERE id = 1;"
	if err := sqlitex.ExecuteScript(conn, consolidate, nil); err != nil {
		t.Fatalf("simulate search consolidation at v74: %v", err)
	}
	seedMigrationV76Store(t, conn)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_search_state SET needs_rebuild = ? WHERE id = 1`, &sqlitex.ExecOptions{
		Args: []any{boolToInt(presetNeedsRebuild)},
	}); err != nil {
		t.Fatalf("preset needs_rebuild: %v", err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(75)); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return conn
}

func runMigrationV76SearchCase(t *testing.T, c migrationV76SearchCase) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v76.db")
	conn := buildV75Store(t, path, c.PresetNeedsRebuild)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close v76 case database: %v", err)
		}
	}()
	// Post-V75-leg writes: V75 routes these to the legacy index only. On the
	// full chain V76 must catch them up; on the capped chain they pin the
	// defect.
	writeMigrationV76LegA(t, conn)
	if c.Chain == "full" {
		if err := sqlitemigration.Migrate(t.Context(), conn, dbSchema); err != nil {
			t.Fatal(err)
		}
	}
	// Post-final-leg writes exercise the live triggers of whichever chain ran.
	writeMigrationV76LegB(t, conn)
	assertMigrationV76SearchCase(t, conn, c)
}

// seedMigrationV76Store seeds the entries targets and the pre-migration body
// row through the live V74 triggers.
func seedMigrationV76Store(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('v76-host', 'v76-host');
INSERT INTO projects(project_hash) VALUES('v76-project');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('`+v76EntriesSession+`', 'claude-code', 'fixture-model', 'v76-host', 'v76-project', 1, 2, 3, '/synthetic/v76-entries.jsonl', 'jsonl');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('`+v76BodiesSession+`', 'claude-code', 'fixture-model', 'v76-host', 'v76-project', 1, 2, 3, '/synthetic/v76-bodies.jsonl', 'jsonl');
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES('`+v76EntriesSession+`', 0, 'claude-code', 'text', 'user', 0, 'photosynthesis kept seed');
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES('`+v76EntriesSession+`', 1, 'claude-code', 'text', 'user', 0, 'mitochondria resync target');
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES('`+v76EntriesSession+`', 2, 'claude-code', 'text', 'assistant', 0, 'chlorophyll trigger target');
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES('`+v76EntriesSession+`', 3, 'claude-code', 'text', 'user', 0, 'kangaroo resync ghost');
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES('`+v76EntriesSession+`', 4, 'claude-code', 'text', 'user', 0, 'turnip trigger ghost');
INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, content_preview, has_tool_use, has_thinking, is_error, depth)
VALUES((SELECT coalesce(max(body_id), `+fmt.Sprintf("%d", BodyRowIDBase-1)+`) + 1 FROM session_entry_bodies), '`+v76BodiesSession+`', '`+strings.Repeat("ab", 32)+`', 0, 'claude-code', 'text', 'user', 'bodyseedterm stored body', 0, 0, 0, 0);`, nil); err != nil {
		t.Fatalf("seed v76 case store: %v", err)
	}
}

// writeMigrationV76LegA runs the entries writes that land between the V75 and
// V76 legs on the full chain: the resync must catch the insert up and refresh
// the updated row's new term. The image row stays at depth 0: the production
// read hides depth>0 rows whose preview duplicates a parent, and a parentless
// depth>0 row would vanish from production search for reasons unrelated to
// this migration.
func writeMigrationV76LegA(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES('`+v76EntriesSession+`', 5, 'claude-code', 'image', 'user', 0, 'quasar image insert');
UPDATE session_entries SET content_preview = 'nebula resync refreshed' WHERE session_id = '`+v76EntriesSession+`' AND entry_index = 1;
DELETE FROM session_entries WHERE session_id = '`+v76EntriesSession+`' AND entry_index = 3;`, nil); err != nil {
		t.Fatalf("write v76 leg A: %v", err)
	}
}

// writeMigrationV76LegB runs the writes after the final migration leg: on the
// full chain these exercise the repaired union triggers, on the capped chain
// they join the defect pin.
func writeMigrationV76LegB(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES('`+v76EntriesSession+`', 6, 'claude-code', 'text', 'user', 0, 'pulsar trigger insert');
UPDATE session_entries SET content_preview = 'ribosome trigger refreshed' WHERE session_id = '`+v76EntriesSession+`' AND entry_index = 2;
DELETE FROM session_entries WHERE session_id = '`+v76EntriesSession+`' AND entry_index = 4;
INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, content_preview, has_tool_use, has_thinking, is_error, depth)
VALUES((SELECT coalesce(max(body_id), `+fmt.Sprintf("%d", BodyRowIDBase-1)+`) + 1 FROM session_entry_bodies), '`+v76BodiesSession+`', '`+strings.Repeat("cd", 32)+`', 1, 'claude-code', 'text', 'user', 'postbodyterm fresh body', 0, 0, 0, 0);`, nil); err != nil {
		t.Fatalf("write v76 leg B: %v", err)
	}
}

// matchInFTS reports whether term matches any posting in the named FTS table.
func matchInFTS(t *testing.T, conn *sqlite.Conn, table, term string) bool {
	t.Helper()
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM `+table+` WHERE `+table+` MATCH ? LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{term},
		ResultFunc: func(*sqlite.Stmt) error {
			found = true
			return nil
		},
	}); err != nil {
		t.Fatalf("match %q in %s: %v", term, table, err)
	}
	return found
}

func assertMigrationV76SearchCase(t *testing.T, conn *sqlite.Conn, c migrationV76SearchCase) {
	t.Helper()
	// The chain under test pins the user_version: the full chain must reach
	// the build schema (V76 applied), the capped chain must stop at V75.
	var version int
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { version = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatalf("read case user_version: %v", err)
	}
	if c.Chain == "full" {
		if version != CurrentSchemaVersion() {
			t.Fatalf("full chain user_version = %d, want the build schema %d (V76 did not run)", version, CurrentSchemaVersion())
		}
	} else if version != 75 {
		t.Fatalf("capped chain user_version = %d, want 75", version)
	}

	// Union postings after both write legs, split by leg: leg A writes predate
	// the resync (removed terms and ghosts linger), leg B writes go through
	// live triggers (exact old values, no residue).
	for _, term := range []string{"quasar", "pulsar"} {
		if got := matchInFTS(t, conn, "session_search_fts", term); got != c.WantInsertedInUnion {
			t.Errorf("union match %q = %v, want %v", term, got, c.WantInsertedInUnion)
		}
	}
	for _, term := range []string{"nebula", "ribosome"} {
		if got := matchInFTS(t, conn, "session_search_fts", term); got != c.WantUpdatedNewInUnion {
			t.Errorf("union match %q = %v, want %v", term, got, c.WantUpdatedNewInUnion)
		}
	}
	if got := matchInFTS(t, conn, "session_search_fts", "mitochondria"); got != c.WantLegAUpdatedOldInUnion {
		t.Errorf("union match %q = %v, want %v (leg A removed term)", "mitochondria", got, c.WantLegAUpdatedOldInUnion)
	}
	if got := matchInFTS(t, conn, "session_search_fts", "chlorophyll"); got != c.WantLegBUpdatedOldInUnion {
		t.Errorf("union match %q = %v, want %v (leg B removed term)", "chlorophyll", got, c.WantLegBUpdatedOldInUnion)
	}
	if got := matchInFTS(t, conn, "session_search_fts", "kangaroo"); got != c.WantLegAGhostInUnion {
		t.Errorf("union match %q = %v, want %v (leg A ghost)", "kangaroo", got, c.WantLegAGhostInUnion)
	}
	if got := matchInFTS(t, conn, "session_search_fts", "turnip"); got != c.WantLegBGhostInUnion {
		t.Errorf("union match %q = %v, want %v (leg B ghost)", "turnip", got, c.WantLegBGhostInUnion)
	}
	// Invariants: the untouched seed and both body terms stay searchable, and
	// the range resync leaves a sound index (integrity-check returns no rows).
	for _, term := range []string{"photosynthesis", "bodyseedterm", "postbodyterm"} {
		if !matchInFTS(t, conn, "session_search_fts", term) {
			t.Errorf("union match %q = false, want the invariant term searchable", term)
		}
	}
	integrity := false
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_search_fts(session_search_fts) VALUES('integrity-check')`, &sqlitex.ExecOptions{
		ResultFunc: func(*sqlite.Stmt) error {
			integrity = true
			return nil
		},
	}); err != nil {
		t.Fatalf("union integrity-check: %v", err)
	}
	if integrity {
		t.Error("union integrity-check returned rows; the resync left a corrupt index")
	}

	// The capped chain proves the defect mechanism: post-V75 entries writes
	// land in the legacy index only.
	if !c.WantLegacyAbsent {
		for _, term := range []string{"quasar", "pulsar"} {
			if !matchInFTS(t, conn, "session_entries_fts", term) {
				t.Errorf("legacy match %q = false, want the capped write in the retired index", term)
			}
		}
	}
	assertMigrationV76ProductionReads(t, conn, c)

	// Retired index presence and trigger targets.
	var legacy int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'session_entries_fts'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { legacy = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if got := legacy == 0; got != c.WantLegacyAbsent {
		t.Errorf("legacy index absent = %v, want %v", got, c.WantLegacyAbsent)
	}
	wantTarget := "session_search_fts"
	if c.WantTriggersTarget == "legacy" {
		wantTarget = "session_entries_fts"
	}
	for _, trigger := range []string{"session_entries_ai", "session_entries_ad", "session_entries_au"} {
		var sql string
		if err := sqlitex.ExecuteTransient(conn, `SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, &sqlitex.ExecOptions{
			Args:       []any{trigger},
			ResultFunc: func(stmt *sqlite.Stmt) error { sql = stmt.ColumnText(0); return nil },
		}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sql, wantTarget) {
			t.Errorf("trigger %s points away from %s: %q", trigger, wantTarget, sql)
		}
	}

	// V76 never touches the rebuild flag.
	state, err := searchStateReadOnConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	if state.NeedsRebuild != c.WantNeedsRebuild {
		t.Errorf("needs_rebuild = %v, want %v", state.NeedsRebuild, c.WantNeedsRebuild)
	}
}

// assertMigrationV76ProductionReads proves the production transition-window
// read serves the healed state correctly: every live entries row is findable,
// and no removed term or ghost returns a hit — the rebuild expunged them.
// Body terms stay out of production asserts: production body search is
// generation-gated (the unified read requires an active generation mapping)
// and this seed carries none, by design. On the capped chain the merged read
// still serves the fresh legacy postings, which is why the defect was
// union-only.
func assertMigrationV76ProductionReads(t *testing.T, conn *sqlite.Conn, c migrationV76SearchCase) {
	t.Helper()
	productionHits := func(term string) int {
		t.Helper()
		hits, err := SearchMergedOnConn(conn, term, 50, 0, SearchFilters{})
		if err != nil {
			t.Fatalf("production search for %q: %v", term, err)
		}
		return len(hits)
	}
	if c.Chain == "full" {
		for _, term := range []string{"quasar", "pulsar", "nebula", "ribosome", "photosynthesis"} {
			if got := productionHits(term); got == 0 {
				t.Errorf("production search for %q returned no hit, want the live row", term)
			}
		}
		for _, term := range []string{"kangaroo", "turnip"} {
			if got := productionHits(term); got != 0 {
				t.Errorf("production search for %q returned %d hits, want none for the deleted ghost", term, got)
			}
		}
		for _, term := range []string{"mitochondria", "chlorophyll"} {
			if got := productionHits(term); got != 0 {
				t.Errorf("production search for %q returned %d hits, want none (rebuild expunged the residue)", term, got)
			}
		}
		return
	}
	if got := productionHits("quasar"); got == 0 {
		t.Error("production search for capped-chain quasar returned no hit, want the fresh legacy posting")
	}
	for _, term := range []string{"kangaroo", "turnip"} {
		if got := productionHits(term); got != 0 {
			t.Errorf("production search for capped-chain %q returned %d hits, want none for the ghost", term, got)
		}
	}
	for _, term := range []string{"mitochondria", "chlorophyll"} {
		if got := productionHits(term); got == 0 {
			t.Errorf("production search for capped-chain %q returned no hit, want the stale posting", term)
		}
	}
}

// applyMigrationV76Window applies one predicate-case window to a v75 store:
// none leaves the freshly rebuilt indexes alone, delete removes a seeded row
// (ghost in the union), reuse deletes a row and reinserts its rowid with
// different-token-count content (the stale term then matches the new,
// unrelated row), insert adds a row only (legacy carries it; the union lacks
// it, which needs no rebuild), and affected combines an update, a delete,
// and a rowid reuse for the Open-driven end-to-end case.
func applyMigrationV76Window(t *testing.T, conn *sqlite.Conn, window string) {
	t.Helper()
	seedRowid := func(entryIndex int) int64 {
		t.Helper()
		var rowid int64
		if err := sqlitex.ExecuteTransient(conn, `SELECT rowid FROM session_entries WHERE session_id = ? AND entry_index = ?`, &sqlitex.ExecOptions{
			Args: []any{v76EntriesSession, entryIndex},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rowid = stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			t.Fatalf("read seed rowid: %v", err)
		}
		if rowid == 0 {
			t.Fatalf("seed row %d has no rowid; the window needs a live row", entryIndex)
		}
		return rowid
	}
	switch window {
	case "none":
		return
	case "delete":
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entries WHERE session_id = ? AND entry_index = 3`, &sqlitex.ExecOptions{
			Args: []any{v76EntriesSession},
		}); err != nil {
			t.Fatalf("window delete: %v", err)
		}
	case "insert":
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview) VALUES(?, 5, 'claude-code', 'text', 'user', 0, 'quasar lonely insert')`, &sqlitex.ExecOptions{
			Args: []any{v76EntriesSession},
		}); err != nil {
			t.Fatalf("window insert: %v", err)
		}
	case "reuse", "affected":
		if window == "affected" {
			if err := sqlitex.ExecuteTransient(conn, `UPDATE session_entries SET content_preview = 'nebula count changed here' WHERE session_id = ? AND entry_index = 1`, &sqlitex.ExecOptions{
				Args: []any{v76EntriesSession},
			}); err != nil {
				t.Fatalf("affected window update: %v", err)
			}
		}
		rowid := seedRowid(3)
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entries WHERE session_id = ? AND entry_index = 3`, &sqlitex.ExecOptions{
			Args: []any{v76EntriesSession},
		}); err != nil {
			t.Fatalf("reuse window delete: %v", err)
		}
		// Explicit rowid reuse, mirroring SQLite max(rowid)+1 reuse on a
		// store without AUTOINCREMENT: the stale union posting for the
		// deleted row now names a live, unrelated row.
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_entries(rowid, session_id, entry_index, provider, entry_type, role, depth, content_preview) VALUES(?, ?, 5, 'claude-code', 'text', 'user', 0, 'quasar')`, &sqlitex.ExecOptions{
			Args: []any{rowid, v76EntriesSession},
		}); err != nil {
			t.Fatalf("reuse window reinsert: %v", err)
		}
	default:
		t.Fatalf("unknown v76 window %q", window)
	}
}

// openMigrationV76Store builds the v75 state every v76 window case starts
// from and applies the caller's window writes. The caller owns the
// connection and must close it.
func openMigrationV76Store(t *testing.T, path, window string) *sqlite.Conn {
	t.Helper()
	conn := buildV75Store(t, path, false)
	applyMigrationV76Window(t, conn, window)
	return conn
}

// TestMigrationV76ResiduePredicate pins the residue gate against the vendored
// SQLite: silence on a clean store and on inserts alone, residue on a delete
// and on a reused rowid. The reused-rowid case also pins the defect the
// rebuild exists for: the deleted row's stale term matches the new,
// unrelated row that reused its rowid.
func TestMigrationV76ResiduePredicate(t *testing.T) {
	t.Parallel()
	for _, c := range loadMigrationV76PredicateCases(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "v76pred.db")
			conn := openMigrationV76Store(t, path, c.Window)
			defer func() {
				if err := conn.Close(); err != nil {
					t.Errorf("close predicate database: %v", err)
				}
			}()
			if c.Window == "reuse" && !matchInFTS(t, conn, "session_search_fts", "kangaroo") {
				t.Fatal("stale kangaroo term does not match the row reusing its rowid; the defect repro is broken")
			}
			got, err := searchIndexResiduePresent(conn)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.WantResidue {
				t.Errorf("residue predicate = %v, want %v for window %q", got, c.WantResidue, c.Window)
			}
		})
	}
}

// TestMigrationV76OpenNotice drives real Opens with the notice writer
// captured. It runs sequentially (no t.Parallel): the writer swap is
// process-global, and sequential tests never run concurrently with parallel
// ones. The affected window prints the one-line rebuild notice and heals the
// window — a deleted-in-window term no longer matches the reused-rowid row —
// while the crossing store stays silent and takes the cheap re-sync.
func TestMigrationV76OpenNotice(t *testing.T) {
	for _, c := range loadMigrationV76OpenCases(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v76open.db")
			conn := openMigrationV76Store(t, path, c.Window)
			before, err := searchIndexResiduePresent(conn)
			if err != nil {
				t.Fatal(err)
			}
			if before != c.WantNotice {
				t.Fatalf("pre-open residue = %v, want %v (the notice follows the predicate)", before, c.WantNotice)
			}
			if c.Window == "affected" && !matchInFTS(t, conn, "session_search_fts", "kangaroo") {
				t.Fatal("stale kangaroo term does not match the reused rowid row before Open; the defect repro is broken")
			}
			if c.Window == "insert" && matchInFTS(t, conn, "session_search_fts", "quasar") {
				t.Fatal("inserted term already in the union before Open; the re-sync would prove nothing")
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			previous := searchIndexRebuildNoticeWriter
			var notice bytes.Buffer
			searchIndexRebuildNoticeWriter = &notice
			s, err := Open(path, WithPoolSize(1))
			searchIndexRebuildNoticeWriter = previous
			if err != nil {
				t.Fatalf("Open with v75 residue: %v", err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Errorf("close migrated store: %v", err)
				}
			})
			if c.WantNotice {
				if notice.String() != searchIndexRebuildNotice+"\n" {
					t.Errorf("notice = %q, want the one-line rebuild warning", notice.String())
				}
			} else if notice.Len() != 0 {
				t.Errorf("notice = %q, want silence on the clean crossing path", notice.String())
			}
			assertMigrationV76OpenHealed(t, s, c)
		})
	}
}

// assertMigrationV76OpenHealed proves the Open-driven end state: union
// triggers live, the retired index gone, every live term findable, and no
// removed term or ghost matching — on the affected path only the rebuild
// expunges them, on the crossing path there was never residue.
func assertMigrationV76OpenHealed(t *testing.T, s *Store, c migrationV76OpenCase) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var version int
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { version = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion() {
		t.Fatalf("user_version = %d, want the build schema %d", version, CurrentSchemaVersion())
	}
	var legacy int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'session_entries_fts'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { legacy = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Fatal("the retired search index survives Open")
	}
	for _, trigger := range []string{"session_entries_ai", "session_entries_ad", "session_entries_au"} {
		var sql string
		if err := sqlitex.ExecuteTransient(conn, `SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, &sqlitex.ExecOptions{
			Args:       []any{trigger},
			ResultFunc: func(stmt *sqlite.Stmt) error { sql = stmt.ColumnText(0); return nil },
		}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sql, "session_search_fts") {
			t.Fatalf("trigger %s points away from the union index after Open: %q", trigger, sql)
		}
	}
	if !matchInFTS(t, conn, "session_search_fts", "photosynthesis") {
		t.Error("union lost the untouched seed term across Open")
	}
	if !matchInFTS(t, conn, "session_search_fts", "bodyseedterm") {
		t.Error("union lost the seeded body term across Open; body search must survive the rebuild path")
	}
	if c.Window == "affected" {
		for _, term := range []string{"kangaroo", "mitochondria"} {
			if matchInFTS(t, conn, "session_search_fts", term) {
				t.Errorf("union match %q survived Open on the affected path; only the rebuild expunges it", term)
			}
		}
		for _, term := range []string{"quasar", "nebula"} {
			if !matchInFTS(t, conn, "session_search_fts", term) {
				t.Errorf("union lost live term %q across Open on the affected path", term)
			}
		}
	}
	if c.Window == "insert" {
		// The re-sync indexed the insert-only window without a rebuild:
		// the row is findable, and the untouched rows are undisturbed.
		if !matchInFTS(t, conn, "session_search_fts", "quasar") {
			t.Error("union lost the inserted term across Open; the cheap re-sync must index it")
		}
		if !matchInFTS(t, conn, "session_search_fts", "kangaroo") {
			t.Error("union lost the untouched seed term across Open on the insert path")
		}
	}
}
