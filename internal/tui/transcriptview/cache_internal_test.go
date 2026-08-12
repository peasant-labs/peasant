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
