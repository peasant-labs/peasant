package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestReclassifyFixtures is a table-driven integration test that parses real
// Claude Code JSONL lines (extracted from actual sessions) and asserts the
// indexer classifies each entry with the correct role and entry type.
//
// Fixtures live in testdata/reclassify_fixtures.jsonl. Each line has a
// "_fixture" field naming the test case.
func TestReclassifyFixtures(t *testing.T) {
	t.Parallel()

	type expected struct {
		Role      schema.Role
		EntryType schema.EntryType
		// SkillName is non-empty if the entry should produce command metadata in Extra.
		SkillName string
		// CheckDepth1System is true if we should verify a depth=1 child is reclassified to system.
		CheckDepth1System string // content substring to match
	}

	expectations := map[string]expected{
		// Builtin slash commands → system
		"builtin_compact": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},
		"builtin_exit":    {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},

		// System-reminder → system
		"system_reminder": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},

		// Task notifications → system
		"task_notification_clean":    {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},
		"task_notification_trailing": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},

		// Local command output → system
		"local_cmd_stdout": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},
		"local_cmd_caveat": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},
		"local_cmd_stderr": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},

		// Teammate messages → system
		"teammate_message": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},

		// Skill body injection (content block array) → system
		"skill_body_array": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},

		// Tool loaded (content block array with tool_result sibling) → depth=0 reclassified to system
		"tool_loaded_array": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem, CheckDepth1System: ingest.ExactToolLoaded},

		// Skill invocation with args → stays user, with command metadata
		"skill_invocation_with_args": {Role: schema.RoleUser, EntryType: schema.EntryTypeText, SkillName: "/aura:epoch"},

		// Regular user text → stays user
		"regular_user_text": {Role: schema.RoleUser, EntryType: schema.EntryTypeText},

		// Non-standard JSONL types without message.role → user (when content is present)
		"type_direct":          {Role: schema.RoleUser, EntryType: schema.EntryTypeText},
		"type_queue_operation": {Role: schema.RoleUser, EntryType: schema.EntryTypeText},

		// progress with no content → reclassified to system (harness artifact)
		"type_progress": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},

		// empty_progress: type=progress with no content → system
		"empty_progress": {Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem},

		// R1/R2: tool_result wrapper (role=user, all children tool_result, none AskUserQuestion)
		// → depth-0 reclassified to role=tool (R2); depth-1 children also role=tool (R1).
		"tool_result_wrapper": {Role: schema.RoleTool, EntryType: schema.EntryTypeToolResult},
	}

	// Load fixture lines.
	fixtureData, err := os.ReadFile("testdata/reclassify_fixtures.jsonl")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}

	// Parse each fixture line into a map keyed by _fixture name.
	type fixtureMeta struct {
		Fixture string `json:"_fixture"`
	}
	fixtures := make(map[string][]byte)
	for _, line := range strings.Split(string(fixtureData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var fm fixtureMeta
		if err := json.Unmarshal([]byte(line), &fm); err != nil || fm.Fixture == "" {
			continue
		}
		fixtures[fm.Fixture] = []byte(line)
	}

	sessionID := ingest.SessionID(testutil.TestSessionUUID)

	// Test each fixture individually — avoids index mapping issues from full-depth.
	for name, exp := range expectations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw, ok := fixtures[name]
			if !ok {
				t.Fatalf("fixture %q not found in testdata/reclassify_fixtures.jsonl", name)
			}

			// Write single-line JSONL to MemFS.
			fs := testutil.NewMemFS()
			sourcePath := "/fixtures/" + name + ".jsonl"
			if err := fs.WriteFile(sourcePath, append(raw, '\n'), 0644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}

			indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
			entries, err := indexer.IndexTranscript(context.Background(), ingest.DiscoveredSession{
				SessionID:  sessionID,
				SourcePath: ingest.ResolvedPath(sourcePath),
			})
			if err != nil {
				t.Fatalf("index: %v", err)
			}

			if len(entries) == 0 {
				t.Fatal("indexer returned 0 entries")
			}

			// The first entry (depth=0) is what we test for role/type.
			depth0 := entries[0]
			if depth0.Role != exp.Role {
				preview := ""
				if depth0.ContentPreview != nil {
					p := *depth0.ContentPreview
					if len(p) > 80 {
						p = p[:80]
					}
					preview = p
				}
				t.Errorf("depth=0 role: got %q, want %q (preview: %s)", depth0.Role, exp.Role, preview)
			}
			if depth0.EntryType != exp.EntryType {
				t.Errorf("depth=0 entry_type: got %q, want %q", depth0.EntryType, exp.EntryType)
			}

			// Check command metadata in Extra.
			if exp.SkillName != "" {
				if depth0.Extra == nil {
					t.Errorf("expected Extra with command_name=%q, got nil", exp.SkillName)
				} else {
					var extra map[string]string
					if err := json.Unmarshal([]byte(*depth0.Extra), &extra); err != nil {
						t.Errorf("parse Extra JSON: %v", err)
					} else if extra["command_name"] != exp.SkillName {
						t.Errorf("command_name: got %q, want %q", extra["command_name"], exp.SkillName)
					}
				}
			}

			// Check depth=1 reclassification.
			if exp.CheckDepth1System != "" {
				found := false
				for _, e := range entries {
					if e.Depth == 1 && e.ContentPreview != nil && *e.ContentPreview == exp.CheckDepth1System {
						found = true
						if e.Role != schema.RoleSystem {
							t.Errorf("depth=1 %q role: got %q, want %q", exp.CheckDepth1System, e.Role, schema.RoleSystem)
						}
						if e.EntryType != schema.EntryTypeSystem {
							t.Errorf("depth=1 %q entry_type: got %q, want %q", exp.CheckDepth1System, e.EntryType, schema.EntryTypeSystem)
						}
						break
					}
				}
				if !found {
					t.Errorf("depth=1 entry with content %q not found", exp.CheckDepth1System)
				}
			}
		})
	}
}

// TestReclassifyFixtures_ToolResultRoles tests multi-line fixture scenarios that
// require transcript context (e.g. AskUserQuestion tool_use must appear before the
// matching tool_result for askUserCallIDs to be populated).
func TestReclassifyFixtures_ToolResultRoles(t *testing.T) {
	t.Parallel()

	// Load all fixture lines into a map keyed by _fixture name.
	fixtureData, err := os.ReadFile("testdata/reclassify_fixtures.jsonl")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	type fixtureMeta struct {
		Fixture string `json:"_fixture"`
	}
	fixtureLines := make(map[string][]byte)
	for _, line := range strings.Split(string(fixtureData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var fm fixtureMeta
		if err := json.Unmarshal([]byte(line), &fm); err != nil || fm.Fixture == "" {
			continue
		}
		fixtureLines[fm.Fixture] = []byte(line)
	}

	sessionID := ingest.SessionID(testutil.TestSessionUUID)

	// R3/AskUserQuestion: An assistant turn with AskUserQuestion tool_use followed
	// by a user turn with the tool_result. The tool_result depth-1 child should keep
	// role=user (genuine user response), not be reclassified to role=tool.
	t.Run("ask_user_tool_result", func(t *testing.T) {
		t.Parallel()

		// Two-line transcript: assistant (AskUserQuestion tool_use) + user (tool_result answer).
		askUserToolUseLine, ok := fixtureLines["mixed_wrapper_askuser_tool_use"]
		if !ok {
			t.Fatal("fixture mixed_wrapper_askuser_tool_use not found")
		}
		// Build a user response line for AskUserQuestion with tool_use_id matching the tool_use above.
		askUserToolResultLine := []byte(`{"type":"user","uuid":"test-ask-result-uuid","timestamp":1700000004000,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_askuser1","content":"Yes, please continue."}]}}`)

		fs := testutil.NewMemFS()
		// Use bytes.Join to avoid append aliasing the shared fixtureLines backing array.
		data := bytes.Join([][]byte{askUserToolUseLine, askUserToolResultLine}, []byte{'\n'})
		data = append(data, '\n')
		if err := fs.WriteFile("/fixtures/ask_user.jsonl", data, 0644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}

		indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
		entries, err := indexer.IndexTranscript(context.Background(), ingest.DiscoveredSession{
			SessionID:  sessionID,
			SourcePath: ingest.ResolvedPath("/fixtures/ask_user.jsonl"),
		})
		if err != nil {
			t.Fatalf("index: %v", err)
		}

		// Expected: assistant depth=0, assistant depth=1 tool_use (AskUserQuestion),
		// user depth=0 wrapper, user depth=1 tool_result (should be role=user).
		if len(entries) < 4 {
			t.Fatalf("expected at least 4 entries, got %d", len(entries))
		}

		// Find the depth-1 tool_result entry for the AskUserQuestion answer.
		var askUserResultEntry *schema.SessionEntry
		for i := range entries {
			e := &entries[i]
			if e.Depth == 1 && e.EntryType == schema.EntryTypeToolResult &&
				e.ToolCallID != nil && *e.ToolCallID == "toolu_askuser1" {
				askUserResultEntry = e
				break
			}
		}
		if askUserResultEntry == nil {
			t.Fatal("depth=1 tool_result for toolu_askuser1 not found")
		}
		if askUserResultEntry.Role != schema.RoleUser {
			t.Errorf("AskUserQuestion tool_result role: got %q, want %q (should stay user — genuine user response)", askUserResultEntry.Role, schema.RoleUser)
		}
	})

	// R2/mixed_wrapper: A user message wrapping one AskUserQuestion tool_result (role=user)
	// and one regular tool_result (role=tool). The depth-0 wrapper should stay role=user
	// because not all children are non-AskUser tool_results.
	t.Run("mixed_wrapper", func(t *testing.T) {
		t.Parallel()

		// Provide the assistant line first so askUserCallIDs is populated.
		askUserToolUseLine, ok := fixtureLines["mixed_wrapper_askuser_tool_use"]
		if !ok {
			t.Fatal("fixture mixed_wrapper_askuser_tool_use not found")
		}
		mixedWrapperLine, ok := fixtureLines["mixed_wrapper"]
		if !ok {
			t.Fatal("fixture mixed_wrapper not found")
		}

		fs := testutil.NewMemFS()
		// Use bytes.Join to avoid append aliasing the shared fixtureLines backing array.
		data := bytes.Join([][]byte{askUserToolUseLine, mixedWrapperLine}, []byte{'\n'})
		data = append(data, '\n')
		if err := fs.WriteFile("/fixtures/mixed_wrapper.jsonl", data, 0644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}

		indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
		entries, err := indexer.IndexTranscript(context.Background(), ingest.DiscoveredSession{
			SessionID:  sessionID,
			SourcePath: ingest.ResolvedPath("/fixtures/mixed_wrapper.jsonl"),
		})
		if err != nil {
			t.Fatalf("index: %v", err)
		}

		// Expected: assistant depth=0, assistant depth=1 tool_use (AskUserQuestion),
		// user depth=0 mixed wrapper, user depth=1 ask_user tool_result, user depth=1 regular tool_result.
		if len(entries) < 5 {
			t.Fatalf("expected at least 5 entries, got %d", len(entries))
		}

		// Find the depth-0 mixed wrapper entry (second depth=0, role=user JSONL).
		var mixedWrapper *schema.SessionEntry
		depth0Count := 0
		for i := range entries {
			if entries[i].Depth == 0 {
				depth0Count++
				if depth0Count == 2 {
					mixedWrapper = &entries[i]
					break
				}
			}
		}
		if mixedWrapper == nil {
			t.Fatal("second depth=0 entry (mixed wrapper) not found")
		}
		// R2: Mixed wrapper (has AskUserQuestion child) must stay role=user.
		if mixedWrapper.Role != schema.RoleUser {
			t.Errorf("mixed wrapper depth=0 role: got %q, want %q (mixed → stay user)", mixedWrapper.Role, schema.RoleUser)
		}

		// Find and verify depth-1 children of the mixed wrapper.
		var askUserChild, regularChild *schema.SessionEntry
		for i := range entries {
			e := &entries[i]
			if e.Depth == 1 && e.EntryType == schema.EntryTypeToolResult {
				if e.ToolCallID != nil && *e.ToolCallID == "toolu_askuser1" {
					askUserChild = e
				} else {
					regularChild = e
				}
			}
		}
		if askUserChild == nil {
			t.Fatal("depth=1 AskUserQuestion tool_result child not found")
		}
		if askUserChild.Role != schema.RoleUser {
			t.Errorf("AskUserQuestion tool_result role: got %q, want %q", askUserChild.Role, schema.RoleUser)
		}
		if regularChild == nil {
			t.Fatal("depth=1 regular tool_result child not found")
		}
		if regularChild.Role != schema.RoleTool {
			t.Errorf("regular tool_result role: got %q, want %q", regularChild.Role, schema.RoleTool)
		}
	})

	// R1: depth-1 tool_result children under tool_result_wrapper should be role=tool.
	t.Run("tool_result_wrapper_depth1_children", func(t *testing.T) {
		t.Parallel()

		raw, ok := fixtureLines["tool_result_wrapper"]
		if !ok {
			t.Fatal("fixture tool_result_wrapper not found")
		}

		fs := testutil.NewMemFS()
		if err := fs.WriteFile("/fixtures/wrapper.jsonl", append(raw, '\n'), 0644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}

		indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
		entries, err := indexer.IndexTranscript(context.Background(), ingest.DiscoveredSession{
			SessionID:  sessionID,
			SourcePath: ingest.ResolvedPath("/fixtures/wrapper.jsonl"),
		})
		if err != nil {
			t.Fatalf("index: %v", err)
		}

		// Expected: 1 depth=0 (reclassified to tool) + 2 depth=1 tool_result children.
		if len(entries) != 3 {
			t.Fatalf("expected 3 entries (1 depth=0 + 2 depth=1), got %d", len(entries))
		}

		// Depth=0 should be role=tool (R2).
		if entries[0].Role != schema.RoleTool {
			t.Errorf("depth=0 role: got %q, want %q (R2: wrapper reclassified)", entries[0].Role, schema.RoleTool)
		}
		if entries[0].EntryType != schema.EntryTypeToolResult {
			t.Errorf("depth=0 entry_type: got %q, want %q", entries[0].EntryType, schema.EntryTypeToolResult)
		}

		// Depth=0 content_preview should be nil (R6: migrated to children).
		if entries[0].ContentPreview != nil {
			t.Errorf("depth=0 ContentPreview: expected nil (migrated to children), got %q", *entries[0].ContentPreview)
		}

		// Depth=1 children should be role=tool (R1).
		for i := 1; i < 3; i++ {
			child := entries[i]
			if child.Depth != 1 {
				t.Errorf("entry[%d] Depth: expected 1, got %d", i, child.Depth)
			}
			if child.EntryType != schema.EntryTypeToolResult {
				t.Errorf("entry[%d] EntryType: expected %s, got %s", i, schema.EntryTypeToolResult, child.EntryType)
			}
			if child.Role != schema.RoleTool {
				t.Errorf("entry[%d] Role: got %q, want %q (R1: tool_result → role=tool)", i, child.Role, schema.RoleTool)
			}
		}

		// R6: Content preview should be migrated from depth-0 to one of the depth-1 children.
		// The fixture has content on both tool_results; check at least one child has preview.
		hasPreview := false
		for i := 1; i < 3; i++ {
			if entries[i].ContentPreview != nil {
				hasPreview = true
				break
			}
		}
		if !hasPreview {
			t.Error("R6: expected at least one depth=1 child to have ContentPreview (migrated from depth-0)")
		}
	})
}
