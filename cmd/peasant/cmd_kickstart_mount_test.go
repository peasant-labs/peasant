package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/schema"
)

// The kickstart preview's mounted wiring, exercised against a REAL store: an
// imported session, a discovered-but-not-yet-imported one, and a row that is
// not a session at all. The ids and text live here as constants so a single
// change updates one place.
// The ids are real UUIDs because the session read path VALIDATES them: an id
// outside the contract's accepted forms resolves to a session with no turns
// rather than an error, so a readable-but-invalid id here would silently send
// every case down the not-imported path and pass.
const (
	mountImportedSessionID  = "3f1c9a52-7b64-4e18-9a0d-2c5e8f7b1a44"
	mountTruncatedSessionID = "8d2e4b71-1c93-4f05-b6a7-9e3d0c5a2f68"
	mountFreshSessionID     = "b47a0e63-5d28-4c91-8f37-6a1b2d9c4e05"
	mountProjectRowID       = "git@github.com:acme/tool.git"
)

// mountPreviewSessions is the discovery listing the mounted preview names rows
// from - the same shape the selection tree is folded from.
func mountPreviewSessions() []ftue.SessionListing {
	return []ftue.SessionListing{
		{
			Harness:     string(defaults.HarnessClaudeCode),
			SessionID:   mountImportedSessionID,
			Title:       "imported session",
			ProjectName: "acme/tool",
			GitRemote:   mountProjectRowID,
			Branch:      "main",
		},
		{
			Harness:     string(defaults.HarnessClaudeCode),
			SessionID:   mountTruncatedSessionID,
			Title:       "truncated session",
			ProjectName: "acme/tool",
			GitRemote:   mountProjectRowID,
			Branch:      "main",
		},
		{
			Harness:     string(defaults.HarnessClaudeCode),
			SessionID:   mountFreshSessionID,
			Title:       "fresh session",
			ProjectName: "acme/tool",
			GitRemote:   mountProjectRowID,
			Branch:      "main",
		},
	}
}

// mountTestCmd builds the command instance the mount helpers read their data
// directory from. The path lives on the per-invocation command, never in
// process-global env, so these tests stay parallel-safe.
func mountTestCmd(t *testing.T, dataHome string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("data-dir", dataHome, "data directory override")
	cmd.SetContext(t.Context())
	return cmd
}

// truncatedStoredBody is the degraded read the fixture's second session stands
// in for: a body the database cut at its content-preview limit, with no source
// transcript on disk to recover the rest from. It is exactly at the limit
// because that is what marks an entry as truncated.
var truncatedStoredBody = "the store cut this body at its preview limit" +
	strings.Repeat(" and the rest of it is gone", 1+defaults.ContentPreviewLimit/27)

// seedKickstartStore creates a real store under dataHome and writes two
// sessions into it through the SAME insert + index path the ingest pipeline
// uses: the recorded conversation, and the truncated-body session the degraded
// read is asserted against.
func seedKickstartStore(t *testing.T, dataHome string, recorded []testutil.TurnFixture) {
	t.Helper()
	dbPath := defaults.ResolveDBFilePathWith(dataHome).String()
	if err := os.MkdirAll(filepath.Dir(dbPath), defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Now().UnixMilli()
	sessions := []ingest.StoreEntry{
		storeEntryFor(mountImportedSessionID, now),
		storeEntryFor(mountTruncatedSessionID, now),
	}
	if err := db.InsertSessions(t.Context(), sessions); err != nil {
		t.Fatalf("insert sessions: %v", err)
	}

	recordedID := ingest.SessionID(mountImportedSessionID)
	if err := db.IndexSessionEntries(t.Context(), recordedID, sessionEntries(t, recordedID, recorded, now)); err != nil {
		t.Fatalf("index recorded session entries: %v", err)
	}

	cut := truncatedStoredBody[:defaults.ContentPreviewLimit]
	truncatedID := ingest.SessionID(mountTruncatedSessionID)
	truncatedEntries := []schema.SessionEntry{{
		SessionID:      truncatedID,
		EntryIndex:     0,
		Harness:        defaults.HarnessClaudeCode,
		EntryType:      ingest.EntryTypeText,
		Role:           ingest.RoleUser,
		ContentPreview: &cut,
		Depth:          0,
		TimestampMs:    &now,
	}}
	if err := db.IndexSessionEntries(t.Context(), truncatedID, truncatedEntries); err != nil {
		t.Fatalf("index truncated session entries: %v", err)
	}
}

// storeEntryFor builds the session row one seeded session is inserted from.
func storeEntryFor(sessionID string, now int64) ingest.StoreEntry {
	return ingest.StoreEntry{Metadata: &ingest.UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     ingest.SessionID(sessionID),
		ModelHarness:  defaults.HarnessClaudeCode,
		Model:         ingest.ModelID("claude-opus-4-6"),
		HostSlug:      ingest.HostSlug("github.com--acme--tool"),
		Timestamp:     ingest.TimestampInfo{Start: now, End: now, Ingested: &now},
		Project:       ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "acme/tool", FilePath: "/tmp/acme/tool"},
		Source:        ingest.SourceInfo{Format: ingest.SourceFormatJSONL},
	}}
}

// sessionEntries lays fixture turns out as the ROW SHAPE the store actually
// holds, which is not the turn shape: a turn's own text is one depth-0 row, and
// each of its tool calls is a depth-1 tool_use row plus a depth-1 tool_result
// row joined to it by tool call id. Seeding the real shape is the point - it is
// what makes EntriesToTurns part of what these tests cover instead of something
// they route around.
func sessionEntries(t *testing.T, sessionID ingest.SessionID, rows []testutil.TurnFixture, now int64) []schema.SessionEntry {
	t.Helper()
	turns := testutil.Turns(t, string(sessionID), rows)
	var entries []schema.SessionEntry
	index := 0
	for _, turn := range turns {
		parent := index
		content := turn.Content
		entries = append(entries, schema.SessionEntry{
			SessionID:      sessionID,
			EntryIndex:     parent,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      turn.EntryType,
			Role:           turn.Role,
			ContentPreview: &content,
			Depth:          0,
			TimestampMs:    &now,
		})
		index++
		for callNo, call := range turn.ToolCalls {
			callID := fmt.Sprintf("%s-call-%d-%d", sessionID, parent, callNo)
			name, args, result := call.Name, call.Arguments, call.Result
			parentIndex := parent
			entries = append(entries,
				schema.SessionEntry{
					SessionID:    sessionID,
					EntryIndex:   index,
					Harness:      defaults.HarnessClaudeCode,
					EntryType:    ingest.EntryTypeToolUse,
					Role:         ingest.RoleAssistant,
					Depth:        1,
					ParentIndex:  &parentIndex,
					ToolCallID:   &callID,
					ToolNamesCSV: &name,
					ToolInput:    &args,
					TimestampMs:  &now,
				},
				schema.SessionEntry{
					SessionID:   sessionID,
					EntryIndex:  index + 1,
					Harness:     defaults.HarnessClaudeCode,
					EntryType:   ingest.EntryTypeToolResult,
					Role:        ingest.RoleTool,
					Depth:       1,
					ParentIndex: &parentIndex,
					ToolCallID:  &callID,
					ToolOutput:  &result,
					IsError:     call.IsError,
					TimestampMs: &now,
				})
			index += 2
		}
	}
	return entries
}

// previewCase is one highlighted row and the lines the mounted pane must and
// must not carry for it.
type previewCase struct {
	Name         string   `yaml:"name"`
	Highlight    string   `yaml:"highlight"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

// previewDoc is the whole fixture plus its row-count guard. Recorded is the
// conversation the real store is seeded with.
type previewDoc struct {
	ExpectedCaseCount int                    `yaml:"expectedCaseCount"`
	Width             int                    `yaml:"width"`
	Recorded          []testutil.TurnFixture `yaml:"recorded"`
	Cases             []previewCase          `yaml:"cases"`
}

//go:embed testdata/kickstart_preview.yaml
var kickstartPreviewData []byte

func loadPreviewDoc(t *testing.T) previewDoc {
	t.Helper()
	var doc previewDoc
	dec := yaml.NewDecoder(bytes.NewReader(kickstartPreviewData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/kickstart_preview.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("kickstart_preview.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", doc.ExpectedCaseCount, len(doc.Cases))
	}
	if len(doc.Recorded) == 0 {
		t.Fatal("fixture records no turns; every case would take the not-imported path")
	}
	if doc.Width <= 0 {
		t.Fatalf("fixture declares width %d; a non-positive width renders nothing to assert on", doc.Width)
	}
	names := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("preview case name %q is missing or duplicated", c.Name)
		}
		names[c.Name] = true
		if c.Highlight == "" {
			t.Fatalf("preview case %q highlights nothing; the zero id previews whatever it resolves to", c.Name)
		}
		if len(c.WantContains)+len(c.WantMissing) == 0 {
			t.Fatalf("preview case %q asserts nothing; an empty want list is a guaranteed pass", c.Name)
		}
		for _, want := range append(append([]string{}, c.WantContains...), c.WantMissing...) {
			if strings.TrimSpace(want) == "" {
				t.Fatalf("preview case %q declares an empty needle; it matches regardless of the code", c.Name)
			}
		}
	}
	return doc
}

// flattenPane strips the styling, folds the gutter rails away, and joins wrapped
// rows, so a phrase the pane split across a wrap point is still findable.
func flattenPane(s string) string {
	out := ansiPattern.ReplaceAllString(s, "")
	out = strings.ReplaceAll(out, "│ ", "")
	out = strings.ReplaceAll(out, "│", "")
	return strings.Join(strings.Fields(out), " ")
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// needle normalises an expected phrase the same way flattenPane normalises the
// pane, so a multi-word expectation is compared against one row of text.
func needle(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestKickstartPreview_ReadsTheLocalStore proves the mounted glue end to end
// over a REAL store: openKickstartStore opens the store the command resolves,
// kickstartPreview threads the session read path into the preview source, and
// the pane for an imported session renders the WHOLE recorded transcript the
// store holds - role tags, the code in a later turn, and the tool step.
//
// Nothing between the store and the assertion is stubbed. The seeded entries go
// in through the same indexer the ingest pipeline uses and come back through
// EntriesToTurns, so a change to how entries fold into turns shows up here
// rather than passing against a hand-built turn list.
func TestKickstartPreview_ReadsTheLocalStore(t *testing.T) {
	t.Parallel()
	doc := loadPreviewDoc(t)
	dataHome := t.TempDir()
	seedKickstartStore(t, dataHome, doc.Recorded)

	cmd := mountTestCmd(t, dataHome)
	db, closeStore := openKickstartStore(cmd)
	defer closeStore()
	if db == nil {
		t.Fatal("openKickstartStore returned no store for a seeded data directory")
	}

	source := kickstartPreview(cmd, db, theme.New(theme.ModeDark), mountPreviewSessions())

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			body, err := source.Body(c.Highlight)
			if err != nil {
				t.Fatalf("preview body for %q: %v", c.Highlight, err)
			}
			if body == nil {
				t.Fatalf("preview for %q loaded no body at all", c.Highlight)
			}
			got := flattenPane(body.Render(doc.Width))
			for _, want := range c.WantContains {
				if !strings.Contains(got, needle(want)) {
					t.Errorf("preview must contain %q; got:\n%s", want, got)
				}
			}
			for _, missing := range c.WantMissing {
				if strings.Contains(got, needle(missing)) {
					t.Errorf("preview must not contain %q; got:\n%s", missing, got)
				}
			}
		})
	}
}

// TestKickstartPreview_HighlightsCodeFromTheStore proves the read path reaches
// the markdown renderer and its code highlighter over turns that came out of a
// REAL store, rather than printing the fence characters. The fixture's code
// arrives in a LATER turn, behind a plain opening prompt - the arrangement the
// replaced first-message preview could never have shown.
func TestKickstartPreview_HighlightsCodeFromTheStore(t *testing.T) {
	t.Parallel()
	doc := loadPreviewDoc(t)
	dataHome := t.TempDir()
	seedKickstartStore(t, dataHome, doc.Recorded)

	cmd := mountTestCmd(t, dataHome)
	db, closeStore := openKickstartStore(cmd)
	defer closeStore()

	source := kickstartPreview(cmd, db, theme.New(theme.ModeDark), mountPreviewSessions())
	body, err := source.Body(mountImportedSessionID)
	if err != nil {
		t.Fatalf("preview body: %v", err)
	}
	got := body.Render(doc.Width)

	if strings.Contains(ansiPattern.ReplaceAllString(got, ""), "```") {
		t.Errorf("the reply's code fence was printed literally instead of being rendered:\n%s", flattenPane(got))
	}
	// The keyword must be carried by a style run of its own: highlighting colors
	// it apart from the code around it, and unstyled code would not.
	if !strings.Contains(got, "\x1b[38;2;") {
		t.Fatalf("nothing in the pane carries a palette color:\n%q", got)
	}
	idx := strings.Index(got, "func")
	if idx <= 0 || !strings.HasSuffix(got[:idx], "m") {
		t.Errorf("the keyword %q from the stored reply is not syntax-highlighted; got:\n%q", "func", got)
	}
}

// TestKickstartPreview_WithoutAStoreStillNamesSessions proves the first-run
// path: with no database on disk yet, openKickstartStore yields no store and a
// no-op close, and the preview still names each discovered session by its
// harness and project and says it is not imported - which is exactly true
// before the first import.
func TestKickstartPreview_WithoutAStoreStillNamesSessions(t *testing.T) {
	t.Parallel()
	doc := loadPreviewDoc(t)
	cmd := mountTestCmd(t, t.TempDir())

	db, closeStore := openKickstartStore(cmd)
	if db != nil {
		t.Fatal("openKickstartStore opened a store for a data directory with no database")
	}
	closeStore() // must be safe to call

	source := kickstartPreview(cmd, db, theme.New(theme.ModeDark), mountPreviewSessions())
	body, err := source.Body(mountImportedSessionID)
	if err != nil {
		t.Fatalf("preview body without a store: %v", err)
	}
	got := flattenPane(body.Render(doc.Width))
	for _, want := range []string{"harness: claude code", "project: acme/tool", "not imported yet"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview must contain %q; got:\n%s", want, got)
		}
	}
}

// TestIngestedSessionIDs_ReadsTheStore proves the other half of the same glue:
// the ids the scanner marks as already imported come from the real store, and a
// run with no store degrades to marking nothing rather than failing onboarding.
func TestIngestedSessionIDs_ReadsTheStore(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedKickstartStore(t, dataHome, loadPreviewDoc(t).Recorded)

	cmd := mountTestCmd(t, dataHome)
	db, closeStore := openKickstartStore(cmd)
	defer closeStore()

	ids := ingestedSessionIDs(cmd, db)
	found := false
	for _, id := range ids {
		if id == mountImportedSessionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the seeded session is not reported as imported; got %v", ids)
	}
	if got := ingestedSessionIDs(mountTestCmd(t, t.TempDir()), nil); len(got) != 0 {
		t.Fatalf("with no store nothing can be marked imported; got %v", got)
	}
}
