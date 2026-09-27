package store_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// These listing and first-user-message cases exercise internal/store directly
// rather than a command, so they live beside the store rather than in the
// command package. Their overlap with reader_test.go is deliberately partial:
// reader_test.go's filter matrix proves CountSessionsFiltered agrees with
// len(ListSessionsFiltered), while these cases assert the listed rows' shape
// (provider, limit, session id, project hash) that the count parity cannot.

// TestStore_ListSessionsFiltered_All verifies basic listing with no filters.
func TestStore_ListSessionsFiltered_All(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	storetest.SeedSession(t, db, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	rows, err := db.ListSessionsFiltered(ctx, store.SessionListFilter{
		SortField: defaults.SessionSortDate,
		SortDesc:  true,
	})
	if err != nil {
		t.Fatalf("ListSessionsFiltered: %v", err)
	}
	if len(rows) < 2 {
		t.Errorf("expected at least 2 sessions; got %d", len(rows))
	}
}

// TestStore_ListSessionsFiltered_Provider verifies provider (ModelHarness) filtering.
func TestStore_ListSessionsFiltered_Provider(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	// Filter by claude (all seeded sessions are claude).
	claudeStr := string(defaults.HarnessClaudeCode)
	rows, err := db.ListSessionsFiltered(ctx, store.SessionListFilter{
		SessionFilter: store.SessionFilter{ModelHarness: &claudeStr},
		SortField:     defaults.SessionSortDate,
		SortDesc:      true,
	})
	if err != nil {
		t.Fatalf("ListSessionsFiltered by provider: %v", err)
	}
	for _, row := range rows {
		if row.ModelHarness != string(defaults.HarnessClaudeCode) {
			t.Errorf("expected provider %q; got %q", defaults.HarnessClaudeCode, row.ModelHarness)
		}
	}
}

// TestStore_ListSessionsFiltered_Limit verifies LIMIT is respected.
func TestStore_ListSessionsFiltered_Limit(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	storetest.SeedSession(t, db, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	storetest.SeedSession(t, db, "cccccccc-cccc-cccc-cccc-cccccccccccc")

	rows, err := db.ListSessionsFiltered(ctx, store.SessionListFilter{
		SortField: defaults.SessionSortDate,
		SortDesc:  true,
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("ListSessionsFiltered with limit: %v", err)
	}
	if len(rows) > 2 {
		t.Errorf("expected at most 2 sessions with limit=2; got %d", len(rows))
	}
}

// TestStore_FirstUserMessage_Basic verifies the first user message is returned.
func TestStore_FirstUserMessage_Basic(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	msg := "Please write a unit test for this function"
	entries := []schema.SessionEntry{
		{
			SessionID:      schema.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &msg,
		},
	}
	if err := db.IndexSessionEntries(ctx, schema.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	preview, err := db.FirstUserMessage(ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("FirstUserMessage: %v", err)
	}
	if preview == "" {
		t.Error("expected non-empty preview")
	}
	// Should be truncated to at most SessionPreviewMaxChars.
	runeCount := 0
	for range preview {
		runeCount++
	}
	if runeCount > defaults.SessionPreviewMaxChars {
		t.Errorf("preview exceeds %d runes: %d", defaults.SessionPreviewMaxChars, runeCount)
	}
	// Should contain beginning of the message.
	if !strings.HasPrefix(preview, "Please write") {
		t.Errorf("expected preview to start with 'Please write'; got %q", preview)
	}
}

// TestStore_FirstUserMessage_UnicodeSafe verifies unicode-aware truncation.
func TestStore_FirstUserMessage_UnicodeSafe(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	// A message that is longer than 40 runes, with multi-byte characters.
	longMsg := "こんにちは世界、私はAIアシスタントです。このメッセージは日本語で書かれています。"
	entries := []schema.SessionEntry{
		{
			SessionID:      schema.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &longMsg,
		},
	}
	if err := db.IndexSessionEntries(ctx, schema.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	preview, err := db.FirstUserMessage(ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("FirstUserMessage: %v", err)
	}
	runeCount := 0
	for range preview {
		runeCount++
	}
	if runeCount > defaults.SessionPreviewMaxChars {
		t.Errorf("unicode preview exceeds %d runes: %d runes in %q", defaults.SessionPreviewMaxChars, runeCount, preview)
	}
}

// TestStore_FirstUserMessage_NoEntries verifies an empty preview when no entries exist.
func TestStore_FirstUserMessage_NoEntries(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	preview, err := db.FirstUserMessage(ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("FirstUserMessage with no entries: %v", err)
	}
	if preview != "" {
		t.Errorf("expected empty preview for session with no entries; got %q", preview)
	}
}

// TestStore_FirstUserMessage_MissingSession verifies an empty preview for unknown session.
func TestStore_FirstUserMessage_MissingSession(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	preview, err := db.FirstUserMessage(ctx, "nonexistent-session-id")
	if err != nil {
		t.Fatalf("FirstUserMessage for unknown session: %v", err)
	}
	if preview != "" {
		t.Errorf("expected empty preview for missing session; got %q", preview)
	}
}

// TestStore_ListSessionsFiltered_SessionID verifies the SessionID filter returns
// only the exact session and carries its project_hash.
func TestStore_ListSessionsFiltered_SessionID(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	ctx := t.Context()

	storetest.SeedSession(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	storetest.SeedSession(t, db, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	want := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	rows, err := db.ListSessionsFiltered(ctx, store.SessionListFilter{
		SessionID: &want,
		SortField: defaults.SessionSortDate,
		SortDesc:  true,
	})
	if err != nil {
		t.Fatalf("ListSessionsFiltered by session id: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row for session id %q; got %d", want, len(rows))
	}
	if rows[0].SessionID != want {
		t.Errorf("expected session id %q; got %q", want, rows[0].SessionID)
	}
	if rows[0].ProjectHash == "" {
		t.Error("expected non-empty ProjectHash on the row")
	}
}
