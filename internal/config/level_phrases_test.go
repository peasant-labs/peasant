package config

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/redact"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/level_phrases.yaml
var levelPhraseFixtureData []byte

//go:embed testdata/level_phrases-reject-one-size-only.yaml
var levelPhraseRejectOneSizeOnlyData []byte

//go:embed testdata/level_phrases-reject-set-the-product-does-not-offer.yaml
var levelPhraseRejectProductSetMissingData []byte

const levelPhraseFixturePath = "internal/config/testdata/level_phrases.yaml"

// levelPhraseFixtureFloor is the row count the committed corpus must not fall below.
// It is a floor EQUAL to the current count rather than a hand-picked minimum:
// any slack between the two is rows that can be deleted in silence.
const levelPhraseFixtureFloor = 3

type levelPhraseDocument struct {
	ExpectedCaseCount int               `yaml:"expectedCaseCount"`
	Cases             []levelPhraseCase `yaml:"cases"`
}

type levelPhraseCase struct {
	Name            string                  `yaml:"name"`
	Levels          []redact.RedactionLevel `yaml:"levels"`
	ChoicePhrase    string                  `yaml:"choicePhrase"`
	OfferedSentence string                  `yaml:"offeredSentence"`
	OfferedClause   string                  `yaml:"offeredClause"`
}

// loadLevelPhraseFixture decodes and fully validates the corpus.
//
// Its coverage requirements are what make the corpus about the RENDERERS rather
// than about today's menu: a row for every size the menu can hold, and a row
// rendering the set the product actually offers. Without the first the corpus can
// only repeat what the singular hand-written sentences already said; without the
// second it describes hypothetical sets beside the product instead of it.
func loadLevelPhraseFixture(data []byte) (levelPhraseDocument, error) {
	var document levelPhraseDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, levelPhraseRuleError(
			"typed YAML fields must match the document schema",
			"loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, levelPhraseRuleError(
			"exactly one YAML document is allowed; cases below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, levelPhraseRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases present")
	}
	seen := map[string]bool{}
	coveredSizes := map[int]bool{}
	sawProductSet := false
	for index, testCase := range document.Cases {
		where := fmt.Sprintf("loader=case index %d", index)
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, levelPhraseRuleError(
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name), where,
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		if len(testCase.Levels) == 0 {
			return document, levelPhraseRuleError(
				fmt.Sprintf("case %q renders no levels at all", testCase.Name), where,
				"fix=name at least one level; an empty offered set is not a state this version can be in, and rendering one "+
					"would pin prose nobody can ever read")
		}
		for _, level := range testCase.Levels {
			if !level.IsValid() {
				return document, levelPhraseRuleError(
					fmt.Sprintf("case %q names %q, which the engine does not define", testCase.Name, level), where,
					"fix=use levels from redact.AllRedactionLevels")
			}
		}
		for label, want := range map[string]string{
			"choicePhrase":    testCase.ChoicePhrase,
			"offeredSentence": testCase.OfferedSentence,
			"offeredClause":   testCase.OfferedClause,
		} {
			if strings.TrimSpace(want) == "" {
				return document, levelPhraseRuleError(
					fmt.Sprintf("case %q leaves %s blank", testCase.Name, label), where,
					"fix=write the expected text; a blank expectation is satisfied by any output, including none")
			}
		}
		// The size is read from the row's own data, not from a label a corpus
		// could move onto another row.
		coveredSizes[len(testCase.Levels)] = true
		if slices.Equal(testCase.Levels, OfferedRedactionLevels) {
			sawProductSet = true
		}
	}
	// Every size the menu can be, walked from the engine's own enumeration: an
	// offered set is a subset of the levels that exist, so it holds between one
	// and all of them. Requiring "one" and "more than one" was not enough - two
	// rows above the singular covered for each other, so either was deletable in
	// silence, and the grammar at a size nobody renders is the grammar nobody has
	// seen. That is exactly the state the hand-written sentences were in: correct
	// at the shipped size, unexamined at every other.
	for size := 1; size <= len(redact.AllRedactionLevels()); size++ {
		if !coveredSizes[size] {
			return document, levelPhraseRuleError(
				fmt.Sprintf("no case renders a set of %d level(s)", size),
				"loader=size coverage",
				fmt.Sprintf("fix=add one; the menu can hold any number from one to %d, and prose tuned to the size that "+
					"happens to ship reads wrong the day it changes - which the disposition table exists to make possible",
					len(redact.AllRedactionLevels())))
		}
	}
	if !sawProductSet {
		return document, levelPhraseRuleError(
			fmt.Sprintf("no case renders the set this version actually offers (%v)", OfferedRedactionLevels),
			"loader=product-set coverage",
			"fix=add a case whose levels are exactly the offered set; without one the corpus describes hypothetical sets "+
				"beside the product rather than the text a user will read")
	}
	return document, nil
}

func levelPhraseRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"level phrase fixture rule failed: %s; a malformed or incomplete corpus invalidates the only evidence that the "+
			"remediation lines a user is told to follow are readable at any menu size; where=%s %s; "+
			"when=test fixture loading; impact=an actionable error's fix line could ship ungrammatical or wrong; %s",
		what, levelPhraseFixturePath, where, fix)
}

// --- loader guards ----------------------------------------------------------

func TestLoadLevelPhraseFixture_RejectsACorpusRenderingOnlyOneSize(t *testing.T) {
	t.Parallel()
	_, err := loadLevelPhraseFixture(levelPhraseRejectOneSizeOnlyData)
	if err == nil || !strings.Contains(err.Error(), "no case renders a set of 2 level(s)") {
		t.Fatalf("error = %v, want rejection of a corpus that never renders a widened menu", err)
	}
}

func TestLoadLevelPhraseFixture_RejectsACorpusThatSkipsTheProductsOwnSet(t *testing.T) {
	t.Parallel()
	_, err := loadLevelPhraseFixture(levelPhraseRejectProductSetMissingData)
	if err == nil || !strings.Contains(err.Error(), "no case renders the set this version actually offers") {
		t.Fatalf("error = %v, want rejection of a corpus with no row for the offered set itself", err)
	}
}

func TestLoadLevelPhraseFixture_RejectsABlankExpectation(t *testing.T) {
	t.Parallel()
	_, err := loadLevelPhraseFixture([]byte(`expectedCaseCount: 1
cases:
  - name: blank
    levels: [standard]
    choicePhrase: ""
    offeredSentence: the level this version offers is standard
    offeredClause: standard is now the single level this version offers
`))
	if err == nil || !strings.Contains(err.Error(), "leaves choicePhrase blank") {
		t.Fatalf("error = %v, want rejection of a blank expectation; any output satisfies one", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestRedactionLevelPhrases_AgreeInNumberAtEverySize drives the renderers over
// sets the product does not ship, and the exported wrappers over the set it does.
func TestRedactionLevelPhrases_AgreeInNumberAtEverySize(t *testing.T) {
	t.Parallel()
	document, err := loadLevelPhraseFixture(levelPhraseFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	// The FLOOR, asserted here rather than in the loader because the rejection
	// fixtures share that loader and are deliberately smaller. The declared count
	// alone is satisfied by deleting a row and decrementing it in the same edit;
	// the floor does not move with the corpus, so the count dropping is caught
	// even when the pair stays self-consistent. Coverage catches a row swapped for
	// a junk one, a floor catches the count dropping - different failures.
	if len(document.Cases) < levelPhraseFixtureFloor {
		t.Fatalf("the level phrase corpus holds %d cases, below the floor of %d. Restore the case, or lower the floor "+
			"deliberately and say in the fixture header which behaviour stopped being covered.",
			len(document.Cases), levelPhraseFixtureFloor)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			for label, got := range map[string]string{
				"choicePhrase":    levelChoicePhrase(testCase.Levels),
				"offeredSentence": offeredLevelsSentence(testCase.Levels),
				"offeredClause":   levelsClause(testCase.Levels),
			} {
				want := map[string]string{
					"choicePhrase":    testCase.ChoicePhrase,
					"offeredSentence": testCase.OfferedSentence,
					"offeredClause":   testCase.OfferedClause,
				}[label]
				if got != want {
					t.Errorf("%s(%v) = %q, want %q", label, testCase.Levels, got, want)
				}
			}

			// The row for the product's own set also pins the EXPORTED wrappers, so
			// the functions the messages actually call are covered rather than only
			// the helpers underneath them.
			if !slices.Equal(testCase.Levels, OfferedRedactionLevels) {
				return
			}
			if got := RedactionLevelChoicePhrase(); got != testCase.ChoicePhrase {
				t.Errorf("RedactionLevelChoicePhrase() = %q, want %q; this is the text a user is told to type", got, testCase.ChoicePhrase)
			}
			if got := OfferedRedactionLevelsSentence(); got != testCase.OfferedSentence {
				t.Errorf("OfferedRedactionLevelsSentence() = %q, want %q", got, testCase.OfferedSentence)
			}
			if got := offeredLevelsClause(); got != testCase.OfferedClause {
				t.Errorf("offeredLevelsClause() = %q, want %q", got, testCase.OfferedClause)
			}
		})
	}
}
