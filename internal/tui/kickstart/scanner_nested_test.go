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

// nestedListingsDoc mirrors the parent/child discovery fixture. It carries
// row-count guards so a fixture edit that adds or drops a row without updating
// the expected counts fails loudly.
type nestedListingsDoc struct {
	Name                    string                `yaml:"name"`
	ExpectedListingCount    int                   `yaml:"expectedListingCount"`
	ExpectedTopSessionCount int                   `yaml:"expectedTopSessionCount"`
	Ingested                []string              `yaml:"ingested"`
	Listings                []ftue.SessionListing `yaml:"listings"`
}

func loadNestedListings(t *testing.T) nestedListingsDoc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "nested_listings.yaml"))
	if err != nil {
		t.Fatalf("read nested listings fixture: %v", err)
	}
	var doc nestedListingsDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode nested listings fixture: %v", err)
	}
	if doc.ExpectedListingCount != len(doc.Listings) {
		t.Fatalf("expectedListingCount=%d but %d listings present", doc.ExpectedListingCount, len(doc.Listings))
	}
	return doc
}

func nestedForest(t *testing.T) ([]*kit.TreeNode, nestedListingsDoc) {
	t.Helper()
	doc := loadNestedListings(t)
	src := kickstart.NewScannerTreeSource(doc.Listings, withFixturePathResolver(), kickstart.WithIngestedSessionIDs(doc.Ingested))
	roots, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("scanner load: %v", err)
	}
	return roots, doc
}

// TestScannerTreeSource_ParentOnlyTopSessions proves a session whose id appears
// in another session's SubagentIDs is NOT a top-level session under a branch:
// only the two parent sessions group into the branch, while the child and
// grandchild are absent from the branch's direct children.
func TestScannerTreeSource_ParentOnlyTopSessions(t *testing.T) {
	t.Parallel()
	roots, doc := nestedForest(t)
	if len(roots) != 1 {
		t.Fatalf("want one project root, got %d", len(roots))
	}
	project := roots[0]
	if len(project.Children) != 1 {
		t.Fatalf("want one branch, got %d", len(project.Children))
	}
	branch := project.Children[0]
	if len(branch.Children) != doc.ExpectedTopSessionCount {
		t.Fatalf("branch top sessions = %d, want %d", len(branch.Children), doc.ExpectedTopSessionCount)
	}
	for _, top := range branch.Children {
		if top.ID == "sess-child" || top.ID == "sess-grandchild" {
			t.Fatalf("subagent session %q must not be a top-level branch child", top.ID)
		}
	}
}

// TestScannerTreeSource_ParentSummarisesSubagents proves a parent session stays
// a LEAF whose row carries the transitive subagent count, so the subagent chain
// costs one annotation rather than two more levels of rows. Every top session
// still carries its harness in Meta.
func TestScannerTreeSource_ParentSummarisesSubagents(t *testing.T) {
	t.Parallel()
	roots, _ := nestedForest(t)
	branch := roots[0].Children[0]
	parent := findRoot(branch.Children, "sess-parent")
	if parent == nil {
		t.Fatal("parent session not found under branch")
	}
	if len(parent.Children) != 0 {
		t.Fatalf("parent session must be a leaf, got %d children", len(parent.Children))
	}
	// The fixture nests a child that itself spawned a grandchild, so the count
	// is the whole descendant chain, not just the direct subagent.
	if got := parent.Meta[settings.MetaChildCount]; got != "2" {
		t.Fatalf("parent child count = %q, want %q", got, "2")
	}
	second := findRoot(branch.Children, "sess-p2")
	if second == nil {
		t.Fatal("second parent session not found under branch")
	}
	if got, ok := second.Meta[settings.MetaChildCount]; ok {
		t.Fatalf("a session with no subagents must carry no count, got %q", got)
	}
	for _, node := range []*kit.TreeNode{parent, second} {
		if node.Meta[settings.MetaHarness] != string(defaults.HarnessClaudeCode) {
			t.Fatalf("session %q harness meta = %q, want %q", node.ID, node.Meta[settings.MetaHarness], defaults.HarnessClaudeCode)
		}
	}
}

// TestScannerTreeSource_IngestedMark proves the scanner marks the sessions the
// store already holds and leaves the rest unmarked, and that an already-imported
// session sorts after the not-yet-imported ones. The fixture's ingested set
// names a subagent too (sess-child), which has no row of its own to mark.
func TestScannerTreeSource_IngestedMark(t *testing.T) {
	t.Parallel()
	roots, _ := nestedForest(t)
	rows := roots[0].Children[0].Children
	wantOrder := []string{"sess-parent", "sess-p2"}
	if len(rows) != len(wantOrder) {
		t.Fatalf("branch has %d session rows, want %d", len(rows), len(wantOrder))
	}
	ingested := map[string]bool{"sess-p2": true}
	for i, id := range wantOrder {
		node := rows[i]
		if node.ID != id {
			t.Fatalf("row %d = %q, want %q (not-yet-imported sessions come first)", i, node.ID, id)
		}
		got := node.Meta[settings.MetaIngested]
		if ingested[id] {
			if got != settings.MetaIngestedValue {
				t.Fatalf("session %q ingested mark = %q, want %q", id, got, settings.MetaIngestedValue)
			}
		} else if got != "" {
			t.Fatalf("session %q must be unmarked, got ingested=%q", id, got)
		}
	}
}

// TestScannerTreeSource_ParentSessionRoundTrip proves a checked parent session
// round-trips through settings.FromTreeNodes to its own session id and is not
// mistaken for a branch-level pick. Its subagents are NOT in the tree, and are
// not in the persisted allowlist either: the ingest side expands a selected
// parent to its children from the same discovery listing.
func TestScannerTreeSource_ParentSessionRoundTrip(t *testing.T) {
	t.Parallel()
	roots, _ := nestedForest(t)
	branch := roots[0].Children[0]
	parent := findRoot(branch.Children, "sess-parent")
	checkSubtree(parent)
	for _, r := range roots {
		rollup(r)
	}

	sel := settings.FromTreeNodes(roots)
	if sel.Mode != config.SelectionModeSelected {
		t.Fatalf("mode = %q, want selected", sel.Mode)
	}
	hc, ok := sel.Harnesses[string(defaults.HarnessClaudeCode)]
	if !ok {
		t.Fatalf("no claude-code harness entry; got %v", sel.Harnesses)
	}
	if len(hc.Sessions) != 1 || hc.Sessions[0] != "sess-parent" {
		t.Fatalf("sessions = %v, want only the checked parent session", hc.Sessions)
	}
	if len(hc.Projects) != 0 {
		t.Fatalf("a partially-selected branch must not persist a project pick, got %v", hc.Projects)
	}
}
