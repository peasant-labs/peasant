// Package htmlpage renders the few standalone HTML pages peasant serves
// outside the local app: the page a browser lands on after a login, and the
// notice a build without the bundled local app shows in its place.
//
// The pages cannot load the fairtrade npm stylesheet, so each carries a small
// inline one. Its colors are not copied by hand: they are read from the
// terminal palette generated from fairtrade's tokens.json
// (theme.GeneratedPalette), so a fairtrade bump that regenerates the palette
// restyles these pages too. Dark is the default and light follows the
// system's prefers-color-scheme, as in fairtrade.
package htmlpage

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"image/color"
	"io"
	"strings"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// Tone is the closed set of states a page's status line can carry.
type Tone string

const (
	// ToneSuccess marks a finished action, such as a completed login.
	ToneSuccess Tone = "success"
	// ToneDanger marks a failed action.
	ToneDanger Tone = "danger"
)

// PartKind is the closed set of ways a run of text inside a line is set.
type PartKind string

const (
	// PartText is plain prose.
	PartText PartKind = "text"
	// PartValue is a value set apart from the prose, such as an account
	// handle. It keeps its own case.
	PartValue PartKind = "value"
	// PartCode is a command the reader can type.
	PartCode PartKind = "code"
)

// Part is one run of text inside a Line. html/template escapes every part.
type Part struct {
	Kind PartKind
	Text string
}

// Text, Value and Code build the three kinds of Part.
func Text(s string) Part  { return Part{Kind: PartText, Text: s} }
func Value(s string) Part { return Part{Kind: PartValue, Text: s} }
func Code(s string) Part  { return Part{Kind: PartCode, Text: s} }

// Line is one paragraph of a page's body.
type Line []Part

// Status is the short state line under a page's body.
type Status struct {
	Tone Tone
	Text string
}

// Page is one standalone page: the peasant wordmark and a heading, a body of
// lines, an optional status and note, and a closing hint.
type Page struct {
	// Title is the browser tab's title.
	Title   string
	Heading string
	Lines   []Line
	Status  *Status
	// Note is an optional chip under the status, such as the test server a
	// login went to.
	Note string
	// Hint is the closing line in the page's footer.
	Hint string
}

//go:embed page.html.tmpl
var pageTemplateSource string

var pageTemplate = template.Must(template.New("page").Parse(pageTemplateSource))

// paletteVars are the fairtrade color tokens the pages paint with, each
// emitted as a CSS custom property of the same name.
var paletteVars = []struct {
	token string
	pair  theme.ColorPair
}{
	{"canvas", theme.GeneratedPalette.Canvas},
	{"surface", theme.GeneratedPalette.Surface},
	{"surface-2", theme.GeneratedPalette.Surface2},
	{"ink-strong", theme.GeneratedPalette.InkStrong},
	{"ink", theme.GeneratedPalette.Ink},
	{"ink-2", theme.GeneratedPalette.Ink2},
	{"ink-3", theme.GeneratedPalette.Ink3},
	{"rule", theme.GeneratedPalette.Rule},
	{"rule-strong", theme.GeneratedPalette.RuleStrong},
	{"amber", theme.GeneratedPalette.Amber},
	{"warning", theme.GeneratedPalette.Warning},
	{"success", theme.GeneratedPalette.Success},
	{"danger", theme.GeneratedPalette.Danger},
	{"focus-ring", theme.GeneratedPalette.FocusRing},
}

// pageRules is the stylesheet after the color custom properties. It reads
// every color from a custom property and sets radius nowhere (fairtrade's
// radius is 0); the sizes are fairtrade's spacing and type scale.
const pageRules = `
*,*::before,*::after{box-sizing:border-box}
html{-webkit-text-size-adjust:100%;text-size-adjust:100%}
body{margin:0;min-height:100vh;min-height:100dvh;display:grid;place-items:center;padding:32px 16px;background:var(--canvas);color:var(--ink);font-family:"Atkinson Hyperlegible",ui-sans-serif,system-ui,-apple-system,sans-serif;font-size:16px;line-height:1.5;letter-spacing:.03em;word-spacing:.16em;-webkit-font-smoothing:antialiased}
::selection{background:var(--amber);color:var(--canvas)}
.pg-card{width:min(27.5rem,100%);border:1px solid var(--rule-strong);background:var(--surface)}
.pg-card>header,.pg-card>footer{padding:20px 24px}
.pg-card>header{display:grid;gap:16px;border-bottom:1px solid var(--rule)}
.pg-card>footer{border-top:1px solid var(--rule)}
.pg-brand,.pg-card h1,.pg-status,.pg-note,.pg-hint,.pg-value,.pg-card code{font-family:"Atkinson Hyperlegible Mono",ui-monospace,Menlo,Consolas,monospace;letter-spacing:0;word-spacing:0}
.pg-brand{display:inline-flex;align-items:center;gap:10px;font-weight:700;font-size:16px;color:var(--ink-strong)}
.pg-brand svg{flex:none;width:18px;height:22px;color:var(--amber)}
.pg-card h1{margin:0;font-weight:700;font-size:22px;line-height:1.2;color:var(--ink-strong)}
.pg-body{display:grid;gap:20px;padding:24px}
.pg-body p{margin:0}
.pg-line{color:var(--ink-2)}
.pg-value{font-weight:700;color:var(--ink-strong);overflow-wrap:anywhere}
.pg-card code{font-size:14px;color:var(--ink);background:var(--surface-2);padding:0 4px;white-space:nowrap}
.pg-status{display:flex;align-items:center;gap:8px;font-size:14px;color:var(--ink)}
.pg-dot{flex:none;width:8px;height:8px;background:var(--ink-3)}
.pg-status[data-tone="success"] .pg-dot{background:var(--success)}
.pg-status[data-tone="danger"] .pg-dot{background:var(--danger)}
.pg-note{justify-self:start;padding:2px 8px;border:1px solid var(--warning);color:var(--warning);font-size:14px}
.pg-hint{margin:0;font-size:14px;color:var(--ink-3)}
@media (prefers-reduced-motion:no-preference){.pg-card{animation:pg-enter 200ms cubic-bezier(.23,1,.32,1) both}}
@keyframes pg-enter{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:none}}
`

// stylesheet is built once: the palette is fixed for the life of the binary.
var stylesheet = buildStylesheet()

// buildStylesheet writes the dark palette as :root custom properties, the
// light palette under prefers-color-scheme: light, then the page rules.
func buildStylesheet() string {
	var b strings.Builder
	writeVars := func(scheme string, pick func(theme.ColorPair) color.Color) {
		fmt.Fprintf(&b, ":root{color-scheme:%s", scheme)
		for _, v := range paletteVars {
			fmt.Fprintf(&b, ";--%s:%s", v.token, hexColor(pick(v.pair)))
		}
		b.WriteString("}")
	}
	writeVars("dark", func(p theme.ColorPair) color.Color { return p.Dark })
	b.WriteString("\n@media (prefers-color-scheme:light){")
	writeVars("light", func(p theme.ColorPair) color.Color { return p.Light })
	b.WriteString("}")
	b.WriteString(pageRules)
	return b.String()
}

// hexColor formats an opaque color as #rrggbb.
func hexColor(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// pageData is what the template reads: the page plus the shared stylesheet.
// The template itself loads Atkinson Hyperlegible and its mono cut through a
// <link>, the way fairtrade loads them; a remote @import is never used.
type pageData struct {
	Page
	Stylesheet template.CSS
}

// Render writes p as a complete HTML document.
func Render(w io.Writer, p Page) error {
	return pageTemplate.Execute(w, pageData{
		Page: p,
		// The stylesheet is built only from the generated palette and the
		// constant rules above, never from request data.
		Stylesheet: template.CSS(stylesheet),
	})
}

// MustRender renders p to bytes. It panics on a template error, which can only
// come from a bug in this package's own template, so it suits pages built
// once at package initialization.
func MustRender(p Page) []byte {
	var buf bytes.Buffer
	if err := Render(&buf, p); err != nil {
		panic(fmt.Sprintf("htmlpage: render %q: %v", p.Title, err))
	}
	return buf.Bytes()
}

// NotBundledPhrase is the sentence the release workflows search a served page
// for to tell the not-bundled notice from the real local app. Keep it in the
// notice word for word.
const NotBundledPhrase = "is not bundled in this build"

// NotBundled is the notice a build without the bundled local app serves in
// its place: `go install`, a nix build, or a bare `go build` that never ran
// `make web`. The release binaries bundle the app and never show it.
var NotBundled = Page{
	Title:   "local app not bundled | peasant",
	Heading: "the local app is not here",
	Lines: []Line{
		{Text("the local app " + NotBundledPhrase + ".")},
		{Text("build from source with "), Code("make build"), Text(", or install a release binary, to open it here.")},
	},
	Hint: "the command line works fully without it.",
}
