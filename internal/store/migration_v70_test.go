package store

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	regexp "github.com/wasilibs/go-re2"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/parent_state.yaml
var parentStateYAML []byte

//go:embed testdata/parent_state.manifest.yaml
var parentStateManifestYAML []byte

// parentStateCase is one stored child and the inputs that decide its parent
// lane. The same row drives the V70 backfill over the predecessor schema and
// the drift check over the production writers.
type parentStateCase struct {
	Name               string `yaml:"name"`
	Child              string `yaml:"child"`
	EvidenceKind       string `yaml:"evidenceKind"`
	EvidenceState      string `yaml:"evidenceState"`
	EvidenceTarget     string `yaml:"evidenceTarget"`
	CacheParent        string `yaml:"cacheParent"`
	Snapshot           string `yaml:"snapshot"`
	InactiveEvidence   bool   `yaml:"inactiveEvidence"`
	WantState          string `yaml:"wantState"`
	WantResolvedParent string `yaml:"wantResolvedParent"`
}

// parentStateRejection is one value the projection column must refuse.
type parentStateRejection struct {
	Name         string `yaml:"name"`
	StateLiteral string `yaml:"stateLiteral"`
}

type parentStateFixture struct {
	Target     string                 `yaml:"target"`
	Unstored   string                 `yaml:"unstored"`
	Snapshot   string                 `yaml:"snapshot"`
	Cases      []parentStateCase      `yaml:"cases"`
	Rejections []parentStateRejection `yaml:"rejections"`
}

type parentStateManifest struct {
	RequiredCases      []string `yaml:"requiredCases"`
	RequiredRejections []string `yaml:"requiredRejections"`
}

func loadParentStateFixture(t *testing.T) parentStateFixture {
	t.Helper()
	var fixture parentStateFixture
	decoder := yaml.NewDecoder(bytes.NewReader(parentStateYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode testdata/parent_state.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("testdata/parent_state.yaml must contain exactly one YAML document: %v", err)
	}
	var manifest parentStateManifest
	decoder = yaml.NewDecoder(bytes.NewReader(parentStateManifestYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode testdata/parent_state.manifest.yaml: %v", err)
	}
	cases := make([]string, 0, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		cases = append(cases, tc.Name)
		for _, token := range []string{tc.EvidenceState, tc.EvidenceTarget, tc.CacheParent, tc.Snapshot, tc.WantResolvedParent} {
			switch token {
			case "", "target", "unstored", "snapshot", "explicit_none", "target_known", "target_known_retained", "unknown", "conflicting_current_native_evidence":
			default:
				t.Fatalf("parent state case %q names the unknown token %q", tc.Name, token)
			}
		}
		switch tc.EvidenceKind {
		case "", "started_by", "context_from":
		default:
			t.Fatalf("parent state case %q names the unknown evidence kind %q", tc.Name, tc.EvidenceKind)
		}
		if _, err := NewParentState(tc.WantState); err != nil {
			t.Fatalf("parent state case %q: %v", tc.Name, err)
		}
	}
	if err := validateRecoveryRequiredNames(manifest.RequiredCases, cases, "parent state"); err != nil {
		t.Fatal(err)
	}
	rejections := make([]string, 0, len(fixture.Rejections))
	for _, rejection := range fixture.Rejections {
		rejections = append(rejections, rejection.Name)
	}
	if err := validateRecoveryRequiredNames(manifest.RequiredRejections, rejections, "parent state rejection"); err != nil {
		t.Fatal(err)
	}
	if fixture.Target == "" || fixture.Unstored == "" || fixture.Snapshot == "" {
		t.Fatal("parent state fixture must name the stored, unstored, and snapshot parent identifiers")
	}
	return fixture
}

// parentStateIDs resolves the fixture tokens for one case.
func parentStateIDs(t *testing.T, fixture parentStateFixture, tc parentStateCase) (child string, cacheParent string, evidenceTarget string, snapshot string) {
	t.Helper()
	child = tc.Child
	switch tc.CacheParent {
	case "":
	case "target":
		cacheParent = fixture.Target
	default:
		t.Fatalf("case %q names the unknown cache token %q", tc.Name, tc.CacheParent)
	}
	switch tc.EvidenceTarget {
	case "":
	case "target":
		evidenceTarget = fixture.Target
	case "unstored":
		evidenceTarget = fixture.Unstored
	default:
		t.Fatalf("case %q names the unknown evidence token %q", tc.Name, tc.EvidenceTarget)
	}
	switch tc.Snapshot {
	case "":
	case "snapshot":
		snapshot = fixture.Snapshot
	default:
		t.Fatalf("case %q names the unknown snapshot token %q", tc.Name, tc.Snapshot)
	}
	return child, cacheParent, evidenceTarget, snapshot
}

// schemaSlotBeforeV70 is the migration slot the V70 test seeds on. It is pinned
// by number, not len(Migrations)-1, so a later migration cannot silently move
// the predecessor past V70 and turn the backfill test into a no-op.
const schemaSlotBeforeV70 = 69

// openPredecessorConn opens a database migrated through exactly the migrations
// that shipped before V70, so the V70 test seeds the state an installed store
// holds.
func openPredecessorConn(t *testing.T) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), "v70.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open predecessor database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := sqlitemigration.Migrate(context.Background(), conn, frozenSchema(schemaSlotBeforeV70)); err != nil {
		t.Fatalf("migrate predecessor schema: %v", err)
	}
	return conn
}

func TestMigrationV70BackfillsParentState(t *testing.T) {
	t.Parallel()
	fixture := loadParentStateFixture(t)
	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			conn := openPredecessorConn(t)
			child, cacheParent, evidenceTarget, snapshot := parentStateIDs(t, fixture, tc)
			seedParentStateRow(t, conn, fixture.Target, child, cacheParent, tc.EvidenceKind, tc.EvidenceState, evidenceTarget, snapshot, tc.InactiveEvidence)
			if err := sqlitemigration.Migrate(context.Background(), conn, dbSchema); err != nil {
				t.Fatalf("migrate to V70: %v", err)
			}
			state, resolved := readParentState(t, conn, child)
			assertParentState(t, tc, fixture, state, resolved)
		})
	}
}

// seedParentStateRow stores one fixture row on the predecessor schema, before
// the projection columns exist. It seeds the availability cache, the managed
// metadata ParentUUID snapshot, and the generation evidence the case names, so
// the backfill and the drift test describe the same stored state.
func seedParentStateRow(t *testing.T, conn *sqlite.Conn, target, child, cacheParent, evidenceKind, evidenceState, evidenceTarget, snapshot string, inactiveEvidence bool) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, `
INSERT OR IGNORE INTO host_slugs(opaque_id, host_slug) VALUES('state-host','state-host');
INSERT OR IGNORE INTO projects(project_hash, canonical_cwd) VALUES('state-project','/synthetic/state');
INSERT OR IGNORE INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('`+target+`','claude-code','state-model','state-host','state-project',1,2,3,'/synthetic/parent.jsonl','jsonl');`, nil); err != nil {
		t.Fatalf("seed the stored parent row: %v", err)
	}
	if cacheParent != "" {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions(session_id, parent_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?, ?, 'claude-code','state-model','state-host','state-project',1,2,3,'/synthetic/child.jsonl','jsonl')`, &sqlitex.ExecOptions{
			Args: []any{child, cacheParent},
		}); err != nil {
			t.Fatalf("seed the child with its availability cache: %v", err)
		}
	} else if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?, 'claude-code','state-model','state-host','state-project',1,2,3,'/synthetic/child.jsonl','jsonl')`, &sqlitex.ExecOptions{
		Args: []any{child},
	}); err != nil {
		t.Fatalf("seed the child row: %v", err)
	}
	if snapshot != "" {
		metadata := `{"parentUuid":"` + snapshot + `"}`
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_publication_metadata(session_id, capture_revision, schema_version, metadata_json, metadata_hash, content_hash, captured_at)
VALUES(?, 1, 1, ?, ?, ?, 1)`, &sqlitex.ExecOptions{
			Args: []any{child, metadata, strings.Repeat("a", 64), strings.Repeat("b", 64)},
		}); err != nil {
			t.Fatalf("seed the managed metadata snapshot: %v", err)
		}
	}
	if inactiveEvidence {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET active_generation_id = 'active' WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{child}}); err != nil {
			t.Fatalf("put the inactive child generation in scope: %v", err)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref)
VALUES(?, 'old-one', 0, 'started_by', 'explicit_none', NULL, 'native_typed', '', '', ''),
       (?, 'old-two', 0, 'started_by', 'target_known', ?, 'native_typed', '', '', '')`, &sqlitex.ExecOptions{
			Args: []any{child, child, target},
		}); err != nil {
			t.Fatalf("seed the inactive-generation evidence: %v", err)
		}
		return
	}
	if evidenceState == "" {
		return
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET active_generation_id = 'gen-state' WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{child}}); err != nil {
		t.Fatalf("put the child generation in scope: %v", err)
	}
	kind := evidenceKind
	if kind == "" {
		kind = "started_by"
	}
	var targetValue any
	if evidenceTarget != "" {
		targetValue = evidenceTarget
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref)
VALUES(?, 'gen-state', 0, ?, ?, ?, 'native_typed', '', '', '')`, &sqlitex.ExecOptions{
		Args: []any{child, kind, evidenceState, targetValue},
	}); err != nil {
		t.Fatalf("seed the started_by evidence: %v", err)
	}
}

func readParentState(t *testing.T, conn *sqlite.Conn, sessionID string) (string, *string) {
	t.Helper()
	var state string
	var resolved *string
	if err := sqlitex.ExecuteTransient(conn, `SELECT parent_state, resolved_parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			state = stmt.ColumnText(0)
			if stmt.ColumnType(1) != sqlite.TypeNull {
				value := stmt.ColumnText(1)
				resolved = &value
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read the parent lane of %s: %v", sessionID, err)
	}
	return state, resolved
}

func assertParentState(t *testing.T, tc parentStateCase, fixture parentStateFixture, state string, resolved *string) {
	t.Helper()
	if state != tc.WantState {
		t.Fatalf("%s: parent_state = %q, want %q", tc.Name, state, tc.WantState)
	}
	want := ""
	switch tc.WantResolvedParent {
	case "target":
		want = fixture.Target
	case "unstored":
		want = fixture.Unstored
	case "snapshot":
		want = fixture.Snapshot
	}
	if want == "" {
		if resolved != nil {
			t.Fatalf("%s: resolved_parent_id = %q, want NULL", tc.Name, *resolved)
		}
		return
	}
	if resolved == nil {
		t.Fatalf("%s: resolved_parent_id = NULL, want %q", tc.Name, want)
	}
	if *resolved != want {
		t.Fatalf("%s: resolved_parent_id = %q, want %q", tc.Name, *resolved, want)
	}
}

func TestMigrationV70ParentStateRefusesNullAndUnknown(t *testing.T) {
	t.Parallel()
	fixture := loadParentStateFixture(t)
	conn := openPredecessorConn(t)
	if err := sqlitemigration.Migrate(context.Background(), conn, dbSchema); err != nil {
		t.Fatalf("migrate to V70: %v", err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT OR IGNORE INTO host_slugs(opaque_id, host_slug) VALUES('reject-host','reject-host');
INSERT OR IGNORE INTO projects(project_hash, canonical_cwd) VALUES('reject-project','/synthetic/reject');`, nil); err != nil {
		t.Fatalf("seed the rejection parents: %v", err)
	}
	for _, rejection := range fixture.Rejections {
		rejection := rejection
		t.Run(rejection.Name, func(t *testing.T) {
			err := sqlitex.ExecuteTransient(conn, fmt.Sprintf(`INSERT INTO sessions(session_id, parent_state, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?, %s, 'claude-code','state-model','reject-host','reject-project',1,2,3,'/synthetic/reject.jsonl','jsonl')`, rejection.StateLiteral), &sqlitex.ExecOptions{
				Args: []any{rejection.Name},
			})
			if err == nil {
				t.Fatalf("%s: the projection column accepted %s; the NOT NULL/CHECK contract is missing", rejection.Name, rejection.StateLiteral)
			}
			if !strings.Contains(err.Error(), "parent_state") {
				t.Fatalf("%s: the insert failed for an unrelated reason, not the closed parent-state contract: %v", rejection.Name, err)
			}
		})
	}
	// The resolved_parent_id CHECK is a row invariant, not a trigger-only
	// convention: neither direction of an inconsistent projection may persist.
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('pair-check','claude-code','state-model','reject-host','reject-project',1,2,3,'/synthetic/pair.jsonl','jsonl')`, &sqlitex.ExecOptions{}); err != nil {
		t.Fatalf("insert the pair-check session: %v", err)
	}
	for _, update := range []string{
		`UPDATE sessions SET parent_state = 'resolved', resolved_parent_id = NULL WHERE session_id = 'pair-check'`,
		`UPDATE sessions SET parent_state = 'root', resolved_parent_id = 'some-parent' WHERE session_id = 'pair-check'`,
	} {
		if err := sqlitex.ExecuteTransient(conn, update, &sqlitex.ExecOptions{}); err == nil {
			t.Fatalf("the resolved_parent_id CHECK accepted an inconsistent pair: %s", update)
		}
	}
}

// TestMigrationV70ParentStateCheckBijective proves the stored CHECK admits
// exactly AllParentStates. SQLite cannot ALTER a CHECK, so a widened or
// narrowed set needs a new migration and fails here until one lands.
func TestMigrationV70ParentStateCheckBijective(t *testing.T) {
	t.Parallel()
	var matches []string
	for _, statement := range strings.Split(migrationV70, ";") {
		flat := strings.Join(strings.Fields(statement), " ")
		if strings.Contains(flat, "ADD COLUMN parent_state TEXT NOT NULL DEFAULT 'root'") && strings.Contains(flat, "IN (") {
			matches = append(matches, flat)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one parent_state column statement carrying an IN (...) CHECK, found %d", len(matches))
	}
	statement := matches[0]
	rest := statement[strings.Index(statement, "IN (")+len("IN ("):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("parent_state CHECK has an unterminated IN (...) clause: %s", statement)
	}
	got := make(map[string]bool)
	for _, part := range strings.Split(rest[:end], ",") {
		got[strings.Trim(strings.TrimSpace(part), "'")] = true
	}
	want := make(map[string]bool, len(AllParentStates))
	for _, state := range AllParentStates {
		want[string(state)] = true
	}
	for value := range got {
		if !want[value] {
			t.Errorf("sessions.parent_state CHECK accepts %q, which is not in AllParentStates; add a rebuild migration that drops it", value)
		}
	}
	for value := range want {
		if !got[value] {
			t.Errorf("AllParentStates carries %q, which the sessions.parent_state CHECK refuses; add a rebuild migration that admits it", value)
		}
	}
}

// TestMigrationV70SessionsUpdateTriggerCoversSchema keeps the trigger's UPDATE
// OF list exhaustive. Every writable sessions column that can change a page
// must fire the trigger; only the projection outputs and the publication
// trigger's own columns are excluded, because firing on them would re-enter.
func TestMigrationV70SessionsUpdateTriggerCoversSchema(t *testing.T) {
	t.Parallel()
	s, _ := openGenerationStore(t)
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	trigger := readTriggerSQL(t, conn, "sessions_parent_state_au")
	match := regexp.MustCompile(`(?s)AFTER UPDATE OF (.*?) ON sessions`).FindStringSubmatch(trigger)
	if len(match) != 2 {
		t.Fatalf("cannot read the UPDATE OF column list from the trigger: %s", trigger)
	}
	fired := make(map[string]bool)
	for _, column := range strings.Split(match[1], ",") {
		fired[strings.TrimSpace(column)] = true
	}
	excluded := map[string]bool{
		"session_id":                           true,
		"parent_state":                         true,
		"resolved_parent_id":                   true,
		"indexed_publication_capture_revision": true,
		"cwd_provenance_kind":                  true,
	}
	want := make(map[string]bool)
	for _, column := range sessionsColumnNames(t, conn) {
		if !excluded[column] {
			want[column] = true
		}
	}
	for column := range want {
		if !fired[column] {
			t.Errorf("sessions column %q can change a page but is missing from the parent-state trigger's UPDATE OF list; add it to parentStateSessionsTriggerFiresOn and ship a migration that redefines the trigger", column)
		}
	}
	for column := range fired {
		if !want[column] {
			t.Errorf("the parent-state trigger fires on %q, which is not a page-relevant sessions column; remove it, because an excluded projection or publication column would re-enter the trigger", column)
		}
	}
}

func sessionsColumnNames(t *testing.T, conn *sqlite.Conn) []string {
	t.Helper()
	var names []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM pragma_table_info('sessions')`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			names = append(names, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("read the sessions column names: %v", err)
	}
	return names
}

func readTriggerSQL(t *testing.T, conn *sqlite.Conn, name string) string {
	t.Helper()
	var sql string
	if err := sqlitex.ExecuteTransient(conn, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, &sqlitex.ExecOptions{
		Args: []any{name},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sql = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read trigger %s: %v", name, err)
	}
	if sql == "" {
		t.Fatalf("trigger %s is absent; the migration did not install it", name)
	}
	return sql
}

// TestMigrationV70PageQueriesUseTheirIndexes proves the root and child reads
// have an ordered index each. Without the root index the read would filter the
// whole sessions table; without the child index it would scan the resolved set.
func TestMigrationV70PageQueriesUseTheirIndexes(t *testing.T) {
	t.Parallel()
	s, _ := openGenerationStore(t)
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	// Explain the statement production actually runs, including the
	// session_metrics join and the OFFSET, not a reduced stand-in.
	rootQuery, rootArgs := offsetPageSQL(ParentStateRoot, "", SyncWindowFilter{Publication: schema.LocalSyncPublicationAll}, 26, 0)
	rootPlan := explainQueryPlan(t, conn, rootQuery, rootArgs...)
	if !strings.Contains(rootPlan, "idx_sessions_root_page") {
		t.Fatalf("the root page plan does not use idx_sessions_root_page: %s", rootPlan)
	}
	childQuery, childArgs := offsetPageSQL(ParentStateResolved, "any-parent", SyncWindowFilter{Publication: schema.LocalSyncPublicationAll}, 26, 0)
	childPlan := explainQueryPlan(t, conn, childQuery, childArgs...)
	if !strings.Contains(childPlan, "idx_sessions_child_page") {
		t.Fatalf("the child page plan does not use idx_sessions_child_page: %s", childPlan)
	}
	rootDDL := readIndexSQL(t, conn, "idx_sessions_root_page")
	if !strings.Contains(rootDDL, "(start_ms DESC, session_id ASC)") || !strings.Contains(rootDDL, "parent_state = 'root'") {
		t.Fatalf("idx_sessions_root_page key order or partial predicate changed: %s", rootDDL)
	}
	childDDL := readIndexSQL(t, conn, "idx_sessions_child_page")
	if !strings.Contains(childDDL, "(resolved_parent_id, start_ms DESC, session_id ASC)") || !strings.Contains(childDDL, "parent_state = 'resolved'") {
		t.Fatalf("idx_sessions_child_page key order or partial predicate changed: %s", childDDL)
	}
}

func explainQueryPlan(t *testing.T, conn *sqlite.Conn, query string, args ...any) string {
	t.Helper()
	var details []string
	if err := sqlitex.ExecuteTransient(conn, "EXPLAIN QUERY PLAN "+query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			details = append(details, stmt.ColumnText(stmt.ColumnCount()-1))
			return nil
		},
	}); err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	return strings.Join(details, "\n")
}

func readIndexSQL(t *testing.T, conn *sqlite.Conn, name string) string {
	t.Helper()
	var sql string
	if err := sqlitex.ExecuteTransient(conn, `SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, &sqlitex.ExecOptions{
		Args: []any{name},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sql = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	if sql == "" {
		t.Fatalf("index %s is absent", name)
	}
	return sql
}

// TestParentStateTriggerTextMatchesGolden pins the exact trigger SQL to a
// checked-in golden file. It is deliberately NOT a comparison against
// LogicalParentSQL: that would compare the generator to itself and pass through
// any rule change. The golden is the reviewed text; a rule change updates the
// golden in the same diff and, once V70 has shipped, lands as a NEW migration.
// Update it deliberately with UPDATE_PARENT_STATE_TRIGGER_GOLDEN=1.
func TestParentStateTriggerTextMatchesGolden(t *testing.T) {
	t.Parallel()
	s, _ := openGenerationStore(t)
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	var text strings.Builder
	for _, name := range []string{
		"sessions_parent_state_ai", "sessions_parent_state_au", "sessions_parent_state_ad",
		"session_relationship_evidence_parent_state_ai",
		"session_relationship_evidence_parent_state_au",
		"session_relationship_evidence_parent_state_ad",
		"session_publication_metadata_parent_state_ai",
		"session_publication_metadata_parent_state_au",
		"session_publication_metadata_parent_state_ad",
	} {
		text.WriteString("-- " + name + "\n")
		text.WriteString(readTriggerSQL(t, conn, name))
		text.WriteString("\n\n")
	}
	path := filepath.Join("testdata", "parent_state_triggers.golden.sql")
	if os.Getenv("UPDATE_PARENT_STATE_TRIGGER_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(text.String()), 0o644); err != nil {
			t.Fatalf("write the parent-state trigger golden: %v", err)
		}
		return
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the parent-state trigger golden: %v", err)
	}
	if string(golden) != text.String() {
		t.Fatalf("the parent-state trigger text differs from testdata/parent_state_triggers.golden.sql; the parent rule or a trigger changed. Review the new text and update the golden deliberately with UPDATE_PARENT_STATE_TRIGGER_GOLDEN=1, then land a new migration if V70 has shipped")
	}
}

// TestParentStateAllStatesAreStable keeps the closed set in one place: a state
// that NewParentState accepts must be one AllParentStates lists.
func TestParentStateAllStatesAreStable(t *testing.T) {
	t.Parallel()
	for _, state := range AllParentStates {
		parsed, err := NewParentState(state.String())
		if err != nil || parsed != state {
			t.Fatalf("NewParentState(%q) = %v, %v; every listed state must parse to itself", state, parsed, err)
		}
	}
	if _, err := NewParentState("child"); err == nil {
		t.Fatal("NewParentState accepted a state outside the closed set")
	}
	if !slices.Contains(AllParentStates, ParentStateRoot) || !slices.Contains(AllParentStates, ParentStateResolved) || !slices.Contains(AllParentStates, ParentStateUnresolved) {
		t.Fatalf("AllParentStates = %v, want root, resolved, and unresolved", AllParentStates)
	}
}
