package gates

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ColorPattern is one hardcoded-terminal-color signature the color grep gate
// searches source text for.
type ColorPattern struct {
	Name string
	re   *regexp.Regexp
}

// ColorPatterns are the three signatures named in the slice specification: a
// hex literal, a raw lipgloss.Color(...) call, and a direct ANSI-indexed
// color construction that bypasses lipgloss.Color entirely. All three are
// legitimate ONLY inside internal/tui/theme, which IS the color source; a
// hit anywhere else in scope means a component picked a color itself instead
// of going through a Theme.
//
// hex-literal is deliberately narrower than "# followed by 3-8 hex digits":
// digits 0-9 are always valid hex characters, so that naive pattern matches
// an ordinary GitHub issue reference like "peasant#166" (a false positive
// found by review mutation-testing this gate against internal/tui/ftue's own
// comments). It instead requires (a) one of the four lengths a CSS/terminal
// hex color actually has - 3, 4, 6, or 8 digits, never 5 or 7 - and (b) the
// character before "#" is not a letter/digit/underscore (so "word#123" never
// matches, but a quoted or backtick-delimited literal like "#1a1b26" does).
var ColorPatterns = []ColorPattern{
	{Name: "hex-literal", re: regexp.MustCompile(`(?:^|[^0-9A-Za-z_])#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{4}|[0-9a-fA-F]{3})\b`)},
	{Name: "lipgloss.Color-call", re: regexp.MustCompile(`\blipgloss\.Color\(`)},
	{Name: "ansi-int-color-literal", re: regexp.MustCompile(`\blipgloss\.ANSIColor\(|\bansi\.(?:Basic|Indexed)Color\(`)},
}

// ColorMatch is one pattern OCCURRENCE at a specific line of a specific
// file - not one per matching line. A line carrying two hex literals, or two
// lipgloss.Color(...) calls, produces two ColorMatch values for that
// pattern, not one; this is what lets the count-pinned allowlist
// (AllowlistEntry.ExpectedHits/ActualHits) see a SECOND color appended onto
// an already-matching line, not only a color on a previously-clean line.
type ColorMatch struct {
	// Path is repository-relative, "/"-separated.
	Path    string
	Line    int
	Pattern string
	Text    string
}

// FindColorMatches scans content (one file's bytes) for every ColorPatterns
// occurrence, line by line, counting per OCCURRENCE (regexp.FindAllString)
// rather than per matching line (regexp.MatchString would collapse two
// same-kind colors on one line into a single hit, which is the same
// same-file blindness the count-pinned allowlist exists to close, just
// relocated to same-line - see TestFindColorMatches_CountsPerOccurrenceNotPerLine).
// It is pure - no filesystem access - so it can be driven directly by
// synthetic content in tests, independent of whatever the real tree
// currently contains.
func FindColorMatches(path string, content []byte) []ColorMatch {
	var matches []ColorMatch
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, pattern := range ColorPatterns {
			for range pattern.re.FindAllString(line, -1) {
				matches = append(matches, ColorMatch{
					Path:    path,
					Line:    i + 1,
					Pattern: pattern.Name,
					Text:    trimmed,
				})
			}
		}
	}
	return matches
}

// ColorScopeDirs are walked in full (recursively, *.go only) by the color
// grep gate, relative to the module root.
var ColorScopeDirs = []string{"internal/tui"}

// ColorScopeFiles are individual files scanned in addition to ColorScopeDirs.
var ColorScopeFiles = []string{"internal/push/wizard.go"}

// ColorExemptDirs are directories inside a scope dir that the color grep
// gate never scans, relative to the module root:
//
//   - internal/tui/theme IS the color source (the generated Palette and its
//     hand-written Mode/ColorPair/Theme types), so lipgloss.Color calls and
//     hex literals there are the point, not a violation.
//   - internal/tui/gates is the grep gate itself: its own source comments
//     document the patterns it searches for, and colors_test.go drives the
//     scanner with synthetic hex/lipgloss.Color/ANSI-int content to prove it
//     actually detects them (see TestFindColorMatches_DetectsAllThreePatterns).
//     Scanning the scanner's own description of, and tests for, the thing it
//     detects would be a self-referential false positive, not a real
//     hardcoded-color choice made by a TUI component.
var ColorExemptDirs = []string{"internal/tui/theme", "internal/tui/gates"}

// ScanForHardcodedColors walks ColorScopeDirs and ColorScopeFiles under root
// (a module root, e.g. testutil.ModuleRoot(t)), skipping ColorExemptDirs,
// and returns every ColorPatterns hit in every *.go file found (production
// and test files alike - a hardcoded color in a _test.go rendering helper is
// still a color the tests didn't get from a Theme).
//
// This used to share its walk/dedup/error-wrap/sort logic with
// ScanForKeyViolations via a generic scanScope helper (scan.go); the key
// grep gate's detection moved onto ast-grep structural rules
// (internal/tui/gates/astrules/, enforced by keys_astgrep_test.go), which
// removed that second caller, so scanScope was inlined back here rather
// than kept as a one-caller abstraction.
func ScanForHardcodedColors(root string) ([]ColorMatch, error) {
	var all []ColorMatch
	seen := map[string]bool{}

	visit := func(absPath string) error {
		relPath, err := filepath.Rel(root, absPath)
		if err != nil {
			return fmt.Errorf("gates.ScanForHardcodedColors: relativize %s under %s: %w", absPath, root, err)
		}
		relPath = filepath.ToSlash(relPath)
		if seen[relPath] {
			return nil
		}
		seen[relPath] = true
		for _, exempt := range ColorExemptDirs {
			if relPath == exempt || strings.HasPrefix(relPath, exempt+"/") {
				return nil
			}
		}
		if !strings.HasSuffix(relPath, ".go") {
			return nil
		}
		content, err := os.ReadFile(absPath)
		if err != nil {
			return fmt.Errorf("gates.ScanForHardcodedColors: read %s: %w", absPath, err)
		}
		all = append(all, FindColorMatches(relPath, content)...)
		return nil
	}

	for _, dir := range ColorScopeDirs {
		absDir := filepath.Join(root, filepath.FromSlash(dir))
		if _, err := os.Stat(absDir); err != nil {
			return nil, fmt.Errorf(
				"gates.ScanForHardcodedColors: scope directory %s does not exist under %s: %w", dir, root, err)
		}
		err := filepath.WalkDir(absDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			return visit(path)
		})
		if err != nil {
			return nil, err
		}
	}
	for _, file := range ColorScopeFiles {
		absFile := filepath.Join(root, filepath.FromSlash(file))
		if _, err := os.Stat(absFile); err != nil {
			return nil, fmt.Errorf(
				"gates.ScanForHardcodedColors: scope file %s does not exist under %s: %w", file, root, err)
		}
		if err := visit(absFile); err != nil {
			return nil, err
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Path != all[j].Path {
			return all[i].Path < all[j].Path
		}
		return all[i].Line < all[j].Line
	})
	return all, nil
}
