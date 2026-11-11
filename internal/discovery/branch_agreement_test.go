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
	Name           string   `yaml:"name"`
	Subfolder      string   `yaml:"subfolder"`
	FolderOnly     bool     `yaml:"folderOnly"`
	RecordedBranch string   `yaml:"recordedBranch"`
	Stored         bool     `yaml:"stored"`
	StoredBranch   string   `yaml:"storedBranch"`
	SourceChanged  bool     `yaml:"sourceChanged"`
	HeadBranch     string   `yaml:"headBranch"`
	ChosenBranch   string   `yaml:"chosenBranch"`
	TreeBranch     string   `yaml:"treeBranch"`
	Matched        bool     `yaml:"matched"`
	BranchDirs     []string `yaml:"branchDirs"`
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
	source := filepath.Join(root, "recordings")
	folder := "-workspace"
	if c.FolderOnly {
		folder = strings.ReplaceAll(clone, string(filepath.Separator), "-")
	}
	recording := filepath.Join(source, folder, sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(recording), 0o700); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().Add(-2 * time.Hour)
	writeBranchAgreementRecording(t, recording, sessionID, subfolder, c, modified)

	cfg := config.BaseConfig()
	for harness := range ingest.DefaultAdapterRegistry {
		if provider, ok := cfg.Sources.Provider(harness); ok {
			provider.Enabled = harness == ingest.HarnessClaudeCode
			provider.Paths = nil
		}
	}
	claude, ok := cfg.Sources.Provider(ingest.HarnessClaudeCode)
	if !ok {
		t.Fatal("base config has no claude-code source")
	}
	claude.Enabled = true
	claude.Paths = []string{source}
	cfg.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			ingest.HarnessClaudeCode.String(): {Projects: []config.ProjectSelection{
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
	adapter := ingest.DefaultAdapterRegistry[ingest.HarnessClaudeCode](&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
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
}

func assertBranchDirs(t *testing.T, who string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s asked git for the checked-out branch of %v, want %v", who, got, want)
	}
}

func writeBranchAgreementRecording(t *testing.T, path, id, cwd string, c branchAgreementCase, modified time.Time) {
	t.Helper()
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
