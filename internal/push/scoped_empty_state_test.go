package push_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/scoped_empty_state.yaml
var scopedEmptyStateFixtureData []byte

const scopedEmptyStateFixturePath = "internal/push/testdata/scoped_empty_state.yaml"

// Shared identifiers for the scoped-empty-state world. The recorded
// subdirectory exists so a test can prove the one-line form does NOT enumerate
// the directories a scope admits, which is where the kilobytes came from.
const (
	scopedRepositoryRoot       = "/repo/in-scope"
	scopedRecordedSubdirectory = "/repo/in-scope/services/api"
	scopedGitRemote            = "git@github.com:user/in-scope.git"
	// The stale world: a directory that still exists inside this worktree whose
	// sessions carry an identity the scope does not admit. It is the only
	// evidence that makes a re-ingest safe to recommend, and the session it
	// names is what makes the scoped form of that command runnable.
	scopedStaleDirectory   = "/repo/in-scope/services/legacy"
	scopedStaleProjectHash = "3333333333333333333333333333333333333333333333333333333333333333"
	scopedStaleSessionID   = "33333333-3333-4333-8333-333333333333"
)

// scopedWorld is what the scope's own recorded sessions look like. The three
// states need three different remedies, which is the whole point: they used to
// collapse into one message decided by unrelated projects' pending work.
type scopedWorld string

const (
	// worldNothingRecorded: no session carries this repository's identity.
	worldNothingRecorded scopedWorld = "nothing-recorded"
	// worldAllPublished: sessions are recorded here and none changed since.
	worldAllPublished scopedWorld = "all-published"
	// worldSelectionExcluded: sessions are recorded here and the configured
	// selection removed every one of them.
	worldSelectionExcluded scopedWorld = "selection-excluded"
)

var allScopedWorlds = [...]scopedWorld{worldNothingRecorded, worldAllPublished, worldSelectionExcluded}

type scopedEmptyStateDocument struct {
	ExpectedCaseCount int                    `yaml:"expectedCaseCount"`
	Cases             []scopedEmptyStateCase `yaml:"cases"`
}

type scopedEmptyStateCase struct {
	Name         string      `yaml:"name"`
	World        scopedWorld `yaml:"world"`
	OtherPending bool        `yaml:"otherPending"`
	// StaleIdentity seeds work recorded inside this repository that the scope
	// cannot reach. It decides whether a re-ingest may be recommended at all:
	// with no such evidence the recommendation is destructive on a repository
	// that has been moved, which is the state that produces the same symptom.
	StaleIdentity  bool     `yaml:"staleIdentity"`
	MustContain    []string `yaml:"mustContain"`
	MustNotContain []string `yaml:"mustNotContain"`
}

// loadScopedEmptyStateFixture decodes and fully validates the corpus.
func loadScopedEmptyStateFixture(data []byte) (scopedEmptyStateDocument, error) {
	var document scopedEmptyStateDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, scopedEmptyStateRuleError(
			"typed YAML fields must match the document schema", "loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return document, scopedEmptyStateRuleError(
			"exactly one YAML document is allowed; trailing data is silently ignored", "loader=end-of-document check",
			"fix=remove the second document so the next decode returns EOF")
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, scopedEmptyStateRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases present")
	}
	seen := make(map[scopedWorld]bool, len(document.Cases))
	names := make(map[string]bool, len(document.Cases))
	for _, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || names[testCase.Name] {
			return document, scopedEmptyStateRuleError(
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name), "loader=case naming",
				"fix=name every case after the behaviour it proves, uniquely")
		}
		names[testCase.Name] = true
		if !containsScopedWorld(allScopedWorlds[:], testCase.World) {
			return document, scopedEmptyStateRuleError(
				fmt.Sprintf("unsupported world %q", testCase.World), "loader=world validation",
				"fix=use nothing-recorded, all-published, or selection-excluded")
		}
		seen[testCase.World] = true
		if len(testCase.MustContain) == 0 {
			return document, scopedEmptyStateRuleError(
				fmt.Sprintf("case %q states nothing the message has to carry", testCase.Name), "loader=assertion validation",
				"fix=state the facts this state's wording must carry, or the case asserts nothing")
		}
		for _, forbidden := range testCase.MustNotContain {
			for _, required := range testCase.MustContain {
				if strings.Contains(required, forbidden) {
					return document, scopedEmptyStateRuleError(
						fmt.Sprintf("case %q both requires and forbids %q", testCase.Name, forbidden),
						"loader=assertion validation",
						"fix=decide which one it is; a phrase in both lists can never pass")
				}
			}
		}
	}
	// Every state must be covered: the defect was one state borrowing another's
	// wording, which a corpus missing a state cannot catch.
	for _, world := range allScopedWorlds {
		if !seen[world] {
			return document, scopedEmptyStateRuleError(
				fmt.Sprintf("no case covers the %q state", world), "loader=coverage validation",
				"fix=add a case for it; an uncovered state is exactly where one message borrows another's wording")
		}
	}
	// Both sides of the evidence gate must be covered. A corpus that only ever
	// seeds stale work proves the diagnosis appears but never that it stays
	// away, and a corpus that never seeds it proves the opposite — and the
	// destructive defect was the recommendation printing with no evidence.
	var withStale, withoutStale bool
	for _, testCase := range document.Cases {
		if testCase.StaleIdentity {
			withStale = true
		} else {
			withoutStale = true
		}
	}
	if !withStale || !withoutStale {
		return document, scopedEmptyStateRuleError(
			fmt.Sprintf("the re-ingest recommendation needs a case on each side of its evidence gate, got staleIdentity=true:%v false:%v",
				withStale, withoutStale),
			"loader=coverage validation",
			"fix=add the missing case; recommending a re-ingest without evidence loses sessions on a moved repository")
	}
	return document, nil
}

func scopedEmptyStateRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"scoped empty-state fixture rule failed: %s; a malformed corpus invalidates the only evidence that a hook's "+
			"per-commit line tells the truth; where=%s %s; when=test fixture loading; "+
			"impact=what a repository-scoped push says when it sent nothing cannot be trusted; %s",
		what, scopedEmptyStateFixturePath, where, fix)
}

func containsScopedWorld(worlds []scopedWorld, want scopedWorld) bool {
	for _, world := range worlds {
		if world == want {
			return true
		}
	}
	return false
}
