package store

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harmonized_tables.yaml
var harmonizedTablesYAML []byte

//go:embed testdata/harmonized_tables.manifest.yaml
var harmonizedTablesManifestYAML []byte

type harmonizedCheck struct {
	Name     string         `yaml:"name"`
	Column   string         `yaml:"column"`
	Tables   []string       `yaml:"tables"`
	Also     map[string]any `yaml:"also"`
	Accepted []any          `yaml:"accepted"`
	Rejected []any          `yaml:"rejected"`
}

type harmonizedNullability struct {
	Name     string   `yaml:"name"`
	Table    string   `yaml:"table"`
	Nullable []string `yaml:"nullable"`
	NotNull  []string `yaml:"notNull"`
}

type harmonizedFixtures struct {
	Checks      []harmonizedCheck       `yaml:"checks"`
	Nullability []harmonizedNullability `yaml:"nullability"`
}

func loadHarmonizedFixtures(t *testing.T) harmonizedFixtures {
	t.Helper()
	var fixtures harmonizedFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(harmonizedTablesYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode harmonized_tables.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(harmonizedTablesManifestYAML)
	if err != nil {
		t.Fatalf("decode harmonized_tables manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Checks)+len(fixtures.Nullability))
	for _, c := range fixtures.Checks {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Column) == "" {
			t.Fatalf("harmonized_tables.yaml: a check has a blank name or column; name every closed set")
		}
		if len(c.Tables) == 0 {
			t.Fatalf("harmonized_tables.yaml: check %q names no tables", c.Name)
		}
		if len(c.Accepted) == 0 || len(c.Rejected) == 0 {
			t.Fatalf("harmonized_tables.yaml: check %q must carry accepted and rejected values", c.Name)
		}
		actual = append(actual, c.Name)
	}
	for _, n := range fixtures.Nullability {
		if strings.TrimSpace(n.Name) == "" || strings.TrimSpace(n.Table) == "" {
			t.Fatalf("harmonized_tables.yaml: a nullability case has a blank name or table")
		}
		actual = append(actual, n.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "harmonized tables"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// harmonizedProbeDB migrates an empty database to the v62 head and seeds the
// shared parents every probe row references.
type harmonizedProbeDB struct {
	conn     *sqlite.Conn
	seq      int
	session  string
	genKids  string
	genEntry string
	genBlob  string
	body     string
	content  string
	segment  string
	section  string
}

func openHarmonizedProbeDB(t *testing.T) *harmonizedProbeDB {
	t.Helper()
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), "harmonized-tables.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := sqlitemigration.Migrate(t.Context(), conn, dbSchema); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	db := &harmonizedProbeDB{conn: conn, session: "s-tables"}
	execHarmonized(t, conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('tables-host','tables-host');
INSERT INTO projects(project_hash, canonical_cwd) VALUES('tables-project','/synthetic/tables');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version)
VALUES('s-tables','opencode','tables-model','tables-host','tables-project',1,2,3,'/synthetic/tables.jsonl','jsonl',11);`)
	db.genKids = "gen-kids"
	db.genEntry = "gen-entry"
	db.genBlob = "gen-blob"
	for _, gen := range []string{db.genKids, db.genEntry, db.genBlob, "gen-seg", "gen-sec"} {
		insertHarmonizedGeneration(t, db, "s-tables", gen)
	}
	db.body = fmt.Sprintf("%064d", 7)
	insertHarmonizedBody(t, db, "s-tables", BodyRowIDBase+7, db.body)
	db.content = fmt.Sprintf("%064d", 8)
	execHarmonized(t, conn, fmt.Sprintf(
		`INSERT INTO session_content(session_id, digest, byte_length) VALUES('s-tables','%s',10)`, db.content))
	db.segment = "gen-seg"
	execHarmonized(t, conn, `INSERT INTO session_context_segments(session_id, generation_id, segment_ordinal, physical_source_id, coordinate_kind, inclusion, captured_refs_json)
VALUES('s-tables','gen-seg',0,'phys','entries','retained','[]')`)
	db.section = "gen-sec"
	execHarmonized(t, conn, `INSERT INTO session_projection_sections(session_id, generation_id, partition_id) VALUES('s-tables','gen-sec',0)`)
	// Start probe keys far above every setup key so no probe collides with a
	// seeded row (or with another probe) on a PRIMARY or UNIQUE key.
	db.seq = 100000
	return db
}

func execHarmonized(t *testing.T, conn *sqlite.Conn, script string) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("exec SQL: %v\nscript: %.200s", err, script)
	}
}

func uniqDigest(n int) string {
	return fmt.Sprintf("%064d", 1000+n)
}

func harmonizedBlobColumn(table, column string) bool {
	return (table == "session_content_chunks" && column == "data") ||
		(table == "session_generations" && column == "prior_evidence")
}

func harmonizedArg(table, column string, value any) any {
	if s, ok := value.(string); ok && harmonizedBlobColumn(table, column) {
		return []byte(s)
	}
	return value
}

func insertHarmonizedGeneration(t *testing.T, db *harmonizedProbeDB, sid, gen string) {
	t.Helper()
	digest := uniqDigest(db.seq)
	db.seq++
	cols := []string{"session_id", "generation_id", "schema_version", "harness", "model", "version",
		"ts_start", "ts_end", "source_format", "project_hash", "project_name", "host_slug",
		"content_hash", "metadata_hash", "redaction_applied", "completeness",
		"source_evidence_digest", "index_format_version", "candidate_digest", "installed_at_ms"}
	args := []any{sid, gen, 11, string(defaults.HarnessOpenCode), "m", "v", 1, 2, string(ingest.SourceFormatJSONL), "p", "pn", "h",
		digest, digest, 0, "complete", digest, 2, digest, 1}
	execHarmonizedInsert(t, db.conn, "session_generations", cols, args)
}

func insertHarmonizedBody(t *testing.T, db *harmonizedProbeDB, sid string, bodyID int64, digest string) {
	t.Helper()
	execHarmonizedInsert(t, db.conn, "session_entry_bodies",
		[]string{"body_id", "session_id", "body_digest", "entry_index", "harness", "entry_type", "role", "has_tool_use", "has_thinking", "is_error", "depth"},
		[]any{bodyID, sid, digest, 0, string(defaults.HarnessOpenCode), "text", "user", 0, 0, 0, 0})
}

func execHarmonizedInsert(t *testing.T, conn *sqlite.Conn, table string, cols []string, args []any) error {
	t.Helper()
	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = "?"
	}
	return sqlitex.ExecuteTransient(conn,
		fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", table, strings.Join(cols, ","), strings.Join(placeholders, ",")),
		&sqlitex.ExecOptions{Args: args})
}

// TestHarmonizedTablesClosedSets pins every new v62 table's allowed values
// and nullability at the SQL boundary, driven from the typed fixture. Each
// CHECK admits its accepted values and refuses its rejected ones; each
// nullable column accepts NULL and each NOT NULL column refuses it.
func TestHarmonizedTablesClosedSets(t *testing.T) {
	t.Parallel()
	fixtures := loadHarmonizedFixtures(t)
	db := openHarmonizedProbeDB(t)
	for _, c := range fixtures.Checks {
		for _, table := range c.Tables {
			if table == "session_search_state" {
				assertSearchStateCheck(t, db, c)
				continue
			}
			if table == "sessions" {
				assertSweepFlagCheck(t, db, c)
				continue
			}
			for _, value := range c.Accepted {
				if err := probeHarmonizedInsert(t, db, table, c.Column, value, c.Also); err != nil {
					t.Errorf("%s/%s: value %v was rejected: %v", c.Name, table, value, err)
				}
			}
			for _, value := range c.Rejected {
				if err := probeHarmonizedInsert(t, db, table, c.Column, value, c.Also); err == nil {
					t.Errorf("%s/%s: value %v was accepted; the CHECK must refuse it", c.Name, table, value)
				}
			}
		}
	}
	for _, n := range fixtures.Nullability {
		assertHarmonizedNullability(t, db, n)
	}
}

// probeHarmonizedInsert inserts a template row into table with column set to
// value (plus the Also companions) using fresh unique keys per attempt, so
// each probe tests its CHECK rather than a key collision.
func probeHarmonizedInsert(t *testing.T, db *harmonizedProbeDB, table, column string, value any, also map[string]any) error {
	t.Helper()
	db.seq++
	n := db.seq
	overrides := map[string]any{column: harmonizedArg(table, column, value)}
	for k, v := range also {
		overrides[k] = harmonizedArg(table, k, v)
	}
	cols, args := harmonizedTemplate(t, db, table, n, overrides)
	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = "?"
	}
	return sqlitex.ExecuteTransient(db.conn,
		fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", table, strings.Join(cols, ","), strings.Join(placeholders, ",")),
		&sqlitex.ExecOptions{Args: args})
}

func harmonizedTemplate(t *testing.T, db *harmonizedProbeDB, table string, n int, overrides map[string]any) ([]string, []any) {
	t.Helper()
	sid := db.session
	var cols []string
	var args []any
	set := func(col string, value any) {
		cols = append(cols, col)
		if override, ok := overrides[col]; ok {
			args = append(args, override)
		} else {
			args = append(args, harmonizedArg(table, col, value))
		}
	}
	switch table {
	case "session_entry_bodies":
		set("body_id", BodyRowIDBase+int64(n))
		set("session_id", sid)
		set("body_digest", uniqDigest(n))
		set("entry_index", 0)
		set("harness", string(defaults.HarnessOpenCode))
		set("entry_type", "text")
		set("role", "user")
		set("timestamp_ms", 1)
		set("content_preview", "x")
		set("tokens_in", 1)
		set("tokens_out", 2)
		set("has_tool_use", 0)
		set("tool_kind", "kind")
		set("tool_names_csv", "a,b")
		set("has_thinking", 0)
		set("is_error", 0)
		set("stop_reason", "stop")
		set("raw_byte_length", 10)
		set("tool_call_id", "t")
		set("entry_id", "e")
		set("parent_entry_id", "p")
		set("depth", 0)
		set("parent_index", 0)
		set("tool_input", "{}")
		set("tool_output", "{}")
		set("model_id", "m")
		set("tokens_reasoning", 1)
		set("cache_read", 2)
		set("cache_write", 3)
		set("extra", `{"a":1}`)
		set("extra_verbatim", nil)
		set("part_type", "part")
		set("source_entry_ref", "e1")
		set("prov_origin", "o")
		set("prov_actor", "a")
		set("prov_delivery", "d")
		set("prov_ownership", "w")
		set("prov_evidence", "e")
		set("prov_input_modality", "i")
		set("prov_submission_ref", "s")
	case "session_content":
		set("session_id", sid)
		set("digest", uniqDigest(n))
		set("byte_length", 10)
	case "session_content_chunks":
		set("session_id", sid)
		set("digest", db.content)
		set("chunk_index", n)
		set("data", "x")
	case "session_generations":
		digest := uniqDigest(n)
		set("session_id", sid)
		set("generation_id", fmt.Sprintf("gen-probe-%d", n))
		set("schema_version", 11)
		set("harness", string(defaults.HarnessOpenCode))
		set("model", "m")
		set("version", "v")
		set("ts_start", 1)
		set("ts_end", 2)
		set("ts_ingested", 1)
		set("source_file_path", "x")
		set("source_format", string(ingest.SourceFormatJSONL))
		set("git_branch", "b")
		set("git_remote", "r")
		set("git_worktree", "w")
		set("git_tracking", "t")
		set("project_hash", "p")
		set("project_file_path", "f")
		set("project_name", "pn")
		set("host_slug", "h")
		set("root_session_id", "r")
		set("purpose", "p")
		set("cwd", "/x")
		set("derived_at", 1)
		set("content_hash", digest)
		set("metadata_hash", digest)
		set("redaction_applied", 0)
		set("redaction_level", "standard")
		set("redaction_rule_set_version", "v1")
		set("redaction_at_ms", 1)
		set("redaction_content_hash_at_redact", digest)
		set("adapter_version", 1)
		set("diagnostics_partial", 0)
		set("completeness", "complete")
		set("source_evidence_digest", digest)
		set("index_format_version", 2)
		set("candidate_digest", digest)
		set("prior_evidence", "x")
		set("installed_at_ms", 1)
		set("activated_at_ms", 2)
	case "session_generation_entries":
		set("session_id", sid)
		set("generation_id", db.genEntry)
		set("partition_id", 0)
		set("entry_index", n)
		set("source_entry_ref", "e1")
		set("body_digest", db.body)
	case "session_generation_content":
		set("session_id", sid)
		set("generation_id", db.genBlob)
		set("source_entry_ref", fmt.Sprintf("ref-%d", n))
		set("digest", db.content)
	case "session_generation_subagents":
		set("session_id", sid)
		set("generation_id", db.genKids)
		set("ordinal", n)
		set("subagent_session_id", "sub")
		set("parent_uuid", "p")
	case "session_generation_commits":
		set("session_id", sid)
		set("generation_id", db.genKids)
		set("ordinal", n)
		set("hash", "h")
		set("message", "m")
		set("author_name", "a")
		set("author_email", "e")
		set("commit_time", 1)
		set("author_time", 2)
	case "session_generation_associations":
		set("session_id", sid)
		set("generation_id", db.genKids)
		set("ordinal", n)
		set("association_id", "a")
		set("observed_commit_hash", "o")
	case "session_generation_diagnostics":
		set("session_id", sid)
		set("generation_id", db.genKids)
		set("ordinal", n)
		set("error_type", "e")
		set("location", "l")
		set("message", "m")
		set("remediation", "r")
	case "session_generation_title_refs":
		set("session_id", sid)
		set("generation_id", db.genKids)
		set("ordinal", n)
		set("source_entry_ref", "e")
	case "session_captured_stats":
		statSid := fmt.Sprintf("s-stats-%d", n)
		execHarmonized(t, db.conn, fmt.Sprintf(`
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version)
VALUES('%s','opencode','m','tables-host','tables-project',1,2,3,'/s.jsonl','jsonl',11)`, statSid))
		set("session_id", statSid)
		set("turn_count", 1)
		set("input_submission_count", 2)
		set("tool_call_count", 3)
		set("subagent_count", 0)
		set("duration_ms", 4)
		set("tokens_in", 5)
		set("tokens_out", 6)
		set("thought_tokens", 7)
		set("cached_read_tokens", 8)
		set("cached_write_tokens", 9)
		set("seed_json", `{"turnCount":1}`)
		set("source", "harness")
		set("updated_at_ms", 10)
		set("overflow", nil)
	case "session_context_segment_refs":
		set("session_id", sid)
		set("generation_id", db.segment)
		set("segment_ordinal", 0)
		set("ordinal", n)
		set("source_entry_ref", fmt.Sprintf("e-%d", n))
	case "session_section_native_metadata":
		set("session_id", sid)
		set("generation_id", db.section)
		set("partition_id", 0)
		set("ordinal", n)
		set("native_id", "n")
		set("kind", "k")
		set("source_entry_ref", "e")
		set("source_type", "t")
		set("source_message_role", "r")
		set("attachment_turn_index", 1)
		set("attachment_tool_call_id", "c")
		set("custom_type", "t")
		set("data", "{}")
	case "annotation_target_entries":
		set("annotation_id", fmt.Sprintf("ann-%d", n))
		set("session_id", sid)
		set("entry_index", 0)
		set("end_index", 1)
	default:
		t.Fatalf("no probe template for table %s; add one", table)
	}
	return cols, args
}

func assertSearchStateCheck(t *testing.T, db *harmonizedProbeDB, c harmonizedCheck) {
	t.Helper()
	if c.Column != "needs_rebuild" {
		t.Fatalf("unsupported search_state check column %q", c.Column)
	}
	for _, value := range c.Accepted {
		if err := sqlitex.ExecuteTransient(db.conn, `UPDATE session_search_state SET needs_rebuild=? WHERE id=1`, &sqlitex.ExecOptions{Args: []any{value}}); err != nil {
			t.Errorf("%s: value %v was rejected: %v", c.Name, value, err)
		}
	}
	for _, value := range c.Rejected {
		if err := sqlitex.ExecuteTransient(db.conn, `UPDATE session_search_state SET needs_rebuild=? WHERE id=1`, &sqlitex.ExecOptions{Args: []any{value}}); err == nil {
			t.Errorf("%s: value %v was accepted; the CHECK must refuse it", c.Name, value)
		}
	}
	if err := sqlitex.ExecuteTransient(db.conn, `UPDATE session_search_state SET needs_rebuild=0 WHERE id=1`, nil); err != nil {
		t.Fatal(err)
	}
}

func assertSweepFlagCheck(t *testing.T, db *harmonizedProbeDB, c harmonizedCheck) {
	t.Helper()
	if c.Column != "content_sweep_pending" {
		t.Fatalf("unsupported sessions check column %q", c.Column)
	}
	for _, value := range c.Accepted {
		sid := fmt.Sprintf("s-sweep-%d", db.seq)
		db.seq++
		execHarmonized(t, db.conn, fmt.Sprintf(`
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version)
VALUES('%s','opencode','m','tables-host','tables-project',1,2,3,'/s.jsonl','jsonl',11)`, sid))
		if err := sqlitex.ExecuteTransient(db.conn, `UPDATE sessions SET content_sweep_pending=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{value, sid}}); err != nil {
			t.Errorf("%s: value %v was rejected: %v", c.Name, value, err)
		}
	}
	for _, value := range c.Rejected {
		sid := fmt.Sprintf("s-sweep-%d", db.seq)
		db.seq++
		execHarmonized(t, db.conn, fmt.Sprintf(`
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version)
VALUES('%s','opencode','m','tables-host','tables-project',1,2,3,'/s.jsonl','jsonl',11)`, sid))
		if err := sqlitex.ExecuteTransient(db.conn, `UPDATE sessions SET content_sweep_pending=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{value, sid}}); err == nil {
			t.Errorf("%s: value %v was accepted; the CHECK must refuse it", c.Name, value)
		}
	}
}

func assertHarmonizedNullability(t *testing.T, db *harmonizedProbeDB, n harmonizedNullability) {
	t.Helper()
	for _, col := range n.Nullable {
		if err := probeHarmonizedInsert(t, db, n.Table, col, nil, nil); err != nil {
			t.Errorf("%s: column %s refuses NULL: %v", n.Name, col, err)
		}
	}
	for _, col := range n.NotNull {
		if err := probeHarmonizedInsert(t, db, n.Table, col, nil, nil); err == nil {
			t.Errorf("%s: column %s accepts NULL; it must be NOT NULL", n.Name, col)
		}
	}
}
