// Package transcriptview renders a recorded session's turns into styled
// terminal text: role-tagged blocks - you, assistant, thinking, tool - with
// prose and fenced code laid out through internal/tui/mdrender, all colored
// from the SAME theme.Palette the surrounding chrome draws from.
//
// It knows nothing about any one surface. The kickstart selection preview
// mounts it beside the session tree today, and the full transcript viewer
// (peasant#72) is meant to mount the same [Renderer] behind its own viewport;
// neither concern reaches in here. What the package owns is exactly the part
// both need: turning []ingest.Turn into text at a given width, cheaply enough
// to do on every frame.
//
// # Width belongs to the draw, not to the load
//
// A [Document] is width-INDEPENDENT: it is the turns, bound to the renderer
// that will draw them. Text appears only in [Document.Render], against the
// width the pane has right then. This is deliberate and it is the shape of a
// bug this code has already had: laying a body out when it is loaded bakes in
// whatever width happened to be current, which on mount is before the pane has
// been sized at all.
//
// # What a frame costs
//
// Render is called on every draw and every scroll, so each turn's finished text
// is cached by (turn content, width, mode) - see [Renderer]. A cold render of
// one turn goes through glamour; a warm one is a map lookup. The bounds in
// bounds.go cap what a single cold render can cost, because a recorded
// transcript is arbitrary data and the pane renders it synchronously while
// drawing a frame.
package transcriptview

import (
	"fmt"
	"hash/fnv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/mdrender"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// Renderer draws recorded turns at one pinned theme. Build ONE per surface and
// keep it: the per-turn cache it carries is the whole reason a full transcript
// can be re-rendered on every frame, and a renderer built per draw would throw
// that cache away each time.
//
// It is NOT safe for concurrent use. Rendering happens at draw time on the UI
// goroutine, and a mutex here would buy nothing that mdrender's own lock does
// not already provide for the expensive part.
type Renderer struct {
	th    theme.Theme
	md    mdrender.Renderer
	cache map[turnKey]string
}

// New builds a Renderer that draws in t's mode, using t's palette for the role
// colors, the gutters, and the code highlighting alike.
func New(t theme.Theme) *Renderer {
	return &Renderer{th: t, md: mdrender.New(t), cache: map[turnKey]string{}}
}

// Document is a loaded transcript bound to the renderer that draws it: a
// width-independent value a pane can hold across resizes and scrolls, and turn
// into text at whatever width it currently has.
//
// It satisfies the kit's structured-preview seam (a Render(width) string
// method) without importing it, so the kit stays free of any transcript
// knowledge and this package stays free of any pane knowledge.
type Document struct {
	r     *Renderer
	turns []ingest.Turn
}

// Document binds turns to r for later rendering. It copies nothing and renders
// nothing: the cost is paid at draw time, per turn, once per width.
func (r *Renderer) Document(turns []ingest.Turn) Document {
	return Document{r: r, turns: turns}
}

// TurnCount reports how many turns the document holds, INCLUDING any past
// [MaxRenderedTurns] that Render will summarize rather than draw.
func (d Document) TurnCount() int { return len(d.turns) }

// Render lays the whole transcript out for a pane of width cells. Every
// returned line is at most width display cells wide.
//
// Turns past [MaxRenderedTurns] are replaced by one line saying how many were
// left out, rather than silently dropped: a preview that quietly ends early
// reads as a session that ended early.
func (d Document) Render(width int) string {
	if d.r == nil || len(d.turns) == 0 {
		return ""
	}
	if width < minRenderWidth {
		width = minRenderWidth
	}
	shown, omitted := boundTurns(d.turns)
	blocks := make([]string, 0, len(shown)+1)
	for i := range shown {
		if block := d.r.turnBlock(shown[i], width); block != "" {
			blocks = append(blocks, block)
		}
	}
	if omitted > 0 {
		blocks = append(blocks, d.r.note(fmt.Sprintf(moreTurnsFormat, omitted), width))
	}
	return strings.Join(blocks, blockSeparator)
}

// turnKey identifies one finished turn block. The turn's IDENTITY is a
// fingerprint of everything that is drawn from it, not its index or its
// session: two loads of the same turn render identically, and a turn whose
// content changed under the same index (a truncated body replaced by the full
// one from the source overlay) is a different key rather than a stale hit.
type turnKey struct {
	fingerprint uint64
	width       int
	mode        theme.Mode
}

// turnBlock returns one turn's finished text, rendering it only on a miss.
func (r *Renderer) turnBlock(turn ingest.Turn, width int) string {
	key := turnKey{fingerprint: fingerprint(turn), width: width, mode: r.th.Mode}
	if out, ok := r.cache[key]; ok {
		return out
	}
	out := r.renderTurn(turn, width)
	if len(r.cache) >= maxCachedTurns {
		// Dropped whole rather than evicted one at a time: the cache exists to
		// absorb re-draws of ONE transcript at a handful of widths, and past the
		// bound the cheapest correct thing is to start over.
		r.cache = map[turnKey]string{}
	}
	r.cache[key] = out
	return out
}

// fingerprint hashes everything renderTurn reads off a turn. Fields are written
// length-prefixed so no two different turns can hash the same by shifting text
// across a field boundary.
func fingerprint(turn ingest.Turn) uint64 {
	h := fnv.New64a()
	writeField(h, string(KindOf(turn)))
	writeField(h, fmt.Sprintf("%d", turn.Depth))
	writeField(h, turn.Content)
	for _, call := range turn.ToolCalls {
		writeField(h, call.Name)
		writeField(h, call.Arguments)
		writeField(h, call.Result)
		writeField(h, call.FilePath)
		writeField(h, fmt.Sprintf("%t", call.IsError))
	}
	return h.Sum64()
}

// writeField writes one length-prefixed field into the hash.
func writeField(h interface{ Write([]byte) (int, error) }, s string) {
	fmt.Fprintf(h, "%d:%s", len(s), s)
}

// renderTurn does the uncached work for one turn: a role label, the turn's own
// body under a colored gutter, and one block per tool call it carries.
//
// A subagent turn is indented by its depth, so a nested agent's work reads as
// nested rather than as more of the main conversation.
func (r *Renderer) renderTurn(turn ingest.Turn, width int) string {
	kind := KindOf(turn)
	indent := depthIndent(turn.Depth)
	inner := width - lipgloss.Width(indent)
	if inner < minRenderWidth {
		// Too narrow to indent and still show anything: drop the indent rather
		// than the content.
		indent = ""
		inner = width
	}

	sections := make([]string, 0, len(turn.ToolCalls)+1)
	if body := r.turnBody(kind, turn.Content, inner); body != "" {
		sections = append(sections, r.label(kind, kind.Label(), inner)+"\n"+body)
	} else if len(turn.ToolCalls) == 0 {
		// An empty turn still says a turn happened; a blank gap does not.
		sections = append(sections, r.label(kind, kind.Label(), inner))
	}
	for _, call := range turn.ToolCalls {
		sections = append(sections, r.toolBlock(call, inner))
	}
	block := strings.Join(sections, "\n")
	if indent == "" {
		return block
	}
	return prefixLines(indent, block)
}

// turnBody lays one turn's own content out under its gutter, or returns empty
// when the turn carries none.
//
// What a person and an agent write is read as MARKDOWN: headings, lists,
// emphasis, and fenced code are how both of them actually write to each other,
// and rendering them is what puts syntax-highlighted code in the pane instead
// of a wall of backticks. Everything else - a reasoning block, a system
// message, tool output - is shown as the plain text it is, because it is
// machine-shaped and markdown syntax in it is far more likely to be literal
// than intended.
func (r *Renderer) turnBody(kind Kind, content string, width int) string {
	inner := width - gutterWidth
	if inner < minRenderWidth {
		inner = minRenderWidth
	}
	var body string
	switch kind {
	case KindUser, KindAssistant:
		body = r.md.Render(boundProse(content), inner)
	default:
		body = plainText(boundProse(content), inner)
	}
	if body == "" {
		return ""
	}
	return prefixLines(r.gutter(kind), body)
}

// toolBlock renders one tool call: a header naming the tool and what it was
// pointed at, then a BOUNDED preview of what it returned. The body is bounded
// because a single tool result can be an entire file or a whole test run, and
// the pane is a preview of a conversation rather than a log viewer.
func (r *Renderer) toolBlock(call ingest.ToolCall, width int) string {
	header := strings.TrimSpace(KindTool.Label() + " " + toolHeadline(call))
	if call.IsError {
		header += " " + toolFailedSuffix
	}
	kind := KindTool
	if call.IsError {
		kind = KindSystem
	}
	out := r.label(kind, header, width)

	inner := width - gutterWidth
	if inner < minRenderWidth {
		inner = minRenderWidth
	}
	body, omitted := boundLines(plainText(call.Result, inner))
	if body != "" {
		out += "\n" + prefixLines(r.gutter(kind), body)
	}
	if omitted > 0 {
		out += "\n" + prefixLines(r.gutter(kind), r.note(fmt.Sprintf(moreLinesFormat, omitted), inner))
	}
	return out
}

// label renders one block's lowercase chrome heading in its kind's color,
// clipped to width.
func (r *Renderer) label(kind Kind, text string, width int) string {
	return r.style(kind).Bold(true).Render(oneLine(text, width))
}

// note renders one line of the pane's own muted chrome - what was left out, and
// how much of it.
func (r *Renderer) note(text string, width int) string {
	return r.th.Styles().Muted.Render(oneLine(text, width))
}

// oneLine sanitizes text down to a single row clipped to width. A header is
// built from recorded values (a tool name, a file path), so it is sanitized
// like any other transcript text - but unlike a body it is never wrapped: a
// heading that folded onto a second row would read as content.
func oneLine(text string, width int) string {
	clean := collapseWhitespace(mdrender.Sanitize(text))
	if width < minRenderWidth {
		return clean
	}
	return ansi.Truncate(clean, width, "")
}

// gutter is the styled two-cell rail every block's body sits behind. It is what
// makes a turn's extent visible at a glance without a border, which the design
// language has no radius for anyway.
func (r *Renderer) gutter(kind Kind) string {
	return r.style(kind).Render(gutterGlyph) + " "
}

// style resolves one kind's palette token.
//
// The tokens are chosen for separation rather than meaning: the two voices of
// the conversation are the two most distinct tokens, the agent's private
// reasoning recedes into the muted ink the rest of the chrome uses, and tool
// steps take the remaining accent. Amber is deliberately NOT used - it is the
// app's scarce selection accent, and a transcript would spend it on every
// other line.
func (r *Renderer) style(kind Kind) lipgloss.Style {
	p := r.th.Palette
	var pair theme.ColorPair
	switch kind {
	case KindUser:
		pair = p.Teal
	case KindAssistant:
		pair = p.Mauve
	case KindThinking:
		pair = p.Ink3
	case KindTool:
		pair = p.Olive
	case KindSystem:
		pair = p.Danger
	default:
		pair = p.Ink3
	}
	return lipgloss.NewStyle().Foreground(r.th.Color(pair))
}

// plainText sanitizes recorded text and hard-wraps it to width. It is the
// non-markdown path: a recorded transcript can carry escape sequences that
// would repaint the screen, so nothing reaches a pane without going through
// mdrender.Sanitize first.
func plainText(s string, width int) string {
	clean := strings.TrimRight(mdrender.Sanitize(s), "\n")
	if strings.TrimSpace(clean) == "" {
		return ""
	}
	if width < minRenderWidth {
		return clean
	}
	return ansi.Wrap(clean, width, "")
}

// prefixLines puts prefix in front of every line of s.
func prefixLines(prefix, s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// depthIndent is the leading space a subagent turn is set in, capped so a
// deeply-nested chain cannot squeeze its own content to nothing.
func depthIndent(depth int) string {
	if depth <= 0 {
		return ""
	}
	if depth > maxIndentedDepth {
		depth = maxIndentedDepth
	}
	return strings.Repeat(" ", depth*depthIndentWidth)
}
