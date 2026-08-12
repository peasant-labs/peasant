//go:build astgrep

// Package gates_test's astgrep-tagged file: the REAL key grep gate, which
// shells out to the ast-grep CLI. It is excluded from a plain `go test
// ./...` / `go test -race ./...` build entirely (Go's build-tag mechanism
// drops this file at compile time when the "astgrep" tag is not passed), so
// the default, hermetic test run never depends on the ast-grep binary being
// on PATH. It is invoked explicitly via `go test -tags=astgrep
// ./internal/tui/gates/...`, added as its own step in `make check` right
// after the pre-existing (and already ast-grep-dependent) `ast-grep scan
// --config sgconfig.yml .` line.
//
// Validated against ast-grep 0.40.5 (this repo's flake.nix-pinned
// devShell/CI version) and 0.43.0 (a newer ambient version): both produce
// byte-identical match sets and --json shapes for the rules in
// internal/tui/gates/astrules against this repository as of this writing.
package gates_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/gates"
)

// astGrepBin is the ast-grep executable name resolved via PATH.
const astGrepBin = "ast-grep"

// astRulesConfigRelPath is the module-root-relative path to the key-string
// rules' own sgconfig.yml - kept separate from the repo-root sgconfig.yml
// so the plain `ast-grep scan --config sgconfig.yml .` step in `make check`
// never sees these rules (see astrules/sgconfig.yml's own doc comment).
const astRulesConfigRelPath = "internal/tui/gates/astrules/sgconfig.yml"

// astGrepMatch is the subset of ast-grep's --json output this gate reads.
type astGrepMatch struct {
	File  string `json:"file"`
	Range struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
	} `json:"range"`
	Text   string `json:"text"`
	RuleID string `json:"ruleId"`
}

// runAstGrep runs `ast-grep scan --config <configAbsPath> --json=compact .`
// with dir as the subprocess's working directory, and decodes the result
// into []gates.KeyMatch.
//
// dir MUST be the root the rule configs' `files`/`ignores` globs (and the
// paths ast-grep reports back) are relative to - verified empirically that
// ast-grep resolves both against the scan invocation's OWN working
// directory, not against the (possibly absolute) scan target path: scanning
// an absolute path from an unrelated cwd made every files/ignores glob
// silently match nothing. configAbsPath may safely be absolute regardless -
// only the SCANNED source paths are subject to files/ignores globbing, not
// the config file's own location.
func runAstGrep(t *testing.T, dir, configAbsPath string) []gates.KeyMatch {
	t.Helper()
	if _, err := exec.LookPath(astGrepBin); err != nil {
		t.Fatalf(
			"keys_astgrep_test: ast-grep binary not found on PATH.\n"+
				"what: exec.LookPath(%q) failed: %v.\n"+
				"why: this test invokes the ast-grep CLI (validated against 0.40.5/0.43.0) to detect raw key-string "+
				"comparisons structurally; it cannot run without the binary.\n"+
				"where: internal/tui/gates/keys_astgrep_test.go, runAstGrep.\n"+
				"when: go test -tags=astgrep ./internal/tui/gates/...\n"+
				"means: this gate did not run at all - a green plain `go test ./...` says nothing about it, since "+
				"it is excluded by the astgrep build tag.\n"+
				"fix: run inside the project's nix devShell (`nix develop`), which provides ast-grep, or install it "+
				"(https://ast-grep.github.io) and ensure it is on PATH.",
			astGrepBin, err)
	}

	cmd := exec.Command(astGrepBin, "scan", "--config", configAbsPath, "--json=compact", ".")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var raw []astGrepMatch
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf(
			"keys_astgrep_test: ast-grep output did not decode as the expected JSON array.\n"+
				"what: json.Unmarshal failed: %v.\n"+
				"why: either ast-grep errored before producing output (see stderr below), or a version drift "+
				"changed the --json shape this test expects (file/range.start.line/text/ruleId).\n"+
				"where: internal/tui/gates/keys_astgrep_test.go, runAstGrep.\n"+
				"when: invoking `ast-grep scan --config %s --json=compact .` in %s.\n"+
				"means: no real matches could be read, so the count-pinned comparison cannot run.\n"+
				"fix: run `ast-grep --version` and compare against the versions this test is validated for "+
				"(0.40.5, 0.43.0); if ast-grep itself errored, see stderr:\n%s",
			err, configAbsPath, dir, stderr.String())
	}
	// ast-grep exits non-zero when it finds matches at error severity (all 3
	// rules here are severity:error) - that is the ordinary "matches found"
	// case, already reflected in the JSON decoded above, not a real
	// invocation failure. Anything other than a clean exit(0) or the
	// expected exit(1) is treated as a real problem (a rule/config error),
	// even though stdout might have decoded.
	var exitErr *exec.ExitError
	if runErr != nil {
		if !errors.As(runErr, &exitErr) || (exitErr.ExitCode() != 0 && exitErr.ExitCode() != 1) {
			t.Fatalf(
				"keys_astgrep_test: ast-grep exited unexpectedly.\n"+
					"what: exit error %v (want a clean exit or exit(1) for \"matches found\").\n"+
					"where: internal/tui/gates/keys_astgrep_test.go, runAstGrep.\n"+
					"when: invoking `ast-grep scan --config %s --json=compact .` in %s.\n"+
					"means: the scan likely did not complete as configured (e.g. a malformed rule), so its output "+
					"cannot be trusted even though stdout happened to decode.\n"+
					"fix: run the same command manually and read stderr:\n%s",
				runErr, configAbsPath, dir, stderr.String())
		}
	}

	matches := make([]gates.KeyMatch, 0, len(raw))
	for i, m := range raw {
		if m.File == "" || m.RuleID == "" || m.Range.Start.Line == 0 {
			t.Fatalf(
				"keys_astgrep_test: ast-grep match %d decoded with an empty/zero required field "+
					"(File=%q RuleID=%q Line=%d).\n"+
					"why: an empty Path would never match any allowlist entry, silently exempting a real hit - "+
					"failing closed here instead of letting that through.\n"+
					"where: internal/tui/gates/keys_astgrep_test.go, runAstGrep.\n"+
					"means: a version drift likely changed ast-grep's --json field names.\n"+
					"fix: compare `ast-grep --version` against 0.40.5/0.43.0 and update astGrepMatch's json tags.",
				i, m.File, m.RuleID, m.Range.Start.Line)
		}
		matches = append(matches, gates.KeyMatch{
			Path:    filepath.ToSlash(m.File),
			Line:    m.Range.Start.Line,
			Pattern: m.RuleID,
			Text:    strings.TrimSpace(m.Text),
		})
	}
	return matches
}

// --- the real gate: scans the actual tree, checked against the shared allowlist ---

// TestKeyGate_MatchesAllowlistCounts is the key grep gate itself: every
// raw-key-string hit path in the real tree must be covered by a "keys"
// allowlist entry, and every covered entry's actual current hit count must
// equal its pinned expectedHits - see checkKeyAllowlistCounts (keys_test.go)
// and TestColorGate_MatchesAllowlistCounts's doc (colors_test.go) for the
// full up/down/stale rationale, which applies identically here. The only
// difference from the color gate is where the matches come from: ast-grep
// structural rules (internal/tui/gates/astrules/) instead of a Go regexp
// scanner.
func TestKeyGate_MatchesAllowlistCounts(t *testing.T) {
	allowlist, err := gates.LoadAllowlist(legacyAllowlistData)
	if err != nil {
		t.Fatalf("load testdata/legacy_allowlist.yaml: %v", err)
	}

	root := testutil.ModuleRoot(t)
	configAbsPath := filepath.Join(root, filepath.FromSlash(astRulesConfigRelPath))
	matches := runAstGrep(t, root, configAbsPath)
	if err := checkKeyAllowlistCounts(allowlist, matches); err != nil {
		t.Fatal(err.Error())
	}
}

// --- rule-behavior/regression fixtures ------------------------------------

//go:embed testdata/astgrep_fixtures.yaml
var astGrepFixturesData []byte

type astGrepFixturesDocument struct {
	ExpectedCaseCount int                  `yaml:"expectedCaseCount"`
	Cases             []astGrepFixtureCase `yaml:"cases"`
}

type astGrepFixtureCase struct {
	Name       string         `yaml:"name"`
	Dir        string         `yaml:"dir"`
	WantByRule map[string]int `yaml:"wantByRule"`
	Source     string         `yaml:"source"`
}

func loadAstGrepFixtures(data []byte) (astGrepFixturesDocument, error) {
	var doc astGrepFixturesDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/astgrep_fixtures.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/astgrep_fixtures.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		return doc, fmt.Errorf(
			"testdata/astgrep_fixtures.yaml: expectedCaseCount=%d but found %d cases (and must be non-zero)",
			doc.ExpectedCaseCount, len(doc.Cases))
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] {
			return doc, fmt.Errorf("testdata/astgrep_fixtures.yaml: case name %q is missing or duplicated", c.Name)
		}
		seen[c.Name] = true
	}
	return doc, nil
}

// TestAstGrepRules_MatchExpectedCounts writes every fixture case's source to
// its own file under a shared temp root (so the rules' `files`/`ignores`
// globs, which are relative to the scan root, apply the same way they do to
// the real tree), scans the WHOLE temp root in ONE ast-grep invocation
// (proving multiple files scanned together don't cross-contaminate each
// other's counts), and partitions the matches back to their originating
// file, asserting each case's exact per-rule-id hit count.
//
// This banks (as committed tests, not throwaway probes) the regressions
// review asked for: a left-operand-only equality comparison (B's
// "msg.String() == \"q\"" miss) is caught, a two-sided occurrence counts
// exactly once, a real chained two-sided form counts once per comparison,
// the msg-anchored switch/capture rules do not false-positive on other
// types' .String() calls, the indexed-capture idiom is excluded, and a file
// under internal/tui/keymap is excluded by the rules' own `ignores`.
func TestAstGrepRules_MatchExpectedCounts(t *testing.T) {
	doc, err := loadAstGrepFixtures(astGrepFixturesData)
	if err != nil {
		t.Fatalf("load testdata/astgrep_fixtures.yaml: %v", err)
	}

	root := testutil.ModuleRoot(t)
	configAbsPath := filepath.Join(root, filepath.FromSlash(astRulesConfigRelPath))

	tempRoot := t.TempDir()
	filePathByCase := make(map[string]string, len(doc.Cases))
	for _, c := range doc.Cases {
		dir := filepath.Join(tempRoot, filepath.FromSlash(c.Dir))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create fixture dir %s: %v", dir, err)
		}
		filePath := filepath.Join(dir, c.Name+".go")
		if err := os.WriteFile(filePath, []byte(c.Source), 0o644); err != nil {
			t.Fatalf("write fixture file %s: %v", filePath, err)
		}
		relPath := filepath.ToSlash(filepath.Join(c.Dir, c.Name+".go"))
		filePathByCase[relPath] = c.Name
	}

	matches := runAstGrep(t, tempRoot, configAbsPath)
	gotByCase := map[string]map[string]int{}
	for _, m := range matches {
		caseName, ok := filePathByCase[m.Path]
		if !ok {
			t.Errorf("ast-grep reported a match in %q, which is not one of this test's fixture files: %+v", m.Path, m)
			continue
		}
		if gotByCase[caseName] == nil {
			gotByCase[caseName] = map[string]int{}
		}
		gotByCase[caseName][m.Pattern]++
	}

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := gotByCase[c.Name]
			want := c.WantByRule
			if len(got) == 0 && len(want) == 0 {
				return
			}
			for rule, wantCount := range want {
				if got[rule] != wantCount {
					t.Errorf("rule %s matched %d time(s) in case %q, want %d", rule, got[rule], c.Name, wantCount)
				}
			}
			for rule, gotCount := range got {
				if _, ok := want[rule]; !ok && gotCount > 0 {
					t.Errorf("rule %s unexpectedly matched %d time(s) in case %q (want 0, not listed in wantByRule)",
						rule, gotCount, c.Name)
				}
			}
		})
	}
}
