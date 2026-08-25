package kickstart_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/source_preview_two_step.yaml
var sourcePreviewTwoStepData []byte

// sourcePreviewTwoStepCase drives one session through both reads: the quick
// leading slice and the whole bounded read behind it.
type sourcePreviewTwoStepCase struct {
	Name          string                   `yaml:"name"`
	SourceFixture string                   `yaml:"source_fixture"`
	Origin        ftue.SessionSourceOrigin `yaml:"origin"`
	// FirstPageBudgetBytes bounds the leading slice.
	FirstPageBudgetBytes int64 `yaml:"first_page_budget_bytes"`
	// PreviewBudgetBytes bounds the whole read. Zero reads the session whole.
	PreviewBudgetBytes int64 `yaml:"preview_budget_bytes"`
	MinFirstTurns      int   `yaml:"min_first_turns"`
	// ExpectMore is whether the session continues past its leading slice, which
	// is what decides between a two-step read and a single read.
	ExpectMore bool `yaml:"expect_more"`
	// ExpectNoticeContains lists the phrases the note must carry AFTER the whole
	// read. An empty list means the whole read shows no note.
	ExpectNoticeContains []string `yaml:"expect_notice_contains"`
}

type sourcePreviewTwoStepDoc struct {
	RequiredCases []string                   `yaml:"required_cases"`
	Cases         []sourcePreviewTwoStepCase `yaml:"cases"`
}

func loadSourcePreviewTwoStepDoc(t *testing.T) sourcePreviewTwoStepDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(sourcePreviewTwoStepData))
	decoder.KnownFields(true)
	var doc sourcePreviewTwoStepDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode two-step preview fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("two-step preview fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("two-step preview fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.SourceFixture == "" || c.Origin == "" || c.MinFirstTurns < 1 {
			t.Fatalf("two-step preview fixture has an incomplete case: %+v", c)
		}
		if c.FirstPageBudgetBytes <= 0 {
			t.Fatalf("two-step preview case %q declares no first-page bound, so it cannot read a slice", c.Name)
		}
		if err := c.Origin.Validate(); err != nil {
			t.Fatalf("two-step preview case %q has an unsupported origin %q", c.Name, c.Origin)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("two-step preview fixture has a duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("two-step preview fixture is missing required case %q", name)
		}
	}
	return doc
}

// twoStepReader builds a source reader over one discovered SQLite session, with
// the case's two bounds applied.
func twoStepReader(t *testing.T, c sourcePreviewTwoStepCase, listing ftue.SessionListing, firstPageBudget int64) *kickstart.SourceTurns {
	t.Helper()
	options := []kickstart.SourceTurnsOption{
		kickstart.WithSourceTurnsGitResolver(testutil.NoGitResolver()),
		kickstart.WithSourceTurnsSalt(salt.Salt{}),
		kickstart.WithSourceTurnsFirstPageBudget(firstPageBudget),
	}
	if c.PreviewBudgetBytes > 0 {
		options = append(options, kickstart.WithSourceTurnsPreviewBudget(c.PreviewBudgetBytes))
	}
	return kickstart.NewSourceTurns(&ingest.OSFileSystem{}, []ftue.SessionListing{listing}, options...)
}

func twoStepListing(t *testing.T, c sourcePreviewTwoStepCase) ftue.SessionListing {
	t.Helper()
	session := discoverOneSQLiteSession(t, c.SourceFixture, c.Origin)
	listing := ftue.SessionListing{
		Harness:   string(session.Harness),
		SessionID: string(session.SessionID),
		Source:    kickstart.ListingSource(session),
	}
	if listing.Source.Origin != c.Origin {
		t.Fatalf("listing origin %q, want %q", listing.Source.Origin, c.Origin)
	}
	return listing
}

func turnSignature(turns []ingest.Turn) []string {
	out := make([]string, 0, len(turns))
	for _, turn := range turns {
		out = append(out, string(turn.Role)+"\x00"+turn.Content)
	}
	return out
}

// TestSourceTurns_ReadsAFirstPageBeforeTheWholeSession proves the two-step
// preview read end to end against a real synthetic provider database: the
// leading slice comes back on its own, it never carries the whole read's note,
// it is never cached, and the whole read behind it is exactly the result the
// single-step read produces.
func TestSourceTurns_ReadsAFirstPageBeforeTheWholeSession(t *testing.T) {
	t.Parallel()
	doc := loadSourcePreviewTwoStepDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			listing := twoStepListing(t, c)

			// The reference is the single-step read: no first page at all.
			reference := twoStepReader(t, c, listing, 0)
			wantTurns, err := reference.Turns(listing.SessionID)
			if err != nil {
				t.Fatalf("single-step read: %v", err)
			}
			wantNotice := reference.Notice(listing.SessionID)

			reader := twoStepReader(t, c, listing, c.FirstPageBudgetBytes)
			first, more, err := reader.FirstTurns(listing.SessionID)
			if err != nil {
				t.Fatalf("read the first page: %v", err)
			}
			if len(first) < c.MinFirstTurns {
				t.Fatalf("the first page produced %d turns, want at least %d", len(first), c.MinFirstTurns)
			}
			if more != c.ExpectMore {
				t.Fatalf("the first page reports more=%v, want %v", more, c.ExpectMore)
			}
			if more {
				// A slice stands for less than the session and says nothing
				// about the bound the whole read will run under, so it must
				// show no note.
				if notice := reader.Notice(listing.SessionID); notice != "" {
					t.Errorf("the first page carries the note %q, want none", notice)
				}
			}

			full, err := reader.Turns(listing.SessionID)
			if err != nil {
				t.Fatalf("read the whole session: %v", err)
			}
			gotSig, wantSig := turnSignature(full), turnSignature(wantTurns)
			if len(gotSig) != len(wantSig) {
				t.Fatalf("the whole read produced %d turns, want the single-step read's %d", len(gotSig), len(wantSig))
			}
			for i := range gotSig {
				if gotSig[i] != wantSig[i] {
					t.Fatalf("turn %d of the whole read differs from the single-step read", i)
				}
			}
			notice := reader.Notice(listing.SessionID)
			if notice != wantNotice {
				t.Fatalf("the note after the whole read is %q, want the single-step read's %q", notice, wantNotice)
			}
			if len(c.ExpectNoticeContains) == 0 {
				if notice != "" {
					t.Fatalf("the whole read showed the note %q, want none", notice)
				}
			}
			for _, phrase := range c.ExpectNoticeContains {
				if !strings.Contains(notice, phrase) {
					t.Errorf("the note %q does not name %q", notice, phrase)
				}
			}

			// The cached answer must be the whole result too, not the slice.
			cached, err := reader.Turns(listing.SessionID)
			if err != nil {
				t.Fatalf("read the cached session: %v", err)
			}
			if len(cached) != len(full) {
				t.Fatalf("the cached read produced %d turns, want the whole read's %d", len(cached), len(full))
			}
			if reader.Notice(listing.SessionID) != wantNotice {
				t.Fatalf("the cached note is %q, want %q", reader.Notice(listing.SessionID), wantNotice)
			}

			assertPaneShowsTheNoteOnlyAfterTheWholeRead(t, c, listing)
			assertOnlyWholeResultsAreCached(t, c, listing)
		})
	}
}

// assertOnlyWholeResultsAreCached proves the cache rule DIRECTLY rather than by
// comparing turn counts, which a session whose slice already holds every turn
// cannot show.
//
// A fresh reader takes the leading slice, and then the provider database is
// removed. A cached session answers without its source; an uncached one cannot.
// So a session with more to read must now FAIL - the slice was never cached -
// and a session that was read whole in one step must still answer.
func assertOnlyWholeResultsAreCached(t *testing.T, c sourcePreviewTwoStepCase, listing ftue.SessionListing) {
	t.Helper()
	reader := twoStepReader(t, c, listing, c.FirstPageBudgetBytes)
	first, more, err := reader.FirstTurns(listing.SessionID)
	if err != nil {
		t.Fatalf("read the first page again: %v", err)
	}
	if err := os.Remove(listing.Source.Path); err != nil {
		t.Fatalf("remove the provider database: %v", err)
	}
	turns, err := reader.Turns(listing.SessionID)
	if !more {
		if err != nil {
			t.Fatalf("a session read whole in one step must answer from the cache without its source: %v", err)
		}
		if len(turns) != len(first) {
			t.Fatalf("the cached read produced %d turns, want the single read's %d", len(turns), len(first))
		}
		return
	}
	if err == nil {
		t.Fatalf("the leading slice was cached: the whole read answered %d turns without its source", len(turns))
	}
}

// assertPaneShowsTheNoteOnlyAfterTheWholeRead drives the PANE over the same
// two-step reader: the body it paints first must carry no note, and the body it
// swaps in must carry the whole read's note.
func assertPaneShowsTheNoteOnlyAfterTheWholeRead(t *testing.T, c sourcePreviewTwoStepCase, listing ftue.SessionListing) {
	t.Helper()
	if len(c.ExpectNoticeContains) == 0 {
		return
	}
	// The note is supplied unconditionally here, for the slice as much as for
	// the whole body. Reading it from the reader instead would prove nothing:
	// the reader has no note to give for a slice it did not cache, so the pane
	// would look right whether or not it applies the rule. Supplying it always
	// leaves the PANE as the only thing that can keep it off the slice.
	note := wholeReadNote(t, c, listing)
	reader := twoStepReader(t, c, listing, c.FirstPageBudgetBytes)
	preview := kickstart.NewListingPreview(theme.New(theme.ModeDark), []ftue.SessionListing{listing}, reader.Turns,
		kickstart.WithSessionPreviewNotice(func(string) string { return note }),
		kickstart.WithSessionFirstTurns(reader.FirstTurns))

	firstBody, more, err := preview.FirstBody(listing.SessionID)
	if err != nil {
		t.Fatalf("paint the first body: %v", err)
	}
	if more != c.ExpectMore {
		t.Fatalf("the pane reports more=%v, want %v", more, c.ExpectMore)
	}
	firstPane := flattenPreviewPane(firstBody.Render(previewPaneWidth))
	for _, phrase := range c.ExpectNoticeContains {
		if strings.Contains(firstPane, previewNeedle(phrase)) {
			t.Errorf("the first body names %q, which describes the whole read's bound, not the slice; pane:\n%s", phrase, firstPane)
		}
	}

	wholeBody, err := preview.Body(listing.SessionID)
	if err != nil {
		t.Fatalf("paint the whole body: %v", err)
	}
	wholePane := flattenPreviewPane(wholeBody.Render(previewPaneWidth))
	for _, phrase := range c.ExpectNoticeContains {
		if !strings.Contains(wholePane, previewNeedle(phrase)) {
			t.Errorf("the whole body does not name %q; pane:\n%s", phrase, wholePane)
		}
	}
}

// previewPaneWidth is a pane wide enough that the note wraps into readable rows
// rather than being clipped away, so a missing phrase is a missing note.
const previewPaneWidth = 80

// wholeReadNote is the note the whole bounded read of this case produces.
func wholeReadNote(t *testing.T, c sourcePreviewTwoStepCase, listing ftue.SessionListing) string {
	t.Helper()
	reader := twoStepReader(t, c, listing, 0)
	if _, err := reader.Turns(listing.SessionID); err != nil {
		t.Fatalf("read the whole session for its note: %v", err)
	}
	note := reader.Notice(listing.SessionID)
	if note == "" {
		t.Fatalf("case %q expects a note but the whole read produced none", c.Name)
	}
	return note
}
