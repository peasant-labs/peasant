package ingest

import (
	"encoding/json"
	"testing"
)

// sampleUnifiedMetadata returns a fully-populated UnifiedMetadata for round-trip tests.
func sampleUnifiedMetadata(t *testing.T) UnifiedMetadata {
	t.Helper()
	parentID := SessionID("99d59925-36bc-424c-a789-8be54d9702ba")
	branch := "unified-schema"
	remote := "git@github.com:acme-dev/peasant.git"
	ingested := int64(1708310000000)
	partial := false
	return UnifiedMetadata{
		SchemaVersion: CurrentSchemaVersion,
		SessionID:     SessionID("agent-a3aee4f"),
		ParentUUID:    &parentID,
		ModelHarness:  HarnessClaudeCode,
		Model:         ModelID("claude-opus-4-6"),
		Version:       "2.1.47",
		Timestamp: TimestampInfo{
			Start:    1708300800000,
			End:      1708304400000,
			Ingested: &ingested,
		},
		Source: SourceInfo{
			FilePath: "/home/user/.claude/projects/.../99d59925.jsonl",
			Format:   SourceFormatJSONL,
		},
		Git: GitContext{
			Branch:   &branch,
			Remote:   &remote,
			Worktree: nil,
		},
		Project: ProjectInfo{
			Hash:     ProjectHash("a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"),
			FilePath: "/home/user/dev/peasant",
			Name:     "peasant",
		},
		Stats: StatsInfo{
			TurnCount:     42,
			ToolCallCount: 18,
			SubagentCount: 3,
			DurationMs:    360000,
		},
		Subagents: []SubagentRef{
			{
				SessionID:  SessionID("agent-a3aee4f"),
				ParentUUID: SessionID("99d59925-36bc-424c-a789-8be54d9702ba"),
			},
		},
		Diagnostics: DiagnosticsInfo{
			Warnings: []DiagnosticEntry{
				{
					ErrorType:   "parse_error",
					Location:    "line 47",
					Message:     "unexpected token",
					Remediation: "check source file encoding",
				},
			},
			Partial: &partial,
		},
	}
}

func TestUnifiedMetadata_JSONRoundTrip(t *testing.T) {
	original := sampleUnifiedMetadata(t)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var decoded UnifiedMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	// Compare field by field for clear failure messages.
	if decoded.SchemaVersion != original.SchemaVersion {
		t.Errorf("SchemaVersion: got %d, want %d", decoded.SchemaVersion, original.SchemaVersion)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID: got %q, want %q", decoded.SessionID, original.SessionID)
	}
	if decoded.ParentUUID == nil || *decoded.ParentUUID != *original.ParentUUID {
		t.Errorf("ParentUUID: got %v, want %v", decoded.ParentUUID, original.ParentUUID)
	}
	if decoded.ModelHarness != original.ModelHarness {
		t.Errorf("ModelHarness: got %q, want %q", decoded.ModelHarness, original.ModelHarness)
	}
	if decoded.Model != original.Model {
		t.Errorf("Model: got %q, want %q", decoded.Model, original.Model)
	}
	if decoded.Version != original.Version {
		t.Errorf("Version: got %q, want %q", decoded.Version, original.Version)
	}
	// Timestamp comparison
	if decoded.Timestamp.Start != original.Timestamp.Start ||
		decoded.Timestamp.End != original.Timestamp.End ||
		(decoded.Timestamp.Ingested == nil) != (original.Timestamp.Ingested == nil) ||
		(decoded.Timestamp.Ingested != nil && *decoded.Timestamp.Ingested != *original.Timestamp.Ingested) {
		t.Errorf("Timestamp: got %+v, want %+v", decoded.Timestamp, original.Timestamp)
	}
	if decoded.Source != original.Source {
		t.Errorf("Source: got %+v, want %+v", decoded.Source, original.Source)
	}
	if decoded.Project != original.Project {
		t.Errorf("Project: got %+v, want %+v", decoded.Project, original.Project)
	}
	if decoded.Stats != original.Stats {
		t.Errorf("Stats: got %+v, want %+v", decoded.Stats, original.Stats)
	}
	if len(decoded.Subagents) != len(original.Subagents) {
		t.Fatalf("Subagents length: got %d, want %d", len(decoded.Subagents), len(original.Subagents))
	}
	for i, ref := range decoded.Subagents {
		if ref != original.Subagents[i] {
			t.Errorf("Subagents[%d]: got %+v, want %+v", i, ref, original.Subagents[i])
		}
	}
	// Partial comparison
	if (decoded.Diagnostics.Partial == nil) != (original.Diagnostics.Partial == nil) ||
		(decoded.Diagnostics.Partial != nil && *decoded.Diagnostics.Partial != *original.Diagnostics.Partial) {
		t.Errorf("Diagnostics.Partial: got %v, want %v", decoded.Diagnostics.Partial, original.Diagnostics.Partial)
	}
	if len(decoded.Diagnostics.Warnings) != len(original.Diagnostics.Warnings) {
		t.Fatalf("Diagnostics.Warnings length: got %d, want %d",
			len(decoded.Diagnostics.Warnings), len(original.Diagnostics.Warnings))
	}
	for i, w := range decoded.Diagnostics.Warnings {
		if w != original.Diagnostics.Warnings[i] {
			t.Errorf("Diagnostics.Warnings[%d]: got %+v, want %+v", i, w, original.Diagnostics.Warnings[i])
		}
	}
}

func TestUnifiedMetadata_NullParentUUID(t *testing.T) {
	m := UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     SessionID("99d59925-36bc-424c-a789-8be54d9702ba"),
		ParentUUID:    nil,
		ModelHarness:  HarnessClaudeCode,
		Model:         ModelID("claude-opus-4-6"),
		Version:       "2.1.47",
		Subagents:     []SubagentRef{},
		Diagnostics:   DiagnosticsInfo{Warnings: []DiagnosticEntry{}},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	// Verify "parentUuid": null is present in JSON.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal to map failed: %v", err)
	}
	parentRaw, ok := raw["parentUuid"]
	if !ok {
		t.Fatal("parentUuid key missing from JSON")
	}
	if string(parentRaw) != "null" {
		t.Errorf("parentUuid: got %s, want null", parentRaw)
	}

	// Verify round-trip preserves nil.
	var decoded UnifiedMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if decoded.ParentUUID != nil {
		t.Errorf("ParentUUID: got %v, want nil", decoded.ParentUUID)
	}
}

func TestUnifiedMetadata_NonNullParentUUID(t *testing.T) {
	parentID := SessionID("99d59925-36bc-424c-a789-8be54d9702ba")
	m := UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     SessionID("agent-a3aee4f"),
		ParentUUID:    &parentID,
		ModelHarness:  HarnessClaudeCode,
		Model:         ModelID("claude-opus-4-6"),
		Version:       "2.1.47",
		Subagents:     []SubagentRef{},
		Diagnostics:   DiagnosticsInfo{Warnings: []DiagnosticEntry{}},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var decoded UnifiedMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if decoded.ParentUUID == nil {
		t.Fatal("ParentUUID: got nil, want non-nil")
	}
	if *decoded.ParentUUID != parentID {
		t.Errorf("ParentUUID: got %q, want %q", *decoded.ParentUUID, parentID)
	}
}

func TestUnifiedMetadata_EmptySubagents(t *testing.T) {
	m := UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     SessionID("99d59925-36bc-424c-a789-8be54d9702ba"),
		ModelHarness:  HarnessClaudeCode,
		Model:         ModelID("claude-opus-4-6"),
		Version:       "2.1.47",
		Subagents:     []SubagentRef{},
		Diagnostics:   DiagnosticsInfo{Warnings: []DiagnosticEntry{}},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	// Verify "subagents": [] is present (not omitted).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal to map failed: %v", err)
	}
	subagentsRaw, ok := raw["subagents"]
	if !ok {
		t.Fatal("subagents key missing from JSON")
	}
	if string(subagentsRaw) != "[]" {
		t.Errorf("subagents: got %s, want []", subagentsRaw)
	}
}

func TestUnifiedMetadata_DiagnosticsWithEntries(t *testing.T) {
	m := UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     SessionID("99d59925-36bc-424c-a789-8be54d9702ba"),
		ModelHarness:  HarnessClaudeCode,
		Model:         ModelID("claude-opus-4-6"),
		Version:       "2.1.47",
		Subagents:     []SubagentRef{},
		Diagnostics: DiagnosticsInfo{
			Warnings: []DiagnosticEntry{
				{
					ErrorType:   "parse_error",
					Location:    "line 47",
					Message:     "unexpected token in JSONL",
					Remediation: "check source file encoding and line endings",
				},
				{
					ErrorType:   "copy_failed",
					Location:    "debug/tool_output_3.json",
					Message:     "permission denied",
					Remediation: "ensure read access to debug artifacts directory",
				},
			},
			Partial: func(b bool) *bool { return &b }(true),
		},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var decoded UnifiedMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if len(decoded.Diagnostics.Warnings) != 2 {
		t.Fatalf("Warnings length: got %d, want 2", len(decoded.Diagnostics.Warnings))
	}
	if decoded.Diagnostics.Partial == nil || !*decoded.Diagnostics.Partial {
		t.Error("Diagnostics.Partial: got false, want true")
	}

	tests := []struct {
		idx         int
		errorType   string
		location    string
		message     string
		remediation string
	}{
		{0, "parse_error", "line 47", "unexpected token in JSONL", "check source file encoding and line endings"},
		{1, "copy_failed", "debug/tool_output_3.json", "permission denied", "ensure read access to debug artifacts directory"},
	}
	for _, tt := range tests {
		t.Run(tt.errorType, func(t *testing.T) {
			w := decoded.Diagnostics.Warnings[tt.idx]
			if w.ErrorType != tt.errorType {
				t.Errorf("ErrorType: got %q, want %q", w.ErrorType, tt.errorType)
			}
			if w.Location != tt.location {
				t.Errorf("Location: got %q, want %q", w.Location, tt.location)
			}
			if w.Message != tt.message {
				t.Errorf("Message: got %q, want %q", w.Message, tt.message)
			}
			if w.Remediation != tt.remediation {
				t.Errorf("Remediation: got %q, want %q", w.Remediation, tt.remediation)
			}
		})
	}
}

func TestUnifiedMetadata_EmptyDiagnostics(t *testing.T) {
	m := UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     SessionID("99d59925-36bc-424c-a789-8be54d9702ba"),
		ModelHarness:  HarnessClaudeCode,
		Model:         ModelID("claude-opus-4-6"),
		Version:       "2.1.47",
		Subagents:     []SubagentRef{},
		Diagnostics: DiagnosticsInfo{
			Warnings: []DiagnosticEntry{},
			Partial:  func(b bool) *bool { return &b }(false),
		},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	// Verify "warnings": [] is present (not omitted) and partial is false.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal to map failed: %v", err)
	}
	diagRaw, ok := raw["diagnostics"]
	if !ok {
		t.Fatal("diagnostics key missing from JSON")
	}

	var diagMap map[string]json.RawMessage
	if err := json.Unmarshal(diagRaw, &diagMap); err != nil {
		t.Fatalf("json.Unmarshal diagnostics failed: %v", err)
	}
	warningsRaw, ok := diagMap["warnings"]
	if !ok {
		t.Fatal("warnings key missing from diagnostics JSON")
	}
	if string(warningsRaw) != "[]" {
		t.Errorf("warnings: got %s, want []", warningsRaw)
	}
	partialRaw, ok := diagMap["partial"]
	if !ok {
		t.Fatal("partial key missing from diagnostics JSON")
	}
	if string(partialRaw) != "false" {
		t.Errorf("partial: got %s, want false", partialRaw)
	}
}

func TestUnifiedMetadata_SchemaVersionPresent(t *testing.T) {
	tests := []struct {
		name    string
		version int
	}{
		{"zero", 0},
		{"one", 1},
		{"large", 42},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := UnifiedMetadata{
				SchemaVersion: tt.version,
				SessionID:     SessionID("99d59925-36bc-424c-a789-8be54d9702ba"),
				ModelHarness:  HarnessClaudeCode,
				Model:         ModelID("claude-opus-4-6"),
				Version:       "2.1.47",
				Subagents:     []SubagentRef{},
				Diagnostics:   DiagnosticsInfo{Warnings: []DiagnosticEntry{}},
			}

			data, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("json.Marshal failed: %v", err)
			}

			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("json.Unmarshal to map failed: %v", err)
			}
			svRaw, ok := raw["schemaVersion"]
			if !ok {
				t.Fatal("schemaVersion key missing from JSON")
			}

			var decoded int
			if err := json.Unmarshal(svRaw, &decoded); err != nil {
				t.Fatalf("unmarshal schemaVersion: %v", err)
			}
			if decoded != tt.version {
				t.Errorf("schemaVersion: got %d, want %d", decoded, tt.version)
			}
		})
	}
}

func TestUnifiedMetadata_NullableGitFields(t *testing.T) {
	m := UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     SessionID("99d59925-36bc-424c-a789-8be54d9702ba"),
		ModelHarness:  HarnessClaudeCode,
		Model:         ModelID("claude-opus-4-6"),
		Version:       "2.1.47",
		Git: GitContext{
			Branch:   nil,
			Remote:   nil,
			Worktree: nil,
		},
		Subagents:   []SubagentRef{},
		Diagnostics: DiagnosticsInfo{Warnings: []DiagnosticEntry{}},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal to map failed: %v", err)
	}
	gitRaw, ok := raw["git"]
	if !ok {
		t.Fatal("git key missing from JSON")
	}

	var gitMap map[string]json.RawMessage
	if err := json.Unmarshal(gitRaw, &gitMap); err != nil {
		t.Fatalf("json.Unmarshal git failed: %v", err)
	}

	for _, field := range []string{"branch", "remote", "worktree"} {
		if _, ok := gitMap[field]; ok {
			t.Errorf("git.%s should be omitted from JSON when nil", field)
		}
	}

	// Round-trip: all git pointer fields should remain nil.
	var decoded UnifiedMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if decoded.Git.Branch != nil {
		t.Errorf("Git.Branch: got %v, want nil", decoded.Git.Branch)
	}
	if decoded.Git.Remote != nil {
		t.Errorf("Git.Remote: got %v, want nil", decoded.Git.Remote)
	}
	if decoded.Git.Worktree != nil {
		t.Errorf("Git.Worktree: got %v, want nil", decoded.Git.Worktree)
	}
}

func TestNewUnifiedMetadata(t *testing.T) {
	m := NewUnifiedMetadata()
	if m.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion: got %d, want %d", m.SchemaVersion, CurrentSchemaVersion)
	}
	if m.Subagents == nil {
		t.Error("Subagents is nil, want empty slice")
	}
	if len(m.Subagents) != 0 {
		t.Errorf("Subagents length: got %d, want 0", len(m.Subagents))
	}
	if m.Diagnostics.Warnings == nil {
		t.Error("Diagnostics.Warnings is nil, want empty slice")
	}

	// JSON round-trip should produce [] not null
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if string(raw["subagents"]) != "[]" {
		t.Errorf("subagents JSON: got %s, want []", raw["subagents"])
	}
}

func TestSourceFormat_IsValid(t *testing.T) {
	tests := []struct {
		format SourceFormat
		valid  bool
	}{
		{SourceFormatJSONL, true},
		{SourceFormatJSON, true},
		{"yaml", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			if got := tt.format.IsValid(); got != tt.valid {
				t.Errorf("SourceFormat(%q).IsValid() = %v, want %v", tt.format, got, tt.valid)
			}
		})
	}
}
