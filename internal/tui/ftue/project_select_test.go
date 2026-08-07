package ftue

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/project_select.yaml
var projectSelectFixtureBytes []byte

type projectSelectFixtureFile struct {
	DeclaredRows int                    `yaml:"declaredRows"`
	Cases        []projectSelectFixture `yaml:"cases"`
}

type projectSelectFixture struct {
	Name              string           `yaml:"name"`
	PWD               string           `yaml:"pwd"`
	TrackedRemote     string           `yaml:"trackedRemote"`
	Sessions          []SessionListing `yaml:"sessions"`
	WantProjects      int              `yaml:"wantProjects"`
	WantFocused       string           `yaml:"wantFocused"`
	WantTracked       int              `yaml:"wantTracked"`
	WantScopeProjects int              `yaml:"wantScopeProjects"`
	WantScopeSessions int              `yaml:"wantScopeSessions"`
	TerminalWidth     int              `yaml:"terminalWidth"`
	MountedWidths     []int            `yaml:"mountedWidths"`
	UserEmail         string           `yaml:"userEmail"`
}

func loadProjectSelectFixtures(raw []byte) ([]projectSelectFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var fixture projectSelectFixtureFile
	if err := decoder.Decode(&fixture); err != nil {
		return nil, fmt.Errorf("decode project selector fixture: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("project selector fixture must contain exactly one YAML document")
	}
	if fixture.DeclaredRows != len(fixture.Cases) || fixture.DeclaredRows < 4 {
		return nil, fmt.Errorf("project selector fixture row count = declared %d actual %d; need at least 4", fixture.DeclaredRows, len(fixture.Cases))
	}
	names := make(map[string]bool, len(fixture.Cases))
	for _, row := range fixture.Cases {
		if row.Name == "" || names[row.Name] || len(row.Sessions) == 0 || row.WantProjects == 0 {
			return nil, fmt.Errorf("project selector fixture has blank, duplicate, or vacuous row %q", row.Name)
		}
		names[row.Name] = true
	}
	return fixture.Cases, nil
}

func TestProjectSelectFixtureStrictLoader(t *testing.T) {
	if _, err := loadProjectSelectFixtures(projectSelectFixtureBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProjectSelectFixtures(append(projectSelectFixtureBytes, []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("loader accepted a second YAML document")
	}
	if _, err := loadProjectSelectFixtures(bytes.Replace(projectSelectFixtureBytes, []byte("declaredRows:"), []byte("unknown: true\ndeclaredRows:"), 1)); err == nil {
		t.Fatal("loader accepted an unknown field")
	}
}

func TestProjectFirstCatalogAndPage(t *testing.T) {
	fixtures, err := loadProjectSelectFixtures(projectSelectFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			var selection *config.SelectionConfig
			if fixture.TrackedRemote != "" {
				value := config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{fixture.Sessions[0].Harness: {Projects: []config.ProjectSelection{{GitRemote: fixture.TrackedRemote}}}}}
				selection = &value
			}
			page := NewProjectSelectPage(fixture.Sessions, selection, fixture.PWD)
			if len(page.projects) != fixture.WantProjects {
				t.Fatalf("projects = %d, want %d", len(page.projects), fixture.WantProjects)
			}
			tracked := 0
			for _, project := range page.projects {
				if project.Tracked {
					tracked++
				}
			}
			if tracked != fixture.WantTracked {
				t.Fatalf("tracked projects = %d, want %d", tracked, fixture.WantTracked)
			}
			if fixture.WantFocused != "" && !strings.Contains(page.projects[page.visible()[page.cursor]].Label, fixture.WantFocused) {
				t.Fatalf("focused project = %q", page.projects[page.visible()[page.cursor]].Label)
			}
			if fixture.WantFocused == "" && page.cursor != 0 {
				t.Fatalf("no-match cursor = %d, want top", page.cursor)
			}
			view := page.View(80, 24)
			if !strings.Contains(view, "Search projects:") || !strings.Contains(view, "Not currently tracked") {
				t.Fatalf("missing sticky search or project pane: %s", view)
			}
		})
	}
}

func TestProjectSelectionDefaultsToEverySessionThenHarnessIntersects(t *testing.T) {
	fixtures, _ := loadProjectSelectFixtures(projectSelectFixtureBytes)
	project := BuildProjectCatalog(fixtures[0].Sessions, nil)[0]
	project.Selected = true
	inventory := ProviderInventory{defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true}, defaults.HarnessOpenCode: {SessionCount: 1, Enabled: true}}
	page := NewProjectScopePage([]ProjectCatalogEntry{project}, inventory, nil, false)
	if got := len(page.SelectedSessions()); got != 2 {
		t.Fatalf("default selected sessions = %d, want 2", got)
	}
	page.harnesses[1].checked = false
	answers := WizardAnswers{ProviderSelections: page.ProviderSelections(), SelectedSessions: page.SelectedSessions()}
	remaining := len(answers.EffectiveSelectedSessions())
	if remaining != 1 {
		t.Fatalf("harness intersection retained %d sessions, want 1", remaining)
	}
	if len(answers.SelectedSessions) != 2 {
		t.Fatalf("harness intersection erased project/session choices: %+v", answers.SelectedSessions)
	}
}

func TestProjectScopeHierarchyHasNoHarnessRoot(t *testing.T) {
	fixtures, err := loadProjectSelectFixtures(projectSelectFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	project := BuildProjectCatalog(fixtures[0].Sessions, nil)[0]
	inventory := ProviderInventory{defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true}, defaults.HarnessOpenCode: {SessionCount: 1, Enabled: true}}
	page := NewProjectScopePage([]ProjectCatalogEntry{project}, inventory, nil, false)
	if len(page.projects) != fixtures[0].WantScopeProjects {
		t.Fatalf("scope projects = %d, want %d", len(page.projects), fixtures[0].WantScopeProjects)
	}
	rows := page.rows()
	if len(rows) < 3 || rows[0].level != projectScopeProject {
		t.Fatalf("first narrow-scope row = %+v, want project", rows)
	}
	for _, row := range rows {
		if row.level < projectScopeProject || row.level > projectScopeSession {
			t.Fatalf("unexpected hierarchy level %d", row.level)
		}
	}
	view := page.View(fixtures[0].TerminalWidth, 30)
	if strings.Count(view, project.Label) != 1 {
		t.Fatalf("project root count = %d, want 1:\n%s", strings.Count(view, project.Label), view)
	}
	if strings.Contains(view, "Claude Code (2 sessions)") || strings.Contains(view, "OpenCode (2 sessions)") {
		t.Fatalf("narrow scope rendered a harness group:\n%s", view)
	}
	if !strings.Contains(view, " │ Harnesses") {
		t.Fatalf("two-column layout is not usable at width %d:\n%s", fixtures[0].TerminalWidth, view)
	}
	if len(page.SelectedSessions()) != fixtures[0].WantScopeSessions {
		t.Fatalf("cross-harness project scope lost sessions: %+v", page.SelectedSessions())
	}
}

func TestMountedProjectScopeFrameKeepsPanesAligned(t *testing.T) {
	fixtures, err := loadProjectSelectFixtures(projectSelectFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixtureNamed(t, fixtures, "long-project-and-session-labels-fit-mounted-frame")
	project := BuildProjectCatalog(fixture.Sessions, nil)[0]
	project.Selected = true
	wizard := NewWizard(
		WithSessions(fixture.Sessions),
		WithProviderInventory(ProviderInventory{
			defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true},
			defaults.HarnessOpenCode:   {SessionCount: 1, Enabled: true},
		}),
	)
	wizard.answers.WantImport = true
	wizard.answers.SelectedProjects = []ProjectCatalogEntry{project}
	wizard.preparePage(pageSessionSelect)
	wizard.current = pageSessionSelect
	updated, _ := wizard.Update(tea.WindowSizeMsg{Width: fixture.TerminalWidth, Height: 24})
	wizard = updated.(WizardModel)
	view := wizard.View().Content

	lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
	headingLine := -1
	separatorColumn := -1
	for i, line := range lines {
		if !strings.Contains(line, "Projects") || !strings.Contains(line, "Harnesses") {
			continue
		}
		headingLine = i
		separatorColumn = mountedPaneSeparatorColumn(t, line)
		if strings.Contains(mountedPaneLeftCell(line), "Harnesses") {
			t.Fatalf("Harnesses rendered in the left pane: %q", line)
		}
		if !strings.Contains(mountedPaneRightCell(line), "Harnesses") {
			t.Fatalf("Harnesses missing from the right header cell: %q", line)
		}
		break
	}
	if headingLine < 0 {
		t.Fatalf("mounted frame has no explicit Projects/Harnesses heading row:\n%s", view)
	}
	if strings.Count(view, "Harnesses") != 1 {
		t.Fatalf("mounted frame rendered Harnesses outside its header:\n%s", view)
	}

	projectPrefix := project.Label
	if len(projectPrefix) > 20 {
		projectPrefix = projectPrefix[:20]
	}
	firstProjectLine := -1
	for i := headingLine + 1; i < len(lines); i++ {
		if strings.Contains(lines[i], projectPrefix) {
			firstProjectLine = i
			break
		}
	}
	if firstProjectLine <= headingLine {
		t.Fatalf("first project row did not begin below the pane headings:\n%s", view)
	}
	if mountedPaneSeparatorColumn(t, lines[firstProjectLine]) != separatorColumn {
		t.Fatalf("first project row moved the pane separator:\n%s", view)
	}
	if strings.Contains(mountedPaneRightCell(lines[firstProjectLine]), projectPrefix) {
		t.Fatalf("first project row started in the right pane:\n%s", view)
	}
	if strings.Contains(lines[firstProjectLine], project.Label) {
		t.Fatalf("long project label crossed the left pane boundary:\n%s", view)
	}

	for i, line := range lines {
		if got := lipgloss.Width(line); got > fixture.TerminalWidth {
			t.Fatalf("mounted frame line %d is %d cells wide, want <= %d: %q", i, got, fixture.TerminalWidth, line)
		}
		if strings.Count(line, "│") >= 3 && mountedPaneSeparatorColumn(t, line) != separatorColumn {
			t.Fatalf("pane separator moved on line %d: got %d, want %d: %q", i, mountedPaneSeparatorColumn(t, line), separatorColumn, line)
		}
	}
}

func TestMountedProjectScopeUsesMidpointBoundary(t *testing.T) {
	fixtures, err := loadProjectSelectFixtures(projectSelectFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixtureNamed(t, fixtures, "long-project-and-session-labels-fit-mounted-frame")
	if len(fixture.MountedWidths) < 3 {
		t.Fatalf("mounted width cases = %d, want at least 3", len(fixture.MountedWidths))
	}
	project := BuildProjectCatalog(fixture.Sessions, nil)[0]
	project.Selected = true

	for _, terminalWidth := range fixture.MountedWidths {
		t.Run(fmt.Sprintf("width-%d", terminalWidth), func(t *testing.T) {
			wizard := NewWizard(
				WithSessions(fixture.Sessions),
				WithProviderInventory(ProviderInventory{
					defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true},
					defaults.HarnessOpenCode:   {SessionCount: 1, Enabled: true},
				}),
			)
			wizard.answers.WantImport = true
			wizard.answers.SelectedProjects = []ProjectCatalogEntry{project}
			wizard.preparePage(pageSessionSelect)
			wizard.current = pageSessionSelect
			updated, _ := wizard.Update(tea.WindowSizeMsg{Width: terminalWidth, Height: 24})
			view := updated.(WizardModel).View().Content

			var header string
			for _, line := range strings.Split(strings.TrimSuffix(view, "\n"), "\n") {
				if strings.Count(line, "│") < 3 {
					continue
				}
				if strings.TrimSpace(mountedPaneLeftCell(line)) == "Projects" && strings.TrimSpace(mountedPaneRightCell(line)) == "Harnesses" {
					header = line
					break
				}
			}
			if header == "" {
				t.Fatalf("mounted frame has no exact Projects/Harnesses headers:\n%s", view)
			}
			if got, want := mountedPaneSeparatorColumn(t, header), (terminalWidth-1)/2; got != want {
				t.Fatalf("left pane boundary = %d, want terminal midpoint %d at width %d:\n%s", got, want, terminalWidth, view)
			}
		})
	}
}

func mountedPaneSeparatorColumn(t *testing.T, line string) int {
	t.Helper()
	first := strings.Index(line, "│")
	if first < 0 {
		t.Fatalf("pane line has no frame border: %q", line)
	}
	remaining := line[first+len("│"):]
	second := strings.Index(remaining, "│")
	if second < 0 {
		t.Fatalf("pane line has no separator: %q", line)
	}
	return lipgloss.Width(line[:first+len("│")+second])
}

// mountedPaneLeftCell / mountedPaneRightCell return the VISIBLE cell text. The
// color escape sequences that style a cell are stripped so a cell's text can be
// compared directly: styled cells now carry per-line color resets, so the bytes
// between two frame borders include escape codes as well as the label.
func mountedPaneLeftCell(line string) string {
	first := strings.Index(line, "│")
	second := strings.Index(line[first+len("│"):], "│")
	return ansi.Strip(line[first+len("│") : first+len("│")+second])
}

func mountedPaneRightCell(line string) string {
	first := strings.Index(line, "│")
	second := strings.Index(line[first+len("│"):], "│")
	third := strings.Index(line[first+len("│")+second+len("│"):], "│")
	return ansi.Strip(line[first+len("│")+second+len("│") : first+len("│")+second+len("│")+third])
}

func TestProjectScopeHarnessDefaultsAndRehydration(t *testing.T) {
	fixtures, err := loadProjectSelectFixtures(projectSelectFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	project := BuildProjectCatalog(fixtures[0].Sessions, nil)[0]
	inventory := ProviderInventory{defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true}, defaults.HarnessOpenCode: {SessionCount: 1, Enabled: false}}
	fresh := NewProjectScopePage([]ProjectCatalogEntry{project}, inventory, nil, false)
	if len(fresh.ProviderSelections()) != 2 {
		t.Fatalf("fresh operational harness defaults = %d, want 2", len(fresh.ProviderSelections()))
	}
	rehydrated := NewProjectScopePage([]ProjectCatalogEntry{project}, inventory, nil, true)
	if got := rehydrated.ProviderSelections(); len(got) != 1 || got[0].Harness != defaults.HarnessClaudeCode.String() {
		t.Fatalf("rehydrated harness selections = %+v, want only Claude Code", got)
	}
}

func TestWizardProjectScopeChoicesSurviveProviderBackNavigation(t *testing.T) {
	fixtures, err := loadProjectSelectFixtures(projectSelectFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	wizard := NewWizard(
		WithSessions(fixtures[0].Sessions),
		WithProviderInventory(ProviderInventory{
			defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true},
			defaults.HarnessOpenCode:   {SessionCount: 1, Enabled: true},
		}),
	)
	wizard.current = pageProjectSelect
	projectPage := wizard.pages[pageProjectSelect].(*ProjectSelectPage)
	projectPage.projects[0].Selected = true
	projectPage.confirmed = true
	wizard.storeAnswer(pageProjectSelect)
	if advanced, _ := wizard.nextPage(); !advanced || wizard.current != pageSessionSelect {
		t.Fatalf("wizard did not advance to project scope: current=%d", wizard.current)
	}
	scope := wizard.pages[pageSessionSelect].(*ProjectScopePage)
	if len(scope.SelectedSessions()) != 2 {
		t.Fatalf("initial project scope sessions = %d, want 2", len(scope.SelectedSessions()))
	}
	// Narrow one branch by using the production selection operation.
	scope.toggle(projectScopeRow{level: projectScopeSession, projectIdx: 0, branchIdx: 0, sessionIdx: 0})
	scope.confirmed = true
	wizard.storeAnswer(pageSessionSelect)
	if advanced, _ := wizard.nextPage(); !advanced || wizard.current != pageAutoIngest {
		t.Fatalf("wizard did not advance directly to auto-ingest: current=%d", wizard.current)
	}
	wizard.prevPage()
	if wizard.current != pageSessionSelect {
		t.Fatalf("back from provider returned to page %d, want project scope", wizard.current)
	}
	if got := len(wizard.pages[pageSessionSelect].(*ProjectScopePage).SelectedSessions()); got != 1 {
		t.Fatalf("back navigation rebuilt and erased narrowed scope: selected=%d", got)
	}
}

func TestExplicitSessionScopePersistsWithoutSiblingWidening(t *testing.T) {
	fixtures, err := loadProjectSelectFixtures(projectSelectFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixtureNamed(t, fixtures, "explicit-session-does-not-widen-to-sibling")
	project := BuildProjectCatalog(fixture.Sessions, nil)[0]
	page := NewProjectScopePage([]ProjectCatalogEntry{project}, ProviderInventory{defaults.HarnessClaudeCode: {SessionCount: 2, Enabled: true}}, nil, false)
	page.toggle(projectScopeRow{level: projectScopeSession, projectIdx: 0, branchIdx: 0, sessionIdx: 1})
	answers := WizardAnswers{
		WantImport:         true,
		ProviderSelections: page.ProviderSelections(),
		SelectedSessions:   page.SelectedSessions(),
		ScopeSelections:    page.ScopeSelections(),
	}
	selection := buildSelectionConfig(&answers)
	if got := selection.Harnesses[defaults.HarnessClaudeCode.String()].Sessions; len(got) != 1 || got[0] != fixture.Sessions[0].SessionID {
		t.Fatalf("saved explicit sessions = %v, want only %s", got, fixture.Sessions[0].SessionID)
	}
	matcher := config.CompileSelectionMatcher(*selection)
	if matcher.MatchDiscovery(defaults.HarnessClaudeCode, fixture.Sessions[0].GitRemote, fixture.Sessions[0].ProjectName, fixture.Sessions[0].Branch, ingest.SessionID(fixture.Sessions[0].SessionID), false) == ingest.BranchMatchNo {
		t.Fatal("production SelectionMatcher rejected the explicitly selected session")
	}
	if matcher.MatchDiscovery(defaults.HarnessClaudeCode, fixture.Sessions[1].GitRemote, fixture.Sessions[1].ProjectName, fixture.Sessions[1].Branch, ingest.SessionID(fixture.Sessions[1].SessionID), false) != ingest.BranchMatchNo {
		t.Fatal("production SelectionMatcher accepted the unselected sibling session")
	}
}

func TestUncheckedHarnessExcludedFromMountedPersistence(t *testing.T) {
	fixtures, err := loadProjectSelectFixtures(projectSelectFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixtureNamed(t, fixtures, "unchecked-harness-is-not-persisted")
	wizard := NewWizard(
		WithSessions(fixture.Sessions),
		WithProviderInventory(ProviderInventory{
			defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true},
			defaults.HarnessOpenCode:   {SessionCount: 1, Enabled: true},
		}),
	)
	projectPage := wizard.pages[pageProjectSelect].(*ProjectSelectPage)
	projectPage.projects[0].Selected = true
	wizard.storeAnswer(pageProjectSelect)
	wizard.preparePage(pageSessionSelect)
	scope := wizard.pages[pageSessionSelect].(*ProjectScopePage)
	scope.harnesses[1].checked = false
	wizard.storeAnswer(pageSessionSelect)
	selection := buildSelectionConfig(wizard.answers)
	if len(wizard.answers.SelectedSessions) != 2 {
		t.Fatalf("raw left selections = %d, want 2", len(wizard.answers.SelectedSessions))
	}
	if _, ok := selection.Harnesses[defaults.HarnessOpenCode.String()]; ok {
		t.Fatalf("unchecked OpenCode persisted: %+v", selection.Harnesses)
	}
	if _, ok := selection.Harnesses[defaults.HarnessClaudeCode.String()]; !ok {
		t.Fatalf("selected Claude missing from persistence: %+v", selection.Harnesses)
	}
	matcher := config.CompileSelectionMatcher(*selection)
	if matcher.MatchDiscovery(defaults.HarnessOpenCode, fixture.Sessions[1].GitRemote, fixture.Sessions[1].ProjectName, fixture.Sessions[1].Branch, ingest.SessionID(fixture.Sessions[1].SessionID), false) != ingest.BranchMatchNo {
		t.Fatal("production SelectionMatcher accepted a session from the unchecked harness")
	}
}

func TestMountedProjectSelectionPersistsStopAllWithoutWidening(t *testing.T) {
	fixtures, err := loadProjectSelectFixtures(projectSelectFixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixtureNamed(t, fixtures, "explicit-stop-all-persists-empty-allowlist")
	exact := filepath.Join(t.TempDir(), "chosen", "config.yaml")
	t.Setenv(defaults.EnvXDGConfigHome.String(), t.TempDir())
	loaded := config.BaseConfig()
	loaded.User.Email = fixture.UserEmail
	loaded.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessClaudeCode.String(): {Projects: []config.ProjectSelection{{GitRemote: fixture.TrackedRemote}}},
		},
	}
	if err := config.SaveAtomic(exact, loaded); err != nil {
		t.Fatalf("seed exact config path: %v", err)
	}
	snapshot, err := os.ReadFile(exact)
	if err != nil {
		t.Fatalf("read exact config snapshot: %v", err)
	}

	wizard := NewWizard(
		WithSessions(fixture.Sessions),
		WithExistingSelection(&loaded.Selection),
		WithConfigPersistence(exact, loaded),
		WithConfigSnapshot(snapshot, true),
	)
	projectPage := wizard.pages[pageProjectSelect].(*ProjectSelectPage)
	projectPage.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	wizard.storeAnswer(pageProjectSelect)
	if wizard.answers.WantImport {
		t.Fatal("mounted project toggle did not record explicit stop-all intent")
	}
	if err := wizard.saveConfig(); err != nil {
		t.Fatalf("persist mounted stop-all choice: %v", err)
	}

	savedBytes, err := os.ReadFile(exact)
	if err != nil {
		t.Fatalf("read persisted exact config: %v", err)
	}
	saved, err := config.Parse(savedBytes)
	if err != nil {
		t.Fatalf("parse persisted exact config: %v", err)
	}
	if saved.User.Email != loaded.User.Email {
		t.Fatalf("unrelated loaded config was not preserved: email=%q", saved.User.Email)
	}
	if saved.Selection.Mode != config.SelectionModeSelected || len(saved.Selection.Harnesses) != 0 {
		t.Fatalf("stop-all selection = %+v, want selected mode with empty allowlist", saved.Selection)
	}
	session := fixture.Sessions[0]
	if saved.SelectionMatcher().MatchDiscovery(defaults.HarnessClaudeCode, session.GitRemote, session.ProjectName, session.Branch, ingest.SessionID(session.SessionID), false) != ingest.BranchMatchNo {
		t.Fatal("canonical SelectionMatcher widened explicit stop-all to discovery")
	}
	if _, err := os.Stat(defaults.ResolveConfigFilePath().String()); !os.IsNotExist(err) {
		t.Fatalf("mounted save wrote outside the exact config path: %v", err)
	}
}

func fixtureNamed(t *testing.T, fixtures []projectSelectFixture, name string) projectSelectFixture {
	t.Helper()
	for _, fixture := range fixtures {
		if fixture.Name == name {
			return fixture
		}
	}
	t.Fatalf("fixture %q not found", name)
	return projectSelectFixture{}
}
