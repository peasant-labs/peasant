package kit_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// kitSourceFiles returns the non-test .go files of the kit package, read from
// the package directory the test runs in.
func kitSourceFiles(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read kit dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("found no kit source files to scan")
	}
	return out
}

// widthByLenPattern flags len(...) applied directly to a rendered string
// (a .View()/.Render() result or a variable named like a rendered line):
// display width must go through the ansi-aware lipgloss.Width, never len(),
// which counts bytes and miscounts escape sequences and wide runes.
var widthByLenPattern = regexp.MustCompile(`len\([^)]*\.(?:View|Render)\(`)

// TestNoWidthByLen proves no kit component measures a rendered string's
// display width with len(). len() on a plain slice (options, items, rows) is
// legitimate and not flagged; only len() reaching into a rendered View/Render
// result is.
func TestNoWidthByLen(t *testing.T) {
	for name, src := range kitSourceFiles(t) {
		if loc := widthByLenPattern.FindString(src); loc != "" {
			t.Errorf("%s uses len() on a rendered string (%q); measure with lipgloss.Width instead", name, loc)
		}
	}
}

// verticalChromeConstPattern flags a subtraction of a small integer literal
// from a height/vertical value - the "-3/-4/-10/-12" chrome-height fudge
// constants the kit exists to eliminate. It is intentionally scoped to height
// identifiers so a legitimate horizontal glyph offset (width-6 for a cursor +
// checkbox) is not flagged.
var verticalChromeConstPattern = regexp.MustCompile(`(?i)(height|outerheight|innerheight|rows)\s*-\s*[0-9]+`)

// TestNoVerticalChromeConstantsOutsideFrame proves that only frame.go performs
// vertical chrome-height accounting. Every other component receives an inner
// height and never subtracts a border/title/footer constant from a height
// itself - the single-owner invariant that retires the per-surface height
// fudge constants.
func TestNoVerticalChromeConstantsOutsideFrame(t *testing.T) {
	for name, src := range kitSourceFiles(t) {
		if name == "frame.go" {
			continue // Frame is the ONE owner of chrome-height accounting.
		}
		for i, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if verticalChromeConstPattern.MatchString(line) {
				t.Errorf("%s:%d subtracts a vertical chrome constant (%q); height accounting belongs to frame.go only",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestKitSourcePresent is a trip-wire: the two discipline scanners are only
// meaningful if they actually saw the component files. This asserts the scan
// covered the expected component set so a future refactor that moves files
// cannot silently empty the scan.
func TestKitSourcePresent(t *testing.T) {
	files := kitSourceFiles(t)
	for _, want := range []string{
		"frame.go", "overlay.go", "confirm.go", "spinner.go", "list.go",
		"textfield.go", "toggle.go", "radio.go", "multiselect.go", "statusbar.go",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("expected kit source file %s to be scanned", want)
		}
	}
}
