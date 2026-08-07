package kickstart_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
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

func loadListings(t *testing.T) listingsDoc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "listings.yaml"))
	if err != nil {
		t.Fatalf("read listings fixture: %v", err)
	}
	var doc listingsDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode listings fixture: %v", err)
	}
	if doc.ExpectedListingCount != len(doc.Listings) {
		t.Fatalf("expectedListingCount=%d but %d listings present", doc.ExpectedListingCount, len(doc.Listings))
	}
	return doc
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
	src := kickstart.NewScannerTreeSource(doc.Listings)
	roots, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	p, b, s := forestCounts(roots)
	if p != doc.ExpectedProjectCount || b != doc.ExpectedBranchCount || s != doc.ExpectedSessionCount {
		t.Fatalf("forest shape = projects %d branches %d sessions %d; want %d/%d/%d",
			p, b, s, doc.ExpectedProjectCount, doc.ExpectedBranchCount, doc.ExpectedSessionCount)
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
	roots, err := kickstart.NewScannerTreeSource(doc.Listings).Load(context.Background())
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
	roots, err := kickstart.NewScannerTreeSource(doc.Listings).Load(context.Background())
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
