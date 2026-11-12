package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/assistant_entry_type.yaml
var assistantEntryTypeYAML []byte

//go:embed testdata/assistant_entry_type.manifest.yaml
var assistantEntryTypeManifestYAML []byte

type assistantEntryToolCall struct {
	Name   string `yaml:"name"`
	Input  string `yaml:"input"`
	Output string `yaml:"output"`
}

type assistantEntryTurn struct {
	Role      string                   `yaml:"role"`
	EntryType string                   `yaml:"entry_type"`
	Content   string                   `yaml:"content"`
	Tools     []assistantEntryToolCall `yaml:"tools"`
}

type assistantEntryExpectation struct {
	Role        string `yaml:"role"`
	EntryType   string `yaml:"entry_type"`
	HasToolUse  bool   `yaml:"has_tool_use"`
	HasThinking bool   `yaml:"has_thinking"`
}

type assistantEntryTreePart struct {
	ID   string `yaml:"id"`
	Data string `yaml:"data"`
}

type assistantEntryTreeMessage struct {
	ID    string                   `yaml:"id"`
	Data  string                   `yaml:"data"`
	Parts []assistantEntryTreePart `yaml:"parts"`
}

type assistantEntryNativeRow struct {
	ID          string `yaml:"id"`
	Type        string `yaml:"type"`
	Seq         int64  `yaml:"seq"`
	TimeCreated int64  `yaml:"timeCreated"`
	TimeUpdated int64  `yaml:"timeUpdated"`
	Data        string `yaml:"data"`
}

type assistantEntryScope struct {
	SessionID        string `yaml:"sessionId"`
	Shape            string `yaml:"shape"`
	ParentNullProven bool   `yaml:"parentNullProven"`
	HasParent        bool   `yaml:"hasParent"`
}

type assistantEntryAllocator struct {
	Entries     []string `yaml:"entries"`
	Submissions []string `yaml:"submissions"`
}

type assistantEntryGenerated struct {
	UserText         int `yaml:"user_text"`
	AssistantText    int `yaml:"assistant_text"`
	ProseToolReplies int `yaml:"prose_tool_replies"`
}

type assistantEntryWant struct {
	Entries []assistantEntryExpectation `yaml:"entries"`
	Turns   []assistantEntryTurn        `yaml:"turns"`
}

type assistantEntryCase struct {
	Name         string                      `yaml:"name"`
	Paths        []string                    `yaml:"paths"`
	Harness      string                      `yaml:"harness"`
	JSONL        []string                    `yaml:"jsonl"`
	TreeMessages []assistantEntryTreeMessage `yaml:"tree_messages"`
	Scope        assistantEntryScope         `yaml:"scope"`
	NativeRows   []assistantEntryNativeRow   `yaml:"native_rows"`
	Allocator    assistantEntryAllocator     `yaml:"allocator"`
	ThreadID     string                      `yaml:"thread_id"`
	Rollout      []string                    `yaml:"rollout"`
	Generated    *assistantEntryGenerated    `yaml:"generated"`
	Want         assistantEntryWant          `yaml:"want"`
	WantManaged  *assistantEntryWant         `yaml:"want_managed"`
}

type assistantEntryDocument struct {
	RequiredNames []string             `yaml:"requiredNames"`
	Cases         []assistantEntryCase `yaml:"cases"`
}

func loadAssistantEntryTypeDocument(t *testing.T) assistantEntryDocument {
	t.Helper()
	var document assistantEntryDocument
	if err := testutil.DecodeFixtureYAML(assistantEntryTypeYAML, &document); err != nil {
		t.Fatalf("decode assistant entry type fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(assistantEntryTypeManifestYAML, "assistant entry type")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Cases))
	seen := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" {
			t.Fatal("assistant entry type fixture has a case with no name")
		}
		if seen[row.Name] {
			t.Fatalf("assistant entry type fixture has a duplicate case name %q", row.Name)
		}
		seen[row.Name] = true
		names = append(names, row.Name)
		if len(row.Paths) == 0 {
			t.Fatalf("case %q names no path; declare indexer, managed, or both", row.Name)
		}
		for _, path := range row.Paths {
			if path != "indexer" && path != "managed" {
				t.Fatalf("case %q names unknown path %q", row.Name, path)
			}
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "assistant entry type"); err != nil {
		t.Fatal(err)
	}
	return document
}

// TestAssistantEntryType is the typing-by-prose fixture: every row states, as
// literals, the folded content and tool calls each path produces. The entry
// type and flag assertions below prove the retype changed only the type.
func TestAssistantEntryType(t *testing.T) {
	t.Parallel()
	for _, row := range loadAssistantEntryTypeDocument(t).Cases {
		row := row
		for _, path := range row.Paths {
			path := path
			t.Run(row.Name+"/"+path, func(t *testing.T) {
				t.Parallel()
				entries := runAssistantEntryPath(t, row, path)
				turns := ingest.EntriesToTurns(entries)
				assertAssistantEntryLiterals(t, row, path, turns)
			})
		}
	}
}

func runAssistantEntryPath(t *testing.T, row assistantEntryCase, path string) []schema.SessionEntry {
	t.Helper()
	switch row.Harness {
	case "claude-code":
		if path == "managed" {
			return runAssistantEntryClaudeManaged(t, row)
		}
		return runAssistantEntryClaudeIndexer(t, row)
	case "cursor":
		if path != "indexer" {
			t.Fatalf("case %q: cursor has no managed path", row.Name)
		}
		return runAssistantEntryCursorIndexer(t, row)
	case "opencode":
		if path == "managed" {
			return runAssistantEntryOpenCodeManaged(t, row)
		}
		return runAssistantEntryOpenCodeIndexer(t, row)
	case "codex":
		if path != "managed" {
			t.Fatalf("case %q: codex runs the managed path only", row.Name)
		}
		return runAssistantEntryCodexManaged(t, row)
	case "pi":
		if path != "indexer" {
			t.Fatalf("case %q: pi runs the indexer path only", row.Name)
		}
		return runAssistantEntryPiIndexer(t, row)
	default:
		t.Fatalf("case %q names unknown harness %q", row.Name, row.Harness)
		return nil
	}
}

func assistantEntryJSONL(t *testing.T, row assistantEntryCase) string {
	t.Helper()
	if row.Generated != nil {
		return buildGeneratedCursorSession(row.Generated)
	}
	lines := append([]string(nil), row.JSONL...)
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("case %q jsonl line %d is not valid JSON: %s", row.Name, i, line)
		}
		lines[i] = strings.TrimSpace(line)
	}
	return strings.Join(lines, "\n") + "\n"
}

func runAssistantEntryClaudeIndexer(t *testing.T, row assistantEntryCase) []schema.SessionEntry {
	t.Helper()
	mfs := testutil.NewMemFS()
	path := "/transcripts/" + testutil.TestSessionUUID + ".jsonl"
	if err := mfs.WriteFile(path, []byte(assistantEntryJSONL(t, row)), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	idx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true), ingest.WithClaudeFullContent(true))
	entries, err := idx.IndexTranscript(context.Background(), ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessClaudeCode,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	})
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	return entries
}

func runAssistantEntryClaudeManaged(t *testing.T, row assistantEntryCase) []schema.SessionEntry {
	t.Helper()
	mfs := testutil.NewMemFS()
	sourcePath := "/native/" + testutil.TestSessionUUID + ".jsonl"
	captured := []byte(assistantEntryJSONL(t, row))
	metadata := makeReindexMeta(t, testutil.TestSessionUUID, sourcePath)
	metadata.ContentHash = schema.ComputeTranscriptHash(captured)
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	idx := ingest.NewClaudeIndexer(mfs,
		ingest.WithClaudeFullDepth(true),
		ingest.WithClaudeFullContent(true),
	)
	candidate, err := idx.BuildGeneration(context.Background(), ingest.GenerationBuildInput{
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      ingest.HarnessClaudeCode,
			SourceFormat: ingest.SourceFormatJSONL,
			SourcePath:   ingest.ResolvedPath(sourcePath),
		},
		GenerationID:       "gen-" + row.Name + "-managed",
		RecordedMetadata:   metadata,
		CapturedTranscript: captured,
	})
	if err != nil {
		t.Fatalf("BuildGeneration: %v", err)
	}
	return candidate.Result.Generation.Main.Entries
}

func runAssistantEntryCursorIndexer(t *testing.T, row assistantEntryCase) []schema.SessionEntry {
	t.Helper()
	mfs := testutil.NewMemFS()
	path := "/cursor/session.jsonl"
	if err := mfs.WriteFile(path, []byte(assistantEntryJSONL(t, row)), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	idx := ingest.NewCursorIndexer(mfs, ingest.WithCursorFullDepth(true), ingest.WithCursorFullContent(true))
	entries, err := idx.IndexTranscript(context.Background(), ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessCursor,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	})
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	return entries
}

func runAssistantEntryPiIndexer(t *testing.T, row assistantEntryCase) []schema.SessionEntry {
	t.Helper()
	raw := assistantEntryJSONL(t, row)
	// The header carries the session identity; the discovered session must
	// name it, or the recording is refused as another session's.
	var header struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(raw), "\n", 2)[0]), &header); err != nil {
		t.Fatalf("read pi header identity: %v", err)
	}
	sid, err := ingest.NewSessionID(header.ID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	mfs := testutil.NewMemFS()
	path := "/pi/session.jsonl"
	if err := mfs.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	idx := ingest.NewPiIndexer(mfs, ingest.WithPiFullContent(true))
	entries, err := idx.IndexTranscript(context.Background(), ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessPi,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	})
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	return entries
}

func runAssistantEntryOpenCodeIndexer(t *testing.T, row assistantEntryCase) []schema.SessionEntry {
	t.Helper()
	if len(row.TreeMessages) == 0 {
		t.Fatalf("case %q has no tree messages", row.Name)
	}
	root := t.TempDir()
	storage := filepath.Join(root, defaults.OpenCodeDirStorage.String())
	sessionID := "ses_assistanttyping01"
	sessionPath := filepath.Join(storage, defaults.OpenCodeDirSession.String(), "project", sessionID+defaults.ExtJSON.String())
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	sessionJSON := fmt.Sprintf(`{"id":%q,"version":"fixture-version","directory":"/synthetic","time":{"created":1000,"updated":2000}}`, sessionID)
	if err := os.WriteFile(sessionPath, []byte(sessionJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, message := range row.TreeMessages {
		if !json.Valid([]byte(message.Data)) {
			t.Fatalf("case %q message %q data is not valid JSON", row.Name, message.ID)
		}
		messageDir := filepath.Join(storage, defaults.OpenCodeDirMessage.String(), sessionID)
		if err := os.MkdirAll(messageDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(messageDir, message.ID+defaults.ExtJSON.String()), []byte(message.Data), 0o600); err != nil {
			t.Fatal(err)
		}
		partDir := filepath.Join(storage, defaults.OpenCodeDirPart.String(), message.ID)
		if err := os.MkdirAll(partDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, part := range message.Parts {
			if !json.Valid([]byte(part.Data)) {
				t.Fatalf("case %q part %q data is not valid JSON", row.Name, part.ID)
			}
			if err := os.WriteFile(filepath.Join(partDir, part.ID+defaults.ExtJSON.String()), []byte(part.Data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	idx := ingest.NewOpenCodeIndexer(
		&ingest.OSFileSystem{},
		ingest.WithOpenCodeFullDepth(true),
		ingest.WithOpenCodeFullContent(true),
	)
	entries, err := idx.IndexTranscript(context.Background(), ingest.DiscoveredSession{
		SessionID:        sid,
		Harness:          ingest.HarnessOpenCode,
		SourcePath:       ingest.ResolvedPath(sessionPath),
		SourceFormat:     ingest.SourceFormatJSON,
		OriginalRoot:     ingest.ResolvedPath(root),
		TranscriptOrigin: ingest.TranscriptOriginFile,
	})
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	return entries
}

func runAssistantEntryOpenCodeManaged(t *testing.T, row assistantEntryCase) []schema.SessionEntry {
	t.Helper()
	if row.Scope.Shape != "current" {
		t.Fatalf("case %q names unknown scope shape %q", row.Name, row.Scope.Shape)
	}
	if len(row.NativeRows) == 0 {
		t.Fatalf("case %q has no native rows", row.Name)
	}
	scope := ingest.OpenCodeProvenanceScope{
		SessionID:        row.Scope.SessionID,
		Shape:            ingest.OpenCodeProvenanceCurrent,
		ParentNullProven: row.Scope.ParentNullProven,
		HasParent:        row.Scope.HasParent,
	}
	snapshot := ingest.OpenCodeHistorySnapshot{SessionID: row.Scope.SessionID}
	for _, native := range row.NativeRows {
		if !json.Valid([]byte(native.Data)) {
			t.Fatalf("case %q row %q data is not valid JSON", row.Name, native.ID)
		}
		decoded, settled, err := ingest.DecodeOpenCodeProvenanceRow(ingest.OpenCodeProvenanceRow{
			ID:          native.ID,
			SessionID:   scope.SessionID,
			Type:        native.Type,
			TimeCreated: native.TimeCreated,
			TimeUpdated: native.TimeUpdated,
			Seq:         native.Seq,
			HasSeq:      true,
			Data:        native.Data,
		}, scope)
		if err != nil {
			t.Fatalf("row %q decode: %v", native.ID, err)
		}
		snapshot.Messages = append(snapshot.Messages, ingest.OpenCodeHistoryRow{
			MessageID:  native.ID,
			Shape:      scope.Shape,
			Seq:        native.Seq,
			HasSeq:     true,
			NativeType: native.Type,
			CreatedMs:  native.TimeCreated,
			UpdatedMs:  native.TimeUpdated,
			Settled:    settled,
			Message:    decoded,
		})
	}
	snapshot.SourceEvidenceDigest = ingest.DigestOpenCodeSnapshot(snapshot)
	capture, err := ingest.BuildOpenCodeProvenanceCapture(snapshot, "gen-"+row.Name, schema.UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     schema.SessionID(row.Scope.SessionID),
		ModelHarness:  schema.HarnessOpenCode,
	}, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()})
	if err != nil {
		t.Fatalf("BuildOpenCodeProvenanceCapture: %v", err)
	}
	built, err := ingest.BuildV2(capture, &queueAllocator{
		entry:      append([]string(nil), row.Allocator.Entries...),
		submission: append([]string(nil), row.Allocator.Submissions...),
	})
	if err != nil {
		t.Fatalf("BuildV2: %v", err)
	}
	return built.Generation.Main.Entries
}

type assistantEntryCodexSource struct {
	authority ingest.CodexSourceAuthority
	files     map[string][]byte
}

func (s assistantEntryCodexSource) ResolveCodexAuthority(context.Context, ingest.DiscoveredSession) (ingest.CodexSourceAuthority, error) {
	return s.authority, nil
}

func (s assistantEntryCodexSource) ReadCodexSource(_ context.Context, pointer string) ([]byte, error) {
	data, ok := s.files[pointer]
	if !ok {
		return nil, fmt.Errorf("fixture has no bytes for pointer %q", pointer)
	}
	return data, nil
}

func runAssistantEntryCodexManaged(t *testing.T, row assistantEntryCase) []schema.SessionEntry {
	t.Helper()
	if row.ThreadID == "" || len(row.Rollout) == 0 {
		t.Fatalf("case %q needs a thread id and rollout lines", row.Name)
	}
	lines := append([]string(nil), row.Rollout...)
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Fatalf("case %q rollout line %d is not valid JSON: %s", row.Name, i, line)
		}
	}
	pointer := "/synthetic/current.jsonl"
	source := assistantEntryCodexSource{
		authority: ingest.CodexSourceAuthority{
			StableThreadID:   row.ThreadID,
			Kind:             ingest.CodexAuthorityDetachedFile,
			CurrentPointer:   pointer,
			PhysicalSourceID: "phys-" + row.ThreadID,
		},
		files: map[string][]byte{pointer: []byte(strings.Join(lines, "\n") + "\n")},
	}
	sid, err := ingest.NewSessionID(row.ThreadID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	history, err := ingest.CaptureCodexHistory(context.Background(), source, ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(pointer),
		SourceFormat: ingest.SourceFormatJSONL,
	}, nil)
	if err != nil {
		t.Fatalf("CaptureCodexHistory: %v", err)
	}
	candidate, err := ingest.BuildCodexCandidate(ingest.CodexCandidateInput{
		History:      history,
		Base:         schema.NewUnifiedMetadata(),
		PriorState:   ingest.NewProjectionPriorState(),
		Allocator:    &codexScriptedAllocator{entries: append([]string(nil), row.Allocator.Entries...), submissions: append([]string(nil), row.Allocator.Submissions...)},
		GenerationID: "gen-" + row.Name,
	})
	if err != nil {
		t.Fatalf("BuildCodexCandidate: %v", err)
	}
	return candidate.V2.Generation.Main.Entries
}

// buildGeneratedCursorSession emits the cursor shape the proposal pins: user
// text prompts, assistant text replies, then assistant replies that hold prose
// and tool calls. The record order is fixed, so the folded turns are too.
func buildGeneratedCursorSession(generated *assistantEntryGenerated) string {
	var lines []string
	for i := 0; i < generated.UserText; i++ {
		lines = append(lines, fmt.Sprintf(`{"role":"user","timestamp":"2026-06-04T10:00:00Z","message":{"content":[{"type":"text","text":"user prompt %d"}]}}`, i))
	}
	for i := 0; i < generated.AssistantText; i++ {
		lines = append(lines, fmt.Sprintf(`{"role":"assistant","timestamp":"2026-06-04T10:01:00Z","message":{"model":"cursor-agent","content":[{"type":"text","text":"assistant reply %d"}]}}`, i))
	}
	for i := 0; i < generated.ProseToolReplies; i++ {
		lines = append(lines, fmt.Sprintf(`{"role":"assistant","timestamp":"2026-06-04T10:02:00Z","message":{"model":"cursor-agent","content":[{"type":"text","text":"progress note %d"},{"type":"tool_use","id":"tool-%d","name":"Bash","input":{"command":"step %d"}}]}}`, i, i, i))
	}
	return strings.Join(lines, "\n") + "\n"
}

// assistantEntryWantFor selects the row's expectations for one path: the
// managed build may fold the same records to more entries and turns than the
// stored indexer (a blank text block is its own entry there), so such rows
// carry a want_managed override.
func assistantEntryWantFor(t *testing.T, row assistantEntryCase, path string) assistantEntryWant {
	t.Helper()
	if path == "managed" && row.WantManaged != nil {
		return *row.WantManaged
	}
	return row.Want
}

// assertAssistantEntryLiterals proves the fold did not move: every turn's
// content and tool calls equal the literals recorded on unmodified code. Only
// the entry type moves under the typing change. An explicit empty turn list
// asserts the records fold to nothing.
func assertAssistantEntryLiterals(t *testing.T, row assistantEntryCase, path string, turns []ingest.Turn) {
	t.Helper()
	wantTurns := assistantEntryWantTurns(t, row, path)
	if len(turns) != len(wantTurns) {
		for i, turn := range turns {
			t.Logf("turn[%d] role=%s type=%s tools=%d content=%q", i, turn.Role, turn.EntryType, len(turn.ToolCalls), turn.Content)
		}
		t.Fatalf("case %q folds to %d turns, want %d", row.Name, len(turns), len(wantTurns))
	}
	for i, want := range wantTurns {
		got := turns[i]
		if string(got.Role) != want.Role {
			t.Errorf("case %q turn[%d] role = %q, want %q", row.Name, i, got.Role, want.Role)
		}
		if got.Content != want.Content {
			t.Errorf("case %q turn[%d] content = %q, want %q", row.Name, i, got.Content, want.Content)
		}
		if len(got.ToolCalls) != len(want.Tools) {
			t.Errorf("case %q turn[%d] tool calls = %d, want %d", row.Name, i, len(got.ToolCalls), len(want.Tools))
			continue
		}
		for j, wantCall := range want.Tools {
			gotCall := got.ToolCalls[j]
			if gotCall.Name != wantCall.Name {
				t.Errorf("case %q turn[%d] tool[%d] name = %q, want %q", row.Name, i, j, gotCall.Name, wantCall.Name)
			}
			if gotCall.Arguments != wantCall.Input {
				t.Errorf("case %q turn[%d] tool[%d] input = %q, want %q", row.Name, i, j, gotCall.Arguments, wantCall.Input)
			}
			if gotCall.Result != wantCall.Output {
				t.Errorf("case %q turn[%d] tool[%d] output = %q, want %q", row.Name, i, j, gotCall.Result, wantCall.Output)
			}
		}
	}
}

func assistantEntryWantTurns(t *testing.T, row assistantEntryCase, path string) []assistantEntryTurn {
	t.Helper()
	if row.Generated != nil {
		return buildGeneratedCursorWantTurns(row.Generated)
	}
	want := assistantEntryWantFor(t, row, path)
	if len(want.Entries) == 0 {
		t.Fatalf("case %q path %s records no entries; a row without stored entries passes without invoking the indexer", row.Name, path)
	}
	return want.Turns
}

func buildGeneratedCursorWantTurns(generated *assistantEntryGenerated) []assistantEntryTurn {
	var want []assistantEntryTurn
	for i := 0; i < generated.UserText; i++ {
		want = append(want, assistantEntryTurn{Role: "user", EntryType: "text", Content: fmt.Sprintf("user prompt %d", i)})
	}
	for i := 0; i < generated.AssistantText; i++ {
		want = append(want, assistantEntryTurn{Role: "assistant", EntryType: "text", Content: fmt.Sprintf("assistant reply %d", i)})
	}
	for i := 0; i < generated.ProseToolReplies; i++ {
		want = append(want, assistantEntryTurn{
			Role: "assistant", EntryType: "text", Content: fmt.Sprintf("progress note %d", i),
			Tools: []assistantEntryToolCall{{Name: "Bash", Input: fmt.Sprintf(`{"command":"step %d"}`, i)}},
		})
	}
	return want
}

// assertAssistantEntryTyping proves the retype: every depth-0 stored entry
// carries the row's entry type and flags.
func assertAssistantEntryTyping(t *testing.T, row assistantEntryCase, path string, entries []schema.SessionEntry) {
	t.Helper()
	var depthZero []schema.SessionEntry
	for _, entry := range entries {
		if entry.Depth == 0 {
			depthZero = append(depthZero, entry)
		}
	}
	var want []assistantEntryExpectation
	if row.Generated != nil {
		want = buildGeneratedCursorWantEntries(row.Generated)
	} else {
		want = assistantEntryWantFor(t, row, path).Entries
	}
	if len(depthZero) != len(want) {
		t.Fatalf("case %q path %s holds %d depth-0 entries, want %d", row.Name, path, len(depthZero), len(want))
	}
	for i, w := range want {
		got := depthZero[i]
		if string(got.Role) != w.Role {
			t.Errorf("case %q path %s entry[%d] role = %q, want %q", row.Name, path, i, got.Role, w.Role)
		}
		if string(got.EntryType) != w.EntryType {
			t.Errorf("case %q path %s entry[%d] entry type = %q, want %q", row.Name, path, i, got.EntryType, w.EntryType)
		}
		if got.HasToolUse != w.HasToolUse {
			t.Errorf("case %q path %s entry[%d] has_tool_use = %t, want %t", row.Name, path, i, got.HasToolUse, w.HasToolUse)
		}
		if got.HasThinking != w.HasThinking {
			t.Errorf("case %q path %s entry[%d] has_thinking = %t, want %t", row.Name, path, i, got.HasThinking, w.HasThinking)
		}
	}
}

func buildGeneratedCursorWantEntries(generated *assistantEntryGenerated) []assistantEntryExpectation {
	var want []assistantEntryExpectation
	for i := 0; i < generated.UserText; i++ {
		want = append(want, assistantEntryExpectation{Role: "user", EntryType: "text"})
	}
	for i := 0; i < generated.AssistantText; i++ {
		want = append(want, assistantEntryExpectation{Role: "assistant", EntryType: "text"})
	}
	for i := 0; i < generated.ProseToolReplies; i++ {
		want = append(want, assistantEntryExpectation{Role: "assistant", EntryType: "text", HasToolUse: true})
	}
	return want
}
