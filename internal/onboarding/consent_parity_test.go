package onboarding_test

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/onboarding"
)

//go:embed testdata/consent_parity.yaml
var consentParityData []byte

type consentCase struct {
	Name             string `yaml:"name"`
	Scope            string `yaml:"scope"`
	SelectionMode    string `yaml:"selectionMode"`
	VillageConnected bool   `yaml:"villageConnected"`
	ClaudeSessions   bool   `yaml:"claudeSessions"`
	RetentionDays    int    `yaml:"retentionDays"`
	License          string `yaml:"license"`
	SharePreference  string `yaml:"sharePreference"`
	AutoPublish      *bool  `yaml:"autoPublishIntent"`
	Visibility       string `yaml:"visibility"`
	Harnesses        map[string]struct {
		Projects []config.ProjectSelection `yaml:"projects"`
		Sessions []string                  `yaml:"sessions"`
	} `yaml:"harnesses"`
	Values    []string `yaml:"values"`
	Effects   []string `yaml:"effects"`
	Forbidden []string `yaml:"forbidden"`
}

type consentDocument struct {
	RequiredNames []string `yaml:"requiredNames"`
	// InternalWords are words of the implementation. No line of any summary
	// may carry one: the summary is read by the person who chose.
	InternalWords []string      `yaml:"internalWords"`
	Cases         []consentCase `yaml:"cases"`
}

func TestConsentParity(t *testing.T) {
	t.Parallel()
	var document consentDocument
	decodeFixture(t, "consent_parity.yaml", consentParityData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)
	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			scope := onboarding.ScopeFull
			if row.Scope == "selection" {
				scope = onboarding.ScopeSelection
			}
			model := onboarding.BuildModel(onboarding.Options{Scope: scope})
			cfg := config.BaseConfig()
			cfg.Selection.Mode = config.SelectionMode(row.SelectionMode)
			cfg.ClaudeRetentionDays = row.RetentionDays
			if row.License != "" || row.Name == "skipped-license-boundary" {
				cfg.Push.License = config.License(row.License)
			}
			if row.SharePreference != "" || row.Name == "keep-local" {
				cfg.Push.SharePreference = config.SharePreference(row.SharePreference)
			}
			if row.AutoPublish != nil {
				cfg.Push.AutoPublishIntent = *row.AutoPublish
			}
			if row.Visibility != "" {
				cfg.Push.Visibility = config.Visibility(row.Visibility)
			}
			if row.Harnesses != nil {
				cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{}
				for name, harness := range row.Harnesses {
					cfg.Selection.Harnesses[name] = config.SelectionHarnessConfig{Projects: harness.Projects, Sessions: harness.Sessions}
				}
			}
			summary, err := onboarding.BuildConsentSummary(model, onboarding.WriteContext{
				Config:           cfg,
				VillageConnected: row.VillageConnected,
				ClaudeSessions:   row.ClaudeSessions,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := yamlEqual(summary.Values, row.Values); err != nil {
				t.Fatalf("values: %v", err)
			}
			if err := yamlEqual(summary.Effects, row.Effects); err != nil {
				t.Fatalf("effects: %v", err)
			}
			joined := strings.Join(append(append([]string{}, summary.Values...), summary.Effects...), "\n")
			for _, forbidden := range append(append([]string{}, row.Forbidden...), document.InternalWords...) {
				if strings.Contains(joined, forbidden) {
					t.Errorf("summary contains forbidden %q:\n%s", forbidden, joined)
				}
			}
		})
	}
}
