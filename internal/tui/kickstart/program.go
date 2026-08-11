package kickstart

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// Phase names the stage the kickstart program is in. It is a closed enum so the
// Update dispatch and the tests address stages by a named constant, never a bare
// int or string.
type Phase int

const (
	// PhaseOAuth offers the optional village connection before any config is
	// touched; its result feeds the flow's village-gated fields.
	PhaseOAuth Phase = iota
	// PhaseFlow runs the declarative settings.Flow: the single atomic config
	// commit point (esc there always prompts a confirm-exit that writes nothing).
	PhaseFlow
	// PhaseVisibility offers the kickstart-only login choice when a user reaches
	// sharing guidance while disconnected. Config Screen never enters this phase.
	PhaseVisibility
	// PhaseIngest runs the mounted local FTUE ingest boundary ONLY after a
	// successful commit, preserving the ordering (save, then import).
	PhaseIngest
	// PhaseDone is the persistent terminal stage after local ingest succeeds or
	// fails, or after the user confirms a no-save exit.
	PhaseDone
)

// String returns a stable lower-case name for p.
func (p Phase) String() string {
	switch p {
	case PhaseOAuth:
		return "oauth"
	case PhaseFlow:
		return "flow"
	case PhaseVisibility:
		return "visibility"
	case PhaseIngest:
		return "ingest"
	case PhaseDone:
		return "done"
	default:
		return "unknown"
	}
}

// LoginFunc performs the village OAuth login and returns the authenticated
// username. Production injects the path-aware internal/auth.LoginFrom runner so
// the program never reaches for auth or the network itself; a test supplies a fake.
type LoginFunc func(ctx context.Context) (username string, err error)

// IngestFunc runs local transcript ingest after the config is saved and returns
// its summary. Production wraps the mounted FTUE local-ingest runner, closed
// over the freshly committed config path so it imports exactly what was saved.
type IngestFunc func(ctx context.Context) (*ftue.IngestResult, error)

// ProgramDeps are the runtime seams the kickstart program composes. Every field
// is business logic the legacy wizard already owned (login, ingest, discovery,
// retention), reused untouched; the program only sequences them on the kit.
type ProgramDeps struct {
	// Theme styles every kit surface.
	Theme theme.Theme
	// Draft is the buffered config the flow commits atomically.
	Draft *settings.Draft
	// Source feeds the selection tree (real scanner adapter in production,
	// fixture source in tests).
	Source kit.TreeSource
	// ClaudeSessionsPresent gates the Claude retention preference.
	ClaudeSessionsPresent bool
	// Preview loads the transcript shown beside the selection tree. When nil the
	// selection step renders the tree alone.
	Preview kit.BodySource
	// Login runs the optional village OAuth. When nil the OAuth step is a no-op
	// skip and the flow's village-gated fields stay hidden.
	Login LoginFunc
	// Ingest runs the pipeline after commit. When nil the program finishes at
	// commit without importing (config-only run).
	Ingest IngestFunc
	// Progress is the concurrent-safe pull source populated by the same local
	// ingest run. Program observes it on Tick without blocking pipeline workers.
	Progress ProgressSource
	// Clock and Tick are the injected timing boundaries for honest elapsed and
	// qualified ETA presentation.
	Clock Clock
	Tick  TickFunc
	// NextSteps produces display-only completion instructions. Program renders
	// them but has no authority to execute any command they name.
	NextSteps NextStepsFunc
	// Retention writes the Claude cleanupPeriodDays preference AFTER the config
	// save (legacy ordering). When nil it is skipped. RetentionDays is an explicit
	// fallback used only when the guided retention section was not offered; zero
	// therefore authorizes no hidden write.
	Retention     RetentionWriter
	RetentionDays int
	// AlreadyConnected is true when the machine already holds valid village
	// credentials. When set, the connect-now step is SKIPPED entirely (the user
	// is already connected, so re-asking is noise) and the flow's village-gated
	// fields are shown as connected. The mount computes this from the existing
	// credential store.
	AlreadyConnected bool
	// Context bounds the login and ingest work.
	Context context.Context
}

// loginDoneMsg carries the result of the injected LoginFunc.
type loginDoneMsg struct {
	username string
	err      error
}

// ingestDoneMsg carries the result of the injected IngestFunc.
type ingestDoneMsg struct {
	result *ftue.IngestResult
	err    error
}

// progressTickMsg asks Program to take one non-blocking snapshot from the
// concurrent ingest progress source. generation binds the recurring timer to
// the ingest attempt that created it, so a late retry-era tick cannot start a
// second polling chain.
type progressTickMsg struct {
	at         time.Time
	generation uint64
}

const progressPollInterval = 100 * time.Millisecond

const (
	oauthPrompt      = "connect to a village now?"
	visibilityPrompt = "log in now to choose a default sharing visibility?"
)

// villageConnectionState is shared by every Program value Bubble Tea produces.
// Registry visibility closes over this pointer, so authentication can reveal a
// section without replacing the mounted Registry, Flow, or any stateful field.
type villageConnectionState struct {
	connected bool
}

func (s *villageConnectionState) Connected() bool {
	return s != nil && s.connected
}

func (s *villageConnectionState) Set(connected bool) {
	if s != nil {
		s.connected = connected
	}
}

// Program is the mounted first-run onboarding: OAuth -> settings.Flow (with an
// optional visibility-login detour) -> local Ingest -> persistent Done, all
// rendered on the kit. The flow is the single commit point; ingest runs only
// after that commit succeeds. A confirmed no-save exit instead enters Done
// without writing config or running ingest.
type Program struct {
	deps ProgramDeps

	phase Phase

	oauth      kit.Confirm
	visibility kit.Confirm
	overlay    kit.Overlay
	spinner    kit.Spinner
	authInFlt  bool
	connection *villageConnectionState
	loginErr   error
	// visibilityAsked prevents repeated login interruptions after the user has
	// explicitly continued locally.
	visibilityAsked bool

	flow      settings.Flow
	flowBuilt bool

	ingestRes          *ftue.IngestResult
	ingestErr          error
	retentionErr       error
	retentionAttempted bool
	retentionApplied   bool
	retryAttempt       bool
	ingestGeneration   uint64
	attemptStarted     time.Time
	stageObservations  map[ingest.Stage]stageObservation
	nextSteps          []NextStepKind
	nextStepsErr       error

	width, height int
}

// NewProgram builds the kickstart program and its one mounted settings flow.
// Village-gated fields read the stable connection state dynamically, so later
// authentication changes visibility without replacing the flow.
func NewProgram(deps ProgramDeps) Program {
	if deps.Context == nil {
		deps.Context = context.Background()
	}
	if deps.Clock == nil {
		deps.Clock = ClockFunc(time.Now)
	}
	if deps.Tick == nil {
		deps.Tick = tea.Tick
	}
	if deps.NextSteps == nil {
		deps.NextSteps = DefaultNextSteps
	}
	p := Program{
		deps:              deps,
		phase:             PhaseOAuth,
		oauth:             kit.NewConfirm(deps.Theme, oauthPrompt),
		visibility:        kit.NewConfirm(deps.Theme, visibilityPrompt),
		overlay:           kit.NewOverlay(deps.Theme),
		spinner:           kit.NewSpinner(deps.Theme, "working"),
		connection:        &villageConnectionState{connected: deps.AlreadyConnected},
		stageObservations: map[ingest.Stage]stageObservation{},
	}
	p = p.buildFlow()
	if deps.AlreadyConnected {
		// Already holding valid credentials: skip the connect-now step and open
		// straight into the flow with the village-gated fields shown. The flow's
		// startup command is not dropped here - the constructor cannot return a
		// tea.Cmd, so Program.Init emits flow.Init() once bubbletea starts the
		// program (see buildFlow).
		p.phase = PhaseFlow
	}
	return p
}

// Phase reports the current stage (primarily for tests and the mount).
func (p Program) Phase() Phase { return p.phase }

// Connected reports whether the village OAuth step authenticated.
func (p Program) Connected() bool { return p.connection.Connected() }

// Committed reports whether the flow committed the config.
func (p Program) Committed() bool { return p.flowBuilt && p.flow.Committed() }

// Exited reports whether the user confirmed a no-save exit in the flow.
func (p Program) Exited() bool { return p.flowBuilt && p.flow.Exited() }

// IngestResult reports the pipeline summary, or nil when ingest did not run.
func (p Program) IngestResult() *ftue.IngestResult { return p.ingestRes }

// IngestErr reports an ingest failure, if any.
func (p Program) IngestErr() error { return p.ingestErr }

// RetentionErr reports a retention-write failure, if any.
func (p Program) RetentionErr() error { return p.retentionErr }

// NextStepsErr reports an invalid completion-provider result. Local setup is
// still complete; the unverified display guidance is withheld.
func (p Program) NextStepsErr() error { return p.nextStepsErr }

// Init starts the active phase: the flow's async startup when the connect-now
// step was skipped (already connected), else the OAuth confirm's cursor.
func (p Program) Init() tea.Cmd {
	if p.phase == PhaseFlow {
		return p.flow.Init()
	}
	return p.oauth.Focus()
}

// Confirming reports whether an exit-confirm modal is currently open in the
// flow. It is exposed so a test can prove esc opens the modal rather than
// exiting outright.
func (p Program) Confirming() bool { return p.flowBuilt && p.flow.Confirming() }

// SetSize records the render region and propagates it to the active surface.
func (p *Program) SetSize(width, height int) {
	p.width, p.height = width, height
	p.oauth.SetSize(promptWidth(oauthPrompt, width), kit.ConfirmMinSize.Height)
	p.visibility.SetSize(promptWidth(visibilityPrompt, width), kit.ConfirmMinSize.Height)
	p.overlay.SetSize(width, height)
	p.spinner.SetSize(width, height)
	if p.flowBuilt {
		p.flow.SetSize(width, height)
	}
}

// Update dispatches by phase.
func (p Program) Update(msg tea.Msg) (Program, tea.Cmd) {
	if sz, ok := msg.(tea.WindowSizeMsg); ok {
		p.SetSize(sz.Width, sz.Height)
		return p, nil
	}
	switch p.phase {
	case PhaseOAuth:
		return p.updateOAuth(msg)
	case PhaseFlow:
		return p.updateFlow(msg)
	case PhaseVisibility:
		return p.updateVisibility(msg)
	case PhaseIngest:
		return p.updateIngest(msg)
	case PhaseDone:
		return p.updateDone(msg)
	default:
		return p, nil
	}
}

// updateOAuth drives the connect-now confirm and, when the user accepts, the
// injected login runner behind a spinner. Either outcome (connected or skipped)
// advances to the flow.
func (p Program) updateOAuth(msg tea.Msg) (Program, tea.Cmd) {
	switch m := msg.(type) {
	case loginDoneMsg:
		p.authInFlt = false
		if m.err != nil {
			p.loginErr = loginActionableError(m.err, "the initial village connection step")
			p.connection.Set(false)
			return p, nil
		}
		p.loginErr = nil
		p.connection.Set(true)
		return p.enterFlow()
	case tea.KeyPressMsg:
		if p.authInFlt {
			return p, nil
		}
		var cmd tea.Cmd
		p.oauth, cmd = p.oauth.Update(m)
		if cmd != nil {
			if res, ok := runOnce(cmd).(kit.ConfirmResultMsg); ok {
				if res.OK && p.deps.Login != nil {
					p.loginErr = nil
					p.authInFlt = true
					p.spinner = p.spinner.SetLabel("connecting to village")
					return p, tea.Batch(p.runLogin(), p.spinner.Tick())
				}
				// Declined or no login runner: proceed local-only.
				return p.enterFlow()
			}
		}
		return p, cmd
	default:
		if p.authInFlt {
			var cmd tea.Cmd
			p.spinner, cmd = p.spinner.Update(msg)
			return p, cmd
		}
		return p, nil
	}
}

// runLogin issues the injected login runner as a command.
func (p Program) runLogin() tea.Cmd {
	login := p.deps.Login
	ctx := p.deps.Context
	return func() tea.Msg {
		username, err := login(ctx)
		return loginDoneMsg{username: username, err: err}
	}
}

// enterFlow opens the already-mounted settings.Flow with the current village
// connection state and returns its one async startup command (the tree scan) so
// the caller can dispatch it on first entry.
func (p Program) enterFlow() (Program, tea.Cmd) {
	p.phase = PhaseFlow
	if current := p.flow.CurrentSectionKey(); current != "" {
		if err := p.flow.OpenSection(current); err != nil {
			p.phase = PhaseOAuth
			p.loginErr = fmt.Errorf(
				"guided settings unavailable.\n"+
					"what: kickstart could not enter its retained settings flow.\n"+
					"why: %v\n"+
					"where: kickstart Program after the initial village connection choice.\n"+
					"when: before transcript scanning or final consent.\n"+
					"means: the draft remains buffered and no setting or transcript was published.\n"+
					"fix: exit without saving, rerun kickstart, and report the missing guided section.",
				err)
			return p, nil
		}
	}
	return p, p.flow.Init()
}

// buildFlow constructs the one settings.Registry and Flow for this Program.
// Their destination visibility reads the stable connection pointer at runtime,
// so neither object is rebuilt after authentication. The constructor cannot
// dispatch Flow.Init; Program.Init or enterFlow emits it exactly once when the
// guided flow first starts.
func (p Program) buildFlow() Program {
	connection := p.connection
	reg := BuildRegistry(Options{
		Source:                p.deps.Source,
		VillageConnected:      p.Connected(),
		VillageConnectedFunc:  connection.Connected,
		ClaudeSessionsPresent: p.deps.ClaudeSessionsPresent,
		Preview:               p.deps.Preview,
	})
	p.flow = settings.NewFlow(p.deps.Theme, reg, p.deps.Draft,
		settings.WithConsentSummary(p.consentSummary))
	p.flow.SetSize(p.width, p.height)
	p.flowBuilt = true
	return p
}

// consentSummary derives the final explanation from the Flow's canonical
// read-only receipt context. Every conditional row is gated by a visible field
// identity, so this presentation never duplicates Registry When predicates.
func (p Program) consentSummary(ctx settings.ConsentContext) (settings.ConsentSummary, error) {
	cfg, err := ctx.Config()
	if err != nil {
		return settings.ConsentSummary{}, fmt.Errorf(
			"build kickstart consent summary.\n"+
				"what: the converged settings draft could not be copied for final review.\n"+
				"why: %v.\n"+
				"where: kickstart consent summary at the review and save step.\n"+
				"when: after hidden settings were reset and before confirmation.\n"+
				"means: no consent summary was invented and no setting was saved.\n"+
				"fix: leave without saving, correct the invalid configuration value, and rerun kickstart.", err)
	}
	values := make([]string, 0, 5)
	if ctx.HasVisibleField(SectionSelection, FieldSelection) {
		if cfg.Selection.Mode == config.SelectionModeSelected {
			values = append(values, "selection: only the buffered projects, branches, and sessions")
		} else {
			values = append(values, "selection: all discovered sessions")
		}
	}
	if ctx.HasVisibleField(SectionAutoIngest, FieldAutoIngest) {
		state := "off"
		if cfg.Selection.AutoIngestNewBranches {
			state = "on"
		}
		values = append(values, "auto-ingest future branches in fully-selected projects: "+state)
	}
	if ctx.HasVisibleField(SectionPrivacy, FieldPrivacy) {
		values = append(values, "publication privacy: "+strings.ToLower(cfg.Redaction.Level.String())+
			" redaction; local imports remain original unless you run `peasant redact`")
	}
	if ctx.HasVisibleField(SectionLicense, FieldLicense) {
		if cfg.Push.License == "" {
			values = append(values, "later publish license: none; all rights remain and reuse requires permission")
		} else {
			values = append(values, "later publish license: "+string(cfg.Push.License))
		}
	}
	if ctx.HasVisibleField(SectionDestination, FieldVisibility) {
		values = append(values, "default visibility after a later publish: "+string(cfg.Push.Visibility))
	}
	if ctx.HasVisibleField(SectionRetention, FieldRetention) && cfg.ClaudeRetentionDays > 0 {
		values = append(values, fmt.Sprintf("claude code source retention: %d days", cfg.ClaudeRetentionDays))
	}

	effects := []string{"save the visible choices to peasant configuration"}
	if ctx.HasVisibleField(SectionRetention, FieldRetention) && cfg.ClaudeRetentionDays > 0 {
		effects = append(effects, "apply claude code retention after config saves")
	}
	if ctx.HasVisibleField(SectionSelection, FieldSelection) {
		effects = append(effects, "import the selected transcripts into the local peasant store")
	}
	effects = append(effects, "publish nothing; sharing requires a later explicit push")
	return settings.ConsentSummary{Values: values, Effects: effects}, nil
}

// updateFlow forwards to the settings.Flow. When the flow commits, ingest starts
// (legacy ordering: save THEN import); when the flow exits (confirmed esc),
// nothing was written and the program is done.
func (p Program) updateFlow(msg tea.Msg) (Program, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok &&
		!p.Connected() && !p.visibilityAsked && p.deps.Login != nil &&
		!p.flow.Confirming() && !p.flow.Helping() &&
		p.flow.CurrentSectionKey() == SectionLicense {
		if action, matched := keymap.Match(keymap.Default(), keyMsg,
			programActionAvailability{keymap.ActionNextField}); matched && action == keymap.ActionNextField {
			p.phase = PhaseVisibility
			p.visibility = kit.NewConfirm(p.deps.Theme, visibilityPrompt)
			p.visibility.SetSize(promptWidth(visibilityPrompt, p.width), kit.ConfirmMinSize.Height)
			return p, p.visibility.Focus()
		}
	}
	var cmd tea.Cmd
	p.flow, cmd = p.flow.Update(msg)
	if p.flow.Exited() {
		p.phase = PhaseDone
		return p, tea.Quit
	}
	if p.flow.Committed() {
		return p.afterCommit()
	}
	return p, cmd
}

// updateVisibility offers authentication exactly where its consequence becomes
// understandable: before the sharing visibility choice. The modal exclusively
// owns keys and loginDoneMsg, while every other non-key message continues to the
// retained Flow and, during authentication, the owner-tagged login spinner.
// Declining resumes the typed Flow transition that brought the user here.
// Success updates the live visibility state on the retained registry, then opens
// the mounted sharing step.
func (p Program) updateVisibility(msg tea.Msg) (Program, tea.Cmd) {
	switch m := msg.(type) {
	case loginDoneMsg:
		p.authInFlt = false
		if m.err != nil {
			p.connection.Set(false)
			p.loginErr = loginActionableError(m.err, "the sharing visibility login step")
			return p, nil
		}
		p.connection.Set(true)
		p.visibilityAsked = true
		p.loginErr = nil
		p.phase = PhaseFlow
		if err := p.flow.OpenSection(SectionDestination); err != nil {
			p.phase = PhaseVisibility
			p.loginErr = fmt.Errorf(
				"sharing guidance unavailable after login.\n"+
					"what: kickstart authenticated but could not open the sharing visibility choice.\n"+
					"why: %v\n"+
					"where: kickstart Program while restoring the guided flow after village login.\n"+
					"when: after authentication succeeded and before final consent.\n"+
					"means: the draft remains buffered and no configuration or transcript was published.\n"+
					"fix: exit without saving, rerun kickstart, and report the missing sharing section.",
				err)
			return p, nil
		}
		return p, nil
	case tea.KeyPressMsg:
		if p.authInFlt {
			return p, nil
		}
		var cmd tea.Cmd
		p.visibility, cmd = p.visibility.Update(m)
		if cmd == nil {
			return p, nil
		}
		result, ok := runOnce(cmd).(kit.ConfirmResultMsg)
		if !ok {
			return p, cmd
		}
		if !result.OK {
			p.visibilityAsked = true
			p.loginErr = nil
			p.phase = PhaseFlow
			p.flow.ResumeNextField()
			return p, nil
		}
		if p.deps.Login == nil {
			p.loginErr = loginActionableError(
				fmt.Errorf("the login runner is not wired"),
				"the sharing visibility login step")
			return p, nil
		}
		p.loginErr = nil
		p.authInFlt = true
		p.spinner = p.spinner.SetLabel("connecting to village")
		return p, tea.Batch(p.runLogin(), p.spinner.Tick())
	default:
		var commands []tea.Cmd
		if p.flowBuilt {
			var command tea.Cmd
			p.flow, command = p.flow.Update(msg)
			if command != nil {
				commands = append(commands, command)
			}
		}
		if p.authInFlt {
			var command tea.Cmd
			p.spinner, command = p.spinner.Update(msg)
			if command != nil {
				commands = append(commands, command)
			}
		}
		if len(commands) == 0 {
			return p, nil
		}
		return p, tea.Batch(commands...)
	}
}

// afterCommit runs the retention write (after the config save) and then starts
// ingest. Retention is a synchronous local write; ingest runs behind a spinner.
func (p Program) afterCommit() (Program, tea.Cmd) {
	// Claude retention write happens AFTER the config save and only when a
	// preference was set - the legacy ordering. The chosen value comes from the
	// retention field the flow just committed; the injected RetentionDays is the
	// fallback for a caller that pre-set it without offering the field.
	if p.deps.Retention != nil {
		days := p.retentionDays()
		if days > 0 {
			p.retentionAttempted = true
			if err := p.deps.Retention.WriteCleanupDays(days); err != nil {
				p.retentionErr = err
			} else {
				p.retentionApplied = true
			}
		}
	}
	if p.deps.Ingest == nil {
		p.phase = PhaseDone
		p = p.resolveNextSteps(nil)
		return p, nil
	}
	return p.startIngest(false)
}

// startIngest begins one local-only attempt. It resets only volatile progress
// and timing state; config and retention are durable effects owned by the first
// post-consent transition and are deliberately not revisited on retry.
func (p Program) startIngest(retry bool) (Program, tea.Cmd) {
	p.ingestGeneration++
	generation := p.ingestGeneration
	p.phase = PhaseIngest
	p.ingestRes = nil
	p.ingestErr = nil
	p.retryAttempt = retry
	p.attemptStarted = p.deps.Clock.Now()
	p.stageObservations = map[ingest.Stage]stageObservation{}
	if p.deps.Progress != nil {
		p.deps.Progress.Reset()
	}
	// A fresh spinner carries a fresh bubbles generation tag. Late animation
	// messages from an earlier failed attempt are therefore ignored rather than
	// extending a second animation chain into the retry.
	p.spinner = kit.NewSpinner(p.deps.Theme, "importing transcripts")
	p.spinner.SetSize(p.width, p.height)
	return p, tea.Batch(p.runIngest(), p.progressTick(generation), p.spinner.Tick())
}

// retentionDays reports the Claude Code cleanup period to write. A Draft value
// is eligible only when discovery offered the retention section to the user;
// otherwise only the caller's explicit fallback may authorize a write.
func (p Program) retentionDays() int {
	if p.deps.ClaudeSessionsPresent && p.deps.Draft != nil {
		if chosen := p.deps.Draft.Working().ClaudeRetentionDays; chosen > 0 {
			return chosen
		}
	}
	return p.deps.RetentionDays
}

// runIngest issues the injected ingest runner as a command.
func (p Program) runIngest() tea.Cmd {
	ingest := p.deps.Ingest
	ctx := p.deps.Context
	return func() tea.Msg {
		res, err := ingest(ctx)
		return ingestDoneMsg{result: res, err: err}
	}
}

func (p Program) progressTick(generation uint64) tea.Cmd {
	return p.deps.Tick(progressPollInterval, func(at time.Time) tea.Msg {
		return progressTickMsg{at: at, generation: generation}
	})
}

// updateIngest advances the spinner until the pipeline reports its result.
func (p Program) updateIngest(msg tea.Msg) (Program, tea.Cmd) {
	switch m := msg.(type) {
	case ingestDoneMsg:
		p = p.observeProgress(p.deps.Clock.Now())
		p.ingestRes = m.result
		p.ingestErr = m.err
		p.phase = PhaseDone
		if m.err == nil {
			p = p.resolveNextSteps(m.result)
		}
		return p, nil
	case progressTickMsg:
		if m.generation != p.ingestGeneration {
			return p, nil
		}
		p = p.observeProgress(m.at)
		return p, p.progressTick(m.generation)
	default:
		var cmd tea.Cmd
		p.spinner, cmd = p.spinner.Update(m)
		return p, cmd
	}
}

// resolveNextSteps validates the provider's complete typed result once. Invalid
// guidance is retained as an actionable presentation error rather than being
// silently dropped one row at a time.
func (p Program) resolveNextSteps(result *ftue.IngestResult) Program {
	kinds := p.deps.NextSteps(result)
	if err := validateNextSteps(kinds); err != nil {
		p.nextSteps = nil
		p.nextStepsErr = err
		return p
	}
	p.nextSteps = append([]NextStepKind(nil), kinds...)
	p.nextStepsErr = nil
	return p
}

// observeProgress records presentation timing from one non-blocking snapshot.
// An estimate is qualified only after the same positive total is observed twice
// with monotonic forward progress, for one active non-error stage on a first
// attempt. Every unsupported state remains explicit rather than guessed.
func (p Program) observeProgress(at time.Time) Program {
	if at.IsZero() {
		at = p.deps.Clock.Now()
	}
	if p.deps.Progress == nil {
		return p
	}
	snapshot := p.deps.Progress.Snapshot()
	active := 0
	for _, stage := range ingest.StageOrder {
		sp := snapshot[stage]
		if sp.Started && !sp.Ended {
			active++
		}
	}
	for _, stage := range ingest.StageOrder {
		sp := snapshot[stage]
		if !sp.Started {
			continue
		}
		observation, seen := p.stageObservations[stage]
		if !seen {
			observation = stageObservation{
				startedAt:        at,
				lastAt:           at,
				lastDone:         sp.Done,
				lastTotal:        sp.Total,
				progress:         sp,
				estimateEligible: sp.Total > 0 && !sp.HasErr && !p.retryAttempt,
			}
			p.stageObservations[stage] = observation
			continue
		}

		observation.estimateValid = false
		stableTotal := sp.Total > 0 && sp.Total == observation.lastTotal
		monotonic := sp.Done >= observation.lastDone
		if !stableTotal || !monotonic || sp.HasErr || p.retryAttempt {
			observation.estimateEligible = false
		}
		advanced := sp.Done > observation.lastDone
		interval := at.Sub(observation.lastAt)
		if observation.estimateEligible && active == 1 && advanced && interval > 0 &&
			!sp.Ended && sp.Done < sp.Total {
			remaining := sp.Total - sp.Done
			delta := sp.Done - observation.lastDone
			observation.estimate = time.Duration(int64(interval) * int64(remaining) / int64(delta))
			observation.estimateValid = observation.estimate >= 0
		}
		observation.lastAt = at
		observation.lastDone = sp.Done
		observation.lastTotal = sp.Total
		observation.progress = sp
		p.stageObservations[stage] = observation
	}
	return p
}

// updateDone keeps completion mounted until an explicit typed action. Enter is
// available only after failure and starts local ingest again without touching
// the already-committed Draft or retention effect.
func (p Program) updateDone(msg tea.Msg) (Program, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil
	}
	actions := []keymap.ActionID{keymap.ActionBack, keymap.ActionQuit}
	if p.ingestErr != nil && p.deps.Ingest != nil {
		actions = append([]keymap.ActionID{keymap.ActionConfirm}, actions...)
	}
	action, matched := keymap.Match(keymap.Default(), keyMsg, programActionAvailability(actions))
	if !matched {
		return p, nil
	}
	switch action {
	case keymap.ActionConfirm:
		return p.startIngest(true)
	case keymap.ActionBack, keymap.ActionQuit:
		return p, tea.Quit
	default:
		return p, nil
	}
}

// View renders the active phase's surface.
func (p Program) View() string {
	switch p.phase {
	case PhaseOAuth:
		if p.authInFlt {
			return p.centered(p.spinner.View())
		}
		return p.viewOAuth()
	case PhaseFlow:
		return p.flow.View()
	case PhaseVisibility:
		if p.authInFlt {
			return p.centered(p.spinner.View())
		}
		return p.viewVisibility()
	case PhaseIngest:
		return p.viewIngest()
	case PhaseDone:
		return p.viewDone()
	default:
		return ""
	}
}

// villageContext explains, in plain language, what connecting to a village does
// and why - the UAT flagged that the connect-now prompt appeared with no
// context at all. It is shown above the confirm in the centered dialog.
const villageContext = "a village is a shared commons for agent transcripts.\n" +
	"local mode keeps all data on this machine.\n" +
	"connecting sends nothing until a later explicit publish.\n" +
	"publishing is separate, explicit, and opt-in. you can connect\n" +
	"later at any time with `peasant village login`."

// viewOAuth renders the connect-now dialog CENTERED in the terminal (the UAT
// flagged the top-left anchoring as strange), with the explanatory context above
// the yes/no confirm, over a fully themed background.
func (p Program) viewOAuth() string {
	styles := p.deps.Theme.Styles()
	var b strings.Builder
	b.WriteString(styles.Header.Render("connect to a village"))
	b.WriteString("\n\n")
	for _, ln := range strings.Split(villageContext, "\n") {
		b.WriteString(styles.Muted.Render(ln))
		b.WriteString("\n")
	}
	if p.loginErr != nil {
		b.WriteString("\n")
		for _, ln := range strings.Split(p.loginErr.Error(), "\n") {
			b.WriteString(styles.Danger.Render(ln))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(p.oauth.View())
	return p.centered(b.String())
}

func (p Program) viewVisibility() string {
	styles := p.deps.Theme.Styles()
	var b strings.Builder
	b.WriteString(styles.Header.Render("choose sharing visibility"))
	b.WriteString("\n\n")
	b.WriteString(styles.Muted.Render("logging in now reveals a default visibility choice for a later explicit publish."))
	b.WriteString("\n")
	b.WriteString(styles.Muted.Render("login authenticates only; it does not save the draft or publish a transcript."))
	if p.loginErr != nil {
		b.WriteString("\n\n")
		for _, line := range strings.Split(p.loginErr.Error(), "\n") {
			b.WriteString(styles.Danger.Render(line))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(p.visibility.View())
	return p.centered(b.String())
}

func (p Program) viewIngest() string {
	styles := p.deps.Theme.Styles()
	lines := []string{
		p.spinner.View(),
		"",
		styles.Header.Render("local import progress"),
	}
	lines = append(lines, p.progressLines(styles, p.deps.Clock.Now())...)
	if p.height > 0 && len(lines) > p.height {
		lines = lines[:p.height]
	}
	return strings.Join(lines, "\n")
}

func (p Program) progressLines(styles theme.Styles, now time.Time) []string {
	if now.Before(p.attemptStarted) {
		now = p.attemptStarted
	}
	lines := []string{styles.Base.Render("local import elapsed: " + displayDuration(now.Sub(p.attemptStarted)))}
	focus, hasFocus := p.progressFocusStage()
	shown := false
	for _, stage := range ingest.StageOrder {
		observation, ok := p.stageObservations[stage]
		if !ok || !observation.progress.Started {
			continue
		}
		shown = true
		sp := observation.progress
		label := strings.ToLower(stage.String()) + " " + fmt.Sprintf("%d", sp.Done)
		if sp.Total > 0 {
			label += fmt.Sprintf("/%d", sp.Total)
		}
		if sp.HasErr {
			label += " failed"
		}
		lines = append(lines, styles.Base.Render(label))
		if !hasFocus || stage != focus {
			continue
		}
		end := now
		if sp.Ended && observation.lastAt.Before(end) {
			end = observation.lastAt
		}
		lines = append(lines, styles.Muted.Render("  observed elapsed: "+displayDuration(end.Sub(observation.startedAt))))
		if observation.estimateValid {
			lines = append(lines, styles.Muted.Render("  estimate: "+displayDuration(observation.estimate)))
		} else {
			lines = append(lines, styles.Muted.Render("  estimate unavailable"))
		}
	}
	if !shown {
		lines = append(lines,
			styles.Muted.Render("waiting for the first progress update"),
			styles.Muted.Render("estimate unavailable"))
	}
	const ingestHeaderLines = 3
	available := p.height - ingestHeaderLines
	if p.height > 0 && len(lines) > available {
		if available <= 0 {
			return nil
		}
		if available == 1 {
			return lines[:1]
		}
		// Whole-run elapsed is always the first line. Keep it plus the newest
		// stage window so the current or latest stage survives a short terminal.
		window := make([]string, 0, available)
		window = append(window, lines[0])
		window = append(window, lines[len(lines)-(available-1):]...)
		return window
	}
	return lines
}

// progressFocusStage chooses the stage whose elapsed and estimate detail is
// most useful now. All observed stages retain a compact status row; detailed
// timing belongs to the latest failure, otherwise the latest active stage, and
// finally the latest completed stage.
func (p Program) progressFocusStage() (ingest.Stage, bool) {
	var latest, active, failed ingest.Stage
	var hasLatest, hasActive, hasFailed bool
	for _, stage := range ingest.StageOrder {
		observation, ok := p.stageObservations[stage]
		if !ok || !observation.progress.Started {
			continue
		}
		latest, hasLatest = stage, true
		if !observation.progress.Ended {
			active, hasActive = stage, true
		}
		if observation.progress.HasErr {
			failed, hasFailed = stage, true
		}
	}
	if hasFailed {
		return failed, true
	}
	if hasActive {
		return active, true
	}
	return latest, hasLatest
}

func (p Program) viewDone() string {
	styles := p.deps.Theme.Styles()
	if p.Exited() && !p.Committed() {
		return strings.Join([]string{
			styles.Header.Render("kickstart closed"),
			styles.Base.Render("nothing was saved, imported, or published"),
		}, "\n")
	}
	var lines []string
	if p.ingestErr != nil {
		lines = append(lines,
			styles.Header.Render("local import needs attention"),
			styles.Base.Render("config remains saved"))
		if p.retentionApplied {
			lines = append(lines, styles.Base.Render("retention remains applied"))
		} else if p.retentionAttempted && p.retentionErr != nil {
			lines = append(lines, styles.Danger.Render(retentionActionableError(p.retentionErr)))
		}
		lines = append(lines,
			styles.Danger.Render("local import failed"),
			styles.Danger.Render(localImportActionableError(p.ingestErr)),
			styles.Muted.Render("estimate unavailable"),
			styles.Base.Render("retry local import only with enter"),
			styles.Base.Render("kickstart published nothing"))
		lines = append(lines, "", keymap.FooterView(p.deps.Theme, keymap.Default(),
			programActionAvailability{keymap.ActionConfirm, keymap.ActionBack, keymap.ActionQuit}))
		return strings.Join(lines, "\n")
	}

	lines = append(lines,
		styles.Header.Render("kickstart complete"),
		styles.Base.Render("config saved"))
	if p.retentionApplied {
		lines = append(lines, styles.Base.Render("retention applied"))
	} else if p.retentionAttempted && p.retentionErr != nil {
		lines = append(lines, styles.Danger.Render(retentionActionableError(p.retentionErr)))
	}
	if p.ingestRes != nil {
		lines = append(lines, styles.Base.Render(fmt.Sprintf(
			"local import completed: %d new, %d updated, %d unchanged, %d errors",
			p.ingestRes.New, p.ingestRes.Updated, p.ingestRes.Unchanged, p.ingestRes.Errors)))
	} else {
		lines = append(lines, styles.Muted.Render("local import was not run"))
	}
	lines = append(lines, styles.Base.Render("kickstart published nothing"), "", styles.Header.Render("next steps"))
	if p.nextStepsErr != nil {
		for _, line := range strings.Split(p.nextStepsErr.Error(), "\n") {
			lines = append(lines, styles.Danger.Render(line))
		}
	} else {
		preamble := "these useful next steps let you modify config, open the local dashboard, connect to a village, or explicitly publish later; kickstart runs none of them."
		if p.width > 0 {
			preamble = ansi.Wrap(preamble, p.width, "")
		}
		for _, line := range strings.Split(preamble, "\n") {
			lines = append(lines, styles.Base.Render(line))
		}
		lines = append(lines, "")
		for _, kind := range p.nextSteps {
			step, present := canonicalNextStep(kind)
			if !present {
				// resolveNextSteps validates the entire result before storing it;
				// this branch is unreachable unless that invariant regresses.
				lines = append(lines, styles.Danger.Render(nextStepsActionableError(
					"a previously validated action lost its canonical catalog entry").Error()))
				break
			}
			lines = append(lines,
				styles.Base.Render(step.title),
				styles.Selected.Render(step.command),
				styles.Muted.Render(step.detail))
		}
	}
	lines = append(lines, "", keymap.FooterView(p.deps.Theme, keymap.Default(),
		programActionAvailability{keymap.ActionBack, keymap.ActionQuit}))
	return strings.Join(lines, "\n")
}

func displayDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	return duration.Round(time.Second).String()
}

func promptWidth(prompt string, available int) int {
	width := len([]rune(prompt))
	if width < kit.ConfirmMinSize.Width {
		width = kit.ConfirmMinSize.Width
	}
	if available > 0 && width > available {
		width = available
	}
	return width
}

func loginActionableError(err error, where string) error {
	return fmt.Errorf(
		"village login failed.\n"+
			"what: kickstart could not authenticate this machine.\n"+
			"why: %v.\n"+
			"where: %s.\n"+
			"when: before the sharing choice and final consent.\n"+
			"means: the draft remains buffered, local data stays on this machine, and nothing was published.\n"+
			"fix: check the village endpoint and browser login, retry here, or continue locally.",
		err, where)
}

func localImportActionableError(err error) string {
	return fmt.Sprintf(
		"what: local transcript import did not complete.\n"+
			"why: %v.\n"+
			"where: kickstart local ingest after config and retention effects.\n"+
			"when: during the current local-import attempt.\n"+
			"means: the saved config and any applied retention setting remain in force; no transcript was published.\n"+
			"fix: correct the reported local problem, then press enter to retry local import only.",
		err)
}

func retentionActionableError(err error) string {
	return fmt.Sprintf(
		"claude retention change failed. what: cleanup settings were not updated. why: %v. "+
			"where: kickstart after config save. when: before local import. "+
			"means: config remains saved. fix: update claude settings manually and rerun kickstart if needed.", err)
}

// programActionAvailability lets Program consume the canonical typed keymap for
// its non-Tree phases without defining any parallel key strings.
type programActionAvailability []keymap.ActionID

func (a programActionAvailability) AvailableActions() []keymap.ActionID { return []keymap.ActionID(a) }

// centered composites content in the middle of the terminal region over a themed
// background, reusing the kit overlay's ansi-aware centering so a dialog never
// clings to the top-left corner.
func (p Program) centered(content string) string {
	return p.overlay.Push(staticLayer{content: content}).View("")
}

// staticLayer is a plain overlay layer that renders a prepared string.
type staticLayer struct{ content string }

func (l staticLayer) View() string { return l.content }

// Model adapts a Program to the bubbletea v2 tea.Model interface so it can be
// handed to tea.NewProgram. It keeps Program's concrete-typed Update (which the
// tests drive and inspect) separate from the interface-typed Update the runtime
// needs, matching the repo idiom of a thin model wrapper over a stateful core.
type Model struct {
	program Program
}

// NewModel wraps a Program as a tea.Model for mounting.
func NewModel(p Program) Model { return Model{program: p} }

// Program exposes the wrapped program (for post-run inspection at the mount).
func (m Model) Program() Program { return m.program }

// Init starts the wrapped program.
func (m Model) Init() tea.Cmd { return m.program.Init() }

// Update advances the wrapped program and returns it as a tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	np, cmd := m.program.Update(msg)
	m.program = np
	return m, cmd
}

// View renders the wrapped program in the alt-screen, matching the legacy wizard.
func (m Model) View() tea.View {
	v := tea.NewView(m.program.View())
	v.AltScreen = true
	return v
}

var _ tea.Model = Model{}

// runOnce runs a command far enough to read the single synchronous message it
// produced (the confirm modal emits its result synchronously), mirroring how the
// bubbletea runtime would consume it.
func runOnce(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}
