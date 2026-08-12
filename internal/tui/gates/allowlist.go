// Package gates implements the shared hardcoded-value grep gates for the TUI
// kit: the color grep gate (colors.go/colors_test.go) and the key grep gate
// (keys.go/keys_test.go), both driven by the SAME
// testdata/legacy_allowlist.yaml - see Kind.
//
// This package is test-only: nothing outside internal/tui/gates imports it,
// and no shipped binary links it. It is a regular (non-_test.go) package so
// its allowlist loader and scanner are shared source, not duplicated per
// gate file, mirroring internal/testutil's role for production tests.
package gates

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind identifies which grep gate a GateCount pins a hit count for.
type Kind string

const (
	// GateColors is the hardcoded-terminal-color grep gate (colors_test.go):
	// hex literals, raw lipgloss.Color(...) calls, and ANSI-int color
	// constructions outside internal/tui/theme.
	GateColors Kind = "colors"
	// GateKeys is the raw-key-string grep gate (keys.go/keys_test.go): a
	// comparison or switch against tea.KeyPressMsg.String() itself, outside
	// internal/tui/keymap, which owns dispatching key presses via
	// keymap.Match against a named ActionID instead.
	GateKeys Kind = "keys"
)

// String implements fmt.Stringer.
func (k Kind) String() string { return string(k) }

// IsValid reports whether k is a known Kind.
func (k Kind) IsValid() bool {
	switch k {
	case GateColors, GateKeys:
		return true
	}
	return false
}

// GateCount pins the EXACT number of a gate Kind's pattern hits an
// AllowlistEntry's Path (and everything under it, for a directory glob) is
// allowed to carry.
//
// A file-granular allowlist (just "this path is exempt") only proves a hit
// existed here once; it says nothing about whether MORE were added later,
// since any new hit in an already-listed file is invisible to a check that
// only asks "is this path listed" - review mutation-testing (appending a
// second lipgloss.Color call to an already-allowlisted file) demonstrated
// exactly that miss. Pinning the count is what makes "every hit is
// accounted for, and the accounting shrinks to zero" a checkable claim
// rather than a comment nobody re-verifies: a rise means a NEW hardcoded
// value was introduced (a real regression), and a fall means one was
// removed without the entry being updated (the entry is now overstated and
// must be lowered or deleted).
//
// ExpectedHits must be at least 1. A path that has been fully migrated away
// from (actual hits reaching 0) is not represented by pinning ExpectedHits
// at 0 - LoadAllowlist rejects that - it is represented by DELETING the
// GateCount (or the whole AllowlistEntry, if colors was its only gate).
// Pinning zero would let a fully-retired entry linger forever: nothing in
// the up/down comparison ever fires for an entry whose actual hits are
// also 0, so "stale, must be deleted" would stop being a checkable claim
// for exactly the entries that most need it checked - the ones this epic's
// "must reach zero before close" is about.
type GateCount struct {
	Gate         Kind `yaml:"gate"`
	ExpectedHits int  `yaml:"expectedHits"`
}

// AllowlistEntry exempts one repository-relative path from one or more gate
// Kinds, up to the EXACT hit count each names, until the epoch named in
// RetiredBy migrates it away or removes the underlying hardcoded value
// outright.
//
// Path is either an exact file ("internal/push/wizard.go") or a directory
// glob ending in "/*" ("internal/tui/ftue/*"), which matches every file
// anywhere under that directory - see PathMatchesEntry. Gates is a list
// rather than a single count so one path can carry independent counts per
// gate Kind (e.g. a keymap file could one day need both a "colors" count and
// a "keys" count without forking the schema or the file).
type AllowlistEntry struct {
	Path      string      `yaml:"path"`
	Gates     []GateCount `yaml:"gates"`
	RetiredBy string      `yaml:"retiredBy"`
	Reason    string      `yaml:"reason"`
}

// ExpectedHits reports the pinned hit count this entry allows for kind, and
// whether the entry names that gate Kind at all.
func (e AllowlistEntry) ExpectedHits(kind Kind) (int, bool) {
	for _, g := range e.Gates {
		if g.Gate == kind {
			return g.ExpectedHits, true
		}
	}
	return 0, false
}

// Allowlist is the decoded, validated contents of legacy_allowlist.yaml.
type Allowlist struct {
	// ExpectedEntryCount pins the entry count declared in the fixture, the
	// same self-consistency check internal/config's level-phrase fixture
	// uses: it catches a stray or duplicated entry, not a shrinking
	// allowlist - unlike that fixture's floor, this one is EXPECTED to
	// shrink as epochs land and migrate their entries away, so there is no
	// non-decreasing floor here.
	ExpectedEntryCount int              `yaml:"expectedEntryCount"`
	Entries            []AllowlistEntry `yaml:"entries"`
}

// LoadAllowlist decodes and fully validates legacy_allowlist.yaml. It fails
// closed on any structural problem, so a malformed allowlist cannot silently
// exempt nothing (and cause an entirely different failure downstream) or
// exempt everything (a decode into a partially-zero value).
func LoadAllowlist(data []byte) (Allowlist, error) {
	var list Allowlist
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&list); err != nil {
		return list, fmt.Errorf(
			"gates.LoadAllowlist: legacy_allowlist.yaml did not decode.\n"+
				"what: the document has an unknown field or does not match the typed schema.\n"+
				"why: %v\n"+
				"where: internal/tui/gates.LoadAllowlist.\n"+
				"when: loading the shared color/key grep gate allowlist.\n"+
				"means: no gate that reads this allowlist can run.\n"+
				"fix: match the AllowlistEntry schema (path, gates: [{gate, expectedHits}], retiredBy, reason) and remove "+
				"unknown fields.", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return list, fmt.Errorf(
			"gates.LoadAllowlist: legacy_allowlist.yaml holds more than one YAML document.\n"+
				"what: %v\n"+
				"where: internal/tui/gates.LoadAllowlist.\n"+
				"when: loading the shared color/key grep gate allowlist.\n"+
				"means: entries after the first document would never be read.\n"+
				"fix: keep exactly one YAML document in the file.", err)
	}
	if list.ExpectedEntryCount != len(list.Entries) {
		return list, fmt.Errorf(
			"gates.LoadAllowlist: expectedEntryCount=%d but the file holds %d entries.\n"+
				"where: internal/tui/gates.LoadAllowlist.\n"+
				"when: loading the shared color/key grep gate allowlist.\n"+
				"means: an entry was added or removed without updating the declared count, which is the same "+
				"stray-edit signal internal/config's fixtures use.\n"+
				"fix: set expectedEntryCount to the number of entries actually present.",
			list.ExpectedEntryCount, len(list.Entries))
	}
	seen := map[string]bool{}
	for i, entry := range list.Entries {
		where := fmt.Sprintf("internal/tui/gates.LoadAllowlist, entry index %d", i)
		if strings.TrimSpace(entry.Path) == "" {
			return list, fmt.Errorf("gates.LoadAllowlist: entry has an empty path (%s)", where)
		}
		if len(entry.Gates) == 0 {
			return list, fmt.Errorf(
				"gates.LoadAllowlist: entry %q names no gates (%s).\n"+
					"means: it exempts nothing and can never be matched by any scan, which is indistinguishable from a "+
					"typo that dropped the gates list.\n"+
					"fix: name at least one gate with a pinned expectedHits: colors, keys.", entry.Path, where)
		}
		seenGate := map[Kind]bool{}
		for _, g := range entry.Gates {
			if !g.Gate.IsValid() {
				return list, fmt.Errorf(
					"gates.LoadAllowlist: entry %q names unknown gate %q (%s); valid gates: colors, keys.",
					entry.Path, g.Gate, where)
			}
			if seenGate[g.Gate] {
				return list, fmt.Errorf(
					"gates.LoadAllowlist: entry %q names gate %q more than once (%s); each entry may pin at most one "+
						"expectedHits per gate Kind.", entry.Path, g.Gate, where)
			}
			seenGate[g.Gate] = true
			if g.ExpectedHits <= 0 {
				return list, fmt.Errorf(
					"gates.LoadAllowlist: entry %q names expectedHits=%d for gate %q, which is not a valid pin (%s).\n"+
						"what: expectedHits must be at least 1.\n"+
						"why: a hit count of 0 (or negative) covers zero occurrences, so pinning it exempts nothing - it "+
						"is indistinguishable from the entry no longer being needed.\n"+
						"where: internal/tui/gates.LoadAllowlist.\n"+
						"when: loading the shared color/key grep gate allowlist.\n"+
						"means: an entry left at expectedHits=0 would never trip the up/down count comparison (0 pinned, "+
						"0 actual matches), so it would never be forced to zero and out - it would just linger, which "+
						"contradicts this allowlist shrinking to empty by epoch close.\n"+
						"fix: DELETE this fully-retired gate entry (or the whole AllowlistEntry, if %q was its only gate) "+
						"instead of pinning it at 0; and decrement expectedEntryCount if the entry itself is removed.",
					entry.Path, g.ExpectedHits, g.Gate, where, g.Gate)
			}
		}
		if strings.TrimSpace(entry.RetiredBy) == "" {
			return list, fmt.Errorf(
				"gates.LoadAllowlist: entry %q has no retiredBy (%s).\n"+
					"means: every legacy exemption must name the epoch or issue that will remove it, or it accumulates "+
					"indefinitely with no owner.\n"+
					"fix: name the epoch/issue that retires this entry.", entry.Path, where)
		}
		key := entry.Path
		if seen[key] {
			return list, fmt.Errorf("gates.LoadAllowlist: duplicate entry for path %q (%s)", entry.Path, where)
		}
		seen[key] = true
	}
	return list, nil
}

// PathMatchesEntry reports whether repoRelPath (a file path relative to the
// module root, using "/" separators) is exempted by entryPath.
//
// entryPath is either an exact file, matched verbatim, or a directory glob
// ending in "/*", which matches every file anywhere under that directory
// (recursively - internal/tui/ftue/* covers internal/tui/ftue/styles.go and
// would equally cover a nested internal/tui/ftue/sub/file.go).
func PathMatchesEntry(entryPath, repoRelPath string) bool {
	if dir, ok := strings.CutSuffix(entryPath, "/*"); ok {
		return repoRelPath == dir || strings.HasPrefix(repoRelPath, dir+"/")
	}
	return entryPath == repoRelPath
}

// Covers reports whether the allowlist names an EXPECTED-hits entry for
// repoRelPath under kind (regardless of what the current actual count is),
// and that entry if so. A path with no covering entry at all - as opposed to
// one whose actual count merely disagrees with a covering entry's
// ExpectedHits - is the "add an allowlist entry" failure mode; a count
// disagreement on an already-covered path is the separate "update the
// pinned count" failure mode CountHitsByPath + AllowlistEntry.ExpectedHits
// exist to catch.
func (a Allowlist) Covers(repoRelPath string, kind Kind) (AllowlistEntry, bool) {
	for _, entry := range a.Entries {
		if _, ok := entry.ExpectedHits(kind); !ok {
			continue
		}
		if PathMatchesEntry(entry.Path, repoRelPath) {
			return entry, true
		}
	}
	return AllowlistEntry{}, false
}

// CountHitsByPath aggregates a slice of repo-relative hit paths (one entry
// per pattern match - a file with 3 hits appears 3 times) into a per-path
// hit count. Shared by the color grep gate and any future key grep gate, so
// both count hits the same way.
func CountHitsByPath(paths []string) map[string]int {
	counts := make(map[string]int, len(paths))
	for _, p := range paths {
		counts[p]++
	}
	return counts
}

// ActualHits sums countsByPath for every path e's Path covers (a single
// file, or every path under a directory glob), so a directory-glob entry
// like internal/tui/ftue/* is checked against the TOTAL hit count across the
// whole directory, not any one file within it.
func (e AllowlistEntry) ActualHits(countsByPath map[string]int) int {
	total := 0
	for path, n := range countsByPath {
		if PathMatchesEntry(e.Path, path) {
			total += n
		}
	}
	return total
}
