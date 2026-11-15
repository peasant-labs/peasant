//go:build guided_screenshots

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestUpgradeProgressFixtureRequiresEveryNamedState(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, state := range document.UpgradeProgressStates {
		t.Run(string(state.Key), func(t *testing.T) {
			mutated := document.UpgradeProgressStates
			remaining := append(append([]upgradeProgressStateFixture(nil), mutated[:index]...), mutated[index+1:]...)
			if err := validateUpgradeProgressMatrix(remaining, document.UpgradeProgressCaptures); err == nil || !strings.Contains(err.Error(), string(state.Key)) {
				t.Fatalf("missing upgrade progress state was not rejected by name: %v", err)
			}
		})
	}
}

func TestUpgradeProgressFixtureRequiresEveryThemeAndSize(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, capture := range document.UpgradeProgressCaptures {
		t.Run(capture.Name, func(t *testing.T) {
			mutated := document.UpgradeProgressCaptures
			remaining := append(append([]upgradeProgressCaptureFixture(nil), mutated[:index]...), mutated[index+1:]...)
			if err := validateUpgradeProgressMatrix(document.UpgradeProgressStates, remaining); err == nil || !strings.Contains(err.Error(), capture.Name) {
				t.Fatalf("missing upgrade progress capture was not rejected by name: %v", err)
			}
		})
	}
}

// TestCaptureFixturePinsTheUpgradeProgressCrossProduct mutation-proves that
// the fixture rejects a duplicated matrix entry standing in for a missing
// one: swapping the light 120x40 done capture for a second copy of the dark
// one must fail decode.
func TestCaptureFixturePinsTheUpgradeProgressCrossProduct(t *testing.T) {
	row := []byte("  - {name: upgrade-progress-done-light-120x40, state: done, theme: light, width: 120, height: 40}\n")
	if count := bytes.Count(captureFixtureData, row); count != 1 {
		t.Fatalf("upgrade progress mutation source occurs %d times, want exactly one", count)
	}
	duplicate := []byte("  - {name: upgrade-progress-done-dark-120x40, state: done, theme: dark, width: 120, height: 40}\n")
	mutated := bytes.Replace(captureFixtureData, row, duplicate, 1)
	if _, err := decodeCaptureDocument(mutated); err == nil {
		t.Fatal("screenshot fixture accepted a missing upgrade progress matrix entry")
	}
}

func TestCaptureFixtureGuardsRequiredUpgradeProgressSheetDeletion(t *testing.T) {
	old := []byte("  - name: upgrade-progress\n    kind: upgrade-progress\n    title: \"mounted store upgrade progress: plan, running, stopped, refused, failed and done in both themes\"\n    theme: dark\n    viewport: {width: 1800, height: 14400}\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("upgrade progress sheet mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required upgrade-progress sheet")
	}
	if !strings.Contains(err.Error(), `omits required sheet "upgrade-progress"`) {
		t.Fatalf("deleted-required-sheet error = %v, want it to name the missing sheet", err)
	}
}

// TestCaptureFixtureRendersUpgradeProgressViews is a production render (not
// just decode validation): it mounts the real upgradeprogress.Model for every
// captured state and asserts the plan, the running counts, the honest
// uncounted indicator, and the terminal footers reach the mounted screen, in
// both themes at both terminal sizes.
func TestCaptureFixtureRendersUpgradeProgressViews(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[upgradeProgressState]upgradeProgressStateFixture, len(document.UpgradeProgressStates))
	for _, state := range document.UpgradeProgressStates {
		states[state.Key] = state
	}
	for _, capture := range document.UpgradeProgressCaptures {
		state := states[capture.State]
		view, renderErr := renderUpgradeProgressCapture(capture)
		if renderErr != nil {
			t.Fatalf("render upgrade progress capture %q: %v", capture.Name, renderErr)
		}
		if err := validateTerminalCapture(capture.Name, view, capture.Width, capture.Height, state.WantContains, state.WantAbsent); err != nil {
			t.Errorf("%s: %v\n%s", capture.Name, err, ansi.Strip(view))
		}
		plain := ansi.Strip(view)
		if lines := strings.Split(plain, "\n"); len(lines) != capture.Height {
			t.Errorf("%s: rendered %d rows, want exactly %d terminal rows", capture.Name, len(lines), capture.Height)
		}
	}
}
