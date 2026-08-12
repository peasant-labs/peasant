package kickstart_test

import (
	_ "embed"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// This file is the kickstart rebuild's acceptance oracle. It drives the new
// selection round-trip - the same settings.FromTreeNodes derivation the rebuilt
// tree field uses, wrapped by kickstart.DeriveSelection's exact-current-list
// policy - and the atomic settings.Draft.Commit path over the same semantic
// scenarios the current onboarding wizard's
// goldens were captured from (internal/tui/ftue/testdata/equivalence). For every
// scenario the persisted config.SelectionConfig must be FIELD-EQUIVALENT
// (order-insensitive within lists, byte-layout-independent) to the captured
// golden, with the exact-current-clone-list transition measured against the
// divergence target instead.
//
// The scenario corpus is READ from the ftue fixtures rather than duplicated, so
// this oracle and the legacy-capture oracle can never drift: a golden edited on
// the wizard side is the golden this rebuild is measured against in the same run.

const (
	legacyGoldensRelPath      = "../ftue/testdata/equivalence/legacy_goldens.yaml"
	ratifiedDivergenceRelPath = "../ftue/testdata/equivalence/ratified_divergence.yaml"
	legacyGoldenFloor         = 5
	ratifiedDivergenceFloor   = 1
)

// --- scenario corpus (a read-only mirror of the ftue fixture schema) ----------

type equivalenceDoc struct {
	ExpectedScenarioCount int                   `yaml:"expectedScenarioCount"`
	Scenarios             []equivalenceScenario `yaml:"scenarios"`
}

type equivalenceScenario struct {
	Name                  string           `yaml:"name"`
	Doc                   string           `yaml:"doc"`
	Oracle                string           `yaml:"oracle"`
	WantImport            bool             `yaml:"wantImport"`
	AutoIngestNewBranches bool             `yaml:"autoIngestNewBranches"`
	Providers             []providerInput  `yaml:"providers"`
	Scopes                []scopeInput     `yaml:"scopes"`
	Golden                goldenSelection  `yaml:"golden"`
	RatifiedExpected      *goldenSelection `yaml:"ratifiedExpected,omitempty"`
}

type providerInput struct {
	Harness   string `yaml:"harness"`
	ImportAll bool   `yaml:"importAll"`
}

type scopeInput struct {
	Level    string           `yaml:"level"`
	Sessions []sessionListing `yaml:"sessions"`
}

type sessionListing struct {
	Harness   string `yaml:"harness"`
	GitRemote string `yaml:"gitRemote"`
	Branch    string `yaml:"branch"`
	SessionID string `yaml:"sessionId"`
}

type goldenSelection struct {
	Mode                  string                   `yaml:"mode"`
	AutoIngestNewBranches bool                     `yaml:"autoIngestNewBranches"`
	Harnesses             map[string]goldenHarness `yaml:"harnesses,omitempty"`
}

type goldenHarness struct {
	Projects []goldenProject `yaml:"projects,omitempty"`
	Sessions []string        `yaml:"sessions,omitempty"`
}

type goldenProject struct {
	GitRemote  string   `yaml:"gitRemote,omitempty"`
	Name       string   `yaml:"name,omitempty"`
	ClonePaths []string `yaml:"clonePaths,omitempty"`
	Branches   []string `yaml:"branches,omitempty"`
}

func (g goldenSelection) toConfig() config.SelectionConfig {
	var harnesses map[string]config.SelectionHarnessConfig
	if len(g.Harnesses) > 0 {
		harnesses = make(map[string]config.SelectionHarnessConfig, len(g.Harnesses))
		for name, h := range g.Harnesses {
			var projects []config.ProjectSelection
			for _, p := range h.Projects {
				projects = append(projects, config.ProjectSelection{
					GitRemote:  p.GitRemote,
					Name:       p.Name,
					ClonePaths: p.ClonePaths,
					Branches:   p.Branches,
				})
			}
			harnesses[name] = config.SelectionHarnessConfig{Projects: projects, Sessions: h.Sessions}
		}
	}
	return config.SelectionConfig{
		Mode:                  config.SelectionMode(g.Mode),
		AutoIngestNewBranches: g.AutoIngestNewBranches,
		Harnesses:             harnesses,
	}
}

func loadScenarios(t *testing.T, relPath string, floor int) []equivalenceScenario {
	t.Helper()
	data, err := os.ReadFile(relPath)
	if err != nil {
		t.Fatalf("read scenario corpus %q: %v", relPath, err)
	}
	var doc equivalenceDoc
	if err := decodeStrictFixture(data, &doc); err != nil {
		t.Fatalf("decode scenario corpus %q: %v", relPath, err)
	}
	if doc.ExpectedScenarioCount != len(doc.Scenarios) {
		t.Fatalf("%q: expectedScenarioCount=%d but %d scenarios present", relPath, doc.ExpectedScenarioCount, len(doc.Scenarios))
	}
	if len(doc.Scenarios) < floor {
		t.Fatalf("%q holds %d scenarios, below the floor of %d", relPath, len(doc.Scenarios), floor)
	}
	return doc.Scenarios
}

// --- the inventory forest the scenarios are checked over ----------------------

// loadInventory parses the canonical scan forest via the shared scannerfix loader
// contract - the SAME embedded-YAML-with-row-count-guard idiom the settings tree
// tests use - so the rebuild's oracle scans a captured shape rather than an
// inline forest. The fixture is authored in the kickstart package's testdata.
func loadInventory(t *testing.T) []*kit.TreeNode {
	t.Helper()
	roots, err := parseForest(inventoryData)
	if err != nil {
		t.Fatalf("parse inventory fixture: %v", err)
	}
	return roots
}

type fixtureNode struct {
	ID       string            `yaml:"id"`
	Label    string            `yaml:"label"`
	State    string            `yaml:"state"`
	Meta     map[string]string `yaml:"meta"`
	Children []fixtureNode     `yaml:"children"`
}

type fixtureDoc struct {
	Name              string        `yaml:"name"`
	ExpectedNodeCount int           `yaml:"expectedNodeCount"`
	Roots             []fixtureNode `yaml:"roots"`
}

//go:embed testdata/inventory.yaml
var inventoryData []byte

func parseForest(data []byte) ([]*kit.TreeNode, error) {
	var doc fixtureDoc
	if err := decodeStrictFixture(data, &doc); err != nil {
		return nil, err
	}
	count := 0
	var roots []*kit.TreeNode
	for _, r := range doc.Roots {
		roots = append(roots, toNode(r, &count))
	}
	if count != doc.ExpectedNodeCount || count == 0 {
		return nil, errCount(doc.ExpectedNodeCount, count)
	}
	return roots, nil
}

func errCount(want, got int) error {
	return &countError{want: want, got: got}
}

type countError struct{ want, got int }

func (e *countError) Error() string {
	return "inventory fixture node count mismatch"
}

func toNode(fn fixtureNode, n *int) *kit.TreeNode {
	*n++
	node := &kit.TreeNode{ID: fn.ID, Label: fn.Label, State: kit.Unchecked}
	if len(fn.Meta) > 0 {
		node.Meta = map[string]string{}
		for k, v := range fn.Meta {
			node.Meta[k] = v
		}
	}
	for _, c := range fn.Children {
		node.Children = append(node.Children, toNode(c, n))
	}
	return node
}

// applyScopes checks the session leaves each scope names, at its grain, then
// rolls interior node states up from the leaves - reproducing what the tree does
// as a user checks nodes, without driving the interactive component.
func applyScopes(roots []*kit.TreeNode, scopes []scopeInput) {
	for _, scope := range scopes {
		for _, s := range scope.Sessions {
			// The forest is PROJECT -> BRANCH -> SESSION (project-first, no
			// harness grouping level), so a scope resolves its project by git
			// remote directly at the root.
			project := findRoot(roots, s.GitRemote)
			if project == nil {
				continue
			}
			switch scope.Level {
			case "project":
				checkSubtree(project)
			case "branch":
				if wt := findChild(project, s.Branch); wt != nil {
					checkSubtree(wt)
				}
			case "session":
				if wt := findChild(project, s.Branch); wt != nil {
					if leaf := findChild(wt, s.SessionID); leaf != nil {
						leaf.State = kit.Checked
					}
				}
			}
		}
	}
	for _, r := range roots {
		rollup(r)
	}
}

func findRoot(roots []*kit.TreeNode, id string) *kit.TreeNode {
	for _, r := range roots {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func findChild(n *kit.TreeNode, id string) *kit.TreeNode {
	for _, c := range n.Children {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func checkSubtree(n *kit.TreeNode) {
	n.State = kit.Checked
	for _, c := range n.Children {
		checkSubtree(c)
	}
}

// rollup mirrors the kit tree / settings rollup: an interior node is Checked when
// all children Checked, Unchecked when all Unchecked, else Partial.
func rollup(n *kit.TreeNode) kit.TriState {
	if len(n.Children) == 0 {
		return n.State
	}
	var checked, unchecked, other int
	for _, c := range n.Children {
		switch rollup(c) {
		case kit.Checked:
			checked++
		case kit.Unchecked:
			unchecked++
		default:
			other++
		}
	}
	switch {
	case other > 0:
		n.State = kit.Partial
	case checked > 0 && unchecked == 0:
		n.State = kit.Checked
	case checked == 0 && unchecked > 0:
		n.State = kit.Unchecked
	default:
		n.State = kit.Partial
	}
	return n.State
}

// --- field-equivalence (order-insensitive) ------------------------------------

func normalizeSelection(sel config.SelectionConfig) config.SelectionConfig {
	out := config.SelectionConfig{Mode: sel.Mode, AutoIngestNewBranches: sel.AutoIngestNewBranches}
	if len(sel.Harnesses) == 0 {
		return out
	}
	out.Harnesses = make(map[string]config.SelectionHarnessConfig, len(sel.Harnesses))
	for name, h := range sel.Harnesses {
		projects := append([]config.ProjectSelection(nil), h.Projects...)
		for i := range projects {
			clonePaths := append([]string(nil), projects[i].ClonePaths...)
			sort.Strings(clonePaths)
			if len(clonePaths) == 0 {
				clonePaths = nil
			}
			projects[i].ClonePaths = clonePaths
			branches := append([]string(nil), projects[i].Branches...)
			sort.Strings(branches)
			if len(branches) == 0 {
				branches = nil
			}
			projects[i].Branches = branches
		}
		sort.Slice(projects, func(i, j int) bool {
			if projects[i].GitRemote != projects[j].GitRemote {
				return projects[i].GitRemote < projects[j].GitRemote
			}
			if projects[i].Name != projects[j].Name {
				return projects[i].Name < projects[j].Name
			}
			return strings.Join(projects[i].ClonePaths, "\x00") < strings.Join(projects[j].ClonePaths, "\x00")
		})
		sessions := append([]string(nil), h.Sessions...)
		sort.Strings(sessions)
		if len(projects) == 0 {
			projects = nil
		}
		if len(sessions) == 0 {
			sessions = nil
		}
		out.Harnesses[name] = config.SelectionHarnessConfig{Projects: projects, Sessions: sessions}
	}
	return out
}

func assertFieldEquivalent(t *testing.T, scenario string, got, want config.SelectionConfig) {
	t.Helper()
	if !reflect.DeepEqual(normalizeSelection(got), normalizeSelection(want)) {
		t.Errorf("scenario %q: persisted selection is not field-equivalent to the golden\n got  = %#v\n want = %#v",
			scenario, got, want)
	}
}

func withoutClonePaths(selection config.SelectionConfig) config.SelectionConfig {
	copy := normalizeSelection(selection)
	for harness, configured := range copy.Harnesses {
		for index := range configured.Projects {
			configured.Projects[index].ClonePaths = nil
		}
		copy.Harnesses[harness] = configured
	}
	return copy
}

// --- the oracle ---------------------------------------------------------------

// commitSelection drives the rebuilt kickstart's persistence path end-to-end: it
// derives the SelectionConfig from the checked forest via kickstart.DeriveSelection
// (the tree field's own round-trip plus the exact-current-list policy), writes it
// into a settings.Draft over a real on-disk config, commits atomically, and
// re-reads the file. The returned value is exactly
// what a user's config.yaml would hold after completing kickstart.
func commitSelection(t *testing.T, roots []*kit.TreeNode, autoIngest bool) config.SelectionConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	base := config.BaseConfig()
	if err := config.SaveAtomic(path, base); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	loaded, err := config.Parse(mustRead(t, path))
	if err != nil {
		t.Fatalf("parse seeded config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	draft.Working().Selection = kickstart.DeriveSelection(roots, autoIngest)
	if err := draft.Commit(); err != nil {
		t.Fatalf("commit draft: %v", err)
	}
	final, err := config.Parse(mustRead(t, path))
	if err != nil {
		t.Fatalf("re-parse committed config: %v", err)
	}
	return final.Selection
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return data
}

// TestKickstartEquivalence_LegacyGoldens proves every pre-existing selection
// field remains equivalent after removing the one additive identity field. Any
// project selection must also carry a clone path; explicit-session-only rows do
// not invent a project.
func TestKickstartEquivalence_LegacyGoldens(t *testing.T) {
	t.Parallel()
	scenarios := loadScenarios(t, legacyGoldensRelPath, legacyGoldenFloor)
	for _, scenario := range scenarios {
		scenario := scenario
		if scenario.Oracle != "legacy-captured" {
			t.Fatalf("scenario %q is a %q row in the legacy golden file", scenario.Name, scenario.Oracle)
		}
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()
			roots := loadInventory(t)
			applyScopes(roots, scenario.Scopes)
			got := commitSelection(t, roots, scenario.AutoIngestNewBranches)
			assertFieldEquivalent(t, scenario.Name, withoutClonePaths(got), scenario.Golden.toConfig())
			for harness, configured := range got.Harnesses {
				for _, project := range configured.Projects {
					if len(project.ClonePaths) == 0 {
						t.Errorf("scenario %q project for %q has no physical clone path: %#v", scenario.Name, harness, project)
					}
				}
			}
		})
	}
}

// TestKickstartEquivalence_RatifiedDivergence pins the physical-identity change:
// selecting every current project remains mode:selected but includes each exact
// resolver-produced clone path. The legacy wizard's pathless golden stays as the
// non-vacuity control.
func TestKickstartEquivalence_RatifiedDivergence(t *testing.T) {
	t.Parallel()
	scenarios := loadScenarios(t, ratifiedDivergenceRelPath, ratifiedDivergenceFloor)
	for _, scenario := range scenarios {
		scenario := scenario
		if scenario.Oracle != "ratified-divergence" {
			t.Fatalf("scenario %q is a %q row in the ratified divergence file", scenario.Name, scenario.Oracle)
		}
		if scenario.RatifiedExpected == nil {
			t.Fatalf("scenario %q sets no ratifiedExpected", scenario.Name)
		}
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()
			roots := loadInventory(t)
			applyScopes(roots, scenario.Scopes)
			got := commitSelection(t, roots, scenario.AutoIngestNewBranches)

			target := scenario.RatifiedExpected.toConfig()
			assertFieldEquivalent(t, scenario.Name, got, target)

			if target.Mode != config.SelectionModeSelected {
				t.Errorf("exact-list target mode = %q, want %q", target.Mode, config.SelectionModeSelected)
			}
			for harness, configured := range target.Harnesses {
				if len(configured.Projects) != 1 || len(configured.Projects[0].ClonePaths) != 1 {
					t.Errorf("exact-list target for %q lacks one physical clone path: %#v", harness, configured)
				}
			}
			if reflect.DeepEqual(normalizeSelection(got), normalizeSelection(scenario.Golden.toConfig())) {
				t.Fatalf("the ratified divergence is vacuous: the rebuild emitted the legacy golden, not the ratified target")
			}
		})
	}
}

// TestKickstartEscWritesNothing pins the ratified esc semantics: pressing esc in
// the mounted flow opens a confirm-exit modal, and a CONFIRMED exit writes
// NOTHING - the on-disk config bytes are unchanged even though the draft was
// dirtied first. It drives the real settings.Flow the kickstart mounts.
func TestKickstartEscWritesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	before := mustRead(t, path)

	loaded, err := config.Parse(before)
	if err != nil {
		t.Fatalf("parse seeded config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	// Dirty the draft so the test proves the exit discards a real pending change.
	draft.Working().Push.License = config.License("CC0-1.0")
	if !draft.Dirty() {
		t.Fatal("expected the draft to be dirty after an edit")
	}

	th := theme.New(theme.ModeDark)
	reg := kickstart.BuildRegistry(kickstart.Options{Source: scannerfix.NewFixtureTreeSource("standard")})
	flow := settings.NewFlow(th, reg, draft)
	flow.SetSize(80, 24)

	flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !flow.Confirming() {
		t.Fatal("esc did not open the confirm-exit modal")
	}
	// Move the highlight onto "yes" and submit it.
	flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	flow, cmd := flow.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !flow.Exited() {
		t.Fatal("confirming the exit modal did not mark the flow exited")
	}
	if flow.Committed() {
		t.Fatal("a confirmed exit must not commit")
	}
	_ = runQuit(cmd)

	after := mustRead(t, path)
	if string(before) != string(after) {
		t.Fatalf("confirmed exit changed the config file bytes\n before=%q\n after =%q", before, after)
	}
}

// runQuit drains a returned command far enough to observe it is the quit command
// (or nil), matching how the bubbletea program would consume it.
func runQuit(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}
