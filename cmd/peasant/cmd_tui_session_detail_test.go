package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/annotations"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tuitest"
	"github.com/peasant-labs/schema"
)

func pressTUIKey(t *testing.T, m tea.Model, token string) (tea.Model, tea.Cmd) {
	t.Helper()
	return m.Update(tuitest.Key(token))
}

// TestTUISessionDetailUsesTheStore guards #685: `peasant tui` handed the app
// session summaries without a store, so the detail view's Turns section stayed
// empty, the turn keys did nothing, the annotation picker had no types, and q
// in the picker quit the program.
func TestTUISessionDetailUsesTheStore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	sid := testutil.TestSessionUUID
	storetest.SeedSession(t, db, sid)
	first, second := "first stored turn", "second stored turn"
	entries := []schema.SessionEntry{
		{SessionID: schema.SessionID(sid), EntryIndex: 0, Harness: ingest.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &first},
		{SessionID: schema.SessionID(sid), EntryIndex: 1, Harness: ingest.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: &second},
	}
	if err := db.IndexSessionEntries(ctx, schema.SessionID(sid), entries); err != nil {
		t.Fatal(err)
	}
	types, err := annotations.NewTypeReader(db).ListTypes(ctx, schema.TypeFilter{})
	if err != nil || len(types) == 0 {
		t.Fatalf("the migrated store must hold annotation types for the picker: types=%d err=%v", len(types), err)
	}

	id, err := ingest.NewSessionID(sid)
	if err != nil {
		t.Fatal(err)
	}
	summary := ingest.Session{ID: id, Harness: ingest.HarnessClaudeCode, StartTime: time.Now(), EndTime: time.Now()}
	app, err := newTUIApp(ctx, db, []ingest.Session{summary})
	if err != nil {
		t.Fatal(err)
	}
	var m tea.Model = app
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = pressTUIKey(t, m, "2")
	m, _ = pressTUIKey(t, m, "enter")
	detail := m.View().Content
	for _, want := range []string{first, second, "> "} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail view does not show the stored turns (missing %q):\n%s", want, detail)
		}
	}

	m, _ = pressTUIKey(t, m, "l")
	if moved := m.View().Content; moved == detail {
		t.Fatalf("l did not move the turn cursor:\n%s", moved)
	}

	m, _ = pressTUIKey(t, m, "a")
	picker := m.View().Content
	if strings.Contains(picker, "No annotation types available") || !strings.Contains(picker, types[0].DisplayName) {
		t.Fatalf("annotation picker does not list the store's types (want %q):\n%s", types[0].DisplayName, picker)
	}

	m, cmd := pressTUIKey(t, m, "q")
	if cmd == nil {
		t.Fatal("q in the picker returned no command; it must close the picker")
	}
	msg := cmd()
	if _, quit := msg.(tea.QuitMsg); quit {
		t.Fatal("q in the annotation picker quit the program")
	}
	m, _ = m.Update(msg)
	if closed := m.View().Content; strings.Contains(closed, types[0].DisplayName) || !strings.Contains(closed, first) {
		t.Fatalf("q did not close the picker back to the detail view:\n%s", closed)
	}
}
