package store

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migration_v62.yaml
var migrationV62YAML []byte

//go:embed testdata/migration_v62.manifest.yaml
var migrationV62ManifestYAML []byte

type v62GenerationSeed struct {
	ID            string `yaml:"id"`
	MetadataJSON  string `yaml:"metadataJson"`
	InstalledAtMs int64  `yaml:"installedAtMs"`
	ActivatedAtMs *int64 `yaml:"activatedAtMs"`
}

type v62ExpectedStats struct {
	TurnCount            *int    `yaml:"turnCount"`
	InputSubmissionCount *int64  `yaml:"inputSubmissionCount"`
	ToolCallCount        *int    `yaml:"toolCallCount"`
	SubagentCount        *int    `yaml:"subagentCount"`
	DurationMs           *int64  `yaml:"durationMs"`
	TokensIn             *int    `yaml:"tokensIn"`
	TokensOut            *int    `yaml:"tokensOut"`
	ThoughtTokens        *int    `yaml:"thoughtTokens"`
	CachedReadTokens     *int    `yaml:"cachedReadTokens"`
	CachedWriteTokens    *int    `yaml:"cachedWriteTokens"`
	SeedJSON             *string `yaml:"seedJson"`
	Source               string  `yaml:"source"`
	UpdatedAtMs          int64   `yaml:"updatedAtMs"`
}

type v62StatsCase struct {
	Name             string              `yaml:"name"`
	SessionID        string              `yaml:"sessionId"`
	ActiveGeneration *string             `yaml:"activeGeneration"`
	Generations      []v62GenerationSeed `yaml:"generations"`
	Expected         *v62ExpectedStats   `yaml:"expected"`
	ExpectedFlag     int                 `yaml:"expectedFlag"`
}

type v62AnnotationRow struct {
	Name         string `yaml:"name"`
	AnnotationID string `yaml:"annotationId"`
	SessionID    string `yaml:"sessionId"`
	EntryIndex   int    `yaml:"entryIndex"`
	EndIndex     int    `yaml:"endIndex"`
}

type v62EvidenceRow struct {
	Name                      string  `yaml:"name"`
	SessionID                 string  `yaml:"sessionId"`
	GenerationID              string  `yaml:"generationId"`
	Kind                      string  `yaml:"kind"`
	TargetState               string  `yaml:"targetState"`
	TargetLocalID             *string `yaml:"targetLocalId"`
	Evidence                  string  `yaml:"evidence"`
	Anchor                    *string `yaml:"anchor"`
	ExpectedAnchorKind        *string `yaml:"expectedAnchorKind"`
	ExpectedAnchorEntryRef    *string `yaml:"expectedAnchorEntryRef"`
	ExpectedAnchorRevisionRef *string `yaml:"expectedAnchorRevisionRef"`
	ExpectedOrdinal           *int    `yaml:"expectedOrdinal"`
}

type v62ExpectedRef struct {
	Ordinal int    `yaml:"ordinal"`
	Ref     string `yaml:"ref"`
}

type v62SegmentCase struct {
	Name                    string           `yaml:"name"`
	SessionID               string           `yaml:"sessionId"`
	GenerationID            string           `yaml:"generationId"`
	SegmentOrdinal          int              `yaml:"segmentOrdinal"`
	LogicalSessionID        *string          `yaml:"logicalSessionId"`
	PhysicalSourceID        string           `yaml:"physicalSourceId"`
	CoordinateKind          string           `yaml:"coordinateKind"`
	StartCoordinate         *int64           `yaml:"startCoordinate"`
	EndExclusive            *int64           `yaml:"endExclusive"`
	DecodedByteStart        *int64           `yaml:"decodedByteStart"`
	DecodedByteEndExclusive *int64           `yaml:"decodedByteEndExclusive"`
	Inclusion               string           `yaml:"inclusion"`
	CapturedRefsJSON        string           `yaml:"capturedRefsJson"`
	ExpectedRefs            []v62ExpectedRef `yaml:"expectedRefs"`
}

type v62ExpectedRecord struct {
	Ordinal              int     `yaml:"ordinal"`
	NativeID             string  `yaml:"nativeId"`
	Kind                 string  `yaml:"kind"`
	SourceEntryRef       string  `yaml:"sourceEntryRef"`
	SourceType           string  `yaml:"sourceType"`
	SourceMessageRole    *string `yaml:"sourceMessageRole"`
	AttachmentTurnIndex  *int    `yaml:"attachmentTurnIndex"`
	AttachmentToolCallID *string `yaml:"attachmentToolCallId"`
	CustomType           *string `yaml:"customType"`
	Data                 *string `yaml:"data"`
}

type v62SectionCase struct {
	Name            string              `yaml:"name"`
	SessionID       string              `yaml:"sessionId"`
	GenerationID    string              `yaml:"generationId"`
	PartitionID     int                 `yaml:"partitionId"`
	EarlierState    *string             `yaml:"earlierState"`
	NativeMetadata  *string             `yaml:"nativeMetadata"`
	ExpectedRecords []v62ExpectedRecord `yaml:"expectedRecords"`
}

type v62Fixtures struct {
	StatsCases     []v62StatsCase     `yaml:"statsCases"`
	AnnotationRows []v62AnnotationRow `yaml:"annotationRows"`
	EvidenceRows   []v62EvidenceRow   `yaml:"evidenceRows"`
	SegmentCases   []v62SegmentCase   `yaml:"segmentCases"`
	SectionCases   []v62SectionCase   `yaml:"sectionCases"`
}

func loadMigrationV62Fixtures(t *testing.T) v62Fixtures {
	t.Helper()
	var fixtures v62Fixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(migrationV62YAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode migration_v62.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(migrationV62ManifestYAML)
	if err != nil {
		t.Fatalf("decode migration_v62 manifest: %v", err)
	}
	actual := make([]string, 0)
	for _, c := range fixtures.StatsCases {
		actual = append(actual, c.Name)
	}
	for _, r := range fixtures.AnnotationRows {
		actual = append(actual, r.Name)
	}
	for _, r := range fixtures.EvidenceRows {
		actual = append(actual, r.Name)
	}
	for _, c := range fixtures.SegmentCases {
		actual = append(actual, c.Name)
	}
	for _, c := range fixtures.SectionCases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "v62 migration"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestMigrationV62Backfills proves the v62 migration carries its backfills and
// rebuilds over a populated predecessor: stats rows and seed documents from
// the active generation metadata, the sweep flag, the annotation targets
// without their foreign key, and the shredded generation-keyed tables.
//
// A V61 database is seeded from the typed fixture, then migrated to V62. The
// migration must preserve every seeded row, extract the stats the live store
// shape carries, and shred the JSON columns so the structured rows reassemble
// byte-identically through the schema package.
func TestMigrationV62Backfills(t *testing.T) {
	t.Parallel()
	fixtures := loadMigrationV62Fixtures(t)
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "v62-backfill.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	predecessor := sqlitemigration.Schema{
		Migrations:       dbSchema.Migrations[:61],
		MigrationOptions: dbSchema.MigrationOptions[:61],
	}
	if err := sqlitemigration.Migrate(ctx, conn, predecessor); err != nil {
		t.Fatalf("migrate to V61: %v", err)
	}

	seedV62Predecessor(t, conn, fixtures)

	if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
		t.Fatalf("migrate to V62: %v", err)
	}

	assertV62StatsBackfill(t, conn, fixtures)
	assertV62AnnotationRebuild(t, conn, fixtures)
	assertV62EvidenceShred(t, conn, fixtures)
	assertV62SegmentShred(t, conn, fixtures)
	assertV62SectionShred(t, conn, fixtures)
	assertV62SearchStore(t, conn)
	assertV62Immutability(t, conn)

	// The synthetic annotation rows have no parents by construction (seeded
	// with foreign keys off); remove them before the integrity gate so the
	// gate proves the migration's own references, not the test setup.
	execV62SQL(t, conn, `DELETE FROM annotation_target_entries WHERE annotation_id LIKE 'ann-v62-%'`)
	assertV62NoFKViolations(t, conn)

	var userVersion int
	queryV62Row(t, conn, "PRAGMA user_version", nil, func(stmt *sqlite.Stmt) {
		userVersion = int(stmt.ColumnInt64(0))
	})
	if userVersion != CurrentSchemaVersion() {
		t.Fatalf("user_version after migration = %d, want the build schema %d", userVersion, CurrentSchemaVersion())
	}
	if got := v62ObjectSQL(t, conn, "index", "idx_session_projection_entries_partition"); got != "" {
		t.Fatalf("duplicate partition index survived v62: %s", got)
	}
}

// seedV62Predecessor populates a V61 database with the fixture sessions,
// generations, evidence, segments, sections, and annotation targets. Foreign
// keys stay off during seeding: the annotation rows deliberately carry no
// parents, and the migration itself runs with foreign keys off.
func seedV62Predecessor(t *testing.T, conn *sqlite.Conn, fixtures v62Fixtures) {
	t.Helper()
	execV62SQL(t, conn, `PRAGMA foreign_keys=OFF`)
	execV62SQL(t, conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('v62-host','v62-host');
INSERT INTO projects(project_hash, canonical_cwd) VALUES('v62-project','/synthetic/v62');`)
	seen := map[string]bool{}
	declareSession := func(sessionID string, active *string) {
		if seen[sessionID] {
			return
		}
		seen[sessionID] = true
		activeValue := "NULL"
		if active != nil {
			activeValue = "'" + *active + "'"
		}
		execV62SQL(t, conn, fmt.Sprintf(`
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id)
VALUES('%s','opencode','v62-model','v62-host','v62-project',1,2,3,'/synthetic/v62.jsonl','jsonl',11,%s);`, sessionID, activeValue))
	}
	digest := strings.Repeat("0", 64)
	for _, c := range fixtures.StatsCases {
		declareSession(c.SessionID, c.ActiveGeneration)
		for _, g := range c.Generations {
			activated := "NULL"
			if g.ActivatedAtMs != nil {
				activated = fmt.Sprintf("%d", *g.ActivatedAtMs)
			}
			execV62SQL(t, conn, fmt.Sprintf(`
INSERT INTO session_projection_generations(session_id, generation_id, metadata_json, source_evidence_digest, completeness, index_format_version, installed_at_ms, activated_at_ms)
VALUES('%s','%s','%s','%s','complete',2,%d,%s);`,
				c.SessionID, g.ID, escapeV62Literal(t, g.MetadataJSON), digest, g.InstalledAtMs, activated))
		}
	}
	for _, r := range fixtures.EvidenceRows {
		declareSession(r.SessionID, nil)
		execV62SQL(t, conn, fmt.Sprintf(`
INSERT INTO session_relationship_evidence(session_id, generation_id, kind, target_state, target_local_id, evidence, anchor)
VALUES('%s','%s','%s','%s',%s,%s,%s);`,
			r.SessionID, r.GenerationID, r.Kind, r.TargetState,
			nullableV62Literal(r.TargetLocalID), quoteV62Literal(r.Evidence), nullableV62JSON(r.Anchor)))
	}
	for _, c := range fixtures.SegmentCases {
		declareSession(c.SessionID, nil)
		execV62SQL(t, conn, fmt.Sprintf(`
INSERT INTO session_context_segments(session_id, generation_id, segment_ordinal, logical_session_id, physical_source_id, coordinate_kind, start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion, captured_refs_json)
VALUES('%s','%s',%d,%s,'%s','%s',%s,%s,%s,%s,'%s','%s');`,
			c.SessionID, c.GenerationID, c.SegmentOrdinal,
			nullableV62Literal(c.LogicalSessionID), c.PhysicalSourceID, c.CoordinateKind,
			nullableV62Int(c.StartCoordinate), nullableV62Int(c.EndExclusive),
			nullableV62Int(c.DecodedByteStart), nullableV62Int(c.DecodedByteEndExclusive),
			c.Inclusion, escapeV62Literal(t, c.CapturedRefsJSON)))
	}
	for _, c := range fixtures.SectionCases {
		declareSession(c.SessionID, nil)
		execV62SQL(t, conn, fmt.Sprintf(`
INSERT INTO session_projection_sections(session_id, generation_id, partition_id, earlier_state, native_metadata)
VALUES('%s','%s',%d,%s,%s);`,
			c.SessionID, c.GenerationID, c.PartitionID,
			nullableV62Literal(c.EarlierState), nullableV62JSON(c.NativeMetadata)))
	}
	for _, r := range fixtures.AnnotationRows {
		execV62SQL(t, conn, fmt.Sprintf(`
INSERT INTO annotation_target_entries(annotation_id, session_id, entry_index, end_index)
VALUES('%s','%s',%d,%d);`, r.AnnotationID, r.SessionID, r.EntryIndex, r.EndIndex))
	}
}

func escapeV62Literal(t *testing.T, s string) string {
	t.Helper()
	if strings.Contains(s, "'") {
		t.Fatalf("fixture literal carries a single quote; keep fixture JSON free of quotes that need escaping: %.40q", s)
	}
	return s
}

func quoteV62Literal(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func nullableV62Literal(s *string) string {
	if s == nil {
		return "NULL"
	}
	return quoteV62Literal(*s)
}

func nullableV62JSON(s *string) string {
	if s == nil {
		return "NULL"
	}
	if strings.Contains(*s, "'") {
		return quoteV62Literal(*s)
	}
	return "'" + *s + "'"
}

func nullableV62Int(v *int64) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%d", *v)
}

func execV62SQL(t *testing.T, conn *sqlite.Conn, script string) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("exec SQL: %v\nscript: %.200s", err, script)
	}
}

func queryV62Row(t *testing.T, conn *sqlite.Conn, query string, args []any, fn func(*sqlite.Stmt)) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		fn(stmt)
		return nil
	}}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
}

// v62ObjectSQL returns the sqlite_master SQL for a schema object, or "" when
// the object is absent. indexDefinition fails on absence, so presence checks
// for triggers and dropped indexes need this absence-tolerant form.
func v62ObjectSQL(t *testing.T, conn *sqlite.Conn, objType, name string) string {
	t.Helper()
	sql := ""
	queryV62Row(t, conn, `SELECT sql FROM sqlite_master WHERE type=? AND name=?`, []any{objType, name}, func(stmt *sqlite.Stmt) {
		sql = stmt.ColumnText(0)
	})
	return sql
}

func assertV62StatsBackfill(t *testing.T, conn *sqlite.Conn, fixtures v62Fixtures) {
	t.Helper()
	for _, c := range fixtures.StatsCases {
		var flag int
		queryV62Row(t, conn, `SELECT content_sweep_pending FROM sessions WHERE session_id=?`, []any{c.SessionID}, func(stmt *sqlite.Stmt) {
			flag = int(stmt.ColumnInt64(0))
		})
		if flag != c.ExpectedFlag {
			t.Errorf("%s: content_sweep_pending = %d, want %d", c.Name, flag, c.ExpectedFlag)
		}
		if c.Expected == nil {
			rows := 0
			queryV62Row(t, conn, `SELECT COUNT(*) FROM session_captured_stats WHERE session_id=?`, []any{c.SessionID}, func(stmt *sqlite.Stmt) {
				rows = int(stmt.ColumnInt64(0))
			})
			if rows != 0 {
				t.Errorf("%s: session_captured_stats holds %d rows, want none", c.Name, rows)
			}
			continue
		}
		got := map[string]any{}
		cols := []string{"turn_count", "input_submission_count", "tool_call_count", "subagent_count", "duration_ms", "tokens_in", "tokens_out", "thought_tokens", "cached_read_tokens", "cached_write_tokens", "seed_json", "source", "updated_at_ms", "overflow"}
		queryV62Row(t, conn, `SELECT turn_count, input_submission_count, tool_call_count, subagent_count, duration_ms, tokens_in, tokens_out, thought_tokens, cached_read_tokens, cached_write_tokens, seed_json, source, updated_at_ms, overflow FROM session_captured_stats WHERE session_id=?`, []any{c.SessionID}, func(stmt *sqlite.Stmt) {
			for i, col := range cols {
				if stmt.ColumnType(i) == sqlite.TypeNull {
					got[col] = nil
				} else {
					got[col] = stmt.ColumnText(i)
				}
			}
		})
		if len(got) == 0 {
			t.Fatalf("%s: no session_captured_stats row for %s", c.Name, c.SessionID)
		}
		checkInt := func(col string, v *int) {
			t.Helper()
			if v == nil {
				if got[col] != nil {
					t.Errorf("%s: %s = %v, want NULL", c.Name, col, got[col])
				}
				return
			}
			if got[col] != fmt.Sprintf("%d", *v) {
				t.Errorf("%s: %s = %v, want %d", c.Name, col, got[col], *v)
			}
		}
		checkInt64 := func(col string, v *int64) {
			t.Helper()
			if v == nil {
				if got[col] != nil {
					t.Errorf("%s: %s = %v, want NULL", c.Name, col, got[col])
				}
				return
			}
			if got[col] != fmt.Sprintf("%d", *v) {
				t.Errorf("%s: %s = %v, want %d", c.Name, col, got[col], *v)
			}
		}
		checkInt("turn_count", c.Expected.TurnCount)
		checkInt64("input_submission_count", c.Expected.InputSubmissionCount)
		checkInt("tool_call_count", c.Expected.ToolCallCount)
		checkInt("subagent_count", c.Expected.SubagentCount)
		checkInt64("duration_ms", c.Expected.DurationMs)
		checkInt("tokens_in", c.Expected.TokensIn)
		checkInt("tokens_out", c.Expected.TokensOut)
		checkInt("thought_tokens", c.Expected.ThoughtTokens)
		checkInt("cached_read_tokens", c.Expected.CachedReadTokens)
		checkInt("cached_write_tokens", c.Expected.CachedWriteTokens)
		if c.Expected.SeedJSON == nil {
			if got["seed_json"] != nil {
				t.Errorf("%s: seed_json = %v, want NULL", c.Name, got["seed_json"])
			}
		} else if got["seed_json"] != *c.Expected.SeedJSON {
			t.Errorf("%s: seed_json = %.80v, want %.80s", c.Name, got["seed_json"], *c.Expected.SeedJSON)
		} else if valid := 0; true {
			queryV62Row(t, conn, `SELECT json_valid(seed_json) FROM session_captured_stats WHERE session_id=?`, []any{c.SessionID}, func(stmt *sqlite.Stmt) {
				valid = int(stmt.ColumnInt64(0))
			})
			if valid != 1 {
				t.Errorf("%s: seed_json fails json_valid", c.Name)
			}
		}
		if got["source"] != c.Expected.Source {
			t.Errorf("%s: source = %v, want %s", c.Name, got["source"], c.Expected.Source)
		}
		if got["updated_at_ms"] != fmt.Sprintf("%d", c.Expected.UpdatedAtMs) {
			t.Errorf("%s: updated_at_ms = %v, want %d", c.Name, got["updated_at_ms"], c.Expected.UpdatedAtMs)
		}
		if got["overflow"] != nil {
			t.Errorf("%s: overflow = %v, want NULL", c.Name, got["overflow"])
		}
	}
}

func assertV62AnnotationRebuild(t *testing.T, conn *sqlite.Conn, fixtures v62Fixtures) {
	t.Helper()
	for _, r := range fixtures.AnnotationRows {
		found := false
		queryV62Row(t, conn, `SELECT session_id, entry_index, end_index FROM annotation_target_entries WHERE annotation_id=?`, []any{r.AnnotationID}, func(stmt *sqlite.Stmt) {
			found = true
			if stmt.ColumnText(0) != r.SessionID || int(stmt.ColumnInt64(1)) != r.EntryIndex || int(stmt.ColumnInt64(2)) != r.EndIndex {
				t.Errorf("%s: rebuilt row = (%s,%d,%d), want (%s,%d,%d)", r.Name,
					stmt.ColumnText(0), stmt.ColumnInt64(1), stmt.ColumnInt64(2),
					r.SessionID, r.EntryIndex, r.EndIndex)
			}
		})
		if !found {
			t.Errorf("%s: annotation target row %s missing after rebuild", r.Name, r.AnnotationID)
		}
	}
	definition := ""
	queryV62Row(t, conn, `SELECT sql FROM sqlite_master WHERE type='table' AND name='annotation_target_entries'`, nil, func(stmt *sqlite.Stmt) {
		definition = stmt.ColumnText(0)
	})
	if strings.Contains(definition, "session_entries") {
		t.Errorf("rebuilt annotation_target_entries still references session_entries: %s", definition)
	}
	if !strings.Contains(definition, "REFERENCES annotations(id)") {
		t.Errorf("rebuilt annotation_target_entries lost its annotations cascade: %s", definition)
	}
	if sql := v62ObjectSQL(t, conn, "index", "idx_ann_target_entry"); !strings.Contains(sql, "annotation_target_entries") {
		t.Errorf("idx_ann_target_entry missing or misplaced after rebuild: %s", sql)
	}
	views := 0
	queryV62Row(t, conn, `SELECT COUNT(*) FROM annotations_with_target`, nil, func(stmt *sqlite.Stmt) {
		views = int(stmt.ColumnInt64(0))
	})
	_ = views
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO annotation_target_entries(annotation_id, session_id, entry_index, end_index) VALUES('ann-v62-check','s-v62-full',5,5)`, nil); err == nil {
		t.Errorf("rebuilt annotation_target_entries admits end_index <= entry_index; the CHECK must hold")
	} else {
		_ = sqlitex.ExecuteTransient(conn, `DELETE FROM annotation_target_entries WHERE annotation_id='ann-v62-check'`, nil)
	}
}

func assertV62EvidenceShred(t *testing.T, conn *sqlite.Conn, fixtures v62Fixtures) {
	t.Helper()
	for _, r := range fixtures.EvidenceRows {
		var kind, targetState, evidence string
		var anchorKind, anchorEntryRef, anchorRevisionRef any
		var ordinal int64
		found := false
		queryV62Row(t, conn, `SELECT kind, target_state, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref, ordinal FROM session_relationship_evidence WHERE session_id=? AND generation_id=? AND kind=?`, []any{r.SessionID, r.GenerationID, r.Kind}, func(stmt *sqlite.Stmt) {
			found = true
			kind, targetState, evidence = stmt.ColumnText(0), stmt.ColumnText(1), stmt.ColumnText(2)
			anchorKind = v62NullableText(stmt, 3)
			anchorEntryRef = v62NullableText(stmt, 4)
			anchorRevisionRef = v62NullableText(stmt, 5)
			ordinal = stmt.ColumnInt64(6)
		})
		if !found {
			t.Errorf("%s: evidence row missing after rebuild", r.Name)
			continue
		}
		if r.ExpectedOrdinal != nil && ordinal != int64(*r.ExpectedOrdinal) {
			t.Errorf("%s: ordinal = %d, want %d: the rebuild must preserve document order", r.Name, ordinal, *r.ExpectedOrdinal)
		}
		if kind != r.Kind || targetState != r.TargetState || evidence != r.Evidence {
			t.Errorf("%s: preserved columns changed: (%s,%s,%s)", r.Name, kind, targetState, evidence)
		}
		checkNullable := func(label string, got any, want *string) {
			t.Helper()
			if want == nil {
				if got != nil {
					t.Errorf("%s: %s = %v, want NULL", r.Name, label, got)
				}
				return
			}
			if got != *want {
				t.Errorf("%s: %s = %v, want %s", r.Name, label, got, *want)
			}
		}
		checkNullable("anchor_kind", anchorKind, r.ExpectedAnchorKind)
		checkNullable("anchor_source_entry_ref", anchorEntryRef, r.ExpectedAnchorEntryRef)
		checkNullable("anchor_source_revision_ref", anchorRevisionRef, r.ExpectedAnchorRevisionRef)
	}
}

func v62NullableText(stmt *sqlite.Stmt, col int) any {
	if stmt.ColumnType(col) == sqlite.TypeNull {
		return nil
	}
	return stmt.ColumnText(col)
}

func assertV62SegmentShred(t *testing.T, conn *sqlite.Conn, fixtures v62Fixtures) {
	t.Helper()
	for _, c := range fixtures.SegmentCases {
		var cols [9]string
		var nulls [9]bool
		found := false
		queryV62Row(t, conn, `SELECT logical_session_id, physical_source_id, coordinate_kind, start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion FROM session_context_segments WHERE session_id=? AND generation_id=? AND segment_ordinal=?`, []any{c.SessionID, c.GenerationID, c.SegmentOrdinal}, func(stmt *sqlite.Stmt) {
			found = true
			for i := 0; i < 8; i++ {
				if stmt.ColumnType(i) == sqlite.TypeNull {
					nulls[i] = true
				} else {
					cols[i] = stmt.ColumnText(i)
				}
			}
		})
		if !found {
			t.Errorf("%s: segment row missing after rebuild", c.Name)
			continue
		}
		if cols[1] != c.PhysicalSourceID || cols[2] != c.CoordinateKind || cols[7] != c.Inclusion {
			t.Errorf("%s: preserved segment columns changed", c.Name)
		}
		refs := []v62ExpectedRef{}
		queryV62Row(t, conn, `SELECT ordinal, source_entry_ref FROM session_context_segment_refs WHERE session_id=? AND generation_id=? AND segment_ordinal=? ORDER BY ordinal`, []any{c.SessionID, c.GenerationID, c.SegmentOrdinal}, func(stmt *sqlite.Stmt) {
			refs = append(refs, v62ExpectedRef{Ordinal: int(stmt.ColumnInt64(0)), Ref: stmt.ColumnText(1)})
		})
		if !reflect.DeepEqual(refs, append([]v62ExpectedRef{}, c.ExpectedRefs...)) && len(refs)+len(c.ExpectedRefs) > 0 {
			if len(refs) != len(c.ExpectedRefs) {
				t.Errorf("%s: shredded %d refs, want %d", c.Name, len(refs), len(c.ExpectedRefs))
			} else {
				for i := range refs {
					if refs[i] != c.ExpectedRefs[i] {
						t.Errorf("%s: ref %d = %+v, want %+v", c.Name, i, refs[i], c.ExpectedRefs[i])
					}
				}
			}
		}
	}
}

func assertV62SectionShred(t *testing.T, conn *sqlite.Conn, fixtures v62Fixtures) {
	t.Helper()
	for _, c := range fixtures.SectionCases {
		found := false
		queryV62Row(t, conn, `SELECT earlier_state FROM session_projection_sections WHERE session_id=? AND generation_id=? AND partition_id=?`, []any{c.SessionID, c.GenerationID, c.PartitionID}, func(stmt *sqlite.Stmt) {
			found = true
			if c.EarlierState == nil {
				if stmt.ColumnType(0) != sqlite.TypeNull {
					t.Errorf("%s: earlier_state = %q, want NULL", c.Name, stmt.ColumnText(0))
				}
			} else if stmt.ColumnText(0) != *c.EarlierState {
				t.Errorf("%s: earlier_state = %q, want %q", c.Name, stmt.ColumnText(0), *c.EarlierState)
			}
		})
		if !found {
			t.Errorf("%s: section row missing after rebuild", c.Name)
			continue
		}
		type storedRecord struct {
			ordinal int
			cols    [9]any
		}
		var stored []storedRecord
		queryV62Row(t, conn, `SELECT ordinal, native_id, kind, source_entry_ref, source_type, source_message_role, attachment_turn_index, attachment_tool_call_id, custom_type, data FROM session_section_native_metadata WHERE session_id=? AND generation_id=? AND partition_id=? ORDER BY ordinal`, []any{c.SessionID, c.GenerationID, c.PartitionID}, func(stmt *sqlite.Stmt) {
			rec := storedRecord{ordinal: int(stmt.ColumnInt64(0))}
			for i := 1; i <= 9; i++ {
				rec.cols[i-1] = v62NullableText(stmt, i)
			}
			stored = append(stored, rec)
		})
		if len(stored) != len(c.ExpectedRecords) {
			t.Errorf("%s: shredded %d metadata records, want %d", c.Name, len(stored), len(c.ExpectedRecords))
			continue
		}
		rebuilt := make([]schema.NativeMetadataRecord, 0, len(stored))
		for i, want := range c.ExpectedRecords {
			got := stored[i]
			if got.ordinal != want.Ordinal {
				t.Errorf("%s: record ordinal = %d, want %d", c.Name, got.ordinal, want.Ordinal)
			}
			str := func(label string, got any, wantVal string) string {
				t.Helper()
				s, ok := got.(string)
				if !ok || s != wantVal {
					t.Errorf("%s: record %d %s = %v, want %s", c.Name, want.Ordinal, label, got, wantVal)
					return ""
				}
				return s
			}
			optStr := func(label string, got any, wantVal *string) *string {
				t.Helper()
				if wantVal == nil {
					if got != nil {
						t.Errorf("%s: record %d %s = %v, want NULL", c.Name, want.Ordinal, label, got)
					}
					return nil
				}
				s, ok := got.(string)
				if !ok || s != *wantVal {
					t.Errorf("%s: record %d %s = %v, want %s", c.Name, want.Ordinal, label, got, *wantVal)
					return nil
				}
				out := s
				return &out
			}
			optInt := func(label string, got any, wantVal *int) *int {
				t.Helper()
				if wantVal == nil {
					if got != nil {
						t.Errorf("%s: record %d %s = %v, want NULL", c.Name, want.Ordinal, label, got)
					}
					return nil
				}
				var n int
				if _, err := fmt.Sscanf(got.(string), "%d", &n); err != nil || n != *wantVal {
					t.Errorf("%s: record %d %s = %v, want %d", c.Name, want.Ordinal, label, got, *wantVal)
					return nil
				}
				out := n
				return &out
			}
			nativeID := str("native_id", got.cols[0], want.NativeID)
			kind := str("kind", got.cols[1], want.Kind)
			entryRef := str("source_entry_ref", got.cols[2], want.SourceEntryRef)
			sourceType := str("source_type", got.cols[3], want.SourceType)
			messageRole := optStr("source_message_role", got.cols[4], want.SourceMessageRole)
			turnIndex := optInt("attachment_turn_index", got.cols[5], want.AttachmentTurnIndex)
			toolCallID := optStr("attachment_tool_call_id", got.cols[6], want.AttachmentToolCallID)
			customType := optStr("custom_type", got.cols[7], want.CustomType)
			var data json.RawMessage
			if want.Data == nil {
				if got.cols[8] != nil {
					t.Errorf("%s: record %d data = %v, want NULL", c.Name, want.Ordinal, got.cols[8])
				}
			} else {
				s, ok := got.cols[8].(string)
				if !ok || s != *want.Data {
					t.Errorf("%s: record %d data = %.60v, want %.60s", c.Name, want.Ordinal, got.cols[8], *want.Data)
				} else {
					data = json.RawMessage(s)
				}
			}
			rec := schema.NativeMetadataRecord{ID: nativeID, Kind: schema.NativeMetadataKind(kind)}
			rec.Source.EntryRef = schema.SourceEntryRef(entryRef)
			rec.Source.SourceType = schema.NativeMetadataSourceType(sourceType)
			if messageRole != nil {
				rec.Source.MessageRole = schema.NativePiMessageRole(*messageRole)
			}
			if turnIndex != nil || toolCallID != nil {
				rec.Attachment = &schema.NativeAttachmentRef{ToolCallID: ""}
				if turnIndex != nil {
					rec.Attachment.TurnIndex = turnIndex
				}
				if toolCallID != nil {
					rec.Attachment.ToolCallID = *toolCallID
				}
			}
			if customType != nil {
				rec.CustomType = *customType
			}
			rec.Data = data
			rebuilt = append(rebuilt, rec)
		}
		if c.NativeMetadata == nil {
			continue
		}
		// The shredded rows must reassemble to the original bytes: unmarshal
		// the stored document and the rebuilt rows into the same struct type
		// and require identical Go-marshaled bytes, so a future serializer
		// restores the document exactly.
		var original []schema.NativeMetadataRecord
		if err := json.Unmarshal([]byte(*c.NativeMetadata), &original); err != nil {
			t.Fatalf("%s: fixture nativeMetadata does not decode: %v", c.Name, err)
		}
		if original == nil {
			// The JSON null scalar shreds to zero rows under the array
			// filter; there is nothing to reassemble.
			continue
		}
		originalBytes, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		if string(originalBytes) != *c.NativeMetadata {
			t.Fatalf("%s: fixture nativeMetadata is not in Go-marshal canonical form; the reassembly check needs canonical bytes", c.Name)
		}
		rebuiltBytes, err := json.Marshal(rebuilt)
		if err != nil {
			t.Fatal(err)
		}
		if string(rebuiltBytes) != *c.NativeMetadata {
			t.Errorf("%s: reassembled metadata differs:\n got %.200s\nwant %.200s", c.Name, string(rebuiltBytes), *c.NativeMetadata)
		}
	}
}

func assertV62SearchStore(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	for _, name := range []string{"session_search_source", "session_search_fts", "session_search_state"} {
		found := false
		queryV62Row(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE name=?`, []any{name}, func(stmt *sqlite.Stmt) {
			found = stmt.ColumnInt64(0) == 1
		})
		if !found {
			t.Errorf("v62 search object %s is missing", name)
		}
	}
	needsRebuild := -1
	queryV62Row(t, conn, `SELECT needs_rebuild FROM session_search_state WHERE id=1`, nil, func(stmt *sqlite.Stmt) {
		needsRebuild = int(stmt.ColumnInt64(0))
	})
	if needsRebuild != 0 {
		t.Errorf("session_search_state.needs_rebuild = %d, want 0", needsRebuild)
	}
	for _, trigger := range []string{"session_entry_bodies_fts_ai", "session_entry_bodies_fts_bd", "session_entry_bodies_immutable", "session_generations_immutable"} {
		if sql := v62ObjectSQL(t, conn, "trigger", trigger); sql == "" {
			t.Errorf("v62 trigger %s is missing", trigger)
		}
	}
}

func assertV62Immutability(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, has_tool_use, has_thinking, is_error, depth) VALUES(1125899906842624,'s-v62-full','`+strings.Repeat("a", 64)+`',0,'opencode','text','user',0,0,0,0)`, nil); err != nil {
		t.Fatalf("seed body row: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_entry_bodies SET depth=1 WHERE body_id=1125899906842624`, nil); err == nil {
		t.Errorf("session_entry_bodies accepts UPDATE; the immutability trigger must refuse")
	}
	_ = sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_bodies WHERE body_id=1125899906842624`, nil)
}

func assertV62NoFKViolations(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	violations := []string{}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_key_check`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			violations = append(violations, fmt.Sprintf("%s rowid %d -> %s", stmt.ColumnText(0), stmt.ColumnInt64(1), stmt.ColumnText(2)))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Errorf("foreign_key_check reports %d violations after v62: %v", len(violations), violations)
	}
}
