package ftue

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
)

// SessionListing is a simplified session entry for display in the FTUE wizard.
// It keeps discovery details independent from ingest's full session type.
type SessionListing struct {
	Harness     string    `yaml:"harness"`
	ProjectName string    `yaml:"projectName"` // human-readable project name (empty if unknown)
	GitRemote   string    `yaml:"gitRemote"`   // git remote URL for project grouping (empty if unknown)
	Branch      string    `yaml:"branch"`      // git branch or worktree name (empty if unknown/non-git)
	Title       string    `yaml:"title"`       // session title (empty if unknown)
	Date        time.Time `yaml:"date"`        // creation time when known, otherwise file mod time
	TurnCount   int       `yaml:"turnCount"`   // may be 0 if unknown from discovery
	SessionID   string    `yaml:"sessionId"`   // raw session ID string supplied by discovery
	SubagentIDs []string  `yaml:"subagentIds"` // child subagent session IDs (populated from discovery)
	WorkingDir  string    `yaml:"workingDir"`  // session working directory, used only to focus its containing project
	// Origin is who drove the session, as discovery's classifier decided it
	// (internal/sessionorigin). The zero value is the empty string, which is
	// NOT a menu value: it marks a listing an adapter mined no evidence for,
	// and callers must resolve it to sessionorigin.Unknown (the visible
	// fail-safe) before relying on it — never treat empty as User or Agent
	// directly. Kickstart is the sole consumer that reads this field to hide
	// agent-driven rows from the picker; it is DISCOVERY scope only and never
	// gates opening an already-stored session by link.
	Origin sessionorigin.Origin `yaml:"origin"`
	// Source locates the transcript the harness wrote. The selection step reads
	// it to preview a session that Peasant has not imported yet.
	Source SessionSource `yaml:"source"`
}

// StageProgress reports progress for a single pipeline stage.
type StageProgress struct {
	Done   int
	Total  int
	Ended  bool
	HasErr bool
}

// ProgressSnapshot provides a point-in-time view of pipeline progress.
// Defined here (not in ingest) to avoid an import cycle.
type ProgressSnapshot interface {
	Snapshot() map[string]StageProgress
}

// ProviderIngestCount holds per-provider ingest counts for display.
type ProviderIngestCount struct {
	Harness string
	New     int
	Updated int
}

// ProviderDiscovery carries the inventory and configured state for one harness.
type ProviderDiscovery struct {
	SessionCount int
	Enabled      bool
	State        DiscoveryState
	Detail       string
}

// DiscoveryState distinguishes an honest empty scan from a harness that could
// not be inspected. The zero value is kept as ready for older callers that
// construct inventory entries directly.
type DiscoveryState string

const (
	DiscoveryReady       DiscoveryState = ""
	DiscoveryUnavailable DiscoveryState = "unavailable"
	DiscoveryFailed      DiscoveryState = "failed"
)

func (s DiscoveryState) IsOperational() bool { return s == DiscoveryReady }

// ProviderInventory is the typed discovery result consumed by the setup wizard.
type ProviderInventory map[defaults.Harness]ProviderDiscovery

// IngestResult holds the outcome of a pipeline run for display in the wizard.
type IngestResult struct {
	New       int
	Updated   int
	Unchanged int
	Errors    int
	Duration  time.Duration
	// ProviderCounts holds per-provider breakdown of new + updated counts.
	ProviderCounts []ProviderIngestCount
}

// IngestRunnerFunc runs the ingestion pipeline given the wizard's collected answers.
// Injected from cmd_kickstart to avoid an import cycle between ftue and ingest.
type IngestRunnerFunc func(ctx context.Context, answers WizardAnswers) (*IngestResult, error)

// ProviderSelection holds the per-provider import configuration from the kickstart wizard.
type ProviderSelection struct {
	Harness   string // provider name string (e.g., "claude")
	ImportAll bool   // true = import all, false = select individual sessions
}

// Wizard page indices mirror the stable PageID identities while retaining the
// integer indexing required by the Bubble Tea page slice.
const (
	pageVillage = iota
	pageWantImport
	pageProjectSelect
	pageSessionSelect
	pageAutoIngest // "Auto-ingest new branches?" — shown after session selection
	pagePrivacy
	pageLicense   // default content license applied to village pushes
	pageRetention // Claude Code transcript retention (cleanupPeriodDays)
	pageDestination
	pageSummary
	pageIngestion
)

// WizardAnswers accumulates user choices across pages.
type WizardAnswers struct {
	VillageConnected bool
	// SelectionMode preserves the committed discovery policy while guided
	// kickstart crosses the legacy wizard adapter. An empty value identifies
	// legacy wizard callers whose provider/session choices retain their existing
	// interpretation.
	SelectionMode config.SelectionMode
	// DaemonMode is reserved for the future daemon feature ("opt-in" or "opt-out").
	// The wizard page is not yet shown; defaults to "opt-in".
	DaemonMode            string
	WantImport            bool
	ProviderSelections    []ProviderSelection // per-provider import config (replaces ImportMethod + SelectedSources)
	SelectedSessions      []SessionListing    // individual session selections from tree
	SelectedProjects      []ProjectCatalogEntry
	ScopeSelections       []ProjectScopeSelection
	AutoIngestNewBranches bool           // auto-ingest new branches in fully-selected projects
	RedactionLevel        string         // "minimal", "standard", "maximum"
	License               schema.License // default push license ("" = none); see schema.AllLicenses
	ClaudeRetentionDays   int            // cleanupPeriodDays for ~/.claude/settings.json
	Destination           Destination
	Authentication        AuthenticationChoice
	RequestedVisibility   schema.Visibility
	EffectiveVisibility   schema.Visibility
	HookConsents          []HookConsent
	FinalConsent          bool
	ConfigPath            string
}

// EffectiveSelectedSessions intersects project/branch/session choices with the
// later global harness filter without mutating either set of choices.
func (a WizardAnswers) EffectiveSelectedSessions() []SessionListing {
	selectedHarnesses := make(map[string]bool, len(a.ProviderSelections))
	for _, provider := range a.ProviderSelections {
		selectedHarnesses[provider.Harness] = true
	}
	result := make([]SessionListing, 0, len(a.SelectedSessions))
	for _, session := range a.SelectedSessions {
		if selectedHarnesses[session.Harness] {
			result = append(result, session)
		}
	}
	return result
}

// WizardOption configures the wizard.
type WizardOption func(*WizardModel)

// WithProviderInventory injects discovered counts and configured provider state.
func WithProviderInventory(inventory ProviderInventory) WizardOption {
	return func(m *WizardModel) {
		m.providerInventory = inventory
	}
}

// WithSessions injects real discovered sessions for individual-selection mode.
func WithSessions(sessions []SessionListing) WizardOption {
	return func(m *WizardModel) {
		m.discoveredSessions = sessions
	}
}

// WithExistingUser injects the username of an already-authenticated user.
// When set, the village page offers "Continue as X" instead of a fresh login.
func WithExistingUser(username string) WizardOption {
	return func(m *WizardModel) {
		m.existingUser = username
	}
}

// WithIngestRunner injects the function that runs the real ingestion pipeline.
func WithIngestRunner(fn IngestRunnerFunc) WizardOption {
	return func(m *WizardModel) {
		m.ingestRunner = fn
	}
}

// WithProgress injects a ProgressSnapshot for the IngestPage to poll.
func WithProgress(p ProgressSnapshot) WizardOption {
	return func(m *WizardModel) {
		m.progress = p
	}
}

// WithExistingSelection injects the selection index from a previously saved config.
// When set, the tree page pre-populates with prior selections on re-run.
func WithExistingSelection(sel *config.SelectionConfig) WizardOption {
	return func(m *WizardModel) {
		m.existingSelection = sel
	}
}

// WithConfigPersistence mounts the exact CLI-selected path and loaded config
// that the wizard must mutate without dropping unrelated settings.
func WithConfigPersistence(path string, loaded *config.Config) WizardOption {
	return func(m *WizardModel) {
		m.configPath = path
		m.loadedConfig = loaded
		m.answers.ConfigPath = path
	}
}

// WithConfigSnapshot supplies the exact bytes reviewed before final consent.
func WithConfigSnapshot(snapshot []byte, existed bool) WizardOption {
	return func(m *WizardModel) {
		m.configSnapshot = append([]byte(nil), snapshot...)
		m.configSnapshotExisted = existed
	}
}

// WithInvocationPWD supplies the directory from which kickstart was invoked.
func WithInvocationPWD(path string) WizardOption {
	return func(m *WizardModel) { m.invocationPWD = path }
}

// WizardModel is the top-level Bubble Tea model for the FTUE wizard.
type WizardModel struct {
	pages                 []Page
	current               int
	answers               *WizardAnswers
	width                 int
	height                int
	quitting              bool
	completed             bool
	providerInventory     ProviderInventory
	discoveredSessions    []SessionListing
	ingestRunner          IngestRunnerFunc
	existingUser          string // non-empty when credentials are already on disk
	progress              ProgressSnapshot
	existingSelection     *config.SelectionConfig // from prior config, used to pre-populate tree
	configPath            string
	loadedConfig          *config.Config
	configSnapshot        []byte
	configSnapshotExisted bool
	invocationPWD         string
	lastTreeSelections    []ProviderSelection // snapshot of provider selections when tree was last built
	projectScopeKeys      []string
	saveErr               error
	journeyRunner         JourneyRunner
	journeyContext        context.Context
	journeyResult         *JourneyResult
	journeyErr            error
	executing             bool
	journeyCancel         context.CancelFunc
	cancellationRequested bool
	journeyProgressToken  uint64
}

// WithJourneyRunner injects the sole side-effect orchestrator used after final
// consent. The runner owns no policy; it composes production authorities.
func WithJourneyRunner(runner JourneyRunner) WizardOption {
	return func(m *WizardModel) { m.journeyRunner = runner }
}

func WithJourneyContext(ctx context.Context) WizardOption {
	return func(m *WizardModel) { m.journeyContext = ctx }
}

type journeyFinishedMsg struct {
	result JourneyResult
	err    error
}

type journeyProgressTickMsg struct{ token uint64 }

func journeyProgressTickCmd(token uint64) tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return journeyProgressTickMsg{token: token} })
}

// NewWizard creates a new FTUE wizard model.
func NewWizard(opts ...WizardOption) WizardModel {
	m := WizardModel{
		answers:           &WizardAnswers{DaemonMode: "opt-in", Destination: DestinationLocal, Authentication: AuthenticationSkipped, RequestedVisibility: schema.VisibilityPrivate, EffectiveVisibility: schema.VisibilityPrivate},
		providerInventory: ProviderInventory{},
		journeyContext:    context.Background(),
	}
	for _, opt := range opts {
		opt(&m)
	}
	m.pages = BuildPages(m.providerInventory, m.ingestRunner, m.existingUser)
	m.pages[pageProjectSelect] = NewProjectSelectPage(m.discoveredSessions, m.existingSelection, m.invocationPWD)
	m.pages[pageDestination] = NewDestinationPage(m.existingUser != "", m.loadedConfig)
	return m
}

func (m WizardModel) Init() tea.Cmd {
	return nil
}

func (m WizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case journeyFinishedMsg:
		if m.journeyCancel != nil {
			m.journeyCancel()
			m.journeyCancel = nil
		}
		m.executing = false
		m.journeyProgressToken++
		m.journeyResult = &msg.result
		m.journeyErr = msg.err
		m.cancellationRequested = false
		return m, nil
	case journeyProgressTickMsg:
		if m.executing && msg.token == m.journeyProgressToken {
			return m, journeyProgressTickCmd(msg.token)
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Forward to the current page so scroll-aware pages can update their viewport.
		if m.current < len(m.pages) {
			updated, _ := m.pages[m.current].Update(msg)
			m.pages[m.current] = updated
		}
		return m, nil

	case tea.KeyPressMsg:
		if m.executing && (msg.String() == defaults.KeyInterrupt.String() || msg.String() == defaults.KeyQuit.String() || msg.String() == defaults.KeyBack.String() || msg.String() == defaults.KeyRestart.String()) {
			if m.journeyCancel != nil {
				m.journeyCancel()
			}
			if !m.cancellationRequested {
				m.journeyProgressToken++
			}
			m.cancellationRequested = true
			return m, nil
		}
		// Outside execution, ctrl+c exits immediately.
		if msg.String() == defaults.KeyInterrupt.String() {
			m.quitting = true
			return m, tea.Quit
		}
		if m.journeyResult != nil && msg.String() == defaults.KeyEnter.String() {
			if len(m.journeyResult.Retry) == 0 || m.journeyRunner == nil {
				m.completed = true
				return m, tea.Quit
			}
			m.journeyErr = nil
			return m, m.startJourneyExecution(m.journeyResult.Retry, m.journeyResult.Effects)
		}

		// Skip other global shortcuts when the current page is capturing text input
		// or showing a modal overlay (confirm summary, help).
		inputMode := false
		if m.current < len(m.pages) {
			switch p := m.pages[m.current].(type) {
			case *TreeSelectPage:
				inputMode = p.IsSearching() || p.IsConfirming() || p.IsShowingHelp()
			case *ProjectSelectPage:
				inputMode = p.IsSearching()
			case *ProjectScopePage:
				inputMode = p.IsShowingHelp()
			}
		}
		if !inputMode {
			switch msg.String() {
			case defaults.KeyQuit.String():
				m.quitting = true
				return m, tea.Quit
			case defaults.KeyBack.String():
				m.prevPage()
				return m, nil
			case defaults.KeyRestart.String():
				return m.restart(), nil
			}
		}
	}

	// Delegate to current page
	if m.current < len(m.pages) {
		page := m.pages[m.current]
		wasComplete := page.IsComplete()
		updated, pageCmd := page.Update(msg)
		m.pages[m.current] = updated

		// If page just became complete, store answer and advance
		if !wasComplete && updated.IsComplete() {
			m.storeAnswer(m.current)
			if m.current == pageSummary && m.journeyRunner != nil {
				return m, tea.Batch(pageCmd, m.startJourneyExecution(nil, nil))
			}
			if m.saveErr != nil {
				m.resetPage(m.current)
				return m, pageCmd
			}
			advanced, startCmd := m.nextPage()
			if !advanced {
				m.completed = true
				return m, tea.Quit
			}
			if startCmd != nil {
				return m, tea.Batch(pageCmd, startCmd)
			}
			return m, pageCmd
		}

		return m, pageCmd
	}

	return m, nil
}

func (m WizardModel) View() tea.View {
	v := tea.NewView(m.viewString())
	v.AltScreen = true
	return v
}

func (m WizardModel) viewString() string {
	if m.quitting {
		return HelpBar.Render("Setup cancelled.\n")
	}
	if m.completed {
		return OptionSelected.Render("Setup complete. Run 'peasant tui' to get started.\n")
	}
	if m.executing {
		if m.cancellationRequested {
			return WizardBorder.Render(DescriptionStyle.Render("Cancelling consented setup...\n\nWaiting for the active operation to stop so completed and partial durable effects can be shown safely."))
		}
		var b strings.Builder
		b.WriteString(DescriptionStyle.Render("Applying your consented setup...\n\nCtrl+C cancels work that has not started. Completed stages remain persisted."))
		if m.progress != nil {
			b.WriteString(TextBg.Render("\n\n"))
			b.WriteString(renderProgressSnapshot(m.progress.Snapshot()))
		}
		return WizardBorder.Render(b.String())
	}
	if m.journeyResult != nil {
		return m.renderJourneyResult()
	}
	if m.saveErr != nil {
		return WizardBorder.Render(DescriptionStyle.Render(fmt.Sprintf(
			"Configuration was not saved.\n\nWhat went wrong: %v\nWhat this means: import did not start and your previous configuration remains in effect.\nHow to fix: correct the filesystem permission or path problem, then press Enter to retry.",
			m.saveErr,
		)))
	}

	var b strings.Builder
	pageWidth := m.width
	if m.width > 0 {
		pageWidth = m.width - WizardBorder.GetHorizontalFrameSize()
		if pageWidth < 1 {
			pageWidth = 1
		}
	}

	// Progress indicator counting only non-skipped pages
	visibleIdx, visibleTotal := m.visibleProgress()
	progress := fmt.Sprintf("Step %d of %d", visibleIdx, visibleTotal)
	b.WriteString(ProgressBar.Render(progress))
	b.WriteString(TextBg.Render("\n\n"))

	// Current page
	if m.current < len(m.pages) {
		b.WriteString(m.pages[m.current].View(pageWidth, m.height))
	}

	content := b.String()

	if m.width > 0 {
		borderWidth := WizardBorder.GetHorizontalBorderSize() + WizardBorder.GetHorizontalMargins()
		return WizardBorder.Width(m.width - borderWidth).Render(content)
	}
	return WizardBorder.Render(content)
}

// shouldSkip returns true if the page at idx should be skipped.
func (m WizardModel) shouldSkip(idx int) bool {
	switch idx {
	case pageWantImport:
		return true // merged into provider selection page
	case pageProjectSelect:
		return false
	case pageSessionSelect:
		return !m.answers.WantImport
	case pageAutoIngest:
		// Skip if not importing, or if session selection was skipped (all import-all).
		if !m.answers.WantImport {
			return true
		}
		// Only show if at least one provider has individually selected sessions.
		for _, ps := range m.answers.ProviderSelections {
			if !ps.ImportAll {
				return false
			}
		}
		return true
	case pageRetention:
		return m.providerInventory[defaults.HarnessClaudeCode].SessionCount == 0
	case pageIngestion:
		return m.journeyRunner != nil || !m.answers.WantImport || m.ingestRunner == nil
	}
	return false
}

// nextPage advances to the next non-skipped page.
// Returns (true, cmd) when advanced, where cmd optionally starts async page work.
// Returns (false, nil) when already past last page.
func (m *WizardModel) nextPage() (bool, tea.Cmd) {
	for i := m.current + 1; i < len(m.pages); i++ {
		if !m.shouldSkip(i) {
			m.current = i
			cmd := m.preparePage(i)
			return true, cmd
		}
	}
	return false, nil
}

// prevPage goes back to the previous non-skipped page.
func (m *WizardModel) prevPage() {
	// If leaving the ingest page, cancel the running pipeline so it
	// doesn't continue in the background while the user edits earlier pages.
	if ip, ok := m.pages[m.current].(*IngestPage); ok {
		ip.Cancel()
	}
	for i := m.current - 1; i >= 0; i-- {
		if !m.shouldSkip(i) {
			m.current = i
			// Reset current page so user can re-answer
			m.resetPage(m.current)
			return
		}
	}
}

// restart resets the wizard to page 0.
func (m WizardModel) restart() WizardModel {
	return NewWizard(
		WithProviderInventory(m.providerInventory),
		WithSessions(m.discoveredSessions),
		WithIngestRunner(m.ingestRunner),
		WithExistingUser(m.existingUser),
		WithProgress(m.progress),
		WithExistingSelection(m.existingSelection),
		WithConfigPersistence(m.configPath, m.loadedConfig),
		WithConfigSnapshot(m.configSnapshot, m.configSnapshotExisted),
		WithJourneyRunner(m.journeyRunner),
		WithJourneyContext(m.journeyContext),
		WithInvocationPWD(m.invocationPWD),
	)
}

// preparePage handles dynamic page reconstruction before display.
// Returns a tea.Cmd if the page needs to start async work immediately.
func (m *WizardModel) preparePage(idx int) tea.Cmd {
	switch idx {
	case pageSessionSelect:
		keys := selectedProjectKeys(m.answers.SelectedProjects)
		if !stringSlicesEqual(keys, m.projectScopeKeys) {
			m.pages[pageSessionSelect] = NewProjectScopePage(m.answers.SelectedProjects, m.providerInventory, m.existingSelection, m.existingSelection != nil)
			m.projectScopeKeys = keys
		}
	case pageSummary:
		if info, ok := m.pages[pageSummary].(*InfoPage); ok {
			info.SetContent(BuildSummaryContent(m.answers))
			info.Reset()
		}
	case pageIngestion:
		if ip, ok := m.pages[pageIngestion].(*IngestPage); ok {
			if m.ingestRunner != nil {
				return ip.Start(m.ingestRunner, m.answers, m.progress)
			}
		}
	}
	return nil
}

// resetPage resets a page so it can be revisited.
func (m *WizardModel) resetPage(idx int) {
	switch p := m.pages[idx].(type) {
	case *OAuthPage:
		p.Reset()
	case *SingleSelectPage:
		p.Reset()
	case *ProviderSelectPage:
		p.Reset()
	case *ProjectSelectPage:
		p.Reset()
	case *MultiSelectPage:
		p.Reset()
	case *TreeSelectPage:
		p.Reset()
	case *ProjectScopePage:
		p.Reset()
	case *PrivacyPreferencePage:
		p.Reset()
	case *LicensePage:
		p.Reset()
	case *RetentionPage:
		p.Reset()
	case *DestinationPage:
		p.Reset()
	case *InfoPage:
		p.Reset()
		if idx == pageSummary {
			p.SetContent(BuildSummaryContent(m.answers))
		}
	case *IngestPage:
		p.Reset()
	}
}

// storeAnswer extracts the value from the page and stores it in answers.
func (m *WizardModel) storeAnswer(pageIndex int) {
	switch pageIndex {
	case pageVillage:
		if p, ok := m.pages[pageVillage].(*OAuthPage); ok {
			m.answers.VillageConnected = p.IsConnected()
			m.pages[pageDestination] = NewDestinationPage(p.IsConnected(), m.loadedConfig)
		}
	case pageWantImport:
		if sp, ok := m.pages[pageWantImport].(*SingleSelectPage); ok {
			m.answers.WantImport = sp.Selected() == 0
		}
	case pageProjectSelect:
		if pp, ok := m.pages[pageProjectSelect].(*ProjectSelectPage); ok {
			m.answers.SelectedProjects = pp.SelectedProjects()
			m.answers.WantImport = len(m.answers.SelectedProjects) > 0
		}
	case pageSessionSelect:
		if tp, ok := m.pages[pageSessionSelect].(*ProjectScopePage); ok {
			m.answers.SelectedSessions = tp.SelectedSessions()
			m.answers.ScopeSelections = tp.ScopeSelections()
			m.answers.ProviderSelections = tp.ProviderSelections()
			m.answers.WantImport = len(m.answers.SelectedProjects) > 0 && len(m.answers.ProviderSelections) > 0
		}
	case pageAutoIngest:
		if sp, ok := m.pages[pageAutoIngest].(*SingleSelectPage); ok {
			m.answers.AutoIngestNewBranches = sp.Selected() == 0 // "Yes" is index 0
		}
	case pagePrivacy:
		if pp, ok := m.pages[pagePrivacy].(*PrivacyPreferencePage); ok {
			m.answers.RedactionLevel = pp.SelectedLevel()
		}
	case pageLicense:
		if lp, ok := m.pages[pageLicense].(*LicensePage); ok {
			m.answers.License = lp.SelectedLicense()
		}
	case pageRetention:
		if rp, ok := m.pages[pageRetention].(*RetentionPage); ok {
			m.answers.ClaudeRetentionDays = rp.SelectedDays()
		}
	case pageDestination:
		if dp, ok := m.pages[pageDestination].(*DestinationPage); ok {
			m.answers.Destination = dp.Destination()
			m.answers.Authentication = dp.Authentication()
			m.answers.RequestedVisibility = dp.RequestedVisibility()
			m.answers.EffectiveVisibility = dp.EffectiveVisibility()
		}
	case pageSummary:
		m.answers.FinalConsent = true
		if m.journeyRunner == nil {
			m.saveErr = m.saveConfig()
		}
		// Write Claude Code retention setting separately (not part of Peasant config).
		if m.journeyRunner == nil && m.saveErr == nil && m.answers.ClaudeRetentionDays > 0 {
			if err := WriteClaudeCleanupDays(m.answers.ClaudeRetentionDays); err != nil {
				m.saveErr = fmt.Errorf("save Claude Code retention setting: %w", err)
			}
		}
	}
}

func (m *WizardModel) startJourney(targets []RetryTarget, prior []PersistedEffect) tea.Cmd {
	answers := *m.answers
	ctx, cancel := context.WithCancel(m.journeyContext)
	m.journeyCancel = cancel
	return func() tea.Msg {
		result, err := m.journeyRunner.Run(ctx, JourneyRequest{Answers: answers, RetryTargets: append([]RetryTarget(nil), targets...), PriorEffects: append([]PersistedEffect(nil), prior...)})
		return journeyFinishedMsg{result: result, err: err}
	}
}

func (m *WizardModel) startJourneyExecution(targets []RetryTarget, prior []PersistedEffect) tea.Cmd {
	m.journeyProgressToken++
	m.executing = true
	return tea.Batch(m.startJourney(targets, prior), journeyProgressTickCmd(m.journeyProgressToken))
}

func (m WizardModel) renderJourneyResult() string {
	const namedEffectDisplayLimit = 5
	var b strings.Builder
	b.WriteString("Setup results\n\n")
	for _, stage := range journeyStageOrder {
		counts := map[ExecutionStatus]int{}
		for _, effect := range m.journeyResult.Effects {
			if effect.Stage == stage {
				counts[effect.Status]++
			}
		}
		if len(counts) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %s:", stage)
		for _, status := range []ExecutionStatus{StatusPersisted, StatusSkipped, StatusFailed, StatusCancelled} {
			if counts[status] > 0 {
				fmt.Fprintf(&b, " %s=%d", status, counts[status])
			}
		}
		b.WriteByte('\n')
	}
	named := 0
	for _, effect := range m.journeyResult.Effects {
		if effect.SessionID == "" && effect.Repository == "" {
			continue
		}
		if named == namedEffectDisplayLimit {
			break
		}
		named++
		fmt.Fprintf(&b, "\n  %s %s", effect.Stage, effect.Status)
		if effect.SessionID != "" {
			fmt.Fprintf(&b, " session=%s", effect.SessionID)
		}
		if effect.Repository != "" {
			fmt.Fprintf(&b, " repository=%s", effect.Repository)
		}
		if effect.Receipt != nil {
			renderAuthoritativeReceipt(&b, effect)
		}
	}
	if remaining := countNamedEffects(m.journeyResult.Effects) - named; remaining > 0 {
		fmt.Fprintf(&b, "\n  and %d more\n", remaining)
	}
	if m.journeyErr != nil {
		fmt.Fprintf(&b, "\nStopped: %v\nCompleted stages above were not rolled back.\n", m.journeyErr)
	}
	if len(m.journeyResult.Retry) > 0 {
		fmt.Fprintf(&b, "\nPress Enter to retry all %d exact failed target(s).\n", len(m.journeyResult.Retry))
	} else {
		b.WriteString("\nPress Enter to finish.\n")
	}
	return WizardBorder.Render(DescriptionStyle.Render(b.String()))
}

func countNamedEffects(effects []PersistedEffect) int {
	count := 0
	for _, effect := range effects {
		if effect.SessionID != "" || effect.Repository != "" {
			count++
		}
	}
	return count
}

func renderAuthoritativeReceipt(b *strings.Builder, effect PersistedEffect) {
	r := effect.Receipt
	fmt.Fprintf(b, "\n    village_origin=%s owner_user_id=%s project_hash=%s", effect.VillageOrigin, effect.OwnerUserID, effect.ProjectHash)
	fmt.Fprintf(b, "\n    remote_transcript_id=%s url=%s visibility=%s", r.TranscriptID, r.TranscriptURL, r.Visibility)
	fmt.Fprintf(b, "\n    content_hash=%s operation_fingerprint=%s", r.ContentHash, r.RequestOperationFingerprint)
	license := "none"
	if r.Applied.License != nil {
		license = r.Applied.License.String()
	}
	fmt.Fprintf(b, "\n    applied.license=%s applied.associations=%s", license, renderReceiptCollection(r.Applied.Associations))
	fmt.Fprintf(b, "\n    applied.normalized.root_harness=%s entry_harnesses=%s derived_title=%v visibility=%s schema_version=%s\n", r.Applied.NormalizedValues.RootHarness, renderReceiptCollection(r.Applied.NormalizedValues.EntryHarnesses), r.Applied.NormalizedValues.DerivedTitle, r.Applied.NormalizedValues.Visibility, r.Applied.NormalizedValues.SchemaVersion)
}

const namedReceiptCollectionLimit = 3

func renderReceiptCollection[T any](values []T) string {
	if len(values) <= namedReceiptCollectionLimit {
		return fmt.Sprint(values)
	}
	return fmt.Sprintf("%v and %d more", values[:namedReceiptCollectionLimit], len(values)-namedReceiptCollectionLimit)
}

func selectedProjectKeys(projects []ProjectCatalogEntry) []string {
	keys := make([]string, len(projects))
	for i, project := range projects {
		keys[i] = project.Key
	}
	return keys
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// visibleProgress returns (currentStep, totalSteps) counting only non-skipped pages.
func (m WizardModel) visibleProgress() (int, int) {
	total := 0
	currentStep := 0
	for i := 0; i < len(m.pages); i++ {
		if !m.shouldSkip(i) {
			total++
			if i <= m.current {
				currentStep = total
			}
		}
	}
	return currentStep, total
}

// saveConfig builds and saves the configuration file.
func (m *WizardModel) saveConfig() error {
	return SaveAnswers(m.configPath, m.loadedConfig, m.configSnapshot, m.configSnapshotExisted, *m.answers)
}

// SaveAnswers persists the final consent snapshot through the same atomic,
// drift-detecting config authority used by the mounted wizard.
func SaveAnswers(path string, loaded *config.Config, snapshot []byte, existed bool, answers WizardAnswers) error {
	// Derive ImportMethod and ImportSources from ProviderSelections for config persistence.
	importMethod := ""
	var importSources []string
	if answers.WantImport {
		allImportAll := true
		for _, ps := range answers.ProviderSelections {
			importSources = append(importSources, ps.Harness)
			if !ps.ImportAll {
				allImportAll = false
			}
		}
		if allImportAll {
			importMethod = "all"
		} else {
			importMethod = "by-source"
		}
	}

	// Build the selection index from wizard answers.
	selection := buildSelectionConfig(&answers)

	cfg := &Config{
		VillageConnected: answers.VillageConnected,
		DaemonMode:       answers.DaemonMode,
		ImportEnabled:    answers.WantImport,
		ImportMethod:     importMethod,
		ImportSources:    importSources,
		RedactionLevel:   answers.RedactionLevel,
		License:          answers.License,
		Selection:        selection,
		ExpectedBytes:    snapshot,
		ExpectedExists:   existed,
		CheckSnapshot:    true,
	}
	if err := cfg.SaveTo(path, loaded); err != nil {
		return fmt.Errorf("save kickstart config during summary confirmation: %w", err)
	}
	return nil
}

// buildSelectionConfig converts wizard answers into a SelectionConfig for persistence.
func buildSelectionConfig(answers *WizardAnswers) *config.SelectionConfig {
	if !answers.WantImport {
		// An empty selected allowlist is the canonical stop-all state. Returning
		// nil here would preserve an existing broad selection (or use the all
		// default), turning an explicit untrack-everything choice into discovery.
		return &config.SelectionConfig{
			Mode:      config.SelectionModeSelected,
			Harnesses: map[string]config.SelectionHarnessConfig{},
		}
	}

	// If all providers chose "Import all", selection mode is "all".
	allImportAll := true
	for _, ps := range answers.ProviderSelections {
		if !ps.ImportAll {
			allImportAll = false
			break
		}
	}
	if allImportAll {
		return &config.SelectionConfig{
			Mode:                  config.SelectionModeAll,
			AutoIngestNewBranches: true,
		}
	}

	if answers.ScopeSelections != nil {
		return buildScopedSelectionConfig(answers)
	}

	// Build per-harness project/branch/session allowlist from selected sessions.
	harnesses := make(map[string]*config.SelectionHarnessConfig)
	for _, s := range answers.SelectedSessions {
		hc, ok := harnesses[s.Harness]
		if !ok {
			hc = &config.SelectionHarnessConfig{}
			harnesses[s.Harness] = hc
		}

		projectKey := s.GitRemote
		projectName := ""
		if projectKey == "" {
			// Fallback: use project name as identifier for local (non-git) projects.
			projectKey = s.ProjectName
			if projectKey == "" {
				// No project grouping — track as explicit session ID.
				hc.Sessions = append(hc.Sessions, s.SessionID)
				continue
			}
			projectName = projectKey
			projectKey = "" // store in Name field, not GitRemote
		}

		// Find or create the project entry.
		projIdx := -1
		for i, proj := range hc.Projects {
			if projectKey != "" && proj.GitRemote == projectKey {
				projIdx = i
				break
			}
			if projectKey == "" && proj.Name == projectName {
				projIdx = i
				break
			}
		}
		if projIdx == -1 {
			proj := config.ProjectSelection{
				GitRemote: projectKey,
				Name:      projectName,
			}
			hc.Projects = append(hc.Projects, proj)
			projIdx = len(hc.Projects) - 1
		}

		// Add branch if present and not already listed.
		if s.Branch != "" {
			found := false
			for _, b := range hc.Projects[projIdx].Branches {
				if b == s.Branch {
					found = true
					break
				}
			}
			if !found {
				hc.Projects[projIdx].Branches = append(hc.Projects[projIdx].Branches, s.Branch)
			}
		}
	}

	// Also add "import all" providers with an empty project list (meaning all projects).
	for _, ps := range answers.ProviderSelections {
		if ps.ImportAll {
			if _, ok := harnesses[ps.Harness]; !ok {
				harnesses[ps.Harness] = &config.SelectionHarnessConfig{}
			}
		}
	}

	result := &config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: answers.AutoIngestNewBranches,
		Harnesses:             make(map[string]config.SelectionHarnessConfig, len(harnesses)),
	}
	for k, v := range harnesses {
		result.Harnesses[k] = *v
	}
	return result
}

func buildScopedSelectionConfig(answers *WizardAnswers) *config.SelectionConfig {
	enabled := make(map[string]bool, len(answers.ProviderSelections))
	for _, provider := range answers.ProviderSelections {
		enabled[provider.Harness] = true
	}
	harnesses := make(map[string]*config.SelectionHarnessConfig, len(enabled))

	for _, choice := range answers.ScopeSelections {
		for _, session := range choice.Sessions {
			if !enabled[session.Harness] {
				continue
			}
			harness := harnesses[session.Harness]
			if harness == nil {
				harness = &config.SelectionHarnessConfig{}
				harnesses[session.Harness] = harness
			}
			if choice.Level == projectScopeSession || session.GitRemote == "" && session.ProjectName == "" {
				if !containsString(harness.Sessions, session.SessionID) {
					harness.Sessions = append(harness.Sessions, session.SessionID)
				}
				continue
			}
			project := projectSelectionForSession(session)
			if choice.Level == projectScopeBranch && session.Branch != "" {
				project.Branches = []string{session.Branch}
			}
			mergeProjectSelection(harness, project)
		}
	}

	result := &config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: answers.AutoIngestNewBranches,
		Harnesses:             make(map[string]config.SelectionHarnessConfig, len(harnesses)),
	}
	for harness, selection := range harnesses {
		result.Harnesses[harness] = *selection
	}
	return result
}

func projectSelectionForSession(session SessionListing) config.ProjectSelection {
	if session.GitRemote != "" {
		return config.ProjectSelection{GitRemote: session.GitRemote}
	}
	return config.ProjectSelection{Name: session.ProjectName}
}

func mergeProjectSelection(harness *config.SelectionHarnessConfig, candidate config.ProjectSelection) {
	for i := range harness.Projects {
		project := &harness.Projects[i]
		if project.GitRemote != candidate.GitRemote || project.Name != candidate.Name {
			continue
		}
		if len(candidate.Branches) == 0 {
			project.Branches = nil
			return
		}
		if len(project.Branches) == 0 {
			return
		}
		for _, branch := range candidate.Branches {
			if !containsString(project.Branches, branch) {
				project.Branches = append(project.Branches, branch)
			}
		}
		return
	}
	harness.Projects = append(harness.Projects, candidate)
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// treeBuiltForCurrentSelections returns true if the tree was already built
// for the current set of provider selections (avoids unnecessary rebuild).
func (m *WizardModel) treeBuiltForCurrentSelections() bool {
	if m.lastTreeSelections == nil {
		return false
	}
	current := m.answers.ProviderSelections
	if len(current) != len(m.lastTreeSelections) {
		return false
	}
	for i := range current {
		if current[i].Harness != m.lastTreeSelections[i].Harness ||
			current[i].ImportAll != m.lastTreeSelections[i].ImportAll {
			return false
		}
	}
	return true
}

// copyProviderSelections returns a deep copy of the current provider selections.
func (m *WizardModel) copyProviderSelections() []ProviderSelection {
	cp := make([]ProviderSelection, len(m.answers.ProviderSelections))
	copy(cp, m.answers.ProviderSelections)
	return cp
}

// buildFilteredSelectionPage creates a TreeSelectPage filtered to checked providers
// from the ProviderSelectPage, with "Import all" providers pre-checked and collapsed.
// When existingSel is non-nil, prior selections are pre-populated from the saved config.
func buildFilteredSelectionPage(allSessions []SessionListing, selections []ProviderSelection, existingSel *config.SelectionConfig) *TreeSelectPage {
	// Build lookup maps from provider selections.
	included := make(map[string]bool, len(selections))
	importAll := make(map[string]bool, len(selections))
	for _, ps := range selections {
		included[ps.Harness] = true
		if ps.ImportAll {
			importAll[ps.Harness] = true
		}
	}

	// Filter sessions to only checked providers.
	var filtered []SessionListing
	for _, s := range allSessions {
		if included[s.Harness] {
			filtered = append(filtered, s)
		}
	}

	page := NewTreeSelectPage("Select Transcripts", filtered)

	// Apply pre-check and expand/collapse logic per provider.
	for pi, prov := range page.providers {
		if importAll[prov.name] {
			// Import-all providers: all sessions pre-checked, collapsed.
			for ri := range prov.remotes {
				for wi := range prov.remotes[ri].worktrees {
					for si := range page.sessionSel[pi][ri][wi] {
						page.sessionSel[pi][ri][wi][si] = true
					}
				}
			}
			page.providers[pi].expanded = false
		} else if existingSel != nil && existingSel.Mode == config.SelectionModeSelected {
			// Pre-populate from existing selection index.
			applyExistingSelection(page, pi, prov, existingSel)
			page.providers[pi].expanded = true
		} else {
			// Select-sessions providers: not pre-checked, provider expanded.
			page.providers[pi].expanded = true
		}
	}

	return page
}

// applyExistingSelection pre-checks sessions in the tree that match the saved
// selection index. It answers a narrower question than the matcher does — what
// to show the user as already chosen — and deliberately diverges from it twice.
// Both divergences are pinned by testdata/persistence.yaml selection_cases.
func applyExistingSelection(page *TreeSelectPage, pi int, prov treeProvider, sel *config.SelectionConfig) {
	// First divergence: a harness with no project entries and no session entries
	// selects everything as far as the matcher is concerned. Here it pre-checks
	// nothing, because "this harness has not been narrowed yet" must not be
	// presented to the user as "you already chose all of these".
	harnessSel, ok := sel.Harnesses[prov.name]
	if !ok || (len(harnessSel.Projects) == 0 && len(harnessSel.Sessions) == 0) {
		return
	}
	matcher := config.CompileSelectionMatcher(*sel)

	for ri, rem := range prov.remotes {
		for wi, wt := range rem.worktrees {
			for si, sess := range wt.sessions {
				// Second divergence: a session the matcher cannot decide
				// (BranchMatchWithheldConflict) is still TICKED, so the test is
				// against BranchMatchNo rather than for BranchMatchYes.
				//
				// The reason is that a tick box here is not a display: page
				// SelectedSessions is the write-back path, so confirming the
				// wizard with the box clear DELETES that session from the saved
				// selection. Leaving it clear would resolve the conflict by
				// deletion and destroy the record that anything was ever in
				// conflict — silently, as a side effect of re-running the
				// wizard. Ticking is what makes re-confirming non-destructive.
				//
				// A plain tick would then claim the session will be ingested,
				// which is false: ingest withholds it. So it is ticked AND
				// marked, and the renderer shows [!] rather than [✓].
				switch matcher.MatchDiscovery(ingest.Harness(sess.Harness), sess.GitRemote, sess.ProjectName, sess.Branch, ingest.SessionID(sess.SessionID), sel.AutoIngestNewBranches) {
				case ingest.BranchMatchYes:
					page.sessionSel[pi][ri][wi][si] = true
				case ingest.BranchMatchWithheldConflict:
					page.sessionSel[pi][ri][wi][si] = true
					page.markSessionConflicted(pi, ri, wi, si)
				}
			}
		}
	}
}
