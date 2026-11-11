package store

import (
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
// and V76; leg B writes land after the final leg. wantLegAUpdatedOldInUnion
// and wantLegAGhostInUnion stay true even on the full chain: FTS5 un-indexes
// by value, so removed terms of rows updated after V75 and ghosts of rows
// deleted after V75 survive any non-rebuild resync (see schema_v76.go). Leg
// B writes go through live union triggers with exact old values and leave no
// residue. The runner hardcodes the invariants: the untouched seed, both body
// terms, and a clean FTS integrity-check on every chain; the legacy-side
// landing of the capped chain's writes (the defect mechanism); and the
// production-path reads, which prove ghosts return no hit while removed terms
// of live rows still return theirs.
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

func loadMigrationV76SearchCases(t *testing.T) []migrationV76SearchCase {
	t.Helper()
	var fixtures struct {
		Cases []migrationV76SearchCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(migrationV76SearchTriggersYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode migration_v76_search_triggers.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(migrationV76SearchTriggersManifestYAML)
	if err != nil {
		t.Fatalf("decode migration_v76_search_triggers manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
		if c.Chain != "full" && c.Chain != "v75" {
			t.Fatalf("migration_v76_search_triggers.yaml: case %q pins chain %q; add it to the runner", c.Name, c.Chain)
		}
		if c.WantTriggersTarget != "union" && c.WantTriggersTarget != "legacy" {
			t.Fatalf("migration_v76_search_triggers.yaml: case %q pins trigger target %q; add it to the runner", c.Name, c.WantTriggersTarget)
		}
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "v76 search triggers"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
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
// Removed terms of rows updated after V75 and ghosts of rows deleted after
// V75 still match raw union MATCH — FTS5 cannot un-index values that no
// longer exist in the table — and the test pins that residue beside the
// production reads, which stay correct for ghosts and noisy for removed
// terms until a future whole-index rebuild.
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

func runMigrationV76SearchCase(t *testing.T, c migrationV76SearchCase) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v76.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close v76 case database: %v", err)
		}
	}()
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(74)); err != nil {
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
		Args: []any{boolToInt(c.PresetNeedsRebuild)},
	}); err != nil {
		t.Fatalf("preset needs_rebuild: %v", err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(75)); err != nil {
		t.Fatal(err)
	}
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
// ghosts of deleted rows return no hit (the unified read filters rows absent
// from session_entries), leg B removed terms are gone (exact triggers), and
// leg A removed terms of live rows still return their row — the documented
// resync residue, relevance noise with current snippets until a future
// whole-index rebuild. Body terms stay out of production asserts: production
// body search is generation-gated (the unified read requires an active
// generation mapping) and this seed carries none, by design. On the capped
// chain the merged read still serves the fresh legacy postings, which is why
// the defect was union-only.
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
		if got := productionHits("chlorophyll"); got != 0 {
			t.Errorf("production search for %q returned %d hits, want none (live triggers un-index exactly)", "chlorophyll", got)
		}
		if got := productionHits("mitochondria"); got == 0 {
			t.Errorf("production search for %q returned no hit, want the resync residue documented", "mitochondria")
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
