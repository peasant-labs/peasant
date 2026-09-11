package export_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// seedCommandInvocationSession writes one stored entry per corpus row. Every
// recorded command sits on a stored user-role entry: both indexers leave the
// role alone when they write command_name.
func seedCommandInvocationSession(t *testing.T, db *store.Store, sessionID string, cases []testutil.CommandInvocationTurnCase) {
	t.Helper()
	storetest.SeedSession(t, db, sessionID)
	sid := schema.SessionID(sessionID)
	entries := make([]schema.SessionEntry, len(cases))
	for index, testCase := range cases {
		timestamp := int64(1705276800000) + int64(index)
		extra := testCase.StoredExtra
		entries[index] = schema.SessionEntry{
			SessionID:   sid,
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
	if err := testutil.WriteFullEntries(context.Background(), db, sid, entries); err != nil {
		t.Fatalf("WriteFullEntries: %v", err)
	}
}

func TestExportSession_CarriesCommandInvocation(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatal(err)
	}

	const sessionID = "45454545-4545-4545-4545-45454545454c"
	db := storetest.Open(t)
	seedCommandInvocationSession(t, db, sessionID, fixture.Cases)

	payload, err := export.ExportSession(context.Background(), db, testutil.NewMemFS(), sessionID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if len(payload.Turns) != len(fixture.Cases) {
		t.Fatalf("exported payload has %d turns, want %d fixture rows", len(payload.Turns), len(fixture.Cases))
	}
	for index, testCase := range fixture.Cases {
		turn := payload.Turns[index]
		if turn.Role != testCase.ExpectedRole {
			t.Errorf("case %q exported role = %q, want %q", testCase.Name, turn.Role, testCase.ExpectedRole)
		}
		if !testCase.ExpectedCommand {
			if turn.Command != nil {
				t.Errorf("case %q export carries command %+v, want none", testCase.Name, *turn.Command)
			}
			continue
		}
		if turn.Command == nil {
			t.Errorf("case %q export carries no command, want %q", testCase.Name, testCase.ExpectedName)
			continue
		}
		if turn.Command.Name != testCase.ExpectedName || turn.Command.Args != testCase.ExpectedArgs {
			t.Errorf("case %q exported command = %+v, want name %q args %q", testCase.Name, *turn.Command, testCase.ExpectedName, testCase.ExpectedArgs)
		}
	}
}

// TestExportSession_CommandInvocationMatchesWebSocketPath is the parity gate:
// the three surfaces cannot drift from each other because they share one
// conversion path, and this fails the moment one of them stops doing so.
func TestExportSession_CommandInvocationMatchesWebSocketPath(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatal(err)
	}

	const sessionID = "45454545-4545-4545-4545-45454545454d"
	db := storetest.Open(t)
	seedCommandInvocationSession(t, db, sessionID, fixture.Cases)

	exported, err := export.ExportSession(context.Background(), db, testutil.NewMemFS(), sessionID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}

	provider := api.NewStoreDataProvider(db, sessionvisibility.All())
	session, err := provider.SessionByID(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	served, err := transcript.SessionToDetailValidated(session)
	if err != nil {
		t.Fatalf("SessionToDetailValidated: %v", err)
	}

	if len(exported.Turns) != len(served.Turns) {
		t.Fatalf("exported payload has %d turns, websocket payload has %d", len(exported.Turns), len(served.Turns))
	}
	for index := range exported.Turns {
		exportedCommand := exported.Turns[index].Command
		servedCommand := served.Turns[index].Command
		if (exportedCommand == nil) != (servedCommand == nil) {
			t.Fatalf("turn %d command presence differs: export=%v websocket=%v", index, exportedCommand != nil, servedCommand != nil)
		}
		if exportedCommand == nil {
			continue
		}
		if *exportedCommand != *servedCommand {
			t.Errorf("turn %d command differs: export=%+v websocket=%+v", index, *exportedCommand, *servedCommand)
		}
	}
}
