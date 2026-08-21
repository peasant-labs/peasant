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

// LayoutPattern is one hand-rolled-layout signature the layout grep gate
// searches source text for.
//
// receiverGroup names the submatch index holding the identifier immediately
// before the matched call, or 0 when the pattern captures nothing. A pattern
// with a receiver group can ignore a homonym call that has nothing to do with
// layout, such as context.Background.
type LayoutPattern struct {
	Name            string
	re              *regexp.Regexp
	receiverGroup   int
	ignoreReceivers []string
}

// LayoutPatterns are the four signatures of a surface painting or padding its
// own lines instead of composing through the kit layout primitives
// (kit.Panel, kit.ScrollStrip, kit.FitLine, kit.FitCell, kit.Center,
// kit.Indent).
//
// Every one of them is legitimate ONLY inside internal/tui/kit, which IS the
// layout source, and inside internal/tui/theme, which defines the style
// bundles. A hit anywhere else means a surface re-implemented the
// measure-widest-line, pad-every-line, paint-the-background rule by hand,
// which is how the ragged background bug kept coming back:
//
//   - hand-rolled-space-pad: a space run built at the surface. Padding must
//     come from kit.FitLine, kit.FitCell, kit.Panel, or kit.Indent, which
//     measure ansi-aware and apply the style exactly once after fitting.
//   - direct-place: lipgloss.Place at the surface. Centering must go through
//     kit.Center (or kit.Panel with PanelAlignCenter), which always paints the
//     surrounding cells.
//   - direct-align: a lipgloss alignment applied at the surface. A panel
//     aligns its own lines inside one measured box.
//   - direct-background: a background painted onto a theme role at the
//     surface. A foreground-only role must be repainted through
//     kit.Panel.Style so the whole block shares one background token.
var LayoutPatterns = []LayoutPattern{
	{Name: "hand-rolled-space-pad", re: regexp.MustCompile(`\bstrings\.Repeat\("\s"`)},
	{Name: "direct-place", re: regexp.MustCompile(`\blipgloss\.Place\(`)},
	{Name: "direct-align", re: regexp.MustCompile(`\.Align\(lipgloss\.`)},
	{
		Name: "direct-background",
		// The receiver group is optional on purpose: a chained call such as
		// lipgloss.NewStyle().Background(bg) has a ")" before the dot, which
		// captures an empty receiver and still counts as a hit.
		re:              regexp.MustCompile(`([A-Za-z0-9_]*)\.Background\(`),
		receiverGroup:   1,
		ignoreReceivers: []string{"context"},
	},
}

// LayoutMatch is one LayoutPatterns OCCURRENCE at a specific line of a
// specific file, counted per occurrence rather than per matching line, for
// the same reason ColorMatch is: two hand-rolled pads on one line must read
// as two hits so the count-pinned allowlist can see the second one.
type LayoutMatch struct {
	// Path is repository-relative, "/"-separated.
	Path    string
	Line    int
	Pattern string
	Text    string
}

// FindLayoutMatches scans content (one file's bytes) for every LayoutPatterns
// occurrence, line by line. It is pure - no filesystem access - so tests can
// drive it with synthetic content, independent of what the real tree holds.
func FindLayoutMatches(path string, content []byte) []LayoutMatch {
	var matches []LayoutMatch
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, pattern := range LayoutPatterns {
			for _, occurrence := range pattern.re.FindAllStringSubmatch(line, -1) {
				if pattern.ignores(occurrence) {
					continue
				}
				matches = append(matches, LayoutMatch{
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

// ignores reports whether one occurrence's captured receiver is on the
// pattern's ignore list.
func (p LayoutPattern) ignores(occurrence []string) bool {
	if p.receiverGroup <= 0 || p.receiverGroup >= len(occurrence) {
		return false
	}
	for _, receiver := range p.ignoreReceivers {
		if occurrence[p.receiverGroup] == receiver {
			return true
		}
	}
	return false
}

// LayoutScopeDirs are walked in full (recursively, production *.go only) by
// the layout grep gate, relative to the module root.
var LayoutScopeDirs = []string{"internal/tui"}

// LayoutScopeFiles are individual files scanned in addition to
// LayoutScopeDirs.
var LayoutScopeFiles = []string{"internal/push/wizard.go"}

// LayoutExemptDirs are directories inside a scope dir the layout grep gate
// never scans, relative to the module root:
//
//   - internal/tui/kit OWNS the layout primitives, so the padding, placing,
//     aligning, and background painting there are the implementation of the
//     rule, not a violation of it.
//   - internal/tui/theme defines the style bundles every surface derives
//     from, so its Background calls are the token source.
//   - internal/tui/gates is the grep gate itself: its own source documents
//     the patterns it searches for.
var LayoutExemptDirs = []string{"internal/tui/kit", "internal/tui/theme", "internal/tui/gates"}

// ScanForHandRolledLayout walks LayoutScopeDirs and LayoutScopeFiles under
// root (a module root, e.g. testutil.ModuleRoot(t)), skipping
// LayoutExemptDirs, and returns every LayoutPatterns hit in every PRODUCTION
// *.go file found.
//
// Test files are out of scope. A test builds expected strings by hand, and
// such a string is not a surface a user ever sees; the gate is about how a
// mounted surface composes its own output.
func ScanForHandRolledLayout(root string) ([]LayoutMatch, error) {
	var all []LayoutMatch
	seen := map[string]bool{}

	visit := func(absPath string) error {
		relPath, err := filepath.Rel(root, absPath)
		if err != nil {
			return fmt.Errorf("gates.ScanForHandRolledLayout: relativize %s under %s: %w", absPath, root, err)
		}
		relPath = filepath.ToSlash(relPath)
		if seen[relPath] {
			return nil
		}
		seen[relPath] = true
		for _, exempt := range LayoutExemptDirs {
			if relPath == exempt || strings.HasPrefix(relPath, exempt+"/") {
				return nil
			}
		}
		if !strings.HasSuffix(relPath, ".go") || strings.HasSuffix(relPath, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(absPath)
		if err != nil {
			return fmt.Errorf("gates.ScanForHandRolledLayout: read %s: %w", absPath, err)
		}
		all = append(all, FindLayoutMatches(relPath, content)...)
		return nil
	}

	for _, dir := range LayoutScopeDirs {
		absDir := filepath.Join(root, filepath.FromSlash(dir))
		if _, err := os.Stat(absDir); err != nil {
			return nil, fmt.Errorf(
				"gates.ScanForHandRolledLayout: scope directory %s does not exist under %s: %w", dir, root, err)
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
	for _, file := range LayoutScopeFiles {
		absFile := filepath.Join(root, filepath.FromSlash(file))
		if _, err := os.Stat(absFile); err != nil {
			return nil, fmt.Errorf(
				"gates.ScanForHandRolledLayout: scope file %s does not exist under %s: %w", file, root, err)
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
