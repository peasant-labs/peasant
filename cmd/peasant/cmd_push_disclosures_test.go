package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/push_disclosures.yaml
var pushDisclosureFixtureData []byte

// The rejection fixtures each hold a full corpus with exactly ONE thing wrong, so
// the evidence that the loader is strict sits beside the corpus it protects.
var (
	//go:embed testdata/push_disclosures-reject-uncovered-notice-shape.yaml
	pushDisclosureRejectUncoveredShapeData []byte
	//go:embed testdata/push_disclosures-reject-uncovered-record-state.yaml
	pushDisclosureRejectUncoveredRecordStateData []byte
	//go:embed testdata/push_disclosures-reject-unfireable-claim.yaml
	pushDisclosureRejectUnfireableClaimData []byte
)

const pushDisclosureFixturePath = "cmd/peasant/testdata/push_disclosures.yaml"

// expectedPushForbiddenClaimCount anchors the forbid-list's size OUTSIDE the file
// that holds the list.
//
// The cases below are anchored by closed sets - every contract visibility, every
// notice shape, both record states - so a deleted case fails a coverage guard. A
// forbid-list has no closed set behind it: it is an open ledger of sentences that
// were printed while untrue, and its declared count sits in the same file as its
// rows, so the pair stays self-consistent however many rows are removed. Every
// row was individually deletable in silence.
//
// This does not make removal hard, and it is not meant to. It makes removal
// VISIBLE: dropping a permanent regression guard becomes an edit to a Go constant
// in a second file, which a reviewer reading a fixture diff would otherwise never
// see.
const expectedPushForbiddenClaimCount = 6

// noticeShape is how much a surface says about a downgrade. The distinction is
// not cosmetic: a Git hook runs with --quiet on every commit, so a four-line
// statement whose own content is "nothing to do" would arrive there forever,
// while dropping it entirely would leave the downgrade undisclosed on the one
// surface that publishes automatically.
type noticeShape string

const (
	// noticeFull is the what/why/means/fix statement.
	noticeFull noticeShape = "full"
	// noticeBrief is the one-line form.
	noticeBrief noticeShape = "brief"
	// noticeNone is silence, which is only correct when nothing was downgraded.
	noticeNone noticeShape = "none"
)

var allNoticeShapes = [...]noticeShape{noticeFull, noticeBrief, noticeNone}

type pushDisclosureDocument struct {
	ExpectedCaseCount           int                  `yaml:"expectedCaseCount"`
	Cases                       []pushDisclosureCase `yaml:"cases"`
	ExpectedForbiddenClaimCount int                  `yaml:"expectedForbiddenClaimCount"`
	ForbiddenClaims             []pushForbiddenClaim `yaml:"forbiddenClaims"`
}

type pushDisclosureCase struct {
	Name             string                `yaml:"name"`
	Visibility       config.Visibility     `yaml:"visibility"`
	ConfiguredLevel  redact.RedactionLevel `yaml:"configuredLevel"`
	Quiet            bool                  `yaml:"quiet"`
	VisibilityNotice noticeShape           `yaml:"visibilityNotice"`
	RedactionRecord  bool                  `yaml:"redactionRecord"`
}

// pushForbiddenClaim is one sentence or shape that must never print again.
//
// It carries a SAMPLE because a forbid-needle is the one kind of assertion that
// passes for free: the text is gone, so it stays green whether the needle works
// or not. The sample is text the needle must match, checked at load, so a needle
// pinned to a form nothing can produce - or misspelled, or emptied - is a load
// failure rather than a permanent pass.
type pushForbiddenClaim struct {
	Name string `yaml:"name"`
	// Phrase is literal text. Exactly one of Phrase and Pattern is set.
	Phrase string `yaml:"phrase,omitempty"`
	// Pattern is a regular expression, for a claim whose exact wording varies
	// with the data - a session count, a level name - where a literal would only
	// fire for the one value the test happens to seed.
	Pattern string `yaml:"pattern,omitempty"`
	// MatchesSample is output the needle must match.
	MatchesSample string `yaml:"matchesSample"`
	// compiled is the prepared Pattern, or nil for a Phrase claim.
	compiled *regexp.Regexp
}

// needle is the claim's own text, for a failure message.
func (c pushForbiddenClaim) needle() string {
	if c.Pattern != "" {
		return c.Pattern
	}
	return c.Phrase
}

// matches reports whether output carries this claim.
func (c pushForbiddenClaim) matches(output string) bool {
	if c.compiled != nil {
		return c.compiled.MatchString(output)
	}
	return strings.Contains(output, c.Phrase)
}

// visibilityRun is one cell of the corpus's cross-product: a configured
// visibility and whether the run is quiet. The two decide the disclosure
// together, so they are covered together.
type visibilityRun struct {
	visibility config.Visibility
	quiet      bool
}

// loadPushDisclosureFixture decodes and fully validates the corpus.
func loadPushDisclosureFixture(data []byte) (pushDisclosureDocument, error) {
	var document pushDisclosureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, pushDisclosureRuleError(
			"typed YAML fields must match the document schema", "loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, pushDisclosureRuleError(
			"exactly one YAML document is allowed; cases below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, pushDisclosureRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases present")
	}
	if len(document.ForbiddenClaims) == 0 || document.ExpectedForbiddenClaimCount != len(document.ForbiddenClaims) {
		return document, pushDisclosureRuleError(
			fmt.Sprintf("declared and actual forbidden-claim counts must match and be non-zero, got expectedForbiddenClaimCount=%d claims=%d",
				document.ExpectedForbiddenClaimCount, len(document.ForbiddenClaims)),
			"loader=forbidden-claim count validation",
			"fix=set expectedForbiddenClaimCount to the number of forbiddenClaims present")
	}
	seen := map[string]bool{}
	var coveredVisibilities []config.Visibility
	coveredShapes := map[noticeShape]bool{}
	coveredRecordStates := map[bool]bool{}
	coveredPairs := map[visibilityRun]bool{}
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, pushDisclosureRuleError(
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				fmt.Sprintf("loader=case index %d", index),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		if !testCase.Visibility.IsValid() {
			return document, pushDisclosureRuleError(
				fmt.Sprintf("case %q names a visibility the contract does not define: %q", testCase.Name, testCase.Visibility),
				fmt.Sprintf("loader=case index %d", index),
				fmt.Sprintf("fix=use one of %s", config.VisibilityMenu()))
		}
		for label, shape := range map[string]noticeShape{
			"visibilityNotice": testCase.VisibilityNotice,
		} {
			if !containsNoticeShape(allNoticeShapes[:], shape) {
				return document, pushDisclosureRuleError(
					fmt.Sprintf("case %q gives %s the unsupported shape %q", testCase.Name, label, shape),
					fmt.Sprintf("loader=case index %d", index),
					"fix=use full, brief, or none")
			}
		}
		coveredVisibilities = append(coveredVisibilities, testCase.Visibility)
		coveredShapes[testCase.VisibilityNotice] = true
		coveredPairs[visibilityRun{testCase.Visibility, testCase.Quiet}] = true
		coveredRecordStates[testCase.RedactionRecord] = true
	}
	for _, visibility := range allContractVisibilities() {
		if !containsVisibility(coveredVisibilities, visibility) {
			return document, pushDisclosureRuleError(
				fmt.Sprintf("no case configures the %q visibility", visibility),
				"loader=closed-set coverage",
				"fix=add one; a disclosure written against a single named visibility passes for that one and lies about the rest")
		}
		// The CROSS-PRODUCT, not the two axes separately. What a push says is
		// decided by the configured visibility AND whether the run is quiet, so
		// those pairs are the behaviour; covering each axis alone lets a row be
		// deleted while its cell goes dark, because a sibling still supplies the
		// visibility and another still supplies the quiet run. Measured: deleting
		// the one quiet PRIVATE case left nothing proving that a quiet run with
		// nothing to disclose says nothing at all, and the corpus stayed green.
		for _, quiet := range []bool{true, false} {
			if !coveredPairs[visibilityRun{visibility, quiet}] {
				return document, pushDisclosureRuleError(
					fmt.Sprintf("no case configures the %q visibility with quiet=%v", visibility, quiet),
					"loader=closed-set coverage",
					"fix=add one; the two settings decide the disclosure TOGETHER - a downgrade says nothing extra on a quiet "+
						"run and everything on a loud one - so a missing pair is a combination the command was never asked to "+
						"handle")
			}
		}
	}
	// Shape coverage, walked from the closed set rather than from the rows. Each
	// shape is a different promise - the full statement, the one-line form, and
	// silence - and the corpus is the only place any of them is proved on the
	// MOUNTED command. Deleting the single brief row left `brief` wholly
	// unexercised with everything still green: the one-line form survived as text,
	// unit-covered on the policy type, and nothing any longer showed the real push
	// selects it. That form is what a Git hook prints on every commit.
	for _, shape := range allNoticeShapes {
		if !coveredShapes[shape] {
			return document, pushDisclosureRuleError(
				fmt.Sprintf("no case expects the %q disclosure shape", shape),
				"loader=notice-shape coverage",
				fmt.Sprintf("fix=add one; with no %s row nothing proves the mounted command ever selects that form, and the "+
					"unit test on the text underneath cannot tell which form the command chose", shape))
		}
	}
	// The record axis, same argument at one bit. Whether the published-content
	// record is kept is a decision the command makes, and a corpus exercising only
	// one answer cannot tell a command that decides from one that always does the
	// same thing.
	for _, kept := range []bool{true, false} {
		if !coveredRecordStates[kept] {
			return document, pushDisclosureRuleError(
				fmt.Sprintf("no case expects the published-content record to be %s", presence(kept)),
				"loader=record-keeping coverage",
				"fix=add one; the record used to sit inside a branch no input could reach, so no push printed it at all, "+
					"and a corpus that only ever expects one answer cannot see that")
		}
	}
	for index, claim := range document.ForbiddenClaims {
		where := fmt.Sprintf("loader=forbidden claim index %d", index)
		if strings.TrimSpace(claim.Name) == "" {
			return document, pushDisclosureRuleError(
				fmt.Sprintf("forbidden claim %d has no name", index), where,
				"fix=name the claim after the thing that must stay true")
		}
		if (strings.TrimSpace(claim.Phrase) == "") == (strings.TrimSpace(claim.Pattern) == "") {
			return document, pushDisclosureRuleError(
				fmt.Sprintf("forbidden claim %q must set exactly one of phrase and pattern", claim.Name), where,
				"fix=use phrase for fixed wording and pattern where the wording varies with the data; setting NEITHER "+
					"leaves an empty needle, which every output trivially contains, so the row would fire on everything or - "+
					"as written here - be skipped and prove nothing")
		}
		if strings.TrimSpace(claim.MatchesSample) == "" {
			return document, pushDisclosureRuleError(
				fmt.Sprintf("forbidden claim %q carries no sample, so nothing shows its needle can fire", claim.Name), where,
				"fix=add matchesSample with output the needle must match; a forbid-needle is green whether it works or not")
		}
		if claim.Pattern != "" {
			compiled, err := regexp.Compile(claim.Pattern)
			if err != nil {
				return document, pushDisclosureRuleError(
					fmt.Sprintf("forbidden claim %q has a pattern that does not compile: %v", claim.Name, err), where,
					"fix=correct the expression")
			}
			claim.compiled = compiled
			document.ForbiddenClaims[index] = claim
		}
		if !claim.matches(claim.MatchesSample) {
			return document, pushDisclosureRuleError(
				fmt.Sprintf("forbidden claim %q does not match its own sample %q", claim.Name, claim.MatchesSample), where,
				"fix=correct the needle so it fires on the text it names; a needle that cannot match the claim it forbids "+
					"leaves the suite green through the exact regression it was written for")
		}
	}
	// Without a case where the CONFIGURED level differs from the level actually
	// applied, the record's level assertion passes whether the record is wired to
	// the redactor that was built or to the configuration - the two coincide. That
	// makes the assertion look like coverage of the wiring while covering nothing.
	sawDivergentLevel := false
	for _, testCase := range document.Cases {
		if !testCase.RedactionRecord {
			continue
		}
		if config.ResolveRedactionPolicy(testCase.ConfiguredLevel).Effective != testCase.ConfiguredLevel {
			sawDivergentLevel = true
		}
	}
	if !sawDivergentLevel {
		return document, pushDisclosureRuleError(
			"no record-keeping case configures a level that resolves to a DIFFERENT applied level",
			"loader=record-level discrimination coverage",
			"fix=add one (a no-longer-offered level does this); where the configured and applied levels coincide, a record "+
				"wired to the configuration is indistinguishable from one wired to the redactor that actually ran")
	}
	return document, nil
}

func pushDisclosureRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"push disclosure fixture rule failed: %s; a malformed corpus invalidates the only evidence that a push says what it "+
			"actually does with the user's settings; where=%s %s; when=test fixture loading; "+
			"impact=a setting could be announced as applied while something else happens; %s",
		what, pushDisclosureFixturePath, where, fix)
}

func containsNoticeShape(shapes []noticeShape, want noticeShape) bool {
	for _, shape := range shapes {
		if shape == want {
			return true
		}
	}
	return false
}

func containsVisibility(values []config.Visibility, want config.Visibility) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// allContractVisibilities is the closed set the corpus has to cover, taken from
// the contract rather than listed here, so a visibility added to it turns this
// corpus red instead of quietly going undisclosed.
func allContractVisibilities() []config.Visibility {
	return append([]config.Visibility(nil), schema.AllVisibilities...)
}

// --- loader guards ----------------------------------------------------------

func TestLoadPushDisclosureFixture_RejectsACorpusThatSkipsAContractVisibility(t *testing.T) {
	t.Parallel()
	_, err := loadPushDisclosureFixture([]byte(`expectedCaseCount: 1
cases:
  - name: public-only
    visibility: public
    quiet: true
    visibilityNotice: none
    redactionRecord: false
expectedForbiddenClaimCount: 1
forbiddenClaims:
  - name: no-public-claim
    phrase: "session(s) publicly"
    matchesSample: "publish 2 session(s) publicly"
`))
	if err == nil || !strings.Contains(err.Error(), `no case configures the "private" visibility`) {
		t.Fatalf("error = %v, want rejection of a corpus that leaves a contract visibility uncovered", err)
	}
}

func TestLoadPushDisclosureFixture_RejectsACorpusWithNoQuietRun(t *testing.T) {
	t.Parallel()
	_, err := loadPushDisclosureFixture([]byte(`expectedCaseCount: 3
cases:
  - name: private
    visibility: private
    quiet: false
    visibilityNotice: none
    redactionRecord: true
  - name: group
    visibility: group
    quiet: false
    visibilityNotice: none
    redactionRecord: true
  - name: public
    visibility: public
    quiet: false
    visibilityNotice: none
    redactionRecord: true
expectedForbiddenClaimCount: 1
forbiddenClaims:
  - name: no-public-claim
    phrase: "session(s) publicly"
    matchesSample: "publish 2 session(s) publicly"
`))
	if err == nil || !strings.Contains(err.Error(), "with quiet=true") {
		t.Fatalf("error = %v, want rejection of a corpus that never exercises the hook's own output level. --quiet is what "+
			"a Git hook runs with, so it is the form nearly every user actually sees, and the cross-product requires it "+
			"for every visibility rather than once anywhere", err)
	}
}

func TestLoadPushDisclosureFixture_RejectsACorpusThatSkipsANoticeShape(t *testing.T) {
	t.Parallel()
	_, err := loadPushDisclosureFixture(pushDisclosureRejectUncoveredShapeData)
	if err == nil || !strings.Contains(err.Error(), `no case expects the "brief" disclosure shape`) {
		t.Fatalf("error = %v, want rejection of a corpus with no one-line-form row. That is the form a Git hook prints on "+
			"every commit, and deleting the single row exercising it left it wholly unproved on the mounted command while "+
			"everything stayed green", err)
	}
}

func TestLoadPushDisclosureFixture_RejectsACorpusThatAlwaysKeepsTheRecord(t *testing.T) {
	t.Parallel()
	_, err := loadPushDisclosureFixture(pushDisclosureRejectUncoveredRecordStateData)
	if err == nil || !strings.Contains(err.Error(), "record to be absent") {
		t.Fatalf("error = %v, want rejection of a corpus that only ever expects one answer about the record; it cannot "+
			"tell a command that DECIDES from one that always does the same thing", err)
	}
}

func TestLoadPushDisclosureFixture_RejectsAForbiddenClaimThatCannotFire(t *testing.T) {
	t.Parallel()
	_, err := loadPushDisclosureFixture(pushDisclosureRejectUnfireableClaimData)
	if err == nil || !strings.Contains(err.Error(), "does not match its own sample") {
		t.Fatalf("error = %v, want rejection of a needle pinned to a form nothing can produce; a forbid-list is green "+
			"whether its needles work or not", err)
	}
}

// TestPushDisclosureCorpus_KeepsEveryForbiddenClaimItEverAdded is the anchor the
// forbid-list has no closed set to provide.
//
// Deleting a row and decrementing expectedForbiddenClaimCount leaves the corpus
// perfectly self-consistent, because both live in the same file. Each of the six
// names a sentence that was printed to a user while being untrue; none of them
// stops being worth guarding because the code that produced it was rewritten.
func TestPushDisclosureCorpus_KeepsEveryForbiddenClaimItEverAdded(t *testing.T) {
	t.Parallel()
	document, err := loadPushDisclosureFixture(pushDisclosureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.ForbiddenClaims) != expectedPushForbiddenClaimCount {
		t.Errorf("the corpus carries %d forbidden claims, want %d. If a guard was deliberately retired, say why in the "+
			"fixture header and change expectedPushForbiddenClaimCount here as well - the point of the second edit is that "+
			"dropping a permanent regression guard cannot happen inside a fixture diff alone. If one was ADDED, this "+
			"constant is simply behind.", len(document.ForbiddenClaims), expectedPushForbiddenClaimCount)
	}
}

// --- the corpus -------------------------------------------------------------

// TestPushCmd_DisclosesWhatItActuallyAppliesAndKeepsTheRecord drives the real
// push command against a seeded, publishable session for every combination of
// the two settings that can be configured to a value this version cannot apply.
//
// The expected wording is read from the SAME resolver the command prints, so this
// asserts that one text reaches the surface rather than that two texts happen to
// match. The forbidden claims are pinned separately, because each of them was
// printed at some point while being untrue.
func TestPushCmd_DisclosesWhatItActuallyAppliesAndKeepsTheRecord(t *testing.T) {
	t.Parallel()
	document, err := loadPushDisclosureFixture(pushDisclosureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			stderr := runPushForDisclosures(t, testCase)

			cfg := config.BaseConfig()
			cfg.Push.Visibility = testCase.Visibility
			visibilityPolicy := config.EffectiveVisibility("", cfg)

			assertNoticeShape(t, "the visibility disclosure", testCase.VisibilityNotice, stderr,
				visibilityPolicy.Disclosure(), visibilityPolicy.BriefDisclosure())

			// The published-content record. It used to live inside a branch gated
			// on a public visibility that no input can reach any more, so no push
			// printed it at all.
			hasRecord := strings.Contains(stderr, "Redaction report:")
			if hasRecord != testCase.RedactionRecord {
				t.Errorf("the published-content record was %s; want %s; stderr:\n%s",
					presence(hasRecord), presence(testCase.RedactionRecord), stderr)
			}
			if testCase.RedactionRecord && !strings.Contains(stderr, "You are about to publish") {
				t.Errorf("the record must say how many sessions it covers; stderr:\n%s", stderr)
			}
			if testCase.RedactionRecord {
				assertRecordNamesTheAppliedLevel(t, testCase, stderr)
			}

			for _, claim := range document.ForbiddenClaims {
				if claim.matches(stderr) {
					t.Errorf("%s: %q must never print; stderr:\n%s", claim.Name, claim.needle(), stderr)
				}
			}
		})
	}
}

// assertRecordNamesTheAppliedLevel is the guard for the defect that made the
// published-content record useless.
//
// ITS ACCEPTANCE TEST, stated as the production edit that must turn it red:
// changing the record's call site in cmd_push.go from the resolved effectiveLevel
// to cfg.Redaction.Level. That is the realistic wiring mistake - the two names sit
// four lines apart - and it makes the record claim a protection level other than
// the one the push actually applies, which is the whole subject of the defect.
//
// The unit-level guard on buildRedactionRecord CANNOT catch that edit: it passes a
// level in and asserts the same level comes out, so it is two operands from one
// source with respect to the wiring. It earns its place by catching a record
// re-derived from stored metadata, and nothing more. This is the half that covers
// the wiring, and it only has power because the corpus supplies a case where the
// CONFIGURED and APPLIED levels differ - hence the loader requirement.
//
// ITS SECOND ACCEPTANCE TEST, added after the first version of this assertion was
// found to cover one entry while reading as covering the record: dropping
// record.Level from the transcript-content body in printRedactionReport, leaving
// the metadata entry untouched. That was green here, because both entries render
// "redacted at <level> on upload" and the search ran over the whole output. Every
// level-bearing entry is now asserted against its own segment.
func assertRecordNamesTheAppliedLevel(t *testing.T, testCase pushDisclosureCase, stderr string) {
	t.Helper()
	applied := config.ResolveRedactionPolicy(testCase.ConfiguredLevel).Effective
	appliedPhrase := "redacted at " + applied.String() + " on upload"
	// EVERY entry that names a level is checked on its own, because both of them
	// render that phrase: a whole-output search for it is satisfied by whichever
	// entry still carries it, and this assertion covered only the metadata entry
	// while reading as covering the record. Dropping the level from the
	// transcript-content body alone was green.
	assertRedactionRecordEntry(t, stderr, "metadata:", appliedPhrase)
	// The entry a record listing only what IS redacted would let a reader wrongly
	// infer away, asserted as a whole: label, applied level, and what the
	// redaction did to the content, all on the one entry.
	assertRedactionRecordEntry(t, stderr, "transcript content:", appliedPhrase, "matched patterns replaced")
	// The distinguishing half. Where the configured level is NOT what runs, the
	// record must not name the configured one - that is the exact wiring mistake,
	// and without this case the assertions above pass either way.
	if applied != testCase.ConfiguredLevel {
		if strings.Contains(stderr, "redacted at "+testCase.ConfiguredLevel.String()+" on upload") {
			t.Errorf("the record names the CONFIGURED level %q, but this push applies %q; the record is wired to the "+
				"configuration rather than to the redactor that was actually built.\nstderr:\n%s",
				testCase.ConfiguredLevel, applied, stderr)
		}
	}
	// The rule set must be the compiled one, not a version an older import wrote.
	assertRedactionRecordEntry(t, stderr, "rule set version:", "v"+redact.RuleSetVersion)
	for _, want := range []string{"KNOWN PATTERNS", "not a guarantee"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the record must state %q; stderr:\n%s", want, stderr)
		}
	}
}

// assertNoticeShape holds a disclosure to exactly one of its two forms, or to
// silence, and requires the text to be the resolver's own.
func assertNoticeShape(t *testing.T, what string, shape noticeShape, stderr, full, brief string) {
	t.Helper()
	// Guard against this assertion proving nothing. The expected text comes from
	// the resolver, and strings.Contains is satisfied by an empty needle - so a
	// resolver that stopped disclosing this case would make every check below pass
	// trivially. That is the exact failure mode this corpus exists to catch, so it
	// must not be reachable from inside the corpus itself.
	if shape != noticeNone && (full == "" || brief == "") {
		t.Fatalf("%s: the resolver renders nothing for a case the corpus says must be disclosed as %q, so this case could only "+
			"pass by vacuity; full=%q brief=%q", what, shape, full, brief)
	}
	switch shape {
	case noticeFull:
		if !strings.Contains(stderr, full) {
			t.Errorf("%s must print the full statement the resolver renders:\n%s\ngot stderr:\n%s", what, full, stderr)
		}
	case noticeBrief:
		if !strings.Contains(stderr, brief) {
			t.Errorf("%s must collapse to the one-line form on a hook's output level:\n%s\ngot stderr:\n%s", what, brief, stderr)
		}
		if strings.Contains(stderr, full) {
			t.Errorf("%s printed the full statement on a run that asked for quiet output; stderr:\n%s", what, stderr)
		}
	case noticeNone:
		if full != "" && strings.Contains(stderr, full) {
			t.Errorf("%s was printed for a setting that applies as configured; stderr:\n%s", what, stderr)
		}
		if brief != "" && strings.Contains(stderr, brief) {
			t.Errorf("%s was printed for a setting that applies as configured; stderr:\n%s", what, stderr)
		}
	}
}

func presence(present bool) string {
	if present {
		return "printed"
	}
	return "absent"
}

// runPushForDisclosures runs the production push command against a village that
// accepts a connection and never answers, under a short budget.
//
// A publishable session has to be present: the record describes what is being
// published, and there is nothing to record when nothing is. The hanging village
// keeps the run off the network without stubbing the pipeline, and everything
// asserted here is written before the first request is made.
func runPushForDisclosures(t *testing.T, testCase pushDisclosureCase) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()

	dir := t.TempDir()
	writeTestCredentialsFor(t, dir, "http://"+listener.Addr().String())
	seedPushableSession(t, dir)
	cfgPath := writeCfg(t, dir, "disclosures.yaml", fmt.Sprintf(
		"version: 1\npush:\n  method: all\n  visibility: %s\nredaction:\n  level: %s\n",
		testCase.Visibility, testCase.ConfiguredLevel))

	args := []string{"--config", cfgPath, "--non-interactive", "--timeout", (300 * time.Millisecond).String()}
	if testCase.Quiet {
		args = append(args, "--quiet")
	}
	_, stderr, _ := executePushCmdSeparate(t, dir, args)
	return stderr
}
