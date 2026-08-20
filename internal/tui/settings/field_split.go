package settings

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// WithExamplePane wraps field so its control renders as the LEFT pane of a
// [kit.PreviewSplit], with a scrollable, illustrative example beside it in
// the RIGHT pane - the same split mechanism the kickstart selection tree
// binds its session preview through ([kit.NewTreeLeftPane] +
// [kit.NewPreviewSplitWithBodies]), reused here instead of a bespoke layout.
//
// It exists for a guide example too tall to render inline above field's
// control without pushing that control below the fold: the control stays
// always visible; the example scrolls in its own pane instead. example is
// typically the SAME [GuideExampleFunc] a section's Guide.Example already
// exposes - pass the identical function to both and set
// Guide.ExampleInSplitPane so the guide's own inline rendering does not draw
// the same example a second time, while Guide.Example itself stays directly
// callable (a test, or another presentation, can still call it without a
// Flow).
func WithExamplePane(field Field, example GuideExampleFunc) Field {
	return &splitField{inner: field, example: example}
}

// splitField adapts one interactive field plus one static illustrative
// example into a [kit.PreviewSplit]. Every identity, validation, and
// persistence hook (Key, Kind, Label, Description, Validate, Dirty, When)
// delegates to the wrapped field unchanged; only the live presentation hooks
// (mount, focus, size, render, and key/async dispatch) run through the split.
type splitField struct {
	inner   Field
	example GuideExampleFunc

	th     theme.Theme
	left   *fieldLeftPane
	source *exampleBodySource
	split  kit.PreviewSplit
	width  int
	height int
}

func (f *splitField) Key() string             { return f.inner.Key() }
func (f *splitField) Kind() FieldKind         { return f.inner.Kind() }
func (f *splitField) Label() string           { return f.inner.Label() }
func (f *splitField) Description() string     { return f.inner.Description() }
func (f *splitField) Validate(d *Draft) error { return f.inner.Validate(d) }
func (f *splitField) Dirty(d *Draft) bool     { return f.inner.Dirty(d) }
func (f *splitField) When(d *Draft) bool      { return f.inner.When(d) }

// setDesc satisfies [describable] so [WithDescription] may still be applied
// after wrapping (e.g. WithDescription(WithExamplePane(field, ex), desc)),
// mirroring the inner field's own description hook rather than requiring
// callers to order the two options one specific way.
func (f *splitField) setDesc(s string) {
	if d, ok := f.inner.(describable); ok {
		d.setDesc(s)
	}
}

var _ describable = (*splitField)(nil)

func (f *splitField) mount(t theme.Theme) {
	f.inner.mount(t)
	f.th = t
	f.left = &fieldLeftPane{field: f.inner, th: t}
	f.source = &exampleBodySource{th: t, example: f.example}
	f.split = kit.NewPreviewSplitWithBodies(t, f.left, f.source).WithRatio(defaultExamplePaneRatio)
}

// defaultExamplePaneRatio favors the control: an interactive choice list is
// almost always narrower than its illustrative example, so the left pane
// keeps a smaller share of the width than [kit.NewPreviewSplitWithBodies]'s
// own default.
const defaultExamplePaneRatio = 0.35

// initCmd starts none of its own work: the example is a fast, in-process
// derivation (never real I/O), so its first load is primed synchronously in
// [splitField.sync] instead of round-tripping through Bubble Tea's async
// message loop, and the wrapped field owns whatever startup work it needs.
func (f *splitField) initCmd() tea.Cmd { return f.inner.initCmd() }

func (f *splitField) focus() tea.Cmd { return f.split.Focus() }
func (f *splitField) blur()          { f.split.Blur() }

// setSize records the region a Flow step offered this field: w is applied to
// the split directly, but h is a CEILING, not the split's actual height. The
// split's real height is decided in render, once the wrapped control's
// content (which may span more or fewer rows once its help text wraps) can be
// measured against the left pane's actual width.
func (f *splitField) setSize(w, h int) {
	f.width, f.height = w, h
	f.split.SetSize(w, h)
}

func (f *splitField) availableActions() []keymap.ActionID { return f.split.PaneActions() }

// ownsViewport reports true only while the right (example) pane holds focus.
// While the left (control) pane is focused, PgUp/PgDn belong to the flow's
// own outer viewport like any other field - the control never needs its own
// paging, since [splitField.setSize] always gives it enough height to render
// fully.
func (f *splitField) ownsViewport() bool { return f.split.ActivePane() == kit.PaneRight }

var _ viewportOwningField = (*splitField)(nil)

func (f *splitField) actionKeymap() keymap.Keymap { return f.inner.actionKeymap() }

// capturesPrintableInput is always false: every field kind this seam wraps
// today (the privacy redaction-level radio) never edits free text. A future
// caller wrapping a text field would need this widened alongside it.
func (f *splitField) capturesPrintableInput() bool { return false }

// sync re-syncs the wrapped field's live component from d and (re)primes the
// example pane, so re-entering this step always shows the current example
// without a transient loading spinner.
func (f *splitField) sync(d *Draft) {
	f.inner.sync(d)
	f.rebind(d)
	f.prime()
}

func (f *splitField) handle(d *Draft, msg tea.Msg) tea.Cmd {
	f.rebind(d)
	var cmd tea.Cmd
	f.split, cmd = f.split.Update(msg)
	return cmd
}

func (f *splitField) render(d *Draft, styles theme.Styles, width int) string {
	f.rebind(d)
	f.width = width
	f.split.SetSize(width, f.contentHeight(d, styles, width))
	return f.split.View()
}

// contentHeight measures the wrapped control's natural row count at the left
// pane's actual width and clamps it to h, the ceiling the flow offered in
// setSize. The split's real height follows the CONTROL, not the example: the
// example pane inherits whatever height that leaves and scrolls for the rest,
// so a longer per-option description grows the whole split (up to the
// ceiling) instead of being clipped to compete with the example for a fixed
// share of it.
func (f *splitField) contentHeight(d *Draft, styles theme.Styles, width int) int {
	leftWidth := f.split.LeftPaneWidth(width)
	content := f.inner.render(d, styles, leftWidth)
	desired := strings.Count(content, "\n") + 1
	if desired < 1 {
		desired = 1
	}
	if f.height > 0 && desired > f.height {
		return f.height
	}
	return desired
}

func (f *splitField) reset(d *Draft) { f.inner.reset(d) }

// rebind points the left pane and the example source at the draft this call
// carries. The Draft pointer a Flow hands its fields is stable for the
// Flow's lifetime, so this is idempotent - it exists so the left pane and
// body source, which the [kit.LeftPane] and [kit.BodySource] seams give no
// Draft parameter to, always read the SAME draft the rest of the field
// machinery is applying msg to.
func (f *splitField) rebind(d *Draft) {
	f.left.draft = d
	f.source.draft = d
}

// OwnsAsync and handleAsync satisfy the private asyncField capability so a
// preview-load result or spinner tick reaches the mounted split regardless of
// which step currently has focus - the same routing every other asynchronous
// field in this package uses.
func (f *splitField) OwnsAsync(msg tea.Msg) bool { return f.split.OwnsAsync(msg) }

func (f *splitField) handleAsync(d *Draft, msg tea.Msg) tea.Cmd {
	if !f.split.OwnsAsync(msg) {
		return nil
	}
	f.rebind(d)
	var cmd tea.Cmd
	f.split, cmd = f.split.Update(msg)
	return cmd
}

var (
	_ Field      = (*splitField)(nil)
	_ asyncField = (*splitField)(nil)
)

// prime synchronously resolves the split's first preview load so the field's
// very first render already shows the example instead of a transient loading
// spinner: the example is a fast in-process derivation, not real I/O, so
// running it inline on step entry costs nothing a user could perceive.
func (f *splitField) prime() {
	for _, msg := range drainCmd(f.split.Load()) {
		f.split, _ = f.split.Update(msg)
	}
}

// drainCmd runs cmd (and, recursively, every command inside a [tea.BatchMsg]
// it returns) to completion and collects the leaf messages produced, without
// going through Bubble Tea's own event loop. It exists for exactly one seam -
// [splitField.prime] - so a fast, in-process load can resolve before the
// field's first draw.
func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, drainCmd(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// fieldLeftPane adapts a settings [Field]'s live control to [kit.LeftPane],
// binding it to one draft, so [splitField] can drive it through
// [kit.PreviewSplit] unchanged. It carries exactly one highlighted row - the
// field itself - so a [kit.PreviewSplit] mounted over it loads its preview
// exactly once per (re)bind rather than once per row, unlike a list or tree
// left pane.
type fieldLeftPane struct {
	field   Field
	draft   *Draft
	th      theme.Theme
	width   int
	height  int
	focused bool
}

// exampleLeftPaneID is the constant identity [fieldLeftPane] reports: a field
// control has exactly one row, so there is nothing for an id to distinguish.
const exampleLeftPaneID = "example"

func (p *fieldLeftPane) Focus() tea.Cmd { p.focused = true; return p.field.focus() }
func (p *fieldLeftPane) Blur()          { p.focused = false; p.field.blur() }
func (p *fieldLeftPane) Focused() bool  { return p.focused }

func (p *fieldLeftPane) SetSize(w, h int) {
	p.width, p.height = w, h
	p.field.setSize(w, h)
}

func (p *fieldLeftPane) AvailableActions() []keymap.ActionID { return p.field.availableActions() }

func (p *fieldLeftPane) Update(msg tea.Msg) (kit.LeftPane, tea.Cmd) {
	cmd := p.field.handle(p.draft, msg)
	return p, cmd
}

func (p *fieldLeftPane) View() string {
	return p.field.render(p.draft, p.th.Styles(), p.width)
}

func (p *fieldLeftPane) HighlightedID() (string, bool) { return exampleLeftPaneID, true }

var _ kit.LeftPane = (*fieldLeftPane)(nil)

// exampleBodySource is the [kit.BodySource] a [splitField]'s right pane loads
// from. It ignores id (the left pane never reports more than one), deriving
// the same typed example lines a section's Guide.Example would, then styling
// them through the shared [renderGuideExampleLines] so a split's right pane
// and a guide's inline rendering can never drift apart in presentation.
type exampleBodySource struct {
	th      theme.Theme
	draft   *Draft
	example GuideExampleFunc
}

func (s *exampleBodySource) Body(string) (kit.PreviewBody, error) {
	lines, err := s.example(s.draft)
	if err != nil {
		return nil, err
	}
	if err := validateGuideExampleLines(lines); err != nil {
		return nil, err
	}
	return exampleBody{th: s.th, lines: lines}, nil
}

var _ kit.BodySource = (*exampleBodySource)(nil)

// exampleBody is the [kit.PreviewBody] a validated example becomes. Layout
// happens in Render, at the pane's CURRENT width on every draw - never at
// load time - the same width-at-draw-time contract every other PreviewBody in
// this codebase keeps, so a terminal resize re-fits the example instead of
// clipping text laid out for a stale width.
type exampleBody struct {
	th    theme.Theme
	lines []GuideExampleLine
}

// Render assumes lines already passed [validateGuideExampleLines] in
// [exampleBodySource.Body]; [styleGuideExampleLines] therefore cannot fail
// here.
func (b exampleBody) Render(width int) string {
	return joinLines(styleGuideExampleLines(b.th, width, b.lines))
}

var _ kit.PreviewBody = exampleBody{}
