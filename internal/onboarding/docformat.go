package onboarding

import (
	"bytes"
	"reflect"
	"regexp"
	"strings"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// spacedComment matches the whitespace between a value and its trailing comment.
// Indentation before a comment-only line is not matched: it is structure.
var spacedComment = regexp.MustCompile(`(\S)[ \t]+#`)

// formatKey is the line with the spacing the YAML encoder normalises taken out:
// trailing whitespace, and the run of spaces before a trailing comment.
func formatKey(line string) string {
	return spacedComment.ReplaceAllString(strings.TrimRight(line, " \t"), "$1 #")
}

// restoreDocumentFormat gives an edited document back the formatting of the
// original that the YAML encoder drops: blank lines, and the spacing before a
// trailing comment. encoded is the encoder's output for the edited document;
// the result has the same meaning and, wherever a line differs from the original
// only in that spacing, the original's text.
//
// Lines are aligned with the line comparison DiffDocuments uses, on keys that
// ignore the normalised spacing. An aligned line is written as the original
// line. A changed or added line is written as the encoder produced it. A blank
// run stays before the line it preceded, or before whatever replaced that line.
//
// The result is the encoder's output unchanged when restoring is not safe: the
// original has carriage returns (its line endings would mix), the encoder wrote
// a blank line (it sits inside a block scalar, where blank lines are content),
// the document is too large to align, or the restored text decodes to anything
// other than what the encoder wrote.
func restoreDocumentFormat(original, encoded []byte) []byte {
	if len(original) == 0 || bytes.IndexByte(original, '\r') >= 0 {
		return encoded
	}
	encodedLines := strings.Split(strings.TrimSuffix(string(encoded), "\n"), "\n")
	for _, line := range encodedLines {
		if strings.TrimSpace(line) == "" {
			return encoded
		}
	}
	var originalLines []string
	var blanksBefore []int
	blanks := 0
	for _, line := range strings.Split(strings.TrimSuffix(string(original), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			blanks++
			continue
		}
		originalLines = append(originalLines, line)
		blanksBefore = append(blanksBefore, blanks)
		blanks = 0
	}
	if len(originalLines)*len(encodedLines) > diffMaxLCSCells {
		return encoded
	}

	var out strings.Builder
	pending := 0
	writeLine := func(line string, blankLines int) {
		for ; blankLines > 0; blankLines-- {
			out.WriteByte('\n')
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	for _, op := range diffOperations(formatKeys(originalLines), formatKeys(encodedLines)) {
		switch op.kind {
		case schema.LocalKickstartDiffCtx:
			writeLine(originalLines[op.oldPos], max(pending, blanksBefore[op.oldPos]))
			pending = 0
		case schema.LocalKickstartDiffDel:
			pending = max(pending, blanksBefore[op.oldPos])
		case schema.LocalKickstartDiffAdd:
			writeLine(encodedLines[op.newPos], pending)
			pending = 0
		}
	}
	restored := []byte(out.String())
	if bytes.Equal(restored, encoded) || !sameDocumentMeaning(restored, encoded) {
		return encoded
	}
	return restored
}

func formatKeys(lines []string) []docLine {
	keys := make([]docLine, len(lines))
	for i, line := range lines {
		keys[i] = docLine{text: formatKey(line)}
	}
	return keys
}

// sameDocumentMeaning reports whether two documents decode to the same values.
// Comments, spacing and blank lines do not count.
func sameDocumentMeaning(a, b []byte) bool {
	var left, right any
	if yaml.Unmarshal(a, &left) != nil || yaml.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}
