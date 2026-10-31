package main

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/consent_answers.yaml
var consentAnswersYAML []byte

type consentAnswerCase struct {
	Name  string `yaml:"name"`
	Input string `yaml:"input"`
	Want  bool   `yaml:"want"`
}

func loadConsentAnswers(t *testing.T) []consentAnswerCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string            `yaml:"requiredNames"`
		Cases         []consentAnswerCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(consentAnswersYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

func TestDestructiveConsentAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range loadConsentAnswers(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			if got := readConsent(strings.NewReader(tc.Input)); got != tc.Want {
				t.Errorf("consent = %v, want %v", got, tc.Want)
			}
		})
	}
}
