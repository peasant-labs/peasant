package api_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

func loadInjectedCommandTurnFixture(t *testing.T) testutil.InjectedCommandTurnFixture {
	t.Helper()
	fixture, err := testutil.LoadInjectedCommandTurnFixture()
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestEntriesToTurns_InjectedCommandRolesReachDetailPayload(t *testing.T) {
	t.Parallel()
	fixture := loadInjectedCommandTurnFixture(t)
	sessionID := schema.SessionID("45454545-4545-4545-4545-454545454545")

	for index, testCase := range fixture.Cases {
		index, testCase := index, testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			content := testCase.StoredContent()
			entries := []schema.SessionEntry{{
				SessionID:      sessionID,
				EntryIndex:     index,
				Harness:        testCase.Harness,
				EntryType:      schema.EntryTypeText,
				Role:           testCase.SourceRole,
				ContentPreview: &content,
			}}

			turns := api.EntriesToTurns(entries)
			if len(turns) != 1 {
				t.Fatalf("EntriesToTurns returned %d turns, want 1", len(turns))
			}
			if turns[0].Role != testCase.ExpectedRole {
				t.Errorf("EntriesToTurns role = %q, want %q", turns[0].Role, testCase.ExpectedRole)
			}
			if turns[0].Content != content {
				t.Errorf("EntriesToTurns content = %q, want unchanged %q", turns[0].Content, content)
			}

			session := &ingest.Session{
				ID:      sessionID,
				Harness: testCase.Harness,
				Turns:   turns,
			}
			payload := api.SessionToDetail(session)
			if len(payload.Turns) != 1 {
				t.Fatalf("SessionToDetail returned %d turns, want 1", len(payload.Turns))
			}
			if payload.Turns[0].Role != testCase.ExpectedRole {
				t.Errorf("SessionToDetail role = %q, want %q", payload.Turns[0].Role, testCase.ExpectedRole)
			}
		})
	}
}

func TestStoreDataProvider_InjectedCommandRolesReachDetailPayload(t *testing.T) {
	t.Parallel()
	fixture := loadInjectedCommandTurnFixture(t)

	const sessionID = "45454545-4545-4545-4545-454545454546"
	db := openTestStore(t)
	storeEntry := makeStoreEntry(
		t,
		sessionID,
		hash1,
		"github.com-test",
		fixture.Cases[0].Harness,
		day1Ms,
		100,
		50,
		"project-injected-command",
		len(fixture.Cases),
		0,
		1000,
	)
	provider := seedStore(t, db, []ingest.StoreEntry{storeEntry})
	sid := schema.SessionID(sessionID)
	entries := make([]schema.SessionEntry, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		content := testCase.StoredContent()
		entries[index] = schema.SessionEntry{
			SessionID:      sid,
			EntryIndex:     index,
			Harness:        testCase.Harness,
			EntryType:      schema.EntryTypeText,
			Role:           testCase.SourceRole,
			ContentPreview: &content,
		}
	}
	if err := db.IndexSessionEntries(context.Background(), sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	session, err := provider.SessionByID(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	payload := transcript.SessionToDetail(session)
	if len(payload.Turns) != len(fixture.Cases) {
		t.Fatalf("stored entry detail has %d turns, want %d fixture rows", len(payload.Turns), len(fixture.Cases))
	}
	for index, testCase := range fixture.Cases {
		turn := payload.Turns[index]
		if turn.Index != index {
			t.Errorf("case %q stored detail turn index = %d, want %d", testCase.Name, turn.Index, index)
		}
		if turn.Role != testCase.ExpectedRole {
			t.Errorf("case %q stored detail role = %q, want %q", testCase.Name, turn.Role, testCase.ExpectedRole)
		}
		if turn.Content != testCase.StoredContent() {
			t.Errorf("case %q stored detail content changed", testCase.Name)
		}
	}
}
