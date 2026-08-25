//go:build guided_screenshots

package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

// validatePNG (main.go) only compares a produced PNG's DECLARED dimensions
// against the fixture's DECLARED dimensions. Because Freeze is invoked with
// that same declared height, that comparison is trivially satisfied even
// when Freeze has silently cropped real content off the bottom of the
// canvas -- a capture row can be entirely missing from the rasterized PNG
// while the gate exits zero. validateContentFits closes that gap: it
// renders the sheet's REAL ansi content a second time onto a deliberately
// oversized, disposable canvas, measures where the content actually ends by
// inspecting the rasterized pixels, and fails when that measured height
// would not fit inside the fixture's declared viewport -- naming the first
// capture row that would be cropped.
//
// contentOverflowMeasurementPxPerLine and contentOverflowMeasurementFloorPx
// size that disposable canvas. The multiplier is deliberately generous:
// observed rendering is roughly 13px per terminal row (11pt font, 1.2 line
// height), so assuming up to 40px per row leaves a wide safety margin
// against the measurement canvas itself ever clipping the content it exists
// to measure. contentOverflowMeasurementMarginPx is the backstop: if
// measured content still reaches within this many pixels of the disposable
// canvas's OWN bottom edge, the measurement cannot be trusted (the canvas
// itself may be too small), and validateContentFits fails loudly instead of
// silently reporting "fits" -- the same discipline the check exists to
// enforce on the real gate.
const (
	contentOverflowMeasurementPxPerLine = 40
	contentOverflowMeasurementFloorPx   = 4000
	contentOverflowMeasurementMarginPx  = 200
	// contentOverflowPaddingTolerancePx bounds how far the measured top
	// padding may drift from freezePaddingPx before the pixel-per-line
	// derivation used to NAME the overflowing row is treated as unreliable.
	// It does not gate the overflow/no-overflow verdict itself, which is a
	// direct pixel comparison and needs no tolerance.
	contentOverflowPaddingTolerancePx = 4
)

// validateContentFits fails when a sheet's real rendered content would not
// fit inside its fixture's declared viewport height.
func validateContentFits(freezePath string, sheet renderedSheet) error {
	totalLines := strings.Count(sheet.ansi, "\n") + 1
	measurementHeight := totalLines*contentOverflowMeasurementPxPerLine + contentOverflowMeasurementFloorPx

	workDirectory, err := os.MkdirTemp("", "peasant-guided-screenshots-measure-")
	if err != nil {
		return fmt.Errorf("create content-fit measurement workspace for sheet %q: %w", sheet.fixture.Name, err)
	}
	defer os.RemoveAll(workDirectory)

	ansiPath := filepath.Join(workDirectory, string(sheet.fixture.Name)+".ansi")
	if err := os.WriteFile(ansiPath, []byte(sheet.ansi), 0o600); err != nil {
		return fmt.Errorf("write content-fit measurement input for sheet %q: %w", sheet.fixture.Name, err)
	}
	measurementPNG := filepath.Join(workDirectory, string(sheet.fixture.Name)+".png")

	ctx, cancel := context.WithTimeout(context.Background(), screenshotCommandTimeout)
	defer cancel()
	command := freezeCommand(ctx, freezePath, ansiPath, measurementPNG, sheet.fixture.Viewport.Width, measurementHeight)
	output, err := command.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("content-fit measurement render for sheet %q timed out after %s", sheet.fixture.Name, screenshotCommandTimeout)
	}
	if err != nil {
		return fmt.Errorf("content-fit measurement render for sheet %q: %w: %s", sheet.fixture.Name, err, strings.TrimSpace(string(output)))
	}

	file, err := os.Open(measurementPNG)
	if err != nil {
		return fmt.Errorf("open content-fit measurement PNG for sheet %q: %w", sheet.fixture.Name, err)
	}
	defer file.Close()
	decoded, err := png.Decode(file)
	if err != nil {
		return fmt.Errorf("decode content-fit measurement PNG for sheet %q: %w", sheet.fixture.Name, err)
	}

	background, err := parseHexColor(freezeBackgroundHex)
	if err != nil {
		return fmt.Errorf("parse Freeze background color %q: %w", freezeBackgroundHex, err)
	}

	return evaluateContentFit(sheet, decoded, background, measurementHeight)
}

// evaluateContentFit is the pure decision at the heart of validateContentFits,
// separated from the Freeze/file I/O above it so it can be exercised directly
// against a synthetic, already-decoded measurement image in tests without
// needing the Freeze binary. measurementHeight is the height of the
// disposable canvas measured was rendered onto (used only for the "canvas
// itself may be too small" backstop message).
func evaluateContentFit(sheet renderedSheet, measured image.Image, background color.Color, measurementHeight int) error {
	firstContentRow, lastContentRow, found := scanContentRows(measured, background)
	if !found {
		return fmt.Errorf(
			"content-fit measurement for sheet %q found no non-background pixels in its %dx%d disposable measurement canvas; "+
				"this check cannot verify content fits without a legible render; "+
				"fix: inspect the composed ANSI content or the Freeze invocation for sheet %q directly",
			sheet.fixture.Name, sheet.fixture.Viewport.Width, measurementHeight, sheet.fixture.Name,
		)
	}

	if measurementHeight-lastContentRow <= contentOverflowMeasurementMarginPx {
		return fmt.Errorf(
			"content-fit measurement for sheet %q rendered content within %dpx of its own %dpx-tall disposable measurement canvas's bottom edge; "+
				"the measurement canvas may itself be too small to trust this result, which would let real overflow pass silently; "+
				"fix: raise contentOverflowMeasurementPxPerLine or contentOverflowMeasurementFloorPx in cmd/peasant-guided-screenshots/contentfit.go and rerun",
			sheet.fixture.Name, contentOverflowMeasurementMarginPx, measurementHeight,
		)
	}

	requiredHeight := lastContentRow + 1 + firstContentRow
	configuredHeight := sheet.fixture.Viewport.Height
	if requiredHeight <= configuredHeight {
		return nil
	}

	overflowPx := requiredHeight - configuredHeight
	label, line, lines := attributeOverflow(sheet, firstContentRow, lastContentRow, configuredHeight)

	return fmt.Errorf(
		"contact sheet %q needs at least %dpx of height to show its content without cropping, but its fixture declares only %dpx (%dpx short); "+
			"%s is the first capture row that falls outside the declared canvas (around content line %d of %d); "+
			"a viewer of the rasterized PNG would see it -- and everything after it -- silently cropped from the reviewed artefact; "+
			"fix: grow sheet %q's viewport.height in cmd/peasant-guided-screenshots/testdata/captures.yaml to at least %dpx",
		sheet.fixture.Name, requiredHeight, configuredHeight, overflowPx,
		label, line, lines,
		sheet.fixture.Name, requiredHeight,
	)
}

// attributeOverflow names the first composed row that would fall outside
// configuredHeight. It derives pixels-per-line directly from this
// measurement pass (contentSpanPx / totalLines) rather than assuming a
// fixed font metric, so the derivation stays correct if font size, line
// height, or padding ever change. This attribution is a best-effort label
// for the error message; the overflow/no-overflow verdict above it is a
// direct pixel comparison and does not depend on this function.
func attributeOverflow(sheet renderedSheet, firstContentRow, lastContentRow, configuredHeight int) (label string, line, totalLines int) {
	totalLines = strings.Count(sheet.ansi, "\n") + 1
	const unresolved = "an unidentified capture row (line attribution unavailable for this measurement)"

	if diff := firstContentRow - freezePaddingPx; diff > contentOverflowPaddingTolerancePx || diff < -contentOverflowPaddingTolerancePx {
		// The measured top padding does not match what freezeCommand was
		// asked to render. The line-attribution math below assumes the
		// measurement's padding matches the real render's padding; when it
		// doesn't, naming a specific row would be a guess dressed up as a
		// measurement, so name none rather than mislead.
		return unresolved, 0, totalLines
	}

	contentSpanPx := lastContentRow - firstContentRow + 1
	if totalLines <= 0 || contentSpanPx <= 0 {
		return unresolved, 0, totalLines
	}
	pxPerLine := float64(contentSpanPx) / float64(totalLines)
	if pxPerLine <= 0 {
		return unresolved, 0, totalLines
	}

	availableContentPx := configuredHeight - 2*firstContentRow
	availableLines := int(float64(availableContentPx) / pxPerLine)
	if availableLines < 0 {
		availableLines = 0
	}
	overflowLine := availableLines // 0-based index of the first line that does not fit

	const headerLines = 2 // renderContactSheet's title line plus its blank separator line
	cursor := headerLines
	for i, row := range sheet.rows {
		if overflowLine < cursor+row.lines {
			return row.label, overflowLine - cursor + 1, totalLines
		}
		cursor += row.lines
		if i != len(sheet.rows)-1 {
			cursor++ // the blank separator line strings.Join(rows, "\n\n") inserts between rows
		}
	}
	if len(sheet.rows) > 0 {
		last := sheet.rows[len(sheet.rows)-1]
		return last.label, last.lines, totalLines
	}
	return unresolved, overflowLine, totalLines
}

// scanContentRows returns the first and last pixel rows in img that carry
// any pixel differing from background. found is false only when the whole
// image is background, which should never happen for a real sheet and is
// reported as a distinct, honest failure rather than treated as "fits".
func scanContentRows(img image.Image, background color.Color) (first, last int, found bool) {
	bounds := img.Bounds()
	br, bg, bb, ba := background.RGBA()

	rowHasContent := func(y int) bool {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if r != br || g != bg || b != bb || a != ba {
				return true
			}
		}
		return false
	}

	first = -1
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if rowHasContent(y) {
			first = y
			break
		}
	}
	if first == -1 {
		return 0, 0, false
	}
	for y := bounds.Max.Y - 1; y >= bounds.Min.Y; y-- {
		if rowHasContent(y) {
			last = y
			break
		}
	}
	return first, last, true
}

// parseHexColor parses a "#rrggbb" string as an opaque color.Color, matching
// the literal --background value passed to Freeze.
func parseHexColor(hex string) (color.Color, error) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return nil, fmt.Errorf("hex color %q is not six hex digits", hex)
	}
	var r, g, b uint8
	if _, err := fmt.Sscanf(hex, "%02x%02x%02x", &r, &g, &b); err != nil {
		return nil, fmt.Errorf("parse hex color %q: %w", hex, err)
	}
	return color.RGBA{R: r, G: g, B: b, A: 0xff}, nil
}
