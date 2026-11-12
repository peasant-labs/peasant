package onboarding_test

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/restoration.yaml
var restorationData []byte

type restorationBranch struct {
	ClonePath string `yaml:"clonePath"`
	Branch    string `yaml:"branch"`
}

type restorationListing struct {
	Harness     string `yaml:"harness"`
	ProjectName string `yaml:"projectName"`
	GitRemote   string `yaml:"gitRemote"`
	WorkingDir  string `yaml:"workingDir"`
	Branch      string `yaml:"branch"`
	Title       string `yaml:"title"`
	SessionID   string `yaml:"sessionId"`
}

type restorationCase struct {
	Name                   string                 `yaml:"name"`
	Check                  string                 `yaml:"check"`
	Listings               []restorationListing   `yaml:"listings"`
	Saved                  config.SelectionConfig `yaml:"saved"`
	ExpectInitiallyChecked []string               `yaml:"expectInitiallyChecked"`
	ClearProjectPaths      []string               `yaml:"clearProjectPaths"`
	SelectProjectPaths     []string               `yaml:"selectProjectPaths"`
	ClearBranches          []restorationBranch    `yaml:"clearBranches"`
	SelectBranches         []restorationBranch    `yaml:"selectBranches"`
	SelectAll              bool                   `yaml:"selectAll"`
	LaterListings          []restorationListing   `yaml:"laterListings"`
	// PreviousListings is the machine the answer was given over; Listings is
	// what the rescan found. The check reads dropped labels from the former.
	PreviousListings    []restorationListing   `yaml:"previousListings"`
	Answered            []string               `yaml:"answered"`
	ExpectKept          []string               `yaml:"expectKept"`
	ExpectDroppedCount  int                    `yaml:"expectDroppedCount"`
	ExpectDroppedLabels []string               `yaml:"expectDroppedLabels"`
	ExpectLaterChecked  []string               `yaml:"expectLaterChecked"`
	Expected            config.SelectionConfig `yaml:"expected"`
}

type restorationDocument struct {
	RequiredNames     []string          `yaml:"requiredNames"`
	ExpectedCaseCount int               `yaml:"expectedCaseCount"`
	Cases             []restorationCase `yaml:"cases"`
}

func TestRestoration(t *testing.T) {
	t.Parallel()
	var document restorationDocument
	decodeFixture(t, "restoration.yaml", restorationData, &document)
	if document.ExpectedCaseCount != len(document.Cases) {
		t.Fatalf("expectedCaseCount=%d but %d cases present", document.ExpectedCaseCount, len(document.Cases))
	}
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)
	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			switch row.Check {
			case "corpus":
				assertEquivalenceCorpus(t)
				return
			case "locked":
				assertLockedBlocksCommit(t)
				return
			case "rescan":
				assertRescanKeepsIDs(t, row)
				return
			}
			got, residual := restoreAndDerive(t, row)
			if row.Check == "residual" {
				saved := residual.Harnesses["claude-code"]
				if !reflect.DeepEqual(saved.Sessions, []string{"gone-session"}) {
					t.Fatalf("residual sessions = %#v, want gone-session", saved.Sessions)
				}
				if len(saved.Projects) != 1 || saved.Projects[0].GitRemote != "git@github.com:acme/ghost.git" ||
					!reflect.DeepEqual(saved.Projects[0].Branches, []string{"main"}) {
					t.Fatalf("residual projects = %#v, want the unavailable ghost project", saved.Projects)
				}
				if len(residual.Branches) != 0 {
					t.Fatalf("residual branches = %#v, want none for this forest", residual.Branches)
				}
			}
			if err := yamlEqual(normalizeSelection(got), normalizeSelection(row.Expected)); err != nil {
				t.Fatalf("derived selection mismatch: %v", err)
			}
		})
	}
}

func restoreAndDerive(t *testing.T, row restorationCase) (config.SelectionConfig, onboarding.Residual) {
	t.Helper()
	roots := loadForest(t, row.Listings)
	forest := settings.SelectionForest(roots)
	_, residual, err := onboarding.RestoreSelection(forest, row.Saved)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	settings.PrepopulateSelection(roots, row.Saved)
	if got := checkedSessionIDs(roots); !reflect.DeepEqual(got, sortedCopy(row.ExpectInitiallyChecked)) {
		t.Fatalf("initially checked = %v, want %v", got, sortedCopy(row.ExpectInitiallyChecked))
	}
	for _, path := range row.ClearProjectPaths {
		setProjectPathState(t, roots, path, kit.Unchecked)
	}
	for _, path := range row.SelectProjectPaths {
		setProjectPathState(t, roots, path, kit.Checked)
	}
	for _, branch := range row.ClearBranches {
		setBranchState(t, roots, branch, kit.Unchecked)
	}
	for _, branch := range row.SelectBranches {
		setBranchState(t, roots, branch, kit.Checked)
	}
	if row.SelectAll {
		for _, root := range roots {
			setNodeState(root, kit.Checked)
		}
	}
	for _, root := range roots {
		rollupKit(root)
	}
	derived, err := onboarding.DeriveSelection(
		settings.SelectionForest(roots),
		settings.CheckedSelectionIDs(roots),
		row.Saved.AutoIngestNewBranches,
		residual,
	)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(row.LaterListings) > 0 {
		later := loadForest(t, row.LaterListings)
		settings.PrepopulateSelection(later, derived)
		if got := checkedSessionIDs(later); !reflect.DeepEqual(got, sortedCopy(row.ExpectLaterChecked)) {
			t.Fatalf("later checked = %v, want %v", got, sortedCopy(row.ExpectLaterChecked))
		}
	}
	return derived, residual
}

func assertLockedBlocksCommit(t *testing.T) {
	t.Helper()
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeSelection})
	field, ok := model.Field(onboarding.FieldSelection)
	if !ok {
		t.Fatal("missing selection field")
	}
	cfg := config.BaseConfig()
	cfg.Selection.Mode = config.SelectionModeSelected
	err := field.Validate(onboarding.WriteContext{
		Config:      cfg,
		ScanState:   onboarding.ScanDone,
		LockedNodes: 1,
	})
	if err == nil || !containsAll(err.Error(), "locked") {
		t.Fatalf("locked row did not block commit: %v", err)
	}
}

func assertRescanKeepsIDs(t *testing.T, row restorationCase) {
	t.Helper()
	current := settings.SelectionForest(loadForest(t, row.Listings))
	previous := settings.SelectionForest(loadForest(t, row.PreviousListings))
	kept, dropped := onboarding.RescanIDs(row.Answered, previous, current)
	if !reflect.DeepEqual(kept, row.ExpectKept) {
		t.Fatalf("kept = %v, want only the ids that still exist: %v", kept, row.ExpectKept)
	}
	if dropped.Count != row.ExpectDroppedCount {
		t.Fatalf("dropped count = %d, want %d", dropped.Count, row.ExpectDroppedCount)
	}
	if !reflect.DeepEqual(dropped.Labels, row.ExpectDroppedLabels) {
		t.Fatalf("dropped labels = %v, want %v (read from the forest the answer was given over)", dropped.Labels, row.ExpectDroppedLabels)
	}
	if len(dropped.Labels) > schema.LocalKickstartDroppedLabelsMax {
		t.Fatalf("dropped labels = %v, want at most %d", dropped.Labels, schema.LocalKickstartDroppedLabelsMax)
	}
	if len(kept) > len(row.Answered) {
		t.Fatal("rescan widened the answered set")
	}
}

func assertEquivalenceCorpus(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "tui", "ftue", "testdata", "equivalence", "legacy_goldens.yaml"))
	if err != nil {
		t.Fatalf("read equivalence corpus: %v", err)
	}
	var doc struct {
		Scenarios []struct {
			Name                  string `yaml:"name"`
			Oracle                string `yaml:"oracle"`
			AutoIngestNewBranches bool   `yaml:"autoIngestNewBranches"`
			Scopes                []struct {
				Level    string `yaml:"level"`
				Sessions []struct {
					GitRemote string `yaml:"gitRemote"`
					Branch    string `yaml:"branch"`
					SessionID string `yaml:"sessionId"`
				} `yaml:"sessions"`
			} `yaml:"scopes"`
			Golden config.SelectionConfig `yaml:"golden"`
		} `yaml:"scenarios"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode equivalence corpus: %v", err)
	}
	if len(doc.Scenarios) < 5 {
		t.Fatalf("equivalence corpus has %d scenarios, want at least 5", len(doc.Scenarios))
	}
	inventory, err := os.ReadFile(filepath.Join("..", "tui", "kickstart", "testdata", "inventory.yaml"))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	// legacy_goldens.yaml records the old wizard and is not edited. The rule now
	// saves the branches a project shows where the wizard saved none, so the
	// scenarios that choose a project at project level are measured against the
	// overlay's entry: the captured golden with the branch list added.
	overlayData, err := os.ReadFile(filepath.Join("..", "tui", "kickstart", "testdata", "legacy_branch_rule.yaml"))
	if err != nil {
		t.Fatalf("read branch rule overlay: %v", err)
	}
	var overlay struct {
		RequiredNames []string `yaml:"requiredNames"`
		Scenarios     []struct {
			Name     string                 `yaml:"name"`
			Expected config.SelectionConfig `yaml:"expected"`
		} `yaml:"scenarios"`
	}
	if err := yaml.Unmarshal(overlayData, &overlay); err != nil {
		t.Fatalf("decode branch rule overlay: %v", err)
	}
	overlaid := map[string]config.SelectionConfig{}
	for _, entry := range overlay.Scenarios {
		overlaid[entry.Name] = entry.Expected
	}
	for _, name := range overlay.RequiredNames {
		if _, ok := overlaid[name]; !ok {
			t.Fatalf("branch rule overlay lost its entry for %q", name)
		}
	}
	for _, scenario := range doc.Scenarios {
		if scenario.Oracle != "legacy-captured" {
			t.Fatalf("scenario %q oracle = %q", scenario.Name, scenario.Oracle)
		}
		want := scenario.Golden
		if expected, changed := overlaid[scenario.Name]; changed {
			want = expected
		}
		roots := inventoryRoots(t, inventory)
		for _, scope := range scenario.Scopes {
			for _, session := range scope.Sessions {
				applyScope(roots, scope.Level, session.GitRemote, session.Branch, session.SessionID)
			}
		}
		for _, root := range roots {
			rollupKit(root)
		}
		got, err := onboarding.DeriveSelection(
			settings.SelectionForest(roots),
			settings.CheckedSelectionIDs(roots),
			scenario.AutoIngestNewBranches,
			onboarding.Residual{},
		)
		if err != nil {
			t.Fatalf("scenario %q derive: %v", scenario.Name, err)
		}
		got.AutoIngestNewBranches = scenario.AutoIngestNewBranches
		if err := yamlEqual(withoutClonePaths(normalizeSelection(got)), normalizeSelection(want)); err != nil {
			t.Fatalf("scenario %q does not match the equivalence corpus: %v", scenario.Name, err)
		}
	}
}

func loadForest(t *testing.T, listings []restorationListing) []*kit.TreeNode {
	t.Helper()
	rows := make([]ftue.SessionListing, len(listings))
	for i, row := range listings {
		rows[i] = ftue.SessionListing{
			Harness:     row.Harness,
			ProjectName: row.ProjectName,
			GitRemote:   row.GitRemote,
			WorkingDir:  row.WorkingDir,
			Branch:      row.Branch,
			Title:       row.Title,
			SessionID:   row.SessionID,
		}
	}
	roots, err := kickstart.NewScannerTreeSource(rows, kickstart.WithPathIdentityResolver(fixtureResolver{})).Load(context.Background())
	if err != nil {
		t.Fatalf("load forest: %v", err)
	}
	return roots
}

type fixtureResolver struct{}

func (fixtureResolver) Resolve(dir string) (ingest.ClonePath, error) {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", fmt.Errorf("fixture path %q is not a clean absolute path", dir)
	}
	return ingest.ClonePath(dir), nil
}

func checkedSessionIDs(roots []*kit.TreeNode) []string {
	var checked []string
	var walk func(*kit.TreeNode)
	walk = func(node *kit.TreeNode) {
		if node != nil && node.Meta != nil && node.Meta[settings.MetaHarness] != "" && node.State == kit.Checked {
			checked = append(checked, node.ID)
		}
		if node == nil {
			return
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return sortedCopy(checked)
}

func setProjectPathState(t *testing.T, roots []*kit.TreeNode, clonePath string, state kit.TriState) {
	t.Helper()
	for _, root := range roots {
		if root.Meta != nil && root.Meta[settings.MetaClonePath] == clonePath {
			setNodeState(root, state)
			return
		}
	}
	t.Fatalf("project path %q is not in the forest", clonePath)
}

func setBranchState(t *testing.T, roots []*kit.TreeNode, target restorationBranch, state kit.TriState) {
	t.Helper()
	for _, root := range roots {
		if root.Meta == nil || root.Meta[settings.MetaClonePath] != target.ClonePath {
			continue
		}
		for _, branch := range root.Children {
			if branch.Meta != nil && branch.Meta[settings.MetaBranch] == target.Branch {
				setNodeState(branch, state)
				return
			}
		}
	}
	t.Fatalf("branch %q for %q is not in the forest", target.Branch, target.ClonePath)
}

func setNodeState(node *kit.TreeNode, state kit.TriState) {
	if node == nil {
		return
	}
	node.State = state
	for _, child := range node.Children {
		setNodeState(child, state)
	}
}

func rollupKit(node *kit.TreeNode) kit.TriState {
	if node == nil || len(node.Children) == 0 {
		if node == nil {
			return kit.Unchecked
		}
		return node.State
	}
	var checked, unchecked, other int
	for _, child := range node.Children {
		switch rollupKit(child) {
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
		node.State = kit.Partial
	case checked > 0 && unchecked == 0:
		node.State = kit.Checked
	case checked == 0 && unchecked > 0:
		node.State = kit.Unchecked
	default:
		node.State = kit.Partial
	}
	return node.State
}

func sortedCopy(values []string) []string {
	copy := append([]string(nil), values...)
	sort.Strings(copy)
	if len(copy) == 0 {
		return nil
	}
	return copy
}

func normalizeSelection(selection config.SelectionConfig) config.SelectionConfig {
	selection.DeprecatedProviders = nil
	if len(selection.Harnesses) == 0 {
		selection.Harnesses = nil
		return selection
	}
	for harness, configured := range selection.Harnesses {
		configured.Sessions = sortedCopy(configured.Sessions)
		for index := range configured.Projects {
			configured.Projects[index].ClonePaths = sortedCopy(configured.Projects[index].ClonePaths)
			configured.Projects[index].Branches = sortedCopy(configured.Projects[index].Branches)
		}
		sort.Slice(configured.Projects, func(i, j int) bool {
			left, right := configured.Projects[i], configured.Projects[j]
			return fmt.Sprintf("%s|%s|%v|%v", left.GitRemote, left.Name, left.ClonePaths, left.Branches) <
				fmt.Sprintf("%s|%s|%v|%v", right.GitRemote, right.Name, right.ClonePaths, right.Branches)
		})
		if len(configured.Projects) == 0 {
			configured.Projects = nil
		}
		selection.Harnesses[harness] = configured
	}
	return selection
}

func withoutClonePaths(selection config.SelectionConfig) config.SelectionConfig {
	for harness, configured := range selection.Harnesses {
		for index := range configured.Projects {
			configured.Projects[index].ClonePaths = nil
		}
		selection.Harnesses[harness] = configured
	}
	return selection
}

func containsAll(text, needle string) bool {
	return len(needle) == 0 || (len(text) >= len(needle) && (text == needle || len(text) > 0 && (stringContains(text, needle))))
}

func stringContains(text, needle string) bool {
	return len(text) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(text); i++ {
			if text[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

type inventoryNode struct {
	ID       string            `yaml:"id"`
	Label    string            `yaml:"label"`
	Meta     map[string]string `yaml:"meta"`
	Children []inventoryNode   `yaml:"children"`
}

func inventoryRoots(t *testing.T, data []byte) []*kit.TreeNode {
	t.Helper()
	var doc struct {
		Roots []inventoryNode `yaml:"roots"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	var roots []*kit.TreeNode
	for _, root := range doc.Roots {
		roots = append(roots, inventoryToKit(root))
	}
	return roots
}

func inventoryToKit(node inventoryNode) *kit.TreeNode {
	built := &kit.TreeNode{ID: node.ID, Label: node.Label, Meta: node.Meta}
	for _, child := range node.Children {
		built.Children = append(built.Children, inventoryToKit(child))
	}
	return built
}

func applyScope(roots []*kit.TreeNode, level, remote, branch, sessionID string) {
	var project *kit.TreeNode
	for _, root := range roots {
		if root.ID == remote {
			project = root
			break
		}
	}
	if project == nil {
		return
	}
	switch level {
	case "project":
		setNodeState(project, kit.Checked)
	case "branch":
		for _, child := range project.Children {
			if child.ID == branch {
				setNodeState(child, kit.Checked)
			}
		}
	case "session":
		for _, child := range project.Children {
			if child.ID != branch {
				continue
			}
			for _, session := range child.Children {
				if session.ID == sessionID {
					session.State = kit.Checked
				}
			}
		}
	}
}
