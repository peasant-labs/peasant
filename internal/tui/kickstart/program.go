package kickstart

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/ftue"
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
	// PhaseIngest runs the ingest/journey pipeline ONLY after a successful
	// commit, preserving the legacy ordering (save, then import).
	PhaseIngest
	// PhaseDone is the terminal stage: the program has committed and ingested,
	// or the user confirmed a no-save exit.
	PhaseDone
)

// String returns a stable lower-case name for p.
func (p Phase) String() string {
	switch p {
	case PhaseOAuth:
		return "oauth"
	case PhaseFlow:
		return "flow"
	case PhaseIngest:
		return "ingest"
	case PhaseDone:
		return "done"
	default:
		return "unknown"
	}
}

// LoginFunc performs the village OAuth login and returns the authenticated
// username. It is the existing login runner (internal/auth.Login) injected so the
// program never reaches for auth or the network itself; a test supplies a fake.
type LoginFunc func(ctx context.Context) (username string, err error)

// IngestFunc runs the ingest pipeline after the config is saved and returns its
// summary. It wraps the existing ftue ingest runner / JourneyRunner, closed over
// the freshly-committed config path so it imports exactly what was just saved.
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
	// Retention writes the Claude cleanupPeriodDays preference AFTER the config
	// save (legacy ordering). When nil, or RetentionDays<=0, it is skipped.
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

// Program is the mounted first-run onboarding: OAuth -> settings.Flow -> Ingest,
// all rendered on the kit. It is a bubbletea model. The flow is the single
// commit point; ingest runs only after that commit succeeds; a confirmed esc in
// the flow exits writing nothing and leaves ingest un-run.
type Program struct {
	deps ProgramDeps

	phase Phase

	oauth      kit.Confirm
	overlay    kit.Overlay
	spinner    kit.Spinner
	authInFlt  bool
	connected  bool
	loginErr   error
	oauthAsked bool

	flow      settings.Flow
	flowBuilt bool

	ingestInFlt  bool
	ingestRes    *ftue.IngestResult
	ingestErr    error
	retentionErr error

	width, height int
}

// NewProgram builds the kickstart program over its dependencies. The flow is not
// constructed until the OAuth step resolves, so its village-gated fields see the
// real connection result.
func NewProgram(deps ProgramDeps) Program {
	if deps.Context == nil {
		deps.Context = context.Background()
	}
	p := Program{
		deps:    deps,
		phase:   PhaseOAuth,
		oauth:   kit.NewConfirm(deps.Theme, "connect to a village now?"),
		overlay: kit.NewOverlay(deps.Theme),
		spinner: kit.NewSpinner(deps.Theme, "working"),
	}
	if deps.AlreadyConnected {
		// Already holding valid credentials: skip the connect-now step and open
		// straight into the flow with the village-gated fields shown. The flow's
		// startup command is not dropped here - the constructor cannot return a
		// tea.Cmd, so Program.Init emits flow.Init() once bubbletea starts the
		// program (see buildFlow).
		p.connected = true
		p = p.buildFlow()
	}
	return p
}

// Phase reports the current stage (primarily for tests and the mount).
func (p Program) Phase() Phase { return p.phase }

// Connected reports whether the village OAuth step authenticated.
func (p Program) Connected() bool { return p.connected }

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

// Init starts the active phase: the flow's async startup when the connect-now
// step was skipped (already connected), else the OAuth confirm's cursor.
func (p Program) Init() tea.Cmd {
	if p.flowBuilt {
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
	p.oauth.SetSize(kit.ConfirmMinSize.Width, kit.ConfirmMinSize.Height)
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
	case PhaseIngest:
		return p.updateIngest(msg)
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
			p.loginErr = m.err
			p.connected = false
		} else {
			p.connected = true
		}
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

// enterFlow builds the settings.Flow with the now-known village connection and
// enters PhaseFlow, returning the flow's async startup command (the tree scan)
// so the caller can dispatch it.
func (p Program) enterFlow() (Program, tea.Cmd) {
	p = p.buildFlow()
	return p, p.flow.Init()
}

// buildFlow constructs the settings.Flow with the now-known village connection
// and enters PhaseFlow WITHOUT dispatching its startup command. Callers that can
// dispatch a tea.Cmd use enterFlow; the constructor, which cannot return a
// command, uses buildFlow and relies on Program.Init to emit flow.Init() exactly
// once when bubbletea starts the program - so the scan still starts and no start
// command is silently dropped.
func (p Program) buildFlow() Program {
	reg := BuildRegistry(Options{
		Source:                p.deps.Source,
		VillageConnected:      p.connected,
		ClaudeSessionsPresent: p.deps.ClaudeSessionsPresent,
		Preview:               p.deps.Preview,
	})
	p.flow = settings.NewFlow(p.deps.Theme, reg, p.deps.Draft)
	p.flow.SetSize(p.width, p.height)
	p.flowBuilt = true
	p.phase = PhaseFlow
	return p
}

// updateFlow forwards to the settings.Flow. When the flow commits, ingest starts
// (legacy ordering: save THEN import); when the flow exits (confirmed esc),
// nothing was written and the program is done.
func (p Program) updateFlow(msg tea.Msg) (Program, tea.Cmd) {
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
			if err := p.deps.Retention.WriteCleanupDays(days); err != nil {
				p.retentionErr = err
			}
		}
	}
	if p.deps.Ingest == nil {
		p.phase = PhaseDone
		return p, tea.Quit
	}
	p.phase = PhaseIngest
	p.ingestInFlt = true
	p.spinner = p.spinner.SetLabel("importing transcripts")
	return p, tea.Batch(p.runIngest(), p.spinner.Tick())
}

// retentionDays reports the Claude Code cleanup period to write: the value the
// user chose in the flow's retention field, or the injected fallback when the
// field was not offered (no Claude sessions) and a caller pre-set a value.
func (p Program) retentionDays() int {
	if p.deps.Draft != nil {
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

// updateIngest advances the spinner until the pipeline reports its result.
func (p Program) updateIngest(msg tea.Msg) (Program, tea.Cmd) {
	switch m := msg.(type) {
	case ingestDoneMsg:
		p.ingestInFlt = false
		p.ingestRes = m.result
		p.ingestErr = m.err
		p.phase = PhaseDone
		return p, tea.Quit
	default:
		var cmd tea.Cmd
		p.spinner, cmd = p.spinner.Update(m)
		return p, cmd
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
	case PhaseIngest:
		return p.spinner.View()
	default:
		return ""
	}
}

// villageContext explains, in plain language, what connecting to a village does
// and why - the UAT flagged that the connect-now prompt appeared with no
// context at all. It is shown above the confirm in the centered dialog.
const villageContext = "a village is a shared commons for agent transcripts.\n" +
	"connecting links this machine to your village account so you\n" +
	"can publish your recorded sessions and pull others' - all\n" +
	"publishing stays explicit and opt-in. you can also connect\n" +
	"later at any time with `peasant login`."

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
	b.WriteString("\n")
	b.WriteString(p.oauth.View())
	return p.centered(b.String())
}

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
