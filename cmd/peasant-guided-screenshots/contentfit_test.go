//go:build guided_screenshots

package main

import (
	"image"
	"image/color"
	"strings"
	"testing"
)

// These tests exercise the overflow-detection logic (scanContentRows,
// evaluateContentFit, attributeOverflow) directly against synthetically
// constructed images, WITHOUT invoking Freeze. validateContentFits itself
// (which shells out to Freeze to produce the measurement image) is exercised
// by the real `make guided-screenshots` run instead -- that path needs the
// Freeze binary on PATH, which this package's existing test suite has never
// required, and this file preserves that: it builds its own image.Image
// values in Go and feeds them straight to the pure decision functions.

var testBackground = color.RGBA{R: 0x17, G: 0x16, B: 0x14, A: 0xff}

// filledContentImage returns an image of the given size, filled with
// testBackground, with every pixel in rows [firstRow, lastRow] (inclusive)
// set to a color that differs from the background -- simulating a real
// Freeze render whose actual content spans exactly that pixel range.
func filledContentImage(width, height, firstRow, lastRow int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, testBackground)
		}
	}
	content := color.RGBA{R: 0xe0, G: 0xe0, B: 0xe0, A: 0xff}
	for y := firstRow; y <= lastRow; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, content)
		}
	}
	return img
}

func TestScanContentRowsFindsContentBounds(t *testing.T) {
	img := filledContentImage(50, 400, 28, 257)
	first, last, found := scanContentRows(img, testBackground)
	if !found {
		t.Fatal("scanContentRows reported no content in an image with a filled content band")
	}
	if first != 28 || last != 257 {
		t.Fatalf("scanContentRows = (first=%d, last=%d), want (28, 257)", first, last)
	}
}

func TestScanContentRowsReportsNotFoundOnAllBackground(t *testing.T) {
	img := filledContentImage(50, 200, 1, 0) // firstRow > lastRow: no content band drawn
	_, _, found := scanContentRows(img, testBackground)
	if found {
		t.Fatal("scanContentRows reported content in an image that is entirely background")
	}
}

// fixtureSheet builds a renderedSheet whose ansi line count and row metadata
// agree by construction: 2 header lines (renderContactSheet's title line and
// its blank separator), then each row's line count, with a single blank
// separator line between consecutive rows -- exactly matching how
// renderContactSheet and the composeXXXSheet functions actually assemble a
// sheet's content and row metadata. Content lines are the filler "x"; only
// their count matters to the functions under test.
func fixtureSheet(name sheetName, viewportHeight int, rows []sheetRow) renderedSheet {
	totalLines := 2
	for i, row := range rows {
		totalLines += row.lines
		if i != len(rows)-1 {
			totalLines++
		}
	}
	ansi := strings.TrimSuffix(strings.Repeat("x\n", totalLines), "\n")
	return renderedSheet{
		fixture: sheetFixture{
			Name:     name,
			Kind:     sheetKindGuided,
			Viewport: viewportFixture{Width: 1800, Height: viewportHeight},
		},
		ansi: ansi,
		rows: rows,
	}
}

// TestEvaluateContentFitPassesWhenContentFitsCanvas is the baseline: content
// that fits inside the declared canvas must not be flagged.
func TestEvaluateContentFitPassesWhenContentFitsCanvas(t *testing.T) {
	rows := []sheetRow{
		{label: `guided section "auto-ingest" (dark)`, lines: 10},
		{label: `guided section "retention" (dark)`, lines: 10},
	}
	sheet := fixtureSheet(sheetGuidedDark, 300, rows) // requires 286px; 300 fits
	img := filledContentImage(1800, 1000, 28, 257)    // first=28, last=257 -> requires 286px

	if err := evaluateContentFit(sheet, img, testBackground, 1000); err != nil {
		t.Fatalf("evaluateContentFit on content that fits = %v, want nil", err)
	}
}

// TestEvaluateContentFitFailsAndNamesOverflowingRowOnTooSmallCanvas is the
// mutation-proof for the defect this file exists to close: validatePNG's
// declared-vs-declared comparison alone lets a cropped sheet exit zero. This
// asserts the replacement check both fails AND names the first row that
// would be cropped, not merely that "content does not fit."
func TestEvaluateContentFitFailsAndNamesOverflowingRowOnTooSmallCanvas(t *testing.T) {
	rows := []sheetRow{
		{label: `guided section "auto-ingest" (dark)`, lines: 10},
		{label: `guided section "retention" (dark)`, lines: 10},
	}
	sheet := fixtureSheet(sheetGuidedDark, 200, rows) // declares 200px; content needs 286px
	img := filledContentImage(1800, 1000, 28, 257)    // real content: first=28, last=257

	err := evaluateContentFit(sheet, img, testBackground, 1000)
	if err == nil {
		t.Fatal("evaluateContentFit accepted a sheet whose declared canvas is smaller than its measured content")
	}
	message := err.Error()
	for _, want := range []string{
		`sheet "guided-dark"`,
		"needs at least 286px",
		"declares only 200px",
		"86px short",
		`guided section "retention" (dark)`,
		"grow sheet",
		"at least 286px",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("evaluateContentFit error = %q, want it to contain %q", message, want)
		}
	}
}

// TestEvaluateContentFitNamesTheFirstRowNotTheLast proves the row
// attribution is not vacuously always "the last row": shrinking the canvas
// only enough to clip the SECOND row must still name the second row, not the
// first, and a canvas that clips inside the FIRST row must name the first.
func TestEvaluateContentFitNamesTheFirstRowNotTheLast(t *testing.T) {
	rows := []sheetRow{
		{label: "row-one", lines: 10},
		{label: "row-two", lines: 10},
	}
	img := filledContentImage(1800, 1000, 28, 257) // pxPerLine = 230/23 = 10

	// Configured just past row-one's content (row-one ends at content line
	// 12 of 23; giving room for content lines 0..12 keeps the cut inside
	// row-two) still names row-two when the cut lands past row-one's span.
	sheetB := fixtureSheet(sheetGuidedDark, 258, rows) // requires 286; short by 28
	errB := evaluateContentFit(sheetB, img, testBackground, 1000)
	if errB == nil || !strings.Contains(errB.Error(), "row-two") {
		t.Fatalf("evaluateContentFit on a canvas cut inside row-two = %v, want it to name row-two", errB)
	}

	// A canvas so short the cut lands inside row-one must name row-one, not
	// row-two -- proving attribution is not hardcoded to the last row.
	sheetA := fixtureSheet(sheetGuidedDark, 100, rows) // far short of 286px
	errA := evaluateContentFit(sheetA, img, testBackground, 1000)
	if errA == nil || !strings.Contains(errA.Error(), "row-one") {
		t.Fatalf("evaluateContentFit on a canvas cut inside row-one = %v, want it to name row-one", errA)
	}
	if errA != nil && strings.Contains(errA.Error(), "row-two") {
		t.Fatalf("evaluateContentFit on a canvas cut inside row-one named row-two too: %v", errA)
	}
}

// TestEvaluateContentFitDistrustsATooSmallMeasurementCanvas is the backstop:
// if the measured content reaches too close to the disposable measurement
// canvas's own bottom edge, the measurement itself cannot be trusted (it may
// have clipped the very thing it exists to measure), and the function must
// fail loudly rather than silently report "fits."
func TestEvaluateContentFitDistrustsATooSmallMeasurementCanvas(t *testing.T) {
	rows := []sheetRow{{label: "only-row", lines: 10}}
	sheet := fixtureSheet(sheetGuidedDark, 5000, rows)
	measurementHeight := 300
	img := filledContentImage(1800, measurementHeight, 28, 257) // 43px from the canvas's own edge

	err := evaluateContentFit(sheet, img, testBackground, measurementHeight)
	if err == nil {
		t.Fatal("evaluateContentFit trusted a measurement whose content reached its own disposable canvas's edge")
	}
	if !strings.Contains(err.Error(), "measurement canvas may itself be too small") {
		t.Fatalf("evaluateContentFit error = %q, want it to explain the measurement canvas may be untrustworthy", err.Error())
	}
}

// TestEvaluateContentFitFailsOnAllBackgroundMeasurement proves the
// found=false path (an entirely blank measurement) is reported as a
// distinct, honest failure rather than silently treated as "fits."
func TestEvaluateContentFitFailsOnAllBackgroundMeasurement(t *testing.T) {
	rows := []sheetRow{{label: "only-row", lines: 10}}
	sheet := fixtureSheet(sheetGuidedDark, 5000, rows)
	img := filledContentImage(1800, 1000, 1, 0) // no content band: all background

	err := evaluateContentFit(sheet, img, testBackground, 1000)
	if err == nil {
		t.Fatal("evaluateContentFit reported success on an all-background measurement image")
	}
	if !strings.Contains(err.Error(), "found no non-background pixels") {
		t.Fatalf("evaluateContentFit error = %q, want it to explain the measurement found nothing", err.Error())
	}
}
