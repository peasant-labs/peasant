package kit_test

import (
	"testing"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// defaultKeymap is the one production keymap the kit dispatches against.
func defaultKeymap() keymap.Keymap { return keymap.Default() }

// spinnerTick returns a zero-value spinner tick message: its unexported id
// and tag are both zero, which passes the wrapped spinner model's "belongs to
// me" guards and advances exactly one frame - a deterministic way to prove a
// tick advances the animation without waiting on a real FPS timer.
func spinnerTick() spinner.TickMsg { return spinner.TickMsg{} }

// pressUpdateMulti drives a MultiSelect Update with one key press and returns
// the resulting model, discarding the command (the multiselect issues none).
func pressUpdateMulti(t *testing.T, m kit.MultiSelect, key string) kit.MultiSelect {
	t.Helper()
	m, _ = m.Update(keyPress(t, key))
	return m
}

// themeForName maps a fixture theme label to a real theme.Theme. An unknown
// label fails the test rather than silently defaulting - the render matrix
// only ever names the two modes theme.New supports.
func themeForName(t *testing.T, name string) theme.Theme {
	t.Helper()
	switch name {
	case "dark":
		return theme.New(theme.ModeDark)
	case "light":
		return theme.New(theme.ModeLight)
	default:
		t.Fatalf("themeForName: unknown theme %q", name)
		return theme.Theme{}
	}
}

// keyPress builds the tea.KeyPressMsg a real terminal produces for the
// keystroke labels the kit's keymap binds, mirroring the keymap package's own
// test helper. It is deliberately narrow: only the keys the interaction tests
// actually press.
func keyPress(t *testing.T, s string) tea.KeyPressMsg {
	t.Helper()
	msg, ok := parseFixtureKeyPress(s)
	if !ok {
		t.Fatalf("keyPress: no mapping for %q", s)
	}
	return msg
}

func parseFixtureKeyPress(s string) (tea.KeyPressMsg, bool) {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}, true
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}, true
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}, true
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}, true
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}, true
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}, true
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}, true
	case "a":
		return tea.KeyPressMsg{Code: 'a', Text: "a"}, true
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}, true
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}, true
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}, true
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}, true
	case "ctrl+h":
		return tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl}, true
	case "ctrl+l":
		return tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}, true
	case "ctrl+p":
		return tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}, true
	case "ctrl+o":
		return tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, true
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}, true
	case "shift+g":
		return tea.KeyPressMsg{Code: 'G', Text: "G"}, true
	case "ctrl+shift+l":
		return tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl | tea.ModShift}, true
	case "ctrl+shift+h":
		return tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl | tea.ModShift}, true
	default:
		if s == "" || !utf8.ValidString(s) {
			return tea.KeyPressMsg{}, false
		}
		cluster, _ := ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
		if cluster != s {
			return tea.KeyPressMsg{}, false
		}
		value, _ := utf8.DecodeRuneInString(cluster)
		return tea.KeyPressMsg{Code: value, Text: cluster}, true
	}
}

// runCmd executes a tea.Cmd (as the bubbletea runtime would) and returns the
// message it produced, or nil for a nil command. It does not recurse into
// batched commands; the interaction tests issue single-message commands.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// buildTreeComponent constructs a deterministic loaded Tree for the render
// matrix: a provider -> remote -> worktree -> session forest carrying a checked
// session, an unchecked session (so the worktree rolls up to Partial), and a
// deleted-worktree Conflict node, so every tri-state glyph appears. It drives
// the real load command to completion so the golden reflects the production
// render path, not a hand-set internal state.
func buildTreeComponent(t *testing.T, th theme.Theme, width, height int) string {
	t.Helper()
	roots := []*kit.TreeNode{{
		ID:    "provider-a",
		Label: "provider-a",
		Children: []*kit.TreeNode{{
			ID:    "peasant",
			Label: "peasant",
			Children: []*kit.TreeNode{
				{
					ID:    "peasant/develop",
					Label: "develop-with-a-deliberately-long-branch-name",
					Children: []*kit.TreeNode{
						{ID: "s1", Label: "ingest refactor", State: kit.Checked},
						{ID: "s2", Label: "commit detector", State: kit.Unchecked},
					},
				},
				{ID: "peasant/gone", Label: "deleted-worktree", State: kit.Conflict},
			},
		}},
	}}
	tr := kit.NewTree(th, staticSource{roots: roots})
	tr, cmd := tr.Load()
	if msg := runCmd(cmd); msg != nil {
		tr, _ = tr.Update(msg)
	}
	tr.SetSize(width, height)
	return tr.View()
}

// collectMsgs runs cmd as the runtime would and returns every leaf message it
// produces, recursing into a tea.Batch so a command that batches (as the
// PreviewSplit load does: the content load plus the spinner tick) yields both
// underlying messages. A nil command yields no messages.
func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, collectMsgs(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// fixedContentSource is a kit.ContentSource that always returns the same
// content (or error), used to build a deterministic PreviewSplit frame for the
// render matrix.
type fixedContentSource struct {
	content string
	err     error
}

func (s fixedContentSource) Content(_ string, _ int) (string, error) {
	return s.content, s.err
}

// buildComponent constructs one kit component in a fixed, deterministic state
// for the render matrix, applies the fixture size, and returns its final
// rendered frame. Every component's state here is constant so a golden diff
// can only come from a real rendering change, never test-run variance.
func buildComponent(t *testing.T, component string, th theme.Theme, width, height int) string {
	t.Helper()
	switch component {
	case "frame":
		f := kit.NewFrame(th).WithTitle("settings").WithFooter("esc back  enter confirm")
		f.SetContent("display theme: dark\nredact secrets: on\nprojects tracked: 3")
		f.SetSize(width, height)
		return f.View()
	case "overlay":
		c := kit.NewConfirm(th, "discard changes?")
		// Size the modal to fit inside the overlay region: a caller never hands
		// the compositor a child wider than the terminal, so the fixture must
		// not either (else the centered modal overflows the overlay width).
		c.SetSize(min(24, width), 2)
		o := kit.NewOverlay(th).Push(c)
		o.SetSize(width, height)
		base := "map view\nsession list\n"
		return o.View(base)
	case "confirm":
		c := kit.NewConfirm(th, "delete this session?")
		c.SetSize(width, height)
		return c.View()
	case "spinner":
		s := kit.NewSpinner(th, "scanning projects")
		s.SetSize(width, height)
		return s.View()
	case "list":
		items := []kit.ListItem{
			kit.StringItem("peasant"),
			kit.StringItem("village"),
			kit.StringItem("fairtrade"),
			kit.StringItem("schema"),
		}
		l := kit.NewList(th, items)
		l.SetSize(width, height)
		return l.View()
	case "textfield":
		f := kit.NewTextField(th, "project name")
		f.SetValue("peasant")
		f.SetSize(width, height)
		return f.View()
	case "toggle":
		tg := kit.NewToggle(th, "redact secrets", true)
		tg.SetSize(width, height)
		return tg.View()
	case "radio":
		r := kit.NewRadio(th, []string{"dark", "light"})
		r.SetSize(width, height)
		return r.View()
	case "multiselect":
		m := kit.NewMultiSelect(th, []string{"secrets", "paths", "pii", "project"})
		m = pressUpdateMulti(t, m, "space") // check the first option deterministically
		m.SetSize(width, height)
		return m.View()
	case "statusbar":
		sb := kit.NewStatusBar(th).WithStatus("3 projects").WithRight("v0.1")
		sb.SetSize(width, height)
		avail := kit.NewConfirm(th, "")
		return sb.View(defaultKeymap(), avail)
	case "tree":
		return buildTreeComponent(t, th, width, height)
	case "previewsplit":
		items := []kit.ListItem{
			kit.StringItem("peasant"),
			kit.StringItem("village"),
			kit.StringItem("fairtrade"),
			kit.StringItem("schema"),
		}
		src := fixedContentSource{content: "preview of peasant\nsecond line\nthird line"}
		ps := kit.NewPreviewSplit(th, kit.NewListLeftPane(kit.NewList(th, items)), src)
		ps.SetSize(width, height)
		// Load the highlighted item's preview synchronously so the golden
		// captures a settled (non-spinner) frame deterministically.
		for _, msg := range collectMsgs(ps.Load()) {
			ps, _ = ps.Update(msg)
		}
		return ps.View()
	default:
		t.Fatalf("buildComponent: unknown component %q", component)
		return ""
	}
}
