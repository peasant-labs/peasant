package kickstart_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

//go:embed testdata/source_preview_scroll.yaml
var sourcePreviewScrollData []byte

// sourcePreviewScrollCase drives one session from its first read to the end of
// what scrolling can reach.
type sourcePreviewScrollCase struct {
	Name          string                   `yaml:"name"`
	SourceFixture string                   `yaml:"source_fixture"`
	Origin        ftue.SessionSourceOrigin `yaml:"origin"`
	// PreviewBudgetBytes bounds the first read; SliceBudgetBytes bounds each
	// scrolled continuation. Zero turns scrolled loading off.
	PreviewBudgetBytes int64 `yaml:"preview_budget_bytes"`
	SliceBudgetBytes   int64 `yaml:"slice_budget_bytes"`
	MaxScrolls         int   `yaml:"max_scrolls"`
	// ExpectMoreAfterFirstRead is whether this session and these bounds leave
	// anything to scroll for. Without it a case could pass by reading the whole
	// session at once and never continuing anything.
	ExpectMoreAfterFirstRead bool     `yaml:"expect_more_after_first_read"`
	ExpectNoticeContains     []string `yaml:"expect_notice_contains"`
	// ExpectFinalNoticeContains is what the sentence must still say once
	// scrolling has finished. It is empty for a preview that has reached the end
	// of what it can load, which says nothing at all.
	ExpectFinalNoticeContains string `yaml:"expect_final_notice_contains"`
}

type sourcePreviewScrollDoc struct {
	RequiredCases []string                  `yaml:"required_cases"`
	Cases         []sourcePreviewScrollCase `yaml:"cases"`
}

func loadSourcePreviewScrollDoc(t *testing.T) sourcePreviewScrollDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(sourcePreviewScrollData))
	decoder.KnownFields(true)
	var doc sourcePreviewScrollDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode the scrolled preview fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("the scrolled preview fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("the scrolled preview fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.SourceFixture == "" || c.Origin == "" || c.MaxScrolls <= 0 {
			t.Fatalf("the scrolled preview fixture has an incomplete case: %+v", c)
		}
		if err := c.Origin.Validate(); err != nil {
			t.Fatalf("scrolled preview case %q has an unsupported origin %q", c.Name, c.Origin)
		}
		if !c.ExpectMoreAfterFirstRead && len(c.ExpectNoticeContains) > 0 {
			t.Fatalf("scrolled preview case %q expects no more content yet expects a sentence about loading more", c.Name)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("the scrolled preview fixture has a duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("the scrolled preview fixture is missing required case %q", name)
		}
	}
	return doc
}

// TestSourceTurns_ExtendsAPreviewAsTheReaderScrolls reads a real synthetic
// provider database the way a reader scrolling the pane does: one first read,
// then one continuation per scroll, until there is nothing more. It asserts
// that each continuation appends rather than replaces, that no turn is shown
// twice, that the chain ends, and that the sentence above the turns follows
// what is on screen.
func TestSourceTurns_ExtendsAPreviewAsTheReaderScrolls(t *testing.T) {
	t.Parallel()
	doc := loadSourcePreviewScrollDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			session := discoverOneSQLiteSession(t, c.SourceFixture, c.Origin)
			listing := ftue.SessionListing{
				Harness:   string(session.Harness),
				SessionID: string(session.SessionID),
				Source:    kickstart.ListingSource(session),
			}
			options := []kickstart.SourceTurnsOption{
				kickstart.WithSourceTurnsGitResolver(testutil.NoGitResolver()),
				kickstart.WithSourceTurnsSalt(salt.Salt{}),
				kickstart.WithSourceTurnsSliceBudget(c.SliceBudgetBytes),
			}
			if c.PreviewBudgetBytes > 0 {
				options = append(options, kickstart.WithSourceTurnsPreviewBudget(c.PreviewBudgetBytes))
			}
			reader := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, []ftue.SessionListing{listing}, options...)

			loaded, err := reader.Turns(listing.SessionID)
			if err != nil {
				t.Fatalf("read the first slice of the preview: %v", err)
			}
			if len(loaded) == 0 {
				t.Fatal("the first read produced no turns, so there is nothing for a reader to scroll")
			}
			if got := reader.HasMore(listing.SessionID); got != c.ExpectMoreAfterFirstRead {
				t.Fatalf("after the first read the preview reports more = %v, want %v", got, c.ExpectMoreAfterFirstRead)
			}
			assertScrollNotice(t, "after the first read", reader.Notice(listing.SessionID), c)

			seen := map[string]int{}
			for _, key := range renderedTurnSignatures(loaded) {
				seen[key]++
			}
			scrolls := 0
			for reader.HasMore(listing.SessionID) && scrolls < c.MaxScrolls {
				previous := loaded
				extended, more, err := reader.MoreTurns(listing.SessionID)
				if err != nil {
					t.Fatalf("scroll %d could not load more: %v", scrolls+1, err)
				}
				scrolls++
				assertAppendedOnto(t, scrolls, previous, extended)
				for _, key := range renderedTurnSignatures(extended[len(previous):]) {
					if seen[key]++; seen[key] > 1 {
						t.Errorf("scroll %d appended a turn the preview had already shown; the reader sees it twice: %q", scrolls, key)
					}
				}
				loaded = extended
				if !more {
					break
				}
				assertScrollNotice(t, "while there is more to load", reader.Notice(listing.SessionID), c)
			}
			if reader.HasMore(listing.SessionID) {
				t.Fatalf("the preview still reports more after %d scrolls; scrolling does not reach the end of the session", scrolls)
			}
			if c.ExpectMoreAfterFirstRead && scrolls == 0 {
				t.Fatal("no scroll ever ran, so this case never exercises a continuation")
			}
			finalNotice := reader.Notice(listing.SessionID)
			if c.ExpectFinalNoticeContains == "" {
				if finalNotice != "" {
					t.Errorf("a preview with nothing more behind it still says %q; there is nothing left to explain", finalNotice)
				}
				return
			}
			if !strings.Contains(finalNotice, c.ExpectFinalNoticeContains) {
				t.Errorf("the sentence above the turns is %q, which does not name %q", finalNotice, c.ExpectFinalNoticeContains)
			}
		})
	}
}

// assertAppendedOnto proves a continuation EXTENDED the preview rather than
// rebuilding it: every turn already on screen is still there, in the same
// order, and the new turns come after them.
func assertAppendedOnto(t *testing.T, scroll int, previous, extended []ingest.Turn) {
	t.Helper()
	if len(extended) < len(previous) {
		t.Fatalf("scroll %d left the preview with %d turns, down from %d; a continuation must never take content away",
			scroll, len(extended), len(previous))
	}
	if len(extended) == len(previous) {
		t.Fatalf("scroll %d added no turns even though the preview reported more to load", scroll)
	}
	before, after := turnSignature(previous), turnSignature(extended[:len(previous)])
	for index := range before {
		if before[index] != after[index] {
			t.Fatalf("scroll %d changed the turn at position %d; the reader's place moves when the turns above it are not the same turns",
				scroll, index)
		}
	}
}

// assertScrollNotice checks the live sentence above the turns.
func assertScrollNotice(t *testing.T, when, notice string, c sourcePreviewScrollCase) {
	t.Helper()
	if len(c.ExpectNoticeContains) == 0 {
		return
	}
	for _, phrase := range c.ExpectNoticeContains {
		if !strings.Contains(notice, phrase) {
			t.Errorf("%s the sentence above the turns is %q, which does not name %q", when, notice, phrase)
		}
	}
}

// renderedTurnSignatures names the turns a reader can actually SEE twice.
//
// A turn with no content is skipped, and is not an omission: two content-free
// turns are two different rows of the session that happen to render as nothing,
// so treating them as one turn shown twice would report a duplicate the reader
// could never observe. That a MESSAGE is never read by two slices - the
// guarantee this rests on - is proven against message identity where the slices
// are read, in the ingest slice-continuation cases.
func renderedTurnSignatures(turns []ingest.Turn) []string {
	out := make([]string, 0, len(turns))
	for _, signature := range turnSignature(turns) {
		if strings.HasSuffix(signature, "\x00") {
			continue
		}
		out = append(out, signature)
	}
	return out
}
