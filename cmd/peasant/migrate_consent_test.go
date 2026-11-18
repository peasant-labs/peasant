package main

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migrate_consent.yaml
var migrateConsentCases []byte

//go:embed testdata/migrate_consent.manifest.yaml
var migrateConsentManifest []byte

func TestMigrateConsentReader(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name     string `yaml:"name"`
			Input    string `yaml:"input"`
			Consent  bool   `yaml:"consent"`
			Reprompt bool   `yaml:"reprompt"`
			Error    bool   `yaml:"error"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(migrateConsentCases, &fixture); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(migrateConsentManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, c := range fixture.Cases {
		present[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := readMigrateConsent(strings.NewReader(c.Input), &out)
			if got != c.Consent || (err != nil) != c.Error {
				t.Fatalf("consent = %v, error = %v", got, err)
			}
			if c.Error && !strings.Contains(err.Error(), "--confirm") {
				t.Fatalf("error lacks recovery: %v", err)
			}
			if c.Reprompt && !strings.Contains(out.String(), "y/yes") {
				t.Fatalf("no accepted answers on retry: %s", out.String())
			}
		})
	}
	if err := testutil.RequireFixtureNames("migration consent", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
}

type consentErrorReader struct{}

func (consentErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("terminal read failed")
}

var _ io.Reader = consentErrorReader{}

func TestMigrateConsentReadFailure(t *testing.T) {
	var out bytes.Buffer
	agreed, err := readMigrateConsent(consentErrorReader{}, &out)
	if agreed || err == nil || !strings.Contains(err.Error(), "terminal read failed") || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("consent = %v, error = %v; want actionable read failure", agreed, err)
	}
}
