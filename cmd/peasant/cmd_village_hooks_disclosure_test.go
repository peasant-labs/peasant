package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/village_hooks_disclosures.yaml
var villageHooksDisclosureFixtureData []byte

const villageHooksDisclosureFixturePath = "cmd/peasant/testdata/village_hooks_disclosures.yaml"

// hooksPathShape is where git is configured to run hooks from. The distinction
// is a consent one: git's own directory is never tracked, while a directory in
// the working tree is the convention for sharing hooks through the repository.
type hooksPathShape string

const (
	hooksPathDefaultGitDir     hooksPathShape = "default-git-dir"
	hooksPathInsideWorkingTree hooksPathShape = "inside-working-tree"
)

var allHooksPathShapes = [...]hooksPathShape{hooksPathDefaultGitDir, hooksPathInsideWorkingTree}

type villageHooksDisclosureDocument struct {
	ExpectedCaseCount int                          `yaml:"expectedCaseCount"`
	Cases             []villageHooksDisclosureCase `yaml:"cases"`
}

type villageHooksDisclosureCase struct {
	Name           string                `yaml:"name"`
	HooksPath      hooksPathShape        `yaml:"hooksPath"`
	Visibility     config.Visibility     `yaml:"visibility"`
	Level          redact.RedactionLevel `yaml:"level"`
	MustContain    []string              `yaml:"mustContain"`
	MustNotContain []string              `yaml:"mustNotContain"`
}

// loadVillageHooksDisclosureFixture decodes and fully validates the corpus.
func loadVillageHooksDisclosureFixture(data []byte) (villageHooksDisclosureDocument, error) {
	var document villageHooksDisclosureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf(
			"village hooks disclosure fixture rule failed: typed YAML fields must match the document schema; unknown or "+
				"malformed data invalidates the consent-disclosure evidence; where=%s loader=first-document decode; "+
				"when=test fixture loading; impact=install-time disclosures cannot be trusted; "+
				"fix=remove unknown fields and provide expectedCaseCount plus typed cases: %w",
			villageHooksDisclosureFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, fmt.Errorf(
			"village hooks disclosure fixture rule failed: exactly one YAML document is allowed; trailing data is silently "+
				"ignored; where=%s loader=end-of-document check; when=test fixture loading; "+
				"impact=install-time disclosures cannot be trusted; fix=remove the second document: %w",
			villageHooksDisclosureFixturePath, err)
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, fmt.Errorf(
			"village hooks disclosure fixture rule failed: declared and actual case counts must match and be non-zero, got "+
				"expectedCaseCount=%d cases=%d; where=%s loader=case-count validation; when=test fixture loading; "+
				"impact=install-time disclosures cannot be trusted; fix=set expectedCaseCount to the number of cases present",
			document.ExpectedCaseCount, len(document.Cases), villageHooksDisclosureFixturePath)
	}
	seen := make(map[string]bool, len(document.Cases))
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, villageHooksDisclosureRuleError(index,
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		if !hooksDisclosureContains(allHooksPathShapes[:], testCase.HooksPath) {
			return document, villageHooksDisclosureRuleError(index,
				fmt.Sprintf("unsupported hooksPath %q", testCase.HooksPath),
				"fix=use default-git-dir or inside-working-tree")
		}
		// Validated against the CONTRACT's closed set, not against the two
		// members the disclosure once compared itself to. A configured group
		// visibility was announced as applied for exactly as long as this check
		// refused to model it.
		if !testCase.Visibility.IsValid() {
			return document, villageHooksDisclosureRuleError(index,
				fmt.Sprintf("unsupported visibility %q", testCase.Visibility),
				fmt.Sprintf("fix=use one of %s", config.VisibilityMenu()))
		}
		if !testCase.Level.IsValid() {
			return document, villageHooksDisclosureRuleError(index,
				fmt.Sprintf("unknown redaction level %q", testCase.Level),
				"fix=use one of minimal, standard, maximum")
		}
		// An install under a level the product cannot apply REFUSES before it
		// reaches any disclosure, so a case pinning install output at such a level
		// describes a run that cannot happen. The refusal is driven in
		// cmd/peasant/testdata/redaction_refusals.yaml instead.
		if !config.RedactionLevelSupported(testCase.Level) {
			return document, villageHooksDisclosureRuleError(index,
				fmt.Sprintf("redaction level %q makes install refuse, so this case can never observe a disclosure", testCase.Level),
				"fix=use a level from config.SupportedRedactionLevels, and drive the refusal from the refusal corpus")
		}
		if len(testCase.MustContain) == 0 {
			return document, villageHooksDisclosureRuleError(index, "mustContain is empty",
				"fix=state what this install has to tell the user, or the case asserts nothing")
		}
		for _, forbidden := range testCase.MustNotContain {
			if hooksDisclosureContains(testCase.MustContain, forbidden) {
				return document, villageHooksDisclosureRuleError(index,
					fmt.Sprintf("%q is both required and forbidden", forbidden),
					"fix=decide which one it is; a phrase in both lists can never pass")
			}
		}
	}
	return document, nil
}

func villageHooksDisclosureRuleError(index int, what, fix string) error {
	return fmt.Errorf(
		"village hooks disclosure fixture rule failed: %s; a malformed case invalidates the consent-disclosure evidence; "+
			"where=%s case index %d; when=test fixture loading; impact=install-time disclosures cannot be trusted; %s",
		what, villageHooksDisclosureFixturePath, index, fix)
}

func hooksDisclosureContains[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// --- loader guards ----------------------------------------------------------

func TestLoadVillageHooksDisclosureFixture_RejectsUnsatisfiablePhrase(t *testing.T) {
	t.Parallel()
	_, err := loadVillageHooksDisclosureFixture([]byte(`expectedCaseCount: 1
cases:
  - name: contradictory
    hooksPath: default-git-dir
    visibility: private
    level: standard
    mustContain: ["created"]
    mustNotContain: ["created"]
`))
	if err == nil || !strings.Contains(err.Error(), "both required and forbidden") {
		t.Fatalf("error = %v, want rejection of a phrase that can never pass", err)
	}
}

func TestLoadVillageHooksDisclosureFixture_RejectsAVisibilityTheContractDoesNotDefine(t *testing.T) {
	t.Parallel()
	_, err := loadVillageHooksDisclosureFixture([]byte(`expectedCaseCount: 1
cases:
  - name: invented-visibility
    hooksPath: default-git-dir
    visibility: unlisted
    level: standard
    mustContain: ["created"]
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported visibility") {
		t.Fatalf("error = %v, want rejection of a visibility the contract does not define", err)
	}
}

// TestLoadVillageHooksDisclosureFixture_AcceptsEveryContractVisibility is the
// guard the previous loader was missing. It refused `group`, so no case could
// cover the visibility that was in fact being announced as applied while the
// village stored private.
func TestLoadVillageHooksDisclosureFixture_AcceptsEveryContractVisibility(t *testing.T) {
	t.Parallel()
	for _, visibility := range schema.AllVisibilities {
		_, err := loadVillageHooksDisclosureFixture([]byte(fmt.Sprintf(`expectedCaseCount: 1
cases:
  - name: contract-visibility
    hooksPath: default-git-dir
    visibility: %s
    level: standard
    mustContain: ["created"]
`, visibility)))
		if err != nil {
			t.Errorf("the corpus must be able to model the contract visibility %q; got: %v", visibility, err)
		}
	}
}

// --- the corpus -------------------------------------------------------------

// TestVillageHooks_InstallDisclosesConsentRelevantFacts drives the production
// install command and checks what it tells the user about a hook it just wrote.
//
// Neither disclosure is a failure: the hook is installed either way. They exist
// because a committable hook and an auto-confirmed public visibility both change
// WHO ends up publishing WHAT, and neither is visible in the file itself.
func TestVillageHooks_InstallDisclosesConsentRelevantFacts(t *testing.T) {
	t.Parallel()
	document, err := loadVillageHooksDisclosureFixture(villageHooksDisclosureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			repo := hooksTestRepo(t)
			if testCase.HooksPath == hooksPathInsideWorkingTree {
				// The convention that exists precisely so hooks are shared
				// through the repository.
				hooksGit(t, repo, "", "config", "core.hooksPath", ".githooks")
			}
			cfgPath := writeCfg(t, t.TempDir(), "hook-settings.yaml", fmt.Sprintf(
				"version: 1\npush:\n  method: all\n  visibility: %s\nredaction:\n  level: %s\n",
				testCase.Visibility, testCase.Level))

			out, installErr := runVillageHooksWithRootFlags(t, []string{"--config", cfgPath},
				"install", "--dir", repo, "--event", "post-commit")
			if installErr != nil {
				t.Fatalf("install: %v (out=%s)", installErr, out)
			}
			for _, want := range testCase.MustContain {
				if !strings.Contains(out, want) {
					t.Errorf("install output must state %q; got:\n%s", want, out)
				}
			}
			for _, forbidden := range testCase.MustNotContain {
				if strings.Contains(out, forbidden) {
					t.Errorf("install output must not state %q; got:\n%s", forbidden, out)
				}
			}
		})
	}
}

// TestVillageHooks_InstallReportsAnUnresolvablePeasantBinary proves install does
// not report unqualified success for a hook that is guaranteed to warn forever.
// The hook resolves `peasant` from the PATH git runs with; if it is not there,
// every commit prints a warning and nothing is ever uploaded.
func TestVillageHooks_InstallReportsAnUnresolvablePeasantBinary(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	out, err := runVillageHooks(t, "install", "--dir", repo, "--event", "post-commit")
	if err != nil {
		t.Fatalf("install: %v (out=%s)", err, out)
	}
	notice := "the peasant binary is not on this shell's PATH"
	// The assertion follows the environment the suite actually runs in rather
	// than mutating PATH, which parallel tests in this package read.
	if _, lookErr := exec.LookPath("peasant"); lookErr != nil {
		if !strings.Contains(out, notice) {
			t.Errorf("peasant is not on PATH, so install must say so; got:\n%s", out)
		}
		if !strings.Contains(out, "no re-install of the hook is needed") {
			t.Errorf("the PATH notice must say the hook itself is fine; got:\n%s", out)
		}
	} else if strings.Contains(out, notice) {
		t.Errorf("peasant is on PATH, so install must not claim otherwise; got:\n%s", out)
	}
	// The notice never turns a successful install into a failure.
	if !strings.Contains(out, string(githooks.OutcomeCreated)) {
		t.Errorf("install must still report what it did; got:\n%s", out)
	}
}

// TestVillageHooks_StatusReportsWhatRunsAndHowToRemoveIt covers the read-only
// surface: status has to answer "what does this actually do to me" and "how do I
// stop it", not describe what an install would have done.
func TestVillageHooks_StatusReportsWhatRunsAndHowToRemoveIt(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	if out, err := runVillageHooks(t, "install", "--dir", repo, "--event", "post-commit"); err != nil {
		t.Fatalf("install: %v (out=%s)", err, out)
	}
	out, err := runVillageHooks(t, "status", "--dir", repo)
	if err != nil {
		t.Fatalf("status: %v (out=%s)", err, out)
	}
	for _, want := range []string{
		"runs: " + githooks.RepositoryCommand(repo, githooks.Binding{}),
		"uninstall: " + githooks.UninstallCommand(githooks.EventPostCommit, repo),
		"Git runs the Peasant-generated post-commit hook",
		// The absent slot must say how to get one, not stay silent.
		githooks.InstallCommand(githooks.EventPrePush, repo),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status must report %q; got:\n%s", want, out)
		}
	}
}

// TestVillageHooks_StatusRepeatsThePublicVisibilityDisclosure covers the gap
// between installing a hook and living with it.
//
// The install-time notice is correct and not enough: push.visibility is one
// global, mutable setting. Installing under one value and later changing it would
// otherwise leave no disclosure anywhere — the hook file names no visibility, and
// the push line that does is suppressed by the --quiet a hook runs with. status is
// the surface that answers "what does this actually do to me".
func TestVillageHooks_StatusRepeatsThePublicVisibilityDisclosure(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	configDir := t.TempDir()
	privatePath := writeCfg(t, configDir, "private.yaml", "version: 1\npush:\n  method: all\n  visibility: private\n")
	publicPath := writeCfg(t, configDir, "public.yaml", "version: 1\npush:\n  method: all\n  visibility: public\n")

	// Installed while private: nothing to disclose yet.
	out, err := runVillageHooksWithRootFlags(t, []string{"--config", privatePath},
		"install", "--dir", repo, "--event", "post-commit")
	if err != nil {
		t.Fatalf("install: %v (out=%s)", err, out)
	}
	if strings.Contains(out, "push.visibility resolves to") {
		t.Errorf("a private install must not disclose a public visibility; got:\n%s", out)
	}
	if out, err := runVillageHooksWithRootFlags(t, []string{"--config", privatePath}, "status", "--dir", repo); err != nil {
		t.Fatalf("status: %v (out=%s)", err, out)
	} else if strings.Contains(out, "push.visibility resolves to") {
		t.Errorf("status must not invent a disclosure while the setting is private; got:\n%s", out)
	}

	// The user later flips the global setting. Nothing about the hook changed.
	out, err = runVillageHooksWithRootFlags(t, []string{"--config", publicPath}, "status", "--dir", repo)
	if err != nil {
		t.Fatalf("status: %v (out=%s)", err, out)
	}
	for _, want := range []string{githooks.UninstallCommandWithBinding(githooks.EventPostCommit, repo, githooks.Binding{ConfigPath: publicPath})} {
		if !strings.Contains(out, want) {
			t.Errorf("status must disclose what the installed hook now publishes, stating %q; got:\n%s", want, out)
		}
	}
	// What it must NOT say. Each of these was printed here while being untrue: a
	// public publish that cannot happen, consent answered for it, and a remedy
	// telling the user to change a configuration that is already correct.
	for _, forbidden := range []string{"visibility is not yet implemented", "downgraded safely", "set push.visibility to private"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("status must not claim %q; got:\n%s", forbidden, out)
		}
	}
	// The event that has no hook must not be named: it publishes nothing.
	if strings.Contains(out, "the pre-push hook publishes") {
		t.Errorf("status must not warn about an event with no hook installed; got:\n%s", out)
	}
}

// TestVillageHooks_StatusReportsARenamedRepository proves status notices the one
// failure mode a healthy-looking hook can hide. The repository root is baked into
// the hook at install time; after the directory is renamed the hook still runs,
// still exits 0, and fails its upload on every single commit.
func TestVillageHooks_StatusReportsARenamedRepository(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	if out, err := runVillageHooks(t, "install", "--dir", repo, "--event", "post-commit"); err != nil {
		t.Fatalf("install: %v (out=%s)", err, out)
	}
	moved := filepath.Join(filepath.Dir(repo), "renamed")
	if err := os.Rename(repo, moved); err != nil {
		t.Fatalf("rename the repository: %v", err)
	}

	out, err := runVillageHooks(t, "status", "--dir", moved)
	if err != nil {
		t.Fatalf("status: %v (out=%s)", err, out)
	}
	for _, want := range []string{
		string(githooks.WarningRepositoryMoved),
		"was written for " + repo,
		"git resolves this repository as " + moved,
		githooks.InstallCommand(githooks.EventPostCommit, moved),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status must report the moved repository with %q; got:\n%s", want, out)
		}
	}
}
