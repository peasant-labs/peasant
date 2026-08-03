package codemap_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/schema"
)

// TestMapGraph_RepoFound covers the full assembly path: parsed structure
// nodes + edges, activity-only node merge, coverage fill, touch counts,
// effort density, and the parsed-language list.
func TestMapGraph_RepoFound(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	payload, err := svc.MapGraph(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph: %v", err)
	}

	if !payload.RepoFound {
		t.Error("RepoFound = false, want true")
	}
	if payload.RepoPath != fxCwd {
		t.Errorf("RepoPath = %q, want %q", payload.RepoPath, fxCwd)
	}
	if want := []string{"go"}; !reflect.DeepEqual(payload.ParsedLanguages, want) {
		t.Errorf("ParsedLanguages = %v, want %v", payload.ParsedLanguages, want)
	}
	if payload.GeneratedAtMs == 0 {
		t.Error("GeneratedAtMs = 0, want a timestamp")
	}

	// Parsed structure: internal/a imports internal/b.
	foundStructure := false
	for _, e := range payload.StructureEdges {
		if e.From == "internal/a" && e.To == "internal/b" && e.Count == 1 {
			foundStructure = true
		}
	}
	if !foundStructure {
		t.Errorf("StructureEdges missing internal/a -> internal/b: %+v", payload.StructureEdges)
	}

	// Parsed node with coverage + touch metrics.
	nodeA := findNode(t, payload.Nodes, "internal/a")
	if nodeA.Kind != schema.MapNodeKindPackage {
		t.Errorf("internal/a Kind = %q, want %q", nodeA.Kind, schema.MapNodeKindPackage)
	}
	// a.go: 4 edit events (2 in session 1, 2 in session 2).
	if nodeA.TouchCount != 4 {
		t.Errorf("internal/a TouchCount = %d, want 4", nodeA.TouchCount)
	}
	if nodeA.TotalFiles != 1 || nodeA.RecordedFiles != 1 {
		t.Errorf("internal/a coverage = %d/%d, want 1/1", nodeA.RecordedFiles, nodeA.TotalFiles)
	}
	// Effort: a.go carries 1 re-edit + 1 error-adjacent edit = max score 2
	// => density 1.0 on its chain.
	if nodeA.EffortDensity != 1.0 {
		t.Errorf("internal/a EffortDensity = %v, want 1.0", nodeA.EffortDensity)
	}
	nodeB := findNode(t, payload.Nodes, "internal/b")
	if nodeB.EffortDensity != 0 {
		t.Errorf("internal/b EffortDensity = %v, want 0", nodeB.EffortDensity)
	}

	// internal module rolls up both packages: a.go + b.go recorded.
	internal := findNode(t, payload.Nodes, "internal")
	if internal.Kind != schema.MapNodeKindModule {
		t.Errorf("internal Kind = %q, want %q", internal.Kind, schema.MapNodeKindModule)
	}
	if internal.TotalFiles != 2 || internal.RecordedFiles != 2 {
		t.Errorf("internal coverage = %d/%d, want 2/2", internal.RecordedFiles, internal.TotalFiles)
	}

	// Activity-only merge: docs/notes.md (edited, unparsed) gets a node, and
	// its docs group lands on a layer after every parsed layer.
	notes := findNode(t, payload.Nodes, "docs/notes.md")
	if notes.Kind != schema.MapNodeKindFile {
		t.Errorf("docs/notes.md Kind = %q, want %q", notes.Kind, schema.MapNodeKindFile)
	}
	docs := findNode(t, payload.Nodes, "docs")
	maxParsed := 0
	for _, n := range payload.Nodes {
		if n.ID == "internal" || n.ID == "internal/a" || n.ID == "internal/b" {
			if n.Layer > maxParsed {
				maxParsed = n.Layer
			}
		}
	}
	if docs.Layer <= maxParsed {
		t.Errorf("docs Layer = %d, want > max parsed layer %d", docs.Layer, maxParsed)
	}
	// docs universe: readme.md (tracked, unedited) + notes.md (edited,
	// untracked) — neither is recorded under the repo-present rule.
	if docs.TotalFiles != 2 || docs.RecordedFiles != 0 {
		t.Errorf("docs coverage = %d/%d, want 0/2", docs.RecordedFiles, docs.TotalFiles)
	}

	// The out-of-repo edit (/elsewhere/x.go) must not produce a node.
	for _, n := range payload.Nodes {
		if n.ID == "elsewhere" || n.ID == "elsewhere/x.go" || n.ID == "/elsewhere/x.go" {
			t.Errorf("out-of-repo edit produced node %q", n.ID)
		}
	}

	assertNoNullArrays(t, payload)
}

// TestMapGraph_RootShelfLayer: every activity-only subtree with no parsed
// ancestor (root-level unparsed files like AGENTS.md/CLAUDE.md, unparsed
// top-level dirs like llm/) lands on ONE shared "root shelf" layer directly
// below the deepest parsed layer — never on per-group layers that exile each
// root file to its own distant row.
func TestMapGraph_RootShelfLayer(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	base := fxBase()

	// A session editing root-level unparsed files plus a file under an
	// unparsed top-level directory.
	seedSession(t, s, fxSession3, "", base+7000, base+8000)
	seedEntries(t, s, fxSession3, []entrySpec{
		userTurn(base+7000, "Update the agent instructions and design notes"),
		toolUse(base+7100, "Edit", fxCwd+"/AGENTS.md"),
		toolUse(base+7200, "Edit", fxCwd+"/CLAUDE.md"),
		toolUse(base+7300, "Write", fxCwd+"/llm/notes.md"),
	})

	payload, err := svc.MapGraph(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph: %v", err)
	}

	// Deepest parsed layer = max layer among the parsed structure nodes.
	activityOnly := map[string]bool{
		"AGENTS.md": true, "CLAUDE.md": true,
		"llm": true, "llm/notes.md": true,
		"docs": true, "docs/notes.md": true,
	}
	maxParsed := 0
	for _, n := range payload.Nodes {
		if !activityOnly[n.ID] && n.Layer > maxParsed {
			maxParsed = n.Layer
		}
	}
	shelf := maxParsed + 1

	for id := range activityOnly {
		if got := findNode(t, payload.Nodes, id).Layer; got != shelf {
			t.Errorf("%s Layer = %d, want shared root shelf %d", id, got, shelf)
		}
	}
	// Dense numbering: nothing may sit beyond the shelf (no sparse jumps).
	for _, n := range payload.Nodes {
		if n.Layer > shelf {
			t.Errorf("node %s Layer = %d beyond root shelf %d", n.ID, n.Layer, shelf)
		}
	}
}

// TestMapGraph_ActivityEdgeFloor: the co-edit pair (internal/a, internal/b)
// is shared by two tasks and becomes an edge; pairs seen once (docs with
// either package) stay below the floor.
func TestMapGraph_ActivityEdgeFloor(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	payload, err := svc.MapGraph(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph: %v", err)
	}

	want := []schema.ActivityEdge{{From: "internal/a", To: "internal/b", TaskCount: 2}}
	if !reflect.DeepEqual(payload.ActivityEdges, want) {
		t.Errorf("ActivityEdges = %+v, want %+v", payload.ActivityEdges, want)
	}
}

// TestMapGraph_RepoMissing: a known project whose cwd is not a git repo
// degrades to an activity-only graph (RepoFound=false, no structure), with
// fallback coverage = "has any recorded edit".
func TestMapGraph_RepoMissing(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, noRepo())

	payload, err := svc.MapGraph(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph: %v", err)
	}

	if payload.RepoFound {
		t.Error("RepoFound = true, want false")
	}
	if payload.RepoPath != "" {
		t.Errorf("RepoPath = %q, want empty", payload.RepoPath)
	}
	if len(payload.ParsedLanguages) != 0 || len(payload.StructureEdges) != 0 || len(payload.Violations) != 0 {
		t.Errorf("structure data on repo-missing payload: langs=%v edges=%d viols=%d",
			payload.ParsedLanguages, len(payload.StructureEdges), len(payload.Violations))
	}

	// Fallback coverage: every edited file is recorded; the universe is the
	// edited files alone (no readme.md, no go.mod).
	nodeA := findNode(t, payload.Nodes, "internal/a/a.go")
	if nodeA.TotalFiles != 1 || nodeA.RecordedFiles != 1 {
		t.Errorf("a.go fallback coverage = %d/%d, want 1/1", nodeA.RecordedFiles, nodeA.TotalFiles)
	}
	docs := findNode(t, payload.Nodes, "docs")
	if docs.TotalFiles != 1 || docs.RecordedFiles != 1 {
		t.Errorf("docs fallback coverage = %d/%d, want 1/1", docs.RecordedFiles, docs.TotalFiles)
	}
	for _, n := range payload.Nodes {
		if n.ID == "go.mod" || n.ID == "docs/readme.md" {
			t.Errorf("tracked-only file %q present without a repo", n.ID)
		}
	}

	// Activity edges still served.
	if len(payload.ActivityEdges) != 1 {
		t.Errorf("ActivityEdges = %+v, want 1 edge", payload.ActivityEdges)
	}

	assertNoNullArrays(t, payload)
}

// TestMapGraph_UnknownProject: unknown hash is an error (API maps to 404).
func TestMapGraph_UnknownProject(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	_, err := svc.MapGraph(context.Background(), schema.ProjectHash("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"), "")
	if !errors.Is(err, codemap.ErrProjectNotFound) {
		t.Errorf("MapGraph(unknown) error = %v, want ErrProjectNotFound", err)
	}
}

// TestMapGraph_AtCommit: the ?commit= ref is echoed and used as the read ref.
func TestMapGraph_AtCommit(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	payload, err := svc.MapGraph(context.Background(), fxProjectHash, fxBranch)
	if err != nil {
		t.Fatalf("MapGraph(at %s): %v", fxBranch, err)
	}
	if payload.AtCommit != fxBranch {
		t.Errorf("AtCommit = %q, want %q", payload.AtCommit, fxBranch)
	}
	// The branch ref includes internal/c (absent at HEAD).
	findNode(t, payload.Nodes, "internal/c")
}

// TestMapGraph_Deterministic: identical inputs produce identical payloads
// (GeneratedAtMs excepted — it is the only clock-derived field).
func TestMapGraph_Deterministic(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	first, err := svc.MapGraph(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph #1: %v", err)
	}
	second, err := svc.MapGraph(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph #2: %v", err)
	}
	first.GeneratedAtMs = 0
	second.GeneratedAtMs = 0
	if !reflect.DeepEqual(first, second) {
		t.Errorf("MapGraph not deterministic:\nfirst:  %+v\nsecond: %+v", first, second)
	}
}
