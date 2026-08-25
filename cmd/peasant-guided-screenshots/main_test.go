//go:build guided_screenshots

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestCaptureFixtureMountsEveryAcceptedSheet(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	sheets, err := renderSheets(document)
	if err != nil {
		t.Fatal(err)
	}
	if len(sheets) != len(document.Sheets) {
		t.Fatalf("rendered sheets=%d, want %d (one per fixture sheet)", len(sheets), len(document.Sheets))
	}
	seen := make(map[sheetName]bool, len(sheets))
	for _, sheet := range sheets {
		if seen[sheet.fixture.Name] {
			t.Fatalf("rendered duplicate sheet %q", sheet.fixture.Name)
		}
		seen[sheet.fixture.Name] = true
		if !strings.Contains(ansi.Strip(sheet.ansi), sheet.fixture.Title) {
			t.Errorf("sheet %q omits title %q", sheet.fixture.Name, sheet.fixture.Title)
		}
	}
}

func TestCaptureFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), captureFixtureData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeCaptureDocument(mutated); err == nil {
		t.Fatal("screenshot fixture accepted an unknown field")
	}
}

func TestCaptureFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), captureFixtureData...), []byte("\n---\n{}\n")...)
	if _, err := decodeCaptureDocument(mutated); err == nil {
		t.Fatal("screenshot fixture accepted a second YAML document")
	}
}

// TestCaptureFixtureGuardsRequiredSheetDeletion mutation-proves that the
// closed set of four required sheets (see validateSheets) still catches a
// missing sheet even without a bare sheet-count guard: deleting the "push"
// sheet block must fail decode with a message naming the missing sheet.
func TestCaptureFixtureGuardsRequiredSheetDeletion(t *testing.T) {
	old := []byte("  - name: push\n    kind: push\n    title: \"mounted push wizard: start, selection, published-transcript preview, consent, and receipt in both themes\"\n    theme: dark\n    viewport: {width: 1800, height: 6000}\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("push-sheet mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required push sheet")
	}
	if !strings.Contains(err.Error(), `omits required sheet "push"`) {
		t.Fatalf("deleted-required-sheet error = %v, want it to name the missing sheet", err)
	}
}

// TestCaptureFixtureGuardsRequiredGuidedSectionDeletion mutation-proves that
// deleting the "publication" guided section (while the fixture continues to
// name it in the wantContains-derived required set) fails decode. This is
// the section the count-guard's own required-key loop previously omitted;
// converting away from the bare count made completing that list load-bearing.
func TestCaptureFixtureGuardsRequiredGuidedSectionDeletion(t *testing.T) {
	old := []byte("  - key: publication\n    wantContains: [publication preference, keep local]\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("publication-section mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required publication guided section")
	}
	if !strings.Contains(err.Error(), `omits guided section "publication"`) {
		t.Fatalf("deleted-required-section error = %v, want it to name the missing section", err)
	}
}

// TestCaptureFixtureGuardsRequiredSelectionSessionDeletion mutation-proves
// the selection session required-name manifest: deleting a required
// session's listing must fail decode naming that session.
func TestCaptureFixtureGuardsRequiredSelectionSessionDeletion(t *testing.T) {
	old := []byte("    - harness: cursor\n      projectName: acme/tool\n      gitRemote: \"git@github.com:acme/tool.git\"\n      branch: main\n      title: cursor session\n      workingDir: /fixtures/worktrees/pasture-b\n      sessionId: cur-1\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("cur-1 session mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing a required selection session")
	}
	if !strings.Contains(err.Error(), `missing required selection session "cur-1"`) {
		t.Fatalf("deleted-required-session error = %v, want it to name the missing session", err)
	}
}

// TestCaptureFixtureGuardsRequiredSelectionHarnessDeletion mutation-proves
// the selection harness required-name manifest: if the only cursor-harness
// listing is retargeted to claude-code, the fixture no longer demonstrates
// cross-harness selection coverage and decode must fail naming "cursor".
func TestCaptureFixtureGuardsRequiredSelectionHarnessDeletion(t *testing.T) {
	old := []byte("    - harness: cursor\n      projectName: acme/tool\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("cursor-harness mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, []byte("    - harness: claude-code\n      projectName: acme/tool\n"), 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required cursor selection harness")
	}
	if !strings.Contains(err.Error(), `missing required selection harness "cursor"`) {
		t.Fatalf("deleted-required-harness error = %v, want it to name the missing harness", err)
	}
}

// TestCaptureFixtureGuardsRequiredSelectionIngestedDeletion mutation-proves
// the selection ingested required-name manifest: removing cc-imported from
// the ingested list must fail decode naming it.
func TestCaptureFixtureGuardsRequiredSelectionIngestedDeletion(t *testing.T) {
	old := []byte("  ingested: [cc-imported]\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("ingested mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, []byte("  ingested: []\n"), 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required ingested session")
	}
	if !strings.Contains(err.Error(), `missing required selection ingested session "cc-imported"`) {
		t.Fatalf("deleted-required-ingested error = %v, want it to name the missing session", err)
	}
}

// TestCaptureFixtureGuardsRequiredPushSessionDeletion mutation-proves the
// push session required-name manifest: deleting a required push session
// must fail decode naming it.
func TestCaptureFixtureGuardsRequiredPushSessionDeletion(t *testing.T) {
	old := []byte("    - sessionId: push-raw-0003\n      harness: claude-code\n      project: schema\n      startMs: 1700002000000\n      redaction: raw\n      withheld: true\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("push-raw-0003 mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing a required push session")
	}
	if !strings.Contains(err.Error(), `missing required push session "push-raw-0003"`) {
		t.Fatalf("deleted-required-push-session error = %v, want it to name the missing session", err)
	}
}

// TestCaptureFixtureGuardsRequiredSelectionStateDeletion mutation-proves
// that the closed set of six required selection states (see
// validateSelectionMatrix) still catches a missing state even without a
// bare state-count guard.
func TestCaptureFixtureGuardsRequiredSelectionStateDeletion(t *testing.T) {
	old := []byte("  - key: harness-source-preview\n    query: \"\"\n    wantContains: [\"harness: claude code\", scrubbed-source-body, read in place]\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("harness-source-preview state mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required harness-source-preview selection state")
	}
	if !strings.Contains(err.Error(), `omits selection state "harness-source-preview"`) {
		t.Fatalf("deleted-required-state error = %v, want it to name the missing state", err)
	}
}

// TestCaptureFixtureGuardsRequiredPushStateDeletion mutation-proves that the
// closed set of five required push states (see validatePushMatrix) still
// catches a missing state even without a bare state-count guard.
func TestCaptureFixtureGuardsRequiredPushStateDeletion(t *testing.T) {
	old := []byte("  - key: receipt\n    wantContains: [\"to the village.\", \"nothing is removed from this machine.\", \"sessions in this push\"]\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("receipt state mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required receipt push state")
	}
	if !strings.Contains(err.Error(), `omits push state "receipt"`) {
		t.Fatalf("deleted-required-push-state error = %v, want it to name the missing state", err)
	}
}

func TestCaptureFixturePinsTheGuidedCrossProduct(t *testing.T) {
	row := []byte("  - {name: retention-light-120x40, section: retention, theme: light, width: 120, height: 40}\n")
	if count := bytes.Count(captureFixtureData, row); count != 1 {
		t.Fatalf("guided mutation source occurs %d times, want exactly one", count)
	}
	duplicate := []byte("  - {name: retention-dark-120x40, section: retention, theme: dark, width: 120, height: 40}\n")
	mutated := bytes.Replace(captureFixtureData, row, duplicate, 1)
	if _, err := decodeCaptureDocument(mutated); err == nil {
		t.Fatal("screenshot fixture accepted a missing guided matrix entry")
	}
}

func TestCaptureFixturePinsThePushCrossProduct(t *testing.T) {
	row := []byte("  - {name: push-receipt-light-120x40, state: receipt, theme: light, width: 120, height: 40}\n")
	if count := bytes.Count(captureFixtureData, row); count != 1 {
		t.Fatalf("push mutation source occurs %d times, want exactly one", count)
	}
	duplicate := []byte("  - {name: push-receipt-dark-120x40, state: receipt, theme: dark, width: 120, height: 40}\n")
	mutated := bytes.Replace(captureFixtureData, row, duplicate, 1)
	if _, err := decodeCaptureDocument(mutated); err == nil {
		t.Fatal("screenshot fixture accepted a missing push matrix entry")
	}
}
