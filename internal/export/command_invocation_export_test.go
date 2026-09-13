package export_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// commandInvocationSessionEntries builds one stored entry per corpus row. Each
// row carries its own stored role because the three indexers differ: the
// Claude Code and Cursor indexers record a command only on a user entry, while
// the OpenCode indexer also records one on an assistant message that called a
// skill.
func commandInvocationSessionEntries(sessionID schema.SessionID, cases []testutil.CommandInvocationTurnCase) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, len(cases))
	for index, testCase := range cases {
		timestamp := int64(1705276800000) + int64(index)
		extra := testCase.StoredExtra
		entries[index] = schema.SessionEntry{
			SessionID:   sessionID,
			EntryIndex:  index,
			Harness:     testCase.Harness,
			EntryType:   schema.EntryTypeText,
			Role:        testCase.SourceRole,
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

// seedCommandInvocationSession writes one stored entry per corpus row into db.
func seedCommandInvocationSession(t *testing.T, db *store.Store, sessionID string, cases []testutil.CommandInvocationTurnCase) {
	t.Helper()
	storetest.SeedSession(t, db, sessionID)
	sid := schema.SessionID(sessionID)
	entries := commandInvocationSessionEntries(sid, cases)
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

	pushed, err := push.BuildTranscriptContentValidated(
		&ingest.UnifiedMetadata{
			SessionID:    schema.SessionID(sessionID),
			ModelHarness: defaults.HarnessClaudeCode,
		},
		commandInvocationSessionEntries(schema.SessionID(sessionID), fixture.Cases),
		defaults.PublishSchemaVersion,
		config.DefaultPushFieldVisibility(),
		sessionorigin.User,
	)
	if err != nil {
		t.Fatalf("BuildTranscriptContentValidated: %v", err)
	}
	if pushed.SessionDetail == nil {
		t.Fatal("BuildTranscriptContentValidated returned no session detail payload")
	}

	if len(exported.Turns) != len(served.Turns) || len(exported.Turns) != len(pushed.SessionDetail.Turns) {
		t.Fatalf("turn counts differ: export=%d websocket=%d push=%d", len(exported.Turns), len(served.Turns), len(pushed.SessionDetail.Turns))
	}
	for index := range exported.Turns {
		// Role travels with the invocation: the projection may move a turn off
		// its stored role, and all three surfaces must make that call identically.
		exportRole := exported.Turns[index].Role
		servedRole := served.Turns[index].Role
		pushedRole := pushed.SessionDetail.Turns[index].Role
		if exportRole != servedRole || exportRole != pushedRole {
			t.Errorf("turn %d role differs: export=%q websocket=%q push=%q", index, exportRole, servedRole, pushedRole)
		}
		exportedCommand := exported.Turns[index].Command
		servedCommand := served.Turns[index].Command
		pushedCommand := pushed.SessionDetail.Turns[index].Command
		if (exportedCommand == nil) != (servedCommand == nil) || (exportedCommand == nil) != (pushedCommand == nil) {
			t.Fatalf("turn %d command presence differs: export=%v websocket=%v push=%v", index, exportedCommand != nil, servedCommand != nil, pushedCommand != nil)
		}
		if exportedCommand == nil {
			continue
		}
		if *exportedCommand != *servedCommand || *exportedCommand != *pushedCommand {
			t.Errorf("turn %d command differs: export=%+v websocket=%+v push=%+v", index, *exportedCommand, *servedCommand, *pushedCommand)
		}
	}
}
