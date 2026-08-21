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
	if len(sheets) != requiredSheetCount {
		t.Fatalf("rendered sheets=%d, want %d", len(sheets), requiredSheetCount)
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

func TestCaptureFixturePinsDeclaredCounts(t *testing.T) {
	old := []byte("expectedSheetCount: 4")
	if count := bytes.Count(captureFixtureData, old); count != 1 {
		t.Fatalf("sheet-count mutation source occurs %d times, want exactly one", count)
	}
	mutated := bytes.Replace(captureFixtureData, old, []byte("expectedSheetCount: 5"), 1)
	if _, err := decodeCaptureDocument(mutated); err == nil {
		t.Fatal("screenshot fixture accepted a changed sheet-count declaration")
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
