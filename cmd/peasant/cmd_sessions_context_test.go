package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// testContextSessionUUID is a stable session ID used in context tests.
const testContextSessionUUID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

// seedContextTestSession creates the DB, inserts a session row, then inserts
// the given entries for that session. numEntries controls how many synthetic
// entries are written (entry_index 0 … numEntries-1) so tests can control
// the range precisely.
//
// Entry layout:
//
//	even index (0, 2, 4, …): role=user,  depth=0
//	odd  index (1, 3, 5, …): role=assistant, depth=0
//	every 5th entry:          role=assistant, depth=1 (tool_use part)
func seedContextTestSession(t *testing.T, dir, sessionID string, numEntries int) {
	t.Helper()
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("seedContextTestSession: create data dir: %v", err)
	}
	storetest.CopyGoldenTo(t, dbPath)
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seedContextTestSession: open store: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Insert the session row first.
	seedTestSession(t, dir, sessionID)

	// Build entries.
	entries := make([]schema.SessionEntry, numEntries)
	baseMs := time.Now().UnixMilli()
	for i := range entries {
		ts := baseMs + int64(i)*1000
		content := "message content " + string(rune('A'+i%26))
		role := schema.RoleUser
		depth := 0
		entryType := schema.EntryTypeText
		var parentIdx *int
		var toolInput, toolOutput *string

		if i%2 == 1 {
			role = schema.RoleAssistant
		}
		// Make every 5th entry (5, 10, 15, …) a depth=1 tool_use part.
		if i > 0 && i%5 == 0 {
			depth = 1
			entryType = schema.EntryTypeToolUse
			parent := i - 1
			parentIdx = &parent
			inp := `{"path":"file.go"}`
			out := `OK`
			toolInput = &inp
			toolOutput = &out
		}

		entries[i] = schema.SessionEntry{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     i,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      entryType,
			Role:           role,
			TimestampMs:    &ts,
			ContentPreview: &content,
			Depth:          depth,
			ParentIndex:    parentIdx,
			ToolInput:      toolInput,
			ToolOutput:     toolOutput,
		}
	}

	if err := db.IndexSessionEntries(ctx, schema.SessionID(sessionID), entries); err != nil {
		t.Fatalf("seedContextTestSession: index entries: %v", err)
	}
}

// TestCLI_SessionsContext_Exists verifies that the context subcommand is registered.
func TestCLI_SessionsContext_Exists(t *testing.T) {
	t.Parallel()
	cmd := BuildSessionsCommand()
	sub, _, err := cmd.Find([]string{"context"})
	if err != nil {
		t.Fatalf("sessions context subcommand not found: %v", err)
	}
	if sub.Name() != "context" {
		t.Errorf("expected subcommand name 'context', got %q", sub.Name())
	}
}

// TestCLI_SessionsContext_Help verifies that --help does not error.
func TestCLI_SessionsContext_Help(t *testing.T) {
	t.Parallel()
	output, err := executeSessionsCmd(t, t.TempDir(), []string{"context", "--help"})
	if err != nil {
		t.Fatalf("sessions context --help: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "context") {
		t.Errorf("help output should mention 'context'; got: %s", output)
	}
}

// TestCLI_SessionsContext_NoSession verifies that running with no --session flag
// falls back to listing sessions and prints a hint.
func TestCLI_SessionsContext_NoSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedContextTestSession(t, dir, testContextSessionUUID, 10)

	output, err := executeSessionsCmd(t, dir, []string{"context"})
	// The command returns an error (to signal to caller to supply --session).
	if err == nil {
		t.Error("expected error when --session is missing; got nil")
	}
	// Output should contain the session listing (first 8 chars of the session ID).
	idPrefix := testContextSessionUUID[:8]
	if !strings.Contains(output, idPrefix) {
		t.Errorf("expected session list with ID prefix %q in output; got:\n%s", idPrefix, output)
	}
	// Output should contain the hint.
	if !strings.Contains(output, "--session") {
		t.Errorf("expected --session hint in output; got:\n%s", output)
	}
}

// TestCLI_SessionsContext_SessionNotFound verifies an error when the session has no entries.
func TestCLI_SessionsContext_SessionNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Seed the session row but no entries.
	seedTestSession(t, dir, testContextSessionUUID)

	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "0",
	})
	if err == nil {
		t.Error("expected error for session with no entries; got nil")
	}
	if !strings.Contains(output+err.Error(), "not found or not yet indexed") {
		t.Errorf("expected 'not found or not yet indexed' error; got output=%q err=%v", output, err)
	}
}

// TestCLI_SessionsContext_BasicWindow verifies that the correct window of entries
// is returned around the center turn.
func TestCLI_SessionsContext_BasicWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 20 entries: indices 0..19
	seedContextTestSession(t, dir, testContextSessionUUID, 20)

	// --turn 9 -C 3 → entries [6,12]
	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "9",
		"-C", "3",
	})
	if err != nil {
		t.Fatalf("sessions context: unexpected error: %v\noutput: %s", err, output)
	}

	// Center entry 9 must appear and be marked.
	if !strings.Contains(output, "[9]") {
		t.Errorf("expected center entry [9] in output; got:\n%s", output)
	}
	if !strings.Contains(output, "◀ center") {
		t.Errorf("expected ◀ center marker in output; got:\n%s", output)
	}
	// Entries 6 and 12 (boundary) should appear.
	if !strings.Contains(output, "[6]") {
		t.Errorf("expected entry [6] in output; got:\n%s", output)
	}
	if !strings.Contains(output, "[12]") {
		t.Errorf("expected entry [12] in output; got:\n%s", output)
	}
	// Entry 5 (before window) should NOT appear (it's 4 away from center at radius 3).
	if strings.Contains(output, "[5]") {
		t.Errorf("entry [5] outside window should not appear; got:\n%s", output)
	}
	// Entry 13 (after window) should NOT appear.
	if strings.Contains(output, "[13]") {
		t.Errorf("entry [13] outside window should not appear; got:\n%s", output)
	}
}

// TestCLI_SessionsContext_BoundaryClampStart verifies clamping at the beginning
// of a session (--turn 1 -C 5 → can only show from index 0, not -4).
func TestCLI_SessionsContext_BoundaryClampStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 15 entries: indices 0..14
	seedContextTestSession(t, dir, testContextSessionUUID, 15)

	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "1",
		"-C", "5",
	})
	if err != nil {
		t.Fatalf("sessions context: unexpected error: %v\noutput: %s", err, output)
	}

	// Should start from entry 0 (clamped).
	if !strings.Contains(output, "[0]") {
		t.Errorf("expected entry [0] (clamped start) in output; got:\n%s", output)
	}
	// Center entry 1 should be marked.
	if !strings.Contains(output, "[1]") || !strings.Contains(output, "◀ center") {
		t.Errorf("expected center [1] marked in output; got:\n%s", output)
	}
}

// TestCLI_SessionsContext_BoundaryClampEnd verifies clamping at the end.
func TestCLI_SessionsContext_BoundaryClampEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 10 entries: indices 0..9
	seedContextTestSession(t, dir, testContextSessionUUID, 10)

	// --turn 8 -C 5 → toIndex clamped at 9
	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "8",
		"-C", "5",
	})
	if err != nil {
		t.Fatalf("sessions context: unexpected error: %v\noutput: %s", err, output)
	}

	// Last entry 9 should appear.
	if !strings.Contains(output, "[9]") {
		t.Errorf("expected entry [9] (clamped end) in output; got:\n%s", output)
	}
	// Center entry 8 should be marked.
	if !strings.Contains(output, "[8]") || !strings.Contains(output, "◀ center") {
		t.Errorf("expected center [8] marked in output; got:\n%s", output)
	}
}

// TestCLI_SessionsContext_Depth1Indentation verifies that depth=1 entries are
// visually indented in the human-readable output.
func TestCLI_SessionsContext_Depth1Indentation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 12 entries: entry 5 is depth=1 (tool_use); window around 5 will include it.
	seedContextTestSession(t, dir, testContextSessionUUID, 12)

	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "5",
		"-C", "2",
	})
	if err != nil {
		t.Fatalf("sessions context: unexpected error: %v\noutput: %s", err, output)
	}

	// Entry 5 is depth=1 (tool_use) → should be indented (leading spaces before [5]).
	lines := strings.Split(output, "\n")
	foundEntry5Indented := false
	for _, line := range lines {
		if strings.Contains(line, "[5]") && strings.HasPrefix(line, "  ") {
			foundEntry5Indented = true
			break
		}
	}
	if !foundEntry5Indented {
		t.Errorf("expected entry [5] (depth=1) to be indented with leading spaces; got:\n%s", output)
	}

	// Tool entries should show box-drawing characters.
	if !strings.Contains(output, "┌") {
		t.Errorf("expected box-drawing ┌ for tool entry in output; got:\n%s", output)
	}
}

// TestCLI_SessionsContext_JSONOutput verifies the --json flag produces well-formed JSON.
func TestCLI_SessionsContext_JSONOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 15 entries
	seedContextTestSession(t, dir, testContextSessionUUID, 15)

	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "7",
		"-C", "3",
		"--json",
	})
	if err != nil {
		t.Fatalf("sessions context --json: unexpected error: %v\noutput: %s", err, output)
	}

	var result struct {
		SessionID     string                `json:"sessionId"`
		CenterIndex   int                   `json:"centerIndex"`
		ContextRadius int                   `json:"contextRadius"`
		MaxEntryIndex int                   `json:"maxEntryIndex"`
		Entries       []schema.SessionEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, output)
	}

	if result.SessionID != testContextSessionUUID {
		t.Errorf("expected sessionId=%q; got %q", testContextSessionUUID, result.SessionID)
	}
	if result.CenterIndex != 7 {
		t.Errorf("expected centerIndex=7; got %d", result.CenterIndex)
	}
	if result.ContextRadius != 3 {
		t.Errorf("expected contextRadius=3; got %d", result.ContextRadius)
	}
	// maxEntryIndex should be 14 (15 entries, last index 14).
	if result.MaxEntryIndex != 14 {
		t.Errorf("expected maxEntryIndex=14; got %d", result.MaxEntryIndex)
	}
	// entries should be in range [4, 10] → 7 entries.
	if len(result.Entries) != 7 {
		t.Errorf("expected 7 entries in range [4,10]; got %d", len(result.Entries))
	}
	// Verify center entry has correct index.
	centerFound := false
	for _, e := range result.Entries {
		if e.EntryIndex == 7 {
			centerFound = true
			break
		}
	}
	if !centerFound {
		t.Errorf("center entry (entryIndex=7) not found in JSON entries")
	}
}

// TestCLI_SessionsContext_DefaultRadius verifies the default -C radius is applied
// when the flag is omitted.
func TestCLI_SessionsContext_DefaultRadius(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 20 entries
	seedContextTestSession(t, dir, testContextSessionUUID, 20)

	// No -C flag: should use default radius of 3 → entries [7, 13] around turn 10.
	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "10",
	})
	if err != nil {
		t.Fatalf("sessions context (default radius): unexpected error: %v\noutput: %s", err, output)
	}

	// Entries 7 and 13 should appear (radius=3 from center 10).
	if !strings.Contains(output, "[7]") {
		t.Errorf("expected entry [7] (turn-radius) in output with default radius; got:\n%s", output)
	}
	if !strings.Contains(output, "[13]") {
		t.Errorf("expected entry [13] (turn+radius) in output with default radius; got:\n%s", output)
	}
	// Entry 6 (outside window) should NOT appear.
	if strings.Contains(output, "[6]") {
		t.Errorf("entry [6] is outside default radius window; should not appear; got:\n%s", output)
	}
}

// --- D3 tests: --format-tool-calls ---

// seedContextToolSession seeds a session with exactly one depth=1 tool_use entry
// (at entry_index 5) so that D3 format tests can assert on tool rendering.
func seedContextToolSession(t *testing.T, dir, sessionID string) {
	t.Helper()
	// 11 entries; seedContextTestSession places a tool_use at every 5th index (5, 10).
	seedContextTestSession(t, dir, sessionID, 11)
}

// goldenContextSessionUUID is a distinct session ID for the embedded-golden test
// so its hand-crafted fixture cannot collide with the algorithmic seeder.
const goldenContextSessionUUID = "cccccccc-cccc-cccc-cccc-cccccccccccc"

// goldenPreviewText is the depth-0 content_preview for entry 0 of the golden
// fixture. It is 144 runes long — deliberately LONGER than
// SessionContextPreviewMaxChars (117) so the golden literal pins the
// 117-rune truncation + "..." behavior of wrapPreview.
const goldenPreviewText = "User request: " +
	"ABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJ"

// goldenToolInput is the tool_use input JSON for entry 1. It is 104 runes long —
// LONGER than 80 — so the golden literal pins truncateJSON(.,80) truncation
// (77 runes + "...") inside renderToolBox.
const goldenToolInput = `{"command":"` +
	"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" + `"}`

// seedContextGoldenSession indexes a deterministic 3-entry fixture (entry_index
// 0, 1, 2) with FIXED timestamps so the embedded-golden assertion is stable:
//
//	[0] user      depth=0  content_preview = goldenPreviewText (144 runes)
//	[1] assistant depth=1  tool_use, parent=0, tool_input=goldenToolInput, preview="Running the command now."
//	[2] assistant depth=0  content_preview = "Done."
//
// Timestamps are 1700000000000 + index*1000 ms (22:13:20, 22:13:21, 22:13:22 UTC).
func seedContextGoldenSession(t *testing.T, dir, sessionID string) {
	t.Helper()
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("seedContextGoldenSession: create data dir: %v", err)
	}
	storetest.CopyGoldenTo(t, dbPath)
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seedContextGoldenSession: open store: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Insert the session row first.
	seedTestSession(t, dir, sessionID)

	const baseMs int64 = 1700000000000
	ts0 := baseMs
	ts1 := baseMs + 1000
	ts2 := baseMs + 2000
	preview0 := goldenPreviewText
	toolPreview := "Running the command now."
	toolInput := goldenToolInput
	toolNames := "Bash"
	preview2 := "Done."
	parent0 := 0

	entries := []schema.SessionEntry{
		{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			TimestampMs:    &ts0,
			ContentPreview: &preview0,
			Depth:          0,
		},
		{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     1,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      schema.EntryTypeToolUse,
			Role:           schema.RoleAssistant,
			TimestampMs:    &ts1,
			ContentPreview: &toolPreview,
			ToolNamesCSV:   &toolNames,
			ToolInput:      &toolInput,
			Depth:          1,
			ParentIndex:    &parent0,
		},
		{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     2,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleAssistant,
			TimestampMs:    &ts2,
			ContentPreview: &preview2,
			Depth:          0,
		},
	}

	if err := db.IndexSessionEntries(ctx, schema.SessionID(sessionID), entries); err != nil {
		t.Fatalf("seedContextGoldenSession: index entries: %v", err)
	}
}

// TestCLI_SessionsContext_FormatToolCalls_VerboseGolden pins the FULL human-readable
// box output of --format-tool-calls=verbose against an embedded literal. Unlike a
// verbose-vs-default self-compare (tautological), this fails if ANY of:
//   - wrapPreview's 117-rune cap (entry 0 preview is 144 runes → truncated +"..."),
//   - renderToolBox's ┌│└ box format,
//   - truncateJSON(.,80) tool-input truncation (input is 104 runes → 77 +"...")
//
// changes. This is the D3 golden regression test.
func TestCLI_SessionsContext_FormatToolCalls_VerboseGolden(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedContextGoldenSession(t, dir, goldenContextSessionUUID)

	// Window: turn=1 (center on the tool_use), -C=1 → entries [0, 2].
	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", goldenContextSessionUUID,
		"--turn", "1",
		"-C", "1",
		"--format-tool-calls", string(defaults.ToolCallFormatVerbose),
	})
	if err != nil {
		t.Fatalf("sessions context --format-tool-calls=verbose: unexpected error: %v\noutput: %s", err, output)
	}

	const wantGolden = "[0] user  22:13:20  depth=0\n" +
		"  User request: ABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABCDEFGHIJABC...\n" +
		"\n" +
		"  [1] tool_use  22:13:21  depth=1  parent=0  ◀ center\n" +
		"    ┌ Tool: Bash({\"command\":\"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx...)\n" +
		"    │ Running the command now.\n" +
		"    └\n" +
		"\n" +
		"[2] assistant  22:13:22  depth=0\n" +
		"  Done.\n" +
		"\n"

	if output != wantGolden {
		t.Errorf("verbose golden mismatch.\n--- got ---\n%q\n--- want ---\n%q", output, wantGolden)
	}
}

// TestCLI_SessionsContext_FormatToolCalls_Compact verifies compact format:
// tool entries render as a single line (no box-drawing), non-tool entries unaffected.
func TestCLI_SessionsContext_FormatToolCalls_Compact(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedContextToolSession(t, dir, testContextSessionUUID)

	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "5",
		"-C", "2",
		"--format-tool-calls", string(defaults.ToolCallFormatCompact),
	})
	if err != nil {
		t.Fatalf("sessions context --format-tool-calls=compact: unexpected error: %v\noutput: %s", err, output)
	}

	// compact: tool entry present but NO box-drawing characters.
	if strings.Contains(output, "┌") {
		t.Errorf("compact output should NOT contain box-drawing ┌; got:\n%s", output)
	}
	if strings.Contains(output, "└") {
		t.Errorf("compact output should NOT contain box-drawing └; got:\n%s", output)
	}
	// compact: a one-line "tool: ..." summary should be present.
	if !strings.Contains(output, "tool:") {
		t.Errorf("compact output should contain 'tool:' summary; got:\n%s", output)
	}
	// Entry [5] header (depth=1) should still appear.
	if !strings.Contains(output, "[5]") {
		t.Errorf("compact output should contain entry [5] header; got:\n%s", output)
	}
}

// TestCLI_SessionsContext_FormatToolCalls_Quiet verifies quiet format:
// tool entry headers still appear, but the tool box body is hidden.
func TestCLI_SessionsContext_FormatToolCalls_Quiet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedContextToolSession(t, dir, testContextSessionUUID)

	output, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "5",
		"-C", "2",
		"--format-tool-calls", string(defaults.ToolCallFormatQuiet),
	})
	if err != nil {
		t.Fatalf("sessions context --format-tool-calls=quiet: unexpected error: %v\noutput: %s", err, output)
	}

	// quiet: NO box-drawing characters, NO "tool:" summary lines.
	if strings.Contains(output, "┌") {
		t.Errorf("quiet output should NOT contain box-drawing ┌; got:\n%s", output)
	}
	if strings.Contains(output, "tool:") {
		t.Errorf("quiet output should NOT contain 'tool:' lines; got:\n%s", output)
	}
	// The depth=1 entry header line for [5] should still appear (header is not suppressed).
	if !strings.Contains(output, "[5]") {
		t.Errorf("quiet output should still contain depth=1 header [5]; got:\n%s", output)
	}
}

// TestCLI_SessionsContext_FormatToolCalls_Invalid verifies that an unknown
// --format-tool-calls value returns the exact error `must be one of {verbose, compact, quiet}`.
func TestCLI_SessionsContext_FormatToolCalls_Invalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedContextToolSession(t, dir, testContextSessionUUID)

	_, err := executeSessionsCmd(t, dir, []string{"context",
		"--session", testContextSessionUUID,
		"--turn", "5",
		"--format-tool-calls", "bogus",
	})
	if err == nil {
		t.Fatal("expected error for invalid --format-tool-calls value; got nil")
	}
	// Ratified substring (must use braces, not brackets).
	const wantMsg = "must be one of {verbose, compact, quiet}"
	if !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("expected error to contain %q; got: %v", wantMsg, err)
	}
	// Parity with the --harness error: echo the flag name and the bad value.
	if !strings.Contains(err.Error(), "--format-tool-calls") {
		t.Errorf("expected error to name the --format-tool-calls flag; got: %v", err)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("expected error to echo the invalid value %q; got: %v", "bogus", err)
	}
}

// --- D1 tests: rune-safe truncation ---

// TestCLI_SessionsContext_WrapPreview_ASCIIUnderCap verifies that ASCII input
// longer than the LIST cap (40) but within the DETAIL cap (117) is returned
// UNCHANGED — no truncation, no "..." ellipsis. This pins the regression fix:
// the detail view must use SessionContextPreviewMaxChars (117), not the
// 40-rune SessionPreviewMaxChars list cap.
func TestCLI_SessionsContext_WrapPreview_ASCIIUnderCap(t *testing.T) {
	t.Parallel()
	// 80 ASCII chars: >40 (list cap) and ≤117 (detail cap).
	input := strings.Repeat("a", 80)
	result := wrapPreview(input)

	if result != input {
		t.Errorf("wrapPreview should return 80-char input unchanged (≤%d cap); got: %q",
			defaults.SessionContextPreviewMaxChars, result)
	}
	if strings.HasSuffix(result, "...") {
		t.Errorf("wrapPreview of under-cap input must NOT append '...'; got: %q", result)
	}
}

// TestCLI_SessionsContext_WrapPreview_ASCIIOverCap verifies that ASCII input
// longer than the DETAIL cap (117) is truncated to exactly 117 runes + "...".
func TestCLI_SessionsContext_WrapPreview_ASCIIOverCap(t *testing.T) {
	t.Parallel()
	// 200 ASCII chars: >117 (detail cap).
	input := strings.Repeat("b", 200)
	result := wrapPreview(input)

	if !strings.HasSuffix(result, "...") {
		t.Errorf("wrapPreview of over-cap input should end with '...'; got: %q", result)
	}
	base := strings.TrimSuffix(result, "...")
	if len([]rune(base)) != defaults.SessionContextPreviewMaxChars {
		t.Errorf("wrapPreview should truncate to exactly %d runes; got %d: %q",
			defaults.SessionContextPreviewMaxChars, len([]rune(base)), result)
	}
}

// TestCLI_SessionsContext_WrapPreview_RuneSafe verifies that CJK input (3 bytes
// each) longer than the 117-rune DETAIL cap is cut on a rune boundary: output is
// valid UTF-8 and at most 117 runes + "...".
func TestCLI_SessionsContext_WrapPreview_RuneSafe(t *testing.T) {
	t.Parallel()
	// 130 CJK runes: >117 cap. Each "日" is 3 bytes.
	longCJK := strings.Repeat("日", 130)
	result := wrapPreview(longCJK)

	// Result must be valid UTF-8 (no mid-rune cut).
	if !isValidUTF8(result) {
		t.Errorf("wrapPreview produced invalid UTF-8: %q", result)
	}

	// Base (sans "...") must be exactly SessionContextPreviewMaxChars runes.
	base := strings.TrimSuffix(result, "...")
	if len([]rune(base)) != defaults.SessionContextPreviewMaxChars {
		t.Errorf("wrapPreview should truncate CJK to exactly %d runes; got %d: %q",
			defaults.SessionContextPreviewMaxChars, len([]rune(base)), result)
	}

	// Result must end with "..." because the input exceeds the cap.
	if !strings.HasSuffix(result, "...") {
		t.Errorf("wrapPreview of over-cap input should end with '...'; got: %q", result)
	}
}

// TestCLI_SessionsContext_WrapPreview_RuneSafe_Emoji verifies that emoji (4-byte
// code points) longer than the 117-rune cap are also handled without mid-rune cuts.
func TestCLI_SessionsContext_WrapPreview_RuneSafe_Emoji(t *testing.T) {
	t.Parallel()
	// "🎉" is U+1F389, 4 bytes in UTF-8. Use 130 to exceed the 117-rune cap.
	longEmoji := strings.Repeat("🎉", 130)
	result := wrapPreview(longEmoji)

	if !isValidUTF8(result) {
		t.Errorf("wrapPreview produced invalid UTF-8 for emoji input: %q", result)
	}
	if !strings.HasSuffix(result, "...") {
		t.Errorf("wrapPreview of over-cap emoji input should end with '...'; got: %q", result)
	}
	base := strings.TrimSuffix(result, "...")
	if len([]rune(base)) != defaults.SessionContextPreviewMaxChars {
		t.Errorf("wrapPreview should truncate emoji to exactly %d runes; got %d",
			defaults.SessionContextPreviewMaxChars, len([]rune(base)))
	}
}

// TestCLI_SessionsContext_TruncateJSON_RuneSafe verifies that truncateJSON produces
// valid UTF-8 output for multibyte input and delegates to store.TruncateToRunes.
func TestCLI_SessionsContext_TruncateJSON_RuneSafe(t *testing.T) {
	t.Parallel()
	// Construct a JSON-like string of CJK characters that exceeds maxLen=10 runes.
	longInput := strings.Repeat("中", 20) // 20 runes, 60 bytes
	result := truncateJSON(longInput, 10)

	if !isValidUTF8(result) {
		t.Errorf("truncateJSON produced invalid UTF-8: %q", result)
	}
	if !strings.HasSuffix(result, "...") {
		t.Errorf("truncateJSON of over-limit input should end with '...'; got: %q", result)
	}
	base := strings.TrimSuffix(result, "...")
	// base must be at most maxLen-3 = 7 runes.
	if len([]rune(base)) > 7 {
		t.Errorf("truncateJSON base exceeds maxLen-3: %d runes (want ≤7)", len([]rune(base)))
	}
}

// isValidUTF8 reports whether s is a valid UTF-8 string.
func isValidUTF8(s string) bool {
	return s == string([]rune(s))
}

// --- D6 tests: single-DB-open fallback ---

// TestCLI_SessionsContext_FallbackRenderSingleOpen verifies that the no-session
// fallback path (which lists recent sessions) renders without error when the DB
// contains sessions. The DB must be opened exactly once for this path.
func TestCLI_SessionsContext_FallbackRenderSingleOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Seed a session so the listing has something to render.
	seedContextTestSession(t, dir, testContextSessionUUID, 5)

	output, err := executeSessionsCmd(t, dir, []string{"context"})
	// The command returns an error (to signal to caller to supply --session).
	if err == nil {
		t.Error("expected error when --session is missing; got nil")
	}
	// The error must be the "no session specified" hint, not a DB error.
	if !strings.Contains(err.Error(), "no session specified") {
		t.Errorf("expected 'no session specified' error; got: %v", err)
	}
	// The output must contain the session listing (proof the DB opened successfully).
	if !strings.Contains(output, testContextSessionUUID[:8]) {
		t.Errorf("fallback render: expected session ID prefix in output; got:\n%s", output)
	}
}
