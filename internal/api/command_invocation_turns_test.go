package api_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// commandInvocationEntries builds one stored entry per corpus row. Every
// recorded command sits on a stored user-role entry: both indexers leave the
// role alone when they write command_name.
func commandInvocationEntries(sessionID schema.SessionID, cases []testutil.CommandInvocationTurnCase) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, len(cases))
	for index, testCase := range cases {
		timestamp := day1Ms + int64(index)
		extra := testCase.StoredExtra
		entries[index] = schema.SessionEntry{
			SessionID:   sessionID,
			EntryIndex:  index,
			Harness:     testCase.Harness,
			EntryType:   schema.EntryTypeText,
			Role:        schema.RoleUser,
			TimestampMs: &timestamp,
			Extra:       &extra,
		}
		if testCase.Content != "" {
			content := testCase.Content
			entries[index].ContentPreview = &content
		}
	}
	return entries
}

func TestStoreDataProvider_CommandInvocationReachesDetailPayload(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatal(err)
	}

	const sessionID = "45454545-4545-4545-4545-45454545454a"
	db := openTestStore(t)
	storeEntry := makeStoreEntry(
		t,
		sessionID,
		hash1,
		"github.com-test",
		defaults.HarnessClaudeCode,
		day1Ms,
		100,
		50,
		"project-command-invocation",
		len(fixture.Cases),
		0,
		1000,
	)
	provider := seedStore(t, db, []ingest.StoreEntry{storeEntry})
	sid := schema.SessionID(sessionID)
	if err := testutil.WriteFullEntries(context.Background(), db, sid, commandInvocationEntries(sid, fixture.Cases)); err != nil {
		t.Fatalf("WriteFullEntries: %v", err)
	}

	session, err := provider.SessionByID(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	// The WebSocket handler serves exactly this producer boundary.
	payload, err := transcript.SessionToDetailValidated(session)
	if err != nil {
		t.Fatalf("SessionToDetailValidated: %v", err)
	}
	if len(payload.Turns) != len(fixture.Cases) {
		t.Fatalf("session detail has %d turns, want %d fixture rows", len(payload.Turns), len(fixture.Cases))
	}
	for index, testCase := range fixture.Cases {
		turn := payload.Turns[index]
		if turn.Index != index {
			t.Errorf("case %q turn index = %d, want %d", testCase.Name, turn.Index, index)
		}
		if turn.Role != testCase.ExpectedRole {
			t.Errorf("case %q websocket role = %q, want %q", testCase.Name, turn.Role, testCase.ExpectedRole)
		}
		if !testCase.ExpectedCommand {
			if turn.Command != nil {
				t.Errorf("case %q websocket payload carries command %+v, want none", testCase.Name, *turn.Command)
			}
			continue
		}
		if turn.Command == nil {
			t.Errorf("case %q websocket payload carries no command, want %q", testCase.Name, testCase.ExpectedName)
			continue
		}
		if turn.Command.Name != testCase.ExpectedName || turn.Command.Args != testCase.ExpectedArgs {
			t.Errorf("case %q websocket command = %+v, want name %q args %q", testCase.Name, *turn.Command, testCase.ExpectedName, testCase.ExpectedArgs)
		}
	}
}
