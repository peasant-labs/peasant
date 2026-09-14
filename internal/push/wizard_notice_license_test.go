package push

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
)

//go:embed testdata/notice_license.yaml
var noticeLicenseData []byte

type noticeLicenseCase struct {
	Name    string `yaml:"name"`
	License string `yaml:"license"`
	Phrase  string `yaml:"phrase"`
}

type noticeLicenseDoc struct {
	Cases []noticeLicenseCase `yaml:"cases"`
}

// requiredNoticeLicenseCases is the deletion guard: the corpus must name exactly
// these cases (one per license choice the product offers). A count would churn on
// every legitimate edit; a required-NAME manifest fails only when coverage
// actually changes.
var requiredNoticeLicenseCases = map[string]bool{
	"cc-by":    true,
	"cc-by-sa": true,
	"cc0":      true,
	"none":     true,
}

func loadNoticeLicenseCases(t *testing.T) []noticeLicenseCase {
	t.Helper()
	var doc noticeLicenseDoc
	if err := yaml.Unmarshal(noticeLicenseData, &doc); err != nil {
		t.Fatalf("decode testdata/notice_license.yaml: %v", err)
	}
	got := make(map[string]bool, len(doc.Cases))
	for _, c := range doc.Cases {
		if got[c.Name] {
			t.Fatalf("duplicate notice_license case %q", c.Name)
		}
		got[c.Name] = true
	}
	for name := range requiredNoticeLicenseCases {
		if !got[name] {
			t.Fatalf("notice_license.yaml is missing required case %q", name)
		}
	}
	for name := range got {
		if !requiredNoticeLicenseCases[name] {
			t.Fatalf("notice_license.yaml has an unexpected case %q; add it to the manifest or remove it", name)
		}
	}
	return doc.Cases
}

// The consent copy is verified from the schema contract's side in the phrase
// helper's own test; here we prove the WIZARD threads the effective license all
// the way to its rendered notice page and points at the privacy notice, for
// every license choice a push can apply.
func TestPushWizard_NoticeNamesLicenseAndNotice(t *testing.T) {
	noticeURL := defaults.CommonsNoticeURL()
	if !strings.HasSuffix(noticeURL, "/privacy") {
		t.Fatalf("notice URL %q does not end in /privacy", noticeURL)
	}

	for _, c := range loadNoticeLicenseCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			// The helper is the single source of the phrase; assert the fixture
			// row agrees with it so a drift in either surfaces here.
			want := fmt.Sprintf("this push will %s.", config.PublishConsentPhrase(config.License(c.License)))
			if want != c.Phrase {
				t.Fatalf("fixture phrase %q disagrees with the helper %q", c.Phrase, want)
			}

			m := NewPushWizard(testTheme(), testSessions(), testPublishedTurns(), config.License(c.License))
			updated, _ := m.Update(windowSize(80, 24))
			m = updated.(PushWizardModel)
			// Onto the consent page, then to its bottom where the notice sits.
			m = pressKey(pressKey(acceptStart(m), keyEnter()), keyRune('G'))
			view := m.viewString()

			if !strings.Contains(view, c.Phrase) {
				t.Errorf("consent notice does not name the license.\nwant substring: %q\ngot:\n%s", c.Phrase, view)
			}
			if !strings.Contains(view, "read the privacy notice: "+noticeURL) {
				t.Errorf("consent notice does not link the privacy notice %q.\ngot:\n%s", noticeURL, view)
			}
		})
	}
}
