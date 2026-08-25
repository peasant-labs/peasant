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

// TestCaptureFixtureGuardsRequiredOriginHiddenStateDeletion mutation-proves
// that the closed set of selection states still requires origin-hidden even
// without a bare state-count guard.
func TestCaptureFixtureGuardsRequiredOriginHiddenStateDeletion(t *testing.T) {
	old := []byte("  - key: origin-hidden\n    query: \"\"\n" +
		"    # \"user driv\" (not the full title) survives the row-label truncation the\n" +
		"    # narrower 80-column capture applies, so only these markers can be asserted\n" +
		"    # at BOTH widths.\n" +
		"    #\n" +
		"    # The child-session badge is width-dependent and is now GATED rather than\n" +
		"    # left to a human eye: the parent's listing carries subagent ids while the\n" +
		"    # cohort holds no child at all, exactly as production discovery hands it\n" +
		"    # over, so the wider capture can only show this count by resolving the\n" +
		"    # discovered subagent relation. The narrower capture drops the badge\n" +
		"    # entirely rather than truncating it, which narrowWantAbsent pins.\n" +
		"    wantContains: [\"user driv\", \"parent ses\"]\n" +
		"    wideWantContains: [\"+ 2 child sessions\"]\n" +
		"    narrowWantAbsent: [\"child sessions\"]\n" +
		"    wantAbsent: [\"agent driven session\"]\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("origin-hidden state block mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required origin-hidden selection state")
	}
	if !strings.Contains(err.Error(), `omits selection state "origin-hidden"`) {
		t.Fatalf("deleted-required-state error = %v, want it to name the missing state", err)
	}
}

// TestCaptureFixtureGuardsOriginHiddenWantAbsent mutation-proves that
// decode itself requires the origin-hidden state to declare a wantAbsent
// marker: a state with no forbidden marker would let a broken origin filter
// (one that stopped hiding agent rows) pass unnoticed.
func TestCaptureFixtureGuardsOriginHiddenWantAbsent(t *testing.T) {
	old := []byte("    wantAbsent: [\"agent driven session\"]\n")
	if bytes.Count(captureFixtureData, old) != 1 {
		t.Fatalf("origin-hidden wantAbsent mutation source occurs %d times, want exactly one", bytes.Count(captureFixtureData, old))
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted an origin-hidden state with no wantAbsent marker")
	}
	if !strings.Contains(err.Error(), "declares no wantAbsent marker") {
		t.Fatalf("missing-wantAbsent error = %v, want it to explain the gap", err)
	}
}

// TestCaptureFixtureGuardsOriginHiddenWidthMarkers mutation-proves that decode
// itself requires BOTH width-specific markers on the origin-hidden state.
// Without them the child-session badge would be back to a human eyeball: the
// wider capture would assert nothing only it can show, and nothing would pin
// that the narrower capture drops the badge instead of truncating it.
func TestCaptureFixtureGuardsOriginHiddenWidthMarkers(t *testing.T) {
	for _, probe := range []struct {
		name   string
		source string
		want   string
	}{
		{"wide", "    wideWantContains: [\"+ 2 child sessions\"]\n", "declares no wideWantContains marker"},
		{"narrow", "    narrowWantAbsent: [\"child sessions\"]\n", "declares no narrowWantAbsent marker"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			source := []byte(probe.source)
			if count := bytes.Count(captureFixtureData, source); count != 1 {
				t.Fatalf("%s marker mutation source occurs %d times, want exactly one", probe.name, count)
			}
			mutated := bytes.Replace(captureFixtureData, source, nil, 1)
			_, err := decodeCaptureDocument(mutated)
			if err == nil {
				t.Fatalf("screenshot fixture accepted an origin-hidden state with no %s marker", probe.name)
			}
			if !strings.Contains(err.Error(), probe.want) {
				t.Fatalf("missing-%s-marker error = %v, want it to explain the gap", probe.name, err)
			}
		})
	}
}

// TestCaptureFixtureGuardsRequiredOriginSessionDeletion mutation-proves the
// origin-agent-hidden and origin-user-visible required-name entries: deleting
// either listing must fail decode naming it, so the hiding capture cannot
// silently lose its subject or its control.
func TestCaptureFixtureGuardsRequiredOriginSessionDeletion(t *testing.T) {
	old := []byte("    - harness: claude-code\n      projectName: acme/tool\n      gitRemote: \"git@github.com:acme/tool.git\"\n      branch: main\n      title: agent driven session\n      workingDir: /fixtures/worktrees/pasture-a\n      sessionId: origin-agent-hidden\n      origin: agent\n")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("origin-agent-hidden session mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, nil, 1)
	_, err := decodeCaptureDocument(mutated)
	if err == nil {
		t.Fatal("screenshot fixture accepted a corpus missing the required origin-agent-hidden session")
	}
	if !strings.Contains(err.Error(), `missing required selection session "origin-agent-hidden"`) {
		t.Fatalf("deleted-required-session error = %v, want it to name the missing session", err)
	}
}

// TestCaptureFixtureRendersOriginHiddenViewActuallyHidesAgentRow is a
// production render (not just decode validation): it mounts the real
// selection tree over the fixture listings and asserts the agent-driven
// session is genuinely absent from the rendered screen while its user-origin
// control is present, at both terminal widths.
func TestCaptureFixtureRendersOriginHiddenViewActuallyHidesAgentRow(t *testing.T) {
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	var state selectionStateFixture
	for _, candidate := range document.SelectionStates {
		if candidate.Key == selectionStateOriginHidden {
			state = candidate
		}
	}
	if state.Key == "" {
		t.Fatal("origin-hidden selection state not found in fixture")
	}
	workingDirectory := t.TempDir()
	for _, size := range []struct{ width, height int }{{80, 24}, {120, 40}} {
		capture := selectionCaptureFixture{
			Name: "mutation-check", State: selectionStateOriginHidden, Theme: captureThemeDark,
			Width: size.width, Height: size.height,
		}
		view, err := renderSelectionCapture(workingDirectory, 0, document.Selection, capture, state)
		if err != nil {
			t.Fatalf("render selection capture at %dx%d: %v", size.width, size.height, err)
		}
		// Each width is reported on its own: the widths assert DIFFERENT things
		// about the child-session badge, so stopping at the first failure would
		// hide whichever one ran second.
		wantContains, wantAbsent := selectionStateExpectations(state, size.width)
		if err := validateTerminalCapture(capture.Name, view, size.width, size.height, wantContains, wantAbsent); err != nil {
			t.Errorf("%dx%d: %v", size.width, size.height, err)
		}
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
