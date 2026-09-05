package kickstart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/animation"
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
//
// onURL, when the runner calls it, reports the exact login URL BEFORE the
// runner blocks on the OAuth callback — the "connecting to village" spinner
// renders it so the user always has a manual fallback even when the browser
// never opens or opens the wrong profile. The runner may call onURL zero or
// one times; a nil onURL must never be called.
type LoginFunc func(ctx context.Context, onURL func(string)) (username string, err error)

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
	// CommitGate evaluates the current selection at the receipt. Production
	// supplies the no-project gate over the scanner's complete candidate cohort.
	CommitGate settings.CommitGateEvaluator
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
	// ProgressAnimation is the optional frame set shown above local-import
	// progress. Production uses the same ingest animation as harvest; tests can
	// leave it nil when they only need text-state assertions.
	ProgressAnimation *animation.Animation
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

// loginDoneMsg carries the result of the injected LoginFunc. epoch identifies the
// attempt that produced it, so a result from an interrupted attempt is ignored.
type loginDoneMsg struct {
	username string
	err      error
	epoch    uint64
}

// loginURLMsg carries the login URL the injected LoginFunc reported via onURL,
// BEFORE the runner blocks on the OAuth callback. epoch ties it to the attempt
// that produced it, exactly like loginDoneMsg, so a URL from an interrupted or
// superseded attempt cannot appear on a later prompt.
type loginURLMsg struct {
	url   string
	epoch uint64
}

// ingestDoneMsg carries the result of the injected IngestFunc, tagged with
// the attempt generation that started it so a superseded attempt can never
// resolve into a newer one.
type ingestDoneMsg struct {
	result     *ftue.IngestResult
	err        error
	generation uint64
}

// progressTickMsg asks Program to take one non-blocking snapshot from the
// concurrent ingest progress source. generation binds the recurring timer to
// the ingest attempt that created it, so a late retry-era tick cannot start a
// second polling chain.
type progressTickMsg struct {
	at         time.Time
	generation uint64
}

const progressPollInterval = time.Second / 24

const (
	oauthPrompt      = "connect to a village now?"
	visibilityPrompt = "log in now to choose a default sharing visibility?"
	connectingLabel  = "connecting to village"
)

// errLoginCanceled is the note shown after the user cancels an in-flight login
// with esc. It is informational, not a failure: local setup can still continue.
var errLoginCanceled = errors.New(
	"village connection canceled. you can connect later with `peasant village login`.")

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
	// loginURL is the login URL reported by the in-flight attempt's onURL
	// callback, shown under the connecting spinner. It is cleared whenever an
	// attempt starts or resolves so a stale URL never survives its attempt.
	loginURL string
	// loginCancel cancels the in-flight login attempt. It is set when a login
	// starts and cleared when the attempt resolves or is interrupted, so a key
	// press on the spinner can abort a login the runner would otherwise block on.
	loginCancel context.CancelFunc
	// loginEpoch tags each login attempt. An interrupted attempt keeps its epoch,
	// so its late result is recognized as stale and cannot clobber a resumed
	// prompt or a newer attempt.
	loginEpoch uint64
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
	ingestCtx          context.Context
	ingestCancel       context.CancelFunc
	attemptStarted     time.Time
	stageObservations  map[ingest.Stage]stageObservation
	progressAnimFrame  int
	lastProgressAnimAt time.Time
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

// ConfirmingNoProjects reports whether the dedicated empty-selection save
// confirmation is open.
func (p Program) ConfirmingNoProjects() bool {
	return p.flowBuilt && p.flow.ConfirmingNoProjects()
}

// OnReceipt reports whether the settings flow is on review and save.
func (p Program) OnReceipt() bool { return p.flowBuilt && p.flow.OnReceipt() }

// SetSize records the render region and propagates it to the active surface.
func (p *Program) SetSize(width, height int) {
	p.width, p.height = width, height
	p.oauth.SetSize(wrapPromptWidth(width), kit.ConfirmMinSize.Height)
	p.visibility.SetSize(wrapPromptWidth(width), kit.ConfirmMinSize.Height)
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
	case loginURLMsg:
		if p.staleLoginURL(m) {
			return p, nil
		}
		p.loginURL = m.url
		return p, nil
	case loginDoneMsg:
		if p.staleLogin(m) {
			return p, nil
		}
		p = p.clearLogin()
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
			return p.interruptLogin(m)
		}
		var cmd tea.Cmd
		p.oauth, cmd = p.oauth.Update(m)
		if cmd != nil {
			if res, ok := runOnce(cmd).(kit.ConfirmResultMsg); ok {
				if res.OK && p.deps.Login != nil {
					return p.beginLogin()
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

// beginLogin starts an interruptible login attempt shared by both the connect
// step and the sharing-visibility detour. It derives a cancellable context so a
// key press on the spinner can abort a login the runner would otherwise block on,
// tags the attempt with a fresh epoch, and returns the runner plus the spinner
// tick.
func (p Program) beginLogin() (Program, tea.Cmd) {
	p.loginErr = nil
	p.loginURL = ""
	p.authInFlt = true
	p.spinner = p.spinner.SetLabel(connectingLabel)
	ctx, cancel := context.WithCancel(p.deps.Context)
	p.loginCancel = cancel
	p.loginEpoch++
	urlCh := make(chan string, 1)
	return p, tea.Batch(p.runLogin(ctx, p.loginEpoch, urlCh), p.waitForLoginURL(urlCh, p.loginEpoch), p.spinner.Tick())
}

// runLogin issues the injected login runner for one attempt. The attempt's epoch
// travels with its result so an interrupted attempt's late result is ignored.
// onURL forwards the runner's pre-callback URL report onto urlCh, non-blocking
// so a runner that never reports a URL (or a fake with no onURL support) cannot
// stall the login itself.
func (p Program) runLogin(ctx context.Context, epoch uint64, urlCh chan<- string) tea.Cmd {
	login := p.deps.Login
	onURL := func(url string) {
		select {
		case urlCh <- url:
		default:
		}
	}
	return func() tea.Msg {
		// Closing urlCh after the runner returns guarantees waitForLoginURL's
		// receive unblocks even when the runner never calls onURL (a fake without
		// URL support, or a real attempt that fails before building one) — without
		// this, that command's goroutine would block on the channel forever.
		defer close(urlCh)
		username, err := login(ctx, onURL)
		return loginDoneMsg{username: username, err: err, epoch: epoch}
	}
}

// waitForLoginURL returns a command that resolves once the in-flight runner
// reports its login URL on urlCh (see runLogin's onURL), tagging the result
// with the attempt's epoch so a stale delivery is ignored exactly like
// loginDoneMsg. runLogin closes urlCh once the attempt returns; if no URL was
// ever sent, the receive yields the zero value and this command emits no
// message (bubbletea drops a nil tea.Msg).
func (p Program) waitForLoginURL(urlCh <-chan string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		url, ok := <-urlCh
		if !ok {
			return nil
		}
		return loginURLMsg{url: url, epoch: epoch}
	}
}

// clearLogin releases the volatile login state once an attempt resolves or is
// interrupted. It always cancels the attempt's context so it is not leaked, and
// clears the in-flight flag so the spinner gives way to a usable surface.
func (p Program) clearLogin() Program {
	if p.loginCancel != nil {
		p.loginCancel()
		p.loginCancel = nil
	}
	p.authInFlt = false
	p.loginURL = ""
	return p
}

// staleLogin reports whether a login result belongs to an attempt that is no
// longer current: a superseded epoch, or one already cleared by an interrupt.
func (p Program) staleLogin(m loginDoneMsg) bool {
	return !p.authInFlt || m.epoch != p.loginEpoch
}

// staleLoginURL is staleLogin's twin for loginURLMsg: it discards a URL report
// from an attempt that is no longer the current in-flight one, so a canceled
// or superseded attempt's URL can never appear under a later prompt.
func (p Program) staleLoginURL(m loginURLMsg) bool {
	return !p.authInFlt || m.epoch != p.loginEpoch
}

// interruptLogin lets the user escape the "connecting to village" spinner. ctrl+c
// (or q) cancels the attempt and quits kickstart; esc cancels the attempt and
// returns to the prompt so the user can continue locally. Any other key is
// ignored while the login runs.
func (p Program) interruptLogin(msg tea.KeyPressMsg) (Program, tea.Cmd) {
	action, ok := keymap.Match(keymap.Default(), msg,
		programActionAvailability{keymap.ActionQuit, keymap.ActionBack})
	if !ok {
		return p, nil
	}
	switch action {
	case keymap.ActionQuit:
		p = p.clearLogin()
		return p, tea.Quit
	case keymap.ActionBack:
		p = p.clearLogin()
		p.loginErr = errLoginCanceled
		return p, nil
	default:
		return p, nil
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
	flowOptions := []settings.FlowOption{settings.WithConsentSummary(p.consentSummary)}
	if p.deps.CommitGate != nil {
		flowOptions = append(flowOptions, settings.WithCommitGate(p.deps.CommitGate))
	}
	p.flow = settings.NewFlow(p.deps.Theme, reg, p.deps.Draft, flowOptions...)
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
	if ctx.HasVisibleField(SectionPublication, FieldPublication) {
		if cfg.Push.SharePreference == config.SharePreferenceShareLater {
			values = append(values, "publication preference: plan to publish later with an explicit `peasant village push`")
		} else {
			values = append(values, "publication preference: keep local; nothing is published now or when kickstart finishes")
		}
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
			p.visibility.SetSize(wrapPromptWidth(p.width), kit.ConfirmMinSize.Height)
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
	case loginURLMsg:
		if p.staleLoginURL(m) {
			return p, nil
		}
		p.loginURL = m.url
		return p, nil
	case loginDoneMsg:
		if p.staleLogin(m) {
			return p, nil
		}
		p = p.clearLogin()
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
			return p.interruptLogin(m)
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
		return p.beginLogin()
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
	// Each attempt owns a cancellable context, mirroring beginLogin, so an
	// interrupt key can stop an ingest the runner would otherwise block on.
	ctx, cancel := context.WithCancel(p.deps.Context)
	p.ingestCtx, p.ingestCancel = ctx, cancel
	p.progressAnimFrame = 0
	p.lastProgressAnimAt = time.Time{}
	if p.deps.Progress != nil {
		p.deps.Progress.Reset()
	}
	// A fresh spinner carries a fresh bubbles generation tag. Late animation
	// messages from an earlier failed attempt are therefore ignored rather than
	// extending a second animation chain into the retry.
	p.spinner = kit.NewSpinner(p.deps.Theme, "importing transcripts")
	p.spinner.SetSize(p.width, p.height)
	return p, tea.Batch(p.runIngest(generation), p.progressTick(generation), p.spinner.Tick())
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

// runIngest issues the injected ingest runner as a command on the current
// attempt's context, tagging the result with the attempt generation so a
// superseded attempt can never resolve into a newer one. A nil attempt
// context falls back to the shared deps context so runners started outside
// startIngest keep working.
func (p Program) runIngest(generation uint64) tea.Cmd {
	ingest := p.deps.Ingest
	ctx := p.ingestCtx
	if ctx == nil {
		ctx = p.deps.Context
	}
	return func() tea.Msg {
		res, err := ingest(ctx)
		return ingestDoneMsg{result: res, err: err, generation: generation}
	}
}

func (p Program) progressTick(generation uint64) tea.Cmd {
	return p.deps.Tick(progressPollInterval, func(at time.Time) tea.Msg {
		return progressTickMsg{at: at, generation: generation}
	})
}

// updateIngest advances the spinner until the pipeline reports its result.
// ctrl+c (or q) cancels the attempt and quits; every other key only feeds the
// spinner while the import runs.
func (p Program) updateIngest(msg tea.Msg) (Program, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyPressMsg:
		return p.interruptIngest(m)
	case ingestDoneMsg:
		if m.generation != p.ingestGeneration {
			return p, nil
		}
		p = p.clearIngestAttempt()
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
		p = p.advanceProgressAnimation(m.at)
		p = p.observeProgress(m.at)
		return p, p.progressTick(m.generation)
	default:
		var cmd tea.Cmd
		p.spinner, cmd = p.spinner.Update(m)
		return p, cmd
	}
}

// interruptIngest lets the user escape the local-import screen. ctrl+c (or q)
// cancels the in-flight ingest attempt and quits kickstart; the already-saved
// config and retention effect are untouched. Any other key is ignored while
// the import runs.
func (p Program) interruptIngest(msg tea.KeyPressMsg) (Program, tea.Cmd) {
	action, ok := keymap.Match(keymap.Default(), msg,
		programActionAvailability{keymap.ActionQuit})
	if !ok {
		return p, nil
	}
	switch action {
	case keymap.ActionQuit:
		p = p.clearIngestAttempt()
		// Invalidate the attempt so a late tick or result still in flight
		// cannot land after the quit.
		p.ingestGeneration++
		return p, tea.Quit
	default:
		return p, nil
	}
}

// clearIngestAttempt releases the in-flight ingest context so a resolved or
// interrupted attempt cannot leak its goroutine, mirroring clearLogin.
func (p Program) clearIngestAttempt() Program {
	if p.ingestCancel != nil {
		p.ingestCancel()
		p.ingestCancel = nil
	}
	p.ingestCtx = nil
	return p
}

func (p Program) advanceProgressAnimation(at time.Time) Program {
	anim := p.deps.ProgressAnimation
	if anim == nil || len(anim.Frames) == 0 {
		return p
	}
	interval := anim.Interval
	if interval <= 0 {
		interval = 300 * time.Millisecond
	}
	if p.lastProgressAnimAt.IsZero() || at.Sub(p.lastProgressAnimAt) >= interval {
		p.progressAnimFrame = (p.progressAnimFrame + 1) % len(anim.Frames)
		p.lastProgressAnimAt = at
	}
	return p
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
// with monotonic forward progress, for a non-error stage on a first attempt.
// Totals that move re-anchor the stability baseline without touching the
// display clock. Only the focused stage computes an estimate — it is the only
// one ever displayed — so concurrent stages no longer suppress each other.
// The estimate divides the remaining work by the recent windowed rate, never
// by one tick's instantaneous flash (which collapses on a bursty batch) nor
// by the lifetime average (which a fast start anchors forever). Every
// unsupported state remains explicit rather than guessed.
func (p Program) observeProgress(at time.Time) Program {
	if at.IsZero() {
		at = p.deps.Clock.Now()
	}
	if p.deps.Progress == nil {
		return p
	}
	snapshot := p.deps.Progress.Snapshot()
	focus, hasFocus := p.progressFocusStage()
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
				estimator:        kit.NewEstimator(estimateWindow),
			}
			observation.estimator.Estimate(at, sp.Done, sp.Total)
			p.stageObservations[stage] = observation
			continue
		}
		if observation.progress.Ended {
			// Terminal state is immutable: keep the first ended snapshot so
			// the elapsed clock stops at completion instead of tracking the
			// wall clock on every later tick.
			continue
		}

		observation.estimateValid = false
		if sp.Total != observation.lastTotal {
			// The total moved (growth, shrinkage, or unknown-to-known):
			// re-anchor the stability baseline and wait for the next stable
			// reading. The display clock (startedAt) is untouched, so stage
			// times never jump when discovery revises a total.
			observation.lastAt = at
			observation.lastDone = sp.Done
			observation.lastTotal = sp.Total
			observation.progress = sp
			observation.estimateEligible = sp.Total > 0 && !sp.HasErr && !p.retryAttempt
			observation.estimator.Estimate(at, sp.Done, sp.Total)
			p.stageObservations[stage] = observation
			continue
		}
		if sp.Total <= 0 || sp.HasErr || p.retryAttempt || sp.Done < observation.lastDone {
			observation.estimateEligible = false
		}
		eta, etaOK := observation.estimator.Estimate(at, sp.Done, sp.Total)
		if observation.estimateEligible && hasFocus && stage == focus && !sp.Ended && sp.Done < sp.Total {
			if etaOK {
				observation.estimate = eta
				observation.estimateValid = true
			}
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
			return p.viewConnecting()
		}
		return p.viewOAuth()
	case PhaseFlow:
		return p.flow.View()
	case PhaseVisibility:
		if p.authInFlt {
			return p.viewConnecting()
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

// villageContextBullets are the key facts about connecting to a village, one
// short Simplified Technical English (ASD-STE100) sentence each - the UAT
// flagged that the connect-now prompt appeared with no context at all. They
// are shown as a bulleted list above the confirm in the centered dialog, and
// wrapped to the terminal width rather than hard-wrapped in the source.
var villageContextBullets = []string{
	"a village is a shared commons for agent transcripts.",
	"local mode keeps all data on this machine.",
	"connecting sends nothing until a later explicit publish.",
	"publishing is separate. it is explicit and opt-in.",
	"you can connect later with `peasant village login`.",
}

// visibilityContextBullets are the key facts shown above the visibility
// confirm, in the same short-sentence bulleted style as villageContextBullets.
var visibilityContextBullets = []string{
	"logging in now reveals a default sharing visibility for a later publish.",
	"login only authenticates.",
	"login does not save your draft.",
	"login does not publish anything.",
}

// promptContentMaxWidth caps how wide a wrapped prompt paragraph grows in the
// centered dialog, so bullets stay readable on very wide terminals instead of
// stretching edge to edge.
const promptContentMaxWidth = 64

// wrapPromptWidth picks the wrap width for a centered dialog paragraph: the
// available terminal width, capped at promptContentMaxWidth, or 0 (no
// wrapping) when the width is not yet known.
func wrapPromptWidth(available int) int {
	if available <= 0 {
		return 0
	}
	if available > promptContentMaxWidth {
		return promptContentMaxWidth
	}
	return available
}

// bulletPrefix marks a fact's first line; bulletHangIndent is the same width
// so a wrapped continuation lines up under the fact text instead of the
// dash, matching how a bulleted list hangs in prose.
const (
	bulletPrefix     = "- "
	bulletHangIndent = "  "
)

// appendBullets word-wraps each fact to width (0 means unwrapped) and adds it
// to the dialog panel as a "- " bulleted line, so the source stays a list of
// short sentences instead of a hard-wrapped prose block. A wrapped
// continuation is hang-indented under the fact text rather than restarting at
// column 0. The panel then fits every line to one shared width, so each row of
// the dialog - not only the glyphs on it - shares one solid background.
func appendBullets(panel *kit.Panel, style lipgloss.Style, facts []string, width int) {
	for _, fact := range facts {
		text := fact
		if width > 0 {
			contentWidth := width - len(bulletPrefix)
			if contentWidth < 1 {
				contentWidth = 1
			}
			text = ansi.Wrap(fact, contentWidth, "")
		}
		for i, ln := range strings.Split(text, "\n") {
			prefix := bulletPrefix
			if i > 0 {
				prefix = bulletHangIndent
			}
			panel.Line(style, prefix+ln)
		}
	}
}

// viewConnecting renders the "connecting to village" spinner with the keys that
// escape it. The login runner can block (an interactive device flow, a slow
// network), so the surface must always advertise a way out: esc cancels and
// returns to the prompt, ctrl+c quits kickstart.
//
// The login URL is the one thing on this screen the user MUST be able to
// read in full (it is the manual fallback when no browser opens), so it is
// deliberately NOT rendered with styles.Selected: that bundle is Ink-on-
// AmberFill, a full-cell background fill meant for a highlighted row, and on
// a long URL that fill reads as a full-width amber bar that still clips at
// the terminal edge. Amber is a scarce accent (see internal/tui/mdrender and
// internal/tui/transcriptview's own "amber is deliberately NOT used" notes
// for the same discipline elsewhere), so the URL instead gets the amber
// token as plain FOREGROUND text - restrained, not a fill - and is wrapped to
// the terminal width so every character stays on screen instead of clipping.
//
// The whole dialog is one centered [kit.Panel]. The panel measures its widest
// line, fits every line to that one width, and paints the Canvas token behind
// every cell, so the block reads as a single uniform card instead of a
// staircase of per-line boxes. A bare foreground-only theme role would leave
// its padding on the terminal's own background, so the panel repaints each
// role through [kit.Panel.Style]. p.centered then places the finished block in
// the middle of the terminal, same as every other kickstart dialog.
func (p Program) viewConnecting() string {
	styles := p.deps.Theme.Styles()
	panel := kit.NewPanel(p.deps.Theme).WithAlign(kit.PanelAlignCenter)
	accent := styles.Base.Foreground(p.deps.Theme.Color(p.deps.Theme.Palette.Amber))

	panel.Rendered(p.spinner.View())
	panel.Blank()
	if p.loginURL != "" {
		url := p.loginURL
		if p.width > 0 {
			url = ansi.Wrap(url, p.width, "")
		}
		panel.Line(styles.Muted, "opening your browser. can't see it? open this link:")
		for _, ln := range strings.Split(url, "\n") {
			panel.Line(accent, ln)
		}
		panel.Blank()
	}
	panel.Line(styles.Muted, "press esc to cancel, ctrl+c to quit")
	return p.centered(panel.View())
}

// dialogPanel builds the shared centered dialog panel the connect and
// visibility prompts render into. Header, Muted, and Danger are shared,
// backgroundless roles used all over the TUI, so the panel repaints them onto
// its own background instead of any surface patching a background by hand.
func (p Program) dialogPanel() kit.Panel {
	return kit.NewPanel(p.deps.Theme)
}

// joinDialog stacks the dialog's text panel above its confirm control. The
// confirm keeps its own View, so the centered placement below still centers
// the short yes/no row inside the dialog, as it did before the panel owned the
// text block.
func joinDialog(panel, control string) string {
	return panel + "\n" + control
}

// viewOAuth renders the connect-now dialog CENTERED in the terminal (the UAT
// flagged the top-left anchoring as strange), with the explanatory context above
// the yes/no confirm, over a fully themed background. The heading names the
// topic once ("before you connect"); the confirm below it asks the actual
// yes/no question ("connect to a village now?"), so the phrase "connect to a
// village" is not repeated back to back.
func (p Program) viewOAuth() string {
	styles := p.deps.Theme.Styles()
	width := wrapPromptWidth(p.width)
	panel := p.dialogPanel()
	panel.SetSize(width, 0)
	panel.Line(styles.Header, "before you connect")
	panel.Line(styles.Muted, "")
	appendBullets(&panel, styles.Muted, villageContextBullets, width)
	if p.loginErr != nil {
		panel.Line(styles.Muted, "")
		for _, ln := range strings.Split(p.loginErr.Error(), "\n") {
			panel.Line(styles.Danger, ln)
		}
	}
	panel.Line(styles.Muted, "")
	return p.centered(joinDialog(panel.View(), p.oauth.View()))
}

func (p Program) viewVisibility() string {
	styles := p.deps.Theme.Styles()
	width := wrapPromptWidth(p.width)
	panel := p.dialogPanel()
	panel.SetSize(width, 0)
	panel.Line(styles.Header, "choose sharing visibility")
	panel.Line(styles.Muted, "")
	appendBullets(&panel, styles.Muted, visibilityContextBullets, width)
	if p.loginErr != nil {
		panel.Line(styles.Muted, "")
		panel.Line(styles.Muted, "")
		for _, line := range strings.Split(p.loginErr.Error(), "\n") {
			panel.Line(styles.Danger, line)
		}
	}
	panel.Line(styles.Muted, "")
	return p.centered(joinDialog(panel.View(), p.visibility.View()))
}

// viewIngest renders the local import progress screen. The whole screen is one
// [kit.Panel], so the spinner row, the heading, and every progress row share
// one width and one background. Before the panel owned the rule, each row
// painted only as many cells as its own text, and the block showed a ragged
// edge.
func (p Program) viewIngest() string {
	styles := p.deps.Theme.Styles()
	panel := kit.NewPanel(p.deps.Theme)
	panel.SetSize(p.width, p.height)
	lines := []string{
		p.spinner.View(),
	}
	if anim := p.deps.ProgressAnimation; anim != nil && len(anim.Frames) > 0 {
		lines = append(lines, "")
		frame := anim.Frames[p.progressAnimFrame%len(anim.Frames)]
		for _, line := range frame {
			lines = append(lines, styles.Muted.Render(line))
		}
	}
	lines = append(lines, "", styles.Header.Render("local import progress"))
	// The footer hint owns two lines of the progress height budget, and is
	// pinned after the height cut, so a short terminal never removes the
	// only escape affordance.
	lines = append(lines, p.progressLines(styles, p.deps.Clock.Now(), len(lines)+2)...)
	footer := []string{"", styles.Muted.Render("ctrl+c to quit")}
	if p.height == 1 {
		// One row holds the hint, not its blank separator.
		footer = footer[1:]
	}
	if p.height > 0 && len(lines)+len(footer) > p.height {
		lines = lines[:max(p.height-len(footer), 0)]
	}
	lines = append(lines, footer...)
	for _, line := range lines {
		panel.Rendered(line)
	}
	return panel.View()
}

func (p Program) progressLines(styles theme.Styles, now time.Time, reservedLines int) []string {
	if now.Before(p.attemptStarted) {
		now = p.attemptStarted
	}
	focus, hasFocus := p.progressFocusStage()
	// One row per pipeline stage, upfront, started or not, so the full scope
	// of the import is visible before any stage begins — the same upfront
	// matrix the harvest renderer shows. Unobserved stages render from the
	// zero progress: not-started icon, empty bar, no count, no duration.
	// Observed stages carry their elapsed duration right of the counts, in a
	// column the matrix aligns across all rows.
	rows := make([]kit.ProgressRow, 0, len(ingest.StageOrder))
	focusIdx := -1
	for _, stage := range ingest.StageOrder {
		sp := ingest.StageProgress{}
		row := kit.ProgressRow{Label: strings.ToLower(stage.String())}
		if observation, ok := p.stageObservations[stage]; ok && observation.progress.Started {
			sp = observation.progress
			end := now
			if sp.Ended && observation.lastAt.Before(end) {
				end = observation.lastAt
			}
			row.Elapsed = displayDuration(end.Sub(observation.startedAt))
		}
		row.Done, row.Total, row.Ended, row.HasErr = sp.Done, sp.Total, sp.Ended, sp.HasErr
		if hasFocus && stage == focus {
			focusIdx = len(rows)
		}
		rows = append(rows, row)
	}
	// The rows match the harvest TTY bars exactly; only the stage names stay
	// lowercase per the lowercase-chrome rule. The duration paints muted,
	// like the trailing roll-up timings.
	lines := make([]string, 0, len(rows))
	for _, line := range kit.ProgressMatrix(rows) {
		rendered := styles.Base.Render(line.Bar)
		if line.Elapsed != "" {
			rendered += styles.Muted.Render("  " + line.Elapsed)
		}
		lines = append(lines, rendered)
	}
	// Trailing roll-up below the whole matrix: the whole-run total always,
	// plus the focused stage estimate, unavailable before anything starts.
	detail := []string{
		styles.Muted.Render("  total elapsed: " + displayDuration(now.Sub(p.attemptStarted))),
		styles.Muted.Render("  estimate unavailable"),
	}
	if hasFocus {
		if observation := p.stageObservations[focus]; observation.estimateValid {
			detail[1] = styles.Muted.Render("  estimate: " + displayDuration(observation.estimate))
		}
	}
	lines = append(lines, detail...)
	available := p.height - reservedLines
	if p.height > 0 && len(lines) > available {
		if available <= 0 {
			return nil
		}
		// Degenerate terminal: select rows newest-first, always keeping the
		// focus row, and keep the trailing roll-up only when it still fits.
		// The roll-up is trimmed before its row, so detail never survives
		// without the stage it describes.
		selected := map[int]bool{}
		used := 0
		if focusIdx >= 0 && used+1 <= available {
			selected[focusIdx] = true
			used++
		}
		useDetail := used+len(detail) <= available
		if useDetail {
			used += len(detail)
		}
		for ri := len(rows) - 1; ri >= 0; ri-- {
			if selected[ri] {
				continue
			}
			if used+1 > available {
				break
			}
			selected[ri] = true
			used++
		}
		var window []string
		for ri := range rows {
			if !selected[ri] {
				continue
			}
			window = append(window, lines[ri])
		}
		if useDetail {
			window = append(window, detail...)
		}
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
