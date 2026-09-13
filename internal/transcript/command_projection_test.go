package transcript_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// commandFixtureEntry builds the one stored entry a corpus row describes. The
// row carries its own stored role because the three indexers differ: the Claude
// Code and Cursor indexers record a command only on a user entry, while the
// OpenCode indexer also records one on an assistant message that called a
// skill.
func commandFixtureEntry(sessionID schema.SessionID, index int, testCase testutil.CommandInvocationTurnCase) schema.SessionEntry {
	timestamp := int64(1705276800000)
	extra := testCase.StoredExtra
	entry := schema.SessionEntry{
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
		entry.ContentPreview = &content
	}
	return entry
}

func TestEntriesToTurns_ProjectsStoredCommandInvocation(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatal(err)
	}
	sessionID := schema.SessionID("45454545-4545-4545-4545-454545454548")

	for index, testCase := range fixture.Cases {
		index, testCase := index, testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			turns := transcript.EntriesToTurns([]schema.SessionEntry{commandFixtureEntry(sessionID, index, testCase)})
			if len(turns) != 1 {
				t.Fatalf("EntriesToTurns returned %d turns, want 1", len(turns))
			}
			if turns[0].Role != testCase.ExpectedRole {
				t.Errorf("role = %q, want %q", turns[0].Role, testCase.ExpectedRole)
			}
			if !testCase.ExpectedCommand {
				if turns[0].Command != nil {
					t.Fatalf("command = %+v, want none", *turns[0].Command)
				}
				return
			}
			if turns[0].Command == nil {
				t.Fatal("command is absent, want one")
			}
			if turns[0].Command.Name != testCase.ExpectedName {
				t.Errorf("command name = %q, want %q", turns[0].Command.Name, testCase.ExpectedName)
			}
			if turns[0].Command.Args != testCase.ExpectedArgs {
				t.Errorf("command args = %q, want %q", turns[0].Command.Args, testCase.ExpectedArgs)
			}
		})
	}
}

func TestSessionToDetail_CarriesCommandInvocationOntoTheWire(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatal(err)
	}
	sessionID := schema.SessionID("45454545-4545-4545-4545-454545454549")

	entries := make([]schema.SessionEntry, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		entries[index] = commandFixtureEntry(sessionID, index, testCase)
	}
	turns := transcript.EntriesToTurns(entries)
	if len(turns) != len(fixture.Cases) {
		t.Fatalf("EntriesToTurns returned %d turns, want %d fixture rows", len(turns), len(fixture.Cases))
	}
	payload := transcript.SessionToDetail(&ingest.Session{
		ID:      sessionID,
		Harness: schema.HarnessClaudeCode,
		Turns:   turns,
	})
	if len(payload.Turns) != len(fixture.Cases) {
		t.Fatalf("SessionToDetail returned %d turns, want %d", len(payload.Turns), len(fixture.Cases))
	}
	for index, testCase := range fixture.Cases {
		turn := payload.Turns[index]
		if turn.Role != testCase.ExpectedRole {
			t.Errorf("case %q wire role = %q, want %q", testCase.Name, turn.Role, testCase.ExpectedRole)
		}
		if !testCase.ExpectedCommand {
			if turn.Command != nil {
				t.Errorf("case %q wire command = %+v, want none", testCase.Name, *turn.Command)
			}
			continue
		}
		if turn.Command == nil {
			t.Errorf("case %q wire command is absent, want %q", testCase.Name, testCase.ExpectedName)
			continue
		}
		if turn.Command.Name != testCase.ExpectedName || turn.Command.Args != testCase.ExpectedArgs {
			t.Errorf("case %q wire command = %+v, want name %q args %q", testCase.Name, *turn.Command, testCase.ExpectedName, testCase.ExpectedArgs)
		}
	}
}

// TestSessionToDetailValidated_AssistantSkillTurnKeepsModelEvidence pins the
// producer boundary against re-roling an assistant's own skill call. One
// OpenCode shape records the invocation on an assistant message that also
// carries a model observation; observedModel is only valid on an assistant
// turn, so moving that turn to any other role makes the whole session fail
// validation and takes the WebSocket, the export, and the push body with it.
func TestSessionToDetailValidated_AssistantSkillTurnKeepsModelEvidence(t *testing.T) {
	t.Parallel()
	const observedModel = "anthropic/claude-fable-5"
	const commandName = "/project:summarize"

	sessionID := schema.SessionID("45454545-4545-4545-4545-45454545454e")
	timestamp := int64(1705276800000)
	extra := `{"command_name":"` + commandName + `","model_id":"` + observedModel + `"}`
	entry := schema.SessionEntry{
		SessionID:   sessionID,
		EntryIndex:  0,
		Harness:     schema.HarnessOpenCode,
		EntryType:   schema.EntryTypeText,
		Role:        schema.RoleAssistant,
		TimestampMs: &timestamp,
		Extra:       &extra,
	}

	turns := transcript.EntriesToTurns([]schema.SessionEntry{entry})
	if len(turns) != 1 {
		t.Fatalf("EntriesToTurns returned %d turns, want 1", len(turns))
	}

	payload, err := transcript.SessionToDetailValidated(&ingest.Session{
		ID:      sessionID,
		Harness: schema.HarnessOpenCode,
		Turns:   turns,
	})
	if err != nil {
		t.Fatalf("SessionToDetailValidated returned an error for an assistant skill turn carrying model evidence: %v", err)
	}
	if len(payload.Turns) != 1 {
		t.Fatalf("session detail has %d turns, want 1", len(payload.Turns))
	}

	turn := payload.Turns[0]
	if turn.Role != schema.RoleAssistant {
		t.Errorf("role = %q, want %q", turn.Role, schema.RoleAssistant)
	}
	if turn.ObservedModel.String() != observedModel {
		t.Errorf("observedModel = %q, want %q", turn.ObservedModel.String(), observedModel)
	}
	if turn.Command == nil {
		t.Fatalf("command is absent, want %q", commandName)
	}
	if turn.Command.Name != commandName {
		t.Errorf("command name = %q, want %q", turn.Command.Name, commandName)
	}
}
