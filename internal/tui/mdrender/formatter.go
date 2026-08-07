package mdrender

import (
	"fmt"
	"image/color"
	"io"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
)

// chromaFormatterName is the name the peasant code formatter is registered in
// chroma's global formatter registry under. glamour selects a formatter BY NAME
// (it offers no way to hand it an implementation), so registration is the only
// seam available.
type chromaFormatterName string

// formatterName is the one registered peasant formatter.
const formatterName chromaFormatterName = "peasant"

// String implements fmt.Stringer.
func (n chromaFormatterName) String() string { return string(n) }

// formatterOnce guards the one-time registration of the formatter.
var formatterOnce sync.Once

// registerFormatter installs the peasant code formatter. It is idempotent.
//
// Why a custom formatter rather than one of chroma's built-in terminal ones:
// chroma's terminal formatters quantize every color down to the 8/256-color
// ANSI cube before emitting it. The peasant palette is a set of exact tokens
// shared with the design system, and quantizing them means highlighted code
// renders in colors that are merely NEAR the theme rather than the theme's own.
// Emitting through lipgloss keeps the token exact and leaves any downsampling
// to the one place that knows the terminal's real capability - the program's
// output writer - instead of guessing here.
func registerFormatter() {
	formatterOnce.Do(func() {
		formatters.Register(formatterName.String(), chroma.FormatterFunc(formatTokens))
	})
}

// formatTokens writes each highlighted token styled by the entry the active
// chroma style holds for it. A token the style says nothing about is written
// as-is rather than guessing at a color for it.
func formatTokens(w io.Writer, style *chroma.Style, it chroma.Iterator) error {
	for token := it(); token != chroma.EOF; token = it() {
		entry := style.Get(token.Type)
		if entry.IsZero() {
			if _, err := fmt.Fprint(w, token.Value); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprint(w, tokenStyle(entry).Render(token.Value)); err != nil {
			return err
		}
	}
	return nil
}

// tokenStyle converts one chroma style entry into a lipgloss style. The color
// comes from the entry, which was built from the peasant palette in
// codeEntries - this function never picks a color of its own.
func tokenStyle(entry chroma.StyleEntry) lipgloss.Style {
	s := lipgloss.NewStyle()
	if entry.Bold == chroma.Yes {
		s = s.Bold(true)
	}
	if entry.Italic == chroma.Yes {
		s = s.Italic(true)
	}
	if entry.Underline == chroma.Yes {
		s = s.Underline(true)
	}
	if entry.Colour.IsSet() {
		s = s.Foreground(tokenColor(entry.Colour))
	}
	return s
}

// tokenColor converts a chroma colour into the color.Color lipgloss renders.
// The value round-trips the palette token codeEntries put into the style, so no
// color originates here - which is also why this does not go through a color
// constructor that would look like a hand-picked terminal color.
func tokenColor(c chroma.Colour) color.Color {
	return color.RGBA{R: c.Red(), G: c.Green(), B: c.Blue(), A: opaqueAlpha}
}

// opaqueAlpha is a fully opaque alpha channel; palette tokens have no
// transparency.
const opaqueAlpha = 0xff
