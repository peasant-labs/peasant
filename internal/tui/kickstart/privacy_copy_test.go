package kickstart

import (
	"bytes"
	_ "embed"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/settings"
)

// privacyCopyFixture pins the guide-copy contract for the privacy step: one short
// intro sentence, the spelled-out PII acronym on first use, and git-remote
// guidance that agrees with the project-identity example the Standard preview
// actually renders.
type privacyCopyFixture struct {
	IntroMaxSentences  int      `yaml:"introMaxSentences"`
	RequiredGuideText  []string `yaml:"requiredGuideText"`
	ForbiddenGuideText []string `yaml:"forbiddenGuideText"`
}

//go:embed testdata/guided/privacy_copy.yaml
var privacyCopyFixtureData []byte

func loadPrivacyCopyFixture(t *testing.T) privacyCopyFixture {
	t.Helper()
	var document privacyCopyFixture
	decoder := yaml.NewDecoder(bytes.NewReader(privacyCopyFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode privacy copy fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("privacy copy fixture must contain exactly one YAML document")
	}
	if document.IntroMaxSentences < 1 {
		t.Fatalf("privacy copy fixture must require at least one intro sentence")
	}
	if len(document.RequiredGuideText) == 0 || len(document.ForbiddenGuideText) == 0 {
		t.Fatalf("privacy copy fixture must declare required and forbidden guide text")
	}
	return document
}

func privacySectionGuide(t *testing.T) *settings.Guide {
	t.Helper()
	registry := BuildRegistry(Options{})
	for _, section := range registry.Sections {
		if section.Key == SectionPrivacy {
			if section.Guide == nil {
				t.Fatalf("privacy section has no guide")
			}
			return section.Guide
		}
	}
	t.Fatalf("privacy section %q not found in registry", SectionPrivacy)
	return nil
}

// countSentences counts sentence terminators in guide copy. The privacy intro
// must read as one short sentence, so more than one terminator is a regression.
func countSentences(text string) int {
	count := 0
	for _, r := range text {
		switch r {
		case '.', '!', '?':
			count++
		}
	}
	return count
}

func TestPrivacyGuideCopyContract(t *testing.T) {
	fixture := loadPrivacyCopyFixture(t)
	guide := privacySectionGuide(t)

	intro := strings.TrimSpace(guide.Intro)
	if intro == "" {
		t.Fatalf("privacy guide intro is empty")
	}
	if got := countSentences(intro); got > fixture.IntroMaxSentences {
		t.Errorf("privacy intro has %d sentences, want at most %d: %q", got, fixture.IntroMaxSentences, intro)
	}
	if strings.Contains(intro, ";") {
		t.Errorf("privacy intro should be one short sentence, not a compound clause: %q", intro)
	}

	guideText := intro + "\n" + strings.Join(guide.Hints, "\n")
	for _, want := range fixture.RequiredGuideText {
		if !strings.Contains(guideText, want) {
			t.Errorf("privacy guide copy is missing required text %q:\n%s", want, guideText)
		}
	}
	for _, forbidden := range fixture.ForbiddenGuideText {
		if strings.Contains(guideText, forbidden) {
			t.Errorf("privacy guide copy still contains forbidden text %q:\n%s", forbidden, guideText)
		}
	}
}
