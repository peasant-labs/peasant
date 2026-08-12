package mdrender

import (
	"fmt"
	"image/color"
	"sync"

	"charm.land/glamour/v2/ansi"
	glamourstyles "charm.land/glamour/v2/styles"
	"github.com/alecthomas/chroma/v2"
	chromastyles "github.com/alecthomas/chroma/v2/styles"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// chromaStyleName is the name a peasant-token code-highlighting style is
// registered in chroma's global registry under. There is one per theme mode:
// glamour hands the highlighter a style BY NAME, and a single shared name would
// let whichever mode rendered first pin the colors for the other.
type chromaStyleName string

const (
	// chromaStyleDark highlights code with the dark side of every token.
	chromaStyleDark chromaStyleName = "peasant-dark"
	// chromaStyleLight highlights code with the light side of every token.
	chromaStyleLight chromaStyleName = "peasant-light"
)

// String implements fmt.Stringer.
func (n chromaStyleName) String() string { return string(n) }

// chromaStyleFor names the registered code style for a mode.
func chromaStyleFor(mode theme.Mode) chromaStyleName {
	if mode == theme.ModeLight {
		return chromaStyleLight
	}
	return chromaStyleDark
}

// registerOnce guards the one-time registration of both code styles into
// chroma's process-global registry.
var registerOnce sync.Once

// registerChromaStyles installs both peasant code styles. It is idempotent and
// runs on the first render of either mode.
func registerChromaStyles() {
	registerOnce.Do(func() {
		for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
			name := chromaStyleFor(mode)
			if _, ok := chromastyles.Registry[name.String()]; ok {
				continue
			}
			chromastyles.Register(chroma.MustNewStyle(name.String(), codeEntries(theme.New(mode))))
		}
	})
}

// codeEntries maps chroma's token vocabulary onto peasant palette tokens. The
// grouping is the point: a language's keywords, its literals, and its comments
// each get ONE token, so highlighted code reads as three or four deliberate
// colors rather than as a rainbow that competes with the surrounding chrome.
// Amber is left out entirely - it is the app's scarce accent, reserved for
// selection and focus.
func codeEntries(t theme.Theme) chroma.StyleEntries {
	p := t.Palette
	m := t.Mode
	ink := hexOf(p.Ink.For(m))
	structure := hexOf(p.Ink2.For(m))
	quiet := hexOf(p.Ink3.For(m))
	keyword := hexOf(p.Mauve.For(m))
	identifier := hexOf(p.Teal.For(m))
	text := hexOf(p.Olive.For(m))
	number := hexOf(p.Clay.For(m))
	bad := hexOf(p.Danger.For(m))
	added := hexOf(p.AddText.For(m))
	removed := hexOf(p.DelText.For(m))
	strong := hexOf(p.InkStrong.For(m))

	return chroma.StyleEntries{
		chroma.Text:                ink,
		chroma.Error:               bad,
		chroma.Comment:             quiet,
		chroma.CommentPreproc:      quiet,
		chroma.Keyword:             keyword,
		chroma.KeywordReserved:     keyword,
		chroma.KeywordNamespace:    keyword,
		chroma.KeywordType:         keyword,
		chroma.Operator:            structure,
		chroma.Punctuation:         structure,
		chroma.Name:                ink,
		chroma.NameBuiltin:         identifier,
		chroma.NameTag:             identifier,
		chroma.NameAttribute:       identifier,
		chroma.NameClass:           identifier,
		chroma.NameConstant:        identifier,
		chroma.NameDecorator:       identifier,
		chroma.NameException:       identifier,
		chroma.NameFunction:        identifier,
		chroma.NameOther:           ink,
		chroma.Literal:             number,
		chroma.LiteralNumber:       number,
		chroma.LiteralDate:         number,
		chroma.LiteralString:       text,
		chroma.LiteralStringEscape: text,
		chroma.GenericDeleted:      removed,
		chroma.GenericInserted:     added,
		chroma.GenericEmph:         structure,
		chroma.GenericStrong:       strong,
		chroma.GenericSubheading:   strong,
	}
}

// styleConfig derives the markdown style glamour renders prose with: the
// structural decisions (heading prefixes, list indents, task glyphs) come from
// glamour's mode-matched standard style, and every COLOR is replaced with a
// peasant palette token so the body cannot drift from the rest of the TUI.
//
// The document margin is dropped: a preview pane is already inside the app's
// frame, and glamour's default two-cell margin would spend scarce width
// re-indenting content the pane has already positioned.
func (r Renderer) styleConfig() ansi.StyleConfig {
	registerChromaStyles()
	registerFormatter()

	cfg := glamourstyles.DarkStyleConfig
	if r.th.Mode == theme.ModeLight {
		cfg = glamourstyles.LightStyleConfig
	}
	p := r.th.Palette
	m := r.th.Mode

	cfg.Document.Margin = uintOf(0)
	cfg.Document.Color = hexPtr(p.Ink.For(m))
	cfg.Paragraph.Color = hexPtr(p.Ink.For(m))
	// Text is left uncolored on purpose: glamour cascades an uncolored inline
	// run from the block that encloses it, so heading text reads as a heading
	// and paragraph text as a paragraph, instead of every run being flattened
	// to one color.
	cfg.Heading.Color = hexPtr(p.InkStrong.For(m))
	cfg.H1.Color = hexPtr(p.InkStrong.For(m))
	cfg.H1.BackgroundColor = nil
	cfg.H2.Color = hexPtr(p.InkStrong.For(m))
	cfg.H3.Color = hexPtr(p.InkStrong.For(m))
	cfg.H4.Color = hexPtr(p.InkStrong.For(m))
	cfg.H5.Color = hexPtr(p.InkStrong.For(m))
	cfg.H6.Color = hexPtr(p.InkStrong.For(m))
	cfg.Strong.Color = hexPtr(p.InkStrong.For(m))
	cfg.Emph.Color = hexPtr(p.Ink2.For(m))
	cfg.BlockQuote.Color = hexPtr(p.Ink3.For(m))
	cfg.HorizontalRule.Color = hexPtr(p.Rule.For(m))
	cfg.Item.Color = hexPtr(p.Ink.For(m))
	cfg.Enumeration.Color = hexPtr(p.Ink2.For(m))
	cfg.Link.Color = hexPtr(p.Teal.For(m))
	cfg.LinkText.Color = hexPtr(p.Teal.For(m))
	cfg.Image.Color = hexPtr(p.Teal.For(m))
	cfg.ImageText.Color = hexPtr(p.Ink3.For(m))
	cfg.Code.Color = hexPtr(p.Olive.For(m))
	cfg.Code.BackgroundColor = nil
	cfg.Strikethrough.Color = hexPtr(p.Ink3.For(m))
	cfg.DefinitionTerm.Color = hexPtr(p.InkStrong.For(m))
	cfg.DefinitionDescription.Color = hexPtr(p.Ink.For(m))

	// Point the code block at the registered peasant style by NAME. glamour's
	// alternative - an inline Chroma block - registers under one fixed name
	// shared by every renderer in the process, so the second mode to render
	// would silently inherit the first mode's colors.
	cfg.CodeBlock.Chroma = nil
	cfg.CodeBlock.Theme = chromaStyleFor(r.th.Mode).String()
	cfg.CodeBlock.Margin = uintOf(0)

	return cfg
}

// hexOf renders a palette color as the "#rrggbb" text chroma and glamour parse
// their colors from. The palette is the source; nothing here picks a color.
func hexOf(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// hexPtr is hexOf as the pointer glamour's optional style fields take.
func hexPtr(c color.Color) *string {
	s := hexOf(c)
	return &s
}

// uintOf returns a pointer to n, for glamour's optional numeric fields.
func uintOf(n uint) *uint { return &n }
