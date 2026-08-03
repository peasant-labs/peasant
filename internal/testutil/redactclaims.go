// The phrases no user-facing redaction copy may use, and the one comparison that
// decides whether a surface uses them.
//
// This exists because the list was written out twice - once for the scope
// sentence in internal/config, once for the refusal and wizard copy in
// cmd/peasant - and the two copies drifted on the detail that decides whether the
// guard fires at all: one lower-cased the text before comparing and the other did
// not. The sentence-cased form, which is how an author would actually write it,
// was invisible to one of them while the identical words mid-sentence were
// caught.
//
// It lives in testutil, the package this repository already uses for test
// support shared across packages that cannot import each other, rather than in
// one of its own. It briefly had a package to itself on the argument that it
// "depends on nothing" - true, but it did not distinguish the alternative:
// testutil does not reach internal/config, so there is no cycle, and BOTH
// consumers already import testutil for other fixtures. A package with no
// production references, existing to avoid a dependency neither consumer would
// have acquired, is machinery without a job.
package testutil

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/completeness_overclaims.yaml
var overclaimFixtureData []byte

// OverclaimFixturePath is the corpus, for a failure message that has to name it.
const OverclaimFixturePath = "internal/testutil/testdata/completeness_overclaims.yaml"

// Overclaim is one phrase that asserts completeness, with the shape of edit that
// produces it.
type Overclaim struct {
	Needle string `yaml:"needle"`
	// MatchesSample is text the needle MUST match. It is what stops a needle that
	// can never fire from reading as a permanent pass.
	MatchesSample string `yaml:"matchesSample"`
	Why           string `yaml:"why"`
}

type overclaimDocument struct {
	ExpectedNeedleCount int         `yaml:"expectedNeedleCount"`
	Needles             []Overclaim `yaml:"needles"`
}

// Overclaims returns the corpus, or an error describing what is wrong with it.
//
// It validates rather than trusting: an empty needle is contained in every string,
// so a blank row would make every surface fail; an upper-case one can never match,
// because Asserts lower-cases the text and not the needle, so it would read as a
// rule and never fire.
func Overclaims() ([]Overclaim, error) {
	var document overclaimDocument
	decoder := yaml.NewDecoder(bytes.NewReader(overclaimFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, overclaimRuleError("typed YAML fields must match the document schema",
			fmt.Sprintf("fix=match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, overclaimRuleError("exactly one YAML document is allowed",
			"fix=remove everything after the first document")
	}
	if len(document.Needles) == 0 || document.ExpectedNeedleCount != len(document.Needles) {
		return nil, overclaimRuleError(
			fmt.Sprintf("declared and actual needle counts must match and be non-zero, got expectedNeedleCount=%d needles=%d",
				document.ExpectedNeedleCount, len(document.Needles)),
			"fix=set expectedNeedleCount to the number of needles present")
	}
	seen := map[string]bool{}
	for _, claim := range document.Needles {
		if strings.TrimSpace(claim.Needle) == "" {
			return nil, overclaimRuleError("a needle is blank",
				"fix=write it; every string contains the empty one, so a blank needle fails every surface at once")
		}
		if claim.Needle != strings.ToLower(claim.Needle) {
			return nil, overclaimRuleError(fmt.Sprintf("the needle %q is not lower case", claim.Needle),
				"fix=lower-case it; matching lower-cases the TEXT and not the needle, so an upper-case needle can never "+
					"fire and would read as a rule while being one")
		}
		if seen[claim.Needle] {
			return nil, overclaimRuleError(fmt.Sprintf("the needle %q appears twice", claim.Needle),
				"fix=remove the duplicate")
		}
		seen[claim.Needle] = true
		// THE FIRE-PROOF. A forbid-list is green whether its needles work or not,
		// so the corpus has to demonstrate each one works. Measured before this
		// existed: a same-count typo in a needle - the edit that RETIRES a guard -
		// left every consumer green, while deleting the row was caught by the
		// count. The floor caught the honest edit and missed the dangerous one.
		if strings.TrimSpace(claim.MatchesSample) == "" {
			return nil, overclaimRuleError(fmt.Sprintf("the needle %q carries no sample", claim.Needle),
				"fix=add matchesSample with text the needle must match, written as a sentence a surface could really "+
					"print; without it a misspelled needle is indistinguishable from a working one")
		}
		if !claim.Asserts(claim.MatchesSample) {
			return nil, overclaimRuleError(
				fmt.Sprintf("the needle %q does not match its own sample %q", claim.Needle, claim.MatchesSample),
				"fix=correct the needle or the sample; a needle that cannot fire on the text it names would stay green "+
					"through the exact regression it exists to catch")
		}
		if strings.TrimSpace(claim.Why) == "" {
			return nil, overclaimRuleError(fmt.Sprintf("the needle %q says nothing about what produces it", claim.Needle),
				"fix=write why; a rule whose reason is unrecorded is deleted by whoever next finds it inconvenient")
		}
	}
	return document.Needles, nil
}

// Asserts reports whether text makes this over-claim.
//
// It lower-cases the TEXT, which is the whole point: the phrase at the start of a
// sentence is the likeliest form of the regression, and a case-sensitive compare
// against sentence-cased copy cannot see it.
func (c Overclaim) Asserts(text string) bool {
	return strings.Contains(strings.ToLower(text), c.Needle)
}

func overclaimRuleError(what, fix string) error {
	return fmt.Errorf(
		"redaction over-claim corpus rule failed: %s; where=%s; when=loading the shared list of completeness claims; "+
			"impact=a user-facing surface could promise that a category is fully redacted, which pattern matching cannot "+
			"deliver and which leads a user to share what they would otherwise withhold; %s",
		what, OverclaimFixturePath, fix)
}

// The over-claim corpus above is imported by internal/config/policy_test.go,
// which is `package config` — an INTERNAL test. So testutil must never reach
// internal/config, or that file stops compiling with an import cycle.
//
// That was true when this list moved here and nothing said so or checked it. The
// standalone package it came from had the property structurally, by depending on
// nothing; testutil trades that for a transitive closure, and the trade is only
// safe while the closure excludes internal/config. It is a property of
// EVERYTHING testutil imports, forever, not of these two files.
//
// TestTestutil_DoesNotReachConfig in this package's tests is the enforcement.
