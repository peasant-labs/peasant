package onboarding

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/schema"
)

// DiffLimits bounds a document diff. A document or a diff past a bound is cut
// and reported as truncated; it never grows without limit.
type DiffLimits struct {
	// MaxHunks is the most hunks returned.
	MaxHunks int
	// MaxLines is the most hunk lines returned, context lines included.
	MaxLines int
	// MaxBytes is the largest document, on either side, that is compared.
	MaxBytes int
}

// DefaultDiffLimits is the bound a config.yaml review uses: the file is a few
// kilobytes in practice, so these are a guard, not a common path.
func DefaultDiffLimits() DiffLimits {
	return DiffLimits{MaxHunks: 40, MaxLines: 400, MaxBytes: 256 << 10}
}

const (
	// diffContextLines is the unchanged lines kept around each change.
	diffContextLines = 3
	// diffMaxLCSCells bounds the comparison table. Past it the changed middle
	// is reported as removed then added, which is a correct if verbose diff.
	diffMaxLCSCells = 1 << 22
)

// docLine is one line of a document. open marks a last line that has no
// terminating newline, so "x" and "x\n" are different documents.
type docLine struct {
	text string
	open bool
}

type diffOp struct {
	kind schema.LocalKickstartDiffLineType
	// text is the line the operation shows.
	text string
	// oldPos and newPos count the lines of each side consumed before this operation.
	oldPos, newPos int
}

// DiffDocuments compares two versions of a document line by line and returns the
// changes as structured hunks: each hunk is a verbatim range header and lines
// that carry their type and their line numbers. Nothing is rendered to text.
// The shape is the contract's review diff, which the client maps field for
// field.
//
// A nil or empty before is an absent file: every line is an addition under a
// "-0,0 +1,N" header. Each change keeps three lines of context, and changes
// closer than twice that share a hunk. The bool reports truncation: a document
// over limits.MaxBytes on either side is not compared and returns no hunks, and
// a diff over limits.MaxHunks or limits.MaxLines is cut after the last hunk or
// line that fits. The error reports limits that cannot bound anything.
func DiffDocuments(before, after []byte, limits DiffLimits) ([]schema.LocalKickstartHunk, bool, error) {
	if limits.MaxHunks <= 0 || limits.MaxLines <= 0 || limits.MaxBytes <= 0 {
		return nil, false, actionable(
			"diff a document",
			fmt.Sprintf("the limits (hunks %d, lines %d, bytes %d) must all be positive", limits.MaxHunks, limits.MaxLines, limits.MaxBytes),
			"onboarding.DiffDocuments",
			"before the documents were compared",
			"no diff was computed",
			"pass DefaultDiffLimits or three positive limits")
	}
	if len(before) > limits.MaxBytes || len(after) > limits.MaxBytes {
		return []schema.LocalKickstartHunk{}, true, nil
	}
	oldLines, newLines := splitDocument(before), splitDocument(after)
	ops := diffOperations(oldLines, newLines)
	hunks := []schema.LocalKickstartHunk{}
	truncated := false
	budget := limits.MaxLines
	for _, span := range hunkWindows(ops) {
		if len(hunks) == limits.MaxHunks || budget <= 0 {
			truncated = true
			break
		}
		window := ops[span[0]:span[1]]
		if len(window) > budget {
			window = window[:budget]
			truncated = true
		}
		if !hasChange(window) {
			// The cut left only context. A hunk with no change says nothing.
			break
		}
		hunks = append(hunks, buildHunk(window))
		budget -= len(window)
		if truncated {
			break
		}
	}
	return hunks, truncated, nil
}

func splitDocument(data []byte) []docLine {
	if len(data) == 0 {
		return nil
	}
	pieces := strings.Split(string(data), "\n")
	last := pieces[len(pieces)-1]
	lines := make([]docLine, 0, len(pieces))
	for _, piece := range pieces[:len(pieces)-1] {
		lines = append(lines, docLine{text: piece})
	}
	if last != "" {
		lines = append(lines, docLine{text: last, open: true})
	}
	return lines
}

// diffOperations returns the whole edit script from a to b: unchanged lines as
// context, removed lines as del, added lines as add. Common leading and
// trailing lines are matched first so the comparison table covers only the
// changed middle.
func diffOperations(a, b []docLine) []diffOp {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	ops := make([]diffOp, 0, len(a)+len(b))
	emit := func(kind schema.LocalKickstartDiffLineType, text string, oldBefore, newBefore int) {
		ops = append(ops, diffOp{kind: kind, text: text, oldPos: oldBefore, newPos: newBefore})
	}
	for i := 0; i < prefix; i++ {
		emit(schema.LocalKickstartDiffCtx, a[i].text, i, i)
	}
	midA, midB := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	o, n := prefix, prefix
	if len(midA)*len(midB) > diffMaxLCSCells {
		for _, line := range midA {
			emit(schema.LocalKickstartDiffDel, line.text, o, n)
			o++
		}
		for _, line := range midB {
			emit(schema.LocalKickstartDiffAdd, line.text, o, n)
			n++
		}
	} else {
		table := suffixLCS(midA, midB)
		width := len(midB) + 1
		i, j := 0, 0
		for i < len(midA) || j < len(midB) {
			switch {
			case i < len(midA) && j < len(midB) && midA[i] == midB[j]:
				emit(schema.LocalKickstartDiffCtx, midA[i].text, o, n)
				i, j, o, n = i+1, j+1, o+1, n+1
			case i < len(midA) && (j == len(midB) || table[(i+1)*width+j] >= table[i*width+j+1]):
				emit(schema.LocalKickstartDiffDel, midA[i].text, o, n)
				i, o = i+1, o+1
			default:
				emit(schema.LocalKickstartDiffAdd, midB[j].text, o, n)
				j, n = j+1, n+1
			}
		}
	}
	o, n = len(a)-suffix, len(b)-suffix
	for k := 0; k < suffix; k++ {
		emit(schema.LocalKickstartDiffCtx, a[o+k].text, o+k, n+k)
	}
	return ops
}

// suffixLCS fills table[i*(len(b)+1)+j] with the length of the longest common
// subsequence of a[i:] and b[j:]. Its largest value is bounded by the cell cap,
// so it fits uint16.
func suffixLCS(a, b []docLine) []uint16 {
	width := len(b) + 1
	table := make([]uint16, (len(a)+1)*width)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				table[i*width+j] = table[(i+1)*width+j+1] + 1
			case table[(i+1)*width+j] >= table[i*width+j+1]:
				table[i*width+j] = table[(i+1)*width+j]
			default:
				table[i*width+j] = table[i*width+j+1]
			}
		}
	}
	return table
}

// hunkWindows returns the [start, end) operation range of each hunk. Changes
// separated by no more than twice the context share one hunk.
func hunkWindows(ops []diffOp) [][2]int {
	var changes []int
	for i, op := range ops {
		if op.kind != schema.LocalKickstartDiffCtx {
			changes = append(changes, i)
		}
	}
	var windows [][2]int
	for i := 0; i < len(changes); {
		j := i
		for j+1 < len(changes) && changes[j+1]-changes[j]-1 <= 2*diffContextLines {
			j++
		}
		start := max(0, changes[i]-diffContextLines)
		end := min(len(ops), changes[j]+diffContextLines+1)
		windows = append(windows, [2]int{start, end})
		i = j + 1
	}
	return windows
}

func hasChange(window []diffOp) bool {
	for _, op := range window {
		if op.kind != schema.LocalKickstartDiffCtx {
			return true
		}
	}
	return false
}

// buildHunk turns an operation range into a hunk. The header counts only the
// lines the hunk shows, so a hunk cut short still states its own range.
func buildHunk(window []diffOp) schema.LocalKickstartHunk {
	oldCount, newCount := 0, 0
	lines := make([]schema.LocalKickstartDiffLine, 0, len(window))
	for _, op := range window {
		line := schema.LocalKickstartDiffLine{Type: op.kind, Text: op.text}
		switch op.kind {
		case schema.LocalKickstartDiffCtx:
			line.OldNo, line.NewNo = op.oldPos+1, op.newPos+1
			oldCount++
			newCount++
		case schema.LocalKickstartDiffDel:
			line.OldNo = op.oldPos + 1
			oldCount++
		case schema.LocalKickstartDiffAdd:
			line.NewNo = op.newPos + 1
			newCount++
		}
		lines = append(lines, line)
	}
	oldStart, newStart := window[0].oldPos+1, window[0].newPos+1
	if oldCount == 0 {
		oldStart--
	}
	if newCount == 0 {
		newStart--
	}
	return schema.LocalKickstartHunk{
		Header: fmt.Sprintf("-%d,%d +%d,%d", oldStart, oldCount, newStart, newCount),
		Lines:  lines,
	}
}
