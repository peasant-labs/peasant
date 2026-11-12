//go:build guided_screenshots

package main

import (
	"strings"
	"testing"
)

func TestDiscoveryProgressFixtureRequiresEveryState(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, state := range document.DiscoveryProgressStates {
		t.Run(string(state.Key), func(t *testing.T) {
			remaining := append(append([]discoveryProgressStateFixture(nil), document.DiscoveryProgressStates[:index]...), document.DiscoveryProgressStates[index+1:]...)
			if err := validateDiscoveryProgressMatrix(remaining, document.DiscoveryProgressCaptures); err == nil || !strings.Contains(err.Error(), string(state.Key)) {
				t.Fatalf("missing discovery state was not rejected by name: %v", err)
			}
		})
	}
}

func TestDiscoveryProgressFixtureRequiresEveryThemeAndSize(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, capture := range document.DiscoveryProgressCaptures {
		t.Run(capture.Name, func(t *testing.T) {
			remaining := append(append([]discoveryProgressCaptureFixture(nil), document.DiscoveryProgressCaptures[:index]...), document.DiscoveryProgressCaptures[index+1:]...)
			if err := validateDiscoveryProgressMatrix(document.DiscoveryProgressStates, remaining); err == nil || !strings.Contains(err.Error(), capture.Name) {
				t.Fatalf("missing discovery capture was not rejected by name: %v", err)
			}
		})
	}
}

func TestDiscoveryProgressFixtureRequiresStateEvidence(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, state := range document.DiscoveryProgressStates {
		t.Run(string(state.Key), func(t *testing.T) {
			for markerIndex, marker := range state.WantContains {
				states := append([]discoveryProgressStateFixture(nil), document.DiscoveryProgressStates...)
				states[index].WantContains = append(append([]string(nil), state.WantContains[:markerIndex]...), state.WantContains[markerIndex+1:]...)
				if err := validateDiscoveryProgressMatrix(states, document.DiscoveryProgressCaptures); err == nil {
					t.Fatalf("discovery state without required marker %q was accepted", marker)
				}
			}
			if state.Key == discoveryProgressPhaseOnly {
				for markerIndex, marker := range state.WantAbsent {
					if marker != "(" && marker != "re-read" {
						continue
					}
					states := append([]discoveryProgressStateFixture(nil), document.DiscoveryProgressStates...)
					states[index].WantAbsent = append(append([]string(nil), state.WantAbsent[:markerIndex]...), state.WantAbsent[markerIndex+1:]...)
					if err := validateDiscoveryProgressMatrix(states, document.DiscoveryProgressCaptures); err == nil {
						t.Fatalf("phase-only state without exclusion %q was accepted", marker)
					}
				}
			}
		})
	}
}

func TestDiscoveryProgressFixtureRequiresSheet(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, sheet := range document.Sheets {
		if sheet.Name != sheetDiscovery {
			continue
		}
		remaining := append(append([]sheetFixture(nil), document.Sheets[:index]...), document.Sheets[index+1:]...)
		if err := validateSheets(remaining); err == nil || !strings.Contains(err.Error(), string(sheetDiscovery)) {
			t.Fatalf("missing discovery sheet was not rejected by name: %v", err)
		}
		return
	}
	t.Fatal("fixture has no discovery sheet to remove")
}

func TestDiscoveryProgressFixtureRendersMountedSpinner(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[discoveryProgressState]discoveryProgressStateFixture, len(document.DiscoveryProgressStates))
	for _, state := range document.DiscoveryProgressStates {
		states[state.Key] = state
	}
	for _, capture := range document.DiscoveryProgressCaptures {
		t.Run(capture.Name, func(t *testing.T) {
			state := states[capture.State]
			view, err := renderDiscoveryProgressCapture(capture, state)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateTerminalCapture(capture.Name, view, capture.Width, capture.Height, state.WantContains, state.WantAbsent); err != nil {
				t.Fatal(err)
			}
		})
	}
}
