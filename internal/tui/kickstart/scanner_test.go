package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

// listingsDoc mirrors the flat-discovery fixture the real scanner adapter folds.
// It carries row-count guards so a fixture edit that adds or drops a row without
// updating the expected counts fails loudly rather than silently changing what
// the adapter is measured against.
type listingsDoc struct {
	Name                 string                `yaml:"name"`
	ExpectedListingCount int                   `yaml:"expectedListingCount"`
	ExpectedProjectCount int                   `yaml:"expectedProjectCount"`
	ExpectedBranchCount  int                   `yaml:"expectedBranchCount"`
	ExpectedSessionCount int                   `yaml:"expectedSessionCount"`
	Listings             []ftue.SessionListing `yaml:"listings"`
}

//go:embed testdata/listings.yaml
var listingsData []byte

func loadListings(t *testing.T) listingsDoc {
	t.Helper()
	var doc listingsDoc
	if err := decodeStrictFixture(listingsData, &doc); err != nil {
		t.Fatalf("decode listings fixture: %v", err)
	}
	if doc.ExpectedListingCount != len(doc.Listings) || len(doc.Listings) == 0 {
		t.Fatalf("expectedListingCount=%d but %d listings present", doc.ExpectedListingCount, len(doc.Listings))
	}
	return doc
}

func TestListingsFixtureRejectsUnknownCloneIdentityKey(t *testing.T) {
	malformed := bytes.Replace(listingsData, []byte("workingDir:"), []byte("workingDirectoryTypo:"), 1)
	if bytes.Equal(malformed, listingsData) {
		t.Fatal("listings fixture has no workingDir key to mutate")
	}
	var document listingsDoc
	if err := decodeStrictFixture(malformed, &document); err == nil {
		t.Fatal("listings fixture decoder accepted an unknown clone-identity key")
	}
}

// forestCounts walks the PROJECT -> BRANCH -> SESSION forest and returns
// (projects, branches, sessions) so the fold's shape can be asserted against the
// fixture guards.
func forestCounts(roots []*kit.TreeNode) (p, b, s int) {
	p = len(roots)
	for _, project := range roots {
		b += len(project.Children)
		for _, branch := range project.Children {
			s += len(branch.Children)
		}
	}
	return
}

// TestScannerTreeSource_FoldsFlatListingIntoForest proves the real scanner
// adapter groups the flat discovery listing into the project-first
// PROJECT -> BRANCH -> SESSION forest at the shape the fixture pins (no harness
// grouping axis), and that a Load never errors.
func TestScannerTreeSource_FoldsFlatListingIntoForest(t *testing.T) {
	t.Parallel()
	doc := loadListings(t)
	src := kickstart.NewScannerTreeSource(doc.Listings, withFixturePathResolver())
	roots, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	p, b, s := forestCounts(roots)
	if p != doc.ExpectedProjectCount || b != doc.ExpectedBranchCount || s != doc.ExpectedSessionCount {
		t.Fatalf("forest shape = projects %d branches %d sessions %d; want %d/%d/%d",
			p, b, s, doc.ExpectedProjectCount, doc.ExpectedBranchCount, doc.ExpectedSessionCount)
	}
	for _, project := range roots {
		for _, branch := range project.Children {
			for _, session := range branch.Children {
				if session.Meta[settings.MetaRemoteMultiplicity] != settings.MetaMultiplicityUnique ||
					session.Meta[settings.MetaNameMultiplicity] != settings.MetaMultiplicityUnique {
					t.Errorf("session %q multiplicities = remote %q name %q, want explicit unique; repeated sessions must not count as clones",
						session.ID, session.Meta[settings.MetaRemoteMultiplicity], session.Meta[settings.MetaNameMultiplicity])
				}
			}
		}
	}
}

// TestScannerTreeSource_ProjectFirstNoHarnessRoot proves the top level is the
// project (labelled with the canonical projectlabel.Label form), NOT the
// harness: no root node's ID is a harness slug, and every project root carries a
// git-remote Meta and a "github:owner/repo"-style label rather than a filesystem
// path.
func TestScannerTreeSource_ProjectFirstNoHarnessRoot(t *testing.T) {
	t.Parallel()
	doc := loadListings(t)
	roots, err := kickstart.NewScannerTreeSource(doc.Listings, withFixturePathResolver()).Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	for _, project := range roots {
		if project.Meta == nil || project.Meta[settings.MetaRemote] == "" {
			t.Fatalf("project root %q carries no git-remote meta; got %v", project.ID, project.Meta)
		}
		if project.Label != "github:acme/tool" {
			t.Fatalf("project root label = %q, want the projectlabel.Label form %q", project.Label, "github:acme/tool")
		}
		if project.ID == string(defaults.HarnessClaudeCode) {
			t.Fatalf("a harness slug %q must not be a top-level root", project.ID)
		}
	}
}

// TestScannerTreeSource_MetaRoundTripsToProjectSelection proves the Meta keys the
// adapter writes are the ones settings.FromTreeNodes reads back: checking one
// project's whole subtree derives a ProjectSelection carrying that project's git
// remote (not a folder-name fallback), keyed under the harness recorded on the
// project's session leaves.
func TestScannerTreeSource_MetaRoundTripsToProjectSelection(t *testing.T) {
	t.Parallel()
	doc := loadListings(t)
	roots, err := kickstart.NewScannerTreeSource(doc.Listings, withFixturePathResolver()).Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	// Check the first project's whole subtree, leave the rest unchecked, and
	// derive the selection.
	project := roots[0]
	checkSubtree(project)
	for _, r := range roots {
		rollup(r)
	}

	sel := settings.FromTreeNodes(roots)
	if sel.Mode != config.SelectionModeSelected {
		t.Fatalf("mode = %q, want selected", sel.Mode)
	}
	// The first project (lexicographically) is the claude-code remote.
	hc, ok := sel.Harnesses[string(defaults.HarnessClaudeCode)]
	if !ok {
		t.Fatalf("no harness entry for claude-code; got %v", sel.Harnesses)
	}
	if len(hc.Projects) != 1 {
		t.Fatalf("expected one project selection, got %d", len(hc.Projects))
	}
	if hc.Projects[0].GitRemote != project.Meta[settings.MetaRemote] {
		t.Fatalf("project git remote = %q, want %q (from node meta)",
			hc.Projects[0].GitRemote, project.Meta[settings.MetaRemote])
	}
	if hc.Projects[0].Branches != nil {
		t.Fatalf("a wholly-checked project must carry nil Branches (all branches), got %v", hc.Projects[0].Branches)
	}
}

func TestScannerTreeSource_UsesPhysicalCloneIdentityAndCompleteMultiplicity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cloneA := filepath.Join(root, "team-a", "tool")
	cloneB := filepath.Join(root, "team-b", "tool")
	for _, directory := range []string{cloneA, cloneB} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create clone directory %q: %v", directory, err)
		}
	}
	aliasA := filepath.Join(root, "tool-alias")
	if err := os.Symlink(cloneA, aliasA); err != nil {
		t.Fatalf("create clone symlink: %v", err)
	}

	listings := []ftue.SessionListing{
		{Harness: "claude-code", ProjectName: "tool", GitRemote: "git@github.com:acme/tool.git", Branch: "main", SessionID: "physical-a", WorkingDir: cloneA},
		{Harness: "claude-code", ProjectName: "tool", GitRemote: "git@github.com:acme/tool.git", Branch: "main", SessionID: "symlink-a", WorkingDir: aliasA},
		{Harness: "claude-code", ProjectName: "tool", GitRemote: "git@github.com:acme/tool.git", Branch: "release", SessionID: "physical-b", WorkingDir: cloneB},
	}
	roots, err := kickstart.NewScannerTreeSource(
		listings,
		kickstart.WithPathIdentityResolver(ingest.NewPhysicalPathResolver()),
	).Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("physical clone roots = %d, want 2", len(roots))
	}

	byPath := map[string]*kit.TreeNode{}
	for _, project := range roots {
		clonePath := project.Meta[settings.MetaClonePath]
		byPath[clonePath] = project
		wantID := (kickstart.ProjectIdentity{Harness: ingest.Harness("claude-code"), ClonePath: ingest.ClonePath(clonePath)}).String()
		if project.ID != wantID || project.Meta[settings.MetaProjectIdentity] != wantID {
			t.Fatalf("project identity = id %q meta %q, want %q", project.ID, project.Meta[settings.MetaProjectIdentity], wantID)
		}
		if project.Label != "github:acme/tool" {
			t.Fatalf("remote project label = %q, want github:acme/tool", project.Label)
		}
		if strings.Contains(project.Label, root) {
			t.Fatalf("project label leaked full physical path %q: %q", root, project.Label)
		}
	}
	if len(byPath[cloneA].Children[0].Children) != 2 {
		t.Fatalf("physical and symlink spellings did not collapse into one clone root")
	}
	for _, project := range roots {
		for _, branch := range project.Children {
			for _, session := range branch.Children {
				if session.Meta[settings.MetaRemoteMultiplicity] != settings.MetaMultiplicityAmbiguous {
					t.Errorf("session %q remote multiplicity = %q, want ambiguous", session.ID, session.Meta[settings.MetaRemoteMultiplicity])
				}
				if session.Meta[settings.MetaNameMultiplicity] != settings.MetaMultiplicityAmbiguous {
					t.Errorf("session %q name multiplicity = %q, want ambiguous", session.ID, session.Meta[settings.MetaNameMultiplicity])
				}
			}
		}
	}
}

func TestScannerTreeSource_NonGitLabelsUseOnlyShortPathContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cloneA := filepath.Join(root, "team-a", "app")
	cloneB := filepath.Join(root, "team-b", "app")
	for _, directory := range []string{cloneA, cloneB} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create clone directory %q: %v", directory, err)
		}
	}
	listings := []ftue.SessionListing{
		{Harness: "claude-code", ProjectName: "app", Branch: "main", SessionID: "app-a", WorkingDir: cloneA},
		{Harness: "claude-code", ProjectName: "app", Branch: "main", SessionID: "app-b", WorkingDir: cloneB},
	}
	roots, err := kickstart.NewScannerTreeSource(listings).Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("non-Git duplicate-name roots = %d, want 2", len(roots))
	}
	labels := map[string]bool{}
	for _, project := range roots {
		labels[project.Label] = true
		if strings.Contains(project.Label, root) {
			t.Fatalf("non-Git label leaked full physical path %q: %q", root, project.Label)
		}
		session := project.Children[0].Children[0]
		if session.Meta[settings.MetaRemoteMultiplicity] != settings.MetaMultiplicityUnique {
			t.Errorf("empty remote multiplicity = %q, want inert explicit unique", session.Meta[settings.MetaRemoteMultiplicity])
		}
		if session.Meta[settings.MetaNameMultiplicity] != settings.MetaMultiplicityAmbiguous {
			t.Errorf("duplicate project name multiplicity = %q, want ambiguous", session.Meta[settings.MetaNameMultiplicity])
		}
	}
	for _, label := range []string{"app (team-a/app)", "app (team-b/app)"} {
		if !labels[label] {
			t.Errorf("missing short-path label %q; got %v", label, labels)
		}
	}
}

func TestScannerTreeSource_NonGitLabelsExtendSuffixToStayDistinct(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cloneA := filepath.Join(root, "team-a", "shared", "app")
	cloneB := filepath.Join(root, "team-b", "shared", "app")
	for _, directory := range []string{cloneA, cloneB} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create clone directory %q: %v", directory, err)
		}
	}
	listings := []ftue.SessionListing{
		{Harness: "claude-code", ProjectName: "app", Branch: "main", SessionID: "deep-app-a", WorkingDir: cloneA},
		{Harness: "claude-code", ProjectName: "app", Branch: "main", SessionID: "deep-app-b", WorkingDir: cloneB},
	}
	roots, err := kickstart.NewScannerTreeSource(listings).Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("non-Git duplicate-tail roots = %d, want 2", len(roots))
	}
	labels := map[string]bool{}
	for _, project := range roots {
		labels[project.Label] = true
		if strings.Contains(project.Label, root) {
			t.Fatalf("non-Git label leaked full physical path %q: %q", root, project.Label)
		}
	}
	for _, label := range []string{"app (team-a/shared/app)", "app (team-b/shared/app)"} {
		if !labels[label] {
			t.Errorf("missing distinct short-path label %q; got %v", label, labels)
		}
	}
}

func TestScannerTreeSource_ProjectIdentityIncludesHarness(t *testing.T) {
	t.Parallel()
	if got := (kickstart.ProjectIdentity{ClonePath: ingest.ClonePath("/fixtures/tool")}).String(); got != "" {
		t.Fatalf("project identity without a harness = %q, want unavailable", got)
	}
	clone := filepath.Join(t.TempDir(), "tool")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatalf("create shared clone directory: %v", err)
	}
	remote := "git@github.com:acme/tool.git"
	listings := []ftue.SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "tool", GitRemote: remote, Branch: "main", SessionID: "session-claude-code", WorkingDir: clone},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "tool", GitRemote: remote, Branch: "main", SessionID: "session-open-code", WorkingDir: clone},
	}
	roots, err := kickstart.NewScannerTreeSource(listings).Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	if len(roots) != 2 || roots[0].ID == roots[1].ID {
		t.Fatalf("same physical path across two harnesses produced identities %q and %q", roots[0].ID, roots[1].ID)
	}
	for _, project := range roots {
		for _, branch := range project.Children {
			for _, session := range branch.Children {
				if session.Meta[settings.MetaRemoteMultiplicity] != settings.MetaMultiplicityUnique {
					t.Errorf("session %q remote multiplicity = %q, want unique within its harness", session.ID, session.Meta[settings.MetaRemoteMultiplicity])
				}
			}
		}
	}
}

func TestScannerTreeSource_UnresolvedPathCannotEnableRemoteFallback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	available := filepath.Join(root, "available", "tool")
	if err := os.MkdirAll(available, 0o755); err != nil {
		t.Fatalf("create available clone: %v", err)
	}
	missing := filepath.Join(root, "missing", "tool")
	remote := "git@github.com:acme/tool.git"
	listings := []ftue.SessionListing{
		{Harness: "claude-code", ProjectName: "tool", GitRemote: remote, Branch: "main", SessionID: "available", WorkingDir: available},
		{Harness: "claude-code", ProjectName: "tool", GitRemote: remote, Branch: "main", SessionID: "missing", WorkingDir: missing},
	}
	roots, err := kickstart.NewScannerTreeSource(listings).Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("available project roots = %d, want 1", len(roots))
	}
	session := roots[0].Children[0].Children[0]
	if session.Meta[settings.MetaRemoteMultiplicity] != settings.MetaMultiplicityAmbiguous {
		t.Fatalf("remote multiplicity = %q, want ambiguous because one cohort path is unresolved", session.Meta[settings.MetaRemoteMultiplicity])
	}

	remoteOnly := config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			"claude-code": {Projects: []config.ProjectSelection{{GitRemote: remote}}},
		},
	}
	unmatched := settings.PrepopulateSelection(roots, remoteOnly)
	if session.State != kit.Unchecked {
		t.Fatalf("remote-only selection guessed the available clone: state=%v", session.State)
	}
	if len(unmatched.Harnesses["claude-code"].Projects) != 1 {
		t.Fatalf("remote-only saved choice was not preserved: %#v", unmatched)
	}

	exact := remoteOnly
	exact.Harnesses = map[string]config.SelectionHarnessConfig{
		"claude-code": {Projects: []config.ProjectSelection{{GitRemote: remote, ClonePaths: []string{available}}}},
	}
	settings.PrepopulateSelection(roots, exact)
	if session.State != kit.Checked {
		t.Fatalf("exact available clone path did not restore: state=%v", session.State)
	}
}

type recordingPathResolver struct {
	calls []string
}

func (r *recordingPathResolver) Resolve(dir string) (ingest.ClonePath, error) {
	r.calls = append(r.calls, dir)
	return ingest.ClonePath(filepath.Clean(dir)), nil
}

func TestScannerTreeSource_ResolvesEveryNonEmptyWorkingDirectory(t *testing.T) {
	resolver := &recordingPathResolver{}
	listings := []ftue.SessionListing{
		{Harness: "claude-code", ProjectName: "tool", WorkingDir: "/fixtures/a/tool", SessionID: "a"},
		{Harness: "claude-code", ProjectName: "tool", WorkingDir: "/fixtures/a/tool", SessionID: "b"},
		{Harness: "claude-code", ProjectName: "unknown", SessionID: "without-path"},
	}
	_, err := kickstart.NewScannerTreeSource(listings, kickstart.WithPathIdentityResolver(resolver)).Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	if len(resolver.calls) != 2 || resolver.calls[0] != listings[0].WorkingDir || resolver.calls[1] != listings[1].WorkingDir {
		t.Fatalf("resolver calls = %v, want every non-empty working directory in cohort order", resolver.calls)
	}
}

var _ ingest.PathIdentityResolver = (*recordingPathResolver)(nil)
