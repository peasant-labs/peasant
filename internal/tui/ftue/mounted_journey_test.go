package ftue

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/mounted_journey.yaml
var mountedJourneyYAML []byte

type mountedJourneyDocument struct {
	DeclaredRows   int                     `yaml:"declaredRows"`
	RequiredArms   []string                `yaml:"requiredArms"`
	ReusedFixtures []string                `yaml:"reusedFixtures"`
	Cases          []mountedJourneyFixture `yaml:"cases"`
}

type mountedJourneyFixture struct {
	ID                  string               `yaml:"id"`
	Arm                 string               `yaml:"arm"`
	Connected           bool                 `yaml:"connected"`
	Keys                []string             `yaml:"keys"`
	Destination         Destination          `yaml:"destination"`
	Authentication      AuthenticationChoice `yaml:"authentication"`
	RequestedVisibility schema.Visibility    `yaml:"requestedVisibility"`
	EffectiveVisibility schema.Visibility    `yaml:"effectiveVisibility"`
	Contains            []string             `yaml:"contains"`
}

func loadMountedJourneyFixtures(raw []byte) (mountedJourneyDocument, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var document mountedJourneyDocument
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode mounted journey fixture: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return document, fmt.Errorf("mounted journey fixture must contain exactly one YAML document")
	}
	if document.DeclaredRows != len(document.Cases) || document.DeclaredRows < 4 || len(document.ReusedFixtures) != 3 {
		return document, fmt.Errorf("mounted journey fixture is vacuous: declared=%d actual=%d reused=%d", document.DeclaredRows, len(document.Cases), len(document.ReusedFixtures))
	}
	ids, arms := map[string]bool{}, map[string]bool{}
	for _, row := range document.Cases {
		if row.ID == "" || ids[row.ID] || row.Arm == "" || !row.Destination.IsValid() || !row.Authentication.IsValid() || len(row.Keys) == 0 {
			return document, fmt.Errorf("mounted journey fixture has blank, duplicate, or invalid row %q", row.ID)
		}
		ids[row.ID], arms[row.Arm] = true, true
	}
	for _, arm := range document.RequiredArms {
		if !arms[arm] {
			return document, fmt.Errorf("mounted journey fixture misses required arm %q", arm)
		}
	}
	return document, nil
}

func TestMountedDestinationJourney(t *testing.T) {
	document, err := loadMountedJourneyFixtures(mountedJourneyYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range document.ReusedFixtures {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("referenced selector/config/diagnostic fixture %q is unavailable: %v", path, statErr)
		}
	}
	for _, row := range document.Cases {
		t.Run(row.ID, func(t *testing.T) {
			page := NewDestinationPage(row.Connected, nil)
			for _, keyName := range row.Keys {
				key := tea.KeyEnter
				switch keyName {
				case "up":
					key = tea.KeyUp
				case "down":
					key = tea.KeyDown
				}
				updated, _ := page.Update(tea.KeyPressMsg{Code: key})
				page = updated.(*DestinationPage)
			}
			if page.Destination() != row.Destination || page.Authentication() != row.Authentication || page.RequestedVisibility() != row.RequestedVisibility || page.EffectiveVisibility() != row.EffectiveVisibility {
				t.Fatalf("destination state=%s/%s/%s/%s want %s/%s/%s/%s", page.Destination(), page.Authentication(), page.RequestedVisibility(), page.EffectiveVisibility(), row.Destination, row.Authentication, row.RequestedVisibility, row.EffectiveVisibility)
			}
			view := page.View(100, 30)
			for _, text := range row.Contains {
				if !strings.Contains(view, text) {
					t.Fatalf("mounted destination omitted %q:\n%s", text, view)
				}
			}
		})
	}
}

func TestMountedJourneyFixtureStrictnessAndMutation(t *testing.T) {
	if _, err := loadMountedJourneyFixtures(append(mountedJourneyYAML, []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("loader accepted second document")
	}
	if _, err := loadMountedJourneyFixtures(bytes.Replace(mountedJourneyYAML, []byte("declaredRows:"), []byte("unknown: true\ndeclaredRows:"), 1)); err == nil {
		t.Fatal("loader accepted unknown field")
	}
	if _, err := loadMountedJourneyFixtures(bytes.Replace(mountedJourneyYAML, []byte("arm: public-warning"), []byte("arm: private-default"), 1)); err == nil {
		t.Fatal("loader accepted missing public-warning arm")
	}
}
