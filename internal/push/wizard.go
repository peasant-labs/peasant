package push

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// Styles (push wizard specific, mirrors FTUE patterns)
// ---------------------------------------------------------------------------

var wizBaseBg = lipgloss.Color(defaults.ColorBg.String())

var (
	wizTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(wizBaseBg)

	wizText = lipgloss.NewStyle().
		Foreground(lipgloss.Color(defaults.ColorFg.String())).
		Background(wizBaseBg)

	wizDim = lipgloss.NewStyle().
		Foreground(lipgloss.Color(defaults.ColorDimText.String())).
		Background(wizBaseBg).
		Italic(true)

	wizBold = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(defaults.ColorFg.String())).
		Background(wizBaseBg)

	wizCursor = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(wizBaseBg)

	wizBg = lipgloss.NewStyle().Background(wizBaseBg)

	wizBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(defaults.ColorBorder.String())).
			Background(wizBaseBg).
			Padding(1, 2)

	wizGreen = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorPastelMint.String())).
			Background(wizBaseBg)

	wizYellow = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorPastelLemon.String())).
			Background(wizBaseBg)

	wizRed = lipgloss.NewStyle().
		Foreground(lipgloss.Color(defaults.ColorPastelPink.String())).
		Background(wizBaseBg)
)

// ---------------------------------------------------------------------------
// PushWizardSession — session with metadata and action
// ---------------------------------------------------------------------------

// PushWizardSession pairs a push session row with its metadata and action.
type PushWizardSession struct {
	Row    ingest.PushSessionRow
	Meta   *schema.UnifiedMetadata // loaded for redaction status; may be nil
	Action PushAction
	// Locked marks a session that the branch-aware selection withheld due to a
	// multi-project conflict (BranchMatchWithheldConflict). Locked sessions are
	// shown in the wizard flagged ("withheld: branch conflict") but are
	// non-selectable: navigation/select/toggle keys skip them and
	// SelectedSessionIDs never returns them.
	Locked bool
}

// WizardCandidates partitions sessions into the wizard's display order: kept
// (unlocked, branch-selected) first, then withheld (Locked=true, branch
// conflict) so the user sees WHY a session is excluded rather than having it
// silently dropped. A nil matcher keeps all sessions (none withheld), matching
// the "no selection => push everything otherwise eligible" rule.
//
// This is the wizard-side counterpart to the pipeline's ApplySelection: both
// consume the SAME shared matcher, so the wizard's selectable set equals the
// pipeline's kept set by construction.
func WizardCandidates(sessions []ingest.PushSessionRow, sel *ingest.SelectionMatcher) []PushWizardSession {
	kept, withheld := ApplySelection(sessions, sel)
	out := make([]PushWizardSession, 0, len(kept)+len(withheld))
	for _, s := range kept {
		out = append(out, PushWizardSession{Row: s, Action: PushWithRedaction})
	}
	for _, s := range withheld {
		out = append(out, PushWizardSession{Row: s, Action: PushExclude, Locked: true})
	}
	return out
}

// redactionLabel returns a styled label based on RedactionInfo state.
func (s PushWizardSession) redactionLabel() string {
	if s.Meta == nil {
		return wizDim.Render("unknown")
	}
	ri := s.Meta.Redaction
	ch := s.Meta.ContentHash
	switch {
	case ri.IsCurrent(ch):
		return wizGreen.Render("current")
	case ri.IsStale(ch):
		return wizYellow.Render("stale")
	case ri.IsRaw():
		return wizRed.Render("raw")
	default:
		return wizDim.Render("unknown")
	}
}

// redactionStatus returns an unstyled status string for redaction.
func (s PushWizardSession) redactionStatus() string {
	if s.Meta == nil {
		return "unknown"
	}
	ri := s.Meta.Redaction
	ch := s.Meta.ContentHash
	switch {
	case ri.IsCurrent(ch):
		return "current"
	case ri.IsStale(ch):
		return "stale"
	case ri.IsRaw():
		return "raw"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// PushWizardModel — 4-page push wizard
// ---------------------------------------------------------------------------

// wizardPage identifies each page of the push wizard.
type wizardPage int

const (
	pageInitialConfirm wizardPage = iota
	pageSessionReview
	pageRedactionPreview
	pageFinalConfirm
	pageCount
)

// PushWizardModel is the top-level BubbleTea model for the push wizard.
type PushWizardModel struct {
	page         wizardPage
	sessions     []PushWizardSession
	cursor       int
	offset       int // scroll offset for session review
	vpOffset     int // scroll offset for redaction preview
	width        int
	height       int
	quitting     bool
	confirmed    bool
	confirmSel   int                     // 0=Yes, 1=No for option select pages
	redactResult *RedactionPreviewResult // populated before redaction preview page
}

// RedactionPreviewResult holds the result of running a dry-run redaction for preview.
type RedactionPreviewResult struct {
	Entries []RedactionPreviewEntry
}

// RedactionPreviewEntry shows what was redacted in a single session.
type RedactionPreviewEntry struct {
	SessionID    string
	Status       string // "current", "stale", "raw"
	SampleBefore string // first ~200 chars of transcript before redaction
	SampleAfter  string // first ~200 chars of transcript after redaction
	Changed      bool   // whether redaction changed anything
}

// NewPushWizard creates a push wizard model with the given sessions.
// All sessions default to PushWithRedaction.
func NewPushWizard(sessions []PushWizardSession) PushWizardModel {
	return PushWizardModel{
		page:     pageInitialConfirm,
		sessions: sessions,
	}
}

// Confirmed returns true if the user confirmed the push.
func (m PushWizardModel) Confirmed() bool { return m.confirmed }

// Quitting returns true if the user cancelled.
func (m PushWizardModel) Quitting() bool { return m.quitting }

// SelectedSessionIDs returns the session IDs that the user approved for push.
// Locked (withheld branch-conflict) sessions are never returned.
func (m PushWizardModel) SelectedSessionIDs() []string {
	var ids []string
	for _, s := range m.sessions {
		if s.Locked {
			continue
		}
		if s.Action == PushWithRedaction {
			ids = append(ids, s.Row.SessionID)
		}
	}
	return ids
}

func (m PushWizardModel) Init() tea.Cmd { return nil }

func (m PushWizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case defaults.KeyInterrupt.String():
			m.quitting = true
			return m, tea.Quit
		}

		switch m.page {
		case pageInitialConfirm:
			return m.updateInitialConfirm(msg)
		case pageSessionReview:
			return m.updateSessionReview(msg)
		case pageRedactionPreview:
			return m.updateRedactionPreview(msg)
		case pageFinalConfirm:
			return m.updateFinalConfirm(msg)
		}
	}
	return m, nil
}

func (m PushWizardModel) View() string {
	if m.quitting {
		return wizDim.Render("Push cancelled.\n")
	}

	var content string
	switch m.page {
	case pageInitialConfirm:
		content = m.viewInitialConfirm()
	case pageSessionReview:
		content = m.viewSessionReview()
	case pageRedactionPreview:
		content = m.viewRedactionPreview()
	case pageFinalConfirm:
		content = m.viewFinalConfirm()
	}

	if m.width > 0 {
		return wizBorder.Width(m.width - 4).Render(content)
	}
	return wizBorder.Render(content)
}

// ---------------------------------------------------------------------------
// Page 1: Initial Confirm
// ---------------------------------------------------------------------------

func (m PushWizardModel) updateInitialConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case defaults.KeyUp.String(), defaults.KeyVimUp.String():
		if m.confirmSel > 0 {
			m.confirmSel--
		}
	case defaults.KeyDown.String(), defaults.KeyVimDown.String():
		if m.confirmSel < 1 {
			m.confirmSel++
		}
	case defaults.KeySpace.String(), defaults.KeyEnter.String():
		if m.confirmSel == 0 { // Yes
			m.page = pageSessionReview
			m.cursor = 0
			m.offset = 0
		} else { // No
			m.quitting = true
			return m, tea.Quit
		}
	case defaults.KeyEscape.String(), defaults.KeyInterrupt.String():
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m PushWizardModel) viewInitialConfirm() string {
	var b strings.Builder
	b.WriteString(wizTitle.Render("Push to Village"))
	b.WriteString(wizBg.Render("\n\n"))
	b.WriteString(wizText.Render(fmt.Sprintf(
		"You have %d session(s) ready to push.", len(m.sessions))))
	b.WriteString(wizBg.Render("\n\n"))
	b.WriteString(wizBold.Render("Are you sure you want to push your data to the village?"))
	b.WriteString(wizBg.Render("\n\n"))

	options := []string{"Yes, continue", "No, cancel"}
	for i, opt := range options {
		if i == m.confirmSel {
			b.WriteString(wizCursor.Render("▸ "))
			b.WriteString(wizBold.Render(opt))
		} else {
			b.WriteString(wizBg.Render("  "))
			b.WriteString(wizText.Render(opt))
		}
		b.WriteString(wizBg.Render("\n"))
	}
	b.WriteString(wizBg.Render("\n"))
	b.WriteString(wizDim.Render("↑/↓: select  enter: confirm  esc: cancel"))
	return b.String()
}

// ---------------------------------------------------------------------------
// Page 2: Session Review
// ---------------------------------------------------------------------------

// viewportHeight returns the number of visible session rows.
func (m PushWizardModel) viewportHeight() int {
	// Reserve space for title, column header, help bar, and border chrome.
	h := m.height - 12
	if h < 5 {
		h = 5
	}
	if h > len(m.sessions) {
		h = len(m.sessions)
	}
	return h
}

func (m PushWizardModel) updateSessionReview(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	vh := m.viewportHeight()
	switch msg.String() {
	case defaults.KeyUp.String(), defaults.KeyVimUp.String():
		if m.cursor > 0 {
			m.cursor--
			if m.cursor < m.offset {
				m.offset = m.cursor
			}
		}
	case defaults.KeyDown.String(), defaults.KeyVimDown.String():
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
			if m.cursor >= m.offset+vh {
				m.offset = m.cursor - vh + 1
			}
		}
	case defaults.KeySpace.String(): // cycle action (Locked sessions are non-selectable)
		if !m.sessions[m.cursor].Locked {
			m.sessions[m.cursor].Action = (m.sessions[m.cursor].Action + 1) % 2
		}
	case defaults.KeyQuit.String(): // set to exclude
		if !m.sessions[m.cursor].Locked {
			m.sessions[m.cursor].Action = PushExclude
		}
	case defaults.KeyPushApprove.String(): // set to push
		if !m.sessions[m.cursor].Locked {
			m.sessions[m.cursor].Action = PushWithRedaction
		}
	case defaults.KeyAnnotate.String(): // approve all (a) — skips Locked
		for i := range m.sessions {
			if !m.sessions[i].Locked {
				m.sessions[i].Action = PushWithRedaction
			}
		}
	case defaults.KeyExclude.String(): // exclude all (x) — skips Locked
		for i := range m.sessions {
			if !m.sessions[i].Locked {
				m.sessions[i].Action = PushExclude
			}
		}
	case defaults.KeyEnter.String():
		m.page = pageRedactionPreview
		m.vpOffset = 0
	case defaults.KeyEscape.String():
		m.page = pageInitialConfirm
	}
	return m, nil
}

func (m PushWizardModel) viewSessionReview() string {
	var b strings.Builder
	b.WriteString(wizTitle.Render("Review Sessions"))
	b.WriteString(wizBg.Render("\n\n"))

	// Column header
	header := fmt.Sprintf("  %-12s %-10s %-14s %-12s %-10s %s",
		"Session ID", "Provider", "Project", "Date", "Redaction", "Action")
	b.WriteString(wizDim.Render(header))
	b.WriteString(wizBg.Render("\n"))

	vh := m.viewportHeight()
	end := m.offset + vh
	if end > len(m.sessions) {
		end = len(m.sessions)
	}

	if m.offset > 0 {
		b.WriteString(wizDim.Render(fmt.Sprintf("  ... %d more above", m.offset)))
		b.WriteString(wizBg.Render("\n"))
	}

	for i := m.offset; i < end; i++ {
		s := m.sessions[i]
		cursor := wizBg.Render("  ")
		if i == m.cursor {
			cursor = wizCursor.Render("▸ ")
		}

		sid := s.Row.SessionID
		if len(sid) > 10 {
			sid = sid[:10] + ".."
		}

		provider := s.Row.ModelHarness
		if len(provider) > 8 {
			provider = provider[:8] + ".."
		}

		project := s.Row.ProjectName
		if project == "" {
			project = "(unknown)"
		}
		if len(project) > 12 {
			project = project[:12] + ".."
		}

		date := time.UnixMilli(s.Row.StartMs).Format("Jan 02 15:04")

		redaction := s.redactionLabel()

		var actionLabel string
		switch {
		case s.Locked:
			actionLabel = wizYellow.Render("withheld: branch conflict")
		case s.Action == PushWithRedaction:
			actionLabel = wizGreen.Render("push")
		case s.Action == PushExclude:
			actionLabel = wizRed.Render("exclude")
		}

		// The redaction label is styled (variable width from ANSI), so we pad differently.
		// We format everything as simple columns; the styled redaction field will be appended.
		row := fmt.Sprintf("%-12s %-10s %-14s %-12s ", sid, provider, project, date)

		if i == m.cursor {
			b.WriteString(cursor + wizBold.Render(row) + redaction + wizBg.Render("  ") + actionLabel)
		} else {
			b.WriteString(cursor + wizText.Render(row) + redaction + wizBg.Render("  ") + actionLabel)
		}
		b.WriteString(wizBg.Render("\n"))
	}

	if end < len(m.sessions) {
		b.WriteString(wizDim.Render(fmt.Sprintf("  ... %d more below", len(m.sessions)-end)))
		b.WriteString(wizBg.Render("\n"))
	}

	b.WriteString(wizBg.Render("\n"))
	b.WriteString(wizDim.Render("↑/↓: navigate  space: toggle  a: select all  x: clear all  enter: confirm  esc: back"))

	return b.String()
}

// ---------------------------------------------------------------------------
// Page 3: Redaction Preview
// ---------------------------------------------------------------------------

// wrapToContentWidth wraps prose to the width the wizard's border leaves for
// content, so the scroll viewport's line count matches what is drawn.
//
// The fallback applies before the terminal size is known, which is the state the
// first frame renders in.
func wrapToContentWidth(text string, width int) string {
	const fallback = 76
	content := width - 8
	if width <= 0 || content < 20 {
		content = fallback
	}
	return lipgloss.NewStyle().Width(content).Render(text)
}

func (m PushWizardModel) redactionPreviewContent() string {
	var b strings.Builder

	var pushSessions []PushWizardSession
	for _, s := range m.sessions {
		if s.Action == PushWithRedaction {
			pushSessions = append(pushSessions, s)
		}
	}

	if len(pushSessions) == 0 {
		b.WriteString(wizText.Render("No sessions selected for push."))
		b.WriteString(wizBg.Render("\n"))
		b.WriteString(wizDim.Render("Press esc to go back and select sessions."))
		return b.String()
	}

	b.WriteString(wizText.Render(fmt.Sprintf("%d session(s) selected for push:\n", len(pushSessions))))

	var stale, raw, current int
	for _, s := range pushSessions {
		switch s.redactionStatus() {
		case "stale":
			stale++
		case "raw":
			raw++
		case "current":
			current++
		}
	}

	// These counts describe the STORED copy on this machine. They are reported
	// because they are true and a user may want to know, but they are deliberately
	// not phrased as a promise about the upload: what is published comes from the
	// indexed entries rather than from the transcript file these counts describe,
	// and it is redacted on its own way out - metadata AND content - independently
	// of whatever the stored copy happens to be.
	//
	// This sentence used to end "nothing here is re-redacted on the way out except
	// metadata". That was true when written and this work falsified it, thirty
	// lines above the consent copy that now tells the user content is redacted
	// too: the paragraph a maintainer reads immediately before editing that copy
	// argued for the state the copy no longer describes.
	if current > 0 {
		b.WriteString(wizBg.Render("\n"))
		b.WriteString(wizGreen.Render(fmt.Sprintf("  %d session(s) whose stored copy was redacted at the current rule set.", current)))
	}
	if stale > 0 {
		b.WriteString(wizBg.Render("\n"))
		b.WriteString(wizYellow.Render(fmt.Sprintf("  %d session(s) whose stored copy was redacted by an older rule set.", stale)))
		b.WriteString(wizBg.Render("\n"))
		b.WriteString(wizDim.Render("    Content has changed since redaction was last applied."))
	}
	if raw > 0 {
		b.WriteString(wizBg.Render("\n"))
		b.WriteString(wizRed.Render(fmt.Sprintf("  %d session(s) whose stored copy has never been redacted.", raw)))
	}

	// What actually happens on upload.
	//
	// This block has now been wrong in BOTH directions, which is why it is worth
	// the care. It first promised that "no raw data will leave your machine" - an
	// absolute guarantee, printed at the moment of consent, that the push path
	// never delivered. It was then corrected to say only metadata is redacted and
	// content is published as recorded, which was true until push began applying
	// the same content redaction `peasant redact` applies. Understating protection
	// is the safer error and is still an error: this is the screen a user reads
	// while deciding what to share, and a person who believes their content leaves
	// untouched will withhold sessions they could safely publish - or, worse,
	// conclude the control does nothing and stop reading it.
	//
	// What it must not become again is an absolute. Matching finds KNOWN patterns.
	b.WriteString(wizBg.Render("\n\n"))
	// WHAT REDACTION COVERS, DERIVED - not written here.
	//
	// This block hand-listed "file paths, the host slug, the project name, the git
	// remote". Measured at standard, the only level a user can select: the project
	// name, the host slug and the git remote are NOT rewritten. The git rules are
	// gated to maximum and maximum is refused, so they cannot fire on any run a
	// user can perform - and the screen promised them at the moment of consent.
	// The list survived a sweep that corrected the sentence about CONTENT beside
	// it and inherited the fields unchecked.
	//
	// The fields now come from running the real redactor, so a claim that stops
	// being true disappears instead of being carried forward.
	covered, coverErr := redactedMetadataFields(config.RecommendedRedactionLevel)
	b.WriteString(wizBg.Render("\n\n"))
	switch {
	case coverErr != nil || len(covered) == 0:
		// Say nothing rather than guess. A screen that cannot measure what it
		// covers has no business naming fields.
		b.WriteString(wizText.Render("Before upload, Peasant redacts at your configured level, never below standard."))
	default:
		b.WriteString(wizText.Render("Before upload, Peasant redacts at your configured level, never below standard:"))
		b.WriteString(wizBg.Render("\n"))
		b.WriteString(wizText.Render("METADATA — " + joinFieldLabels(covered) + " — and CONVERSATION CONTENT,"))
		b.WriteString(wizBg.Render("\n"))
		b.WriteString(wizText.Render("including tool arguments and tool results."))
	}
	b.WriteString(wizBg.Render("\n\n"))
	// THE HEDGE IS THE SHARED CONSTANT, CONSUMED VERBATIM.
	//
	// This screen hand-wrote its own wording of a sentence that exists once, in
	// config.RedactionScopeSentence, which every other surface consumes - the
	// onboarding screen, every push record, both sync refusals and the generated
	// web policy. Rewording it here only ever produced a third phrasing: the last
	// edit claimed to match the canonical one and did not, and dropped its scope
	// clause ("in both metadata and transcript content") while doing so.
	//
	// Consuming the constant is what ends that. It also removes two statements the
	// hand-written version had just acquired: that what leaves is "what the
	// patterns above matched" (the opposite of the line above it, which says
	// matched tokens are REPLACED), and that "nothing else scans this on your
	// behalf" (false for the transcript part, which the village does scan).
	// Wrapped to the content width HERE rather than left to the outer border.
	//
	// The border does wrap it - measured, no rendered line exceeds the terminal -
	// but this page slices its content into lines for the scroll viewport BEFORE
	// the border sees it, so a 180-character sentence counts as one line for the
	// height accounting and three on screen. Wrapping it now keeps the two in
	// agreement.
	b.WriteString(wizYellow.Render(wrapToContentWidth(config.RedactionScopeSentence(), m.width)))
	b.WriteString(wizBg.Render("\n\n"))
	b.WriteString(wizYellow.Render("Source code is published with matched tokens replaced, so a published"))
	b.WriteString(wizBg.Render("\n"))
	b.WriteString(wizYellow.Render("transcript can differ from what you see locally. Read what you share."))
	b.WriteString(wizBg.Render("\n\n"))
	b.WriteString(wizText.Render("If a session holds something you do not want published, deselect it:"))
	b.WriteString(wizBg.Render("\n"))
	b.WriteString(wizText.Render("press esc to go back, then space to toggle it off."))

	return b.String()
}

func (m PushWizardModel) updateRedactionPreview(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case defaults.KeyEnter.String():
		// Only advance if there are sessions to push.
		hasSelected := false
		for _, s := range m.sessions {
			if s.Action == PushWithRedaction {
				hasSelected = true
				break
			}
		}
		if hasSelected {
			m.page = pageFinalConfirm
			m.confirmSel = 0
		}
	case defaults.KeyEscape.String():
		m.page = pageSessionReview
	case defaults.KeyUp.String(), defaults.KeyVimUp.String():
		if m.vpOffset > 0 {
			m.vpOffset--
		}
	case defaults.KeyDown.String(), defaults.KeyVimDown.String():
		m.vpOffset++
	}
	return m, nil
}

func (m PushWizardModel) viewRedactionPreview() string {
	var b strings.Builder
	b.WriteString(wizTitle.Render("Redaction Preview"))
	b.WriteString(wizBg.Render("\n\n"))

	content := m.redactionPreviewContent()

	// Simple scrollable viewport using line slicing.
	lines := strings.Split(content, "\n")
	vpHeight := m.height - 10
	if vpHeight < 5 {
		vpHeight = 5
	}
	maxOffset := len(lines) - vpHeight
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.vpOffset > maxOffset {
		m.vpOffset = maxOffset
	}

	end := m.vpOffset + vpHeight
	if end > len(lines) {
		end = len(lines)
	}
	for i := m.vpOffset; i < end; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
	}

	b.WriteString(wizBg.Render("\n"))
	b.WriteString(wizDim.Render("↑/↓: scroll  enter: continue  esc: back"))

	return b.String()
}

// ---------------------------------------------------------------------------
// Page 4: Final Confirm
// ---------------------------------------------------------------------------

func (m PushWizardModel) updateFinalConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case defaults.KeyUp.String(), defaults.KeyVimUp.String():
		if m.confirmSel > 0 {
			m.confirmSel--
		}
	case defaults.KeyDown.String(), defaults.KeyVimDown.String():
		if m.confirmSel < 1 {
			m.confirmSel++
		}
	case defaults.KeySpace.String(), defaults.KeyEnter.String():
		if m.confirmSel == 0 { // Yes, push
			m.confirmed = true
			return m, tea.Quit
		}
		// No, go back
		m.page = pageSessionReview
	case defaults.KeyEscape.String(), defaults.KeyInterrupt.String():
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m PushWizardModel) viewFinalConfirm() string {
	var b strings.Builder
	b.WriteString(wizTitle.Render("Confirm Push"))
	b.WriteString(wizBg.Render("\n\n"))

	var selected []PushWizardSession
	for _, s := range m.sessions {
		if s.Action == PushWithRedaction {
			selected = append(selected, s)
		}
	}

	b.WriteString(wizBold.Render(fmt.Sprintf(
		"You are about to push %d session(s) to the village.", len(selected))))
	b.WriteString(wizBg.Render("\n\n"))

	// Show selected sessions summary.
	for _, s := range selected {
		sid := s.Row.SessionID
		if len(sid) > 10 {
			sid = sid[:10] + ".."
		}
		date := time.UnixMilli(s.Row.StartMs).Format("Jan 02 15:04")
		b.WriteString(wizText.Render(fmt.Sprintf("  %s  %s  %s  %s",
			sid, s.Row.ModelHarness, s.Row.ProjectName, date)))
		b.WriteString(wizBg.Render("\n"))
	}

	b.WriteString(wizBg.Render("\n"))
	b.WriteString(wizBold.Render("Proceed?"))
	b.WriteString(wizBg.Render("\n\n"))

	options := []string{"Yes, push now", "No, go back"}
	for i, opt := range options {
		if i == m.confirmSel {
			b.WriteString(wizCursor.Render("▸ "))
			b.WriteString(wizBold.Render(opt))
		} else {
			b.WriteString(wizBg.Render("  "))
			b.WriteString(wizText.Render(opt))
		}
		b.WriteString(wizBg.Render("\n"))
	}
	b.WriteString(wizBg.Render("\n"))
	b.WriteString(wizDim.Render("↑/↓: select  enter: confirm  esc: cancel"))

	return b.String()
}
