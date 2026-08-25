package transcriptview

import (
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// The per-turn cache is what makes a full transcript affordable to re-draw on
// every frame, and it is invisible from outside: a black-box test can only
// prove the OUTPUT is stable, which a renderer with no cache at all would also
// satisfy. These tests are therefore in-package, and they assert on the cache
// itself - that it fills, that a repeat draw adds nothing to it, that a
// different width is a different entry rather than a stale hit, and that it
// cannot grow without bound.

// cacheTurns builds n distinct plain turns.
func cacheTurns(n int) []ingest.Turn {
	turns := make([]ingest.Turn, 0, n)
	for i := range n {
		turns = append(turns, ingest.Turn{
			Index: i, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText,
			Content: fmt.Sprintf("turn number %d", i),
		})
	}
	return turns
}

const cacheTestWidth = 40

func TestTurnCache_FillsOncePerTurnAndWidth(t *testing.T) {
	t.Parallel()
	const turnCount = 6
	r := New(theme.New(theme.ModeDark))
	document := r.Document(cacheTurns(turnCount))

	first := document.Render(cacheTestWidth)
	if len(r.cache) != turnCount {
		t.Fatalf("after one draw the cache holds %d entries, want one per turn (%d)", len(r.cache), turnCount)
	}

	second := document.Render(cacheTestWidth)
	if len(r.cache) != turnCount {
		t.Errorf("a repeat draw grew the cache to %d entries; it must be served from it", len(r.cache))
	}
	if second != first {
		t.Error("a cached draw differs from the first one")
	}

	// A different width is a DIFFERENT entry. If width were left out of the key,
	// the count would stay put and the pane would keep showing the old layout.
	document.Render(cacheTestWidth * 2)
	if len(r.cache) != turnCount*2 {
		t.Errorf("after drawing at a second width the cache holds %d entries, want %d; width is not part of the key",
			len(r.cache), turnCount*2)
	}
}

func TestTurnCache_ChangedContentIsANewEntry(t *testing.T) {
	t.Parallel()
	r := New(theme.New(theme.ModeDark))
	stored := []ingest.Turn{{
		Index: 0, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText,
		Content: "the pipeline writes to a temp directory and then renam",
	}}
	// The same turn index, now carrying the full body the source overlay
	// recovered. Keying the cache on the index would serve the truncated text
	// forever; keying it on the content cannot.
	recovered := []ingest.Turn{{
		Index: 0, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText,
		Content: "the pipeline writes to a temp directory and then renames it into place",
	}}

	before := r.Document(stored).Render(cacheTestWidth)
	after := r.Document(recovered).Render(cacheTestWidth)
	if before == after {
		t.Fatal("a turn whose content changed re-used the cached render of the old content")
	}
	if !strings.Contains(strings.Join(strings.Fields(visibleForCache(after)), " "), "renames it into place") {
		t.Errorf("the recovered body is missing from the redraw:\n%s", after)
	}
}

func TestTurnCache_IsBounded(t *testing.T) {
	t.Parallel()
	r := New(theme.New(theme.ModeDark))

	// Draw enough DISTINCT turns to pass the bound several times over. Without a
	// bound the cache would hold every one of them, so a long onboarding session
	// spent scrolling would keep growing the process.
	const documents = (maxCachedTurns / MaxRenderedTurns) + 2
	for d := range documents {
		turns := make([]ingest.Turn, 0, MaxRenderedTurns)
		for i := range MaxRenderedTurns {
			turns = append(turns, ingest.Turn{
				Index: i, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText,
				Content: fmt.Sprintf("document %d turn %d", d, i),
			})
		}
		r.Document(turns).Render(cacheTestWidth)
		if len(r.cache) > maxCachedTurns {
			t.Fatalf("after %d documents the cache holds %d entries, past the %d bound", d+1, len(r.cache), maxCachedTurns)
		}
	}
	if total := documents * MaxRenderedTurns; len(r.cache) >= total {
		t.Fatalf("the cache holds %d of %d distinct turns; it never reset", len(r.cache), total)
	}
}

// visibleForCache strips styling so an in-package assertion can read the words.
func visibleForCache(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == '│':
			// The gutter rail is chrome; dropping it lets a phrase that wrapped
			// across rows read as one sentence.
		case r == '\x1b':
			inEscape = true
		case inEscape && r == 'm':
			inEscape = false
		case !inEscape:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestTurnCache_KeepsAScrolledTranscriptWarm proves the cache ceiling FOLLOWS
// the document actually drawn, so a transcript larger than the starting ceiling
// is re-drawn warm rather than cold.
//
// This is a regression against a cache CLIFF, not a leak. The cache is dropped
// whole when it is exceeded, so a working set one entry too large clears the map
// part-way through every single draw and every re-draw pays a fully cold render.
// A preview that grows as the reader scrolls walks straight into that: measured
// against a real session, the re-draw cost went from 2ms at 200 turns to 563ms
// at 883 - on every frame, so on every keystroke.
//
// It asserts the cache STATE rather than elapsed time, because a timing
// assertion on a shared machine reports the machine's load as a defect.
func TestTurnCache_KeepsAScrolledTranscriptWarm(t *testing.T) {
	t.Parallel()
	r := New(theme.New(theme.ModeDark))

	// A document comfortably past the starting ceiling, which is what a reader
	// reaches after a few scrolls of a long session.
	const turnCount = maxCachedTurns * 2
	turns := make([]ingest.Turn, 0, turnCount)
	for i := range turnCount {
		turns = append(turns, ingest.Turn{
			Index: i, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText,
			Content: fmt.Sprintf("scrolled turn %d", i),
		})
	}
	r.UnboundedDocument(turns).Render(cacheTestWidth)
	if len(r.cache) < turnCount {
		t.Fatalf("after drawing %d turns the cache holds %d of them; the next draw re-renders the whole transcript cold",
			turnCount, len(r.cache))
	}

	// The ceiling rose for this document, and it must still BOUND. Drawing more
	// DISTINCT documents than the ceiling can hold has to reset it, or the cache
	// grows without limit as a reader steps through sessions.
	//
	// Two documents of this size legitimately fit side by side: the ceiling
	// covers one document at cacheWidthsKeptWarm widths, which is the same
	// number of entries as two documents at one width. So the count that proves
	// a bound is the one past that.
	const documents = cacheWidthsKeptWarm + 2
	for d := 1; d < documents; d++ {
		next := make([]ingest.Turn, 0, turnCount)
		for i := range turnCount {
			next = append(next, ingest.Turn{
				Index: i, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText,
				Content: fmt.Sprintf("document %d scrolled turn %d", d, i),
			})
		}
		r.UnboundedDocument(next).Render(cacheTestWidth)
		if len(r.cache) > r.ceiling {
			t.Fatalf("after document %d the cache holds %d entries, past its own %d ceiling",
				d, len(r.cache), r.ceiling)
		}
	}
	if distinct := documents * turnCount; len(r.cache) >= distinct {
		t.Fatalf("the cache holds %d of %d distinct turns; it never reset and grows without limit",
			len(r.cache), distinct)
	}
}

// TestTurnCache_ABoundedDocumentKeepsTheStartingCeiling proves the ceiling only
// ever rises for a caller that ASKED to draw more. Every existing caller hands
// over a bounded document, so none of them may see the ceiling move.
func TestTurnCache_ABoundedDocumentKeepsTheStartingCeiling(t *testing.T) {
	t.Parallel()
	r := New(theme.New(theme.ModeDark))
	turns := make([]ingest.Turn, 0, MaxRenderedTurns*4)
	for i := range MaxRenderedTurns * 4 {
		turns = append(turns, ingest.Turn{
			Index: i, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText,
			Content: fmt.Sprintf("handed over turn %d", i),
		})
	}
	r.Document(turns).Render(cacheTestWidth)
	if r.ceiling != maxCachedTurns {
		t.Fatalf("a bounded document moved the cache ceiling to %d; it must stay at %d for every caller that did not ask to draw more",
			r.ceiling, maxCachedTurns)
	}
}
