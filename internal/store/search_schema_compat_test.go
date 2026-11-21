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

//go:embed testdata/search_schema_compat.yaml
var searchSchemaCompatYAML []byte

//go:embed testdata/search_schema_compat.manifest.yaml
var searchSchemaCompatManifestYAML []byte

type searchSchemaCompatWantHit struct {
	SessionID  string `yaml:"sessionID"`
	EntryIndex int    `yaml:"entryIndex"`
	Role       string `yaml:"role"`
}

type searchSchemaCompatCase struct {
	Name                 string                      `yaml:"name"`
	Schema               int                         `yaml:"schema"`
	Query                string                      `yaml:"query"`
	WantHasMappingSetSQL bool                        `yaml:"wantHasMappingSetSQL"`
	WantMatchesAny       bool                        `yaml:"wantMatchesAny"`
	WantHits             []searchSchemaCompatWantHit `yaml:"wantHits"`
}

type searchSchemaCompatFixtures struct {
	Cases []searchSchemaCompatCase `yaml:"cases"`
}

func loadSearchSchemaCompatCases(t *testing.T) []searchSchemaCompatCase {
	t.Helper()
	var fixtures searchSchemaCompatFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(searchSchemaCompatYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode search_schema_compat.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(searchSchemaCompatManifestYAML)
	if err != nil {
		t.Fatalf("decode search_schema_compat manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "search schema compat"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

// TestSearchSchemaCompat verifies that production search (SearchMergedOnConn
// and SearchMatchesAnyOnConn) succeeds and returns identical results on
// synthetic stores at schemas 75, 81, and 82, and emits the golden schema-82
// SQL when mapping_set_id is present vs pre-82 SQL when it is absent.
func TestSearchSchemaCompat(t *testing.T) {
	t.Parallel()
	for _, c := range loadSearchSchemaCompatCases(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "compat.db")
			conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if closeErr := conn.Close(); closeErr != nil {
					t.Errorf("close compat db: %v", closeErr)
				}
			}()
			if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(c.Schema)); err != nil {
				t.Fatalf("migrate to schema %d: %v", c.Schema, err)
			}
			seedSearchSchemaCompatStore(t, conn)

			querySQL, _, err := unifiedSearchSQLOnConn(conn, SearchFilters{})
			if err != nil {
				t.Fatalf("unifiedSearchSQLOnConn: %v", err)
			}
			if c.WantHasMappingSetSQL {
				if querySQL != unifiedSearchZeroGolden {
					t.Fatalf("schema %d search SQL drifted from unified_search_zero_filter.sql", c.Schema)
				}
			} else {
				if strings.Contains(querySQL, "mapping_set_id") {
					t.Fatalf("schema %d search SQL unexpectedly references mapping_set_id", c.Schema)
				}
				if !strings.Contains(querySQL, "m.generation_id = g.generation_id") {
					t.Fatalf("schema %d search SQL missing pre-82 generation predicate", c.Schema)
				}
			}

			matched, err := SearchMatchesAnyOnConn(conn, c.Query, SearchFilters{})
			if err != nil {
				t.Fatalf("SearchMatchesAnyOnConn(%q) at schema %d: %v", c.Query, c.Schema, err)
			}
			if matched != c.WantMatchesAny {
				t.Fatalf("SearchMatchesAnyOnConn(%q) at schema %d = %v, want %v", c.Query, c.Schema, matched, c.WantMatchesAny)
			}

			hits, err := SearchMergedOnConn(conn, c.Query, 50, 0, SearchFilters{})
			if err != nil {
				t.Fatalf("SearchMergedOnConn(%q) at schema %d: %v", c.Query, c.Schema, err)
			}
			if len(hits) != len(c.WantHits) {
				t.Fatalf("SearchMergedOnConn(%q) at schema %d returned %d hits, want %d: %+v", c.Query, c.Schema, len(hits), len(c.WantHits), hits)
			}
			for i, want := range c.WantHits {
				got := hits[i]
				if got.SessionID != want.SessionID || got.EntryIndex != want.EntryIndex || got.Role != want.Role {
					t.Fatalf("hit[%d] at schema %d = {SessionID:%q EntryIndex:%d Role:%q}, want %+v",
						i, c.Schema, got.SessionID, got.EntryIndex, got.Role, want)
				}
			}
		})
	}
}

func seedSearchSchemaCompatStore(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	const (
		sid     = "cccccccc-3333-4333-8333-cccccccccccc"
		genID   = "gen-compat-1"
		digest0 = "0000000000000000000000000000000000000000000000000000000000000000"
		digest1 = "1111111111111111111111111111111111111111111111111111111111111111"
		digest2 = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	script := fmt.Sprintf(`
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('compat-host', 'compat-host');
INSERT INTO projects(project_hash) VALUES('compat-project');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, active_generation_id)
VALUES('%s', 'claude-code', 'fixture-model', 'compat-host', 'compat-project', 1, 2, 3, '/synthetic/compat.jsonl', 'jsonl', '%s');
INSERT INTO session_generations(
  session_id, generation_id, schema_version, harness, model, version,
  ts_start, ts_end, ts_ingested, source_file_path, source_format,
  project_hash, project_name, host_slug, content_hash, metadata_hash,
  redaction_applied, completeness, source_evidence_digest, index_format_version,
  candidate_digest, installed_at_ms
) VALUES(
  '%s', '%s', 1, 'claude-code', 'fixture-model', '1.0',
  1, 2, 3, '/synthetic/compat.jsonl', 'jsonl',
  'compat-project', 'compat-project', 'compat-host', 'content-1', '%s',
  0, 'complete', '%s', 2,
  '%s', 3
);
INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, content_preview, has_tool_use, has_thinking, is_error, depth, parent_index)
VALUES(%d, '%s', '%s', 0, 'claude-code', 'text', 'user', 'nebula parent signal', 0, 0, 0, 0, NULL);
INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, content_preview, has_tool_use, has_thinking, is_error, depth, parent_index)
VALUES(%d, '%s', '%s', 1, 'claude-code', 'text', 'assistant', 'quasar child echo', 0, 0, 0, 1, 0);
INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, content_preview, has_tool_use, has_thinking, is_error, depth, parent_index)
VALUES(%d, '%s', '%s', 2, 'claude-code', 'text', 'assistant', 'nebula parent signal', 0, 0, 0, 1, 0);
INSERT INTO session_generation_entries(session_id, generation_id, partition_id, entry_index, body_digest)
VALUES('%s', '%s', 0, 0, '%s'), ('%s', '%s', 0, 1, '%s'), ('%s', '%s', 0, 2, '%s');
INSERT INTO session_search_fts(session_search_fts) VALUES('rebuild');
UPDATE session_search_state SET needs_rebuild = 0 WHERE id = 1;
`,
		sid, genID,
		sid, genID, digest0, digest0, digest0,
		BodyRowIDBase, sid, digest0,
		BodyRowIDBase+1, sid, digest1,
		BodyRowIDBase+2, sid, digest2,
		sid, genID, digest0, sid, genID, digest1, sid, genID, digest2,
	)
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("seed search schema compat store: %v", err)
	}
}
