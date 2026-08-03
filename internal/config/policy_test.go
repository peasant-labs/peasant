package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/policy.yaml
var policyFixtureData []byte

// The rejection fixtures each hold a full corpus with exactly ONE thing wrong, so
// the evidence that the loader is strict lives beside the corpus it protects
// rather than as a string literal in a test body. Each one names the guard it
// proves in its own header.
var (
	//go:embed testdata/policy-reject-raised-downward.yaml
	policyRejectRaisedDownwardData []byte
	//go:embed testdata/policy-reject-offered-rewritten.yaml
	policyRejectOfferedRewrittenData []byte
	//go:embed testdata/policy-reject-missing-disposition-arm.yaml
	policyRejectMissingDispositionArmData []byte
	//go:embed testdata/policy-reject-refused-with-resolution.yaml
	policyRejectRefusedWithResolutionData []byte
	//go:embed testdata/policy-reject-unknown-disposition.yaml
	policyRejectUnknownDispositionData []byte
	//go:embed testdata/policy-reject-uncovered-visibility.yaml
	policyRejectUncoveredVisibilityData []byte
	//go:embed testdata/policy-reject-uncovered-level.yaml
	policyRejectUncoveredLevelData []byte
	//go:embed testdata/policy-reject-uncovered-unset-level.yaml
	policyRejectUncoveredUnsetLevelData []byte
)

// configurableRedactionInputs is the closed set of values a configuration's
// redaction level can actually hold: every level the engine defines, plus the
// UNSET one.
//
// The unset value is a member because it is a real input with its own branch in
// RedactionLevelDispositionOf, not because a corpus happens to carry a row for it.
// Deriving it here rather than listing rows is what makes the row undeletable.
func configurableRedactionInputs() []redact.RedactionLevel {
	return append(redact.AllRedactionLevels(), "")
}

const policyFixturePath = "internal/config/testdata/policy.yaml"

// expectedOverclaimCount anchors the shared over-claim list's size from
// THIS package.
//
// A forbid-list cannot be anchored the way a case corpus is: the phrases are
// absent by design, so removing one changes no behaviour and fails nothing -
// measured, all nine deleted green. Both consumers carry their own count, so
// retiring a phrase takes an edit in three files across three packages rather
// than one line of YAML. It does not make removal hard; it makes it visible.
const expectedOverclaimCount = 9

type policyDocument struct {
	ExpectedRedactionCaseCount  int                    `yaml:"expectedRedactionCaseCount"`
	Redaction                   []redactionPolicyCase  `yaml:"redaction"`
	ExpectedVisibilityCaseCount int                    `yaml:"expectedVisibilityCaseCount"`
	Visibility                  []visibilityPolicyCase `yaml:"visibility"`
}

// dispositionName is the fixture's spelling of a RedactionLevelDisposition.
//
// It is its own type with an explicit lookup rather than an int the YAML could
// carry directly, because a numeric disposition in a corpus is unreadable and,
// worse, a missing or misspelled one would decode to 0 - which is
// RedactionLevelDispositionUnknown, a real arm. A case would then silently test
// the fail-closed path while reading as though it tested something else.
type dispositionName string

const (
	dispositionOffered dispositionName = "offered"
	dispositionRaised  dispositionName = "raised"
	dispositionRefused dispositionName = "refused"
)

// fixtureDispositions maps every disposition a corpus may name to the production
// value it must equal. RedactionLevelDispositionUnknown is deliberately absent: it
// is what an unhandled level resolves to, not a state a configuration can be in,
// and a corpus that could name it would be asserting that a level nobody taught
// this version about is an expected input.
var fixtureDispositions = map[dispositionName]RedactionLevelDisposition{
	dispositionOffered: RedactionLevelDispositionOffered,
	dispositionRaised:  RedactionLevelDispositionRaised,
	dispositionRefused: RedactionLevelDispositionRefused,
}

type redactionPolicyCase struct {
	Name        string                `yaml:"name"`
	Configured  redact.RedactionLevel `yaml:"configured"`
	Disposition dispositionName       `yaml:"disposition"`
	Effective   redact.RedactionLevel `yaml:"effective,omitempty"`
}

type visibilityPolicyCase struct {
	Name       string     `yaml:"name"`
	Configured Visibility `yaml:"configured"`
	Effective  Visibility `yaml:"effective"`
	Downgraded bool       `yaml:"downgraded"`
}

// loadPolicyFixture decodes and fully validates the corpus.
//
// The closed-set guards are the point of this loader: the two corpora must cover
// every redaction level and every contract visibility. A resolution written
// against one named member instead of the closed set passes for the member it
// names and lies about the rest, which is exactly how a configured group
// visibility kept being announced as applied. A corpus that no longer covers the
// whole set cannot detect that, so a missing row is a load failure, not a gap.
func loadPolicyFixture(data []byte) (policyDocument, error) {
	var document policyDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, policyFixtureError(
			"typed YAML fields must match the document schema",
			"loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, policyFixtureError(
			"exactly one YAML document is allowed; cases below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(document.Redaction) == 0 || document.ExpectedRedactionCaseCount != len(document.Redaction) {
		return document, policyFixtureError(
			fmt.Sprintf("declared and actual redaction case counts must match and be non-zero, got expectedRedactionCaseCount=%d cases=%d",
				document.ExpectedRedactionCaseCount, len(document.Redaction)),
			"loader=redaction case-count validation",
			"fix=set expectedRedactionCaseCount to the number of redaction cases present")
	}
	if len(document.Visibility) == 0 || document.ExpectedVisibilityCaseCount != len(document.Visibility) {
		return document, policyFixtureError(
			fmt.Sprintf("declared and actual visibility case counts must match and be non-zero, got expectedVisibilityCaseCount=%d cases=%d",
				document.ExpectedVisibilityCaseCount, len(document.Visibility)),
			"loader=visibility case-count validation",
			"fix=set expectedVisibilityCaseCount to the number of visibility cases present")
	}
	seen := map[string]bool{}
	var coveredLevels []redact.RedactionLevel
	coveredDispositions := map[dispositionName]bool{}
	for index, testCase := range document.Redaction {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, policyFixtureError(
				fmt.Sprintf("redaction case name %q is missing or duplicated", testCase.Name),
				fmt.Sprintf("loader=redaction case index %d", index),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		// An unset configured level is a real input - it is what a zero-value
		// config.Config carries - so "" is allowed here and nowhere else. It is NOT
		// what config.Load produces from an install with no redaction block: Parse
		// starts from BaseConfig, which writes the default level, and validate
		// rejects an invalid one, so a loaded configuration can never be unset.
		if testCase.Configured != "" && !testCase.Configured.IsValid() {
			return document, policyFixtureError(
				fmt.Sprintf("redaction case %q has an unknown configured level %q", testCase.Name, testCase.Configured),
				fmt.Sprintf("loader=redaction case index %d", index),
				"fix=use one of minimal, standard, maximum, or \"\" for an unset level")
		}
		want, known := fixtureDispositions[testCase.Disposition]
		if !known {
			return document, policyFixtureError(
				fmt.Sprintf("redaction case %q names the disposition %q, which the policy does not define",
					testCase.Name, testCase.Disposition),
				fmt.Sprintf("loader=redaction case index %d", index),
				fmt.Sprintf("fix=use one of %s, %s, %s; a blank or invented value would decode as the fail-closed unknown arm "+
					"while reading as a deliberate choice", dispositionOffered, dispositionRaised, dispositionRefused))
		}
		coveredDispositions[testCase.Disposition] = true
		switch want {
		case RedactionLevelDispositionRefused:
			// A refused level stops the caller before any level is resolved, so a
			// row that also pins a resolution is describing a run that cannot
			// happen - and would bless resolving one.
			if testCase.Effective != "" {
				return document, policyFixtureError(
					fmt.Sprintf("redaction case %q is refused but also pins effective %q", testCase.Name, testCase.Effective),
					fmt.Sprintf("loader=redaction case index %d", index),
					"fix=drop effective; a refused level refuses instead of resolving")
			}
		case RedactionLevelDispositionOffered:
			// Offered means applied AS CONFIGURED. A row claiming otherwise would
			// let a silent substitution read as intended behaviour. An unset
			// configured level is the one exception: it resolves to the default.
			if !testCase.Effective.IsValid() {
				return document, policyFixtureError(
					fmt.Sprintf("redaction case %q is offered but has an unknown effective level %q", testCase.Name, testCase.Effective),
					fmt.Sprintf("loader=redaction case index %d", index),
					"fix=pin effective to the level the run applies")
			}
			if testCase.Configured != "" && testCase.Effective != testCase.Configured {
				return document, policyFixtureError(
					fmt.Sprintf("redaction case %q is offered but claims %q runs as %q",
						testCase.Name, testCase.Configured, testCase.Effective),
					fmt.Sprintf("loader=redaction case index %d", index),
					"fix=set effective to the configured level, or change the disposition to raised; an offered level that "+
						"runs as something else is the silent substitution this package exists to prevent")
			}
		case RedactionLevelDispositionRaised:
			if !testCase.Effective.IsValid() {
				return document, policyFixtureError(
					fmt.Sprintf("redaction case %q is raised but has an unknown effective level %q", testCase.Name, testCase.Effective),
					fmt.Sprintf("loader=redaction case index %d", index),
					"fix=pin effective to the level the run applies instead")
			}
			// The DIRECTION guard, and the whole safety argument for raising rather
			// than refusing: the substitute must redact strictly MORE. A corpus
			// asserting a downward transition would ratify the one direction that
			// can publish content at less protection than the user chose.
			if testCase.Effective.Ord() <= testCase.Configured.Ord() {
				return document, policyFixtureError(
					fmt.Sprintf("redaction case %q raises %q to %q, which does not redact more", testCase.Name,
						testCase.Configured, testCase.Effective),
					fmt.Sprintf("loader=redaction case index %d", index),
					"fix=raise only to a stricter level; a level substituted downward protects less than the user configured, "+
						"which is a refusal case, not a transition")
			}
		}
		coveredLevels = append(coveredLevels, testCase.Configured)
	}
	// The closed set is walked from the engine's own enumeration PLUS the unset
	// input, so a level added to the redaction module fails here rather than shipping with no
	// disposition at all - and so does the deletion of the unset row.
	//
	// The unset row belongs in the walk rather than beside it. It was not in the
	// set, because "" is not a member of redact.AllRedactionLevels, so deleting it
	// removed the ONLY coverage of RedactionLevelDispositionOf's unset branch and
	// nothing anywhere noticed. That branch decides whether a zero-value
	// configuration may run at all: with it broken, `peasant village push` refuses
	// outright for every such install, and internal/api cannot catch it either -
	// its own omitted-level path resolves "" through IsValid() before any
	// disposition is asked for.
	for _, level := range configurableRedactionInputs() {
		if !slices.Contains(coveredLevels, level) {
			return document, policyFixtureError(
				fmt.Sprintf("no redaction case configures the %q level", level),
				"loader=closed-set coverage",
				"fix=add a case for it; an uncovered level is a level whose resolution nobody checks, and the unset one is "+
					"the input every zero-value configuration carries")
		}
	}
	for _, disposition := range []dispositionName{dispositionOffered, dispositionRaised, dispositionRefused} {
		if !coveredDispositions[disposition] {
			return document, policyFixtureError(
				fmt.Sprintf("no redaction case is %q", disposition),
				"loader=disposition coverage",
				fmt.Sprintf("fix=add one; with no %s row nothing proves that arm still behaves, and it could be replaced by "+
					"another arm unnoticed", disposition))
		}
	}
	var coveredVisibilities []Visibility
	for index, testCase := range document.Visibility {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, policyFixtureError(
				fmt.Sprintf("visibility case name %q is missing or duplicated", testCase.Name),
				fmt.Sprintf("loader=visibility case index %d", index),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		if !testCase.Configured.IsValid() || !testCase.Effective.IsValid() {
			return document, policyFixtureError(
				fmt.Sprintf("visibility case %q names a visibility the contract does not define", testCase.Name),
				fmt.Sprintf("loader=visibility case index %d", index),
				fmt.Sprintf("fix=use one of %s", VisibilityMenu()))
		}
		coveredVisibilities = append(coveredVisibilities, testCase.Configured)
	}
	for _, visibility := range schema.AllVisibilities {
		if !slices.Contains(coveredVisibilities, visibility) {
			return document, policyFixtureError(
				fmt.Sprintf("no visibility case configures %q", visibility),
				"loader=closed-set coverage",
				"fix=add a case for it; the contract gained a visibility and nothing proves what this version does with it")
		}
	}
	return document, nil
}

func policyFixtureError(what, where, fix string) error {
	return fmt.Errorf(
		"effective-policy fixture rule failed: %s; a malformed or incomplete corpus invalidates the only evidence that this "+
			"version discloses what it actually applies; where=%s %s; when=test fixture loading; "+
			"impact=a setting could be announced as applied while something else happens; %s",
		what, policyFixturePath, where, fix)
}

// --- loader guards ----------------------------------------------------------

func TestLoadPolicyFixture_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := loadPolicyFixture([]byte("expectedRedactionCaseCount: 1\nsomethingElse: true\n"))
	if err == nil || !strings.Contains(err.Error(), "typed YAML fields must match") {
		t.Fatalf("error = %v, want rejection of an unknown field", err)
	}
}

func TestLoadPolicyFixture_RejectsAnUncoveredVisibility(t *testing.T) {
	t.Parallel()
	_, err := loadPolicyFixture(policyRejectUncoveredVisibilityData)
	if err == nil || !strings.Contains(err.Error(), `no visibility case configures "group"`) {
		t.Fatalf("error = %v, want rejection of a corpus that leaves a contract visibility uncovered", err)
	}
}

func TestLoadPolicyFixture_RejectsAnUncoveredRedactionLevel(t *testing.T) {
	t.Parallel()
	_, err := loadPolicyFixture(policyRejectUncoveredLevelData)
	if err == nil || !strings.Contains(err.Error(), `no redaction case configures the "minimal" level`) {
		t.Fatalf("error = %v, want rejection of a corpus that leaves a redaction level uncovered", err)
	}
}

func TestLoadPolicyFixture_RejectsACorpusWithNoUnsetLevelCase(t *testing.T) {
	t.Parallel()
	_, err := loadPolicyFixture(policyRejectUncoveredUnsetLevelData)
	if err == nil || !strings.Contains(err.Error(), `no redaction case configures the "" level`) {
		t.Fatalf("error = %v, want rejection of a corpus with no unset-level row. That row is the only cover for the "+
			"branch deciding whether a zero-value configuration may run at all, and it used to be deletable in silence: "+
			"\"\" is not a member of redact.AllRedactionLevels, so the closed-set walk never asked for it", err)
	}
}

// The five disposition guards each get their own rejection fixture rather than an
// inline string, so the corpus that proves the loader strict sits beside the corpus
// it protects and its header can explain what it is for.

func TestLoadPolicyFixture_RejectsARaisedCaseThatRedactsLess(t *testing.T) {
	t.Parallel()
	_, err := loadPolicyFixture(policyRejectRaisedDownwardData)
	if err == nil || !strings.Contains(err.Error(), "which does not redact more") {
		t.Fatalf("error = %v, want rejection of a transition that substitutes a WEAKER level; that direction publishes "+
			"content at less protection than the user chose, and a corpus must not be able to ratify it", err)
	}
}

func TestLoadPolicyFixture_RejectsAnOfferedCaseThatRunsAsSomethingElse(t *testing.T) {
	t.Parallel()
	_, err := loadPolicyFixture(policyRejectOfferedRewrittenData)
	if err == nil || !strings.Contains(err.Error(), "runs as") {
		t.Fatalf("error = %v, want rejection of an offered level pinned to a different effective level", err)
	}
}

func TestLoadPolicyFixture_RejectsACorpusMissingADispositionArm(t *testing.T) {
	t.Parallel()
	_, err := loadPolicyFixture(policyRejectMissingDispositionArmData)
	if err == nil || !strings.Contains(err.Error(), `no redaction case is "raised"`) {
		t.Fatalf("error = %v, want rejection of a corpus with no raised row; without one nothing proves an existing "+
			"minimal configuration keeps working rather than dead-ending", err)
	}
}

func TestLoadPolicyFixture_RejectsARefusedCaseThatAlsoResolves(t *testing.T) {
	t.Parallel()
	_, err := loadPolicyFixture(policyRejectRefusedWithResolutionData)
	if err == nil || !strings.Contains(err.Error(), "is refused but also pins effective") {
		t.Fatalf("error = %v, want rejection of a refused level that also pins a resolution", err)
	}
}

func TestLoadPolicyFixture_RejectsAnUndefinedDisposition(t *testing.T) {
	t.Parallel()
	_, err := loadPolicyFixture(policyRejectUnknownDispositionData)
	if err == nil || !strings.Contains(err.Error(), "which the policy does not define") {
		t.Fatalf("error = %v, want rejection of an invented disposition; a blank or misspelled one decodes to the "+
			"fail-closed unknown arm while reading as deliberate", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestRedactionLevelPolicy_DisposesOfEveryLevelTheEngineDefines drives the
// resolution every production path consumes: the two ingest checks, the redact
// command, both sync handler request paths, push, and the hooks surface.
func TestRedactionLevelPolicy_DisposesOfEveryLevelTheEngineDefines(t *testing.T) {
	t.Parallel()
	document, err := loadPolicyFixture(policyFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Redaction {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			want := fixtureDispositions[testCase.Disposition]
			if got := RedactionLevelDispositionOf(testCase.Configured); got != want {
				t.Fatalf("RedactionLevelDispositionOf(%q) = %v, want %v", testCase.Configured, got, want)
			}

			// The two predicates the surfaces actually call must follow from the
			// disposition rather than being maintained beside it. RedactionLevelOffered
			// is the check for a level a caller asked for; RedactionLevelSupported is
			// the check for a level a configuration merely carries.
			wantOffered := want == RedactionLevelDispositionOffered
			if got := RedactionLevelOffered(testCase.Configured); got != wantOffered {
				t.Errorf("RedactionLevelOffered(%q) = %v, want %v", testCase.Configured, got, wantOffered)
			}
			wantSupported := want == RedactionLevelDispositionOffered || want == RedactionLevelDispositionRaised
			if got := RedactionLevelSupported(testCase.Configured); got != wantSupported {
				t.Errorf("RedactionLevelSupported(%q) = %v, want %v", testCase.Configured, got, wantSupported)
			}

			if want == RedactionLevelDispositionRefused {
				assertRefusalIsActionable(t, testCase.Configured)
				// A refused level must not also be reachable as a request: the two
				// refusals differ in wording, and offering it here would hand a
				// caller a level every command then stops on.
				if RedactionLevelOffered(testCase.Configured) {
					t.Errorf("the refused level %q is still offered to callers", testCase.Configured)
				}
				assertRefusedLevelResolvesToNothing(t, testCase.Configured)
				return
			}

			policy := ResolveRedactionPolicy(testCase.Configured)
			if policy.Effective != testCase.Effective {
				t.Errorf("ResolveRedactionPolicy(%q).Effective = %q, want %q",
					testCase.Configured, policy.Effective, testCase.Effective)
			}
			// The resolved level must itself be one a user could have chosen.
			// Resolving to something unofferable would mean the product runs at a
			// level it will not let anyone select.
			if !RedactionLevelOffered(policy.Effective) {
				t.Errorf("ResolveRedactionPolicy(%q) resolved to %q, which is not an offered level",
					testCase.Configured, policy.Effective)
			}
			wantRaised := want == RedactionLevelDispositionRaised
			if policy.Raised() != wantRaised {
				t.Errorf("ResolveRedactionPolicy(%q).Raised() = %v, want %v", testCase.Configured, policy.Raised(), wantRaised)
			}
			assertRedactionTransitionDisclosure(t, policy, testCase)

			// THE SETTLING PROPERTY: whatever a transition resolves TO must itself
			// resolve with nothing further to disclose, or a saved configuration
			// would keep printing the notice forever.
			//
			// This used to be asserted by calling ConfigRedactionTransition.Apply,
			// an exported method with no production caller kept against a save path
			// that might adopt it. The property does not need it - it is a
			// statement about the resolver, and the level a save would write is
			// policy.Effective whether or not a method exists to write it. The
			// method is gone; the invariant is not.
			if settled := ResolveRedactionPolicy(policy.Effective); settled.Raised() {
				t.Errorf("re-resolving the level a save would store (%q) still reports a transition to %q, so the notice "+
					"would never stop", policy.Effective, settled.Effective)
			}
		})
	}
}

// assertRefusedLevelResolvesToNothing holds the resolver to refusing rather than
// substituting, for every level the corpus says is refused.
//
// It exists because of what the substitution actually said. The resolver used to
// fall back to the recommended level when nothing offered was at least as strict,
// and then build a transition from it - so a level that could only be replaced by
// a WEAKER one produced the disclosure "this run redacts at X instead, which
// redacts MORE than you had configured; nothing is protected less". Both halves
// backwards, in the one direction this file exists to prevent, and phrased
// confidently enough that a user would not question it.
//
// Callers check RedactionLevelSupported first, so nothing reaches the resolver
// this way today. That is exactly the condition under which a fallback rots: the
// fifth caller added later inherits it, and the failure is not a crash but a
// reassuring false statement.
func assertRefusedLevelResolvesToNothing(t *testing.T, configured redact.RedactionLevel) {
	t.Helper()
	policy := ResolveRedactionPolicy(configured)
	if policy.Effective != "" {
		t.Errorf("ResolveRedactionPolicy(%q) resolved to %q instead of refusing. Every offered level redacts LESS than a "+
			"refused one, so a substitute here publishes content at less protection than the user chose - and the "+
			"disclosure would announce it as MORE. The unset level is what makes a caller that skipped its own check fail "+
			"at the point of use rather than run at a level nobody chose.", configured, policy.Effective)
	}
	if policy.Raised() || policy.Disclosure() != "" || policy.BriefDisclosure() != "" {
		t.Errorf("ResolveRedactionPolicy(%q) discloses a transition for a level that cannot be applied at all:\n%s",
			configured, policy.Disclosure())
	}
}

// TestConfigRedactionTransition_ReportsNothingForADownwardPair holds the type's
// own documented invariant - "To is always the stricter of the two" - to the code
// rather than to the comment.
//
// The resolver cannot build such a pair any more, so this is about the TYPE: a
// transition assembled anywhere else, by a future caller, must not be able to
// produce Disclosure's claim that the substitute redacts more.
func TestConfigRedactionTransition_ReportsNothingForADownwardPair(t *testing.T) {
	t.Parallel()
	downward := ConfigRedactionTransition{From: redact.Maximum, To: redact.Minimal}
	if downward.Occurred() {
		t.Errorf("a transition from %q to %q reports as a transition; Disclosure would then tell the user the run "+
			"redacts MORE and that nothing is protected less, which is false in the one direction that matters",
			downward.From, downward.To)
	}
	if downward.Disclosure() != "" || downward.BriefDisclosure() != "" {
		t.Errorf("a downward transition renders a disclosure:\n%s", downward.Disclosure())
	}
	// There used to be a third assertion here, that Apply left the configuration
	// alone for a downward pair. Apply is gone - an exported method with no
	// production caller - and its guard was Occurred(), which is asserted above.
	// Nothing that persists a transition can act on this pair, because nothing
	// that reads one gets past the first check.
}

// assertRedactionTransitionDisclosure holds the transition to the same contract
// the visibility downgrade obeys: a change is always disclosed, in both the full
// and the one-line form, naming what was configured and what happens instead;
// anything else is silent. The extra requirement here is the DIRECTION - the text
// has to say the substitute redacts more, because a user reading "your setting was
// changed" needs to know it was changed towards safety.
func assertRedactionTransitionDisclosure(t *testing.T, policy EffectiveRedactionPolicy, testCase redactionPolicyCase) {
	t.Helper()
	full, brief := policy.Disclosure(), policy.BriefDisclosure()
	if !policy.Raised() {
		if full != "" || brief != "" {
			t.Errorf("nothing was raised, so nothing may be disclosed; got full=%q brief=%q", full, brief)
		}
		return
	}
	for label, text := range map[string]string{"Disclosure()": full, "BriefDisclosure()": brief} {
		if text == "" {
			t.Fatalf("%s is empty for a raised level; the user would never learn the configured level was not applied", label)
		}
		for _, want := range []string{
			testCase.Configured.String(), // what they configured
			policy.Effective.String(),    // what actually runs
			"no longer offer",            // why
			"nothing to do",              // that no action is required
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s must state %q so the user can see what they set, what happens, and that no action is needed; got:\n%s",
					label, want, text)
			}
		}
		// The DIRECTION, matched case-insensitively because the full form emphasises
		// it in capitals and the one-line form does not. Without this the notice
		// reads as "your setting was ignored" rather than "your setting was made
		// safer", which is the difference between alarming a user and informing one.
		if !strings.Contains(strings.ToLower(text), "redacts more") {
			t.Errorf("%s must say the substitute redacts MORE, or the notice reads as protection lost rather than added; got:\n%s",
				label, text)
		}
		if strings.ContainsRune(text, '—') {
			t.Errorf("%s must stay ASCII: it is printed to stderr from a Git hook; got:\n%s", label, text)
		}
	}
}

// TestUnofferedRedactionLevelError_IsActionableAndNamesTheRightRemedy holds the
// direct-request refusal to the project's error contract.
//
// It is a separate refusal from the unsupported one on purpose, and the risk of
// having two is that they drift into saying the same thing - at which point the
// distinction is decoration. So this asserts what only THIS one may say: that the
// level still works in a stored configuration, which is exactly the fact that
// makes refusing the request rather than the configuration coherent.
func TestUnofferedRedactionLevelError_IsActionableAndNamesTheRightRemedy(t *testing.T) {
	t.Parallel()
	raised := levelsWithDisposition(RedactionLevelDispositionRaised)
	if len(raised) == 0 {
		t.Skip("no level is currently raised, so there is no direct-request refusal to hold to the contract")
	}
	for _, level := range raised {
		refusal := &UnofferedRedactionLevelError{
			Level:     level,
			Source:    "the --level flag",
			Operation: "peasant redact",
			Step:      "before any stored transcript was read or rewritten",
			Impact:    "No transcript was changed.",
		}
		if !errors.Is(refusal, ErrUnofferedRedactionLevel) {
			t.Error("the refusal must classify as ErrUnofferedRedactionLevel so handlers can map it to a client error")
		}
		// It must NOT classify as the unsupported refusal: a caller distinguishing
		// "this version cannot do that" from "that is no longer a choice" would
		// otherwise get the wrong answer.
		if errors.Is(refusal, ErrUnsupportedRedactionLevel) {
			t.Error("a no-longer-offered level must not classify as unsupported; the level works, it is just not a choice")
		}
		message := refusal.Error()
		for _, want := range []string{
			level.String(),                     // what was refused
			"no longer offered",                // what went wrong
			"peasant redact",                   // where
			"the --level flag",                 // which input to change
			"before any stored transcript",     // when
			"No transcript was changed",        // what it means for the caller
			"keeps working",                    // that an existing config is not broken
			RecommendedRedactionLevel.String(), // the value to use instead
			RedactionLevelMenu(),               // every value that works
			"KNOWN PATTERNS",                   // the hedge: what redaction can and cannot promise
		} {
			if !strings.Contains(message, want) {
				t.Errorf("the refusal must state %q; got:\n%s", want, message)
			}
		}
		if strings.Contains(RedactionLevelMenu(), level.String()) {
			t.Errorf("the offered menu %q names the refused level %q", RedactionLevelMenu(), level)
		}
		if strings.ContainsRune(message, '—') {
			t.Errorf("the refusal must stay ASCII: it is printed to stderr from a Git hook; got:\n%s", message)
		}
	}
}

// TestOfferedRedactionLevels_IsExactlyWhatTheDispositionTableSays is the guard
// that the derived sets stay derived.
//
// The sets are what every user-facing menu, flag help string, and remediation
// sentence is built from. If one were ever hand-maintained beside the table, a
// level could be removed from the table and keep being offered - which is the
// defect this whole change exists to close, reappearing one level further out.
//
// HONEST LIMIT OF MOST OF WHAT FOLLOWS. OfferedRedactionLevels and
// SupportedRedactionLevels are COMPUTED from redactionLevelDispositions, so
// comparing them against that same table is two operands from one source: it
// reads like a strong invariant but it mostly cannot fail while
// levelsWithDisposition is correct. It catches a hand-written literal that
// DISAGREES with the table, and it caught a menu re-derived from the wrong set.
// It does NOT catch a literal whose value happens to equal the derived one -
// measured green - because comparing values can only see a literal that is wrong,
// never one that is merely hand-maintained. An earlier wording claimed it caught
// "a set replaced by a hand-written literal" without that qualification, which is
// more than a value comparison can deliver. But the load-bearing check is the FIXTURE
// cross-check at the top, which compares production against a corpus a human
// wrote by hand and must deliberately edit. Do not read the rest as independent
// confirmation.
func TestOfferedRedactionLevels_IsExactlyWhatTheDispositionTableSays(t *testing.T) {
	t.Parallel()

	// The independent anchor. policy.yaml is authored by hand, is not generated
	// from the table, and names a disposition per level; if the table changes and
	// the corpus does not, this disagrees. That is the only comparison here whose
	// two sides have different origins.
	document, err := loadPolicyFixture(policyFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	var offeredPerFixture []redact.RedactionLevel
	for _, testCase := range document.Redaction {
		if testCase.Disposition == dispositionOffered && testCase.Configured != "" {
			offeredPerFixture = append(offeredPerFixture, testCase.Configured)
		}
	}
	if len(offeredPerFixture) != len(OfferedRedactionLevels) {
		t.Errorf("the corpus names %d offered level(s) %v but production offers %d %v; one of the two was changed without "+
			"the other, and the corpus is the side a human has to mean",
			len(offeredPerFixture), offeredPerFixture, len(OfferedRedactionLevels), OfferedRedactionLevels)
	}
	for _, level := range offeredPerFixture {
		if !slices.Contains(OfferedRedactionLevels, level) {
			t.Errorf("the corpus says %q is offered but production does not offer it", level)
		}
	}
	for _, level := range redact.AllRedactionLevels() {
		disposition := RedactionLevelDispositionOf(level)
		if disposition == RedactionLevelDispositionUnknown {
			t.Errorf("the engine defines the %q level but the policy has no disposition for it; it would be refused "+
				"everywhere with a message that cannot explain why", level)
		}
		inOffered := slices.Contains(OfferedRedactionLevels, level)
		if want := disposition == RedactionLevelDispositionOffered; inOffered != want {
			t.Errorf("OfferedRedactionLevels contains %q = %v, but its disposition is %v", level, inOffered, disposition)
		}
		inSupported := slices.Contains(SupportedRedactionLevels, level)
		want := disposition == RedactionLevelDispositionOffered || disposition == RedactionLevelDispositionRaised
		if inSupported != want {
			t.Errorf("SupportedRedactionLevels contains %q = %v, but its disposition is %v", level, inSupported, disposition)
		}
	}
	// The menu is the user-visible rendering of the offered set. Nothing outside it
	// may appear there, and a level that is merely supported is the one most likely
	// to leak in - it used to be listed.
	for _, level := range redact.AllRedactionLevels() {
		named := strings.Contains(RedactionLevelMenu(), level.String())
		if want := RedactionLevelOffered(level); named != want {
			t.Errorf("RedactionLevelMenu() = %q names %q = %v, want %v; the menu is an offer, not a statement about what "+
				"the engine can apply", RedactionLevelMenu(), level, named, want)
		}
	}
	if len(OfferedRedactionLevels) == 0 {
		t.Fatal("no redaction level is offered, so no user could configure one that runs")
	}
	// Every offered level must resolve to itself. An offered level that resolved to
	// something else would mean the menu offers a choice the product overrides.
	for _, level := range OfferedRedactionLevels {
		if policy := ResolveRedactionPolicy(level); policy.Effective != level || policy.Raised() {
			t.Errorf("the offered level %q resolves to %q (raised=%v); an offered level must run as chosen",
				level, policy.Effective, policy.Raised())
		}
	}
}

// TestRedactionScopeSentence_NeverClaimsCompleteness is the language guard.
//
// Pattern matching is best effort. A sentence that says redaction removes personal
// data, without saying it removes the patterns it recognises and cannot promise it
// found them all, is a false claim about a privacy control - and it is the single
// sentence every surface quotes, so one bad edit here reaches all of them at once.
func TestRedactionScopeSentence_NeverClaimsCompleteness(t *testing.T) {
	t.Parallel()
	// "as recorded" is deliberately NO LONGER required. It was, while transcript
	// content was published untransformed; push now applies the same content
	// redaction the redact command applies, so requiring the sentence to say
	// content is published as recorded would pin a claim that has become false.
	// What replaced it is the requirement that the sentence name transcript
	// content at all, so the scope cannot quietly narrow back to metadata.
	sentence := RedactionScopeSentence()
	if sentence == "" {
		t.Fatal("the scope sentence is empty, so every surface that quotes it says nothing about what redaction does")
	}
	for _, want := range []string{
		"KNOWN PATTERNS",     // the scope
		"best effort",        // the limit
		"not a guarantee",    // said outright, not implied
		"transcript content", // that content is covered, not only metadata
	} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the scope sentence must contain %q; got:\n%s", want, sentence)
		}
	}
	// "village" is deliberately NO LONGER required, and its return is forbidden.
	//
	// The sentence used to credit the village's own scan as a second, independent
	// check on secrets. The village scans the transcript part and NOT the metadata
	// part published beside it, so the credit was wider than the check - and this
	// one constant renders on the onboarding screen, on every push, in both sync
	// refusals and in the generated web policy, so the over-claim was six surfaces
	// wide the moment it was written here.
	for _, overBroad := range []string{"village", "second, independent"} {
		if strings.Contains(sentence, overBroad) {
			t.Errorf("the scope sentence names %q. This sentence can only promise what PEASANT does; a backstop that "+
				"covers one of the two published parts must not be described as though it covers a publish.\ngot:\n%s",
				overBroad, sentence)
		}
	}
	// The over-claim phrases come from the shared corpus rather than being listed
	// here. They were listed in two places, and the copies drifted on whether the
	// text was lower-cased before comparing - which is the single detail that
	// decides whether the guard sees a phrase at the start of a sentence.
	overclaims, err := testutil.Overclaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(overclaims) < expectedOverclaimCount {
		t.Fatalf("the shared over-claim list holds %d phrases, below the %d this surface expects; if one was retired, "+
			"say why in the corpus header and update the count here and in cmd/peasant too.",
			len(overclaims), expectedOverclaimCount)
	}
	for _, claim := range overclaims {
		if claim.Asserts(sentence) {
			t.Errorf("the scope sentence claims completeness with %q, which pattern matching cannot deliver.\n"+
				"that phrasing usually arrives as: %s\ngot:\n%s", claim.Needle, claim.Why, sentence)
		}
	}
}

// assertRefusalIsActionable holds the refusal to the project's error contract.
// A refusal that does not name the setting, a supported value to use, and what
// was left untouched is a dead end rather than a remedy.
func assertRefusalIsActionable(t *testing.T, level redact.RedactionLevel) {
	t.Helper()
	refusal := &UnsupportedRedactionLevelError{
		Level:     level,
		Source:    "your configuration file /tmp/example/config.yaml",
		Operation: "village push",
		Step:      "while building the redactor, before any session was uploaded",
		Impact:    "Nothing was published and nothing was recorded as published.",
	}
	if !errors.Is(refusal, ErrUnsupportedRedactionLevel) {
		t.Error("the refusal must classify as ErrUnsupportedRedactionLevel so handlers can map it to a client error")
	}
	message := refusal.Error()
	for _, want := range []string{
		level.String(),                     // what was refused
		"not supported in this version",    // what went wrong
		"village push",                     // where
		"/tmp/example/config.yaml",         // which file to edit
		"before any session was uploaded",  // when
		"Nothing was published",            // what it means for the caller
		"redaction.level",                  // the setting to change
		RecommendedRedactionLevel.String(), // the value to change it to
		RedactionLevelMenu(),               // every value that works
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal must state %q; got:\n%s", want, message)
		}
	}
	// It must not offer the level it just refused as a way out.
	if strings.Contains(RedactionLevelMenu(), level.String()) {
		t.Errorf("the supported menu %q offers the refused level %q", RedactionLevelMenu(), level)
	}
	if strings.ContainsRune(message, '\u2014') {
		t.Errorf("the refusal must stay ASCII: it is printed to stderr from a Git hook; got:\n%s", message)
	}
}

// TestEffectiveVisibility_ResolvesEveryContractVisibility is the guard against
// the defect that a `== public` comparison caused: a configured group visibility
// was announced as applied while the village stored private.
func TestEffectiveVisibility_ResolvesEveryContractVisibility(t *testing.T) {
	t.Parallel()
	document, err := loadPolicyFixture(policyFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Visibility {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			cfg := BaseConfig()
			cfg.Push.Visibility = testCase.Configured
			policy := EffectiveVisibility("", cfg)
			if policy.Configured != testCase.Configured {
				t.Errorf("Configured = %q, want %q", policy.Configured, testCase.Configured)
			}
			if policy.Effective != testCase.Effective {
				t.Errorf("Effective = %q, want %q", policy.Effective, testCase.Effective)
			}
			if policy.Downgraded() != testCase.Downgraded {
				t.Errorf("Downgraded() = %v, want %v", policy.Downgraded(), testCase.Downgraded)
			}
			assertDisclosureMatchesDowngrade(t, testCase.Downgraded,
				policy.Disclosure(), policy.BriefDisclosure(),
				string(testCase.Configured), string(testCase.Effective))

			// The same value passed as an explicit per-run override must resolve
			// identically: the flag is a different door onto one policy.
			fromFlag := EffectiveVisibility(testCase.Configured, BaseConfig())
			if fromFlag != policy {
				t.Errorf("an explicit override resolved to %+v but the same configured value resolved to %+v", fromFlag, policy)
			}
		})
	}
}

// assertDisclosureMatchesDowngrade holds the contract both policies share: a
// downgrade is always disclosed, in both the full and the one-line form, naming
// what was asked for and what happens instead; anything else is silent.
func assertDisclosureMatchesDowngrade(t *testing.T, downgraded bool, full, brief, configured, effective string) {
	t.Helper()
	if !downgraded {
		if full != "" || brief != "" {
			t.Errorf("nothing was downgraded, so nothing may be disclosed; got full=%q brief=%q", full, brief)
		}
		return
	}
	for label, text := range map[string]string{"Disclosure()": full, "BriefDisclosure()": brief} {
		if text == "" {
			t.Fatalf("%s is empty for a downgrade; the user would never learn their setting did not apply", label)
		}
		for _, want := range []string{configured, effective, "is not", "implemented in this version", "nothing to do"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s must state %q, so the user can see what they asked for, what happened, and that no action is needed; got:\n%s",
					label, want, text)
			}
		}
		if strings.ContainsRune(text, '—') {
			t.Errorf("%s must stay ASCII: it is printed to stderr from a Git hook, where a non-ASCII glyph can render badly; got:\n%s",
				label, text)
		}
	}
	if strings.Count(brief, "\n") != 0 {
		t.Errorf("BriefDisclosure() must be one line: a hook prints it on every commit; got:\n%s", brief)
	}
}

// TestVisibilityMenu_DerivesFromTheContract proves the flag's validation message
// is derived from the closed set rather than restated, so it cannot drift from
// what the contract accepts.
func TestVisibilityMenu_DerivesFromTheContract(t *testing.T) {
	t.Parallel()
	menu := VisibilityMenu()
	for _, visibility := range schema.AllVisibilities {
		if !strings.Contains(menu, visibility.String()) {
			t.Errorf("VisibilityMenu() = %q, which omits the contract visibility %q", menu, visibility)
		}
	}
}

// TestRedactionRefusalReason_StatesSomethingForALevelNobodyTaughtItAbout pins the
// third arm of RedactionRefusalReason: the fail-closed one, for a level the
// disposition table has no row for.
//
// Every level the engine defines has a row today, so this arm is unreachable
// through production inputs — and that is exactly why nothing read it. All four
// of its fields were measured droppable with the whole module green: the level
// name, the consequence clause, the sentence, and the whole return value
// replaced by "". It renders to a user the moment anyone adds a level to
// the redaction module without adding a row here, which is the situation it exists for, and
// it would have rendered nothing.
//
// The arm is reached through the PUBLIC function with a level the table does not
// carry. That is not a test-only door: it is the same call every refusal makes,
// with the argument the arm was written to handle.
func TestRedactionRefusalReason_StatesSomethingForALevelNobodyTaughtItAbout(t *testing.T) {
	t.Parallel()
	// A level the engine does not define, so the table cannot have a row for it.
	const untaught = redact.RedactionLevel("paranoid")
	if RedactionLevelDispositionOf(untaught) != RedactionLevelDispositionUnknown {
		t.Fatalf("%q resolves to a known disposition, so this test is no longer reaching the fail-closed arm; pick a "+
			"level the table does not carry", untaught)
	}
	const consequence = "the run would not apply the protection that was asked for"
	reason := RedactionRefusalReason(untaught, consequence)

	if strings.TrimSpace(reason) == "" {
		t.Fatal("the unknown-disposition arm renders NOTHING. It is the arm that fires when a level is added to the " +
			"engine and not to the policy table, so it is the one a user meets least often and understands least - an " +
			"empty reason leaves a refusal with no explanation at all")
	}
	// The level has to be named, or the user cannot tell which setting to change.
	if !strings.Contains(reason, untaught.String()) {
		t.Errorf("the reason does not name the level %q it is refusing, so a user reading it cannot tell what to edit; "+
			"got: %s", untaught, reason)
	}
	// And the caller's consequence has to survive, or each surface's own
	// explanation of what it did is dropped on this arm alone.
	if !strings.Contains(reason, consequence) {
		t.Errorf("the reason drops the caller's consequence clause, so the surface-specific half of the sentence - what "+
			"THIS command did about it - is lost on the fail-closed arm; got: %s", reason)
	}
	// It must not invent a cause. The other two arms explain WHY the level is not
	// available; this one cannot, because nothing here knows anything about the
	// level, and guessing is how the refusal sentence has already been wrong twice.
	for _, invented := range []string{"cgo", "code identifiers", "redacts less"} {
		if strings.Contains(reason, invented) {
			t.Errorf("the fail-closed arm claims %q about a level this version has never been taught about. It knows "+
				"nothing except that it has no rule for it, and saying more is the defect the other two arms have "+
				"already had twice; got: %s", invented, reason)
		}
	}
}
