package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/redact"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/redact_level_refusals.yaml
var redactRefusalFixtureData []byte

//go:embed testdata/redact_level_refusals-reject-uncovered-disposition.yaml
var redactRefusalRejectUncoveredDispositionData []byte

const redactRefusalFixturePath = "cmd/peasant/testdata/redact_level_refusals.yaml"

// redactRefusalFixtureFloor is the row count the committed corpus must not fall below.
// It is a floor EQUAL to the current count rather than a hand-picked minimum:
// any slack between the two is rows that can be deleted in silence.
const redactRefusalFixtureFloor = 5

// refusalOutcome is what the command returns for a level.
type refusalOutcome string

const (
	// refusalNone lets the run proceed.
	refusalNone refusalOutcome = "none"
	// refusalUnsupported is the level this version cannot apply at all.
	refusalUnsupported refusalOutcome = "unsupported"
	// refusalUnoffered is the level it still applies from a configuration but
	// will not accept as a request.
	refusalUnoffered refusalOutcome = "unoffered"
)

// dispositionSpelling is the corpus's name for a config.RedactionLevelDisposition.
//
// Explicit lookup, not an integer: a missing or misspelled value would decode to
// 0, which is the fail-closed unknown arm, and the row would silently exercise
// that while reading as something else.
var dispositionSpelling = map[string]config.RedactionLevelDisposition{
	"unknown": config.RedactionLevelDispositionUnknown,
	"offered": config.RedactionLevelDispositionOffered,
	"raised":  config.RedactionLevelDispositionRaised,
	"refused": config.RedactionLevelDispositionRefused,
}

type redactRefusalDocument struct {
	ExpectedCaseCount int                 `yaml:"expectedCaseCount"`
	Cases             []redactRefusalCase `yaml:"cases"`
}

type redactRefusalCase struct {
	Name        string                `yaml:"name"`
	Level       redact.RedactionLevel `yaml:"level"`
	AskedByFlag bool                  `yaml:"askedByFlag"`
	Disposition string                `yaml:"disposition"`
	Refusal     refusalOutcome        `yaml:"refusal"`
	SourceNames string                `yaml:"sourceNames,omitempty"`
}

// loadRedactRefusalFixture decodes and fully validates the corpus.
func loadRedactRefusalFixture(data []byte) (redactRefusalDocument, error) {
	var document redactRefusalDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, redactRefusalRuleError(
			"typed YAML fields must match the document schema", "loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, redactRefusalRuleError(
			"exactly one YAML document is allowed; cases below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, redactRefusalRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases present")
	}
	seen := map[string]bool{}
	covered := map[config.RedactionLevelDisposition]bool{}
	sawBothOriginsForRaised := map[bool]bool{}
	for index, testCase := range document.Cases {
		where := fmt.Sprintf("loader=case index %d", index)
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, redactRefusalRuleError(
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name), where,
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		want, known := dispositionSpelling[testCase.Disposition]
		if !known {
			return document, redactRefusalRuleError(
				fmt.Sprintf("case %q names the disposition %q, which the policy does not define", testCase.Name, testCase.Disposition),
				where, "fix=use unknown, offered, raised, or refused; a blank or invented value decodes as the fail-closed "+
					"unknown arm while reading as a deliberate choice")
		}
		// The row states the disposition it means to exercise, and production is
		// asked whether that is the disposition the level actually has. Without
		// this a row could drift onto a different arm than the one it names and
		// still pass, which is how per-category coverage becomes decoration.
		if got := config.RedactionLevelDispositionOf(testCase.Level); got != want {
			return document, redactRefusalRuleError(
				fmt.Sprintf("case %q says the %q level is %q, but the policy says it is %q",
					testCase.Name, testCase.Level, testCase.Disposition, got),
				where, "fix=correct the row, or the policy table; a row exercising a different arm than the one it names "+
					"reports coverage of an arm nothing touches")
		}
		if !slices.Contains([]refusalOutcome{refusalNone, refusalUnsupported, refusalUnoffered}, testCase.Refusal) {
			return document, redactRefusalRuleError(
				fmt.Sprintf("case %q expects the outcome %q, which the command cannot produce", testCase.Name, testCase.Refusal),
				where, fmt.Sprintf("fix=use %s, %s, or %s", refusalNone, refusalUnsupported, refusalUnoffered))
		}
		if testCase.Refusal == refusalNone && testCase.SourceNames != "" {
			return document, redactRefusalRuleError(
				fmt.Sprintf("case %q proceeds but also pins refusal text %q", testCase.Name, testCase.SourceNames),
				where, "fix=drop sourceNames; a run that proceeds produces no refusal to read")
		}
		if testCase.Refusal != refusalNone && strings.TrimSpace(testCase.SourceNames) == "" {
			return document, redactRefusalRuleError(
				fmt.Sprintf("case %q refuses without pinning what the message names", testCase.Name),
				where, "fix=set sourceNames to what the user has to edit; a refusal that does not name its own source "+
					"leaves the reader hunting for which of the flag and the config file produced it")
		}
		covered[want] = true
		if want == config.RedactionLevelDispositionRaised {
			sawBothOriginsForRaised[testCase.AskedByFlag] = true
		}
	}
	// The closed set is walked from the policy's own enumeration, INCLUDING the
	// fail-closed unknown value, which no input reaches through the command.
	for _, disposition := range config.AllRedactionLevelDispositions {
		if !covered[disposition] {
			return document, redactRefusalRuleError(
				fmt.Sprintf("no case exercises the %q disposition", disposition),
				"loader=closed-set coverage",
				fmt.Sprintf("fix=add one; the %s arm can otherwise be deleted with every test still green, and the one "+
					"nothing can reach through today's inputs is the fail-closed arm - deleting it lets a level nobody "+
					"classified run at full strength", disposition))
		}
	}
	for _, byFlag := range []bool{true, false} {
		if !sawBothOriginsForRaised[byFlag] {
			return document, redactRefusalRuleError(
				fmt.Sprintf("no case reaches the raised disposition with askedByFlag=%v", byFlag),
				"loader=origin coverage",
				"fix=add one; raised is the single disposition where WHERE THE LEVEL CAME FROM changes the outcome rather "+
					"than only the wording, and a corpus carrying one origin cannot tell a command that distinguishes them "+
					"from one that does not")
		}
	}
	return document, nil
}

func redactRefusalRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"redact refusal fixture rule failed: %s; a malformed or incomplete corpus invalidates the only evidence that the "+
			"command refuses what it cannot honestly run; where=%s %s; when=test fixture loading; "+
			"impact=a level nobody classified could be applied at full strength, or a level a user typed could be "+
			"silently answered with a different one; %s",
		what, redactRefusalFixturePath, where, fix)
}

// --- loader guards ----------------------------------------------------------

func TestLoadRedactRefusalFixture_RejectsACorpusThatSkipsADisposition(t *testing.T) {
	t.Parallel()
	_, err := loadRedactRefusalFixture(redactRefusalRejectUncoveredDispositionData)
	if err == nil || !strings.Contains(err.Error(), `no case exercises the "unknown" disposition`) {
		t.Fatalf("error = %v, want rejection of a corpus with no unknown-disposition row; that arm is unreachable through "+
			"the command's own inputs, so a corpus is the only thing that can hold it", err)
	}
}

func TestLoadRedactRefusalFixture_RejectsARowThatNamesTheWrongArm(t *testing.T) {
	t.Parallel()
	_, err := loadRedactRefusalFixture([]byte(`expectedCaseCount: 1
cases:
  - name: standard-called-refused
    level: standard
    askedByFlag: false
    disposition: refused
    refusal: unsupported
    sourceNames: config
`))
	if err == nil || !strings.Contains(err.Error(), "but the policy says it is") {
		t.Fatalf("error = %v, want rejection of a row exercising a different arm than the one it names", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestRedactCmd_RefusesWhatItCannotHonestlyRun drives the production decision the
// redact command makes before it opens the store.
func TestRedactCmd_RefusesWhatItCannotHonestlyRun(t *testing.T) {
	t.Parallel()
	document, err := loadRedactRefusalFixture(redactRefusalFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	// The FLOOR, asserted here rather than in the loader because the rejection
	// fixtures share that loader and are deliberately smaller. The declared count
	// alone is satisfied by deleting a row and decrementing it in the same edit;
	// the floor does not move with the corpus, so the count dropping is caught
	// even when the pair stays self-consistent. Coverage catches a row swapped for
	// a junk one, a floor catches the count dropping - different failures.
	if len(document.Cases) < redactRefusalFixtureFloor {
		t.Fatalf("the redact refusal corpus holds %d cases, below the floor of %d. Restore the case, or lower the floor "+
			"deliberately and say in the fixture header which behaviour stopped being covered.",
			len(document.Cases), redactRefusalFixtureFloor)
	}
	const configSource = "config file /home/example/.config/peasant/config.yaml"
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			refusal := redactionLevelRefusal(testCase.Level, testCase.AskedByFlag, configSource)
			switch testCase.Refusal {
			case refusalNone:
				if refusal != nil {
					t.Fatalf("the run was refused for a level it must apply:\n%v", refusal)
				}
				return
			case refusalUnsupported:
				if !errors.Is(refusal, config.ErrUnsupportedRedactionLevel) {
					t.Fatalf("refusal = %v, want the unsupported-level refusal; the two refusals carry different remedies, "+
						"so answering with the wrong one tells the user to make a change that will not help", refusal)
				}
			case refusalUnoffered:
				if !errors.Is(refusal, config.ErrUnofferedRedactionLevel) {
					t.Fatalf("refusal = %v, want the no-longer-offered refusal. Answering with the unsupported one would "+
						"tell the user the level cannot be applied, when in fact it still runs from a configuration - the "+
						"whole reason the two exist separately", refusal)
				}
			}
			message := refusal.Error()
			if !strings.Contains(message, testCase.SourceNames) {
				t.Errorf("the refusal does not name %q, so the user cannot tell which of the flag and the configuration "+
					"file produced it:\n%s", testCase.SourceNames, message)
			}
			if !strings.Contains(message, testCase.Level.String()) {
				t.Errorf("the refusal never quotes the level %q back to the user:\n%s", testCase.Level, message)
			}
		})
	}
}
