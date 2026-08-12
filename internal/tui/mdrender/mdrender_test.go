package mdrender_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"image/color"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/mdrender"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/render.yaml
var renderData []byte

// coloredRun is one span of output text that must carry a NAMED palette
// token's color. Naming the token (rather than a hex value) is what keeps the
// expectation readable and keeps the fixture from silently agreeing with
// whatever the code happens to emit.
type coloredRun struct {
	Text  string `yaml:"text"`
	Token string `yaml:"token"`
}

// renderCase is one source rendered at one width in one theme mode.
type renderCase struct {
	Name        string       `yaml:"name"`
	Theme       string       `yaml:"theme"`
	Width       int          `yaml:"width"`
	Source      string       `yaml:"source"`
	WantVisible []string     `yaml:"wantVisible"`
	WantMissing []string     `yaml:"wantMissing"`
	WantColored []coloredRun `yaml:"wantColored"`
	WantPlain   bool         `yaml:"wantPlain"`
}

// renderDoc is the whole fixture plus its row-count and closed-set guards.
type renderDoc struct {
	ConcurrencyWidth       int          `yaml:"concurrencyWidth"`
	ExpectedCaseCount      int          `yaml:"expectedCaseCount"`
	ExpectedTokenNameCount int          `yaml:"expectedTokenNameCount"`
	TokenNames             []string     `yaml:"tokenNames"`
	Cases                  []renderCase `yaml:"cases"`
}

func loadRenderDoc(t *testing.T) renderDoc {
	t.Helper()
	var doc renderDoc
	dec := yaml.NewDecoder(bytes.NewReader(renderData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/render.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("render.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", doc.ExpectedCaseCount, len(doc.Cases))
	}
	if doc.ConcurrencyWidth < len(concurrentMarker(0)) {
		t.Fatalf("concurrencyWidth=%d is too narrow to hold a %d-cell marker on one line; the contamination check would read a marker broken across rows",
			doc.ConcurrencyWidth, len(concurrentMarker(0)))
	}
	if doc.ExpectedTokenNameCount != len(doc.TokenNames) || len(doc.TokenNames) == 0 {
		t.Fatalf("expectedTokenNameCount=%d but %d token names listed", doc.ExpectedTokenNameCount, len(doc.TokenNames))
	}
	declared := map[string]bool{}
	for _, name := range doc.TokenNames {
		// Resolving here proves the declared set itself is real, so a typo in
		// the list cannot make a case's token silently unassertable.
		paletteColorFor(t, name, theme.New(theme.ModeDark))
		declared[name] = true
	}
	names := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("render case name %q is missing or duplicated", c.Name)
		}
		names[c.Name] = true
		if c.Width <= 0 {
			t.Fatalf("render case %q declares width %d; a non-positive width renders nothing", c.Name, c.Width)
		}
		if c.Source == "" {
			t.Fatalf("render case %q declares no source", c.Name)
		}
		if len(c.WantVisible)+len(c.WantMissing)+len(c.WantColored) == 0 {
			t.Fatalf("render case %q asserts nothing; an empty want list is a guaranteed pass", c.Name)
		}
		for _, run := range c.WantColored {
			if !declared[run.Token] {
				t.Fatalf("render case %q names palette token %q, which is not in tokenNames", c.Name, run.Token)
			}
			if run.Text == "" {
				t.Fatalf("render case %q declares a colored run with no text; an empty needle always matches", c.Name)
			}
		}
		themeFor(t, c.Theme)
	}
	return doc
}

// themeFor resolves a fixture's theme name, failing closed on an unknown one.
func themeFor(t *testing.T, name string) theme.Theme {
	t.Helper()
	mode := theme.Mode(name)
	if !mode.IsValid() {
		t.Fatalf("render fixture names theme %q, which is neither %q nor %q", name, theme.ModeDark, theme.ModeLight)
	}
	return theme.New(mode)
}

// paletteColorFor resolves a fixture's palette token name against the SAME
// theme.Palette the renderer draws from, failing closed on an unknown name so
// a typo can never turn into an assertion that quietly matches nothing.
func paletteColorFor(t *testing.T, name string, th theme.Theme) color.Color {
	t.Helper()
	p := th.Palette
	var pair theme.ColorPair
	switch name {
	case "ink":
		pair = p.Ink
	case "ink-strong":
		pair = p.InkStrong
	case "ink-3":
		pair = p.Ink3
	case "mauve":
		pair = p.Mauve
	case "teal":
		pair = p.Teal
	case "olive":
		pair = p.Olive
	default:
		t.Fatalf("render fixture names palette token %q, which this test cannot resolve; add it here or fix the fixture", name)
	}
	return th.Color(pair)
}

// coloredRunPattern matches text carried by a style run that sets the given
// foreground color. It tolerates the other attributes glamour and chroma set
// alongside it (bold on a heading, italic on a comment) instead of pinning one
// exact escape sequence, so the assertion is about the COLOR the palette
// supplies - which is the thing under test - and not about every attribute that
// happens to travel with it.
func coloredRunPattern(t *testing.T, run coloredRun, th theme.Theme) *regexp.Regexp {
	t.Helper()
	r, g, b, _ := paletteColorFor(t, run.Token, th).RGBA()
	fg := fmt.Sprintf("38;2;%d;%d;%d", uint8(r>>8), uint8(g>>8), uint8(b>>8))
	return regexp.MustCompile(`\x1b\[[0-9;]*` + regexp.QuoteMeta(fg) + `[0-9;]*m` + regexp.QuoteMeta(run.Text))
}

// ansiPattern matches the styling in rendered output, so a text assertion reads
// the visible characters rather than the escapes around them.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visible(s string) string { return ansiPattern.ReplaceAllString(s, "") }

// TestRender_PerCase drives the REAL renderer over each fixture case.
func TestRender_PerCase(t *testing.T) {
	t.Parallel()
	doc := loadRenderDoc(t)

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			th := themeFor(t, c.Theme)
			got := mdrender.New(th).Render(c.Source, c.Width)

			for _, want := range c.WantVisible {
				if !strings.Contains(visible(got), want) {
					t.Errorf("rendered output must contain %q; got:\n%q", want, got)
				}
			}
			for _, missing := range c.WantMissing {
				if strings.Contains(got, missing) {
					t.Errorf("rendered output must not contain %q; got:\n%q", missing, got)
				}
			}
			if c.WantPlain {
				if ansiPattern.MatchString(got) {
					t.Errorf("case expects no styling at all; got:\n%q", got)
				}
				return
			}
			for _, run := range c.WantColored {
				pattern := coloredRunPattern(t, run, th)
				if !pattern.MatchString(got) {
					t.Errorf("output must carry %q styled with the %s token (pattern %s); got:\n%q",
						run.Text, run.Token, pattern, got)
				}
			}
		})
	}
}

// TestRender_Golden snapshots each case's full rendered output, escapes and
// all. It is the regression net the per-case assertions cannot be: a change in
// glamour's layout, in the padding trim, or in the palette shows up here even
// when every named expectation above still passes.
func TestRender_Golden(t *testing.T) {
	doc := loadRenderDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			golden.RequireEqual(t, []byte(mdrender.New(themeFor(t, c.Theme)).Render(c.Source, c.Width)))
		})
	}
}

// TestRender_FitsTheRequestedWidth proves the wrap contract every pane depends
// on: no rendered line is wider than the width asked for. glamour word-wraps
// prose but leaves fenced code and over-long tokens alone, so this is the
// assertion that the hard wrap after it is doing real work.
func TestRender_FitsTheRequestedWidth(t *testing.T) {
	t.Parallel()
	doc := loadRenderDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			// Every case is re-rendered across the narrow band as well as at its
			// own width. Glamour's fenced-code layout overflows its wrap column
			// by a cell or two at narrow widths - a real defect this package's
			// case widths do not reach, and one a consumer laying a body out
			// inside a gutter DOES reach.
			widths := append([]int{c.Width}, narrowWidths...)
			for _, width := range widths {
				got := mdrender.New(themeFor(t, c.Theme)).Render(c.Source, width)
				for i, line := range strings.Split(got, "\n") {
					if w := lipgloss.Width(line); w > width {
						t.Errorf("at width %d, line %d is %d cells wide: %q", width, i, w, visible(line))
					}
				}
			}
		})
	}
}

// narrowWidths is the band where glamour's own wrapping stops holding, swept on
// every fixture case so the clip that fixes it cannot go vacuous.
var narrowWidths = []int{6, 8, 10, 12, 13, 14, 20}

// concurrentRounds is how many times the fixture's case list is replayed
// concurrently. Every replay renders DIFFERENT source, so this is the number of
// simultaneous cold trips through goldmark, not a repeat count.
const concurrentRounds = 6

// concurrentMarker names the source one goroutine renders. Markers are fixed
// width so no marker is a substring of another - which is what lets the
// contamination check below distinguish "my output" from "someone else's".
func concurrentMarker(n int) string { return fmt.Sprintf("marker-%03d", n) }

// TestRender_ConcurrentColdRenders proves the package mutex earns its place.
//
// goldmark (glamour's parser) carries state across its public Render API and is
// NOT reentrant, so two goroutines rendering at once corrupt each other's AST.
// The trap this test exists to avoid is testing that with the SAME source every
// time: the rendered-output cache would serve every concurrent call a hit, so
// goldmark would never actually run twice at once and the test would pass with
// the mutex deleted. Each goroutine therefore renders a source only IT has, so
// every one of them is a cold first-time render all the way through glamour.
//
// Three things are asserted, and each catches a different failure: every result
// carries its OWN marker (the render happened), no result carries ANOTHER
// goroutine's marker (no cross-goroutine contamination), and re-rendering one
// source afterwards returns the same bytes (the concurrent result was not a
// half-built document that got cached). Run it with -race, which is what turns
// the underlying data race into a hard failure rather than garbled output.
func TestRender_ConcurrentColdRenders(t *testing.T) {
	t.Parallel()
	doc := loadRenderDoc(t)

	type job struct {
		marker string
		source string
		theme  string
	}
	jobs := make([]job, 0, len(doc.Cases)*concurrentRounds)
	for range concurrentRounds {
		for _, c := range doc.Cases {
			marker := concurrentMarker(len(jobs))
			jobs = append(jobs, job{
				marker: marker,
				// A heading carrying the marker, then the fixture's own source,
				// so each goroutine parses a real markdown shape rather than a
				// trivial one - and a different one from every other goroutine.
				source: "# " + marker + "\n\n" + c.Source,
				theme:  c.Theme,
			})
		}
	}
	if len(jobs) < len(doc.Cases) {
		t.Fatalf("built %d concurrent jobs; the test needs at least one per fixture case", len(jobs))
	}

	got := make([]string, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = mdrender.New(theme.New(theme.Mode(j.theme))).Render(j.source, doc.ConcurrencyWidth)
		}()
	}
	wg.Wait()

	for i, j := range jobs {
		// Marker text can be split across style runs at a wrap point, so the
		// check reads the visible characters of the line it landed on.
		flat := strings.ReplaceAll(visible(got[i]), "\n", " ")
		if !strings.Contains(flat, j.marker) {
			t.Fatalf("goroutine %d rendered a body without its own marker %q; got:\n%q", i, j.marker, got[i])
		}
		for k, other := range jobs {
			if k == i {
				continue
			}
			if strings.Contains(flat, other.marker) {
				t.Fatalf("goroutine %d's output carries goroutine %d's marker %q - the renders contaminated each other; got:\n%q",
					i, k, other.marker, got[i])
			}
		}
	}

	// Rendering one of them again must reproduce the concurrent result exactly:
	// a torn document that got cached would show up here.
	again := mdrender.New(theme.New(theme.Mode(jobs[0].theme))).Render(jobs[0].source, doc.ConcurrencyWidth)
	if again != got[0] {
		t.Fatalf("re-rendering the first source returned different bytes:\n want %q\n  got %q", got[0], again)
	}
}

// TestRender_DegradesRatherThanStallOnAbsurdlyNestedQuoting pins the cost
// guard. glamour's nested-blockquote layout grows exponentially with depth, and
// a preview body is arbitrary recorded text the pane lays out synchronously
// while drawing a frame - so past a sane depth the renderer must hand back
// readable plain text promptly instead of freezing the surface. The budget here
// is orders of magnitude above the real render and orders of magnitude below
// the stall.
func TestRender_DegradesRatherThanStallOnAbsurdlyNestedQuoting(t *testing.T) {
	t.Parallel()
	const stallBudget = 2 * time.Second
	source := strings.Repeat("> ", 400) + "buried \x00text\x1b[2J"

	done := make(chan string, 1)
	go func() { done <- mdrender.New(theme.New(theme.ModeDark)).Render(source, 40) }()

	var got string
	select {
	case got = <-done:
	case <-time.After(stallBudget):
		t.Fatalf("rendering absurdly nested quoting did not finish within %s; the pane would spin forever", stallBudget)
	}

	if strings.TrimSpace(visible(got)) == "" {
		t.Fatal("the degraded render produced an empty preview")
	}
	if !strings.Contains(got, "buried") {
		t.Errorf("the degraded render lost the words it was meant to keep; got:\n%q", got)
	}
	for _, bad := range []string{"\x00", "\x1b[2J"} {
		if strings.Contains(got, bad) {
			t.Errorf("the degraded render still carries %q; got:\n%q", bad, got)
		}
	}
}

// TestSanitize_RemovesWhatATerminalWouldActOn covers the exported fallback
// directly: it is what a caller shows when rendering is not possible at all, so
// it must be safe on its own.
func TestSanitize_RemovesWhatATerminalWouldActOn(t *testing.T) {
	t.Parallel()
	got := mdrender.Sanitize("keep\r\nthis\ttext \x1b[31mred\x1b[0m \x1b]0;title\x07 \x07 \x9bcsi done")

	for _, bad := range []string{"\r", "\t", "\x1b", "\x07", "\x9b", "title"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitized text still carries %q; got %q", bad, got)
		}
	}
	for _, want := range []string{"keep", "this", "text", "red", "done"} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitized text lost %q; got %q", want, got)
		}
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("sanitizing dropped the line structure markdown is built on; got %q", got)
	}
}
