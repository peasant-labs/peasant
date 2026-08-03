package ftue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/browser"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/peasant/internal/tui/search"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// Page is the interface for each wizard page.
type Page interface {
	Title() string
	Init() tea.Cmd
	Update(msg tea.Msg) (Page, tea.Cmd)
	View(width, height int) string
	IsComplete() bool
}

// DestinationPage keeps local continuation available and resolves Village
// visibility through config's canonical policy authority.
type DestinationPage struct {
	connected bool
	cfg       *config.Config
	cursor    int
	selected  bool
}

func NewDestinationPage(connected bool, cfg *config.Config) *DestinationPage {
	cursor := 0
	if connected {
		cursor = 1 // private Village publication is the safe remote default.
	}
	return &DestinationPage{connected: connected, cfg: cfg, cursor: cursor}
}

func (p *DestinationPage) Title() string    { return "Choose Destination" }
func (p *DestinationPage) Init() tea.Cmd    { return nil }
func (p *DestinationPage) IsComplete() bool { return p.selected }
func (p *DestinationPage) Reset()           { p.selected = false }

func (p *DestinationPage) Destination() Destination {
	if p.connected && p.cursor > 0 {
		return DestinationVillage
	}
	return DestinationLocal
}

func (p *DestinationPage) Authentication() AuthenticationChoice {
	if p.Destination() == DestinationVillage {
		return AuthenticationAuthenticated
	}
	return AuthenticationSkipped
}

func (p *DestinationPage) RequestedVisibility() schema.Visibility {
	if p.Destination() != DestinationVillage {
		return ""
	}
	if p.cursor == 2 {
		return schema.VisibilityPublic
	}
	return schema.VisibilityPrivate
}

func (p *DestinationPage) policy() config.VisibilityPolicy {
	return config.EffectiveVisibility(config.Visibility(p.RequestedVisibility()), p.cfg)
}

func (p *DestinationPage) EffectiveVisibility() schema.Visibility {
	if p.Destination() != DestinationVillage {
		return ""
	}
	return schema.Visibility(p.policy().Effective)
}

func (p *DestinationPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	max := 0
	if p.connected {
		max = 2
	}
	switch keyMsg.String() {
	case defaults.KeyUp.String(), defaults.KeyVimUp.String():
		if p.cursor > 0 {
			p.cursor--
		}
	case defaults.KeyDown.String(), defaults.KeyVimDown.String():
		if p.cursor < max {
			p.cursor++
		}
	case defaults.KeyEnter.String():
		p.selected = true
	}
	return p, nil
}

func (p *DestinationPage) View(width, height int) string {
	options := []string{"Keep local only"}
	if p.connected {
		options = append(options, "Publish privately to Village", "Publish publicly to Village")
	}
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.Title()) + "\n\n")
	if !p.connected {
		b.WriteString(DescriptionStyle.Render("You skipped login. Local setup will continue; run 'peasant login' and 'peasant village push --non-interactive' later to publish.") + "\n\n")
	}
	for i, option := range options {
		cursor := "  "
		if i == p.cursor {
			cursor = "▸ "
		}
		b.WriteString(fmt.Sprintf("%s%s\n", cursor, option))
	}
	if p.connected && p.cursor == 2 {
		b.WriteString("\nPublic warning: anyone may download and retain copies. Unpublishing cannot recall downloaded copies, and Creative Commons grants remain irrevocable.\n")
	}
	if p.Destination() == DestinationVillage {
		if disclosure := p.policy().Disclosure(); disclosure != "" {
			b.WriteString("\n" + disclosure + "\n")
		}
	}
	b.WriteString("\n" + HelpBar.Render("↑/↓: choose  enter: continue"))
	return b.String()
}

var _ Page = (*DestinationPage)(nil)

// ---------------------------------------------------------------------------
// SingleSelectPage
// ---------------------------------------------------------------------------

// SingleSelectPage presents a list of options; user picks one.
type SingleSelectPage struct {
	title        string
	description  string
	options      []string
	descriptions []string
	cursor       int
	selected     int // -1 = none
}

// NewSingleSelect creates a new single-select page.
func NewSingleSelect(title, description string, options, descriptions []string) *SingleSelectPage {
	return &SingleSelectPage{
		title:        title,
		description:  description,
		options:      options,
		descriptions: descriptions,
		cursor:       0,
		selected:     -1,
	}
}

func (p *SingleSelectPage) Title() string    { return p.title }
func (p *SingleSelectPage) IsComplete() bool { return p.selected >= 0 }
func (p *SingleSelectPage) Init() tea.Cmd    { return nil }
func (p *SingleSelectPage) Selected() int    { return p.selected }

// Reset clears the selection so the page can be revisited.
func (p *SingleSelectPage) Reset() {
	p.selected = -1
}

func (p *SingleSelectPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case defaults.KeyUp.String(), defaults.KeyVimUp.String():
			if p.cursor > 0 {
				p.cursor--
			}
		case defaults.KeyDown.String(), defaults.KeyVimDown.String():
			if p.cursor < len(p.options)-1 {
				p.cursor++
			}
		case defaults.KeyEnter.String():
			p.selected = p.cursor
		default:
			// Number keys 1-9 for instant select
			if len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= '9' {
				idx := int(msg.String()[0] - '1')
				if idx < len(p.options) {
					p.selected = idx
				}
			}
		}
	}
	return p, nil
}

func (p *SingleSelectPage) View(width, height int) string {
	var b strings.Builder

	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))

	if p.description != "" {
		b.WriteString(DescriptionStyle.Render(p.description))
		b.WriteString(TextBg.Render("\n\n"))
	}

	for i, opt := range p.options {
		cursor := TextBg.Render("  ")
		if i == p.cursor {
			cursor = OptionCursor.Render("▸ ")
		}

		num := fmt.Sprintf("%d) ", i+1)
		label := opt
		if i == p.cursor {
			label = OptionSelected.Render(num + label)
		} else {
			label = OptionUnselected.Render(num + label)
		}

		b.WriteString(cursor + label)

		if i < len(p.descriptions) && p.descriptions[i] != "" {
			b.WriteString(TextBg.Render("\n    "))
			b.WriteString(HelpBar.Render(p.descriptions[i]))
		}
		b.WriteString(TextBg.Render("\n"))
	}

	b.WriteString(TextBg.Render("\n"))
	b.WriteString(HelpBar.Render("↑/↓: navigate  enter: select"))

	return b.String()
}

// ---------------------------------------------------------------------------
// ProviderSelectPage
// ---------------------------------------------------------------------------

// providerEntry holds per-provider state in the ProviderSelectPage.
type providerEntry struct {
	provider     defaults.Harness
	displayName  string
	sessionCount int
	checked      bool
	importAll    bool // true = import all, false = select sessions
	discovery    DiscoveryState
	detail       string
}

// providerRowKind identifies what kind of row is at a given cursor position.
type providerRowKind int

const (
	providerRowProvider  providerRowKind = iota // checkbox row for a provider
	providerRowImportAll                        // "Import all" sub-item
	providerRowSelect                           // "Select sessions" sub-item
	providerRowContinue                         // "[Continue →]" action
)

// providerFlatRow is a single navigable row in the virtual flat list.
type providerFlatRow struct {
	kind        providerRowKind
	providerIdx int // index into ProviderSelectPage.providers (-1 for Continue)
}

// ProviderSelectPage presents discovered providers with per-provider
// checkboxes and import-mode sub-items (Import all / Select sessions).
type ProviderSelectPage struct {
	title       string
	description string
	providers   []providerEntry
	cursor      int  // index into flatRows()
	confirmed   bool // set when user selects Continue
	showingHelp bool
	keymap      PageKeyMap
}

// NewProviderSelectPage builds a provider selection page from discovered inventory.
// All known providers are shown; those with 0 sessions are unchecked and dimmed.
func NewProviderSelectPage(title, description string, inventory ProviderInventory) *ProviderSelectPage {
	var entries []providerEntry
	for _, p := range defaults.AllHarnesses {
		name := schema.HarnessDisplayName(p)
		discovery := inventory[p]
		checked := discovery.SessionCount > 0
		entries = append(entries, providerEntry{
			provider:     p,
			displayName:  name,
			sessionCount: discovery.SessionCount,
			checked:      checked,
			importAll:    false, // default: select sessions
		})
	}
	return &ProviderSelectPage{
		title:       title,
		description: description,
		providers:   entries,
		keymap:      DefaultPageKeyMap,
	}
}

func (p *ProviderSelectPage) Title() string       { return p.title }
func (p *ProviderSelectPage) IsComplete() bool    { return p.confirmed }
func (p *ProviderSelectPage) Init() tea.Cmd       { return nil }
func (p *ProviderSelectPage) IsShowingHelp() bool { return p.showingHelp }

// Selections returns the per-provider import configuration for checked providers.
func (p *ProviderSelectPage) Selections() []ProviderSelection {
	var out []ProviderSelection
	for _, e := range p.providers {
		if e.checked {
			out = append(out, ProviderSelection{
				Harness:   e.provider.String(),
				ImportAll: e.importAll,
			})
		}
	}
	return out
}

// Reset clears the confirmed state so the page can be revisited.
// Provider check/radio state is preserved across resets.
func (p *ProviderSelectPage) Reset() {
	p.confirmed = false
	p.showingHelp = false
}

// flatRows builds the virtual navigable list. Checked providers expose
// two sub-items ("Import all", "Select sessions") immediately below them.
// Providers with 0 sessions are excluded — they are rendered but not navigable.
func (p *ProviderSelectPage) flatRows() []providerFlatRow {
	var rows []providerFlatRow
	for i, e := range p.providers {
		if e.sessionCount == 0 {
			continue // shown in View but not navigable
		}
		rows = append(rows, providerFlatRow{kind: providerRowProvider, providerIdx: i})
		if e.checked {
			rows = append(rows,
				providerFlatRow{kind: providerRowImportAll, providerIdx: i},
				providerFlatRow{kind: providerRowSelect, providerIdx: i},
			)
		}
	}
	return rows
}

func (p *ProviderSelectPage) providerHelpBarText() string {
	return pageStatusBar()
}

func (p *ProviderSelectPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if p.showingHelp {
			if key.Matches(msg, p.keymap.Help) || key.Matches(msg, p.keymap.Cancel) {
				p.showingHelp = false
			}
			return p, nil
		}

		rows := p.flatRows()
		totalRows := len(rows)

		switch {
		case key.Matches(msg, p.keymap.Help):
			p.showingHelp = true
			return p, nil
		case key.Matches(msg, p.keymap.Up):
			if p.cursor > 0 {
				p.cursor--
			}
		case key.Matches(msg, p.keymap.Down):
			if p.cursor < totalRows-1 {
				p.cursor++
			}
		case key.Matches(msg, p.keymap.Select):
			if p.cursor >= 0 && p.cursor < totalRows {
				row := rows[p.cursor]
				switch row.kind {
				case providerRowProvider:
					// Only allow toggling providers that have sessions.
					if p.providers[row.providerIdx].sessionCount > 0 {
						p.providers[row.providerIdx].checked = !p.providers[row.providerIdx].checked
						newRows := p.flatRows()
						if p.cursor >= len(newRows) {
							p.cursor = len(newRows) - 1
						}
					}
				case providerRowImportAll:
					p.providers[row.providerIdx].importAll = true
				case providerRowSelect:
					p.providers[row.providerIdx].importAll = false
				}
			}
		case key.Matches(msg, p.keymap.Confirm):
			// Always allow confirm — no checked providers means "skip import".
			p.confirmed = true
		}
	}
	return p, nil
}

func (p *ProviderSelectPage) View(width, height int) string {
	var b strings.Builder

	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))

	if p.showingHelp {
		b.WriteString(renderHelpOverlay(pageHelpCategories(p.keymap)))
		return b.String()
	}

	if p.description != "" {
		b.WriteString(DescriptionStyle.Render(p.description))
		b.WriteString(TextBg.Render("\n\n"))
	}

	rows := p.flatRows()

	// Build a set of flat-row indices per provider for cursor tracking.
	rowIdx := 0
	for _, e := range p.providers {
		if e.sessionCount == 0 {
			// Dimmed, non-navigable provider.
			label := fmt.Sprintf("  [ ] %s (0 sessions)", e.displayName)
			b.WriteString(HelpBar.Render(label) + TextBg.Render("\n"))
			continue
		}

		// Provider row.
		active := rowIdx < len(rows) && p.cursor == rowIdx
		cur := TextBg.Render("  ")
		if active {
			cur = OptionCursor.Render("▸ ")
		}
		check := CheckboxUnchecked.Render("[ ]")
		if e.checked {
			check = CheckboxChecked.Render("[✓]")
		}
		label := fmt.Sprintf("%s (%d sessions)", e.displayName, e.sessionCount)
		if active {
			label = OptionSelected.Render(label)
		} else {
			label = OptionUnselected.Render(label)
		}
		b.WriteString(cur + check + TextBg.Render(" ") + label + TextBg.Render("\n"))
		rowIdx++

		// Sub-items (only if checked).
		if e.checked {
			for _, subKind := range []providerRowKind{providerRowImportAll, providerRowSelect} {
				subActive := rowIdx < len(rows) && p.cursor == rowIdx
				subCur := TextBg.Render("  ")
				if subActive {
					subCur = OptionCursor.Render("▸ ")
				}
				radio := "○"
				var subLabel string
				if subKind == providerRowImportAll {
					if e.importAll {
						radio = "●"
					}
					subLabel = radio + " Import all"
				} else {
					if !e.importAll {
						radio = "●"
					}
					subLabel = radio + " Select sessions"
				}
				if subActive {
					subLabel = OptionSelected.Render(subLabel)
				} else {
					subLabel = HelpBar.Render(subLabel)
				}
				b.WriteString(TextBg.Render("       ") + subCur + subLabel + TextBg.Render("\n"))
				rowIdx++
			}
		}
	}

	b.WriteString(TextBg.Render("\n"))
	b.WriteString(p.providerHelpBarText())

	return b.String()
}

// ---------------------------------------------------------------------------
// TreeSelectPage
// ---------------------------------------------------------------------------

// treeCheckState classifies how much of a subtree is selected.
type treeCheckState int

const (
	treeUnchecked treeCheckState = iota
	treePartial
	treeChecked
)

// treeProvider is the top-level node (e.g. "Claude").
type treeProvider struct {
	name     string
	remotes  []treeRemote
	expanded bool
}

// treeRemote is a second-level node (e.g. "user/my-repo").
type treeRemote struct {
	name      string
	worktrees []treeWorktree
	expanded  bool
}

// treeWorktree is a third-level node (e.g. "fix--kickstart-config").
type treeWorktree struct {
	name     string
	sessions []SessionListing
	expanded bool
}

// treeLevel identifies what kind of row a treeFlatItem represents.
type treeLevel int

const (
	treeLevelProvider treeLevel = iota
	treeLevelRemote
	treeLevelWorktree
	treeLevelSession
	treeLevelConfirm // the [Confirm →] footer row
)

// treeFlatItem is one visible row in the flattened tree.
type treeFlatItem struct {
	level       treeLevel
	providerIdx int
	remoteIdx   int
	worktreeIdx int
	sessionIdx  int
}

// selectionGrid is a 4-dimensional boolean grid tracking session selections:
// selectionGrid[provider][remote][worktree][session].
type selectionGrid [][][][]bool

const treeOverhead = 12

// TreeSelectPage shows sessions in a 4-level scrollable tree:
// Provider → Remote → Worktree → Session.
type TreeSelectPage struct {
	title      string
	providers  []treeProvider
	sessionSel selectionGrid // sessionSel[pi][ri][wi][si]
	// sessionConflict marks sessions the saved selection cannot decide: two or
	// more project entries identify them and disagree about their branch. It is
	// recorded once, where the matcher answers, rather than recomputed at render
	// time — the renderer has no matcher and no configuration to consult.
	sessionConflict     selectionGrid // sessionConflict[pi][ri][wi][si]
	cursor              int
	offset              int
	viewHeight          int
	confirmed           bool
	confirmingSelection bool
	filter              search.FilterState
	showingHelp         bool
	keymap              TreeKeyMap
}

// NewTreeSelectPage builds a Provider → Remote → Worktree → Session tree.
func NewTreeSelectPage(title string, sessions []SessionListing) *TreeSelectPage {
	provIdx := map[string]int{}
	var providers []treeProvider

	for _, s := range sessions {
		pi, ok := provIdx[s.Harness]
		if !ok {
			pi = len(providers)
			provIdx[s.Harness] = pi
			providers = append(providers, treeProvider{name: s.Harness})
		}

		remoteName := s.ProjectName
		if s.GitRemote != "" {
			remoteName = projectlabel.Label(s.GitRemote, s.ProjectName)
		}
		if remoteName == "" {
			remoteName = "(unknown)"
		}

		branchName := s.Branch
		if branchName == "" {
			branchName = "(default)"
		}

		ri := -1
		for i, r := range providers[pi].remotes {
			if r.name == remoteName {
				ri = i
				break
			}
		}
		if ri == -1 {
			ri = len(providers[pi].remotes)
			providers[pi].remotes = append(providers[pi].remotes, treeRemote{name: remoteName})
		}

		wi := -1
		for i, w := range providers[pi].remotes[ri].worktrees {
			if w.name == branchName {
				wi = i
				break
			}
		}
		if wi == -1 {
			wi = len(providers[pi].remotes[ri].worktrees)
			providers[pi].remotes[ri].worktrees = append(providers[pi].remotes[ri].worktrees, treeWorktree{name: branchName})
		}
		providers[pi].remotes[ri].worktrees[wi].sessions = append(providers[pi].remotes[ri].worktrees[wi].sessions, s)
	}

	sel := newSelectionGrid(providers)
	return &TreeSelectPage{
		title:           title,
		providers:       providers,
		sessionSel:      sel,
		sessionConflict: newSelectionGrid(providers),
		viewHeight:      15,
		keymap:          DefaultTreeKeyMap,
	}
}

func (p *TreeSelectPage) Title() string    { return p.title }
func (p *TreeSelectPage) IsComplete() bool { return p.confirmed }
func (p *TreeSelectPage) Init() tea.Cmd    { return nil }

func (p *TreeSelectPage) SelectedProviders() []string {
	var out []string
	for pi, prov := range p.providers {
		if p.providerState(pi) != treeUnchecked {
			out = append(out, prov.name)
		}
	}
	return out
}

// sessionCheckbox renders one session's box. It has THREE states, not two: a
// session the saved selection cannot decide is still selected — unticking it
// would delete it from the configuration on confirm — but a plain tick would
// claim it will be ingested, and it will not be. The marker is the difference
// between the two claims.
//
// An explicitly unticked box wins over the marker: the user's own answer is not
// overridden by a note about the configuration they are editing.
func (p *TreeSelectPage) sessionCheckbox(pi, ri, wi, si int) string {
	switch {
	case !p.sessionSel[pi][ri][wi][si]:
		return CheckboxUnchecked.Render("[ ]")
	case p.sessionConflicted(pi, ri, wi, si):
		return CheckboxConflict.Render("[!]")
	default:
		return CheckboxChecked.Render("[✓]")
	}
}

// sessionConflicted is the single authority for "this box must be drawn [!]",
// used by the session row and by every ancestor row above it so the two cannot
// disagree. An explicitly cleared box is
// the user's own answer and is not overridden by a note about the configuration
// they are editing, so a cleared session contributes no conflict to anything.
func (p *TreeSelectPage) sessionConflicted(pi, ri, wi, si int) bool {
	return p.sessionSel[pi][ri][wi][si] && p.sessionConflict[pi][ri][wi][si]
}

// markSessionConflicted records that the saved selection disagrees about this
// session. It is a DISPLAY distinction only: SelectedSessions still returns it.
func (p *TreeSelectPage) markSessionConflicted(pi, ri, wi, si int) {
	p.sessionConflict[pi][ri][wi][si] = true
}

func (p *TreeSelectPage) SelectedSessions() []SessionListing {
	var selected []SessionListing
	for pi, prov := range p.providers {
		for ri, rem := range prov.remotes {
			for wi, wt := range rem.worktrees {
				for si, sess := range wt.sessions {
					if p.sessionSel[pi][ri][wi][si] {
						selected = append(selected, sess)
					}
				}
			}
		}
	}
	return selected
}

func (p *TreeSelectPage) Reset() {
	p.confirmed = false
	p.confirmingSelection = false
	p.showingHelp = false
	p.filter.Clear()
	// Preserve checkbox and expand state so users don't lose selections on back-navigation.
}

func (p *TreeSelectPage) IsConfirming() bool  { return p.confirmingSelection }
func (p *TreeSelectPage) IsSearching() bool   { return p.filter.Active }
func (p *TreeSelectPage) IsShowingHelp() bool { return p.showingHelp }

// newSelectionGrid allocates a 4-dimensional boolean grid matching the tree shape.
func newSelectionGrid(providers []treeProvider) selectionGrid {
	sel := make(selectionGrid, len(providers))
	for pi, prov := range providers {
		sel[pi] = make([][][]bool, len(prov.remotes))
		for ri, rem := range prov.remotes {
			sel[pi][ri] = make([][]bool, len(rem.worktrees))
			for wi, wt := range rem.worktrees {
				sel[pi][ri][wi] = make([]bool, len(wt.sessions))
			}
		}
	}
	return sel
}

// aggregateCheckState reduces a sequence of child states into a single parent state.
func aggregateCheckState(childState func(i int) treeCheckState, count int) treeCheckState {
	any, allChecked := false, true
	for i := 0; i < count; i++ {
		switch childState(i) {
		case treeChecked:
			any = true
		case treePartial:
			any = true
			allChecked = false
		case treeUnchecked:
			allChecked = false
		}
	}
	if allChecked && any {
		return treeChecked
	}
	if any {
		return treePartial
	}
	return treeUnchecked
}

func (p *TreeSelectPage) worktreeState(pi, ri, wi int) treeCheckState {
	sel := p.sessionSel[pi][ri][wi]
	if len(sel) == 0 {
		return treeUnchecked
	}
	return aggregateCheckState(func(i int) treeCheckState {
		if sel[i] {
			return treeChecked
		}
		return treeUnchecked
	}, len(sel))
}

func (p *TreeSelectPage) remoteState(pi, ri int) treeCheckState {
	wts := p.providers[pi].remotes[ri].worktrees
	return aggregateCheckState(func(i int) treeCheckState {
		return p.worktreeState(pi, ri, i)
	}, len(wts))
}

func (p *TreeSelectPage) providerState(pi int) treeCheckState {
	rems := p.providers[pi].remotes
	return aggregateCheckState(func(i int) treeCheckState {
		return p.remoteState(pi, i)
	}, len(rems))
}

// worktreeConflicted / remoteConflicted / providerConflicted answer, for a group
// row, whether anything under it is ticked-but-undecidable.
//
// They exist because the tick state alone cannot be drawn honestly on a group
// row: [✓] on a collapsed group says every session under it will be ingested,
// and [~] says some will, and both are false when the only thing under them is
// a session the saved selection cannot decide. The group therefore carries the
// same [!] its descendant does, which is why the marker survives collapse.
//
// This is deliberately a MARKER, not a derived third tick state: the group's
// tick state is unchanged (SelectedProviders and the space keypress still read
// it), and how a group EXPLAINS a conflict remains the deferred information-
// architecture question. Precedence is over both [✓] and [~], because the
// choice is between a row that over-claims and a row that says "open me".
func (p *TreeSelectPage) worktreeConflicted(pi, ri, wi int) bool {
	for si := range p.sessionSel[pi][ri][wi] {
		if p.sessionConflicted(pi, ri, wi, si) {
			return true
		}
	}
	return false
}

func (p *TreeSelectPage) remoteConflicted(pi, ri int) bool {
	for wi := range p.providers[pi].remotes[ri].worktrees {
		if p.worktreeConflicted(pi, ri, wi) {
			return true
		}
	}
	return false
}

func (p *TreeSelectPage) providerConflicted(pi int) bool {
	for ri := range p.providers[pi].remotes {
		if p.remoteConflicted(pi, ri) {
			return true
		}
	}
	return false
}

func (p *TreeSelectPage) flatItems() []treeFlatItem {
	var items []treeFlatItem
	filter := p.filter.NormalizedFilter()
	for pi, prov := range p.providers {
		if filter != "" && !p.providerMatchesFilter(pi, filter) {
			continue
		}
		items = append(items, treeFlatItem{level: treeLevelProvider, providerIdx: pi})
		if !prov.expanded {
			continue
		}
		for ri, rem := range prov.remotes {
			if filter != "" && !p.remoteMatchesFilter(pi, ri, filter) {
				continue
			}
			items = append(items, treeFlatItem{level: treeLevelRemote, providerIdx: pi, remoteIdx: ri})
			if !rem.expanded {
				continue
			}
			for wi, wt := range rem.worktrees {
				if filter != "" && !p.worktreeMatchesFilter(pi, ri, wi, filter) {
					continue
				}
				items = append(items, treeFlatItem{level: treeLevelWorktree, providerIdx: pi, remoteIdx: ri, worktreeIdx: wi})
				if !wt.expanded {
					continue
				}
				for si, sess := range wt.sessions {
					if filter != "" && !sessionMatchesFilter(sess, filter) {
						continue
					}
					items = append(items, treeFlatItem{level: treeLevelSession, providerIdx: pi, remoteIdx: ri, worktreeIdx: wi, sessionIdx: si})
				}
			}
		}
	}
	return items
}

func sessionMatchesFilter(s SessionListing, filter string) bool {
	return search.MatchesAny(filter, s.Title, s.ProjectName, s.Harness, s.SessionID, projectlabel.Label(s.GitRemote, s.ProjectName), s.Branch)
}

func (p *TreeSelectPage) worktreeMatchesFilter(pi, ri, wi int, filter string) bool {
	wt := p.providers[pi].remotes[ri].worktrees[wi]
	if search.MatchesAny(filter, wt.name) {
		return true
	}
	for _, sess := range wt.sessions {
		if sessionMatchesFilter(sess, filter) {
			return true
		}
	}
	return false
}

func (p *TreeSelectPage) remoteMatchesFilter(pi, ri int, filter string) bool {
	rem := p.providers[pi].remotes[ri]
	if search.MatchesAny(filter, rem.name) {
		return true
	}
	for wi := range rem.worktrees {
		if p.worktreeMatchesFilter(pi, ri, wi, filter) {
			return true
		}
	}
	return false
}

func (p *TreeSelectPage) providerMatchesFilter(pi int, filter string) bool {
	prov := p.providers[pi]
	if search.MatchesAny(filter, prov.name) {
		return true
	}
	for ri := range prov.remotes {
		if p.remoteMatchesFilter(pi, ri, filter) {
			return true
		}
	}
	return false
}

func (p *TreeSelectPage) toggleItem(item treeFlatItem) {
	pi, ri, wi, si := item.providerIdx, item.remoteIdx, item.worktreeIdx, item.sessionIdx
	switch item.level {
	case treeLevelProvider:
		target := p.providerState(pi) != treeChecked
		for ri2 := range p.providers[pi].remotes {
			for wi2 := range p.providers[pi].remotes[ri2].worktrees {
				for si2 := range p.sessionSel[pi][ri2][wi2] {
					p.sessionSel[pi][ri2][wi2][si2] = target
				}
			}
		}
	case treeLevelRemote:
		target := p.remoteState(pi, ri) != treeChecked
		for wi2 := range p.providers[pi].remotes[ri].worktrees {
			for si2 := range p.sessionSel[pi][ri][wi2] {
				p.sessionSel[pi][ri][wi2][si2] = target
			}
		}
	case treeLevelWorktree:
		target := p.worktreeState(pi, ri, wi) != treeChecked
		for si2 := range p.sessionSel[pi][ri][wi] {
			p.sessionSel[pi][ri][wi][si2] = target
		}
	case treeLevelSession:
		p.sessionSel[pi][ri][wi][si] = !p.sessionSel[pi][ri][wi][si]
	}
}

func (p *TreeSelectPage) clamp(flatLen int) {
	if flatLen == 0 {
		p.cursor = 0
		p.offset = 0
		return
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= flatLen {
		p.cursor = flatLen - 1
	}
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+p.viewHeight {
		p.offset = p.cursor - p.viewHeight + 1
	}
	if p.offset < 0 {
		p.offset = 0
	}
}

func (p *TreeSelectPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if h := msg.Height - treeOverhead; h > 5 {
			p.viewHeight = h
		}
		return p, nil

	case tea.KeyMsg:
		if p.filter.Active {
			return p.updateSearchMode(msg)
		}

		if p.showingHelp {
			if key.Matches(msg, p.keymap.Help) || key.Matches(msg, p.keymap.Cancel) {
				p.showingHelp = false
			}
			return p, nil
		}

		if p.confirmingSelection {
			switch {
			case key.Matches(msg, p.keymap.Confirm):
				p.confirmed = true
			case key.Matches(msg, p.keymap.Cancel):
				p.confirmingSelection = false
			case msg.String() == defaults.KeyBack.String():
				p.confirmingSelection = false
			}
			return p, nil
		}

		items := p.flatItems()
		switch {
		case key.Matches(msg, p.keymap.Cancel):
			if p.filter.HasFilter() {
				p.filter.Clear()
				p.cursor = 0
				p.offset = 0
				p.clamp(len(p.flatItems()))
			}
		case key.Matches(msg, p.keymap.Help):
			p.showingHelp = true
			return p, nil
		case key.Matches(msg, p.keymap.Search):
			p.filter.Enter()
			return p, nil
		case key.Matches(msg, p.keymap.Up):
			p.cursor--
			p.clamp(len(items))
		case key.Matches(msg, p.keymap.Down):
			p.cursor++
			p.clamp(len(items))
		case key.Matches(msg, p.keymap.PageDown):
			p.cursor += p.viewHeight
			p.clamp(len(items))
		case key.Matches(msg, p.keymap.PageUp):
			p.cursor -= p.viewHeight
			p.clamp(len(items))
		case key.Matches(msg, p.keymap.JumpTop):
			p.cursor = 0
			p.clamp(len(items))
		case key.Matches(msg, p.keymap.JumpBottom):
			p.cursor = len(items) - 1
			p.clamp(len(items))
		case key.Matches(msg, p.keymap.Select):
			if p.cursor < len(items) {
				p.toggleItem(items[p.cursor])
			}
		case key.Matches(msg, p.keymap.Expand):
			if p.cursor < len(items) {
				cur := items[p.cursor]
				switch cur.level {
				case treeLevelProvider:
					p.providers[cur.providerIdx].expanded = true
				case treeLevelRemote:
					p.providers[cur.providerIdx].remotes[cur.remoteIdx].expanded = true
				case treeLevelWorktree:
					p.providers[cur.providerIdx].remotes[cur.remoteIdx].worktrees[cur.worktreeIdx].expanded = true
				case treeLevelSession:
					// No-op: sessions cannot expand.
				}
				p.clamp(len(p.flatItems()))
			}
		case key.Matches(msg, p.keymap.Collapse):
			if p.cursor < len(items) {
				cur := items[p.cursor]
				switch cur.level {
				case treeLevelProvider:
					p.providers[cur.providerIdx].expanded = false
				case treeLevelRemote:
					p.providers[cur.providerIdx].remotes[cur.remoteIdx].expanded = false
				case treeLevelWorktree:
					p.providers[cur.providerIdx].remotes[cur.remoteIdx].worktrees[cur.worktreeIdx].expanded = false
				case treeLevelSession:
					p.providers[cur.providerIdx].remotes[cur.remoteIdx].worktrees[cur.worktreeIdx].expanded = false
				}
				p.clamp(len(p.flatItems()))
			}
		case key.Matches(msg, p.keymap.PrevSibling):
			if p.cursor < len(items) {
				cur := items[p.cursor]
				switch cur.level {
				case treeLevelProvider:
					for i := p.cursor - 1; i >= 0; i-- {
						if items[i].level == treeLevelProvider {
							p.cursor = i
							break
						}
					}
				case treeLevelRemote:
					for i := p.cursor - 1; i >= 0; i-- {
						if items[i].providerIdx != cur.providerIdx {
							break
						}
						if items[i].level == treeLevelRemote && items[i].remoteIdx != cur.remoteIdx {
							p.cursor = i
							break
						}
					}
				case treeLevelWorktree:
					for i := p.cursor - 1; i >= 0; i-- {
						if items[i].providerIdx != cur.providerIdx || items[i].remoteIdx != cur.remoteIdx {
							break
						}
						if items[i].level == treeLevelWorktree && items[i].worktreeIdx != cur.worktreeIdx {
							p.cursor = i
							break
						}
					}
				case treeLevelSession:
					for i := p.cursor - 1; i >= 0; i-- {
						if items[i].level != treeLevelSession {
							p.cursor = i
							break
						}
					}
				}
				p.clamp(len(items))
			}
		case key.Matches(msg, p.keymap.NextSibling):
			if p.cursor < len(items) {
				cur := items[p.cursor]
				switch cur.level {
				case treeLevelProvider:
					for i := p.cursor + 1; i < len(items); i++ {
						if items[i].level == treeLevelProvider {
							p.cursor = i
							break
						}
					}
				case treeLevelRemote:
					for i := p.cursor + 1; i < len(items); i++ {
						if items[i].providerIdx != cur.providerIdx {
							break
						}
						if items[i].level == treeLevelRemote && items[i].remoteIdx != cur.remoteIdx {
							p.cursor = i
							break
						}
					}
				case treeLevelWorktree:
					for i := p.cursor + 1; i < len(items); i++ {
						if items[i].providerIdx != cur.providerIdx || items[i].remoteIdx != cur.remoteIdx {
							break
						}
						if items[i].level == treeLevelWorktree && items[i].worktreeIdx != cur.worktreeIdx {
							p.cursor = i
							break
						}
					}
				case treeLevelSession:
					for i := p.cursor + 1; i < len(items); i++ {
						if items[i].level != treeLevelSession {
							p.cursor = i
							break
						}
					}
				}
				p.clamp(len(items))
			}
		case key.Matches(msg, p.keymap.Confirm):
			p.confirmingSelection = true
		}
	}
	return p, nil
}

func (p *TreeSelectPage) updateSearchMode(msg tea.KeyMsg) (Page, tea.Cmd) {
	switch {
	case key.Matches(msg, p.keymap.Cancel):
		p.filter.Clear()
		p.cursor = 0
		p.offset = 0
		p.clamp(len(p.flatItems()))
	case key.Matches(msg, p.keymap.Confirm):
		p.filter.Exit()
		p.clamp(len(p.flatItems()))
	case msg.Type == tea.KeyBackspace:
		if p.filter.HasFilter() {
			p.filter.Backspace()
			p.cursor = 0
			p.offset = 0
			p.clamp(len(p.flatItems()))
		}
	default:
		if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 {
			p.filter.Append(msg.Runes[0])
			p.cursor = 0
			p.offset = 0
			p.clamp(len(p.flatItems()))
		}
	}
	return p, nil
}

func (p *TreeSelectPage) helpBarText() string {
	return treeStatusBar()
}

func totalSessionsUnderProvider(prov treeProvider) int {
	total := 0
	for _, rem := range prov.remotes {
		for _, wt := range rem.worktrees {
			total += len(wt.sessions)
		}
	}
	return total
}

func totalSessionsUnderRemote(rem treeRemote) int {
	total := 0
	for _, wt := range rem.worktrees {
		total += len(wt.sessions)
	}
	return total
}

// renderGroupRow renders a checkbox + arrow + label row for provider, remote, or worktree levels.
func renderGroupRow(state treeCheckState, conflicted bool, expanded bool, indent string, label string, active bool) string {
	var check string
	switch {
	case conflicted:
		check = CheckboxConflict.Render("[!]")
	case state == treeChecked:
		check = CheckboxChecked.Render("[✓]")
	case state == treePartial:
		check = CheckboxChecked.Render("[~]")
	default:
		check = CheckboxUnchecked.Render("[ ]")
	}
	arrow := "▷"
	if expanded {
		arrow = "▽"
	}
	fullLabel := arrow + " " + label
	if active {
		fullLabel = OptionSelected.Render(fullLabel)
	} else {
		fullLabel = OptionUnselected.Render(fullLabel)
	}
	cur := TextBg.Render("  ")
	if active {
		cur = OptionCursor.Render("▸ ")
	}
	return indent + cur + check + TextBg.Render(" ") + fullLabel + TextBg.Render("\n")
}

func (p *TreeSelectPage) View(width, height int) string {
	var b strings.Builder

	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))

	if p.showingHelp {
		b.WriteString(renderHelpOverlay(treeHelpCategories(p.keymap)))
		return b.String()
	}

	if p.confirmingSelection {
		selected := p.SelectedSessions()
		if len(selected) == 0 {
			b.WriteString(DescriptionStyle.Render("No sessions selected. Import will be skipped."))
			b.WriteString(TextBg.Render("\n"))
		} else {
			provCounts := map[string]int{}
			for _, s := range selected {
				provCounts[s.Harness]++
			}
			b.WriteString(DescriptionStyle.Render(fmt.Sprintf("You will ingest %d sessions:", len(selected))))
			b.WriteString(TextBg.Render("\n"))
			for prov, count := range provCounts {
				b.WriteString(DescriptionStyle.Render(fmt.Sprintf("  • %s: %d sessions", prov, count)))
				b.WriteString(TextBg.Render("\n"))
			}
		}
		b.WriteString(TextBg.Render("\n"))
		b.WriteString(HelpBar.Render("enter: confirm  esc/b: cancel"))
		return b.String()
	}

	items := p.flatItems()

	if len(items) == 0 {
		b.WriteString(DescriptionStyle.Render("No transcripts found. You can import later with 'peasant ingest'."))
		b.WriteString(TextBg.Render("\n\n"))
		b.WriteString(HelpBar.Render("enter: continue"))
		return b.String()
	}

	start := p.offset
	end := p.offset + p.viewHeight
	if end > len(items) {
		end = len(items)
	}

	if p.offset > 0 {
		b.WriteString(HelpBar.Render(fmt.Sprintf("  ↑ %d more", p.offset)))
		b.WriteString(TextBg.Render("\n"))
	}

	for i := start; i < end; i++ {
		item := items[i]
		active := p.cursor == i
		pi, ri, wi, si := item.providerIdx, item.remoteIdx, item.worktreeIdx, item.sessionIdx

		switch item.level {
		case treeLevelProvider:
			prov := p.providers[pi]
			totalSessions := totalSessionsUnderProvider(prov)
			label := fmt.Sprintf("%s (%d sessions)", schema.HarnessDisplayName(defaults.Harness(prov.name)), totalSessions)
			b.WriteString(renderGroupRow(p.providerState(pi), p.providerConflicted(pi), prov.expanded, "", label, active))

		case treeLevelRemote:
			rem := p.providers[pi].remotes[ri]
			totalSessions := totalSessionsUnderRemote(rem)
			label := fmt.Sprintf("%s (%d)", rem.name, totalSessions)
			b.WriteString(renderGroupRow(p.remoteState(pi, ri), p.remoteConflicted(pi, ri), rem.expanded, TextBg.Render("  "), label, active))

		case treeLevelWorktree:
			wt := p.providers[pi].remotes[ri].worktrees[wi]
			label := fmt.Sprintf("%s (%d)", wt.name, len(wt.sessions))
			b.WriteString(renderGroupRow(p.worktreeState(pi, ri, wi), p.worktreeConflicted(pi, ri, wi), wt.expanded, TextBg.Render("    "), label, active))

		case treeLevelSession:
			s := p.providers[pi].remotes[ri].worktrees[wi].sessions[si]
			cur := TextBg.Render("  ")
			if active {
				cur = OptionCursor.Render("▸ ")
			}
			check := p.sessionCheckbox(pi, ri, wi, si)
			label := s.Date.Format("Jan 02, 2006 15:04")
			if s.TurnCount > 0 {
				label = fmt.Sprintf("%s (%d turns)", label, s.TurnCount)
			}
			if s.Title != "" {
				label = fmt.Sprintf("%s — %s", label, s.Title)
			}
			if active {
				label = OptionSelected.Render(label)
			} else {
				label = OptionUnselected.Render(label)
			}
			b.WriteString(TextBg.Render("      ") + cur + check + TextBg.Render(" ") + label + TextBg.Render("\n"))

		}
	}

	if end < len(items) {
		b.WriteString(HelpBar.Render(fmt.Sprintf("  ↓ %d more", len(items)-end)))
		b.WriteString(TextBg.Render("\n"))
	}

	b.WriteString(TextBg.Render("\n"))
	if p.filter.Active {
		b.WriteString(HelpBar.Render(fmt.Sprintf("Search: %s█  (enter: keep filter  esc: clear)", p.filter.Text)))
	} else if p.filter.HasFilter() {
		b.WriteString(HelpBar.Render(fmt.Sprintf("filter: %q", p.filter.Text)))
		b.WriteString(TextBg.Render("\n"))
		b.WriteString(p.helpBarText())
	} else {
		b.WriteString(p.helpBarText())
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// MultiSelectPage
// ---------------------------------------------------------------------------

type MultiSelectPage struct {
	title     string
	items     []string
	checked   []bool
	cursor    int
	confirmed bool
}

func NewMultiSelect(title string, items []string) *MultiSelectPage {
	return &MultiSelectPage{title: title, items: items, checked: make([]bool, len(items))}
}

func (p *MultiSelectPage) Title() string    { return p.title }
func (p *MultiSelectPage) IsComplete() bool { return p.confirmed }
func (p *MultiSelectPage) Init() tea.Cmd    { return nil }

func (p *MultiSelectPage) SelectedIndices() []int {
	var indices []int
	for i, c := range p.checked {
		if c {
			indices = append(indices, i)
		}
	}
	return indices
}

func (p *MultiSelectPage) Reset() {
	p.confirmed = false
	for i := range p.checked {
		p.checked[i] = false
	}
}

func (p *MultiSelectPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case defaults.KeyUp.String(), defaults.KeyVimUp.String():
			if p.cursor > 0 {
				p.cursor--
			}
		case defaults.KeyDown.String(), defaults.KeyVimDown.String():
			if p.cursor < len(p.items)-1 {
				p.cursor++
			}
		case defaults.KeySpace.String():
			p.checked[p.cursor] = !p.checked[p.cursor]
		case defaults.KeyEnter.String():
			p.checked[p.cursor] = !p.checked[p.cursor]
		case defaults.KeyTab.String():
			p.confirmed = true
		default:
			if len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= '9' {
				idx := int(msg.String()[0] - '1')
				if idx < len(p.items) {
					p.checked[idx] = !p.checked[idx]
				}
			}
		}
	}
	return p, nil
}

func (p *MultiSelectPage) View(width, height int) string {
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))
	for i, item := range p.items {
		cursor := TextBg.Render("  ")
		if i == p.cursor {
			cursor = OptionCursor.Render("▸ ")
		}
		check := CheckboxUnchecked.Render("[ ]")
		if p.checked[i] {
			check = CheckboxChecked.Render("[✓]")
		}
		num := fmt.Sprintf("%d) ", i+1)
		label := item
		if i == p.cursor {
			label = OptionSelected.Render(label)
		} else {
			label = OptionUnselected.Render(label)
		}
		b.WriteString(cursor + check + TextBg.Render(" "+num) + label + TextBg.Render("\n"))
	}
	b.WriteString(TextBg.Render("\n"))
	b.WriteString(HelpBar.Render("space: toggle  enter: confirm"))
	return b.String()
}

// ---------------------------------------------------------------------------
// InfoPage
// ---------------------------------------------------------------------------

type InfoPage struct {
	title     string
	content   string
	confirmed bool
}

func NewInfoPage(title, content string) *InfoPage {
	return &InfoPage{title: title, content: content}
}

func (p *InfoPage) Title() string    { return p.title }
func (p *InfoPage) IsComplete() bool { return p.confirmed }
func (p *InfoPage) Init() tea.Cmd    { return nil }

func (p *InfoPage) SetContent(content string) { p.content = content }

func (p *InfoPage) Reset() { p.confirmed = false }

func (p *InfoPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == defaults.KeyEnter.String() {
			p.confirmed = true
		}
	}
	return p, nil
}

func (p *InfoPage) View(width, height int) string {
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))
	b.WriteString(DescriptionStyle.Render(p.content))
	b.WriteString(TextBg.Render("\n\n"))
	b.WriteString(HelpBar.Render("enter: confirm"))
	return b.String()
}

// ---------------------------------------------------------------------------
// OAuthPage
// ---------------------------------------------------------------------------

type loginResultMsg struct {
	username string
	err      error
}

type OAuthPage struct {
	title               string
	description         string
	options             []string
	descriptions        []string
	cursor              int
	selected            int
	authInProgress      bool
	reAuthLogoutPending bool
	authResult          *loginResultMsg
	villageURL          string
	existingUser        string
	openBrowser         func(string) error
	browserErr          error
}

func NewOAuthPage(title, description string, options, descriptions []string, villageURL, existingUser string) *OAuthPage {
	return &OAuthPage{
		title: title, description: description, options: options, descriptions: descriptions,
		cursor: 0, selected: -1, villageURL: villageURL,
		existingUser: existingUser, openBrowser: browser.Open,
	}
}

func (p *OAuthPage) Title() string { return p.title }
func (p *OAuthPage) Init() tea.Cmd { return nil }

func (p *OAuthPage) IsComplete() bool {
	if p.existingUser != "" {
		switch p.selected {
		case 0:
			return true
		case 1:
			return p.authResult != nil && p.authResult.err == nil
		case 2:
			return true
		}
		return false
	}
	if p.selected == 1 {
		return true
	}
	return p.selected == 0 && p.authResult != nil && p.authResult.err == nil
}

func (p *OAuthPage) IsConnected() bool {
	if p.existingUser != "" {
		switch p.selected {
		case 0:
			return true
		case 1:
			return p.authResult != nil && p.authResult.err == nil
		}
		return false
	}
	return p.selected == 0 && p.authResult != nil && p.authResult.err == nil
}

func (p *OAuthPage) Reset() {
	p.selected = -1
	p.cursor = 0
	p.authInProgress = false
	p.reAuthLogoutPending = false
	p.authResult = nil
}

func (p *OAuthPage) displayOptions() ([]string, []string) {
	if p.existingUser != "" {
		return []string{
				fmt.Sprintf("Continue as %s", p.existingUser),
				"Log in with a different account",
				"Stay local",
			}, []string{
				"Already authenticated — no action needed",
				"Authenticate with a different GitHub account",
				"All data stays on your machine",
			}
	}
	return p.options, p.descriptions
}

func (p *OAuthPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	opts, _ := p.displayOptions()
	switch msg := msg.(type) {
	case loginResultMsg:
		p.authInProgress = false
		p.authResult = &msg
		if msg.err != nil {
			p.selected = -1
		}
		return p, nil
	case tea.KeyMsg:
		if p.reAuthLogoutPending {
			switch msg.String() {
			case defaults.KeyEnter.String():
				p.reAuthLogoutPending = false
				p.authInProgress = true
				url := p.villageURL
				return p, func() tea.Msg {
					if err := auth.ClearCredentials(); err != nil {
						return loginResultMsg{err: fmt.Errorf("clear credentials: %w", err)}
					}
					creds, err := auth.Login(context.Background(), url, false)
					if err != nil {
						return loginResultMsg{err: err}
					}
					return loginResultMsg{username: creds.Username}
				}
			case defaults.KeyEscape.String():
				p.reAuthLogoutPending = false
				p.selected = -1
			}
			return p, nil
		}
		if p.authInProgress {
			if msg.String() == defaults.KeyEscape.String() {
				p.authInProgress = false
				p.selected = -1
			}
			return p, nil
		}
		switch msg.String() {
		case defaults.KeyUp.String(), defaults.KeyVimUp.String():
			if p.cursor > 0 {
				p.cursor--
			}
		case defaults.KeyDown.String(), defaults.KeyVimDown.String():
			if p.cursor < len(opts)-1 {
				p.cursor++
			}
		case defaults.KeyEnter.String():
			p.selected = p.cursor
			return p, p.handleSelection()
		default:
			if len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= '9' {
				idx := int(msg.String()[0] - '1')
				if idx < len(opts) {
					p.selected = idx
					return p, p.handleSelection()
				}
			}
		}
	}
	return p, nil
}

func (p *OAuthPage) handleSelection() tea.Cmd {
	connectIdx := 0
	if p.existingUser != "" {
		connectIdx = 1
	}
	if p.selected == connectIdx {
		if p.existingUser != "" {
			p.reAuthLogoutPending = true
			// Surface a browser-launch failure in the page so the user is told
			// to open the sign-out URL manually instead of waiting forever.
			p.browserErr = p.openBrowser("https://github.com/logout")
			return nil
		}
		p.authInProgress = true
		url := p.villageURL
		return func() tea.Msg {
			creds, err := auth.Login(context.Background(), url, false)
			if err != nil {
				return loginResultMsg{err: err}
			}
			return loginResultMsg{username: creds.Username}
		}
	}
	return nil
}

func (p *OAuthPage) View(width, height int) string {
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))
	if p.description != "" {
		b.WriteString(DescriptionStyle.Render(p.description))
		b.WriteString(TextBg.Render("\n\n"))
	}
	if p.reAuthLogoutPending {
		if p.browserErr != nil {
			b.WriteString(OptionSelected.Render("Could not open your browser automatically."))
			b.WriteString(TextBg.Render("\n\n"))
			b.WriteString(DescriptionStyle.Render(fmt.Sprintf("%s\n\nOpen this URL manually to sign out: https://github.com/logout", p.browserErr)))
		} else {
			b.WriteString(OptionSelected.Render("GitHub sign-out page opened in your browser."))
			b.WriteString(TextBg.Render("\n\n"))
			b.WriteString(DescriptionStyle.Render("Click \"Sign out\" on that page, then press Enter here to continue."))
		}
		b.WriteString(TextBg.Render("\n\n"))
		b.WriteString(HelpBar.Render("enter: continue  esc: cancel"))
		return b.String()
	}
	if p.authInProgress {
		b.WriteString(OptionSelected.Render("Opening browser for GitHub authentication..."))
		b.WriteString(TextBg.Render("\n\n"))
		b.WriteString(HelpBar.Render("Waiting for login to complete. Press Esc to cancel."))
		return b.String()
	}
	if p.authResult != nil && p.authResult.err == nil {
		b.WriteString(OptionSelected.Render(fmt.Sprintf("Logged in as %s!", p.authResult.username)))
		return b.String()
	}
	if p.authResult != nil && p.authResult.err != nil {
		b.WriteString(DescriptionStyle.Render(fmt.Sprintf("Login failed: %s", p.authResult.err)))
		b.WriteString(TextBg.Render("\n\n"))
	}
	opts, descs := p.displayOptions()
	for i, opt := range opts {
		cursor := TextBg.Render("  ")
		if i == p.cursor {
			cursor = OptionCursor.Render("▸ ")
		}
		num := fmt.Sprintf("%d) ", i+1)
		label := opt
		if i == p.cursor {
			label = OptionSelected.Render(num + label)
		} else {
			label = OptionUnselected.Render(num + label)
		}
		b.WriteString(cursor + label)
		if i < len(descs) && descs[i] != "" {
			b.WriteString(TextBg.Render("\n    "))
			b.WriteString(HelpBar.Render(descs[i]))
		}
		b.WriteString(TextBg.Render("\n"))
	}
	b.WriteString(TextBg.Render("\n"))
	b.WriteString(HelpBar.Render("↑/↓: navigate  enter: select"))
	return b.String()
}

// ---------------------------------------------------------------------------
// IngestPage
// ---------------------------------------------------------------------------

type ingestResultMsg struct {
	result *IngestResult
	err    error
}

var stageDisplayOrder = []string{
	"DISCOVER", "DIFF", "FILTER", "EXTRACT+WRITE", "DB INSERT",
	"INDEX", "COMPUTE", "ANNOTATE", "CLEANUP", "REPORT",
}

var stageLabel = map[string]string{
	"DISCOVER": "Discover", "DIFF": "Diff", "FILTER": "Filter",
	"EXTRACT+WRITE": "Extract", "DB INSERT": "DB Insert", "INDEX": "Index",
	"COMPUTE": "Compute", "ANNOTATE": "Annotate", "CLEANUP": "Cleanup", "REPORT": "Report",
}

type progressTickMsg struct{}

type IngestPage struct {
	title    string
	running  bool
	result   *IngestResult
	err      error
	done     bool
	progress ProgressSnapshot
	cancelFn context.CancelFunc
}

func NewIngestPage(title string) *IngestPage { return &IngestPage{title: title} }

func (p *IngestPage) Title() string    { return p.title }
func (p *IngestPage) IsComplete() bool { return p.done }
func (p *IngestPage) Init() tea.Cmd    { return nil }

func (p *IngestPage) Start(runner IngestRunnerFunc, answers *WizardAnswers, progress ProgressSnapshot) tea.Cmd {
	if p.cancelFn != nil {
		p.cancelFn()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFn = cancel
	p.running = true
	p.result = nil
	p.err = nil
	p.done = false
	p.progress = progress
	a := *answers
	return tea.Batch(
		func() tea.Msg {
			result, err := runner(ctx, a)
			return ingestResultMsg{result: result, err: err}
		},
		p.tickCmd(),
	)
}

func (p *IngestPage) tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return progressTickMsg{} })
}

func (p *IngestPage) Cancel() {
	if p.cancelFn != nil {
		p.cancelFn()
		p.cancelFn = nil
	}
}

func (p *IngestPage) Reset() {
	p.Cancel()
	p.running = false
	p.result = nil
	p.err = nil
	p.done = false
}

func (p *IngestPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case ingestResultMsg:
		p.running = false
		p.result = msg.result
		p.err = msg.err
		return p, nil
	case progressTickMsg:
		if p.running {
			return p, p.tickCmd()
		}
		return p, nil
	case tea.KeyMsg:
		if !p.running && msg.String() == defaults.KeyEnter.String() {
			p.done = true
		}
	}
	return p, nil
}

func (p *IngestPage) View(width, height int) string {
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))
	switch {
	case p.running:
		if p.progress != nil {
			b.WriteString(p.renderProgress())
		} else {
			b.WriteString(DescriptionStyle.Render("Ingesting your transcripts, please wait..."))
		}
	case p.err != nil:
		b.WriteString(DescriptionStyle.Render(fmt.Sprintf("Ingestion failed: %v\n\nYou can run 'peasant ingest' manually at any time to retry.", p.err)))
		b.WriteString(TextBg.Render("\n\n"))
		b.WriteString(HelpBar.Render("enter: continue"))
	case p.result != nil:
		imported := p.result.New + p.result.Updated
		b.WriteString(DescriptionStyle.Render(fmt.Sprintf("Ingestion complete!\n\n  %d sessions imported (%d new, %d updated)", imported, p.result.New, p.result.Updated)))
		for _, pc := range p.result.ProviderCounts {
			b.WriteString(DescriptionStyle.Render(fmt.Sprintf("\n    %s: %d new, %d updated", pc.Harness, pc.New, pc.Updated)))
		}
		if p.result.Unchanged > 0 {
			b.WriteString(DescriptionStyle.Render(fmt.Sprintf("\n  %d sessions skipped (unchanged)", p.result.Unchanged)))
		}
		b.WriteString(DescriptionStyle.Render(fmt.Sprintf("\n  Duration: %s", p.result.Duration.Round(100*time.Millisecond))))
		if p.result.Errors > 0 {
			b.WriteString(DescriptionStyle.Render(fmt.Sprintf("\n  %d session(s) had errors — run 'peasant ingest' to retry.", p.result.Errors)))
		}
		b.WriteString(TextBg.Render("\n\n"))
		b.WriteString(HelpBar.Render("enter: finish setup"))
	default:
		b.WriteString(DescriptionStyle.Render("Preparing ingestion..."))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// RetentionPage
// ---------------------------------------------------------------------------

// retentionOption represents a selectable cleanup period.
type retentionOption struct {
	days        int // 0 means "never clean up" (sets a very large value)
	label       string
	description string
	recommended bool
}

// retentionOptions defines the available cleanup period choices.
var retentionOptions = []retentionOption{
	{days: 30, label: "30 days", description: "Claude Code default — transcripts are deleted after 30 days of inactivity."},
	{days: 90, label: "90 days", description: "Keep transcripts for 3 months."},
	{days: 365, label: "1 year", description: "Keep transcripts for a full year."},
	{days: 0, label: "Never expire", recommended: true, description: "Transcripts are never automatically deleted. Recommended for Peasant users."},
}

// RetentionPage lets users view and change Claude Code's cleanupPeriodDays setting.
type RetentionPage struct {
	title       string
	currentDays int  // current setting (-1 = not set / using default)
	hasExisting bool // whether a value was explicitly set
	cursor      int
	confirmed   bool
}

// NewRetentionPage creates a retention page pre-loaded with the current setting.
func NewRetentionPage(title string, currentDays int, hasExisting bool) *RetentionPage {
	// Default cursor to "Never expire" (last option).
	cursor := len(retentionOptions) - 1
	if hasExisting {
		// Try to match current setting to an option.
		for i, opt := range retentionOptions {
			if opt.days == 0 && currentDays >= 9999 {
				cursor = i
				break
			}
			if opt.days == currentDays {
				cursor = i
				break
			}
		}
	}
	return &RetentionPage{
		title:       title,
		currentDays: currentDays,
		hasExisting: hasExisting,
		cursor:      cursor,
	}
}

func (p *RetentionPage) Title() string    { return p.title }
func (p *RetentionPage) IsComplete() bool { return p.confirmed }
func (p *RetentionPage) Init() tea.Cmd    { return nil }

// SelectedDays returns the chosen cleanup period in days.
// Returns 99999 for "never expire".
func (p *RetentionPage) SelectedDays() int {
	opt := retentionOptions[p.cursor]
	if opt.days == 0 {
		return 99999
	}
	return opt.days
}

func (p *RetentionPage) Reset() { p.confirmed = false }

func (p *RetentionPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case defaults.KeyUp.String(), defaults.KeyVimUp.String():
			if p.cursor > 0 {
				p.cursor--
			}
		case defaults.KeyDown.String(), defaults.KeyVimDown.String():
			if p.cursor < len(retentionOptions)-1 {
				p.cursor++
			}
		case defaults.KeyEnter.String():
			p.confirmed = true
		default:
			if len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= '4' {
				idx := int(msg.String()[0] - '1')
				if idx < len(retentionOptions) {
					p.cursor = idx
					p.confirmed = true
				}
			}
		}
	}
	return p, nil
}

func (p *RetentionPage) View(width, height int) string {
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))

	// Show current setting with warning if using the default.
	b.WriteString(DescriptionStyle.Render("Claude Code automatically deletes conversation history after a period of inactivity."))
	b.WriteString(TextBg.Render("\n"))
	b.WriteString(DescriptionStyle.Render("Peasant needs these transcripts to analyze your sessions."))
	b.WriteString(TextBg.Render("\n\n"))

	if !p.hasExisting {
		b.WriteString(OptionSelected.Render("⚠  Current setting: not set (defaults to 30 days)"))
		b.WriteString(TextBg.Render("\n"))
		b.WriteString(HelpBar.Render("   Your transcripts will be deleted after 30 days of inactivity!"))
	} else if p.currentDays >= 9999 {
		b.WriteString(DescriptionStyle.Render("   Current setting: never expire"))
	} else {
		b.WriteString(fmt.Sprintf("   Current setting: %d days", p.currentDays))
		if p.currentDays <= 30 {
			b.WriteString(TextBg.Render("\n"))
			b.WriteString(OptionSelected.Render("⚠  Your transcripts may be deleted before Peasant can analyze them!"))
		}
	}
	b.WriteString(TextBg.Render("\n\n"))

	b.WriteString(DescriptionStyle.Render("Choose how long Claude Code should keep transcripts:"))
	b.WriteString(TextBg.Render("\n\n"))

	for i, opt := range retentionOptions {
		radio := "○"
		if i == p.cursor {
			radio = "●"
		}
		cursor := TextBg.Render("  ")
		if i == p.cursor {
			cursor = OptionCursor.Render("▸ ")
		}
		label := opt.label
		if opt.recommended {
			label += " (recommended)"
		}
		if i == p.cursor {
			b.WriteString(cursor + OptionSelected.Render(radio+" "+label))
		} else {
			b.WriteString(cursor + OptionUnselected.Render(radio+" "+label))
		}
		b.WriteString(TextBg.Render("\n"))
		b.WriteString(TextBg.Render("      "))
		b.WriteString(HelpBar.Render(opt.description))
		b.WriteString(TextBg.Render("\n\n"))
	}

	b.WriteString(HelpBar.Render("This updates cleanupPeriodDays in ~/.claude/settings.json"))
	b.WriteString(TextBg.Render("\n\n"))
	b.WriteString(HelpBar.Render("↑/↓: navigate  enter: confirm"))
	return b.String()
}

// ---------------------------------------------------------------------------
// PrivacyPreferencePage
// ---------------------------------------------------------------------------

type privacyOption struct {
	level       redact.RedactionLevel
	label       string
	recommended bool
	description string
	// keeps names what redaction deliberately does NOT touch. It is a statement
	// about scope rather than about output, so unlike the before/after examples it
	// cannot be derived by running the redactor - nothing is produced to compare.
	keeps string
}

// privacyOptions offers exactly the levels a user may choose.
//
// It is one option, and that is the point rather than an accident of trimming.
// Maximum is absent because every command refuses to run under it, so a wizard
// that could write it would produce a configuration that breaks the next import
// and the next upload. Minimal is absent because it protects less than Standard
// and offering a weaker choice on an onboarding screen invites a user to pick it
// without understanding what they gave up; a configuration that already carries it
// keeps working and is raised, which the review screen discloses.
//
// The order matches config.OfferedRedactionLevels, and
// TestPrivacyPreferencePage_OffersOnlyLevelsTheProductOffers keeps the two from
// drifting apart in either direction.
var privacyOptions = []privacyOption{
	{
		level:       redact.Standard,
		label:       "Standard",
		recommended: true,
		description: "Redacts secrets, personal data, and username paths it recognizes. Pattern matching is best effort: it cannot find every one.",
		keeps:       "Keeps: code content, project names, tool names",
	},
}

// The wizard must offer exactly the levels the product offers - no more, so its
// output cannot break the next command, and no fewer, so a level that becomes
// available cannot be silently withheld.
//
// That invariant is held by TestPrivacyPreferencePage_OffersOnlyLevelsTheProductOffers,
// which checks SET containment in both directions and names the offending level.
// It was additionally enforced by two package init() panics, and those are gone:
//
//   - They aborted the SHIPPED BINARY, not the tests. cmd_kickstart.go imports this
//     package, so the init ran on every invocation: a safe additive change to
//     internal/config alone - widening the offered menu, which moves the product
//     towards offering MORE - built cleanly and then made `peasant version` panic.
//     A static developer-facing invariant should not be able to take a user's
//     unrelated command down.
//   - The count check was strictly weaker than the test beside it. Widen the menu
//     and give this list a duplicate Standard entry and both panics pass at 2 == 2
//     while the test correctly reports that the page withholds a level the product
//     offers.
//
// So they cost a startup abort and caught a subset of what already fails. The test
// is the guard.

type PrivacyPreferencePage struct {
	title     string
	cursor    int
	confirmed bool
	// examples are the before/after lines for the recommended level, PRODUCED by
	// running the real redactor at construction rather than written down.
	examples []string
}

// NewPrivacyPreferencePage builds the screen, deriving its examples from the
// redactor it is describing.
//
// When the examples cannot be produced the screen shows NONE rather than showing
// something. That is the fail-safe direction and it is deliberate: an empty
// examples block understates what redaction does, which costs the user nothing,
// while a stale or invented one over-claims protection - which is exactly the
// defect this replaced, where the screen promised a whole path was withheld when
// only the username segment is. TestPrivacyPreferencePage_DerivesItsExamples
// asserts the shipped rules never take this path, so it cannot degrade in silence.
func NewPrivacyPreferencePage(title string) *PrivacyPreferencePage {
	page := &PrivacyPreferencePage{title: title, cursor: recommendedPrivacyOption()}
	if lines, err := privacyExamples(privacyOptions[page.cursor].level); err == nil {
		page.examples = lines
	}
	return page
}

// recommendedPrivacyOption is the option the cursor starts on: the recommended
// one, found by its own flag.
//
// It used to be the literal index 1, which was the recommended option only for as
// long as the list had exactly the two entries it had when that was written.
// Removing the first entry would have left the cursor one past the end and
// panicked on the first read of SelectedLevel.
func recommendedPrivacyOption() int {
	for index, option := range privacyOptions {
		if option.recommended {
			return index
		}
	}
	return 0
}

func (p *PrivacyPreferencePage) Title() string    { return p.title }
func (p *PrivacyPreferencePage) IsComplete() bool { return p.confirmed }
func (p *PrivacyPreferencePage) Init() tea.Cmd    { return nil }

func (p *PrivacyPreferencePage) SelectedLevel() string {
	return string(privacyOptions[p.cursor].level)
}

func (p *PrivacyPreferencePage) Reset() { p.confirmed = false }

func (p *PrivacyPreferencePage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case defaults.KeyUp.String(), defaults.KeyVimUp.String():
			if p.cursor > 0 {
				p.cursor--
			}
		case defaults.KeyDown.String(), defaults.KeyVimDown.String():
			if p.cursor < len(privacyOptions)-1 {
				p.cursor++
			}
		case defaults.KeyEnter.String():
			p.confirmed = true
		default:
			if len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= byte('0'+len(privacyOptions)) {
				idx := int(msg.String()[0] - '1')
				if idx >= 0 && idx < len(privacyOptions) {
					p.cursor = idx
					p.confirmed = true
				}
			}
		}
	}
	return p, nil
}

func (p *PrivacyPreferencePage) View(width, height int) string {
	if offeredLevelIsSingular() && len(privacyOptions) == 1 {
		return p.viewSingleOffered()
	}
	return p.viewChooser()
}

// viewSingleOffered STATES the level that will be applied instead of offering it.
//
// With one level offered, a radio glyph, a "(recommended)" tag and an up/down
// hint are affordances asserting alternatives that do not exist. On a privacy
// screen that is the worst available implication: a careful reader looks for
// the alternatives, does not find them, and concludes something was withheld.
// This is the one screen whose job is to say what is withheld from everyone else.
//
// The chooser below is NOT removed. It renders automatically the moment a second
// level is offered, which is the widening the disposition table exists to support
// and which the rest of this work already pays for.
func (p *PrivacyPreferencePage) viewSingleOffered() string {
	option := privacyOptions[0]
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))
	b.WriteString(DescriptionStyle.Render(privacyDisclosure(option.level)))
	b.WriteString(TextBg.Render("\n\n"))
	for _, line := range p.examples {
		b.WriteString(TextBg.Render("  "))
		b.WriteString(HelpBar.Render(line))
		b.WriteString(TextBg.Render("\n"))
	}
	if len(p.examples) > 0 {
		b.WriteString(TextBg.Render("\n"))
	}
	// What happens to the conversation text, and the hedge every other redaction
	// surface prints. This screen was the only one that omitted it.
	b.WriteString(HelpBar.Render(privacyContentSentence()))
	b.WriteString(TextBg.Render("\n\n"))
	b.WriteString(HelpBar.Render("You can change this at any time in\n~/.config/peasant/config.yaml"))
	b.WriteString(TextBg.Render("\n\n"))
	// No navigate hint: there is nowhere to navigate to.
	b.WriteString(HelpBar.Render("enter: continue"))
	return b.String()
}

// viewChooser is the multi-option rendering, unchanged. It is reached when the
// product offers more than one level.
func (p *PrivacyPreferencePage) viewChooser() string {
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))
	b.WriteString(DescriptionStyle.Render("Choose your default redaction level. This controls what is redacted\nfrom your transcripts before they leave your machine."))
	b.WriteString(TextBg.Render("\n\n"))
	for i, opt := range privacyOptions {
		radio := "○"
		if i == p.cursor {
			radio = "●"
		}
		cursor := TextBg.Render("  ")
		if i == p.cursor {
			cursor = OptionCursor.Render("▸ ")
		}
		label := opt.label
		if opt.recommended {
			label += " (recommended)"
		}
		if i == p.cursor {
			b.WriteString(cursor + OptionSelected.Render(radio+" "+label))
		} else {
			b.WriteString(cursor + OptionUnselected.Render(radio+" "+label))
		}
		b.WriteString(TextBg.Render("\n"))
		b.WriteString(TextBg.Render("      "))
		b.WriteString(HelpBar.Render(opt.description))
		b.WriteString(TextBg.Render("\n"))
		for _, line := range p.examplesFor(i) {
			b.WriteString(TextBg.Render("      "))
			b.WriteString(HelpBar.Render(line))
			b.WriteString(TextBg.Render("\n"))
		}
		b.WriteString(TextBg.Render("      "))
		b.WriteString(HelpBar.Render(opt.keeps))
		b.WriteString(TextBg.Render("\n\n"))
	}
	b.WriteString(HelpBar.Render("You can change this at any time in ~/.config/peasant/config.yaml"))
	b.WriteString(TextBg.Render("\n\n"))
	b.WriteString(HelpBar.Render("↑/↓: navigate  enter: confirm"))
	return b.String()
}

// examplesFor renders the before/after lines for one option in the chooser.
//
// Each option redacts at its OWN level, so the examples shown beside a level are
// what that level actually does - which is the whole point of deriving them. A
// level whose examples cannot be produced shows none rather than showing another
// level's.
//
// It recomputes unconditionally. There used to be a branch returning the
// construction-time examples when the index matched the recommended option, and
// it was unobservable: removing it entirely was green, because the two paths
// produce identical output by construction and the branch only ran in a chooser
// the shipped one-level configuration never renders. A cache with no measurable
// effect is a second code path to keep correct for nothing.
func (p *PrivacyPreferencePage) examplesFor(index int) []string {
	lines, err := privacyExamples(privacyOptions[index].level)
	if err != nil {
		return nil
	}
	return lines
}

// licenseOption is one row on the LicensePage.
type licenseOption struct {
	license     schema.License
	label       string
	description string
}

// licenseOptions is the kickstart license menu. Every row is a real license (no
// "none" escape hatch) so a contributor who reaches this page must pick one; the
// default cursor sits on the first/most-permissive entry (CC0). Mirrors
// schema.AllLicenses.
var licenseOptions = []licenseOption{
	{license: schema.LicenseCC0, label: "CC0 1.0", description: "Public-domain dedication — no rights reserved."},
	{license: schema.LicenseCCBY, label: "CC BY 4.0", description: "Reuse allowed with attribution."},
	{license: schema.LicenseCCBYSA, label: "CC BY-SA 4.0", description: "Attribution + share-alike (derivatives keep this license)."},
}

// LicensePage lets the user pick a default content license applied to all pushed
// transcripts. Mirrors PrivacyPreferencePage (single-select radio list).
type LicensePage struct {
	title     string
	cursor    int
	confirmed bool
}

func NewLicensePage(title string) *LicensePage {
	return &LicensePage{title: title} // cursor 0 = CC0 (first option)
}

func (p *LicensePage) Title() string    { return p.title }
func (p *LicensePage) IsComplete() bool { return p.confirmed }
func (p *LicensePage) Init() tea.Cmd    { return nil }
func (p *LicensePage) Reset()           { p.confirmed = false }

// SelectedLicense returns the chosen license (always a real license — the menu
// has no "none" entry).
func (p *LicensePage) SelectedLicense() schema.License {
	return licenseOptions[p.cursor].license
}

func (p *LicensePage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case defaults.KeyUp.String(), defaults.KeyVimUp.String():
			if p.cursor > 0 {
				p.cursor--
			}
		case defaults.KeyDown.String(), defaults.KeyVimDown.String():
			if p.cursor < len(licenseOptions)-1 {
				p.cursor++
			}
		case defaults.KeyEnter.String():
			p.confirmed = true
		default:
			if len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= '9' {
				idx := int(msg.String()[0] - '1')
				if idx < len(licenseOptions) {
					p.cursor = idx
					p.confirmed = true
				}
			}
		}
	}
	return p, nil
}

func (p *LicensePage) View(width, height int) string {
	var b strings.Builder
	b.WriteString(PageTitle.Render(p.title))
	b.WriteString(TextBg.Render("\n\n"))
	b.WriteString(DescriptionStyle.Render("Choose a default license for transcripts you publish to the village.\nIt applies to every push; override a single run with --license."))
	b.WriteString(TextBg.Render("\n\n"))
	for i, opt := range licenseOptions {
		radio := "○"
		if i == p.cursor {
			radio = "●"
		}
		cursor := TextBg.Render("  ")
		if i == p.cursor {
			cursor = OptionCursor.Render("▸ ")
		}
		if i == p.cursor {
			b.WriteString(cursor + OptionSelected.Render(radio+" "+opt.label))
		} else {
			b.WriteString(cursor + OptionUnselected.Render(radio+" "+opt.label))
		}
		b.WriteString(TextBg.Render("\n"))
		b.WriteString(TextBg.Render("      "))
		b.WriteString(HelpBar.Render(opt.description))
		b.WriteString(TextBg.Render("\n\n"))
	}
	b.WriteString(HelpBar.Render("You can change this at any time in ~/.config/peasant/config.yaml"))
	b.WriteString(TextBg.Render("\n\n"))
	b.WriteString(HelpBar.Render("↑/↓: navigate  enter: confirm"))
	return b.String()
}

func (p *IngestPage) renderProgress() string {
	return renderProgressSnapshot(p.progress.Snapshot())
}

func renderProgressSnapshot(snap map[string]StageProgress) string {
	var b strings.Builder
	for _, key := range stageDisplayOrder {
		sp, ok := snap[key]
		label := stageLabel[key]
		if label == "" {
			label = key
		}
		icon := "○"
		if ok && sp.Ended {
			if sp.HasErr {
				icon = "✗"
			} else {
				icon = "✔"
			}
		} else if ok && sp.Done > 0 {
			icon = "●"
		}
		barWidth := 20
		filled := 0
		if ok && sp.Total > 0 {
			filled = sp.Done * barWidth / sp.Total
			if filled > barWidth {
				filled = barWidth
			}
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		counts := ""
		if ok && sp.Total > 0 {
			counts = fmt.Sprintf(" %d/%d", sp.Done, sp.Total)
		}
		line := fmt.Sprintf("  %s %-10s [%s]%s", icon, label, bar, counts)
		b.WriteString(DescriptionStyle.Render(line))
		b.WriteString(TextBg.Render("\n"))
	}
	return b.String()
}

// PrivacyLevels returns the redaction levels the wizard offers, in display order.
// Exported so the command layer can prove the wizard can only produce a level the
// product will actually run.
func PrivacyLevels() []redact.RedactionLevel {
	levels := make([]redact.RedactionLevel, 0, len(privacyOptions))
	for _, option := range privacyOptions {
		levels = append(levels, option.level)
	}
	return levels
}

// PrivacyDescriptions returns the description shown for each offered redaction
// level, in the same order as PrivacyLevels. Exported so the wording rules that
// apply to every redaction claim can be checked against what the wizard actually
// displays, rather than against a copy of it.
func PrivacyDescriptions() []string {
	descriptions := make([]string, 0, len(privacyOptions))
	for _, option := range privacyOptions {
		descriptions = append(descriptions, option.description)
	}
	return descriptions
}
