package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

const (
	strikeFixtureRootID  = "20260728T123456.123456789Z-AAAAAAAAAAAAAAAAAAAAAAAAAA"
	strikeFixtureChildID = "BBBBBBBBBBBBBBBBBBBBBBBBBB"
)

func TestStrikeIngestCommandPersistsSessionDetail(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	sourceDir := completeStrikeFixtureDir(t, testRoot)
	outputDir := filepath.Join(testRoot, "sync")

	before, err := executeHarvestCmd(t, testRoot, []string{"--output", outputDir, "--json"})
	if err != nil {
		t.Fatalf("disabled-source harvest: %v\n%s", err, before)
	}
	if strings.Contains(before, strikeFixtureRootID) || strings.Contains(before, strikeFixtureChildID) {
		t.Fatalf("disabled Strike source imported fixture sessions: %s", before)
	}

	result, err := executeHarvestCmd(t, testRoot, []string{
		"--source-harness", schema.HarnessStrike.String(),
		"--source-path", sourceDir,
		"--output", outputDir,
		"--include-active",
		"--json",
	})
	if err != nil {
		t.Fatalf("source-scoped Strike harvest: %v\n%s", err, result)
	}
	if !strings.Contains(result, strikeFixtureRootID) || !strings.Contains(result, strikeFixtureChildID) {
		t.Fatalf("source-scoped Strike harvest did not report both fixture sessions: %s", result)
	}

	db, err := store.Open(defaults.ResolveDBFilePathWith(testRoot).String())
	if err != nil {
		t.Fatalf("open ingested Strike store: %v", err)
	}
	defer db.Close()

	provider := api.NewStoreDataProvider(db, sessionvisibility.All())
	ctx := context.Background()
	rootSession, err := provider.SessionByID(ctx, strikeFixtureRootID)
	if err != nil {
		t.Fatalf("load root session detail: %v", err)
	}
	rootDetail := api.SessionToDetail(rootSession)
	children, err := provider.ChildSessionsForParent(ctx, strikeFixtureRootID)
	if err != nil {
		t.Fatalf("load root child sessions: %v", err)
	}
	rootDetail.ChildSessions = children

	if rootDetail.Harness != schema.HarnessStrike {
		t.Errorf("root harness = %q, want %q", rootDetail.Harness, schema.HarnessStrike)
	}
	if len(rootDetail.ChildSessions) != 1 || rootDetail.ChildSessions[0].ID != strikeFixtureChildID {
		t.Fatalf("root child sessions = %+v, want child %q", rootDetail.ChildSessions, strikeFixtureChildID)
	}

	assistant := findStrikeTurn(t, rootDetail, schema.RoleAssistant)
	if !strings.Contains(assistant.Content, "inspect the fixture carefully") || !strings.Contains(assistant.Content, "inspect it now") {
		t.Errorf("assistant content did not preserve reasoning and text deltas: %q", assistant.Content)
	}
	if !assistant.HasThinking {
		t.Error("assistant turn did not retain thinking state")
	}
	if assistant.StopReason == nil || *assistant.StopReason != schema.StopReasonEndTurn {
		t.Errorf("assistant stop reason = %v, want %q", assistant.StopReason, schema.StopReasonEndTurn)
	}
	if assistant.TokensIn == nil || *assistant.TokensIn != 12 || assistant.TokensOut == nil || *assistant.TokensOut != 8 {
		t.Errorf("assistant usage = (%v, %v), want (12, 8)", assistant.TokensIn, assistant.TokensOut)
	}

	readCall := findStrikeToolCall(t, assistant.ToolCalls, "call-read")
	if readCall.Name != "todoread" || readCall.ToolKind != schema.ToolCallKindRead {
		t.Errorf("read call = {name:%q kind:%q}, want todoread/read", readCall.Name, readCall.ToolKind)
	}
	if readCall.Result != "read-process\nread-stream\nread-final" {
		t.Errorf("read call result = %q, want process, stream, and final output in source order", readCall.Result)
	}

	editCall := findStrikeToolCall(t, assistant.ToolCalls, "call-edit")
	if editCall.Name != "notebook_edit" || editCall.ToolKind != schema.ToolCallKindEdit {
		t.Errorf("edit call = {name:%q kind:%q}, want notebook_edit/edit", editCall.Name, editCall.ToolKind)
	}
	if editCall.Result != "edit-stream\nedit-final" {
		t.Errorf("edit call result = %q, want stream and final output", editCall.Result)
	}

	childSession, err := provider.SessionByID(ctx, strikeFixtureChildID)
	if err != nil {
		t.Fatalf("load child session detail: %v", err)
	}
	childDetail := api.SessionToDetail(childSession)
	childAssistant := findStrikeTurn(t, childDetail, schema.RoleAssistant)
	pluginCall := findStrikeToolCall(t, childAssistant.ToolCalls, "child-call")
	if pluginCall.Name != "plugin_custom" || pluginCall.ToolKind != schema.ToolCallKindOther || pluginCall.Result != "child-result" {
		t.Errorf("plugin call = %+v, want raw plugin_custom name, other kind, and child-result", pluginCall)
	}
	orphanCall := findStrikeToolCall(t, childAssistant.ToolCalls, "orphan-call")
	if orphanCall.Name != "orphan_tool" || orphanCall.ToolKind != schema.ToolCallKindOther || orphanCall.Result != "orphan-result" {
		t.Errorf("unmatched tool.end call = %+v, want preserved synthetic tool use/result", orphanCall)
	}
}

func TestStrikeIngestCommandAddsChildAfterParentSourceDisappears(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	sourceDir := filepath.Join(testRoot, "strike-source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("create isolated Strike source: %v", err)
	}
	fixtureDir, err := filepath.Abs(filepath.Join("testdata", "strike"))
	if err != nil {
		t.Fatalf("resolve Strike fixture directory: %v", err)
	}
	copyStrikeFixtureFiles(t, fixtureDir, sourceDir,
		strikeFixtureRootID+".jsonl",
		strikeFixtureRootID+".meta.json",
	)

	outputDir := filepath.Join(testRoot, "sync")
	args := []string{
		"--source-harness", schema.HarnessStrike.String(),
		"--source-path", sourceDir,
		"--output", outputDir,
		"--include-active",
		"--json",
	}
	first, err := executeHarvestCmd(t, testRoot, args)
	if err != nil {
		t.Fatalf("initial parent-only Strike harvest: %v\n%s", err, first)
	}
	if !strings.Contains(first, strikeFixtureRootID) {
		t.Fatalf("initial Strike harvest did not report parent session: %s", first)
	}
	for _, suffix := range []string{".jsonl", ".meta.json"} {
		if err := os.Remove(filepath.Join(sourceDir, strikeFixtureRootID+suffix)); err != nil {
			t.Fatalf("remove parent source %s: %v", suffix, err)
		}
	}
	copyStrikeFixtureFiles(t, fixtureDir, sourceDir,
		strikeFixtureChildID+".jsonl",
		strikeFixtureChildID+".meta.json",
	)

	second, err := executeHarvestCmd(t, testRoot, args)
	if err != nil {
		t.Fatalf("second child-only Strike harvest: %v\n%s", err, second)
	}
	if !strings.Contains(second, strikeFixtureChildID) {
		t.Fatalf("second Strike harvest did not report child session: %s", second)
	}

	db, err := store.Open(defaults.ResolveDBFilePathWith(testRoot).String())
	if err != nil {
		t.Fatalf("open two-run Strike store: %v", err)
	}
	defer db.Close()
	provider := api.NewStoreDataProvider(db, sessionvisibility.All())
	children, err := provider.ChildSessionsForParent(context.Background(), strikeFixtureRootID)
	if err != nil {
		t.Fatalf("load children after second Strike harvest: %v", err)
	}
	if len(children) != 1 || children[0].ID != strikeFixtureChildID {
		t.Fatalf("persisted children = %+v, want child %q", children, strikeFixtureChildID)
	}
}

// retiredStrikePerLineLimit is the 10 MiB per-line limit this build replaced.
// A record that long once failed or was omitted forever; the mounted case
// below proves the real command now ingests it whole.
const retiredStrikePerLineLimit = 10 << 20

func TestStrikeIngestKeepsARecordOverTheRetiredPerLineLimit(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	sourceDir := completeStrikeFixtureDir(t, testRoot)

	rootTranscript := filepath.Join(sourceDir, strikeFixtureRootID+".jsonl")
	transcript, err := os.ReadFile(rootTranscript)
	if err != nil {
		t.Fatalf("read copied root transcript: %v", err)
	}
	marker := `{"type":"assistant.reasoning.delta"`
	markerIndex := strings.Index(string(transcript), marker)
	if markerIndex < 0 {
		t.Fatalf("root fixture is missing insertion marker %q", marker)
	}
	const largeRecordSentinel = "LARGE_RECORD_KEPT_SENTINEL"
	large := `{"type":"assistant.text.delta","time":"2026-07-28T12:34:58.500Z","data":{"turnId":"turn-1","delta":"` +
		largeRecordSentinel + strings.Repeat("x", retiredStrikePerLineLimit) + `"}}` + "\n"
	if len(large) <= retiredStrikePerLineLimit {
		t.Fatalf("built a %d-byte record; the case only means something over the retired %d-byte limit", len(large), retiredStrikePerLineLimit)
	}
	withLarge := string(transcript[:markerIndex]) + large + string(transcript[markerIndex:])
	if err := os.WriteFile(rootTranscript, []byte(withLarge), 0o600); err != nil {
		t.Fatalf("write generated large Strike record: %v", err)
	}

	outputDir := filepath.Join(testRoot, "sync")
	result, err := executeHarvestCmd(t, testRoot, []string{
		"--source-harness", schema.HarnessStrike.String(),
		"--source-path", sourceDir,
		"--output", outputDir,
		"--include-active",
		"--json",
	})
	if err != nil {
		t.Fatalf("harvest a Strike session holding a record over the retired per-line limit: %v\n%s", err, result)
	}

	artifactPath := findStrikeArtifact(t, outputDir, strikeFixtureRootID+"--transcript.jsonl")
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read persisted Strike artifact: %v", err)
	}
	if !strings.Contains(string(artifact), largeRecordSentinel) {
		t.Fatal("the persisted Strike artifact dropped the large source record")
	}
	if !strings.Contains(string(artifact), "I will inspect it now.") {
		t.Fatal("the persisted Strike artifact lost valid content after the large record")
	}

	metadataPath := findStrikeArtifact(t, outputDir, strikeFixtureRootID+defaults.MetadataSuffix)
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read Strike metadata artifact: %v", err)
	}
	var metadata schema.UnifiedMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatalf("decode Strike metadata artifact: %v", err)
	}
	if metadata.Diagnostics.Partial != nil && *metadata.Diagnostics.Partial {
		t.Errorf("the session was stored partial; a record within the limit is kept whole and the capture stays complete")
	}
	for _, warning := range metadata.Diagnostics.Warnings {
		if warning.ErrorType == "record_too_large" {
			t.Errorf("a record within the limit was reported as too large: %+v", warning)
		}
	}

	db, err := store.Open(defaults.ResolveDBFilePathWith(testRoot).String())
	if err != nil {
		t.Fatalf("open large-record test store: %v", err)
	}
	defer db.Close()
	provider := api.NewStoreDataProvider(db, sessionvisibility.All())
	detail, err := provider.SessionByID(context.Background(), strikeFixtureRootID)
	if err != nil {
		t.Fatalf("the session detail read refused a session holding a large record: %v", err)
	}
	served, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal served session detail: %v", err)
	}
	if !strings.Contains(string(served), largeRecordSentinel) {
		t.Fatal("the served session detail does not carry the large record's turn")
	}
	if _, err := executeHarvestCmd(t, testRoot, []string{"index", "--force", "--output", outputDir, "--json"}); err != nil {
		t.Fatalf("harvest index --force refused a complete session holding a large record: %v", err)
	}
}

// Successful persistent indexing needs complete recognized records. Keep the
// deliberately malformed shared fixture for adapter/legacy tolerance tests;
// these positive mounted cases use its complete variant and the same sidecars.
func completeStrikeFixtureDir(t *testing.T, testRoot string) string {
	t.Helper()
	sourceDir := filepath.Join(testRoot, "strike-source")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	copyStrikeFixtureFiles(t, filepath.Join("testdata", "strike"), sourceDir,
		strikeFixtureRootID+".meta.json", strikeFixtureChildID+".jsonl", strikeFixtureChildID+".meta.json")
	transcript, err := os.ReadFile(filepath.Join("testdata", "strike_complete.jsonl"))
	if err != nil {
		t.Fatalf("read complete Strike fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, strikeFixtureRootID+".jsonl"), transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	return sourceDir
}

func findStrikeArtifact(t *testing.T, root, name string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == name {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk Strike output artifacts: %v", err)
	}
	if found == "" {
		t.Fatalf("Strike output artifact %q not found under %s", name, root)
	}
	return found
}

func copyStrikeFixtureFiles(t *testing.T, fixtureDir, destination string, names ...string) {
	t.Helper()
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatalf("read Strike fixture %q: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(destination, name), data, 0o600); err != nil {
			t.Fatalf("copy Strike fixture %q: %v", name, err)
		}
	}
}

func findStrikeTurn(t *testing.T, detail *api.SessionDetailPayload, role schema.Role) schema.TurnDetail {
	t.Helper()
	for _, turn := range detail.Turns {
		if turn.Role == role && (turn.Content != "" || len(turn.ToolCalls) > 0) {
			return turn
		}
	}
	t.Fatalf("session %q has no populated %q turn: %+v", detail.ID, role, detail.Turns)
	return schema.TurnDetail{}
}

func findStrikeToolCall(t *testing.T, calls []schema.ToolCallDetail, id string) schema.ToolCallDetail {
	t.Helper()
	for _, call := range calls {
		if call.ID == id {
			return call
		}
	}
	t.Fatalf("tool call %q not found in %+v", id, calls)
	return schema.ToolCallDetail{}
}
