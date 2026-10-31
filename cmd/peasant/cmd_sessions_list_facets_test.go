package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/sessions_list_facets.yaml
var sessionsListFacetsYAML []byte

//go:embed testdata/sessions_list_facets.manifest.yaml
var sessionsListFacetsManifestYAML []byte

type facetTool struct {
	Name  string         `yaml:"name"`
	Input map[string]any `yaml:"input"`
}

type facetRecord struct {
	User            string      `yaml:"user"`
	Assistant       string      `yaml:"assistant"`
	Model           string      `yaml:"model"`
	Tools           []facetTool `yaml:"tools"`
	CompactBoundary bool        `yaml:"compactBoundary"`
}

type facetTranscript struct {
	ID      string        `yaml:"id"`
	Records []facetRecord `yaml:"records"`
}

type facetLegacyEntry struct {
	Role    string `yaml:"role"`
	Type    string `yaml:"type"`
	Preview string `yaml:"preview"`
	Tools   string `yaml:"tools"`
	Input   string `yaml:"input"`
	Part    string `yaml:"part"`
	Extra   string `yaml:"extra"`
}

type facetLegacySession struct {
	ID      string             `yaml:"id"`
	Turns   int                `yaml:"turns"`
	Entries []facetLegacyEntry `yaml:"entries"`
}

type facetArrangeStep struct {
	Step    string `yaml:"step"`
	Session string `yaml:"session"`
	Parent  string `yaml:"parent"`
}

type facetAlsoCheck struct {
	Args     []string `yaml:"args"`
	Contains []string `yaml:"contains"`
	Lacks    []string `yaml:"lacks"`
}

type facetCase struct {
	Name         string              `yaml:"name"`
	Args         []string            `yaml:"args"`
	Want         []string            `yaml:"want"`
	WantContains []string            `yaml:"wantContains"`
	WantAbsent   []string            `yaml:"wantAbsent"`
	WantFacets   map[string][]string `yaml:"wantFacets"`
	WantAlso     []facetAlsoCheck    `yaml:"wantAlso"`
	Table        bool                `yaml:"table"`
	WantOutput   string              `yaml:"wantOutput"`
}

type facetRefusal struct {
	Name      string   `yaml:"name"`
	Args      []string `yaml:"args"`
	WantError string   `yaml:"wantError"`
}

type facetFixture struct {
	Transcripts []facetTranscript    `yaml:"transcripts"`
	Legacy      []facetLegacySession `yaml:"legacy"`
	Arrange     []facetArrangeStep   `yaml:"arrange"`
	Cases       []facetCase          `yaml:"cases"`
	Refusals    []facetRefusal       `yaml:"refusals"`
}

func loadSessionsListFacetsFixtures(t *testing.T) facetFixture {
	t.Helper()
	var fixture facetFixture
	if err := yaml.Unmarshal(sessionsListFacetsYAML, &fixture); err != nil {
		t.Fatalf("decode sessions_list_facets.yaml: %v", err)
	}
	var manifest struct {
		Required []string `yaml:"required"`
	}
	if err := yaml.Unmarshal(sessionsListFacetsManifestYAML, &manifest); err != nil {
		t.Fatalf("decode sessions_list_facets.manifest.yaml: %v", err)
	}
	for _, name := range manifest.Required {
		if !slices.ContainsFunc(fixture.Cases, func(c facetCase) bool { return c.Name == name }) {
			t.Errorf("sessions_list_facets.yaml lost the required case %q", name)
		}
	}
	return fixture
}

// facetSessionUUID gives a fixture session a stable id of the shape Claude Code
// names its files with.
func facetSessionUUID(index int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", index+1)
}

// writeFacetTranscript writes one Claude Code source file.
func writeFacetTranscript(t *testing.T, dir, id string, order int, transcript facetTranscript) {
	t.Helper()
	var lines []string
	emit := func(index int, record map[string]any) {
		record["parentUuid"] = nil
		record["isSidechain"] = false
		record["userType"] = "external"
		record["cwd"] = "/synthetic/project"
		record["sessionId"] = id
		record["version"] = "2.1.0"
		record["gitBranch"] = "main"
		record["uuid"] = fmt.Sprintf("%s-%04d", id[:23], index)
		record["timestamp"] = fmt.Sprintf("2026-01-%02dT10:00:%02d.000Z", 1+order%27, index)
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(line))
	}
	for i, r := range transcript.Records {
		switch {
		case r.CompactBoundary:
			emit(i, map[string]any{"type": "system", "subtype": "compact_boundary", "content": "Conversation compacted", "compactMetadata": map[string]any{"trigger": "auto", "preTokens": 10}})
		case r.User != "":
			emit(i, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": r.User}})
		default:
			content := []map[string]any{{"type": "text", "text": r.Assistant}}
			for j, tool := range r.Tools {
				input := tool.Input
				if input == nil {
					input = map[string]any{}
				}
				content = append(content, map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d_%d", i, j), "name": tool.Name, "input": input})
			}
			model := r.Model
			if model == "" {
				model = "model-one"
			}
			emit(i, map[string]any{"type": "assistant", "message": map[string]any{
				"id": fmt.Sprintf("msg_%s_%d", id[:8], i), "role": "assistant", "model": model, "content": content,
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
			}})
		}
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// facetStore is one store built from the fixture and the names of its sessions.
type facetStore struct {
	dir   string
	names map[string]string // session id -> fixture id
	ids   map[string]string // fixture id -> session id
}

func seedFacetStore(t *testing.T, fixture facetFixture) facetStore {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "store")
	source := filepath.Join(root, "projects", "-synthetic-project")
	for _, path := range []string{dir, source} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	seeded := facetStore{dir: dir, names: map[string]string{}, ids: map[string]string{}}
	for i, transcript := range fixture.Transcripts {
		id := facetSessionUUID(i)
		seeded.names[id], seeded.ids[transcript.ID] = transcript.ID, id
		writeFacetTranscript(t, source, id, i, transcript)
	}
	if output, err := executeHarvestCmd(t, dir, []string{"--output=" + filepath.Join(root, "sync"), "--source-harness=claude-code", "--source-path=" + filepath.Dir(source)}); err != nil {
		t.Fatalf("harvest the fixture transcripts: %v\n%s", err, output)
	}
	legacy := make([]seedOpts, len(fixture.Legacy))
	for i, session := range fixture.Legacy {
		seeded.names[session.ID], seeded.ids[session.ID] = session.ID, session.ID
		legacy[i] = seedOpts{SessionID: session.ID, StartMs: int64(i+1) * 1_000_000, TurnCount: session.Turns}
	}
	mustSeedMany(t, dir, legacy)

	db := openSeedStore(t, dir, "seedFacetStore")
	defer db.Close()
	for _, session := range fixture.Legacy {
		entries := make([]schema.SessionEntry, len(session.Entries))
		for i, e := range session.Entries {
			entries[i] = facetLegacySchemaEntry(session.ID, i, e)
		}
		if err := db.IndexSessionEntries(t.Context(), schema.SessionID(session.ID), entries); err != nil {
			t.Fatalf("index legacy entries for %s: %v", session.ID, err)
		}
	}
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	for _, step := range fixture.Arrange {
		arrangeFacetStep(t, conn, seeded, step)
	}
	return seeded
}

func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func facetLegacySchemaEntry(id string, index int, e facetLegacyEntry) schema.SessionEntry {
	entryType := schema.EntryTypeText
	if e.Type != "" {
		entryType = schema.EntryType(e.Type)
	}
	return schema.SessionEntry{
		SessionID: schema.SessionID(id), EntryIndex: index, Harness: defaults.HarnessClaudeCode,
		EntryType: entryType, Role: schema.Role(e.Role),
		ContentPreview: optionalText(e.Preview), ToolNamesCSV: optionalText(e.Tools),
		ToolInput: optionalText(e.Input), PartType: optionalText(e.Part), Extra: optionalText(e.Extra),
		HasToolUse: e.Tools != "",
	}
}

// facetAddBodySQL adds one tool call body to the session, which the arrange
// step then maps. Bodies are immutable, so a different state is a second row.
const facetAddBodySQL = `INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, has_tool_use, has_thinking, is_error, depth, tool_names_csv, tool_input)
SELECT (SELECT MAX(body_id) + 1 FROM session_entry_bodies), session_id, printf('%064d', 7), 90, harness, 'tool_use', 'assistant', 1, 0, 0, 0, ?, ?
FROM session_entry_bodies WHERE session_id = ? LIMIT 1`

// facetMapBodySQL maps that body into the active generation.
const facetMapBodySQL = `INSERT INTO session_generation_entries(session_id, generation_id, partition_id, entry_index, body_digest)
SELECT session_id, active_generation_id, ?, 90, printf('%064d', 7) FROM sessions WHERE session_id = ?`

// arrangeFacetStep edits the store for a state a harvest cannot reach cheaply.
func arrangeFacetStep(t *testing.T, conn *sqlite.Conn, seeded facetStore, step facetArrangeStep) {
	t.Helper()
	id, ok := seeded.ids[step.Session]
	if !ok {
		t.Fatalf("arrange step %s names unknown session %q", step.Step, step.Session)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("arrange step %s: %v", step.Step, err)
		}
	}
	const copyGeneration = `CREATE TEMP TABLE IF NOT EXISTS facet_generation AS SELECT * FROM session_generations WHERE 0`
	// Generation rows are immutable, so a different state is a second row.
	addGeneration := func(generation string, partial bool) {
		exec(copyGeneration)
		exec(`DELETE FROM facet_generation`)
		exec(`INSERT INTO facet_generation SELECT g.* FROM session_generations g JOIN sessions s ON s.session_id = g.session_id AND s.active_generation_id = g.generation_id WHERE g.session_id = ?`, id)
		exec(`UPDATE facet_generation SET generation_id = ?, diagnostics_partial = ?`, generation, partial)
		exec(`INSERT INTO session_generations SELECT * FROM facet_generation`)
	}
	switch step.Step {
	case "active_partial":
		addGeneration("partial", true)
		exec(`UPDATE sessions SET active_generation_id = 'partial' WHERE session_id = ?`, id)
	case "older_partial":
		addGeneration("older", true)
	case "older_generation_tool":
		addGeneration("older", false)
		exec(facetAddBodySQL, "mcp__hidden__tool", nil, id)
		exec(`INSERT INTO session_generation_entries(session_id, generation_id, partition_id, entry_index, body_digest) VALUES (?, 'older', 0, 90, printf('%064d', 7))`, id)
	case "earlier_partition_tool":
		exec(facetAddBodySQL, "mcp__hidden__tool", nil, id)
		exec(facetMapBodySQL, 1, id)
	case "stale_mirror":
		exec(`INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, has_tool_use, tool_names_csv) VALUES (?, 0, 'claude-code', 'tool_use', 'assistant', 1, 'mcp__stale__tool')`, id)
		exec(`INSERT INTO session_commands(session_id, entry_index, command_name) VALUES (?, 0, '/stale')`, id)
	case "metrics_turns_differ":
		exec(`UPDATE session_metrics SET turn_count = 99 WHERE session_id = ?`, id)
	case "remove_captured_stats":
		exec(`DELETE FROM session_captured_stats WHERE session_id = ?`, id)
	case "unparsable_tool_input":
		exec(facetAddBodySQL, "Bash", `{"run_in_background": tru`, id)
		exec(facetMapBodySQL, 0, id)
	case "child_of":
		exec(`UPDATE sessions SET parent_id = ? WHERE session_id = ?`, seeded.ids[step.Parent], id)
	default:
		t.Fatalf("unknown arrange step %q", step.Step)
	}
}

// facetRow is the part of a JSON row these tests read. Facets stays raw so an
// absent key, a null and an empty array stay three different answers.
type facetRow struct {
	ID     string          `json:"id"`
	Facets json.RawMessage `json:"facets"`
}

func listFacetRows(t *testing.T, seeded facetStore, args []string) []facetRow {
	t.Helper()
	all := append([]string{"list", "--json", "--limit", "0"}, args...)
	out, err := executeWithDataDir(t, BuildSessionsCommand(), seeded.dir, all)
	if err != nil {
		t.Fatalf("sessions %v: %v\n%s", all, err, out)
	}
	var rows []facetRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("sessions %v: not a bare JSON array: %v\n%s", all, err, out)
	}
	return rows
}

// compactFacets drops the indentation of the printed array, nothing else, so
// null and [] stay different answers.
func compactFacets(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	return compact.String()
}

func facetRowNames(seeded facetStore, rows []facetRow) []string {
	names := make([]string, len(rows))
	for i, row := range rows {
		names[i] = seeded.names[row.ID]
	}
	slices.Sort(names)
	return names
}

func TestSessionsListFacets(t *testing.T) {
	t.Parallel()
	fixture := loadSessionsListFacetsFixtures(t)
	seeded := seedFacetStore(t, fixture)
	everyRow := listFacetRows(t, seeded, []string{"--include-subagents"})
	facetsByName := make(map[string]json.RawMessage, len(everyRow))
	for _, row := range everyRow {
		facetsByName[seeded.names[row.ID]] = row.Facets
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			for name, want := range tc.WantFacets {
				got, ok := facetsByName[name]
				if !ok {
					t.Errorf("session %s has no row", name)
					continue
				}
				encoded, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				if compactFacets(t, got) != string(encoded) {
					t.Errorf("session %s facets = %s, want %s", name, got, encoded)
				}
			}
			if tc.Table {
				out, err := executeWithDataDir(t, BuildSessionsCommand(), seeded.dir, append([]string{"list"}, tc.Args...))
				if err != nil {
					t.Fatalf("sessions list %v: %v\n%s", tc.Args, err, out)
				}
				if !strings.Contains(out, tc.WantOutput) {
					t.Fatalf("sessions list %v does not print %q:\n%s", tc.Args, tc.WantOutput, out)
				}
				return
			}
			if tc.Args != nil {
				got := facetRowNames(seeded, listFacetRows(t, seeded, tc.Args))
				if tc.Want != nil && !slices.Equal(got, tc.Want) {
					t.Errorf("sessions list %v = %v, want %v", tc.Args, got, tc.Want)
				}
				for _, name := range tc.WantContains {
					if !slices.Contains(got, name) {
						t.Errorf("sessions list %v lacks %s: %v", tc.Args, name, got)
					}
				}
				for _, name := range tc.WantAbsent {
					if slices.Contains(got, name) {
						t.Errorf("sessions list %v holds %s: %v", tc.Args, name, got)
					}
				}
			}
			for _, also := range tc.WantAlso {
				got := facetRowNames(seeded, listFacetRows(t, seeded, also.Args))
				for _, name := range also.Contains {
					if !slices.Contains(got, name) {
						t.Errorf("sessions list %v lacks %s: %v", also.Args, name, got)
					}
				}
				for _, name := range also.Lacks {
					if slices.Contains(got, name) {
						t.Errorf("sessions list %v holds %s: %v", also.Args, name, got)
					}
				}
			}
		})
	}
}

// TestSessionsListFacetsRefusals proves a refused call exits with the usage
// status and never reaches the store: the store directory stays absent.
func TestSessionsListFacetsRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range loadSessionsListFacetsFixtures(t).Refusals {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "never-opened")
			out, err := executeWithDataDir(t, BuildSessionsCommand(), dir, append([]string{"list"}, tc.Args...))
			if err == nil {
				t.Fatalf("sessions list %v was accepted:\n%s", tc.Args, out)
			}
			var usage *clicontract.UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("sessions list %v returned %T (%v), want a usage error", tc.Args, err, err)
			}
			if !strings.Contains(err.Error(), tc.WantError) {
				t.Errorf("refusal %q does not name %q", err, tc.WantError)
			}
			if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused call touched the store directory: %v", statErr)
			}
		})
	}
}

// TestSessionsListTableReadsNoFacets proves the default table costs nothing
// extra: with a table the facet rules read gone, the table still lists and only
// the JSON row, which does read them, fails.
func TestSessionsListTableReadsNoFacets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustSeedMany(t, dir, []seedOpts{{SessionID: "plain-session", StartMs: 1_000_000, TurnCount: 2}})
	db := openSeedStore(t, dir, "TestSessionsListTableReadsNoFacets")
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DROP TABLE session_commands`, nil); err != nil {
		t.Fatal(err)
	}
	db.Pool().Put(conn)
	db.Close()

	if out, err := executeWithDataDir(t, BuildSessionsCommand(), dir, []string{"list"}); err != nil || !strings.Contains(out, "plain-session") {
		t.Fatalf("the table read the facet rules: %v\n%s", err, out)
	}
	if out, err := executeWithDataDir(t, BuildSessionsCommand(), dir, []string{"list", "--json"}); err == nil {
		t.Fatalf("--json did not read the facet rules:\n%s", out)
	}
}

// TestSessionsListFacetsOpenCode harvests a real native OpenCode source. It
// proves the rules that differ from Claude's: the summary row records a
// compaction with no part type, and the lowercase skill tool counts. Its
// model-switched row is stored with no marker and no observed model, so it holds
// no facet of its own.
func TestSessionsListFacetsOpenCode(t *testing.T) {
	t.Parallel()
	source := testfixture.MaterializeByName(t, "semantic-parity-current")
	dir := t.TempDir()
	if output, err := executeHarvestCmd(t, dir, []string{"--output=" + filepath.Join(dir, "sync"), "--source-harness=opencode", "--source-path=" + filepath.Dir(source.Path)}); err != nil {
		t.Fatalf("harvest the OpenCode source: %v\n%s", err, output)
	}
	seeded := facetStore{dir: dir, names: map[string]string{}}
	rows := listFacetRows(t, seeded, []string{"--include-subagents"})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the one fixture session", rows)
	}
	if got, want := compactFacets(t, rows[0].Facets), `["skill","compact"]`; got != want {
		t.Fatalf("facets = %s, want %s", got, want)
	}
}
