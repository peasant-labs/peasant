package ingest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// TestClaudeVSCodeExtensionPromptIsOneUserTurn reproduces the Claude Code VS
// Code extension shape: a user record whose content array holds injected editor
// context followed by the typed prompt. The carrier is one user turn with the
// whole prompt; the blocks must not become depth-1 subagent turns.
func TestClaudeVSCodeExtensionPromptIsOneUserTurn(t *testing.T) {
	t.Parallel()
	const contextText = "<ide_opened_file>The user opened the file /src/main.go in the IDE. This may or may not be related to the current task.</ide_opened_file>"
	line := `{"type":"user","uuid":"u1","sessionId":"` + testutil.TestSessionUUID + `","message":{"role":"user","content":[{"type":"text","text":` +
		`"<ide_opened_file>The user opened the file /src/main.go in the IDE. This may or may not be related to the current task.</ide_opened_file>"},` +
		`{"type":"text","text":"Test"}]}}` + "\n"
	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile("/test/session.jsonl", []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	indexer := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true), ingest.WithClaudeFullContent(true))
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      ingest.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}
	entries, err := indexer.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stored entries = %d, want one user carrier", len(entries))
	}
	user := entries[0]
	if user.Depth != 0 || user.Role != schema.RoleUser || user.EntryType != schema.EntryTypeText {
		t.Fatalf("carrier = depth %d role %q type %q, want a depth-0 user text entry", user.Depth, user.Role, user.EntryType)
	}
	if user.ContentPreview == nil {
		t.Fatal("carrier has no prompt text")
	}
	if *user.ContentPreview != contextText+"\nTest" {
		t.Fatalf("carrier content = %q, want injected context and the typed prompt", *user.ContentPreview)
	}

	turns := transcript.EntriesToTurns(entries)
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want one user turn", len(turns))
	}
	if turns[0].Role != schema.RoleUser || turns[0].Depth != 0 {
		t.Fatalf("turn = role %q depth %d, want the depth-0 user prompt", turns[0].Role, turns[0].Depth)
	}
	if !strings.Contains(turns[0].Content, "Test") {
		t.Fatalf("turn content = %q, want the typed prompt", turns[0].Content)
	}
}
