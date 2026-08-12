package ftue

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/schema"
)

// ProjectCatalogEntry is one canonical project with all of its harness sessions.
type ProjectCatalogEntry struct {
	Key      string
	Label    string
	Tracked  bool
	Selected bool
	Sessions []SessionListing
}

// BuildProjectCatalog applies the same remote normalization and remote/name/session
// fallback order as SelectionMatcher, then aggregates matching projects across harnesses.
func BuildProjectCatalog(sessions []SessionListing, selection *config.SelectionConfig) []ProjectCatalogEntry {
	byKey := make(map[string]*ProjectCatalogEntry)
	order := make([]string, 0)
	for _, session := range sessions {
		key, label := projectCatalogIdentity(session)
		entry := byKey[key]
		if entry == nil {
			entry = &ProjectCatalogEntry{Key: key, Label: label}
			byKey[key] = entry
			order = append(order, key)
		}
		entry.Sessions = append(entry.Sessions, session)
	}

	var matcher ingest.SelectionMatcher
	if selection != nil {
		matcher = config.CompileSelectionMatcher(*selection)
	}
	result := make([]ProjectCatalogEntry, 0, len(order))
	for _, key := range order {
		entry := *byKey[key]
		if selection != nil && selection.Mode == config.SelectionModeAll {
			entry.Tracked = true
		} else if selection != nil && selection.Mode == config.SelectionModeSelected {
			for _, session := range entry.Sessions {
				if matcher.MatchDiscovery(ingest.Harness(session.Harness), session.GitRemote, session.ProjectName, session.Branch, ingest.SessionID(session.SessionID), selection.AutoIngestNewBranches) != ingest.BranchMatchNo {
					entry.Tracked = true
					break
				}
			}
		}
		entry.Selected = entry.Tracked
		sort.SliceStable(entry.Sessions, func(i, j int) bool {
			if entry.Sessions[i].Harness != entry.Sessions[j].Harness {
				return entry.Sessions[i].Harness < entry.Sessions[j].Harness
			}
			if entry.Sessions[i].Branch != entry.Sessions[j].Branch {
				return entry.Sessions[i].Branch < entry.Sessions[j].Branch
			}
			return entry.Sessions[i].SessionID < entry.Sessions[j].SessionID
		})
		result = append(result, entry)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Tracked != result[j].Tracked {
			return !result[i].Tracked
		}
		if result[i].Label != result[j].Label {
			return strings.ToLower(result[i].Label) < strings.ToLower(result[j].Label)
		}
		return result[i].Key < result[j].Key
	})
	return result
}

func projectCatalogIdentity(session SessionListing) (string, string) {
	if remote := ingest.NormalizeRemoteForMatch(session.GitRemote); remote != "" {
		return "remote:" + remote, projectlabel.Label(session.GitRemote, session.ProjectName)
	}
	if session.ProjectName != "" {
		return "name:" + session.ProjectName, session.ProjectName
	}
	return "session:" + session.Harness + ":" + session.SessionID, "Unknown project (" + session.SessionID + ")"
}

// ProjectSelectPage is the first scope decision in kickstart. Projects are the
// primary rows; harnesses appear only as metadata beneath each project identity.
type ProjectSelectPage struct {
	projects   []ProjectCatalogEntry
	cursor     int
	offset     int
	viewHeight int
	filter     string
	searching  bool
	confirmed  bool
	keymap     TreeKeyMap
}

func NewProjectSelectPage(sessions []SessionListing, selection *config.SelectionConfig, invocationPWD string) *ProjectSelectPage {
	p := &ProjectSelectPage{projects: BuildProjectCatalog(sessions, selection), viewHeight: 14, keymap: DefaultTreeKeyMap}
	if invocationPWD != "" {
		for i, project := range p.projects {
			if projectContainsPWD(project, invocationPWD) {
				p.cursor = i
				p.offset = i
				break
			}
		}
	}
	return p
}

func projectContainsPWD(project ProjectCatalogEntry, pwd string) bool {
	pwd = filepath.Clean(pwd)
	for _, session := range project.Sessions {
		if session.WorkingDir == "" {
			continue
		}
		root := filepath.Clean(session.WorkingDir)
		if pwd == root || strings.HasPrefix(pwd, root+string(filepath.Separator)) || strings.HasPrefix(root, pwd+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (p *ProjectSelectPage) Title() string     { return "Select Projects" }
func (p *ProjectSelectPage) Init() tea.Cmd     { return nil }
func (p *ProjectSelectPage) IsComplete() bool  { return p.confirmed }
func (p *ProjectSelectPage) IsSearching() bool { return p.searching }
func (p *ProjectSelectPage) Reset()            { p.confirmed = false }

func (p *ProjectSelectPage) SelectedProjects() []ProjectCatalogEntry {
	var selected []ProjectCatalogEntry
	for _, project := range p.projects {
		if project.Selected {
			selected = append(selected, project)
		}
	}
	return selected
}

func (p *ProjectSelectPage) visible() []int {
	needle := strings.ToLower(strings.TrimSpace(p.filter))
	var visible []int
	for i, project := range p.projects {
		if needle == "" || strings.Contains(strings.ToLower(project.Label), needle) || projectSessionsMatch(project, needle) {
			visible = append(visible, i)
		}
	}
	return visible
}

func projectSessionsMatch(project ProjectCatalogEntry, needle string) bool {
	for _, session := range project.Sessions {
		if strings.Contains(strings.ToLower(session.Branch+" "+session.Title+" "+session.Harness), needle) {
			return true
		}
	}
	return false
}

func (p *ProjectSelectPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil
	}
	if p.searching {
		switch {
		case key.Matches(keyMsg, p.keymap.Cancel):
			p.searching = false
			p.filter = ""
			p.cursor, p.offset = 0, 0
		case key.Matches(keyMsg, p.keymap.Confirm):
			p.searching = false
		case keyMsg.Code == tea.KeyBackspace && len(p.filter) > 0:
			p.filter = p.filter[:len(p.filter)-1]
			p.cursor, p.offset = 0, 0
		case keyMsg.Text != "":
			p.filter += keyMsg.Text
			p.cursor, p.offset = 0, 0
		}
		return p, nil
	}
	visible := p.visible()
	switch {
	case key.Matches(keyMsg, p.keymap.Search):
		p.searching = true
	case key.Matches(keyMsg, p.keymap.Up) && p.cursor > 0:
		p.cursor--
	case key.Matches(keyMsg, p.keymap.Down) && p.cursor+1 < len(visible):
		p.cursor++
	case key.Matches(keyMsg, p.keymap.Select) && p.cursor < len(visible):
		i := visible[p.cursor]
		p.projects[i].Selected = !p.projects[i].Selected
	case key.Matches(keyMsg, p.keymap.Confirm):
		p.confirmed = true
	}
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+p.viewHeight {
		p.offset = p.cursor - p.viewHeight + 1
	}
	return p, nil
}

func (p *ProjectSelectPage) View(width, height int) string {
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.Title()))
	b.WriteString(TextBg.Render("\n\nSearch projects: " + p.filter))
	if p.searching {
		b.WriteString(OptionCursor.Render("▏"))
	}
	b.WriteString(TextBg.Render("\n\n") + DescriptionStyle.Render("Not currently tracked") + TextBg.Render("\n"))
	visible := p.visible()
	end := p.offset + p.viewHeight
	if end > len(visible) {
		end = len(visible)
	}
	trackedHeading := false
	for row := p.offset; row < end; row++ {
		project := p.projects[visible[row]]
		if project.Tracked && !trackedHeading {
			b.WriteString(TextBg.Render("\n") + DescriptionStyle.Render("Already tracked") + TextBg.Render("\n"))
			trackedHeading = true
		}
		cur := TextBg.Render("  ")
		if row == p.cursor {
			cur = OptionCursor.Render("▸ ")
		}
		check := CheckboxUnchecked.Render("[ ]")
		if project.Selected {
			check = CheckboxChecked.Render("[✓]")
		}
		harnesses := make(map[string]struct{})
		for _, session := range project.Sessions {
			harnesses[session.Harness] = struct{}{}
		}
		labelText := fmt.Sprintf("%s (%d sessions, %d harnesses)", project.Label, len(project.Sessions), len(harnesses))
		label := OptionUnselected.Render(labelText)
		if row == p.cursor {
			label = OptionSelected.Render(labelText)
		}
		b.WriteString(cur + check + TextBg.Render(" ") + label + TextBg.Render("\n"))
	}
	if len(visible) == 0 {
		b.WriteString(DescriptionStyle.Render("No projects match this search.\n"))
	}
	b.WriteString(TextBg.Render("\n"))
	b.WriteString(HelpBar.Render("↑/k: up  ↓/j: down  space: track/untrack  f: search  enter: continue"))
	return b.String()
}

type projectScopeLevel int

const (
	projectScopeProject projectScopeLevel = iota
	projectScopeBranch
	projectScopeSession
)

type projectScopeBranchEntry struct {
	name     string
	sessions []SessionListing
	expanded bool
	selected []bool
	level    projectScopeLevel
}

type projectScopeProjectEntry struct {
	label    string
	branches []projectScopeBranchEntry
	expanded bool
	level    projectScopeLevel
}

// ProjectScopeSelection preserves the level at which the user selected scope.
// Sessions identify the affected project or branch and are filtered by harness
// only when the persisted configuration is built.
type ProjectScopeSelection struct {
	Level    projectScopeLevel
	Sessions []SessionListing
}

type projectScopeRow struct {
	level      projectScopeLevel
	projectIdx int
	branchIdx  int
	sessionIdx int
}

// ProjectScopePage narrows selected projects without introducing a harness
// grouping axis. Its hierarchy is strictly Project -> Branch -> Session.
type ProjectScopePage struct {
	projects      []projectScopeProjectEntry
	cursor        int
	focusRight    bool
	harnessCursor int
	harnesses     []providerEntry
	offset        int
	viewHeight    int
	confirmed     bool
	keymap        TreeKeyMap
}

func NewProjectScopePage(projects []ProjectCatalogEntry, inventory ProviderInventory, existing *config.SelectionConfig, rehydrateHarnesses bool) *ProjectScopePage {
	page := &ProjectScopePage{viewHeight: 15, keymap: DefaultTreeKeyMap}
	for _, harness := range defaults.AllHarnesses {
		discovery := inventory[harness]
		if discovery.SessionCount == 0 && discovery.State == DiscoveryReady {
			continue
		}
		selected := discovery.State.IsOperational()
		if rehydrateHarnesses {
			selected = discovery.Enabled && discovery.State.IsOperational()
		}
		page.harnesses = append(page.harnesses, providerEntry{provider: harness, displayName: schema.HarnessDisplayName(harness), sessionCount: discovery.SessionCount, checked: selected, discovery: discovery.State, detail: discovery.Detail})
	}
	var matcher ingest.SelectionMatcher
	useExisting := existing != nil && existing.Mode == config.SelectionModeSelected
	if useExisting {
		matcher = config.CompileSelectionMatcher(*existing)
	}
	for _, project := range projects {
		entry := projectScopeProjectEntry{label: project.Label, expanded: true, level: projectScopeProject}
		branchIndexes := make(map[string]int)
		for _, session := range project.Sessions {
			branch := session.Branch
			if branch == "" {
				branch = "(default)"
			}
			bi, ok := branchIndexes[branch]
			if !ok {
				bi = len(entry.branches)
				branchIndexes[branch] = bi
				entry.branches = append(entry.branches, projectScopeBranchEntry{name: branch, level: projectScopeBranch})
			}
			selected := true
			if useExisting {
				selected = matcher.MatchDiscovery(ingest.Harness(session.Harness), session.GitRemote, session.ProjectName, session.Branch, ingest.SessionID(session.SessionID), existing.AutoIngestNewBranches) != ingest.BranchMatchNo
			}
			entry.branches[bi].sessions = append(entry.branches[bi].sessions, session)
			entry.branches[bi].selected = append(entry.branches[bi].selected, selected)
		}
		sort.SliceStable(entry.branches, func(i, j int) bool { return entry.branches[i].name < entry.branches[j].name })
		page.projects = append(page.projects, entry)
	}
	return page
}

func (p *ProjectScopePage) Title() string       { return "Narrow Project Scope" }
func (p *ProjectScopePage) Init() tea.Cmd       { return nil }
func (p *ProjectScopePage) IsComplete() bool    { return p.confirmed }
func (p *ProjectScopePage) Reset()              { p.confirmed = false }
func (p *ProjectScopePage) IsShowingHelp() bool { return false }

func (p *ProjectScopePage) rows() []projectScopeRow {
	var rows []projectScopeRow
	for pi, project := range p.projects {
		rows = append(rows, projectScopeRow{level: projectScopeProject, projectIdx: pi})
		if !project.expanded {
			continue
		}
		for bi, branch := range project.branches {
			rows = append(rows, projectScopeRow{level: projectScopeBranch, projectIdx: pi, branchIdx: bi})
			if !branch.expanded {
				continue
			}
			for si := range branch.sessions {
				rows = append(rows, projectScopeRow{level: projectScopeSession, projectIdx: pi, branchIdx: bi, sessionIdx: si})
			}
		}
	}
	return rows
}

func (p *ProjectScopePage) SelectedSessions() []SessionListing {
	var sessions []SessionListing
	for _, project := range p.projects {
		for _, branch := range project.branches {
			for i, session := range branch.sessions {
				if branch.selected[i] {
					sessions = append(sessions, session)
				}
			}
		}
	}
	return sessions
}

func (p *ProjectScopePage) ScopeSelections() []ProjectScopeSelection {
	var selections []ProjectScopeSelection
	for _, project := range p.projects {
		if project.level == projectScopeProject && p.projectSelected(project) {
			selections = append(selections, ProjectScopeSelection{Level: projectScopeProject, Sessions: selectedProjectSessions(project)})
			continue
		}
		for _, branch := range project.branches {
			if branch.level == projectScopeBranch && scopeState(branch.selected) == treeChecked {
				selections = append(selections, ProjectScopeSelection{Level: projectScopeBranch, Sessions: append([]SessionListing(nil), branch.sessions...)})
				continue
			}
			for i, selected := range branch.selected {
				if selected {
					selections = append(selections, ProjectScopeSelection{Level: projectScopeSession, Sessions: []SessionListing{branch.sessions[i]}})
				}
			}
		}
	}
	return selections
}

func (p *ProjectScopePage) projectSelected(project projectScopeProjectEntry) bool {
	for _, branch := range project.branches {
		if scopeState(branch.selected) != treeChecked {
			return false
		}
	}
	return len(project.branches) > 0
}

func selectedProjectSessions(project projectScopeProjectEntry) []SessionListing {
	var sessions []SessionListing
	for _, branch := range project.branches {
		for i, session := range branch.sessions {
			if branch.selected[i] {
				sessions = append(sessions, session)
			}
		}
	}
	return sessions
}

func (p *ProjectScopePage) ProviderSelections() []ProviderSelection {
	var selections []ProviderSelection
	for _, harness := range p.harnesses {
		if harness.checked {
			selections = append(selections, ProviderSelection{Harness: harness.provider.String(), ImportAll: false})
		}
	}
	return selections
}

func scopeState(selected []bool) treeCheckState {
	return aggregateCheckState(func(i int) treeCheckState {
		if selected[i] {
			return treeChecked
		}
		return treeUnchecked
	}, len(selected))
}

func (p *ProjectScopePage) projectState(pi int) treeCheckState {
	return aggregateCheckState(func(i int) treeCheckState {
		return scopeState(p.projects[pi].branches[i].selected)
	}, len(p.projects[pi].branches))
}

func (p *ProjectScopePage) toggle(row projectScopeRow) {
	project := &p.projects[row.projectIdx]
	switch row.level {
	case projectScopeProject:
		project.level = projectScopeProject
		target := p.projectState(row.projectIdx) != treeChecked
		for bi := range project.branches {
			project.branches[bi].level = projectScopeBranch
			for si := range project.branches[bi].selected {
				project.branches[bi].selected[si] = target
			}
		}
	case projectScopeBranch:
		project.level = projectScopeBranch
		branch := &project.branches[row.branchIdx]
		branch.level = projectScopeBranch
		target := scopeState(branch.selected) != treeChecked
		for si := range branch.selected {
			branch.selected[si] = target
		}
	case projectScopeSession:
		project.level = projectScopeBranch
		branch := &project.branches[row.branchIdx]
		branch.level = projectScopeSession
		branch.selected[row.sessionIdx] = !branch.selected[row.sessionIdx]
	}
}

func (p *ProjectScopePage) Update(msg tea.Msg) (Page, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil
	}
	rows := p.rows()
	switch {
	case keyMsg.Code == tea.KeyTab:
		p.focusRight = !p.focusRight
	case p.focusRight && key.Matches(keyMsg, p.keymap.Up) && p.harnessCursor > 0:
		p.harnessCursor--
	case p.focusRight && key.Matches(keyMsg, p.keymap.Down) && p.harnessCursor+1 < len(p.harnesses):
		p.harnessCursor++
	case p.focusRight && key.Matches(keyMsg, p.keymap.Select) && p.harnessCursor < len(p.harnesses):
		if p.harnesses[p.harnessCursor].discovery.IsOperational() {
			p.harnesses[p.harnessCursor].checked = !p.harnesses[p.harnessCursor].checked
		}
	case key.Matches(keyMsg, p.keymap.Up) && p.cursor > 0:
		p.cursor--
	case key.Matches(keyMsg, p.keymap.Down) && p.cursor+1 < len(rows):
		p.cursor++
	case key.Matches(keyMsg, p.keymap.Select) && p.cursor < len(rows):
		p.toggle(rows[p.cursor])
	case key.Matches(keyMsg, p.keymap.Expand) && p.cursor < len(rows):
		row := rows[p.cursor]
		if row.level == projectScopeProject {
			p.projects[row.projectIdx].expanded = true
		} else if row.level == projectScopeBranch {
			p.projects[row.projectIdx].branches[row.branchIdx].expanded = true
		}
	case key.Matches(keyMsg, p.keymap.Collapse) && p.cursor < len(rows):
		row := rows[p.cursor]
		if row.level == projectScopeProject {
			p.projects[row.projectIdx].expanded = false
		} else if row.level == projectScopeBranch {
			p.projects[row.projectIdx].branches[row.branchIdx].expanded = false
		}
	case key.Matches(keyMsg, p.keymap.Confirm):
		p.confirmed = true
	}
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+p.viewHeight {
		p.offset = p.cursor - p.viewHeight + 1
	}
	return p, nil
}

func scopeCheckbox(state treeCheckState) string {
	switch state {
	case treeChecked:
		return "[✓]"
	case treePartial:
		return "[~]"
	default:
		return "[ ]"
	}
}

const projectScopeSeparator = " │ "

func projectScopePaneWidths(width int) (int, int) {
	separatorWidth := lipgloss.Width(projectScopeSeparator)
	if width <= separatorWidth {
		return 0, 0
	}
	contentWidth := width - separatorWidth
	leftWidth := contentWidth / 2
	return leftWidth, contentWidth - leftWidth
}

func fitProjectScopeCell(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	value = lipgloss.NewStyle().MaxWidth(width).Render(value)
	if padding := width - lipgloss.Width(value); padding > 0 {
		value += strings.Repeat(" ", padding)
	}
	return value
}

func (p *ProjectScopePage) View(width, height int) string {
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.Title()))
	b.WriteString(TextBg.Render("\n\nChoose project scope on the left and harnesses on the right. Press tab to change columns.\n\n"))
	if width <= 0 {
		width = 80
	}
	leftWidth, rightWidth := projectScopePaneWidths(width)
	rows := p.rows()
	end := p.offset + p.viewHeight
	if end > len(rows) {
		end = len(rows)
	}
	leftLines := make([]string, 0, end-p.offset)
	for i := p.offset; i < end; i++ {
		row := rows[i]
		cursor := "  "
		if !p.focusRight && i == p.cursor {
			cursor = "▸ "
		}
		project := p.projects[row.projectIdx]
		switch row.level {
		case projectScopeProject:
			arrow := "▷"
			if project.expanded {
				arrow = "▽"
			}
			leftLines = append(leftLines, fitProjectScopeCell(fmt.Sprintf("%s%s %s %s", cursor, scopeCheckbox(p.projectState(row.projectIdx)), arrow, project.label), leftWidth))
		case projectScopeBranch:
			branch := project.branches[row.branchIdx]
			arrow := "▷"
			if branch.expanded {
				arrow = "▽"
			}
			leftLines = append(leftLines, fitProjectScopeCell(fmt.Sprintf("  %s%s %s %s", cursor, scopeCheckbox(scopeState(branch.selected)), arrow, branch.name), leftWidth))
		case projectScopeSession:
			branch := project.branches[row.branchIdx]
			session := branch.sessions[row.sessionIdx]
			box := "[ ]"
			if branch.selected[row.sessionIdx] {
				box = "[✓]"
			}
			label := session.Title
			if label == "" {
				label = session.SessionID
			}
			leftLines = append(leftLines, fitProjectScopeCell(fmt.Sprintf("      %s%s %s  (%s)", cursor, box, label, schema.HarnessDisplayName(defaults.Harness(session.Harness))), leftWidth))
		}
	}
	lineCount := len(leftLines) + 1
	if len(p.harnesses)+1 > lineCount {
		lineCount = len(p.harnesses) + 1
	}
	for i := 0; i < lineCount; i++ {
		left := ""
		right := ""
		if i == 0 {
			left = "Projects"
			right = "Harnesses"
		} else {
			if i-1 < len(leftLines) {
				left = leftLines[i-1]
			}
			if i-1 < len(p.harnesses) {
				harness := p.harnesses[i-1]
				cursor := "  "
				if p.focusRight && p.harnessCursor == i-1 {
					cursor = "▸ "
				}
				box := "[ ]"
				if harness.checked {
					box = "[✓]"
				}
				if harness.discovery.IsOperational() {
					right = fmt.Sprintf("%s%s %s (%d)", cursor, box, harness.displayName, harness.sessionCount)
				} else {
					right = fmt.Sprintf("%s[!] %s: %s", cursor, harness.displayName, harness.detail)
				}
			}
		}
		b.WriteString(fitProjectScopeCell(left, leftWidth) + projectScopeSeparator + fitProjectScopeCell(right, rightWidth) + "\n")
	}
	b.WriteString(TextBg.Render("\n"))
	b.WriteString(HelpBar.Render("tab: change column  ↑/k: up  ↓/j: down  space: select  l/→: expand  h/←: collapse  enter: continue"))
	return b.String()
}

var _ Page = (*ProjectScopePage)(nil)

var _ Page = (*ProjectSelectPage)(nil)
