package export_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// seedEntries indexes transcript bytes into the DB so ListEntries returns them.
// Returns the indexed entries for use in assertions.
func seedEntriesFromJSONL(t *testing.T, ctx context.Context, s interface {
	IndexSessionEntries(context.Context, ingest.SessionID, []schema.SessionEntry) error
}, fs ingest.FileSystem, sessionID string, data []byte) []schema.SessionEntry {
	t.Helper()
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID(sessionID),
		SourcePath:   ingest.ResolvedPath("/f"),
		SourceFormat: ingest.SourceFormatJSONL,
		Harness:      ingest.HarnessClaudeCode,
	}
	indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullContent(true), ingest.WithClaudeFullDepth(true))
	entries, err := indexer.IndexTranscriptBytes(ctx, session, data)
	if err != nil {
		t.Fatalf("seedEntries: IndexTranscriptBytes: %v", err)
	}
	if err := s.IndexSessionEntries(ctx, schema.SessionID(sessionID), entries); err != nil {
		t.Fatalf("seedEntries: IndexSessionEntries: %v", err)
	}
	return entries
}

// seedEntriesFromOpenCode indexes an OpenCode session directory into the DB.
// Returns the indexed entries for use in assertions.
func seedEntriesFromOpenCode(t *testing.T, ctx context.Context, s interface {
	IndexSessionEntries(context.Context, ingest.SessionID, []schema.SessionEntry) error
}, fs ingest.FileSystem, sessionID string, sourcePath string) []schema.SessionEntry {
	t.Helper()
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID(sessionID),
		SourcePath:   ingest.ResolvedPath(sourcePath),
		SourceFormat: ingest.SourceFormatJSON,
		Harness:      ingest.HarnessOpenCode,
	}
	indexer := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullContent(true), ingest.WithOpenCodeFullDepth(true))
	entries, err := indexer.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("seedEntriesFromOpenCode: IndexTranscript: %v", err)
	}
	if err := s.IndexSessionEntries(ctx, schema.SessionID(sessionID), entries); err != nil {
		t.Fatalf("seedEntriesFromOpenCode: IndexSessionEntries: %v", err)
	}
	return entries
}

// countConversationalTurns counts depth-0 user+assistant entries (matches computeTurns semantics).
func countConversationalTurns(entries []schema.SessionEntry) int {
	count := 0
	for i := range entries {
		e := &entries[i]
		if e.Depth == 0 && (e.Role == schema.RoleUser || e.Role == schema.RoleAssistant) {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// Claude JSONL integration test
// ---------------------------------------------------------------------------

// TestExportSession_ClaudeJSONL seeds a store with a Claude session whose
// source_path points to a JSONL fixture on MemFS, calls ExportSession, and
// verifies the exported envelope and turns.
//
// Key invariants under the new DB-first flow:
//   - turn.Index == DB entry_index (not loop counter)
//   - TurnCount == depth-0 user+assistant entries (not total entries)
//   - len(Turns) == total DB entry count
func TestExportSession_ClaudeJSONL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. Open a test store and seed a session.
	s := storetest.Open(t)
	sessionID := testutil.TestSessionUUID
	storetest.SeedSession(t, s, sessionID) // seeds source_path="/f", source_format="jsonl"

	// 2. Build a MemFS with the Claude JSONL transcript at the seeded source_path.
	fs := testutil.NewMemFS()
	userContent := "Hello, can you help me with a Go question?"
	assistantContent := "Of course! I would be happy to help you with Go."
	line1 := fmt.Sprintf(
		`{"type":"user","message":{"role":"user","content":%q}}`,
		userContent,
	)
	line2 := fmt.Sprintf(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input_tokens":100,"output_tokens":50}}}`,
		assistantContent,
	)
	transcript := line1 + "\n" + line2 + "\n"
	if err := fs.WriteFile("/f", []byte(transcript), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// 3. Seed DB entries from the transcript (DB is source of truth for indices).
	seededEntries := seedEntriesFromJSONL(t, ctx, s, fs, sessionID, []byte(transcript))
	// With fullDepth, the assistant message's content array [{"type":"text","text":...}]
	// is decomposed into: parent entry (depth=0) + text block child (depth=1) = 3 total.
	if len(seededEntries) != 3 {
		t.Fatalf("seeded entry count: got %d, want 3", len(seededEntries))
	}

	// 4. Export.
	exported, err := export.ExportSession(ctx, s, fs, sessionID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}

	// 5. Verify envelope.
	if exported.ID != sessionID {
		t.Errorf("ID: got %q, want %q", exported.ID, sessionID)
	}
	// Export applies EntriesToTurns: depth=1 text child is folded into the
	// depth=0 assistant parent. Result: 2 display turns (user + assistant).
	if exported.TurnCount != 2 {
		t.Errorf("TurnCount: got %d, want 2", exported.TurnCount)
	}
	if len(exported.Turns) != 2 {
		t.Fatalf("Turns length: got %d, want 2 (EntriesToTurns folds children)", len(exported.Turns))
	}

	// 6. Verify turns content and roles.
	// Turn 0: user message.
	if exported.Turns[0].Role != schema.RoleUser {
		t.Errorf("Turn[0].Role: got %q, want %q", exported.Turns[0].Role, schema.RoleUser)
	}
	if exported.Turns[0].Content != userContent {
		t.Errorf("Turn[0].Content: got %q, want %q", exported.Turns[0].Content, userContent)
	}

	// Turn 1: assistant (depth=0 parent with text child folded in).
	// Content should be the assistant's text (from the folded child or parent preview).
	if exported.Turns[1].Role != schema.RoleAssistant {
		t.Errorf("Turn[1].Role: got %q, want %q", exported.Turns[1].Role, schema.RoleAssistant)
	}

	// 8. Content must NOT be truncated (full text preserved).
	// Isolated in its own sub-test so failures here don't mask other phases.
	t.Run("long_content_not_truncated", func(t *testing.T) {
		s2 := storetest.Open(t)
		storetest.SeedSession(t, s2, sessionID)
		fs2 := testutil.NewMemFS()

		longContent := strings.Repeat("Z", 2000)
		longLine := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, longContent)
		longTranscript := longLine + "\n"
		if err := fs2.WriteFile("/f", []byte(longTranscript), 0644); err != nil {
			t.Fatalf("write long fixture: %v", err)
		}
		longSeededEntries := seedEntriesFromJSONL(t, ctx, s2, fs2, sessionID, []byte(longTranscript))
		if len(longSeededEntries) != 1 {
			t.Fatalf("long seeded entry count: got %d, want 1", len(longSeededEntries))
		}

		exportedLong, err := export.ExportSession(ctx, s2, fs2, sessionID)
		if err != nil {
			t.Fatalf("ExportSession (long): %v", err)
		}
		if len(exportedLong.Turns) != len(longSeededEntries) {
			t.Fatalf("long Turns length: got %d, want %d", len(exportedLong.Turns), len(longSeededEntries))
		}
		if exportedLong.Turns[0].Content != longContent {
			t.Errorf("long content not preserved: got length %d, want %d",
				len(exportedLong.Turns[0].Content), len(longContent))
		}
	})

	// 9. Verify token data is populated.
	// Isolated in its own sub-test with independent store and MemFS.
	t.Run("token_data_populated", func(t *testing.T) {
		s3 := storetest.Open(t)
		storetest.SeedSession(t, s3, sessionID)
		fs3 := testutil.NewMemFS()

		if err := fs3.WriteFile("/f", []byte(transcript), 0644); err != nil {
			t.Fatalf("write token fixture: %v", err)
		}
		tokenSeededEntries := seedEntriesFromJSONL(t, ctx, s3, fs3, sessionID, []byte(transcript))
		if len(tokenSeededEntries) != 3 {
			t.Fatalf("token seeded entry count: got %d, want 3", len(tokenSeededEntries))
		}

		exportedTokens, err := export.ExportSession(ctx, s3, fs3, sessionID)
		if err != nil {
			t.Fatalf("ExportSession (tokens): %v", err)
		}
		// Tokens are on the parent assistant entry (index 1), not the depth=1 text child.
		assistantTurn := exportedTokens.Turns[1]
		if assistantTurn.TokensIn == nil || *assistantTurn.TokensIn != 100 {
			t.Errorf("Turn[1].TokensIn: got %v, want 100", assistantTurn.TokensIn)
		}
		if assistantTurn.TokensOut == nil || *assistantTurn.TokensOut != 50 {
			t.Errorf("Turn[1].TokensOut: got %v, want 50", assistantTurn.TokensOut)
		}
	})
}

// ---------------------------------------------------------------------------
// OpenCode JSON integration test
// ---------------------------------------------------------------------------

// TestExportSession_OpenCodeJSON seeds a store with an OpenCode session whose
// source_format is "json" and verifies ExportSession produces correct turns.
func TestExportSession_OpenCodeJSON(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. Open a test store and seed an OpenCode session.
	s := storetest.Open(t)
	sessionID := testutil.TestOpenCodeSesID

	// Seed with OpenCode format. We need to use storetest.SeedSession but override
	// source_format to "json". Since SeedSession uses "jsonl", we need direct SQL.
	storetest.SeedSession(t, s, sessionID)
	// Override source_format and source_path to point to the OpenCode fixture.
	conn, err := s.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take conn: %v", err)
	}
	sesPath := "/opencode-store/storage/session/proj1/" + sessionID + ".json"
	stmt := conn.Prep("UPDATE sessions SET source_format = ?, source_path = ? WHERE session_id = ?")
	stmt.BindText(1, string(schema.SourceFormatJSON))
	stmt.BindText(2, sesPath)
	stmt.BindText(3, sessionID)
	if _, err := stmt.Step(); err != nil {
		s.Pool().Put(conn)
		t.Fatalf("update source_format: %v", err)
	}
	stmt.Finalize()
	s.Pool().Put(conn)

	// 2. Build MemFS with OpenCode directory structure.
	fs := testutil.NewMemFS()
	root := "/opencode-store"

	// Session JSON.
	sesJSON := fmt.Sprintf(`{"id":%q,"version":"0.1.0","projectID":"proj1","directory":"/home/test/project","time":{"created":1700000000000,"updated":1700000060000}}`, sessionID)
	if err := fs.WriteFile(sesPath, []byte(sesJSON), 0644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	// Message JSON — user message with inline content.
	userContent := "What is the meaning of life?"
	msgPath1 := fmt.Sprintf("%s/storage/message/%s/msg_001.json", root, sessionID)
	msgJSON1 := fmt.Sprintf(`{"id":"msg_001","sessionID":%q,"role":"user","time":{"created":1700000001000,"completed":1700000002000},"content":%q}`,
		sessionID, userContent)
	if err := fs.WriteFile(msgPath1, []byte(msgJSON1), 0644); err != nil {
		t.Fatalf("write msg 1: %v", err)
	}

	// Message JSON — assistant message with tokens.
	assistantContent := "The meaning of life is 42."
	msgPath2 := fmt.Sprintf("%s/storage/message/%s/msg_002.json", root, sessionID)
	msgJSON2 := fmt.Sprintf(`{"id":"msg_002","sessionID":%q,"role":"assistant","time":{"created":1700000003000,"completed":1700000004000},"tokens":{"input":200,"output":75,"reasoning":0,"cache_read":0,"cache_write":0},"content":%q}`,
		sessionID, assistantContent)
	if err := fs.WriteFile(msgPath2, []byte(msgJSON2), 0644); err != nil {
		t.Fatalf("write msg 2: %v", err)
	}

	// 3. Seed DB entries from the OpenCode session directory.
	seededEntries := seedEntriesFromOpenCode(t, ctx, s, fs, sessionID, sesPath)

	// 4. Export.
	exported, err := export.ExportSession(ctx, s, fs, sessionID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}

	// 5. Verify TurnCount = depth-0 user+assistant entries.
	wantTurnCount := countConversationalTurns(seededEntries)
	if exported.TurnCount != wantTurnCount {
		t.Errorf("TurnCount: got %d, want %d", exported.TurnCount, wantTurnCount)
	}
	// len(Turns) = total DB entries.
	if len(exported.Turns) != len(seededEntries) {
		t.Fatalf("Turns length: got %d, want %d", len(exported.Turns), len(seededEntries))
	}

	// 6. Verify each turn's Index matches DB entry_index.
	for i, e := range seededEntries {
		if exported.Turns[i].Index != e.EntryIndex {
			t.Errorf("Turns[%d].Index: got %d, want DB entry_index %d", i, exported.Turns[i].Index, e.EntryIndex)
		}
	}

	// 7. Verify turn content and roles.
	if exported.Turns[0].Role != schema.RoleUser {
		t.Errorf("Turn[0].Role: got %q, want %q", exported.Turns[0].Role, schema.RoleUser)
	}
	if exported.Turns[0].Content != userContent {
		t.Errorf("Turn[0].Content: got %q, want %q", exported.Turns[0].Content, userContent)
	}

	if exported.Turns[1].Role != schema.RoleAssistant {
		t.Errorf("Turn[1].Role: got %q, want %q", exported.Turns[1].Role, schema.RoleAssistant)
	}
	if exported.Turns[1].Content != assistantContent {
		t.Errorf("Turn[1].Content: got %q, want %q", exported.Turns[1].Content, assistantContent)
	}

	// 8. Verify tokens populated from DB entries.
	if exported.Turns[1].TokensIn == nil || *exported.Turns[1].TokensIn != 200 {
		t.Errorf("Turn[1].TokensIn: got %v, want 200", exported.Turns[1].TokensIn)
	}
	if exported.Turns[1].TokensOut == nil || *exported.Turns[1].TokensOut != 75 {
		t.Errorf("Turn[1].TokensOut: got %v, want 75", exported.Turns[1].TokensOut)
	}
}

// ---------------------------------------------------------------------------
// OpenCode JSON with inline content array
// ---------------------------------------------------------------------------

// TestExportSession_OpenCodeJSON_WithParts verifies ExportSession handles
// OpenCode sessions where the assistant message uses inline content arrays
// (not separate part files). Inline array content is NOT decomposed into
// depth=1 children by the indexer; it yields a single depth=0 assistant entry.
// See TestExportSession_OpenCodeJSON_WithActualPartFiles for the part-file variant.
func TestExportSession_OpenCodeJSON_WithParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. Open a test store; use a distinct session ID to avoid conflicts.
	s := storetest.Open(t)
	// Use TestSessionUUID2 since TestOpenCodeSesID is used by the other OpenCode test.
	sessionID := testutil.TestSessionUUID2

	storetest.SeedSession(t, s, sessionID)
	conn, err := s.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take conn: %v", err)
	}
	sesPath := "/oc-parts/storage/session/projX/" + sessionID + ".json"
	stmt := conn.Prep("UPDATE sessions SET source_format = ?, source_path = ? WHERE session_id = ?")
	stmt.BindText(1, string(schema.SourceFormatJSON))
	stmt.BindText(2, sesPath)
	stmt.BindText(3, sessionID)
	if _, err := stmt.Step(); err != nil {
		s.Pool().Put(conn)
		t.Fatalf("update source_format: %v", err)
	}
	stmt.Finalize()
	s.Pool().Put(conn)

	// 2. Build MemFS with OpenCode directory structure including part files.
	fs := testutil.NewMemFS()
	root := "/oc-parts"

	// Session JSON.
	sesJSON := fmt.Sprintf(`{"id":%q,"version":"0.1.0","projectID":"projX","directory":"/home/test/projX","time":{"created":1700000000000,"updated":1700000060000}}`, sessionID)
	if err := fs.WriteFile(sesPath, []byte(sesJSON), 0644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	// User message — plain string content (no parts).
	userContent := "Explain recursion to me."
	msgPath1 := fmt.Sprintf("%s/storage/message/%s/msg_001.json", root, sessionID)
	msgJSON1 := fmt.Sprintf(`{"id":"msg_001","sessionID":%q,"role":"user","time":{"created":1700100001000,"completed":1700100002000},"content":%q}`,
		sessionID, userContent)
	if err := fs.WriteFile(msgPath1, []byte(msgJSON1), 0644); err != nil {
		t.Fatalf("write msg 1: %v", err)
	}

	// Assistant message — content is an array of parts (triggers full-depth).
	part1Text := "Recursion is when a function calls itself."
	part2Text := "The base case prevents infinite recursion."
	msgPath2 := fmt.Sprintf("%s/storage/message/%s/msg_002.json", root, sessionID)
	msgJSON2 := fmt.Sprintf(
		`{"id":"msg_002","sessionID":%q,"role":"assistant","time":{"created":1700100003000,"completed":1700100004000},`+
			`"tokens":{"input":50,"output":30,"reasoning":0,"cache_read":0,"cache_write":0},`+
			`"content":[{"type":"text","text":%q},{"type":"text","text":%q}]}`,
		sessionID, part1Text, part2Text,
	)
	if err := fs.WriteFile(msgPath2, []byte(msgJSON2), 0644); err != nil {
		t.Fatalf("write msg 2: %v", err)
	}

	// 3. Seed DB entries from the OpenCode session directory (fullDepth=true).
	seededEntries := seedEntriesFromOpenCode(t, ctx, s, fs, sessionID, sesPath)
	// With inline array content (not part files), the indexer does NOT decompose
	// content blocks into depth=1 children — that only happens with actual part files.
	// We expect: user (depth=0) + assistant (depth=0) = 2 entries.
	// TurnCount = depth-0 user+assistant = 2.
	if len(seededEntries) != 2 {
		t.Fatalf("seeded entry count: got %d, want 2 (user, asst with inline array content)", len(seededEntries))
	}

	// 4. Export.
	exported, err := export.ExportSession(ctx, s, fs, sessionID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}

	// 5. Verify total Turns count equals total DB entries.
	if len(exported.Turns) != len(seededEntries) {
		t.Fatalf("Turns length: got %d, want %d (all DB entries)", len(exported.Turns), len(seededEntries))
	}

	// 6. Verify TurnCount = conversational (depth-0 user+assistant) turns.
	wantTurnCount := countConversationalTurns(seededEntries)
	if exported.TurnCount != wantTurnCount {
		t.Errorf("TurnCount: got %d, want %d", exported.TurnCount, wantTurnCount)
	}

	// 7. Verify each turn's Index matches DB entry_index.
	for i, e := range seededEntries {
		if exported.Turns[i].Index != e.EntryIndex {
			t.Errorf("Turns[%d].Index: got %d, want DB entry_index %d", i, exported.Turns[i].Index, e.EntryIndex)
		}
	}

	// 8. Verify depth=0 user entry has correct content.
	if exported.Turns[0].Role != schema.RoleUser {
		t.Errorf("Turn[0].Role: got %q, want %q", exported.Turns[0].Role, schema.RoleUser)
	}
	if exported.Turns[0].Content != userContent {
		t.Errorf("Turn[0].Content: got %q, want %q", exported.Turns[0].Content, userContent)
	}

	// 9. Verify depth=0 assistant entry has correct role and tokens.
	if exported.Turns[1].Role != schema.RoleAssistant {
		t.Errorf("Turn[1].Role: got %q, want %q", exported.Turns[1].Role, schema.RoleAssistant)
	}
	if exported.Turns[1].TokensIn == nil || *exported.Turns[1].TokensIn != 50 {
		t.Errorf("Turn[1].TokensIn: got %v, want 50", exported.Turns[1].TokensIn)
	}
	if exported.Turns[1].TokensOut == nil || *exported.Turns[1].TokensOut != 30 {
		t.Errorf("Turn[1].TokensOut: got %v, want 30", exported.Turns[1].TokensOut)
	}

	// 10. Verify assistant entry has content overlaid from contentMap.
	// The inline array content is serialised and stored as a preview by the indexer.
	if exported.Turns[1].Content == "" {
		t.Errorf("Turn[1].Content: expected non-empty content for assistant entry with inline array")
	}
}

// ---------------------------------------------------------------------------
// OpenCode JSON with actual part files
// ---------------------------------------------------------------------------

// TestExportSession_OpenCodeJSON_WithActualPartFiles verifies ExportSession handles
// OpenCode sessions where content is delivered via separate part/{msgID}/{partID}.json
// files rather than inline content arrays. The parent message has null inline content
// so that the indexer must read part files as the sole content source.
//
// This exercises a different code path than TestExportSession_OpenCodeJSON_WithParts:
// that test uses inline JSON arrays; this test uses real part files on MemFS.
func TestExportSession_OpenCodeJSON_WithActualPartFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. Open a test store; use TestOpenCodeSesID (each test has an independent store).
	s := storetest.Open(t)
	sessionID := testutil.TestOpenCodeSesID

	storetest.SeedSession(t, s, sessionID)
	conn, err := s.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take conn: %v", err)
	}
	root := "/oc-partfiles"
	sesPath := root + "/storage/session/projPF/" + sessionID + ".json"
	stmt := conn.Prep("UPDATE sessions SET source_format = ?, source_path = ? WHERE session_id = ?")
	stmt.BindText(1, string(schema.SourceFormatJSON))
	stmt.BindText(2, sesPath)
	stmt.BindText(3, sessionID)
	if _, err := stmt.Step(); err != nil {
		s.Pool().Put(conn)
		t.Fatalf("update source_format: %v", err)
	}
	stmt.Finalize()
	s.Pool().Put(conn)

	// 2. Build MemFS with OpenCode directory structure using actual part files.
	fs := testutil.NewMemFS()

	// Session JSON.
	sesJSON := fmt.Sprintf(`{"id":%q,"version":"0.1.0","projectID":"projPF","directory":"/home/test/projPF","time":{"created":1700000000000,"updated":1700000060000}}`, sessionID)
	if err := fs.WriteFile(sesPath, []byte(sesJSON), 0644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	// User message — plain string inline content (no parts).
	userContent := "What is a closure in Go?"
	msgPath1 := fmt.Sprintf("%s/storage/message/%s/msg_pf001.json", root, sessionID)
	msgJSON1 := fmt.Sprintf(`{"id":"msg_pf001","sessionID":%q,"role":"user","time":{"created":1700200001000,"completed":1700200002000},"content":%q}`,
		sessionID, userContent)
	if err := fs.WriteFile(msgPath1, []byte(msgJSON1), 0644); err != nil {
		t.Fatalf("write msg 1: %v", err)
	}

	// Assistant message — null inline content. Parts are the sole content source.
	// tokens are still present on the parent message.
	msgPath2 := fmt.Sprintf("%s/storage/message/%s/msg_pf002.json", root, sessionID)
	msgJSON2 := fmt.Sprintf(`{"id":"msg_pf002","sessionID":%q,"role":"assistant","time":{"created":1700200003000,"completed":1700200004000},"tokens":{"input":80,"output":40,"reasoning":0,"cache_read":0,"cache_write":0},"content":null}`,
		sessionID)
	if err := fs.WriteFile(msgPath2, []byte(msgJSON2), 0644); err != nil {
		t.Fatalf("write msg 2 (null content): %v", err)
	}

	// Actual part files for msg_pf002.
	// Part layout: {root}/storage/part/{msgID}/{partID}.json
	part1Text := "A closure is a function that captures variables from its enclosing scope."
	part1Path := fmt.Sprintf("%s/storage/part/msg_pf002/part_pf001.json", root)
	part1JSON := fmt.Sprintf(`{"id":"part_pf001","type":"text","text":%q}`, part1Text)
	if err := fs.WriteFile(part1Path, []byte(part1JSON), 0644); err != nil {
		t.Fatalf("write part 1: %v", err)
	}

	part2Text := "Closures are commonly used for callbacks and deferred execution."
	part2Path := fmt.Sprintf("%s/storage/part/msg_pf002/part_pf002.json", root)
	part2JSON := fmt.Sprintf(`{"id":"part_pf002","type":"text","text":%q}`, part2Text)
	if err := fs.WriteFile(part2Path, []byte(part2JSON), 0644); err != nil {
		t.Fatalf("write part 2: %v", err)
	}

	// 3. Seed DB entries from the OpenCode session directory (fullDepth=true).
	// Expected: user (depth=0) + assistant parent (depth=0) + 2 text-part children (depth=1) = 4 entries.
	seededEntries := seedEntriesFromOpenCode(t, ctx, s, fs, sessionID, sesPath)
	if len(seededEntries) != 4 {
		t.Fatalf("seeded entry count: got %d, want 4 (user, asst-parent, 2 part children)", len(seededEntries))
	}

	// 4. Export.
	exported, err := export.ExportSession(ctx, s, fs, sessionID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}

	// 5. Export applies EntriesToTurns: the assistant parent's content (from
	// extractPreviewFromParts) matches the first text child, so consecutive dedup
	// collapses them. Result: user(1) + assistant parent(1) + second text child(1) = 3.
	if len(exported.Turns) != 3 {
		t.Fatalf("Turns length: got %d, want 3 (EntriesToTurns folds/dedupes)", len(exported.Turns))
	}
	if exported.TurnCount != 3 {
		t.Errorf("TurnCount: got %d, want 3", exported.TurnCount)
	}

	// 6. Verify user entry.
	if exported.Turns[0].Role != schema.RoleUser {
		t.Errorf("Turn[0].Role: got %q, want %q", exported.Turns[0].Role, schema.RoleUser)
	}
	if exported.Turns[0].Content != userContent {
		t.Errorf("Turn[0].Content: got %q, want %q", exported.Turns[0].Content, userContent)
	}

	// 7. Verify assistant parent has tokens.
	if exported.Turns[1].Role != schema.RoleAssistant {
		t.Errorf("Turn[1].Role: got %q, want %q", exported.Turns[1].Role, schema.RoleAssistant)
	}
	if exported.Turns[1].TokensIn == nil || *exported.Turns[1].TokensIn != 80 {
		t.Errorf("Turn[1].TokensIn: got %v, want 80", exported.Turns[1].TokensIn)
	}
	if exported.Turns[1].TokensOut == nil || *exported.Turns[1].TokensOut != 40 {
		t.Errorf("Turn[1].TokensOut: got %v, want 40", exported.Turns[1].TokensOut)
	}

	// 10. Verify the depth=1 part children have content extracted from part files.
	// Turn 2 and Turn 3 are the text part children; each should carry the part text.
	if exported.Turns[2].Content == "" {
		t.Errorf("Turn[2].Content: expected non-empty content from part file part_pf001")
	}
	// Turn 2: second text child (not deduped — different content from parent).
	if exported.Turns[2].Role != schema.RoleAssistant {
		t.Errorf("Turn[2].Role: got %q, want %q", exported.Turns[2].Role, schema.RoleAssistant)
	}
	if exported.Turns[2].Content == "" {
		t.Errorf("Turn[2].Content: expected non-empty content from second part file")
	}
}

// ---------------------------------------------------------------------------
// contentMap miss fallback
// ---------------------------------------------------------------------------

// TestExportSession_ContentMapMiss verifies the fallback path in ExportSession
// where the source re-index produces fewer entries than are stored in the DB.
// When contentMap has no entry for a given DB entry_index, ExportSession must
// fall back to ContentPreview from the DB row (not error or return empty content).
//
// Setup: seed two DB entries for session, then overwrite the source transcript
// with content that produces only one entry on re-index. The second DB entry
// has no contentMap hit, so the turn must fall back to the DB ContentPreview.
func TestExportSession_ContentMapMiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := storetest.Open(t)
	sessionID := testutil.TestSessionUUID2
	storetest.SeedSession(t, s, sessionID) // seeds source_path="/f", source_format="jsonl"

	// 1. Build a two-turn transcript and seed it into the DB.
	fs := testutil.NewMemFS()
	line1 := `{"type":"user","message":{"role":"user","content":"First question."}}`
	line2 := `{"type":"user","message":{"role":"user","content":"Second question."}}`
	fullTranscript := line1 + "\n" + line2 + "\n"
	if err := fs.WriteFile("/f", []byte(fullTranscript), 0644); err != nil {
		t.Fatalf("write full transcript: %v", err)
	}
	seededEntries := seedEntriesFromJSONL(t, ctx, s, fs, sessionID, []byte(fullTranscript))
	// Expect 2 DB entries after seeding.
	if len(seededEntries) != 2 {
		t.Fatalf("seeded entry count: got %d, want 2", len(seededEntries))
	}
	// DB entry 1 should have ContentPreview set (used as fallback below).
	if seededEntries[1].ContentPreview == nil {
		t.Fatal("seededEntries[1].ContentPreview: expected non-nil for fallback test")
	}
	fallbackPreview := *seededEntries[1].ContentPreview

	// 2. Overwrite the source file with a shorter transcript (only one entry).
	// The re-index in ExportSession will produce only one entry in contentMap.
	// DB entry at index 1 will miss contentMap → fallback to ContentPreview.
	shortTranscript := line1 + "\n"
	if err := fs.WriteFile("/f", []byte(shortTranscript), 0644); err != nil {
		t.Fatalf("write short transcript: %v", err)
	}

	// 3. Export. The DB still has 2 entries; the source only has 1 now.
	exported, err := export.ExportSession(ctx, s, fs, sessionID)
	if err != nil {
		t.Fatalf("ExportSession (contentMap miss): %v", err)
	}

	// 4. All DB entries must appear in the output (DB is source of truth for count).
	if len(exported.Turns) != len(seededEntries) {
		t.Fatalf("Turns length: got %d, want %d (all DB entries)", len(exported.Turns), len(seededEntries))
	}

	// 5. Turn 0 should have full content from contentMap (source re-index hit).
	if exported.Turns[0].Content == "" {
		t.Errorf("Turn[0].Content: expected non-empty (contentMap hit)")
	}

	// 6. Turn 1 should fall back to ContentPreview (contentMap miss for this index).
	// The content must not be empty — it must equal the DB ContentPreview value.
	if exported.Turns[1].Content == "" {
		t.Errorf("Turn[1].Content: expected non-empty (contentMap miss → ContentPreview fallback)")
	}
	if exported.Turns[1].Content != fallbackPreview {
		t.Errorf("Turn[1].Content: got %q, want ContentPreview fallback %q",
			exported.Turns[1].Content, fallbackPreview)
	}
}

// ---------------------------------------------------------------------------
// Codex JSONL integration test for a main-turn truncation regression.
// ---------------------------------------------------------------------------

// TestExportSession_CodexJSONL_LongContent guards against the harness-dispatch
// bug this fix corrects: Codex and Claude Code both write source_format
// "jsonl", so a content-overlay dispatch keyed on FORMAT alone (the previous
// export.go behaviour) picks the Claude indexer for a Codex session — which
// doesn't recognise Codex's "response_item" envelope at all, so the re-index
// silently produces zero matching entries and every turn falls back to the
// DB's truncated content_preview. Dispatch must be keyed on the session's
// actual HARNESS (transcript.BuildContentOverlay) so this recovers full
// content for Codex the same way it already does for Claude.
func TestExportSession_CodexJSONL_LongContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := storetest.Open(t)
	sessionID := testutil.TestCodexSessionID
	storetest.SeedSession(t, s, sessionID) // seeds source_path="/f", source_format="jsonl", harness=claude-code by default

	// Override model_harness to codex — SeedSession only wires claude-code.
	conn, err := s.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take conn: %v", err)
	}
	stmt := conn.Prep("UPDATE sessions SET model_harness = ? WHERE session_id = ?")
	stmt.BindText(1, string(ingest.HarnessCodex))
	stmt.BindText(2, sessionID)
	if _, err := stmt.Step(); err != nil {
		s.Pool().Put(conn)
		t.Fatalf("update model_harness: %v", err)
	}
	stmt.Finalize()
	s.Pool().Put(conn)

	// Build a Codex rollout JSONL with one user message well past
	// defaults.ContentPreviewLimit (2000 chars).
	fs := testutil.NewMemFS()
	longContent := strings.Repeat("Q", 2500)
	line := fmt.Sprintf(
		`{"timestamp":"2026-07-22T00:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`,
		longContent,
	)
	codexTranscript := line + "\n"
	if err := fs.WriteFile("/f", []byte(codexTranscript), 0644); err != nil {
		t.Fatalf("write codex fixture: %v", err)
	}

	// Seed the DB entry using the SAME codex indexer, truncated (fullContent
	// disabled) — this is what a real ingest run stores in content_preview,
	// and is the length ExportSession must recover PAST via re-index.
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID(sessionID),
		SourcePath:   ingest.ResolvedPath("/f"),
		SourceFormat: ingest.SourceFormatJSONL,
		Harness:      ingest.HarnessCodex,
	}
	codexIndexer := ingest.NewCodexIndexer(fs)
	seededEntries, err := codexIndexer.IndexTranscriptBytes(ctx, session, []byte(codexTranscript))
	if err != nil {
		t.Fatalf("seed codex entries: %v", err)
	}
	if len(seededEntries) != 1 {
		t.Fatalf("seeded entry count: got %d, want 1", len(seededEntries))
	}
	if seededEntries[0].ContentPreview == nil || len(*seededEntries[0].ContentPreview) != 2000 {
		t.Fatalf("seeded preview: expected exactly 2000 chars (truncated), got %v", seededEntries[0].ContentPreview)
	}
	if err := s.IndexSessionEntries(ctx, schema.SessionID(sessionID), seededEntries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Export must recover the FULL 2500-char content via the codex-specific
	// full-content re-index, not the truncated 2000-char DB preview.
	exported, err := export.ExportSession(ctx, s, fs, sessionID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if len(exported.Turns) != 1 {
		t.Fatalf("Turns length: got %d, want 1", len(exported.Turns))
	}
	if len(exported.Turns[0].Content) != len(longContent) {
		t.Fatalf("Turns[0].Content length: got %d, want %d (full content) — a length of exactly 2000 means the content-overlay dispatch fell back to Claude's indexer or the DB preview instead of Codex's",
			len(exported.Turns[0].Content), len(longContent))
	}
	if exported.Turns[0].Content != longContent {
		t.Errorf("Turns[0].Content: mismatch beyond length")
	}
}

// ---------------------------------------------------------------------------
// Error cases
// ---------------------------------------------------------------------------

// TestExportSession_NotFound verifies that ExportSession returns ErrSessionNotFound
// when the session ID does not exist in the store.
func TestExportSession_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)
	fs := testutil.NewMemFS()

	_, err := export.ExportSession(ctx, s, fs, "nonexistent-session-id-that-will-never-be-found")
	if err == nil {
		t.Fatal("ExportSession: expected error for non-existent session, got nil")
	}
	if !errors.Is(err, export.ErrSessionNotFound) {
		t.Errorf("ExportSession: expected ErrSessionNotFound, got: %v", err)
	}
}

// TestExportSession_MissingSourceFile verifies that ExportSession returns an
// actionable error when the source transcript file does not exist on the
// filesystem AND content recovery is actually needed (a DB entry hit the
// preview limit). transcript.AnyContentTruncated gates the re-index — with
// no entries indexed at all there is nothing to recover, so a missing file
// is a non-issue; this test must seed a truncated entry to exercise the
// gated-in path (see TestExportSession_MissingSourceFile_NothingTruncated
// for the gated-OUT case).
func TestExportSession_MissingSourceFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := storetest.Open(t)
	sessionID := testutil.TestSessionUUID2
	storetest.SeedSession(t, s, sessionID) // seeds source_path="/f"

	// Seed one entry at exactly the preview limit, so AnyContentTruncated
	// gates the overlay IN and ExportSession actually needs to read "/f".
	truncatedPreview := strings.Repeat("Z", 2000)
	dbEntries := []schema.SessionEntry{
		{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     0,
			Harness:        ingest.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &truncatedPreview,
		},
	}
	if err := s.IndexSessionEntries(ctx, schema.SessionID(sessionID), dbEntries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Do NOT write "/f" to MemFS — the file is missing.
	fs := testutil.NewMemFS()

	_, err := export.ExportSession(ctx, s, fs, sessionID)
	if err == nil {
		t.Fatal("ExportSession: expected error for missing source file, got nil")
	}
	// The error should mention the source path for actionability.
	if !strings.Contains(err.Error(), "/f") {
		t.Errorf("ExportSession error should mention source path '/f', got: %v", err)
	}
	// It should NOT be ErrSessionNotFound (the session exists, the file does not).
	if errors.Is(err, export.ErrSessionNotFound) {
		t.Error("ExportSession: got ErrSessionNotFound, but session exists — error should be about missing file")
	}
}

// ---------------------------------------------------------------------------
// Perf gate: skip the full source re-index when nothing was truncated
// (gate the full re-index on actual truncation)
// ---------------------------------------------------------------------------

// TestExportSession_MissingSourceFile_NothingTruncated is the mirror of
// TestExportSession_MissingSourceFile: the source file is ALSO missing here,
// but every DB entry's content is well under defaults.ContentPreviewLimit —
// nothing needs recovering. ExportSession must NOT attempt to read the
// (missing) source file at all in this case; it must succeed, returning the
// DB's content_preview verbatim. This is the observable proof of the
// transcript.AnyContentTruncated gate: without it, this test would fail with
// the same "file not found" error as TestExportSession_MissingSourceFile,
// even though nothing here actually needs the source file.
func TestExportSession_MissingSourceFile_NothingTruncated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := storetest.Open(t)
	sessionID := testutil.TestSessionUUID2
	storetest.SeedSession(t, s, sessionID) // seeds source_path="/f"

	shortContent := "a perfectly ordinary, short turn"
	dbEntries := []schema.SessionEntry{
		{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     0,
			Harness:        ingest.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &shortContent,
		},
	}
	if err := s.IndexSessionEntries(ctx, schema.SessionID(sessionID), dbEntries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Do NOT write "/f" to MemFS — if ExportSession tried to read it despite
	// nothing being truncated, this would fail exactly like
	// TestExportSession_MissingSourceFile does.
	fs := testutil.NewMemFS()

	exported, err := export.ExportSession(ctx, s, fs, sessionID)
	if err != nil {
		t.Fatalf("ExportSession: expected no error when nothing is truncated (gate should skip the missing source file entirely), got: %v", err)
	}
	if len(exported.Turns) != 1 || exported.Turns[0].Content != shortContent {
		t.Errorf("Turns[0].Content: got %+v, want the DB preview %q verbatim", exported.Turns, shortContent)
	}
}
