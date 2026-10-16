//go:build guided_screenshots

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestPullProgressFixtureRequiresEveryNamedState mutation-proves that the
// closed set of three required pull-progress states (see
// validatePullProgressMatrix) still catches a missing state even without a
// bare state-count guard.
func TestPullProgressFixtureRequiresEveryNamedState(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, state := range document.PullProgressStates {
		t.Run(string(state.Key), func(t *testing.T) {
			mutated := document.PullProgressStates
			remaining := append(append([]pullProgressStateFixture(nil), mutated[:index]...), mutated[index+1:]...)
			if err := validatePullProgressMatrix(remaining, document.PullProgressCaptures); err == nil || !strings.Contains(err.Error(), string(state.Key)) {
				t.Fatalf("missing pull progress state was not rejected by name: %v", err)
			}
		})
	}
}

// TestPullProgressFixtureRequiresEveryThemeAndSize mutation-proves that
// removing any single pull-progress capture fails decode naming that
// capture: every state stays pinned in both themes at both terminal sizes.
func TestPullProgressFixtureRequiresEveryThemeAndSize(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, capture := range document.PullProgressCaptures {
		t.Run(capture.Name, func(t *testing.T) {
			mutated := document.PullProgressCaptures
			remaining := append(append([]pullProgressCaptureFixture(nil), mutated[:index]...), mutated[index+1:]...)
			if err := validatePullProgressMatrix(document.PullProgressStates, remaining); err == nil || !strings.Contains(err.Error(), capture.Name) {
				t.Fatalf("missing pull progress capture was not rejected by name: %v", err)
			}
		})
	}
}

// TestCaptureFixturePinsThePullProgressCrossProduct mutation-proves that the
// fixture rejects a duplicated matrix entry standing in for a missing one:
// swapping the light 120x40 canceled capture for a second copy of the dark
// one must fail decode.
func TestCaptureFixturePinsThePullProgressCrossProduct(t *testing.T) {
	row := []byte("  - {name: pull-progress-canceled-light-120x40, state: canceled, theme: light, width: 120, height: 40}\n")
	if count := bytes.Count(captureFixtureData, row); count != 1 {
		t.Fatalf("pull progress mutation source occurs %d times, want exactly one", count)
	}
	duplicate := []byte("  - {name: pull-progress-canceled-dark-120x40, state: canceled, theme: dark, width: 120, height: 40}\n")
	mutated := bytes.Replace(captureFixtureData, row, duplicate, 1)
	if _, err := decodeCaptureDocument(mutated); err == nil {
		t.Fatal("screenshot fixture accepted a missing pull progress matrix entry")
	}
}

// TestCaptureFixtureGuardsRequiredPullProgressSheetDeletion mutation-proves
// that the closed sheet set still catches a missing pull-progress sheet:
// deleting its block must fail decode naming the missing sheet.
func TestCaptureFixtureGuardsRequiredPullProgressSheetDeletion(t *testing.T) {
	old := []byte("  - name: pull-progress\n    kind: pull-progress\n    title: \"mounted collective pull progress: running, canceling, and canceled in both themes\"\n    theme: dark\n    viewport: {width: 1800, height: 3600}\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("pull-progress sheet mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required pull-progress sheet")
	}
	if !strings.Contains(err.Error(), `omits required sheet "pull-progress"`) {
		t.Fatalf("deleted-required-sheet error = %v, want it to name the missing sheet", err)
	}
}

// TestCaptureFixtureRendersPullProgressViewsActuallyShowCounts is a
// production render (not just decode validation): it mounts the real
// pullprogress.Model for every captured state and asserts the header, the
// retained done/total count, the bar cells, and the state's footer reach
// the mounted screen while the other states' footers stay absent, in both
// themes at both terminal widths.
func TestCaptureFixtureRendersPullProgressViewsActuallyShowCounts(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[pullProgressState]pullProgressStateFixture, len(document.PullProgressStates))
	for _, state := range document.PullProgressStates {
		states[state.Key] = state
	}
	for _, capture := range document.PullProgressCaptures {
		state := states[capture.State]
		view, renderErr := renderPullProgressCapture(capture)
		if renderErr != nil {
			t.Fatalf("render pull progress capture %q: %v", capture.Name, renderErr)
		}
		if err := validateTerminalCapture(capture.Name, view, capture.Width, capture.Height, state.WantContains, state.WantAbsent); err != nil {
			t.Errorf("%s: %v", capture.Name, err)
		}
		plain := ansi.Strip(view)
		if lines := strings.Split(plain, "\n"); len(lines) != capture.Height {
			t.Errorf("%s: rendered %d rows, want exactly %d terminal rows", capture.Name, len(lines), capture.Height)
		}
	}
}
