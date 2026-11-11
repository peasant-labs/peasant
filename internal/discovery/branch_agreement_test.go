package discovery_test

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/selectionfilter"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/branch_agreement.yaml
var branchAgreementData []byte

type branchAgreementCase struct {
	Name             string   `yaml:"name"`
	Harness          string   `yaml:"harness"`
	ExtractsNoBranch bool     `yaml:"extractsNoBranch"`
	Subfolder        string   `yaml:"subfolder"`
	FolderOnly       bool     `yaml:"folderOnly"`
	RecordedBranch   string   `yaml:"recordedBranch"`
	Stored           bool     `yaml:"stored"`
	StoredBranch     string   `yaml:"storedBranch"`
	SourceChanged    bool     `yaml:"sourceChanged"`
	HeadBranch       string   `yaml:"headBranch"`
	ChosenBranch     string   `yaml:"chosenBranch"`
	TreeBranch       string   `yaml:"treeBranch"`
	Matched          bool     `yaml:"matched"`
	BranchDirs       []string `yaml:"branchDirs"`
}

// branchRecordingGit answers every directory with one checked-out branch and
// remembers which directories it was asked about.
type branchRecordingGit struct {
	*testutil.StubGitResolver
	branchDirs []string
}

func (g *branchRecordingGit) Branch(ctx context.Context, dir string) (string, error) {
	g.branchDirs = append(g.branchDirs, dir)
	return g.StubGitResolver.Branch(ctx, dir)
}

// TestTreeAndImportFilterPlaceASessionOnTheSameBranch runs the real scan (what
// the tree lists) and the real selection filter (what the import judges) over
// one recording, the same stored-session index and equivalent git doubles. They
// must agree on the branch and on the directory they ask git about.
func TestTreeAndImportFilterPlaceASessionOnTheSameBranch(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string              `yaml:"requiredNames"`
		Cases         []branchAgreementCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(branchAgreementData, &fixtures); err != nil {
		t.Fatalf("decode branch agreement fixture: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/discovery/testdata/branch_agreement.yaml", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runBranchAgreementCase(t, c)
		})
	}
}

func runBranchAgreementCase(t *testing.T, c branchAgreementCase) {
	t.Helper()
	const (
		remote    = "https://github.com/example-org/demo.git"
		sessionID = "11111111-1111-4111-8111-111111111111"
	)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(root, "clone")
	subfolder := clone
	if c.Subfolder != "" {
		subfolder = filepath.Join(clone, filepath.FromSlash(c.Subfolder))
	}
	if err := os.MkdirAll(subfolder, 0o700); err != nil {
		t.Fatal(err)
	}
	harness := ingest.HarnessClaudeCode
	if c.Harness != "" {
		harness = ingest.Harness(c.Harness)
	}
	source := filepath.Join(root, "recordings")
	var recording string
	switch harness {
	case ingest.HarnessClaudeCode:
		folder := "-workspace"
		if c.FolderOnly {
			folder = strings.ReplaceAll(clone, string(filepath.Separator), "-")
		}
		recording = filepath.Join(source, folder, sessionID+".jsonl")
	case ingest.HarnessCodex:
		// Codex files its rollouts under dated folders of its sessions directory.
		source = filepath.Join(root, harness.String(), "sessions")
		recording = filepath.Join(source, "2026", "05", "29", "rollout-2026-05-29T21-14-24-"+sessionID+".jsonl")
	default:
		t.Fatalf("the agreement fixture has no recording writer for harness %q", harness)
	}
	if err := os.MkdirAll(filepath.Dir(recording), 0o700); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().Add(-2 * time.Hour)
	writeBranchAgreementRecording(t, harness, recording, sessionID, subfolder, c, modified)

	cfg := config.BaseConfig()
	for registered := range ingest.DefaultAdapterRegistry {
		if provider, ok := cfg.Sources.Provider(registered); ok {
			provider.Enabled = registered == harness
			provider.Paths = nil
		}
	}
	under, ok := cfg.Sources.Provider(harness)
	if !ok {
		t.Fatalf("base config has no %s source", harness)
	}
	under.Enabled = true
	under.Paths = []string{source}
	cfg.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			harness.String(): {Projects: []config.ProjectSelection{
				{GitRemote: remote, Branches: []string{c.ChosenBranch}},
			}},
		},
	}

	var known discovery.KnownSessionIndex
	if c.Stored {
		ingested := time.Now()
		if c.SourceChanged {
			ingested = modified.Add(-time.Hour)
		}
		known = discovery.NewStoreKnownSessions([]store.IngestedSessionRow{{
			SessionID: sessionID, IngestedMs: ingested.UnixMilli(), SchemaVersion: int(ingest.CurrentSchemaVersion),
			GitRemote: remote, Branch: c.StoredBranch,
		}})
	}
	newGit := func() *branchRecordingGit {
		return &branchRecordingGit{StubGitResolver: &testutil.StubGitResolver{Remote: remote, BranchName: c.HeadBranch}}
	}
	expand := func(dirs []string) []string {
		out := make([]string, 0, len(dirs))
		for _, dir := range dirs {
			dir = strings.ReplaceAll(dir, "{subfolder}", subfolder)
			out = append(out, strings.ReplaceAll(dir, "{clone}", clone))
		}
		return out
	}
	wantDirs := expand(c.BranchDirs)

	// The tree: what the scan lists the session under.
	treeGit := newGit()
	_, listings, _ := discovery.Scan(t.Context(), cfg, &ingest.OSFileSystem{}, treeGit, known, nil, nil)
	var treeBranch string
	found := false
	for _, listing := range listings {
		if listing.SessionID == sessionID {
			treeBranch, found = listing.Branch, true
		}
	}
	if !found {
		t.Fatalf("the scan did not list session %s: %+v", sessionID, listings)
	}
	if treeBranch != c.TreeBranch {
		t.Fatalf("the tree lists the session under %q, want %q", treeBranch, c.TreeBranch)
	}
	assertBranchDirs(t, "the scan", treeGit.branchDirs, wantDirs)

	// The import filter: the same recording, the same index.
	adapter := ingest.DefaultAdapterRegistry[harness](&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}, Enabled: true})
	if err != nil || len(discovered) != 1 {
		t.Fatalf("discovered %d sessions, error %v", len(discovered), err)
	}
	filterGit := newGit()
	filter, _ := selectionfilter.New(cfg, filterGit, ingest.NewPhysicalPathResolver(),
		selectionfilter.WithKnownSessions(known, discovery.ConfigStaleness(cfg)))
	if err := filter.Prepare(t.Context(), discovered); err != nil {
		t.Fatalf("prepare the import cohort: %v", err)
	}
	if got := filter.Match(discovered[0]); got != c.Matched {
		t.Fatalf("the import filter admits the session = %v, want %v (the tree lists it under %q, the choice is %q)", got, c.Matched, treeBranch, c.ChosenBranch)
	}
	assertBranchDirs(t, "the import filter", filterGit.branchDirs, wantDirs)

	// The store: what the harness's own metadata extraction keeps for the session.
	if c.ExtractsNoBranch {
		extractGit := newGit()
		extractor := ingest.DefaultAdapterRegistry[harness](&ingest.OSFileSystem{}, extractGit, salt.Salt{})
		meta, err := extractor.ExtractMetadata(t.Context(), discovered[0])
		if err != nil {
			t.Fatalf("extract the metadata the store would keep: %v", err)
		}
		if meta.Git.Branch != nil {
			t.Fatalf("the store would keep the branch %q for a %s session that recorded none", *meta.Git.Branch, harness)
		}
		assertBranchDirs(t, "the metadata extraction", extractGit.branchDirs, nil)
	}
}

func assertBranchDirs(t *testing.T, who string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s asked git for the checked-out branch of %v, want %v", who, got, want)
	}
}

func writeBranchAgreementRecording(t *testing.T, harness ingest.Harness, path, id, cwd string, c branchAgreementCase, modified time.Time) {
	t.Helper()
	if harness == ingest.HarnessCodex {
		writeCodexAgreementRollout(t, path, id, cwd, c, modified)
		return
	}
	fields := fmt.Sprintf(`"sessionId":%q,"type":"user","uuid":"%s-user","timestamp":"2026-01-01T00:00:00Z"`, id, id)
	if !c.FolderOnly {
		fields += fmt.Sprintf(`,"cwd":%q`, cwd)
	}
	if c.RecordedBranch != "" {
		fields += fmt.Sprintf(`,"gitBranch":%q`, c.RecordedBranch)
	}
	body := "{" + fields + `,"message":{"role":"user","content":"hello"}}` + "\n" +
		fmt.Sprintf(`{"sessionId":%q,"type":"assistant","uuid":"%s-assistant","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"ok"}]}}`, id, id) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// writeCodexAgreementRollout writes a Codex rollout whose session_meta names the
// working directory and, when the case records a branch, a git block with it.
func writeCodexAgreementRollout(t *testing.T, path, id, cwd string, c branchAgreementCase, modified time.Time) {
	t.Helper()
	git := ""
	if c.RecordedBranch != "" {
		git = fmt.Sprintf(`,"git":{"commit_hash":"abc123","branch":%q,"repository_url":"https://github.com/example-org/demo.git"}`, c.RecordedBranch)
	}
	lines := []string{
		fmt.Sprintf(`{"timestamp":"2026-05-30T04:29:24.992Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-05-30T04:29:04.063Z","cwd":%q,"originator":"codex_vscode","cli_version":"0.133.0","source":"vscode","thread_source":"user","model_provider":"openai"%s}}`, id, cwd, git),
		fmt.Sprintf(`{"timestamp":"2026-05-30T04:29:25.001Z","type":"turn_context","payload":{"cwd":%q,"current_date":"2026-05-29","timezone":"UTC","model":"gpt-5.5","effort":"medium","turn_id":"turn-1"}}`, cwd),
		`{"timestamp":"2026-05-30T04:29:25.002Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first prompt"}]}}`,
		`{"timestamp":"2026-05-30T04:29:31.611Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first response"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}
