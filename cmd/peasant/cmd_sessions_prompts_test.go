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

//go:embed testdata/sessions_prompts.yaml
var sessionsPromptsYAML []byte

//go:embed testdata/sessions_prompts.manifest.yaml
var sessionsPromptsManifestYAML []byte

// promptLongText builds a text too long to write out in the fixture.
type promptLongText struct {
	Lead   string `yaml:"lead"`
	Rune   string `yaml:"rune"`
	Repeat int    `yaml:"repeat"`
}

func (l *promptLongText) String() string {
	if l == nil {
		return ""
	}
	return l.Lead + strings.Repeat(l.Rune, l.Repeat)
}

type promptSourceRecord struct {
	Type     string          `yaml:"type"`
	Content  any             `yaml:"content"`
	LongText *promptLongText `yaml:"longText"`
	IsMeta   bool            `yaml:"isMeta"`
}

type promptNativeSession struct {
	Key            string               `yaml:"key"`
	ID             string               `yaml:"id"`
	PreviewCapture bool                 `yaml:"previewCapture"`
	Records        []promptSourceRecord `yaml:"records"`
}

type promptMirrorEntry struct {
	Role     schema.Role      `yaml:"role"`
	Type     schema.EntryType `yaml:"type"`
	Depth    int              `yaml:"depth"`
	Text     string           `yaml:"text"`
	LongText *promptLongText  `yaml:"longText"`
	Extra    string           `yaml:"extra"`
}

type promptMirrorSession struct {
	Key              string              `yaml:"key"`
	ID               string              `yaml:"id"`
	Capture          string              `yaml:"capture"`
	Parent           string              `yaml:"parent"`
	UnresolvedParent bool                `yaml:"unresolvedParent"`
	IndexFormat      int                 `yaml:"indexFormat"`
	Entries          []promptMirrorEntry `yaml:"entries"`
}

type promptCase struct {
	Name        string   `yaml:"name"`
	Session     string   `yaml:"session"`
	SessionID   string   `yaml:"sessionID"`
	Args        []string `yaml:"args"`
	Present     []string `yaml:"present"`
	Absent      []string `yaml:"absent"`
	WantCommand *struct {
		Name string `yaml:"name"`
		Args string `yaml:"args"`
		Text string `yaml:"text"`
	} `yaml:"wantCommand"`
	WantCut *struct {
		Lead  string `yaml:"lead"`
		Runes int    `yaml:"runes"`
	} `yaml:"wantCut"`
	WantWhole *struct {
		Lead  string `yaml:"lead"`
		Runes int    `yaml:"runes"`
	} `yaml:"wantWhole"`
	WantParent    string `yaml:"wantParent"`
	WantNote      string `yaml:"wantNote"`
	WantCount     *int   `yaml:"wantCount"`
	WantEmptyCode string `yaml:"wantEmptyCode"`
	WantWarning   string `yaml:"wantWarning"`
	WantNoWarning bool   `yaml:"wantNoWarning"`
	WantTextView  string `yaml:"wantTextView"`
	WantError     string `yaml:"wantError"`
	WantExit      int    `yaml:"wantExit"`
}

type promptUsageCase struct {
	Name  string   `yaml:"name"`
	Args  []string `yaml:"args"`
	Error string   `yaml:"error"`
}

type promptFixture struct {
	NativeSessions []promptNativeSession `yaml:"nativeSessions"`
	MirrorSessions []promptMirrorSession `yaml:"mirrorSessions"`
	Cases          []promptCase          `yaml:"cases"`
	Usage          []promptUsageCase     `yaml:"usage"`
}

func loadPromptFixture(t *testing.T) promptFixture {
	t.Helper()
	var fixture promptFixture
	decoder := yaml.NewDecoder(bytes.NewReader(sessionsPromptsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	for _, c := range fixture.Usage {
		names = append(names, c.Name)
	}
	var manifest struct {
		Required []string `yaml:"required"`
	}
	if err := yaml.Unmarshal(sessionsPromptsManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, name := range manifest.Required {
		if !slices.Contains(names, name) {
			t.Errorf("sessions_prompts.yaml lost the required case %q", name)
		}
	}
	return fixture
}

// writePromptSource writes the fixture's records as a Claude Code transcript.
func writePromptSource(t *testing.T, root string, session promptNativeSession) {
	t.Helper()
	path := filepath.Join(root, "project", session.ID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var source bytes.Buffer
	encoder := json.NewEncoder(&source)
	parent := any(nil)
	for i, record := range session.Records {
		uuid := fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", i+1)
		message := map[string]any{"role": record.Type, "content": record.Content}
		if record.LongText != nil {
			message["content"] = record.LongText.String()
		}
		line := map[string]any{
			"type": record.Type, "sessionId": session.ID, "uuid": uuid, "parentUuid": parent,
			"timestamp": fmt.Sprintf("2026-10-29T10:00:%02dZ", i), "cwd": "/synthetic/project",
			"isMeta": record.IsMeta, "message": message,
		}
		if record.Type == "assistant" {
			message["model"] = "claude-sonnet-4-5"
			message["id"] = fmt.Sprintf("msg_%d", i)
		}
		if err := encoder.Encode(line); err != nil {
			t.Fatal(err)
		}
		parent = uuid
	}
	if err := os.WriteFile(path, source.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedPromptNativeStore harvests the fixture's transcripts through the real
// command and returns the data directory.
func seedPromptNativeStore(t *testing.T, sessions []promptNativeSession) string {
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
		writePromptSource(t, source, session)
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
	for _, session := range sessions {
		native := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT active_generation_id IS NOT NULL, (SELECT COUNT(*) FROM session_commands c WHERE c.session_id = s.session_id) FROM sessions s WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{session.ID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				native = stmt.ColumnInt(0) == 1 && stmt.ColumnInt(1) == 0
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		if !native {
			t.Fatalf("session %s must be stored natively with no session_commands row", session.ID)
		}
		if session.PreviewCapture {
			if err := sqlitex.ExecuteTransient(conn, `UPDATE session_content_captures SET capture_format = 'preview_only' WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{session.ID}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// seedPromptMirrorStore stores the fixture's sessions in the mirror tables.
func seedPromptMirrorStore(t *testing.T, sessions []promptMirrorSession) string {
	t.Helper()
	dir := t.TempDir()
	db := openSeedStore(t, dir, "seedPromptMirrorStore")
	defer db.Close()
	ctx := t.Context()
	for i, session := range sessions {
		entries := make([]schema.SessionEntry, len(session.Entries))
		for j, seed := range session.Entries {
			text := seed.Text + seed.LongText.String()
			stamp := int64(1_700_000_000_000 + j)
			entry := schema.SessionEntry{
				SessionID: schema.SessionID(session.ID), EntryIndex: j, Harness: defaults.HarnessClaudeCode,
				EntryType: seed.Type, Role: seed.Role, Depth: seed.Depth, TimestampMs: &stamp, ContentPreview: &text,
			}
			if seed.Depth > 0 {
				parent := 0
				entry.ParentIndex = &parent
			}
			if seed.Extra != "" {
				entry.Extra = &seed.Extra
			}
			entries[j] = entry
		}
		start := int64(1_700_000_000_000 + i)
		if session.Capture == "full" {
			entry := storeEntryFor(session.ID, start)
			entry.Metadata.SchemaVersion = ingest.CurrentSchemaVersion
			testutil.SeedReadyPublication(t, db, entry.Metadata, entries)
			continue
		}
		seedOne(t, ctx, db, "seedPromptMirrorStore", seedOpts{SessionID: session.ID, StartMs: start})
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
		switch {
		case session.Parent != "":
			exec(`UPDATE sessions SET parent_id = ? WHERE session_id = ?`, session.Parent, session.ID)
		case session.UnresolvedParent:
			exec(`UPDATE sessions SET latest_attempt_generation_id = 'attempt' WHERE session_id = ?`, session.ID)
			exec(`INSERT INTO session_relationship_evidence(session_id,generation_id,ordinal,kind,target_state,evidence) VALUES(?,'attempt',0,'started_by','unknown','existing_adapter')`, session.ID)
		}
		if session.IndexFormat != 0 {
			exec(`UPDATE sessions SET index_format_version = ? WHERE session_id = ?`, session.IndexFormat, session.ID)
		}
	}
	return dir
}

// runPromptsCommand runs the real command with stdout and stderr apart.
func runPromptsCommand(t *testing.T, dir string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	root := newTestRoot()
	root.AddCommand(BuildSessionsCommand())
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "sessions", "prompts"}, args...))
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func TestSessionsPrompts(t *testing.T) {
	fixture := loadPromptFixture(t)
	nativeDir := seedPromptNativeStore(t, fixture.NativeSessions)
	mirrorDir := seedPromptMirrorStore(t, fixture.MirrorSessions)
	type located struct{ id, dir string }
	sessions := map[string]located{}
	for _, s := range fixture.NativeSessions {
		sessions[s.Key] = located{s.ID, nativeDir}
	}
	for _, s := range fixture.MirrorSessions {
		sessions[s.Key] = located{s.ID, mirrorDir}
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			where, ok := sessions[tc.Session]
			if !ok {
				where = located{tc.SessionID, mirrorDir}
			}
			stdout, stderr, err := runPromptsCommand(t, where.dir, append([]string{where.id, "--json"}, tc.Args...))
			if tc.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.WantError) {
					t.Fatalf("err = %v, want %q", err, tc.WantError)
				}
				if got := exitCodeFor(err); int(got) != tc.WantExit {
					t.Fatalf("exit = %d, want %d", got, tc.WantExit)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v\nstderr: %s", err, stderr)
			}
			assertPromptDocument(t, tc, stdout, stderr)
			if tc.WantTextView != "" {
				text, _, err := runPromptsCommand(t, where.dir, []string{where.id})
				if err != nil || !strings.Contains(text, tc.WantTextView) {
					t.Errorf("text view = %q (%v), want %q", text, err, tc.WantTextView)
				}
			}
		})
	}
	for _, tc := range fixture.Usage {
		t.Run(tc.Name, func(t *testing.T) {
			dir := t.TempDir()
			_, _, err := runPromptsCommand(t, dir, tc.Args)
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

	t.Run("text_view_commands", func(t *testing.T) {
		main := sessions["main"]
		stdout, _, err := runPromptsCommand(t, main.dir, []string{main.id})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "/review 12\n") || strings.Contains(stdout, "<command-name>") {
			t.Errorf("default text view must show the one-line command:\n%s", stdout)
		}
		stdout, _, err = runPromptsCommand(t, main.dir, []string{main.id, "--include-commands"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "<command-name>/review</command-name>") {
			t.Errorf("--include-commands must show the stored text:\n%s", stdout)
		}
	})

	t.Run("stderr_keeps_stdout_data_only", func(t *testing.T) {
		preview := sessions["mirror_preview"]
		stdout, stderr, err := runPromptsCommand(t, preview.dir, []string{preview.id})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stdout, "warning") || !strings.Contains(stderr, "warning: session "+preview.id) {
			t.Errorf("stdout %q stderr %q", stdout, stderr)
		}
	})

	t.Run("read_leaves_store_rows_alone", func(t *testing.T) {
		untouched := sessions["read_only"]
		before := storeRowsDigest(t, untouched.dir)
		if _, _, err := runPromptsCommand(t, untouched.dir, []string{untouched.id, "--json"}); err != nil {
			t.Fatal(err)
		}
		if before != storeRowsDigest(t, untouched.dir) {
			t.Error("a read-only command changed a stored row")
		}
	})
}

// storeRowsDigest hashes every row of every table. Opening a store rewrites
// bookkeeping bytes in the file, so the file itself cannot show a write.
func storeRowsDigest(t *testing.T, dir string) string {
	t.Helper()
	db := openSeedStore(t, dir, "storeRowsDigest")
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

func assertPromptDocument(t *testing.T, tc promptCase, stdout, stderr string) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if warnings, ok := raw["warnings"].([]any); !ok || warnings == nil {
		t.Errorf("warnings = %#v, want an array", raw["warnings"])
	}
	var doc sessionPromptsOutput
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Prompts == nil || doc.PromptCount != len(doc.Prompts) {
		t.Errorf("promptCount %d over prompts %#v", doc.PromptCount, doc.Prompts)
	}
	joined := func() string {
		texts := make([]string, len(doc.Prompts))
		for i, p := range doc.Prompts {
			texts[i] = p.Text
		}
		return strings.Join(texts, "\n")
	}()
	for _, want := range tc.Present {
		if !strings.Contains(joined, want) {
			t.Errorf("%q is missing from %q", want, joined)
		}
	}
	for _, unwanted := range tc.Absent {
		if strings.Contains(joined, unwanted) {
			t.Errorf("%q is listed", unwanted)
		}
	}
	find := func(lead string) *sessionPrompt {
		for i := range doc.Prompts {
			if strings.HasPrefix(doc.Prompts[i].Text, lead) {
				return &doc.Prompts[i]
			}
		}
		t.Fatalf("no prompt starts with %q", lead)
		return nil
	}
	if want := tc.WantCommand; want != nil {
		got := find(want.Text)
		if got.Command == nil || got.Command.Name != want.Name || got.Command.Args != want.Args || got.Text != want.Text {
			t.Errorf("command prompt = %+v, command %+v", got, got.Command)
		}
	}
	if want := tc.WantCut; want != nil {
		got := find(want.Lead)
		if !got.Truncated || !utf8.ValidString(got.Text) || utf8.RuneCountInString(got.Text) != want.Runes {
			t.Errorf("cut prompt has %d runes, truncated %v", utf8.RuneCountInString(got.Text), got.Truncated)
		}
	}
	if want := tc.WantWhole; want != nil {
		got := find(want.Lead)
		if got.Truncated || utf8.RuneCountInString(got.Text) != want.Runes {
			t.Errorf("prompt has %d runes, want the whole %d", utf8.RuneCountInString(got.Text), want.Runes)
		}
	}
	if tc.WantParent != "" && (doc.ParentSessionID == nil || *doc.ParentSessionID != tc.WantParent) {
		t.Errorf("parentSessionId = %v", doc.ParentSessionID)
	}
	if !strings.Contains(doc.Note, tc.WantNote) {
		t.Errorf("note = %q, want %q", doc.Note, tc.WantNote)
	}
	if tc.WantCount != nil && doc.PromptCount != *tc.WantCount {
		t.Errorf("promptCount = %d", doc.PromptCount)
	}
	if string(doc.EmptyCode) != tc.WantEmptyCode || (tc.WantEmptyCode != "") != (doc.EmptyReason != "") {
		t.Errorf("empty = %q %q, want code %q", doc.EmptyCode, doc.EmptyReason, tc.WantEmptyCode)
	}
	codes := []string{}
	for _, w := range doc.Warnings {
		codes = append(codes, string(w.Code))
	}
	if tc.WantWarning != "" && (!slices.Contains(codes, tc.WantWarning) || !strings.Contains(stderr, "warning: ")) {
		t.Errorf("warnings = %v, stderr %q, want %s", codes, stderr, tc.WantWarning)
	}
	if tc.WantNoWarning && len(codes) != 0 {
		t.Errorf("warnings = %v, want none", codes)
	}
}
