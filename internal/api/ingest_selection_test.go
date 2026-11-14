package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
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
	// Unrecorded names a session whose recording carries no branch.
	Unrecorded string `yaml:"unrecorded"`
}

type ingestSelectionCase struct {
	Name            string   `yaml:"name"`
	PreludeAll      bool     `yaml:"preludeAll"`
	ThroughRoute    bool     `yaml:"throughRoute"`
	Mode            string   `yaml:"mode"`
	Allow           []string `yaml:"allow"`
	WithChild       bool     `yaml:"withChild"`
	EmptyHarnesses  bool     `yaml:"emptyHarnesses"`
	Conflict        bool     `yaml:"conflict"`
	BranchExclusion bool     `yaml:"branchExclusion"`
	Route           string   `yaml:"route"`
	ScopeSession    string   `yaml:"scopeSession"`
	// MovedClone runs the case on a clone that has since moved to ChosenBranch:
	// the Selected session recorded ChosenBranch, the Other session recorded
	// OtherRecordedBranch, and both ran in that one clone.
	MovedClone          bool   `yaml:"movedClone"`
	ChosenBranch        string `yaml:"chosenBranch"`
	OtherRecordedBranch string `yaml:"otherRecordedBranch"`
	// DerivedBranchRule runs the case on a single-branch clone whose selection is
	// derived from the real tree by choosing its one branch, then adds a session
	// recorded on LaterBranch to that clone. AutoIngestNewBranches is the answer
	// the person gave for new branches.
	DerivedBranchRule     bool     `yaml:"derivedBranchRule"`
	LaterBranch           string   `yaml:"laterBranch"`
	AutoIngestNewBranches bool     `yaml:"autoIngestNewBranches"`
	ExpectImported        []string `yaml:"expectImported"`
	ExpectExcluded        []string `yaml:"expectExcluded"`
	ExpectDiagnostic      bool     `yaml:"expectDiagnostic"`
	// ExpectWritten is what the finish panel and the status route report for
	// the run: the sessions written new and the sessions written again.
	ExpectWritten *ingestSelectionWritten `yaml:"expectWritten"`
	// ExpectIngestError makes the case run the import and require it to refuse
	// to start, with each of these in its message.
	ExpectIngestError []string `yaml:"expectIngestError"`
	// ActiveRecordings leaves each recording's modification time at the moment
	// it was written, so the run meets it while it is still being written.
	// Without it every recording is two hours old.
	ActiveRecordings bool `yaml:"activeRecordings"`
}

type ingestSelectionWritten struct {
	New     int `yaml:"new"`
	Updated int `yaml:"updated"`
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
	for _, raw := range []string{fixture.IDs.Selected, fixture.IDs.Other, fixture.IDs.Child, fixture.IDs.Unrecorded} {
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
	if item.MovedClone {
		runMovedCloneTreeAndImportAgree(t, ids, item)
		return
	}
	if item.DerivedBranchRule {
		runDerivedBranchRuleAdmitsOnlyTheChosenBranch(t, ids, item)
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
	if item.ActiveRecordings {
		for _, id := range []string{ids.Selected, ids.Other} {
			now := time.Now()
			if err := os.Chtimes(filepath.Join(project, id+".jsonl"), now, now); err != nil {
				t.Fatal(err)
			}
		}
	}
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
	if len(item.ExpectIngestError) > 0 {
		assertIngestRefused(t, handler, item.ExpectIngestError)
		return
	}
	if item.PreludeAll {
		prelude := hs.handler(nil, webIngestConfig(t, source, output, ingestSelectionCase{Mode: "all"}, "", ""))
		first := runWebIngest(t, prelude, false)
		assertIngestMembership(t, first, []string{ids.Selected, ids.Other}, nil)
	}
	result := runWebIngest(t, handler, item.ThroughRoute)
	assertIngestMembership(t, result, item.ExpectImported, item.ExpectExcluded)
	if item.ExpectWritten != nil {
		assertIngestWritten(t, handler, *item.ExpectWritten)
	}
	if item.PreludeAll {
		opts, err := store.ManagedGenerationOptions(output)
		if err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(hs.dbPath(), append(opts, store.WithWriteConfig(cfg.Write))...)
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
	// The finish panel is told about the sessions the selection withheld, beside
	// the counts, and about nothing when no session was withheld.
	if note := handler.ingestWire().Note; item.ExpectDiagnostic {
		configPath := handler.configPath
		if !strings.Contains(note, "1 session was left out") || !strings.Contains(note, configPath) {
			t.Fatalf("the finished import's note is %q, want it to say one session was left out and name the configuration file %s", note, configPath)
		}
	} else if note != "" {
		t.Fatalf("the finished import's note is %q, want none: no session was withheld", note)
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

// assertIngestRefused runs the import and requires it to end in an error that
// says what is wrong and where, before it read or imported any source.
func assertIngestRefused(t *testing.T, handler *syncHandler, wantInMessage []string) {
	t.Helper()
	handler.runIngestPipeline(ingest.NewProgressState())
	handler.mu.Lock()
	status, refusal, result := handler.ingestStatus, handler.ingestError, handler.ingestResult
	handler.mu.Unlock()
	if status != "error" || refusal == nil || result != nil {
		t.Fatalf("the import status = %q, error = %v, result = %v; want an error and no result", status, refusal, result)
	}
	for _, part := range wantInMessage {
		if !strings.Contains(refusal.Error(), part) {
			t.Errorf("the refusal %q should contain %q", refusal, part)
		}
	}
}

// assertIngestWritten checks the counts the finish panel is given when the
// import ends and the counts the status route serves to a tab that reconnects:
// both are what the run wrote, one number each for new and updated.
func assertIngestWritten(t *testing.T, handler *syncHandler, want ingestSelectionWritten) {
	t.Helper()
	wire := handler.ingestWire()
	if wire.New != want.New || wire.Updated != want.Updated {
		t.Errorf("the finish panel is told %d new and %d updated, want %d and %d", wire.New, wire.Updated, want.New, want.Updated)
	}
	response := httptest.NewRecorder()
	handler.handleSyncIngestStatus(response, httptest.NewRequest(http.MethodGet, "/api/v1/sync/ingest/status", nil))
	var status struct {
		Result struct {
			New     int `json:"new"`
			Updated int `json:"updated"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode the ingest status: %v; body %s", err, response.Body.String())
	}
	if status.Result.New != want.New || status.Result.Updated != want.Updated {
		t.Errorf("the status route reports %d new and %d updated, want %d and %d", status.Result.New, status.Result.Updated, want.New, want.Updated)
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
	db, err := store.Open(hs.dbPath(), append(storeOptions, store.WithWriteConfig(cfg.Write))...)
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

// runMovedCloneTreeAndImportAgree proves the choice a person makes in the tree
// is the import that follows, however the clone has moved since. One clone
// hosts three sessions: one recorded on the chosen branch, one recorded on
// another, and one that recorded no branch. The real scan, the real forest and
// the saved selection decide what the tree shows as chosen; the real web import
// must bring in exactly that set.
//
// First the clone sits on the chosen branch, so git's checked-out branch would
// put the session recorded elsewhere on it too. Then the clone moves to another
// branch and the sessions, now stored and unchanged, are scanned and imported
// again: the session that recorded no branch stays where the store put it.
func runMovedCloneTreeAndImportAgree(t *testing.T, ids ingestSelectionIDs, item ingestSelectionCase) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "recordings")
	output := filepath.Join(root, "managed")
	project := filepath.Join(source, "-workspace")
	clone := filepath.Join(root, "clone")
	for _, dir := range []string{project, clone} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const remote = "https://github.com/example-org/demo.git"
	initSelectionRepo(t, clone, remote, item.ChosenBranch)
	writeClaudeSession(t, filepath.Join(project, ids.Selected+".jsonl"), ids.Selected, clone, item.ChosenBranch)
	writeClaudeSession(t, filepath.Join(project, ids.Other+".jsonl"), ids.Other, clone, item.OtherRecordedBranch)
	writeClaudeSession(t, filepath.Join(project, ids.Unrecorded+".jsonl"), ids.Unrecorded, clone, "")

	cfg := webIngestConfig(t, source, output, ingestSelectionCase{Mode: "selected", EmptyHarnesses: true}, "", "")
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		ingest.HarnessClaudeCode.String(): {Projects: []config.ProjectSelection{
			{GitRemote: remote, Branches: []string{item.ChosenBranch}},
		}},
	}
	hs := newTestXDGHomes(t)
	all := []string{ids.Selected, ids.Other, ids.Unrecorded}

	// tree is what the setup tree shows: the branch each session is listed under
	// and the sessions the saved selection marks as chosen.
	tree := func(known ingest.KnownSessionIndex) (map[string]string, []string) {
		_, listings, relation := discovery.Scan(t.Context(), cfg, &ingest.OSFileSystem{}, &ingest.ExecGitResolver{}, known, nil, nil)
		prepared := discovery.PrepareSessionListings(listings, ingest.NewPhysicalPathResolver())
		forest, _, err := onboarding.RestoreSelection(discovery.BuildForest(prepared, nil, relation), cfg.Selection)
		if err != nil {
			t.Fatalf("restore the saved selection onto the tree: %v", err)
		}
		branchOfNode := make(map[string]string)
		for _, node := range forest.Nodes {
			if node.Kind == onboarding.NodeKindBranch {
				branchOfNode[node.ID] = node.Label
			}
		}
		listedUnder := make(map[string]string)
		var chosen []string
		for _, node := range forest.Nodes {
			if node.Kind != onboarding.NodeKindSession {
				continue
			}
			listedUnder[node.ID] = branchOfNode[node.ParentID]
			if node.State == onboarding.NodeStateAll {
				chosen = append(chosen, node.ID)
			}
		}
		slices.Sort(chosen)
		return listedUnder, chosen
	}
	assertTreeShows := func(phase string, listedUnder map[string]string, wantUnrecorded string) {
		t.Helper()
		want := map[string]string{ids.Selected: item.ChosenBranch, ids.Other: item.OtherRecordedBranch, ids.Unrecorded: wantUnrecorded}
		if !maps.Equal(listedUnder, want) {
			t.Fatalf("%s: the tree lists the sessions under %v, want %v", phase, listedUnder, want)
		}
	}

	// The clone sits on the chosen branch. A session that recorded no branch is
	// placed by it, and nothing else is.
	listedUnder, treeChosen := tree(nil)
	assertTreeShows("clone on the chosen branch", listedUnder, item.ChosenBranch)
	wantChosen := []string{ids.Selected, ids.Unrecorded}
	slices.Sort(wantChosen)
	if !slices.Equal(treeChosen, wantChosen) {
		t.Fatalf("clone on the chosen branch: the tree shows %v as chosen, want %v", treeChosen, wantChosen)
	}
	result := runWebIngest(t, hs.handler(nil, cfg), false)
	assertIngestMembership(t, result, item.ExpectImported, item.ExpectExcluded)
	imported, _ := importedAndLeftOut(result)
	if !slices.Equal(imported, treeChosen) {
		t.Fatalf("clone on the chosen branch: the import brought in %v but the tree showed %v as chosen", imported, treeChosen)
	}

	// The clone moves on. The stored, unchanged sessions are scanned and imported
	// again against the rows the store now holds.
	runGit(t, clone, "checkout", "--quiet", "-b", "moved-on")
	storeOptions, err := store.ManagedGenerationOptions(output)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(hs.dbPath(), append(storeOptions, store.WithWriteConfig(cfg.Write))...)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.AllIngestedSessions(t.Context())
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatalf("read the stored sessions: read=%v close=%v", err, closeErr)
	}
	listedUnder, treeChosen = tree(discovery.NewStoreKnownSessions(rows))
	assertTreeShows("clone moved on", listedUnder, item.ChosenBranch)
	if !slices.Equal(treeChosen, wantChosen) {
		t.Fatalf("clone moved on: the tree shows %v as chosen, want %v", treeChosen, wantChosen)
	}
	result = runWebIngest(t, hs.handler(nil, cfg), false)
	_, leftOut := importedAndLeftOut(result)
	var treeUnchosen []string
	for _, id := range all {
		if !slices.Contains(treeChosen, id) {
			treeUnchosen = append(treeUnchosen, id)
		}
	}
	slices.Sort(treeUnchosen)
	if !slices.Equal(leftOut, treeUnchosen) {
		t.Fatalf("clone moved on: the import left out %v but the tree showed %v as unchosen", leftOut, treeUnchosen)
	}
}

// importedAndLeftOut is what the web import did with each session: the ones it
// wrote, and the ones the saved selection left out.
func importedAndLeftOut(result *ingest.PipelineResult) (imported, leftOut []string) {
	for _, session := range result.Sessions {
		switch {
		case session.ExcludedBySelection:
			leftOut = append(leftOut, session.SessionID.String())
		case session.Error == nil && session.OutputPath != "":
			imported = append(imported, session.SessionID.String())
		}
	}
	slices.Sort(imported)
	slices.Sort(leftOut)
	return imported, leftOut
}

// runDerivedBranchRuleAdmitsOnlyTheChosenBranch proves that choosing the one
// branch of a checkout saves that branch, and that the saved rule then decides
// what the import admits. The selection is not written by hand: the real scan
// builds the tree, the person's answer is the chosen session, and the shared
// derivation writes the rule. A session recorded in the clone later, on a branch
// that did not exist when the choice was made, is admitted only by the
// new-branch setting, in the tree and in the import alike.
func runDerivedBranchRuleAdmitsOnlyTheChosenBranch(t *testing.T, ids ingestSelectionIDs, item ingestSelectionCase) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "recordings")
	output := filepath.Join(root, "managed")
	project := filepath.Join(source, "-workspace")
	clone := filepath.Join(root, "clone")
	for _, dir := range []string{project, clone} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	physicalClone, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	const remote = "https://github.com/example-org/demo.git"
	initSelectionRepo(t, clone, remote, item.ChosenBranch)
	writeClaudeSession(t, filepath.Join(project, ids.Selected+".jsonl"), ids.Selected, clone, item.ChosenBranch)

	cfg := webIngestConfig(t, source, output, ingestSelectionCase{Mode: "selected", EmptyHarnesses: true}, "", "")
	cfg.Selection.AutoIngestNewBranches = item.AutoIngestNewBranches
	scanForest := func() onboarding.Forest {
		_, listings, relation := discovery.Scan(t.Context(), cfg, &ingest.OSFileSystem{}, &ingest.ExecGitResolver{}, nil, nil, nil)
		prepared := discovery.PrepareSessionListings(listings, ingest.NewPhysicalPathResolver())
		return discovery.BuildForest(prepared, nil, relation)
	}
	chosenInTree := func() []string {
		restored, _, restoreErr := onboarding.RestoreSelection(scanForest(), cfg.Selection)
		if restoreErr != nil {
			t.Fatalf("restore the saved selection onto the tree: %v", restoreErr)
		}
		chosen := onboarding.SelectedSessionIDs(restored)
		slices.Sort(chosen)
		return chosen
	}

	// The person chooses the branch: the answer is its session, as the setup tree
	// reports it, and the shared derivation writes the rule.
	value, err := onboarding.SelectionIDsValue([]string{ids.Selected})
	if err != nil {
		t.Fatal(err)
	}
	answers := onboarding.Answers{onboarding.FieldSelection: value}
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeSelection})
	if err := model.ApplyAnswers(cfg, answers, onboarding.WriteContext{Config: cfg, Forest: scanForest(), Answers: answers}); err != nil {
		t.Fatalf("derive the selection from the chosen branch: %v", err)
	}
	wantRule := []config.ProjectSelection{{GitRemote: remote, ClonePaths: []string{physicalClone}, Branches: []string{item.ChosenBranch}}}
	if got := cfg.Selection.Harnesses[ingest.HarnessClaudeCode.String()].Projects; !reflect.DeepEqual(got, wantRule) {
		t.Fatalf("the saved rule for the chosen branch is %+v, want %+v", got, wantRule)
	}
	hs := newTestXDGHomes(t)
	first := runWebIngest(t, hs.handler(nil, cfg), false)
	assertIngestMembership(t, first, []string{ids.Selected}, nil)

	// A session recorded in the clone later, on a branch that did not exist when
	// the choice was made.
	writeClaudeSession(t, filepath.Join(project, ids.Other+".jsonl"), ids.Other, clone, item.LaterBranch)
	wantChosen := []string{ids.Selected}
	if item.AutoIngestNewBranches {
		wantChosen = append(wantChosen, ids.Other)
	}
	slices.Sort(wantChosen)
	if got := chosenInTree(); !slices.Equal(got, wantChosen) {
		t.Fatalf("with new branches %t the tree shows %v as chosen, want %v", item.AutoIngestNewBranches, got, wantChosen)
	}
	second := runWebIngest(t, hs.handler(nil, cfg), false)
	imported, leftOut := importedAndLeftOut(second)
	if slices.Contains(imported, ids.Other) != item.AutoIngestNewBranches || slices.Contains(leftOut, ids.Other) == item.AutoIngestNewBranches {
		t.Fatalf("with new branches %t the import wrote %v and left out %v, but the tree shows %v as chosen", item.AutoIngestNewBranches, imported, leftOut, wantChosen)
	}
	if slices.Contains(leftOut, ids.Selected) {
		t.Fatalf("the import left out the chosen branch's session: %v", leftOut)
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
