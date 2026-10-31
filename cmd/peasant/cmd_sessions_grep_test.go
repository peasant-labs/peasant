package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/sessions_grep.yaml
var sessionsGrepYAML []byte

//go:embed testdata/sessions_grep.manifest.yaml
var sessionsGrepManifestYAML []byte

type grepLongText struct {
	Lead   string `yaml:"lead"`
	Rune   string `yaml:"rune"`
	Repeat int    `yaml:"repeat"`
	Tail   string `yaml:"tail"`
}

func (l *grepLongText) String() string {
	if l == nil {
		return ""
	}
	return l.Lead + strings.Repeat(l.Rune, l.Repeat) + l.Tail
}

type grepSourceRecord struct {
	Type     string        `yaml:"type"`
	Content  any           `yaml:"content"`
	LongText *grepLongText `yaml:"longText"`
	Count    int           `yaml:"count"`
}

type grepNativeSession struct {
	Key     string             `yaml:"key"`
	ID      string             `yaml:"id"`
	Records []grepSourceRecord `yaml:"records"`
}

type grepMirrorEntry struct {
	Role     schema.Role      `yaml:"role"`
	Type     schema.EntryType `yaml:"type"`
	Text     string           `yaml:"text"`
	LongText *grepLongText    `yaml:"longText"`
	Count    int              `yaml:"count"`
}

type grepMirrorSession struct {
	Key            string            `yaml:"key"`
	ID             string            `yaml:"id"`
	Capture        string            `yaml:"capture"`
	Parent         string            `yaml:"parent"`
	IndexFormat    int               `yaml:"indexFormat"`
	UnreadableFrom int               `yaml:"unreadableFrom"`
	Entries        []grepMirrorEntry `yaml:"entries"`
}

type grepWantMatch struct {
	Session string `yaml:"session"`
	Role    string `yaml:"role"`
	Depth   *int   `yaml:"depth"`
	Field   string `yaml:"field"`
	Snippet string `yaml:"snippet"`
}

type grepCase struct {
	Name                 string          `yaml:"name"`
	Store                string          `yaml:"store"`
	Pattern              string          `yaml:"pattern"`
	Args                 []string        `yaml:"args"`
	SessionsFrom         []string        `yaml:"sessionsFrom"`
	SessionArg           string          `yaml:"sessionArg"`
	Want                 []grepWantMatch `yaml:"want"`
	WantCount            *int            `yaml:"wantCount"`
	WantScanned          *int            `yaml:"wantScanned"`
	WantMode             string          `yaml:"wantMode"`
	WantTruncated        bool            `yaml:"wantTruncated"`
	WantReason           string          `yaml:"wantReason"`
	WantEmptyCode        string          `yaml:"wantEmptyCode"`
	WantSnippetTruncated bool            `yaml:"wantSnippetTruncated"`
	WantMaxSnippetRunes  int             `yaml:"wantMaxSnippetRunes"`
	WantSnippetRunes     int             `yaml:"wantSnippetRunes"`
	WantNoWarning        bool            `yaml:"wantNoWarning"`
	WantNoRefusal        bool            `yaml:"wantNoRefusal"`
	WantWarning          *struct {
		Code    string `yaml:"code"`
		Session string `yaml:"session"`
	} `yaml:"wantWarning"`
}

type grepUsageCase struct {
	Name  string   `yaml:"name"`
	Args  []string `yaml:"args"`
	Stdin string   `yaml:"stdin"`
	Error string   `yaml:"error"`
}

type grepFixture struct {
	NativeSessions []grepNativeSession `yaml:"nativeSessions"`
	MirrorSessions []grepMirrorSession `yaml:"mirrorSessions"`
	Cases          []grepCase          `yaml:"cases"`
	Usage          []grepUsageCase     `yaml:"usage"`
}

func loadGrepFixture(t *testing.T) grepFixture {
	t.Helper()
	var fixture grepFixture
	decoder := yaml.NewDecoder(bytes.NewReader(sessionsGrepYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	names := []string{"text_view_one_line", "text_view_reason_stays_on_stderr"}
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	for _, c := range fixture.Usage {
		names = append(names, c.Name)
	}
	var manifest struct {
		Required []string `yaml:"required"`
	}
	if err := yaml.Unmarshal(sessionsGrepManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, name := range manifest.Required {
		if !slices.Contains(names, name) {
			t.Errorf("sessions_grep.yaml lost the required case %q", name)
		}
	}
	return fixture
}

// writeGrepSource writes the fixture's records as a Claude Code transcript.
func writeGrepSource(t *testing.T, root string, session grepNativeSession) {
	t.Helper()
	path := filepath.Join(root, "project", session.ID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var source bytes.Buffer
	encoder := json.NewEncoder(&source)
	parent := any(nil)
	line := 0
	for _, record := range session.Records {
		for range max(record.Count, 1) {
			line++
			uuid := fmt.Sprintf("bbbbbbbb-0000-4000-8000-%012d", line)
			message := map[string]any{"role": record.Type, "content": record.Content}
			if record.LongText != nil {
				message["content"] = record.LongText.String()
			}
			if record.Type == "assistant" {
				message["model"] = "claude-sonnet-4-5"
				message["id"] = fmt.Sprintf("msg_%d", line)
			}
			if err := encoder.Encode(map[string]any{
				"type": record.Type, "sessionId": session.ID, "uuid": uuid, "parentUuid": parent,
				"timestamp": fmt.Sprintf("2026-10-29T10:%02d:%02dZ", line/60, line%60),
				"cwd":       "/synthetic/project", "message": message,
			}); err != nil {
				t.Fatal(err)
			}
			parent = uuid
		}
	}
	if err := os.WriteFile(path, source.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedGrepNativeStore harvests the fixture's transcripts through the real
// command and proves they landed in the native representation.
func seedGrepNativeStore(t *testing.T, sessions []grepNativeSession) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(defaults.EnvXDGConfigHome.String(), filepath.Join(root, "config"))
	t.Setenv(defaults.EnvXDGDataHome.String(), filepath.Join(root, "data"))
	t.Setenv(defaults.EnvXDGStateHome.String(), filepath.Join(root, "state"))
	dir := filepath.Join(root, "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	for _, session := range sessions {
		writeGrepSource(t, source, session)
	}
	output := filepath.Join(root, "output")
	if text, err := executeHarvestCmd(t, dir, []string{"--source-harness", "claude-code", "--source-path", source, "--output", output, "--include-active"}); err != nil {
		t.Fatalf("harvest: %v\n%s", err, text)
	}
	options, err := generationStoreOptions(output)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)), options...)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	native := 0
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sessions WHERE active_generation_id IS NOT NULL`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			native = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if native != len(sessions) {
		t.Fatalf("%d sessions are native, want %d", native, len(sessions))
	}
	return dir
}

// seedGrepMirrorStore stores the fixture's sessions in the mirror tables.
func seedGrepMirrorStore(t *testing.T, sessions []grepMirrorSession) string {
	t.Helper()
	dir := t.TempDir()
	db := openSeedStore(t, dir, "seedGrepMirrorStore")
	defer db.Close()
	ctx := t.Context()
	for i, session := range sessions {
		var entries []schema.SessionEntry
		for _, seed := range session.Entries {
			for range max(seed.Count, 1) {
				text := seed.Text + seed.LongText.String()
				stamp := int64(1_700_000_000_000 + len(entries))
				entries = append(entries, schema.SessionEntry{
					SessionID: schema.SessionID(session.ID), EntryIndex: len(entries), Harness: defaults.HarnessClaudeCode,
					EntryType: seed.Type, Role: seed.Role, TimestampMs: &stamp, ContentPreview: &text,
				})
			}
		}
		start := int64(1_700_000_000_000 + i)
		if session.Capture == "full" {
			entry := storeEntryFor(session.ID, start)
			entry.Metadata.SchemaVersion = ingest.CurrentSchemaVersion
			testutil.SeedReadyPublication(t, db, entry.Metadata, entries)
			continue
		}
		seedOne(t, ctx, db, "seedGrepMirrorStore", seedOpts{SessionID: session.ID, StartMs: start})
		if err := db.IndexSessionEntries(ctx, schema.SessionID(session.ID), entries); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	exec := func(query string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	for _, session := range sessions {
		if session.Parent != "" {
			exec(`UPDATE sessions SET parent_id = ? WHERE session_id = ?`, session.Parent, session.ID)
		}
		if session.IndexFormat != 0 {
			exec(`UPDATE sessions SET index_format_version = ? WHERE session_id = ?`, session.IndexFormat, session.ID)
		}
		if session.UnreadableFrom != 0 {
			exec(`UPDATE session_entries SET extra = '{"pi":' WHERE session_id = ? AND entry_index = ?`, session.ID, session.UnreadableFrom)
		}
	}
	return dir
}

// runGrepCommand runs the real command with stdout and stderr apart.
func runGrepCommand(t *testing.T, dir, stdin string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	root := newTestRoot()
	root.AddCommand(BuildSessionsCommand())
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "sessions", "grep"}, args...))
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func TestSessionsGrep(t *testing.T) {
	fixture := loadGrepFixture(t)
	dirs := map[string]string{
		"":       seedGrepNativeStore(t, fixture.NativeSessions),
		"mirror": seedGrepMirrorStore(t, fixture.MirrorSessions),
		"empty":  seedGrepMirrorStore(t, nil),
		// Read by read_leaves_store_rows_alone and by nothing else.
		"read_only": seedGrepMirrorStore(t, fixture.MirrorSessions[:1]),
	}
	ids := map[string]string{}
	for _, s := range fixture.NativeSessions {
		ids[s.Key] = s.ID
	}
	for _, s := range fixture.MirrorSessions {
		ids[s.Key] = s.ID
	}
	idOf := func(key string) string {
		if id, ok := ids[key]; ok {
			return id
		}
		return key
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			args := append([]string{tc.Pattern, "--json"}, tc.Args...)
			stdin := ""
			if tc.SessionsFrom != nil {
				args = append(args, "--sessions-from", "-")
				for _, key := range tc.SessionsFrom {
					stdin += idOf(key) + "\n"
				}
			}
			if tc.SessionArg != "" {
				args = append(args, "--session", idOf(tc.SessionArg))
			}
			stdout, stderr, err := runGrepCommand(t, dirs[tc.Store], stdin, args)
			if err != nil {
				t.Fatalf("%v\nstderr: %s", err, stderr)
			}
			assertGrepDocument(t, tc, idOf, stdout, stderr)
		})
	}
	for _, tc := range fixture.Usage {
		t.Run(tc.Name, func(t *testing.T) {
			dir := t.TempDir()
			_, _, err := runGrepCommand(t, dir, tc.Stdin, tc.Args)
			var usage *clicontract.UsageError
			if !errors.As(err, &usage) || !strings.Contains(err.Error(), tc.Error) {
				t.Fatalf("err = %v, want a usage error naming %q", err, tc.Error)
			}
			if got := exitCodeFor(err); got != defaults.ExitUsage {
				t.Fatalf("exit = %d", got)
			}
			if _, statErr := os.Stat(string(defaults.ResolveDBFilePathWith(dir))); !os.IsNotExist(statErr) {
				t.Fatalf("the store was opened before the refusal: %v", statErr)
			}
		})
	}

	t.Run("text_view_one_line", func(t *testing.T) {
		stdout, stderr, err := runGrepCommand(t, dirs[""], "", []string{"OUTPUTMARK"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(stdout, ids["alpha"]+" [") || !strings.HasSuffix(stdout, "] tool depth=1  OUTPUTMARK and more\n") {
			t.Errorf("stdout = %q", stdout)
		}
		if strings.Contains(stdout, "note:") || stderr != "" {
			t.Errorf("stdout %q stderr %q", stdout, stderr)
		}
	})

	t.Run("escapes_control_characters", func(t *testing.T) {
		if got := escapeSnippetControls("a\nb\x1b[31m"); got != `a\nb\x1b[31m` {
			t.Errorf("escaped = %q", got)
		}
	})

	t.Run("help_says_it_does_not_redact", func(t *testing.T) {
		stdout, _, err := runGrepCommand(t, t.TempDir(), "", []string{"--help"})
		if err != nil || !strings.Contains(stdout, "does not redact") {
			t.Errorf("help = %q (%v)", stdout, err)
		}
	})

	t.Run("read_leaves_store_rows_alone", func(t *testing.T) {
		before := grepStoreRowsDigest(t, dirs["read_only"])
		if _, _, err := runGrepCommand(t, dirs["read_only"], "", []string{"DEEPMIRROR", "--json"}); err != nil {
			t.Fatal(err)
		}
		if before != grepStoreRowsDigest(t, dirs["read_only"]) {
			t.Error("a read-only command changed a stored row")
		}
	})

	t.Run("text_view_reason_stays_on_stderr", func(t *testing.T) {
		stdout, stderr, err := runGrepCommand(t, dirs[""], "", []string{"NOTANYWHERE"})
		if err != nil {
			t.Fatal(err)
		}
		if stdout != "" || !strings.Contains(stderr, "note: no entry in 2 sessions matched") {
			t.Errorf("stdout %q stderr %q", stdout, stderr)
		}
	})
}

// grepStoreRowsDigest hashes every row of every table. Opening a store rewrites
// bookkeeping bytes in the file, so the file itself cannot show a write.
func grepStoreRowsDigest(t *testing.T, dir string) string {
	t.Helper()
	db := openSeedStore(t, dir, "grepStoreRowsDigest")
	defer db.Close()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var tables []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			tables = append(tables, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	for _, table := range tables {
		fmt.Fprintf(digest, "table %s\n", table)
		if err := sqlitex.ExecuteTransient(conn, `SELECT * FROM "`+table+`"`, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				for column := range stmt.ColumnCount() {
					fmt.Fprintf(digest, "%q,", stmt.ColumnText(column))
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func assertGrepDocument(t *testing.T, tc grepCase, idOf func(string) string, stdout, stderr string) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if warnings, ok := raw["warnings"].([]any); !ok || warnings == nil {
		t.Errorf("warnings = %#v, want an array", raw["warnings"])
	}
	if matches, ok := raw["matches"].([]any); !ok || matches == nil {
		t.Errorf("matches = %#v, want an array", raw["matches"])
	}
	var doc sessionGrepOutput
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Pattern != tc.Pattern {
		t.Errorf("pattern = %q", doc.Pattern)
	}
	wantMode := tc.WantMode
	if wantMode == "" && !slices.Contains(tc.Args, "--regexp") {
		wantMode = "literal"
	}
	if wantMode != "" && doc.Mode != wantMode {
		t.Errorf("mode = %q, want %q", doc.Mode, wantMode)
	}
	for _, want := range tc.Want {
		found := slices.ContainsFunc(doc.Matches, func(got sessionGrepMatch) bool {
			return got.SessionID == idOf(want.Session) && strings.Contains(got.Snippet, want.Snippet) &&
				(want.Role == "" || got.Role == want.Role) && (want.Field == "" || got.Field == want.Field) &&
				(want.Depth == nil || got.Depth == *want.Depth)
		})
		if !found {
			t.Errorf("no match %+v in %+v", want, doc.Matches)
		}
	}
	if tc.WantCount != nil && len(doc.Matches) != *tc.WantCount {
		t.Errorf("%d matches, want %d: %+v", len(doc.Matches), *tc.WantCount, doc.Matches)
	}
	if tc.WantScanned != nil && doc.SessionsScanned != *tc.WantScanned {
		t.Errorf("sessionsScanned = %d, want %d", doc.SessionsScanned, *tc.WantScanned)
	}
	if doc.Truncated != tc.WantTruncated {
		t.Errorf("truncated = %v", doc.Truncated)
	}
	if !strings.Contains(doc.Reason, tc.WantReason) {
		t.Errorf("reason = %q, want %q", doc.Reason, tc.WantReason)
	}
	if string(doc.EmptyCode) != tc.WantEmptyCode {
		t.Errorf("empty_code = %q, want %q", doc.EmptyCode, tc.WantEmptyCode)
	}
	if tc.WantEmptyCode != "" && (doc.EmptyReason == "" || doc.Reason == "") {
		t.Errorf("an empty result must say why: %q %q", doc.EmptyReason, doc.Reason)
	}
	for _, got := range doc.Matches {
		if !utf8.ValidString(got.Snippet) {
			t.Errorf("snippet %q is not valid UTF-8", got.Snippet)
		}
		if tc.WantSnippetTruncated && !got.SnippetTruncated {
			t.Errorf("snippet %q is not marked truncated", got.Snippet)
		}
		if tc.WantSnippetRunes > 0 && utf8.RuneCountInString(got.Snippet) != tc.WantSnippetRunes {
			t.Errorf("snippet has %d runes, want %d", utf8.RuneCountInString(got.Snippet), tc.WantSnippetRunes)
		}
		if tc.WantMaxSnippetRunes > 0 && utf8.RuneCountInString(got.Snippet) > tc.WantMaxSnippetRunes {
			t.Errorf("snippet has %d runes, over %d", utf8.RuneCountInString(got.Snippet), tc.WantMaxSnippetRunes)
		}
	}
	if tc.WantNoWarning && len(doc.Warnings) != 0 {
		t.Errorf("warnings = %+v, want none", doc.Warnings)
	}
	if tc.WantNoRefusal && slices.ContainsFunc(doc.Warnings, func(w clicontract.Warning) bool { return w.Code == clicontract.WarningSessionRefused }) {
		t.Errorf("warnings = %+v, want no refusal", doc.Warnings)
	}
	if want := tc.WantWarning; want != nil {
		found := slices.ContainsFunc(doc.Warnings, func(w clicontract.Warning) bool {
			return string(w.Code) == want.Code && w.SessionID == idOf(want.Session)
		})
		if !found || !strings.Contains(stderr, "warning: session "+idOf(want.Session)) {
			t.Errorf("warnings = %+v, stderr %q, want %+v", doc.Warnings, stderr, want)
		}
	}
}
