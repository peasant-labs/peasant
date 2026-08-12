package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/kickstart_consent.yaml
var kickstartConsentYAML []byte

const requiredKickstartConsentRowCount = 5

type kickstartConsentDocument struct {
	DeclaredRows int                       `yaml:"declaredRows"`
	RequiredArms []string                  `yaml:"requiredArms"`
	Cases        []kickstartConsentFixture `yaml:"cases"`
}

type kickstartConsentFixture struct {
	ID              string   `yaml:"id"`
	Arm             string   `yaml:"arm"`
	ExistingUser    string   `yaml:"existingUser"`
	Destination     string   `yaml:"destination"`
	WarningContains []string `yaml:"warningContains"`
	ConsentContains []string `yaml:"consentContains"`
	ConsentExcludes []string `yaml:"consentExcludes"`
	SelectionTarget string   `yaml:"selectionTarget"`
}

func loadKickstartConsentFixtures(raw []byte) ([]kickstartConsentFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var document kickstartConsentDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode kickstart consent fixture: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("kickstart consent fixture must contain exactly one YAML document")
	}
	if document.DeclaredRows != requiredKickstartConsentRowCount || len(document.Cases) != requiredKickstartConsentRowCount {
		return nil, fmt.Errorf("kickstart consent fixture rows: declared=%d actual=%d required=%d",
			document.DeclaredRows, len(document.Cases), requiredKickstartConsentRowCount)
	}
	ids, arms := map[string]bool{}, map[string]bool{}
	for _, row := range document.Cases {
		if row.ID == "" || ids[row.ID] || row.Arm == "" || row.Destination == "" || len(row.ConsentContains) == 0 {
			return nil, fmt.Errorf("kickstart consent fixture has blank, duplicate, or vacuous row %q", row.ID)
		}
		ids[row.ID], arms[row.Arm] = true, true
	}
	for _, arm := range document.RequiredArms {
		if !arms[arm] {
			return nil, fmt.Errorf("kickstart consent fixture misses arm %q", arm)
		}
	}
	return document.Cases, nil
}

func mutateKickstartConsentFixture(t *testing.T, data, old, replacement []byte) []byte {
	t.Helper()
	if count := bytes.Count(data, old); count != 1 {
		t.Fatalf("kickstart consent mutation source %q occurs %d times, want exactly one", old, count)
	}
	return bytes.Replace(data, old, replacement, 1)
}

func TestBuildKickstartCommandMountsDestinationAndExactConsent(t *testing.T) {
	fixtures, err := loadKickstartConsentFixtures(kickstartConsentYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.ID, func(t *testing.T) {
			var warning, consent string
			command := buildKickstartCommand(kickstartCommandDeps{
				discover: func(context.Context, string, string, *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing) {
					return ftue.ProviderInventory{defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true}}, []ftue.SessionListing{{Harness: defaults.HarnessClaudeCode.String(), ProjectName: "tool", GitRemote: "https://github.com/acme/tool.git", Branch: "main", Title: "consent session", SessionID: "session-consent", WorkingDir: "/work/tool"}}
				},
				getwd:        func() (string, error) { return "/work/tool", nil },
				existingUser: func(string) string { return fixture.ExistingUser },
				run: func(model ftue.WizardModel) error {
					updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
					model = updated.(ftue.WizardModel)
					if fixture.ExistingUser == "" {
						updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
						model = updated.(ftue.WizardModel)
					}
					updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					model = updated.(ftue.WizardModel)
					if strings.Contains(model.View().Content, "[ ] tool") {
						updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeySpace})
						model = updated.(ftue.WizardModel)
					}
					updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					model = updated.(ftue.WizardModel)
					switch fixture.SelectionTarget {
					case "branch":
						for _, key := range []rune{tea.KeyDown, tea.KeySpace, tea.KeySpace} {
							updated, _ = model.Update(tea.KeyPressMsg{Code: key})
							model = updated.(ftue.WizardModel)
						}
					case "session":
						for _, key := range []rune{tea.KeyDown, tea.KeyRight, tea.KeyDown, tea.KeySpace, tea.KeySpace} {
							updated, _ = model.Update(tea.KeyPressMsg{Code: key})
							model = updated.(ftue.WizardModel)
						}
					}
					updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					model = updated.(ftue.WizardModel)
					for range 4 {
						updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
						model = updated.(ftue.WizardModel)
					}
					if fixture.Destination == "public" {
						updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
						model = updated.(ftue.WizardModel)
					}
					warning = model.View().Content
					updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					model = updated.(ftue.WizardModel)
					consent = model.View().Content
					return nil
				},
			})
			command.SetContext(t.Context())
			if executeErr := command.Execute(); executeErr != nil {
				t.Fatal(executeErr)
			}
			for _, want := range fixture.WarningContains {
				if !strings.Contains(warning, want) {
					t.Fatalf("warning omitted %q:\n%s", want, warning)
				}
			}
			for _, want := range fixture.ConsentContains {
				if !strings.Contains(consent, want) {
					t.Fatalf("consent omitted %q:\n%s", want, consent)
				}
			}
			for _, forbidden := range fixture.ConsentExcludes {
				if strings.Contains(consent, forbidden) {
					t.Fatalf("consent widened canonical scope with %q:\n%s", forbidden, consent)
				}
			}
		})
	}
}

func TestKickstartConsentFixtureStrictnessAndMutation(t *testing.T) {
	if _, err := loadKickstartConsentFixtures(append(kickstartConsentYAML, []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("loader accepted second document")
	}
	if _, err := loadKickstartConsentFixtures(bytes.Replace(kickstartConsentYAML, []byte("declaredRows:"), []byte("unknown: true\ndeclaredRows:"), 1)); err == nil {
		t.Fatal("loader accepted unknown field")
	}
	if _, err := loadKickstartConsentFixtures(bytes.Replace(kickstartConsentYAML, []byte("arm: authenticated-public"), []byte("arm: authenticated-private"), 1)); err == nil {
		t.Fatal("loader accepted missing public arm")
	}
}

func TestKickstartConsentFixtureRejectsCoordinatedSessionSelectionRemoval(t *testing.T) {
	t.Parallel()

	mutated := mutateKickstartConsentFixture(t, kickstartConsentYAML,
		[]byte("declaredRows: 5"), []byte("declaredRows: 4"))
	mutated = mutateKickstartConsentFixture(t, mutated,
		[]byte("requiredArms: [logged-out-local, authenticated-private, authenticated-public, branch-selection, session-selection]"),
		[]byte("requiredArms: [logged-out-local, authenticated-private, authenticated-public, branch-selection]"))
	mutated = mutateKickstartConsentFixture(t, mutated,
		[]byte("  - id: session selection consent\n    arm: session-selection\n    existingUser: \"\"\n    destination: local\n    selectionTarget: session\n    consentContains: [\"Claude Code/remote:github.com/acme/tool/main: consent session [session-consent]\"]\n"), nil)
	if _, err := loadKickstartConsentFixtures(mutated); err == nil {
		t.Fatal("kickstart consent fixture accepted removal of the session-selection regression row coordinated with its declarations")
	}
}
