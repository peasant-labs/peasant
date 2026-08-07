package ingest

import (
	"strings"
	"sync"

	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// sharedTitlePipeline is the process-wide, immutable redaction-free title
// pipeline. It cleans harness-owned markup (system-reminder blocks, command and
// query wrappers) and caps length WITHOUT redacting secrets, paths, or user
// identifiers. That is appropriate for a LOCAL display title of the user's own
// sessions, where readable real paths and names are wanted; a published title
// must instead go through redact's Generate. Constructed once.
var sharedTitlePipeline = sync.OnceValues(redact.NewTitlePipeline)

// simpleTitle derives a legible display title from a session's first user-turn
// text using the shared pipeline. On any construction or cleanup error it falls
// back to a plain first-line, length-capped title, so a session always carries
// some label rather than none.
func simpleTitle(firstTurn string, harness schema.Harness) string {
	if p, err := sharedTitlePipeline(); err == nil {
		if title, terr := p.SimpleTitle(firstTurn, harness); terr == nil {
			return firstLine(title)
		}
	}
	return firstLine(fallbackTitle(firstTurn))
}

// firstLine reduces a title to its first non-empty line, so a multi-line first
// user turn yields a one-line title rather than a value that renders across
// several rows. Leading blank lines are skipped.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return strings.TrimSpace(s)
}

// fallbackTitle is the pre-pipeline behaviour: the first line, capped to 80
// code points. It is only reached when the pipeline cannot be built or the
// input markup is malformed.
func fallbackTitle(text string) string {
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	if runes := []rune(text); len(runes) > 80 {
		return string(runes[:77]) + "..."
	}
	return text
}
