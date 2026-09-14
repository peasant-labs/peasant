package main

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
)

//go:embed testdata/publish_consent_prompt.yaml
var publishConsentPromptData []byte

type consentPromptCase struct {
	Name     string `yaml:"name"`
	License  string `yaml:"license"`
	Sentence string `yaml:"sentence"`
}

// requiredConsentPromptCases is the deletion guard: exactly one row per license
// choice a push can apply. A required-NAME manifest, never a count.
var requiredConsentPromptCases = map[string]bool{
	"cc-by": true, "cc-by-sa": true, "cc0": true, "none": true,
}

func loadConsentPromptCases(t *testing.T) []consentPromptCase {
	t.Helper()
	var doc struct {
		Cases []consentPromptCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(publishConsentPromptData, &doc); err != nil {
		t.Fatalf("decode testdata/publish_consent_prompt.yaml: %v", err)
	}
	got := make(map[string]bool, len(doc.Cases))
	for _, c := range doc.Cases {
		if got[c.Name] {
			t.Fatalf("duplicate consent-prompt case %q", c.Name)
		}
		got[c.Name] = true
	}
	for name := range requiredConsentPromptCases {
		if !got[name] {
			t.Fatalf("publish_consent_prompt.yaml is missing required case %q", name)
		}
	}
	for name := range got {
		if !requiredConsentPromptCases[name] {
			t.Fatalf("publish_consent_prompt.yaml has an unexpected case %q", name)
		}
	}
	return doc.Cases
}

// The interactive public-consent prompt must name the license the push will
// apply and point at the privacy notice, for every license choice, so consent is
// informed at the moment it is given.
func TestPromptPublicConsent_NamesLicenseAndNotice(t *testing.T) {
	noticeURL := defaults.CommonsNoticeURL()
	for _, c := range loadConsentPromptCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			var out bytes.Buffer
			// A TTY, not auto-confirmed: the interactive branch that prints the
			// notice. The reader answers "n" so the call returns without blocking.
			consented, err := promptPublicConsent(strings.NewReader("n\n"), &out, false, true, config.License(c.License))
			if err != nil {
				t.Fatalf("promptPublicConsent: %v", err)
			}
			if consented {
				t.Fatalf("a 'n' answer must not consent")
			}
			got := out.String()
			if !strings.Contains(got, c.Sentence) {
				t.Errorf("prompt does not name the license.\nwant substring: %q\ngot: %q", c.Sentence, got)
			}
			if !strings.Contains(got, "Read the privacy notice at "+noticeURL) {
				t.Errorf("prompt does not link the notice %q.\ngot: %q", noticeURL, got)
			}
			if !strings.Contains(got, "Continue? [y/N]") {
				t.Errorf("prompt lost its Continue line.\ngot: %q", got)
			}
		})
	}
}
