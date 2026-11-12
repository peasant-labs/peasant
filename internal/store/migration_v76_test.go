package store

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strconv"
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
// the needs_rebuild preset and the flag V76 must leave (the rebuild branch
// the full chain takes clears it), and the observable
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
		case "none", "delete", "reuse", "insert", "update-same-count", "update-count-changed":
		default:
			t.Fatalf("migration_v76_search_triggers.yaml: predicate case %q pins window %q; add it to the runner", c.Name, c.Window)
		}
	}
	for _, c := range fixtures.OpenCases {
		actual = append(actual, c.Name)
		switch c.Window {
		case "affected", "none", "insert", "update-same-count", "consolidated", "crossing-from-v74", "tampered":
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
// the window; the crossing path stays silent and takes the targeted insert.
// The notice only warns — it is not the cheap-path probe. A sentinel posting
// in the body half probes the SQL gate: it survives the targeted insert and
// is gone only after a whole-index rebuild.
//
// presetNeedsRebuild sets the search rebuild flag before the window and
// wantNeedsRebuild pins it after Open: the rebuild branch clears it, the
// targeted-insert branch leaves it. wantPrecheckWarning marks a tampered
// store whose pre-open residue check errors: Open prints one warning line
// instead of the notice and continues, and v76 repairs the store itself.
type migrationV76OpenCase struct {
	Name                string `yaml:"name"`
	Window              string `yaml:"window"`
	WantNotice          bool   `yaml:"wantNotice"`
	WantPrecheckWarning bool   `yaml:"wantPrecheckWarning,omitempty"`
	PresetNeedsRebuild  bool   `yaml:"presetNeedsRebuild,omitempty"`
	WantNeedsRebuild    bool   `yaml:"wantNeedsRebuild,omitempty"`
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
// union index in which every live row is findable,
// and no retired index, while body search survives and the rebuild branch
// clears a pending rebuild flag. The
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

// buildV75Store builds the v74 store buildV74Store describes and migrates it
// to v75 through the frozen shipped chain. The caller owns the connection.
func buildV75Store(t *testing.T, path string, presetNeedsRebuild bool) *sqlite.Conn {
	t.Helper()
	conn := buildV74Store(t, path, presetNeedsRebuild)
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(75)); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return conn
}

// buildV74Store opens a scratch database, migrates it to v74 through the
// frozen shipped chain, simulates the production consolidation a pre-v75
// store ran, seeds it, and presets the search rebuild flag. The caller owns
// the connection.
func buildV74Store(t *testing.T, path string, presetNeedsRebuild bool) *sqlite.Conn {
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

// ftsContentIntegrityCheck runs the content-aware FTS5 integrity check on
// the union index: INSERT ... (session_search_fts, rank)
// VALUES('integrity-check', 1) compares the index against the content view
// and reports any drift or corruption as an error; it never returns rows.
func ftsContentIntegrityCheck(conn *sqlite.Conn) error {
	return sqlitex.ExecuteTransient(conn, `INSERT INTO session_search_fts(session_search_fts, rank) VALUES('integrity-check', 1)`, nil)
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
	// Invariants: the untouched seed and both body terms stay searchable.
	for _, term := range []string{"photosynthesis", "bodyseedterm", "postbodyterm"} {
		if !matchInFTS(t, conn, "session_search_fts", term) {
			t.Errorf("union match %q = false, want the invariant term searchable", term)
		}
	}
	// The content-aware check reports a corrupt or stale external-content
	// index as an error (FTS5 never returns rows from it). On the full chain
	// it must pass; on the capped v75 chain the union is stale by design
	// (the defect V76 repairs), so it must fail: that failure is the pinned
	// defect.
	integrityErr := ftsContentIntegrityCheck(conn)
	if c.Chain == "full" {
		if integrityErr != nil {
			t.Fatalf("union integrity-check after the full chain: %v", integrityErr)
		}
	} else if integrityErr == nil {
		t.Error("union integrity-check passed clean on the capped v75 chain; want it to report the stale union the defect leaves")
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

	// The rebuild branch clears a pending rebuild flag (the whole-index
	// rebuild makes it moot); the capped chain never ran V76.
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
// it, which needs no rebuild), update-same-count swaps a term keeping the
// token count (the documented blind spot: the predicate stays silent while
// the stale term still matches), update-count-changed swaps a term changing
// the token count (residue), and affected combines an update, a delete, and
// a rowid reuse for the Open-driven end-to-end case.
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
	case "update-same-count":
		// Token-count-preserving swap: 'photosynthesis kept seed' (3 tokens)
		// becomes 'sunflower kept seed' (3 tokens). The union keeps the stale
		// 'photosynthesis' posting while the legacy moves to 'sunflower', so
		// the docsize rows compare equal and the predicate stays silent — the
		// documented blind spot.
		if err := sqlitex.ExecuteTransient(conn, `UPDATE session_entries SET content_preview = 'sunflower kept seed' WHERE session_id = ? AND entry_index = 0`, &sqlitex.ExecOptions{
			Args: []any{v76EntriesSession},
		}); err != nil {
			t.Fatalf("update-same-count window: %v", err)
		}
	case "update-count-changed":
		// Token-count-changing update: the new text carries more tokens, so
		// the union/legacy docsize rows differ and the predicate fires.
		if err := sqlitex.ExecuteTransient(conn, `UPDATE session_entries SET content_preview = 'nebula count changed here with extra words' WHERE session_id = ? AND entry_index = 0`, &sqlitex.ExecOptions{
			Args: []any{v76EntriesSession},
		}); err != nil {
			t.Fatalf("update-count-changed window: %v", err)
		}
	case "consolidated":
		// A 0.16.1 store consolidated after reaching v75: the retired index
		// is gone and the triggers already target the union index. v76 must
		// open it with no error, no notice, and no residue.
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
			t.Fatalf("consolidate after v75: %v", err)
		}
	case "tampered":
		// A tampered v75 store: the retired index is gone but the triggers
		// still target it, so the store is not consolidated and the
		// pre-open check cannot evaluate the predicate. v76 creates the
		// retired side empty, the predicate then fires on every entries
		// posting, and the whole-index rebuild repairs the store.
		if err := sqlitex.ExecuteTransient(conn, `DROP TABLE session_entries_fts`, nil); err != nil {
			t.Fatalf("tamper: drop the retired index: %v", err)
		}
	default:
		t.Fatalf("unknown v76 window %q", window)
	}
}

// v76SentinelRID is the fixed body-half rid of the gate-probe sentinel: far
// above any allocated body_id, so it never collides with a stored row. The
// sentinel is an orphan posting (indexed text with no content-view
// counterpart): a whole-index rebuild drops it while the entries-only
// targeted insert leaves it. An orphan trips the content-aware integrity
// check, so the healed asserts delete it (with its exact planted values)
// before running the check.
const v76SentinelRID = BodyRowIDBase + 777777

// plantBodySentinel plants a stale body-half posting with no stored row: an
// orphan rid whose indexed text has no content-view counterpart. A
// whole-index rebuild re-indexes from the union view (orphan gone); the
// targeted insert touches only the entries half (orphan survives). The
// sentinel therefore probes the SQL gate directly: gone means rebuilt,
// surviving means the clean path ran. Planting touches no live posting.
func plantBodySentinel(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_search_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index) VALUES(?, 'sentinelbody stale', NULL, NULL, ?, 9)`, &sqlitex.ExecOptions{
		Args: []any{v76SentinelRID, v76BodiesSession},
	}); err != nil {
		t.Fatalf("plant stale sentinel posting: %v", err)
	}
}

// removeBodySentinel deletes the gate-probe sentinel with its exact planted
// values, restoring the index to a content-consistent state before the
// integrity check runs.
func removeBodySentinel(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_search_fts(session_search_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index) VALUES('delete', ?, 'sentinelbody stale', NULL, NULL, ?, 9)`, &sqlitex.ExecOptions{
		Args: []any{v76SentinelRID, v76BodiesSession},
	}); err != nil {
		t.Fatalf("remove sentinel posting: %v", err)
	}
}

// openMigrationV76Store builds the v75 state every v76 window case starts
// from, applies the caller's window writes, and plants the body-half
// sentinel that probes the SQL gate. The crossing-from-v74 window instead
// stops at v74 with no sentinel: Open then crosses v75 and v76 in one run,
// and v75 re-creates the union index, which would drop any planted posting.
// The caller owns the connection and must close it.
func openMigrationV76Store(t *testing.T, path, window string, presetNeedsRebuild bool) *sqlite.Conn {
	t.Helper()
	if window == "crossing-from-v74" {
		return buildV74Store(t, path, presetNeedsRebuild)
	}
	conn := buildV75Store(t, path, presetNeedsRebuild)
	applyMigrationV76Window(t, conn, window)
	plantBodySentinel(t, conn)
	return conn
}

// TestMigrationV76ResiduePredicate pins the residue gate against the vendored
// SQLite: silence on a clean store, on inserts alone, and on
// token-count-preserving updates (the blind spot); residue on a delete, on a
// token-count-changing update, and on a reused rowid. The reused-rowid case
// also pins the defect the rebuild exists for: the deleted row's stale term
// matches the new, unrelated row that reused its rowid. The same-count case
// pins the blind spot from the other side: the predicate stays silent while
// the stale term still matches.
func TestMigrationV76ResiduePredicate(t *testing.T) {
	t.Parallel()
	for _, c := range loadMigrationV76PredicateCases(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "v76pred.db")
			conn := openMigrationV76Store(t, path, c.Window, false)
			defer func() {
				if err := conn.Close(); err != nil {
					t.Errorf("close predicate database: %v", err)
				}
			}()
			if c.Window == "reuse" && !matchInFTS(t, conn, "session_search_fts", "kangaroo") {
				t.Fatal("stale kangaroo term does not match the row reusing its rowid; the defect repro is broken")
			}
			if c.Window == "update-same-count" {
				if !matchInFTS(t, conn, "session_search_fts", "photosynthesis") {
					t.Fatal("stale photosynthesis term does not match after the same-count swap; the blind-spot repro is broken")
				}
				if matchInFTS(t, conn, "session_search_fts", "sunflower") {
					t.Fatal("new sunflower term already in the union before any repair; the blind-spot repro is broken")
				}
			}
			if c.Window == "update-count-changed" && !matchInFTS(t, conn, "session_search_fts", "photosynthesis") {
				t.Fatal("stale photosynthesis term does not match after the count-changed update; the repro is broken")
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

// TestMigrationV76OpenNotice drives real upgrades with the notices captured
// through UpgradeOptions.Notices: the writer is an option of the upgrade, so
// no package variable is swapped and the cases run in parallel. The affected
// window prints the one-line rebuild notice and heals the
// window; the crossing, insert, and same-count windows stay silent. The
// notice only warns. The body-half sentinel probes the gate: it is gone after
// a rebuild and survives the targeted insert.
func TestMigrationV76OpenNotice(t *testing.T) {
	t.Parallel()
	for _, c := range loadMigrationV76OpenCases(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "v76open.db")
			conn := openMigrationV76Store(t, path, c.Window, c.PresetNeedsRebuild)
			before, err := searchIndexResiduePresent(conn)
			if c.WantPrecheckWarning {
				if err == nil {
					t.Fatalf("pre-open residue check on the %s window succeeded; the tampered repro is broken", c.Window)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if before != c.WantNotice {
				t.Fatalf("pre-open residue = %v, want %v (the notice follows the predicate)", before, c.WantNotice)
			}
			if c.Window == "affected" && !matchInFTS(t, conn, "session_search_fts", "kangaroo") {
				t.Fatal("stale kangaroo term does not match the reused rowid row before Open; the defect repro is broken")
			}
			if c.Window == "insert" && matchInFTS(t, conn, "session_search_fts", "quasar") {
				t.Fatal("inserted term already in the union before Open; the targeted insert would prove nothing")
			}
			if c.Window == "update-same-count" {
				if !matchInFTS(t, conn, "session_search_fts", "photosynthesis") {
					t.Fatal("stale photosynthesis term does not match before Open; the blind-spot repro is broken")
				}
				if matchInFTS(t, conn, "session_search_fts", "sunflower") {
					t.Fatal("new sunflower term already in the union before Open; the blind-spot repro is broken")
				}
			}
			if c.Window != "crossing-from-v74" && !matchInFTS(t, conn, "session_search_fts", "sentinelbody stale") {
				t.Fatal("stale sentinel posting missing before Open; the gate probe is broken")
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			var notice bytes.Buffer
			if _, err := Upgrade(context.Background(), path, UpgradeOptions{Notices: &notice}); err != nil {
				t.Fatalf("Upgrade with v75 residue: %v", err)
			}
			s, err := Open(path, WithPoolSize(1))
			if err != nil {
				t.Fatalf("Open after the upgrade: %v", err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Errorf("close migrated store: %v", err)
				}
			})
			switch {
			case c.WantPrecheckWarning:
				got := notice.String()
				if !strings.HasPrefix(got, searchIndexPrecheckWarning) || strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
					t.Errorf("notice = %q, want one warning line starting %q", got, searchIndexPrecheckWarning)
				}
			case c.WantNotice:
				if notice.String() != searchIndexRebuildNotice+"\n" {
					t.Errorf("notice = %q, want the one-line rebuild warning", notice.String())
				}
			default:
				if notice.Len() != 0 {
					t.Errorf("notice = %q, want silence on the clean path", notice.String())
				}
			}
			assertMigrationV76OpenHealed(t, s, c)
		})
	}
}

// assertPostOpenBodyIndexed proves the bodies triggers live after Open: a
// body row inserted post-migration is indexed and findable. It reuses the
// caller's pooled connection: the Open tests run with pool size 1, so taking
// a second connection would deadlock.
func assertPostOpenBodyIndexed(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, content_preview, has_tool_use, has_thinking, is_error, depth)
VALUES((SELECT coalesce(max(body_id), `+fmt.Sprintf("%d", BodyRowIDBase-1)+`) + 1 FROM session_entry_bodies), '`+v76BodiesSession+`', '`+strings.Repeat("de", 32)+`', 10, 'claude-code', 'text', 'user', 'postopenbody fresh', 0, 0, 0, 0)`, nil); err != nil {
		t.Fatalf("insert post-open body row: %v", err)
	}
	if !matchInFTS(t, conn, "session_search_fts", "postopenbody") {
		t.Error("union lost the body term added after Open; the bodies triggers must index it")
	}
}

// assertMigrationV76OpenHealed proves the Open-driven end state: union
// triggers live, the retired index gone, every live term findable, and the
// body-half sentinel resolved by the branch taken — gone after a rebuild,
// surviving after the targeted insert. The content-aware integrity check runs
// on every Open: it only matches when the index is corrupt. On the affected
// path only the rebuild expunges ghosts; on the crossing path there was never
// residue; on the same-count path the blind spot keeps the stale term.
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
		t.Error("union lost the seeded body term across Open; body search must survive both branches")
	}
	// The rebuild branch clears a pending rebuild flag; the targeted-insert
	// branch leaves it as it was.
	state, err := searchStateReadOnConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	if state.NeedsRebuild != c.WantNeedsRebuild {
		t.Errorf("needs_rebuild = %v after Open, want %v", state.NeedsRebuild, c.WantNeedsRebuild)
	}
	// The sentinel probes the branch taken: gone after a rebuild, surviving
	// after the targeted insert. Assert first, then remove the orphan before
	// the content-aware integrity check (an orphan trips it by design).
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
		// The rebuild drops the orphan sentinel: it re-indexes from the union
		// view, which has no such row.
		if matchInFTS(t, conn, "session_search_fts", "sentinelbody stale") {
			t.Error("stale sentinel posting survived Open on the affected path; only the rebuild removes it")
		}
		assertPostOpenBodyIndexed(t, conn)
	}
	if c.Window == "tampered" {
		// The pre-open check could not decide, so Open only warned; v76
		// created the retired side empty, the predicate fired, and the
		// whole-index rebuild dropped the orphan sentinel.
		if matchInFTS(t, conn, "session_search_fts", "sentinelbody stale") {
			t.Error("stale sentinel posting survived Open on the tampered path; v76 must rebuild it")
		}
		assertPostOpenBodyIndexed(t, conn)
	}
	if c.Window == "crossing-from-v74" {
		// A v74 store crosses v75 and v76 in one Open: no window between,
		// so no residue and no notice; the union index serves every live
		// row and a body term added after Open is indexed.
		for _, term := range []string{"mitochondria", "chlorophyll", "kangaroo", "turnip"} {
			if !matchInFTS(t, conn, "session_search_fts", term) {
				t.Errorf("union lost live seed term %q across the v74 crossing", term)
			}
		}
		assertPostOpenBodyIndexed(t, conn)
	}
	if c.Window == "insert" {
		// The targeted insert indexed the insert-only window without a rebuild:
		// the row is findable, and the untouched rows are undisturbed.
		if !matchInFTS(t, conn, "session_search_fts", "quasar") {
			t.Error("union lost the inserted term across Open; the targeted insert must index it")
		}
		if !matchInFTS(t, conn, "session_search_fts", "kangaroo") {
			t.Error("union lost the untouched seed term across Open on the insert path")
		}
		// The targeted insert touches only the entries half: the orphan
		// sentinel survives it.
		if !matchInFTS(t, conn, "session_search_fts", "sentinelbody stale") {
			t.Error("stale sentinel posting missing after Open on the insert path; the targeted insert must leave it")
		}
		removeBodySentinel(t, conn)
		// A body term added after Open is indexed: the bodies triggers live.
		assertPostOpenBodyIndexed(t, conn)
	}
	if c.Window == "none" {
		// Crossing path: nothing to heal, sentinel still stale, and a body
		// term added after Open is indexed.
		if !matchInFTS(t, conn, "session_search_fts", "sentinelbody stale") {
			t.Error("stale sentinel posting missing after Open on the crossing path")
		}
		removeBodySentinel(t, conn)
		assertPostOpenBodyIndexed(t, conn)
	}
	if c.Window == "consolidated" {
		// A 0.16.1 store consolidated after v75 opens with no error and no
		// notice: the latch records consolidated, skips the rebuild, and the
		// targeted insert is a no-op over the already-current union. The
		// orphan sentinel survives (entries-only repair), and new bodies index.
		if !matchInFTS(t, conn, "session_search_fts", "sentinelbody stale") {
			t.Error("stale sentinel posting missing after Open on the consolidated path")
		}
		removeBodySentinel(t, conn)
		assertPostOpenBodyIndexed(t, conn)
	}
	if c.Window == "update-same-count" {
		// Blind spot: the predicate stays silent, so no rebuild runs and the
		// stale term still matches after Open.
		if !matchInFTS(t, conn, "session_search_fts", "photosynthesis") {
			t.Error("stale photosynthesis term missing after Open on the same-count path; the blind spot keeps it")
		}
		if matchInFTS(t, conn, "session_search_fts", "sunflower") {
			t.Error("new sunflower term indexed after Open on the same-count path; no repair ran")
		}
		if !matchInFTS(t, conn, "session_search_fts", "sentinelbody stale") {
			t.Error("stale sentinel posting missing after Open on the same-count path")
		}
		removeBodySentinel(t, conn)
		assertPostOpenBodyIndexed(t, conn)
		// The content-aware probe sees the blind spot: the union still
		// indexes the pre-swap term while the stored row carries the new
		// text, so the check reports an error instead of passing clean. That
		// failure IS the pinned outcome: the swap is invisible to the residue
		// gate but visible to the content-aware probe. No other Open path may
		// fail this check.
		if err := ftsContentIntegrityCheck(conn); err == nil {
			t.Error("content-aware integrity check passed on the same-count path; want it to report the stale posting the blind spot leaves")
		}
		return
	}
	// Content-aware integrity check on every other Open, after the orphan
	// probe is removed: any drift or corruption is an error. It runs last so
	// the sentinel - an intentional orphan - never trips it.
	if err := ftsContentIntegrityCheck(conn); err != nil {
		t.Fatalf("open integrity-check: %v", err)
	}
}

// TestSearchBodyRowIDBaseSQLMatchesBodyRowIDBase pins the one SQL spelling
// of the entries/bodies rowid split to the Go constant it stands for, so the
// v76 statements and the bodies allocation cannot drift apart.
func TestSearchBodyRowIDBaseSQLMatchesBodyRowIDBase(t *testing.T) {
	t.Parallel()
	if want := strconv.FormatInt(BodyRowIDBase, 10); searchBodyRowIDBaseSQL != want {
		t.Fatalf("searchBodyRowIDBaseSQL = %s, want BodyRowIDBase %s", searchBodyRowIDBaseSQL, want)
	}
}
