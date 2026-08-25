package transcriptview

import (
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// The renderer draws SYNCHRONOUSLY while a frame is being composed, over data
// nobody validated: a recorded transcript is whatever a coding agent wrote to
// disk. Every bound below caps one way that data could make a single frame
// cost more than the surface is worth. Each one is visible in the output - the
// pane always says what it left out - because a preview that silently stops is
// indistinguishable from a session that stopped.
const (
	// MaxRenderedTurns caps how many turns one Render lays out. A cold render
	// of a turn goes through glamour; past this many, the first draw after a
	// load would be doing more layout work than a person reads before scrolling.
	// Anything beyond is summarized by count.
	MaxRenderedTurns = 200

	// MaxToolResultLines caps a tool call's result preview. A single result can
	// be an entire file or a whole test run, and the pane shows a conversation
	// rather than a log.
	MaxToolResultLines = 8

	// MaxProseBytes caps what one turn hands to the markdown renderer. Layout
	// cost grows with the source, and a recorded turn can carry a pasted file:
	// this is what keeps ONE oversized message from stalling a frame.
	MaxProseBytes = 32768

	// maxCachedTurns is where the per-turn cache STARTS. It is well above
	// MaxRenderedTurns so one bounded transcript at a couple of settled widths
	// fits entirely, which is the case the cache exists for. A document that
	// draws more than that raises it; see Renderer.raiseCeilingFor.
	maxCachedTurns = 512

	// cacheWidthsKeptWarm is how many settled widths one document is expected to
	// be drawn at while a reader works with it - the pane's own width, and the
	// one it takes after a resize. It is what turns a document's turn count into
	// a cache ceiling that keeps that document warm rather than thrashing it.
	cacheWidthsKeptWarm = 2
)

const (
	// gutterGlyph is the rail a block's body sits behind. Radius zero, one cell
	// wide, drawn in the block's own color.
	gutterGlyph = "│"
	// gutterWidth is the glyph plus the space after it.
	gutterWidth = 2
	// blockSeparator is the blank line between turns.
	blockSeparator = "\n\n"
	// depthIndentWidth is how far one level of subagent nesting is set in.
	depthIndentWidth = 2
	// maxIndentedDepth caps the nesting indent so a deep chain still has room
	// for its own content.
	maxIndentedDepth = 4
	// minRenderWidth is the narrowest column anything is laid out against; a
	// non-positive width would make the markdown renderer fall back to its own
	// default and overflow the pane.
	minRenderWidth = 1
	// toolFailedSuffix marks a tool call the harness recorded as failed.
	toolFailedSuffix = "failed"
	// moreLinesFormat says how much of a tool result was not shown.
	moreLinesFormat = "(%d more lines)"
	// moreTurnsFormat says how many turns past MaxRenderedTurns exist.
	moreTurnsFormat = "(%d more turns not shown here)"
	// truncatedProseMarker ends a turn body cut at MaxProseBytes.
	truncatedProseMarker = "\n\n(message continues)"
)

// boundTurns returns the turns to draw and how many were left out.
func boundTurns(turns []ingest.Turn) (shown []ingest.Turn, omitted int) {
	if len(turns) <= MaxRenderedTurns {
		return turns, 0
	}
	return turns[:MaxRenderedTurns], len(turns) - MaxRenderedTurns
}

// boundProse caps one turn's body at MaxProseBytes, cutting on a rune boundary
// so the result is still valid text, and saying that it continues.
func boundProse(s string) string {
	if len(s) <= MaxProseBytes {
		return s
	}
	cut := MaxProseBytes
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedProseMarker
}

// utf8RuneStart reports whether b begins a UTF-8 rune (i.e. is not one of its
// continuation bytes).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// boundLines returns the first MaxToolResultLines lines of s and how many it
// dropped.
func boundLines(s string) (kept string, omitted int) {
	if s == "" {
		return "", 0
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= MaxToolResultLines {
		return s, 0
	}
	return strings.Join(lines[:MaxToolResultLines], "\n"), len(lines) - MaxToolResultLines
}

// toolHeadline names one tool call on its header line: the tool, then the
// single most identifying thing about the call. A file path is preferred over
// the raw arguments when the harness extracted one, because "read
// internal/api/store_adapter.go" answers what the step did and
// `{"file_path":"…","offset":180}` makes the reader parse JSON to find out.
func toolHeadline(call ingest.ToolCall) string {
	target := call.FilePath
	if target == "" {
		target = collapseWhitespace(call.Arguments)
	}
	if target == "" {
		return call.Name
	}
	return call.Name + " " + target
}

// collapseWhitespace folds every run of whitespace into one space, so a
// multi-line argument blob can sit on a single header line.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
