package htmlpage_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/htmlpage"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/theme/gen"
)

// tokensSnapshotRelPath is the committed fairtrade tokens.json snapshot the
// terminal palette is generated from.
const tokensSnapshotRelPath = "internal/tui/theme/testdata/tokens.json"

const lightMedia = "@media (prefers-color-scheme:light){"

var (
	styleBlock = regexp.MustCompile(`(?s)<style>(.*?)</style>`)
	customProp = regexp.MustCompile(`--([a-z0-9-]+):(#[0-9a-f]{6})`)
	anyColor   = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(`)
)

// TestStylesheetColorsMatchTokenSnapshot proves the inline stylesheet's colors
// are fairtrade's: every custom property the rendered page declares, in the
// dark :root and under prefers-color-scheme: light, carries exactly the value
// of the token of the same name in the committed tokens.json snapshot.
func TestStylesheetColorsMatchTokenSnapshot(t *testing.T) {
	t.Parallel()
	snapshot, err := os.ReadFile(filepath.Join(testutil.ModuleRoot(t), filepath.FromSlash(tokensSnapshotRelPath)))
	if err != nil {
		t.Fatalf("read tokens snapshot: %v", err)
	}
	tokens, err := gen.ParseColorTokens(snapshot)
	if err != nil {
		t.Fatalf("parse tokens snapshot: %v", err)
	}
	byName := make(map[string]gen.Token, len(tokens))
	for _, token := range tokens {
		byName[token.Name] = token
	}

	css := renderedStylesheet(t, htmlpage.NotBundled)
	lightAt := strings.Index(css, lightMedia)
	if lightAt < 0 {
		t.Fatalf("stylesheet has no %q block:\n%s", lightMedia, css)
	}
	dark := css[:strings.Index(css, "}")]
	light := css[lightAt+len(lightMedia):]
	light = light[:strings.Index(light, "}")]

	assertVars := func(scheme, block string, want func(gen.Token) string) {
		t.Helper()
		vars := customProp.FindAllStringSubmatch(block, -1)
		if len(vars) == 0 {
			t.Fatalf("%s: no color custom properties in %q", scheme, block)
		}
		for _, v := range vars {
			token, ok := byName[v[1]]
			if !ok {
				t.Errorf("%s: --%s names no fairtrade color token", scheme, v[1])
				continue
			}
			if got, want := v[2], strings.ToLower(want(token)); got != want {
				t.Errorf("%s: --%s = %s, want the token's %s", scheme, v[1], got, want)
			}
		}
	}
	assertVars("dark", dark, func(token gen.Token) string { return token.Dark })
	assertVars("light", light, func(token gen.Token) string { return token.Light })

	if strings.Contains(css, "@import") {
		t.Errorf("stylesheet uses @import; fonts load through a <link>")
	}
	if colors, props := len(anyColor.FindAllString(css, -1)), len(customProp.FindAllString(css, -1)); colors != props {
		t.Errorf("stylesheet carries %d colors but only %d token custom properties; every color must come from a token:\n%s", colors, props, css)
	}
}

// TestRenderEscapesEveryPart proves the page text is escaped: a handle or a
// heading carrying markup is shown as text and never parsed.
func TestRenderEscapesEveryPart(t *testing.T) {
	t.Parallel()
	const hostile = `<script>alert("x")</script>`
	page := string(htmlpage.MustRender(htmlpage.Page{
		Title:   hostile,
		Heading: hostile,
		Lines:   []htmlpage.Line{{htmlpage.Text(hostile), htmlpage.Value(hostile), htmlpage.Code(hostile)}},
		Status:  &htmlpage.Status{Tone: htmlpage.ToneSuccess, Text: hostile},
		Note:    hostile,
		Hint:    hostile,
	}))
	if strings.Contains(page, "<script") {
		t.Fatalf("rendered page carries an unescaped <script>:\n%s", page)
	}
	if got := strings.Count(page, "&lt;script&gt;"); got != 8 {
		t.Errorf("escaped markup appears %d times, want once per field (8):\n%s", got, page)
	}
}

// TestNotBundledCarriesThePhraseTheWorkflowsSearch keeps the notice findable:
// the release workflows tell it from the real app by this exact phrase.
func TestNotBundledCarriesThePhraseTheWorkflowsSearch(t *testing.T) {
	t.Parallel()
	page := string(htmlpage.MustRender(htmlpage.NotBundled))
	if !strings.Contains(page, "is not bundled in this build") {
		t.Fatalf("not-bundled notice lost the phrase the release workflows search for:\n%s", page)
	}
}

func renderedStylesheet(t *testing.T, p htmlpage.Page) string {
	t.Helper()
	match := styleBlock.FindSubmatch(htmlpage.MustRender(p))
	if match == nil {
		t.Fatalf("rendered page has no <style> block")
	}
	return string(match[1])
}
