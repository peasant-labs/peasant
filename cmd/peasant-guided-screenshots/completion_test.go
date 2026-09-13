//go:build guided_screenshots

package main

import (
	"strings"
	"testing"
)

func TestCompletionFixtureRequiresEveryNamedState(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, state := range document.Completion.States {
		t.Run(string(state.Key), func(t *testing.T) {
			mutated := document.Completion
			mutated.States = append(append([]completionStateFixture(nil), mutated.States[:index]...), mutated.States[index+1:]...)
			if err := validateCompletionMatrix(mutated); err == nil || !strings.Contains(err.Error(), string(state.Key)) {
				t.Fatalf("missing state was not rejected by name: %v", err)
			}
		})
	}
}

func TestCompletionFixtureRequiresEveryThemeAndSize(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for index, capture := range document.Completion.Captures {
		t.Run(capture.Name, func(t *testing.T) {
			mutated := document.Completion
			mutated.Captures = append(append([]completionCaptureFixture(nil), mutated.Captures[:index]...), mutated.Captures[index+1:]...)
			if err := validateCompletionMatrix(mutated); err == nil || !strings.Contains(err.Error(), capture.Name) {
				t.Fatalf("missing capture was not rejected by name: %v", err)
			}
		})
	}
}
