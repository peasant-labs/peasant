package ftue

import (
	_ "embed"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// menuPage names the wizard page that shows a shared menu. It is a closed set
// the fixture decodes through menuPageNamed, so a mistyped page fails loudly.
type menuPage string

const (
	menuPageRetention menuPage = "retention"
	menuPagePrivacy   menuPage = "privacy"
	menuPageLicense   menuPage = "license"
)

func menuPageNamed(t *testing.T, name string) menuPage {
	t.Helper()
	for _, page := range []menuPage{menuPageRetention, menuPagePrivacy, menuPageLicense} {
		if string(page) == name {
			return page
		}
	}
	t.Fatalf("shared menu fixture names page %q; the pages are retention, privacy, and license", name)
	return ""
}

// shownMenu is what one wizard page shows for a menu: the option tokens it
// writes, its labels and descriptions, and the typed answer each digit key picks.
type shownMenu struct {
	tokens       []string
	labels       []string
	descriptions []string
	// view renders the page with every option visible.
	view func() string
	// pick presses the digit key of option index and returns the token the page
	// then reports as selected.
	pick func(index int) string
}

func showMenu(t *testing.T, page menuPage) shownMenu {
	t.Helper()
	press := func(update func(tea.Msg), index int) {
		key := rune('1' + index)
		update(tea.KeyPressMsg{Code: key, Text: string(key)})
	}
	switch page {
	case menuPageRetention:
		shown := NewRetentionPage("Retention", -1, false)
		menu := shown.options
		out := shownMenu{view: func() string { return shown.View(100, 60) }}
		for _, option := range menu {
			out.tokens = append(out.tokens, strconv.Itoa(option.days))
			out.labels = append(out.labels, option.label)
			out.descriptions = append(out.descriptions, option.description)
		}
		out.pick = func(index int) string {
			fresh := NewRetentionPage("Retention", -1, false)
			press(func(msg tea.Msg) { fresh.Update(msg) }, index)
			return strconv.Itoa(fresh.SelectedDays())
		}
		return out
	case menuPagePrivacy:
		shown := NewPrivacyPreferencePage("Privacy")
		out := shownMenu{view: shown.viewChooser}
		for _, option := range shown.options {
			out.tokens = append(out.tokens, string(option.level))
			out.labels = append(out.labels, option.label)
			out.descriptions = append(out.descriptions, option.description)
		}
		out.pick = func(index int) string {
			fresh := NewPrivacyPreferencePage("Privacy")
			press(func(msg tea.Msg) { fresh.Update(msg) }, index)
			return fresh.SelectedLevel()
		}
		return out
	case menuPageLicense:
		shown := NewLicensePage("License")
		out := shownMenu{view: func() string { return shown.View(100, 60) }}
		for _, option := range shown.options {
			out.tokens = append(out.tokens, string(option.license))
			out.labels = append(out.labels, option.label)
			out.descriptions = append(out.descriptions, option.description)
		}
		out.pick = func(index int) string {
			fresh := NewLicensePage("License")
			press(func(msg tea.Msg) { fresh.Update(msg) }, index)
			return string(fresh.SelectedLicense())
		}
		return out
	default:
		t.Fatalf("no page shows the %q menu", page)
		return shownMenu{}
	}
}

type sharedMenuCase struct {
	Name           string   `yaml:"name"`
	Field          string   `yaml:"field"`
	Page           string   `yaml:"page"`
	WithheldTokens []string `yaml:"withheldTokens"`
}

type sharedMenuDocument struct {
	RequiredNames []string         `yaml:"requiredNames"`
	Cases         []sharedMenuCase `yaml:"cases"`
}

//go:embed testdata/shared_menus.yaml
var sharedMenusYAML []byte

// TestWizardMenus_ReadTheSharedModel holds each wizard menu to the setup model's
// definition of it. The guided flow and the local web setup read the same model, so
// a menu that drifted here would make the same answers write a different
// configuration depending on which presentation collected them.
func TestWizardMenus_ReadTheSharedModel(t *testing.T) {
	t.Parallel()
	var doc sharedMenuDocument
	if err := testutil.DecodeNamedFixtureYAML(sharedMenusYAML, &doc); err != nil {
		t.Fatalf("decode testdata/shared_menus.yaml: %v", err)
	}
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull})
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			key, err := onboarding.NewFieldKey(c.Field)
			if err != nil {
				t.Fatalf("fixture field: %v", err)
			}
			field, ok := model.Field(key)
			if !ok {
				t.Fatalf("the shared model has no %s field", key)
			}
			withheld := map[string]bool{}
			for _, token := range c.WithheldTokens {
				withheld[token] = true
			}
			var want []onboarding.Option
			for _, option := range field.Options() {
				if !withheld[option.Value] {
					want = append(want, option)
				}
			}
			if len(want) == 0 {
				t.Fatalf("the shared %s menu leaves nothing to show; the fixture withholds every option", key)
			}

			shown := showMenu(t, menuPageNamed(t, c.Page))
			if len(shown.tokens) != len(want) {
				t.Fatalf("the %s page shows %d options %v, the shared menu has %d to show",
					c.Page, len(shown.tokens), shown.tokens, len(want))
			}
			view := shown.view()
			for i, option := range want {
				if shown.tokens[i] != option.Value {
					t.Errorf("option %d writes %q, the shared menu has %q there", i+1, shown.tokens[i], option.Value)
				}
				if shown.labels[i] != option.Label {
					t.Errorf("option %d is labelled %q, the shared menu says %q", i+1, shown.labels[i], option.Label)
				}
				if shown.descriptions[i] != option.Description {
					t.Errorf("option %d is described %q, the shared menu says %q", i+1, shown.descriptions[i], option.Description)
				}
				if !strings.Contains(view, option.Label) || !strings.Contains(view, option.Description) {
					t.Errorf("the %s page does not render the shared label %q and description %q:\n%s",
						c.Page, option.Label, option.Description, view)
				}
				if got := shown.pick(i); got != option.Value {
					t.Errorf("pressing %d on the %s page selects %q, the shared menu has %q there",
						i+1, c.Page, got, option.Value)
				}
			}
		})
	}
}

type retentionPageCase struct {
	Name               string `yaml:"name"`
	HasExisting        bool   `yaml:"hasExisting"`
	CurrentDays        int    `yaml:"currentDays"`
	WantDays           int    `yaml:"wantDays"`
	WantNeverExpire    bool   `yaml:"wantNeverExpire"`
	WantCurrentSetting string `yaml:"wantCurrentSetting"`
	WantSummaryLine    string `yaml:"wantSummaryLine"`
}

type retentionPageDocument struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Cases         []retentionPageCase `yaml:"cases"`
}

//go:embed testdata/retention_page.yaml
var retentionPageYAML []byte

// TestRetentionPage_ReadsStoredPeriodsThroughTheSharedNeverExpire holds the page and
// the completion summary to one meaning of "never expire": the shared model's
// period to write and its floor to read.
func TestRetentionPage_ReadsStoredPeriodsThroughTheSharedNeverExpire(t *testing.T) {
	t.Parallel()
	var doc retentionPageDocument
	if err := testutil.DecodeNamedFixtureYAML(retentionPageYAML, &doc); err != nil {
		t.Fatalf("decode testdata/retention_page.yaml: %v", err)
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.WantNeverExpire == (c.WantDays != 0) {
				t.Fatalf("fixture case %q must set exactly one of wantDays and wantNeverExpire", c.Name)
			}
			page := NewRetentionPage("Retention", c.CurrentDays, c.HasExisting)

			want := c.WantDays
			if c.WantNeverExpire {
				want = onboarding.NeverExpireDays
			}
			if got := page.SelectedDays(); got != want {
				t.Errorf("the page opens on %d days, want %d", got, want)
			}
			if view := page.View(100, 60); !strings.Contains(view, c.WantCurrentSetting) {
				t.Errorf("the page does not show %q:\n%s", c.WantCurrentSetting, view)
			}

			summary := BuildSummaryContent(&WizardAnswers{ClaudeRetentionDays: c.CurrentDays})
			switch {
			case c.WantSummaryLine == "" && strings.Contains(summary, "Retention:"):
				t.Errorf("the summary prints a retention line for %d days, want none:\n%s", c.CurrentDays, summary)
			case c.WantSummaryLine != "" && !strings.Contains(summary, c.WantSummaryLine):
				t.Errorf("the summary does not contain %q:\n%s", c.WantSummaryLine, summary)
			}
		})
	}
}
