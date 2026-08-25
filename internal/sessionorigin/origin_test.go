package sessionorigin_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/classification/menu.yaml
var menuFixtureBytes []byte

// requiredMenuCaseNames is the deletion guard for the boundary corpus.
var requiredMenuCaseNames = []string{
	"user_is_on_the_menu",
	"agent_is_on_the_menu",
	"unknown_is_on_the_menu",
	"the_empty_string_is_not_an_origin",
	"the_empty_string_error_says_no_default_may_be_substituted",
	"an_unlisted_token_is_refused",
	"casing_is_not_forgiven",
	"a_padded_token_is_refused",
	"a_refusal_names_the_accepted_menu",
}

type menuFixture struct {
	Cases []menuCase `yaml:"cases"`
}

type menuCase struct {
	Name          string `yaml:"name"`
	Value         string `yaml:"value"`
	Valid         bool   `yaml:"valid"`
	ErrorContains string `yaml:"error_contains"`
}

// LoadMenuFixtures decodes the boundary corpus and refuses a fixture that has
// lost a required case or declares a refusal with no needle to prove it by.
func LoadMenuFixtures(data []byte) (menuFixture, error) {
	var fixture menuFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return menuFixture{}, fmt.Errorf("decode menu fixture first document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return menuFixture{}, fmt.Errorf("menu fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" {
			return menuFixture{}, errors.New("menu fixture holds a case with no name")
		}
		if _, duplicate := names[tc.Name]; duplicate {
			return menuFixture{}, fmt.Errorf("menu fixture repeats case name %q", tc.Name)
		}
		names[tc.Name] = struct{}{}
		if tc.Valid == (tc.ErrorContains != "") {
			return menuFixture{}, fmt.Errorf("menu fixture case %q must carry error_contains if and only if the value is refused", tc.Name)
		}
	}
	for _, required := range requiredMenuCaseNames {
		if _, ok := names[required]; !ok {
			return menuFixture{}, fmt.Errorf("menu fixture is missing required case %q", required)
		}
	}
	return fixture, nil
}

func TestOriginMenuBoundary(t *testing.T) {
	fixture, err := LoadMenuFixtures(menuFixtureBytes)
	if err != nil {
		t.Fatalf("load menu fixture: %v", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			candidate := sessionorigin.Origin(tc.Value)
			if candidate.Valid() != tc.Valid {
				t.Errorf("Valid() = %t, want %t", candidate.Valid(), tc.Valid)
			}
			validateErr := candidate.Validate()
			parsed, parseErr := sessionorigin.Parse(tc.Value)
			if tc.Valid {
				if validateErr != nil {
					t.Errorf("Validate() = %v, want nil", validateErr)
				}
				if parseErr != nil {
					t.Errorf("Parse() error = %v, want nil", parseErr)
				}
				if parsed.String() != tc.Value {
					t.Errorf("Parse() = %q, want %q", parsed, tc.Value)
				}
				return
			}
			if validateErr == nil {
				t.Fatalf("Validate() accepted %q", tc.Value)
			}
			if !strings.Contains(validateErr.Error(), tc.ErrorContains) {
				t.Errorf("Validate() error %q does not contain %q", validateErr, tc.ErrorContains)
			}
			if parseErr == nil {
				t.Fatalf("Parse() accepted %q", tc.Value)
			}
			if parsed != "" {
				t.Errorf("Parse() returned %q alongside its error, want the zero value", parsed)
			}
		})
	}
}

// TestOriginMenuIsClosedOverAll proves the menu and the validator agree, in both
// directions, against the production slice rather than a restated list.
func TestOriginMenuIsClosedOverAll(t *testing.T) {
	for _, origin := range sessionorigin.All {
		if !origin.Valid() {
			t.Errorf("All lists %q but Valid() refuses it", origin)
		}
		if !strings.Contains(sessionorigin.Menu(), origin.String()) {
			t.Errorf("Menu() %q omits %q", sessionorigin.Menu(), origin)
		}
	}
	if sessionorigin.Origin("").Valid() {
		t.Error("the empty string is valid, but it marks the absence of an origin rather than one")
	}
}

func TestLoadMenuFixturesRejectsADeletedCase(t *testing.T) {
	fixture, err := LoadMenuFixtures(menuFixtureBytes)
	if err != nil {
		t.Fatalf("load menu fixture: %v", err)
	}
	var trimmed menuFixture
	trimmed.Cases = append(trimmed.Cases, fixture.Cases[1:]...)
	encoded, err := yaml.Marshal(trimmed)
	if err != nil {
		t.Fatalf("marshal trimmed fixture: %v", err)
	}
	if _, err := LoadMenuFixtures(encoded); err == nil {
		t.Fatal("loader accepted a fixture with a required case removed")
	} else if !strings.Contains(err.Error(), "missing required case") {
		t.Fatalf("loader refused for the wrong reason: %v", err)
	}
}

// TestAllSignalsAreDistinctAndValid keeps the deciding-signal set closed: a new
// signal has to be added to AllSignals, which is what the fixture arms derive
// their coverage requirement from.
func TestAllSignalsAreDistinctAndValid(t *testing.T) {
	seen := make(map[sessionorigin.Signal]struct{}, len(sessionorigin.AllSignals))
	for _, signal := range sessionorigin.AllSignals {
		if !signal.Valid() {
			t.Errorf("AllSignals lists %q but Valid() refuses it", signal)
		}
		if _, duplicate := seen[signal]; duplicate {
			t.Errorf("AllSignals repeats %q", signal)
		}
		seen[signal] = struct{}{}
	}
	if sessionorigin.Signal("parent linked").Valid() {
		t.Error("an unlisted signal was accepted")
	}
}
