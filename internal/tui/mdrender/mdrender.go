// Package mdrender turns markdown source into styled terminal text for the
// peasant TUI: prose structure via glamour, fenced code via chroma, both
// colored from the SAME theme.Palette every kit component draws from, so a
// rendered body sits in the surrounding chrome instead of importing a second,
// unrelated palette.
//
// It is deliberately a leaf package with one narrow entry point ([Renderer.Render])
// so any surface that needs rich text - today the kickstart transcript preview,
// tomorrow a full transcript viewer - binds to the same renderer rather than
// growing its own.
//
// Three properties the callers depend on, and why they are here rather than at
// each call site:
//
//   - The style mode is PINNED to the theme.Theme it was built with. glamour
//     can auto-detect a terminal's background, but that detection queries the
//     terminal mid-render, which both races a bubbletea program's stdin and
//     makes output non-reproducible. Render never probes anything.
//   - Rendering is serialized on a package mutex, which makes Render safe to
//     call from any goroutine. goldmark (glamour's parser) and glamour's own
//     ansi block stack both carry state across the public Render API and are
//     NOT reentrant: without the mutex, two simultaneous renders corrupt each
//     other's document. The kickstart preview does not need that today - see
//     the note on where it calls from, below - but the guarantee is what lets
//     a future consumer render off the UI goroutine without re-deriving it.
//   - Failure degrades, never panics and never blanks: any glamour error
//     returns the SANITIZED plain source, which is still readable text.
//
// # Where the kickstart preview calls Render from
//
// Synchronously, at DRAW time, on the UI goroutine: the kit preview split lays
// its body out in its View and scroll paths, against the width it has right
// then. The asynchronous load that pane runs off the UI goroutine fetches the
// body as PLAIN TEXT and does not come through here.
//
// That is a deliberate trade, and it is why the two cost bounds in this package
// exist. Only the first render of a given (text, width, mode) does real work;
// every later draw of the same body is a cache hit. And that first render is
// bounded by maxQuoteDepth, so no recorded message can make a frame take
// unbounded time. Moving the first render onto a background pre-warm would lift
// that bound, and is a question for the full transcript viewer (peasant#72),
// which will render bodies far larger than one message.
package mdrender

import (
	"regexp"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// Renderer renders markdown at one pinned theme mode. It is a small value:
// build one per surface (or per render) rather than threading a pointer, since
// the expensive part - the glamour term renderer and the chroma style - is
// cached package-wide, keyed by the mode and width it was built for.
type Renderer struct {
	th theme.Theme
}

// New builds a Renderer that renders in t's mode, using t's palette for both
// the markdown styles and the code-highlighting tokens.
func New(t theme.Theme) Renderer { return Renderer{th: t} }

// minRenderWidth is the narrowest column glamour is asked to wrap to. A
// non-positive width would make glamour fall back to its own default (80),
// which would overflow whatever pane asked for the render.
const minRenderWidth = 1

// maxCachedRenders bounds the rendered-output cache. A preview pane re-renders
// the same body on every scroll and every resize settles on a handful of
// widths, so a small cache absorbs nearly all of the repeat work; past the
// bound the cache is dropped whole rather than evicted one entry at a time,
// which keeps the bookkeeping to the one thing this cache is for.
const maxCachedRenders = 64

// maxQuoteDepth is the deepest blockquote nesting the renderer will lay out
// before falling back to plain text.
//
// This is a real cost cliff, not a style rule: glamour's layout of nested
// blockquotes grows exponentially with depth (measured on this dependency:
// ~4ms at depth 10, ~57ms at 30, and no longer finishing in two minutes at
// 200). A preview body is arbitrary recorded text, and the pane renders it
// synchronously while drawing a frame, so a message carrying absurd quoting
// would peg a core and freeze the whole surface - not just its own pane. Eight
// levels is already far past anything a person writes to a coding agent, and
// past it the words are still shown - just without layout.
const maxQuoteDepth = 8

// leadingQuoteDepth returns the deepest run of blockquote markers any line of s
// opens with. Only the leading run counts, because that is the only place a
// ">" nests a block; a ">" inside a sentence is ordinary text.
func leadingQuoteDepth(s string) int {
	deepest := 0
	for _, line := range strings.Split(s, "\n") {
		depth := 0
		for _, r := range line {
			if r == '>' {
				depth++
				continue
			}
			if r == ' ' {
				continue
			}
			break
		}
		if depth > deepest {
			deepest = depth
		}
	}
	return deepest
}

// renderKey identifies one rendered result: the same source at the same width
// in the same mode always renders identically, which is exactly what makes the
// output safe to cache AND what makes a golden snapshot of it stable.
type renderKey struct {
	source string
	width  int
	mode   theme.Mode
}

var (
	// renderMu serializes glamour/goldmark, which is not reentrant, and also
	// guards the caches below - one lock, because every cache read is followed
	// by the render it is trying to avoid.
	renderMu sync.Mutex
	// rendered caches finished output by source, width, and mode.
	rendered = map[renderKey]string{}
	// termRenderers caches one glamour renderer per (mode, width); building one
	// parses a style config and registers a chroma style.
	termRenderers = map[renderKey]*glamour.TermRenderer{}
)

// Render renders markdown source into styled terminal text wrapped to width
// cells.
//
// The result is wrapped ONCE, here: glamour needs a column to lay prose out
// against, so a caller must not wrap the result again. Every returned line is at
// most width display cells wide, which is what lets a pane clip it without ever
// cutting an escape sequence in half.
//
// It never returns an error. Unrenderable source degrades to its sanitized
// plain text, because a preview pane showing plain words is useful and a
// preview pane showing an error is not.
func (r Renderer) Render(source string, width int) string {
	clean := Sanitize(source)
	if strings.TrimSpace(clean) == "" {
		return ""
	}
	if width < minRenderWidth {
		width = minRenderWidth
	}
	if leadingQuoteDepth(clean) > maxQuoteDepth {
		// See maxQuoteDepth: laying this out would cost more than the pane is
		// worth, so it degrades to the same readable plain text a render
		// failure does.
		return clean
	}
	key := renderKey{source: clean, width: width, mode: r.th.Mode}

	renderMu.Lock()
	defer renderMu.Unlock()

	if out, ok := rendered[key]; ok {
		return out
	}
	out, err := r.render(key)
	if err != nil {
		// Degrade to readable plain text rather than surfacing a rendering
		// failure into a pane whose job is to show a message.
		out = clean
	}
	if len(rendered) >= maxCachedRenders {
		rendered = map[renderKey]string{}
	}
	rendered[key] = out
	return out
}

// render does the uncached work. The caller holds renderMu, which is what makes
// the shared glamour renderer safe to reuse here.
func (r Renderer) render(key renderKey) (string, error) {
	tr, err := r.termRenderer(key)
	if err != nil {
		return "", err
	}
	out, err := tr.Render(key.source)
	if err != nil {
		return "", err
	}
	return clipToWidth(trimPadding(strings.Trim(out, "\n")), key.width), nil
}

// clipToWidth holds glamour to the column it was given.
//
// It is documented on Render, and it is what every pane relies on to clip
// output without cutting an escape sequence - but glamour does not quite keep
// it. At narrow widths its fenced-code layout overflows by a cell or two (a
// chroma token plus the block's own indent lands past the wrap column), which a
// pane then either overflows with or truncates blindly. Clipping here makes the
// promise true for every caller instead of each one re-deriving a defense.
func clipToWidth(s string, width int) string {
	if s == "" || width < minRenderWidth {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if ansi.StringWidth(line) > width {
			lines[i] = ansi.Truncate(line, width, "")
		}
	}
	return strings.Join(lines, "\n")
}

// termRenderer returns the cached glamour renderer for one mode and width,
// building it on first use.
func (r Renderer) termRenderer(key renderKey) (*glamour.TermRenderer, error) {
	if tr, ok := termRenderers[key.rendererKey()]; ok {
		return tr, nil
	}
	tr, err := glamour.NewTermRenderer(
		glamour.WithStyles(r.styleConfig()),
		glamour.WithWordWrap(key.width),
		glamour.WithChromaFormatter(string(formatterName)),
	)
	if err != nil {
		return nil, err
	}
	termRenderers[key.rendererKey()] = tr
	return tr, nil
}

// rendererKey drops the source, so every body of a given width and mode shares
// one renderer.
func (k renderKey) rendererKey() renderKey {
	k.source = ""
	return k
}

// trimPadding removes each line's trailing padding.
//
// glamour pads every block line out to the wrap column with individually-styled
// spaces. That is invisible on screen but it triples the size of the text a pane
// then has to carry, clip, and snapshot - and the pane pads its own rows anyway.
// Line WIDTH is glamour's job: it wraps prose, code, and over-long tokens alike
// to the column it was given, and TestRender_FitsTheRequestedWidth is what holds
// that dependency to it.
func trimPadding(s string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = dropEmptyStyleRuns(trimTrailingBlank(line))
	}
	return strings.Join(lines, "\n")
}

// trimTrailingBlank drops the trailing spaces of one styled line, measuring and
// cutting through the ansi helpers so the cut always lands between escape
// sequences rather than inside one.
func trimTrailingBlank(line string) string {
	visible := strings.TrimRight(ansi.Strip(line), " ")
	if visible == "" {
		return ""
	}
	return ansi.Truncate(line, ansi.StringWidth(visible), "")
}

// emptyStyleRun matches one or more style-SETTING escape sequences followed
// immediately by a reset, with no text between them.
//
// Two details are load-bearing. The run must match as a whole (not just its
// last pair), so removing it can never leave a style set with nothing to close
// it. And the run must START with a sequence carrying parameters, so a reset
// that closes real text ahead of the padding is never swallowed as the run's
// opening - which would leak that text's color across the rest of the line.
var emptyStyleRun = regexp.MustCompile("(?:\x1b\\[[0-9;]+m)+\x1b\\[0?m")

// dropEmptyStyleRuns removes styling that wraps no characters. glamour styles
// each of its padding spaces individually, so trimming that padding leaves
// behind a long tail of open-then-close pairs: invisible on screen, but they
// dominate the size of the text a pane carries and of any snapshot of it.
func dropEmptyStyleRuns(line string) string {
	return emptyStyleRun.ReplaceAllString(line, "")
}

// tabExpansion is what one tab becomes. Markdown gives a leading tab structural
// meaning (it indents a code block), so a tab is widened rather than collapsed
// to a single space: collapsing it would silently reflow indented code into
// prose, and leaving it would put a character of undefined display width into a
// pane that measures its rows in cells.
const tabExpansion = "    "

// Sanitize strips everything a recorded transcript can carry that must never
// reach a terminal: escape sequences (CSI/OSC/DCS and friends) that would
// repaint the screen or move the cursor out of the pane, and the C0/C1 control
// characters left over once those are gone. Newlines survive (markdown is
// line-structured), tabs are expanded, and carriage returns are normalized.
//
// It is exported because it is also the honest fallback: a caller that cannot
// render markdown at all can still show Sanitize's output safely.
func Sanitize(s string) string {
	if s == "" {
		return s
	}
	// ansi.Strip removes whole escape sequences; doing it first means the
	// control-character pass below never sees a sequence's payload as text.
	s = ansi.Strip(s)
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", tabExpansion).Replace(s)
	return strings.Map(dropControl, s)
}

// dropControl removes a rune that a terminal would interpret rather than print.
// Newline is the one control character a rendered body is allowed to carry.
func dropControl(r rune) rune {
	switch {
	case r == '\n':
		return r
	case r < 0x20, r == 0x7f:
		// C0 controls and DEL.
		return -1
	case r >= 0x80 && r <= 0x9f:
		// C1 controls, which some terminals honor as single-byte CSI/OSC.
		return -1
	default:
		return r
	}
}
