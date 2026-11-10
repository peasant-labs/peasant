package api

import (
	"bytes"
	"context"
	_ "embed"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/ingest-selection.yaml
var ingestSelectionYAML []byte

type ingestSelectionFixture struct {
	RequiredNames []string              `yaml:"requiredNames"`
	IDs           ingestSelectionIDs    `yaml:"ids"`
	Cases         []ingestSelectionCase `yaml:"cases"`
}

type ingestSelectionIDs struct {
	Selected string `yaml:"selected"`
	Other    string `yaml:"other"`
	Child    string `yaml:"child"`
}

type ingestSelectionCase struct {
	Name             string   `yaml:"name"`
	PreludeAll       bool     `yaml:"preludeAll"`
	ThroughRoute     bool     `yaml:"throughRoute"`
	Mode             string   `yaml:"mode"`
	Allow            []string `yaml:"allow"`
	WithChild        bool     `yaml:"withChild"`
	EmptyHarnesses   bool     `yaml:"emptyHarnesses"`
	Conflict         bool     `yaml:"conflict"`
	BranchExclusion  bool     `yaml:"branchExclusion"`
	Route            string   `yaml:"route"`
	ScopeSession     string   `yaml:"scopeSession"`
	ExpectImported   []string `yaml:"expectImported"`
	ExpectExcluded   []string `yaml:"expectExcluded"`
	ExpectDiagnostic bool     `yaml:"expectDiagnostic"`
}

func loadIngestSelectionFixture(t *testing.T) ingestSelectionFixture {
	t.Helper()
	var fixture ingestSelectionFixture
	if err := testutil.DecodeFixtureYAML(ingestSelectionYAML, &fixture); err != nil {
		t.Fatalf("decode ingest selection fixture: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, item := range fixture.Cases {
		if item.Name == "" || present[item.Name] {
			t.Fatalf("ingest selection fixture has an empty or duplicate case name %q", item.Name)
		}
		present[item.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/api/testdata/ingest-selection.yaml", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{fixture.IDs.Selected, fixture.IDs.Other, fixture.IDs.Child} {
		if _, err := ingest.NewSessionID(raw); err != nil {
			t.Fatalf("fixture session id %q: %v", raw, err)
		}
	}
	return fixture
}

func TestIngestSelectionFixtureRejectsMissingRequiredName(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(ingestSelectionYAML, []byte("name: all-policy-imports-every-session"), []byte("name: renamed-all-policy"), 1)
	var fixture ingestSelectionFixture
	if err := testutil.DecodeFixtureYAML(mutated, &fixture); err != nil {
		t.Fatalf("decode mutated fixture: %v", err)
	}
	present := map[string]bool{}
	for _, item := range fixture.Cases {
		present[item.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/api/testdata/ingest-selection.yaml", "case", fixture.RequiredNames, present); err == nil {
		t.Fatal("removing a required ingest-selection case name was accepted")
	}
}

func TestWebIngestAppliesSavedSelection(t *testing.T) {
	t.Parallel()
	fixture := loadIngestSelectionFixture(t)
	for _, item := range fixture.Cases {
		item := item
		t.Run(item.Name, func(t *testing.T) {
			t.Parallel()
			runIngestSelectionCase(t, fixture.IDs, item)
		})
	}
}

func runIngestSelectionCase(t *testing.T, ids ingestSelectionIDs, item ingestSelectionCase) {
	t.Helper()
	if item.Route == "session-scope" {
		runScopedIngestIgnoresSelection(t, ids, item)
		return
	}
	root := t.TempDir()
	source := filepath.Join(root, "recordings")
	output := filepath.Join(root, "managed")
	project := filepath.Join(source, "-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	selectedCWD := filepath.Join(root, "selected-cwd")
	otherCWD := filepath.Join(root, "other-cwd")
	if err := os.MkdirAll(selectedCWD, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherCWD, 0o700); err != nil {
		t.Fatal(err)
	}
	var exclusionPath, conflictRemote string
	if item.Conflict || item.BranchExclusion {
		branch := "main"
		if item.BranchExclusion {
			branch = "feature"
		}
		conflictRemote = "https://github.com/example-org/demo.git"
		initSelectionRepo(t, selectedCWD, conflictRemote, branch)
		resolved, err := filepath.EvalSymlinks(selectedCWD)
		if err != nil {
			t.Fatal(err)
		}
		exclusionPath = resolved
	}
	writeClaudeSession(t, filepath.Join(project, ids.Selected+".jsonl"), ids.Selected, selectedCWD, branchOf(item))
	writeClaudeSession(t, filepath.Join(project, ids.Other+".jsonl"), ids.Other, otherCWD, "main")
	if item.WithChild {
		childDir := filepath.Join(project, ids.Selected, "subagents")
		if err := os.MkdirAll(childDir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeClaudeSession(t, filepath.Join(childDir, ids.Child+".jsonl"), ids.Child, selectedCWD, "main")
	}

	cfg := webIngestConfig(t, source, output, item, exclusionPath, conflictRemote)
	hs := newTestXDGHomes(t)
	handler := hs.handler(nil, cfg)
	handler.configPath = filepath.Join(root, "custom-config", "settings.yaml")
	if item.PreludeAll {
		prelude := hs.handler(nil, webIngestConfig(t, source, output, ingestSelectionCase{Mode: "all"}, "", ""))
		first := runWebIngest(t, prelude, false)
		assertIngestMembership(t, first, []string{ids.Selected, ids.Other}, nil)
	}
	result := runWebIngest(t, handler, item.ThroughRoute)
	assertIngestMembership(t, result, item.ExpectImported, item.ExpectExcluded)
	if item.PreludeAll {
		opts, err := store.ManagedGenerationOptions(output)
		if err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(hs.dbPath(), append(opts, store.WithWriteConfig(cfg.Write), store.WithSkipMigrations())...)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		row, err := db.SessionByID(context.Background(), ids.Other)
		if err != nil || row == nil {
			t.Fatalf("excluded stored session missing: row=%v err=%v", row, err)
		}
		rows, err := db.ListSessionsFiltered(context.Background(), store.SessionListFilter{SessionID: &ids.Other})
		if err != nil || len(rows) != 1 || rows[0].SessionID != ids.Other {
			t.Fatalf("excluded session not listable: rows=%v err=%v", rows, err)
		}
	}
	if item.ExpectDiagnostic {
		configPath := handler.configPath
		found := false
		for _, diagnostic := range result.Diagnostics {
			if diagnostic.ErrorType == "selection_withheld_conflict" &&
				strings.Contains(diagnostic.Message, ids.Selected) &&
				strings.Contains(diagnostic.Message, "withheld") &&
				strings.Contains(diagnostic.Message, configPath) {
				found = true
			}
		}
		if !found {
			t.Fatalf("ingest diagnostics do not name the withheld session %s and config %s: %+v", ids.Selected, configPath, result.Diagnostics)
		}
	}
}

func runScopedIngestIgnoresSelection(t *testing.T, ids ingestSelectionIDs, item ingestSelectionCase) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "recordings")
	output := filepath.Join(root, "managed")
	project := filepath.Join(source, "-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaudeSession(t, filepath.Join(project, ids.Selected+".jsonl"), ids.Selected, cwd, "main")
	writeClaudeSession(t, filepath.Join(project, ids.Other+".jsonl"), ids.Other, cwd, "main")

	cfg := webIngestConfig(t, source, output, ingestSelectionCase{Mode: "selected", Allow: []string{ids.Selected}}, "", "")
	hs := newTestXDGHomes(t)
	if err := os.MkdirAll(filepath.Dir(hs.dbPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	storetest.CopyGoldenTo(t, hs.dbPath())
	handler := hs.handler(nil, cfg)
	first := runWebIngest(t, handler, false)
	assertIngestMembership(t, first, []string{ids.Selected}, []string{ids.Other})

	storeOptions, err := store.ManagedGenerationOptions(output)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(hs.dbPath(), append(storeOptions, store.WithWriteConfig(cfg.Write), store.WithSkipMigrations())...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	row, err := db.SessionByID(context.Background(), ids.Other)
	if err != nil || row != nil {
		t.Fatalf("excluded session unexpectedly stored: row=%v err=%v", row, err)
	}
	// The re-index endpoint accepts only stored sessions. Import the excluded
	// recording explicitly before re-indexing it, then change the source so a
	// successful HTTP response without a fresh index cannot pass this test.
	prelude := hs.handler(nil, webIngestConfig(t, source, output, ingestSelectionCase{Mode: "all"}, "", ""))
	runWebIngest(t, prelude, false)
	before := userTurns(t, db, ids.Other)
	if before == 0 {
		t.Fatal("prelude failed to index the target")
	}
	file, err := os.OpenFile(filepath.Join(project, ids.Other+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"sessionId":"` + ids.Other + `","type":"user","uuid":"new-user-turn","timestamp":"2026-01-01T00:00:02Z","cwd":"` + cwd + `","message":{"role":"user","content":"new turn"}}` + "\n")
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatalf("append reindex turn: write=%v close=%v", err, closeErr)
	}
	handler.store = db

	response := httptest.NewRecorder()
	handler.handleSyncIngestSession(response, httptest.NewRequest(http.MethodPost, "/api/v1/sync/ingest/session?session_id="+item.ScopeSession, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("explicit session re-index status = %d, want 200; body %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"status":"done"`) {
		t.Fatalf("explicit session re-index body = %s, want done", response.Body.String())
	}
	if got := userTurns(t, db, ids.Other); got != before+1 {
		t.Fatalf("scoped re-index stored %d user turns; want %d", got, before+1)
	}
}

func webIngestConfig(t *testing.T, source, output string, item ingestSelectionCase, exclusionPath, conflictRemote string) *config.Config {
	t.Helper()
	cfg := config.BaseConfig()
	cfg.Output.BasePath = output
	for harness := range ingest.DefaultAdapterRegistry {
		provider, ok := cfg.Sources.Provider(harness)
		if !ok {
			continue
		}
		provider.Enabled = harness == ingest.HarnessClaudeCode
		provider.Paths = nil
	}
	claude, ok := cfg.Sources.Provider(ingest.HarnessClaudeCode)
	if !ok {
		t.Fatal("base config has no claude-code source")
	}
	claude.Enabled = true
	claude.Paths = []string{source}
	mode := config.SelectionMode(item.Mode)
	if mode == "" {
		mode = config.SelectionModeSelected
	}
	cfg.Selection = config.SelectionConfig{Mode: mode}
	if item.EmptyHarnesses || mode == config.SelectionModeAll && len(item.Allow) == 0 && !item.Conflict && !item.BranchExclusion {
		return cfg
	}
	selected := config.SelectionHarnessConfig{Sessions: append([]string(nil), item.Allow...)}
	if item.Conflict {
		selected.Projects = []config.ProjectSelection{
			{GitRemote: conflictRemote, Branches: []string{"main"}},
			{GitRemote: conflictRemote, Branches: []string{"other"}},
		}
	}
	if item.BranchExclusion {
		selected.Exclusions.Branches = []config.BranchExclusion{{
			ClonePath: exclusionPath,
			Branches:  []string{"feature"},
		}}
	}
	if mode == config.SelectionModeAll && len(item.Allow) > 0 {
		// A restrictive allowlist must not matter while mode is all. Leaving it
		// on the config makes a wrongly applied filter fail this case.
		selected.Sessions = append([]string(nil), item.Allow...)
	}
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		ingest.HarnessClaudeCode.String(): selected,
	}
	return cfg
}

func runWebIngest(t *testing.T, handler *syncHandler, throughRoute bool) *ingest.PipelineResult {
	t.Helper()
	if throughRoute {
		response := httptest.NewRecorder()
		handler.handleSyncIngest(response, httptest.NewRequest(http.MethodPost, "/api/v1/sync/ingest", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("POST /api/v1/sync/ingest status = %d, body %s", response.Code, response.Body.String())
		}
		return waitIngest(t, handler)
	}
	handler.runIngestPipeline(ingest.NewProgressState())
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.ingestError != nil {
		t.Fatalf("web ingest failed: %v", handler.ingestError)
	}
	if handler.ingestResult == nil {
		t.Fatalf("web ingest status %q produced no result", handler.ingestStatus)
	}
	return handler.ingestResult
}

func waitIngest(t *testing.T, handler *syncHandler) *ingest.PipelineResult {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		handler.mu.Lock()
		status := handler.ingestStatus
		result := handler.ingestResult
		ingestErr := handler.ingestError
		handler.mu.Unlock()
		switch status {
		case "done":
			if result == nil {
				t.Fatal("ingest finished with no result")
			}
			return result
		case "error":
			t.Fatalf("ingest failed: %v", ingestErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingest still %q after 45s", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertIngestMembership(t *testing.T, result *ingest.PipelineResult, imported, excluded []string) {
	t.Helper()
	gotImported := map[string]bool{}
	gotExcluded := map[string]bool{}
	var described []string
	for _, session := range result.Sessions {
		id := session.SessionID.String()
		described = append(described, id+" status="+session.Status.String()+" excluded="+boolString(session.ExcludedBySelection)+" output="+session.OutputPath)
		if session.Error != nil {
			described[len(described)-1] += " error=" + session.Error.Error()
		}
		if session.ExcludedBySelection {
			gotExcluded[id] = true
			continue
		}
		if session.Error == nil && session.OutputPath != "" {
			gotImported[id] = true
		}
	}
	for _, id := range imported {
		if !gotImported[id] {
			t.Errorf("session %s was not imported; results: %s", id, strings.Join(described, "; "))
		}
	}
	for _, id := range excluded {
		if gotImported[id] || !gotExcluded[id] {
			t.Errorf("session %s was not excluded by selection; results: %s", id, strings.Join(described, "; "))
		}
	}
	if slices.Contains(imported, "") || slices.Contains(excluded, "") {
		t.Fatal("fixture listed an empty session id")
	}
}

func writeClaudeSession(t *testing.T, path, id, cwd, branch string) {
	t.Helper()
	body := `{"sessionId":"` + id + `","type":"user","uuid":"` + id + `-user","timestamp":"2026-01-01T00:00:00Z","cwd":"` + cwd + `","gitBranch":"` + branch + `","message":{"role":"user","content":"hello from the selection test"}}
{"sessionId":"` + id + `","type":"assistant","uuid":"` + id + `-assistant","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"ok"}]}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
}

func branchOf(item ingestSelectionCase) string {
	if item.BranchExclusion {
		return "feature"
	}
	return "main"
}

func initSelectionRepo(t *testing.T, dir, remote, branch string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "selection@example.test"},
		{"git", "config", "user.name", "Selection Fixture"},
		{"git", "config", "commit.gpgsign", "false"},
		{"git", "remote", "add", "origin", remote},
		{"git", "commit", "--allow-empty", "-m", "init"},
		{"git", "branch", "-M", branch},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup %v: %v\n%s", args, err, out)
		}
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
