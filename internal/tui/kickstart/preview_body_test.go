package kickstart_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/preview_bodies.yaml
var previewBodyData []byte

// previewBodyCase is one highlighted row and the lines its preview must and
// must not carry.
type previewBodyCase struct {
	Name         string   `yaml:"name"`
	Highlight    string   `yaml:"highlight"`
	Width        int      `yaml:"width"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

// previewBodyDocument is the whole fixture plus its row-count guard. Stored maps
// a session id to the RECORDED TURNS the local store holds for it.
type previewBodyDocument struct {
	ExpectedCaseCount int                               `yaml:"expectedCaseCount"`
	Listings          []ftue.SessionListing             `yaml:"listings"`
	Stored            map[string][]testutil.TurnFixture `yaml:"stored"`
	Cases             []previewBodyCase                 `yaml:"cases"`
}

func loadPreviewBodies(t *testing.T) previewBodyDocument {
	t.Helper()
	var doc previewBodyDocument
	dec := yaml.NewDecoder(bytes.NewReader(previewBodyData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/preview_bodies.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("preview_bodies.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", doc.ExpectedCaseCount, len(doc.Cases))
	}
	if len(doc.Listings) == 0 {
		t.Fatal("fixture declares no listings")
	}
	if len(doc.Stored) == 0 {
		t.Fatal("fixture declares no stored sessions; every case would take the not-imported path")
	}
	// Every stored id must name a real listing row, or a case could assert
	// against a transcript the tree could never highlight.
	listed := map[string]bool{}
	for _, l := range doc.Listings {
		listed[l.SessionID] = true
	}
	for id := range doc.Stored {
		if !listed[id] {
			t.Fatalf("fixture stores turns for session %q, which is not in the listing", id)
		}
	}
	for _, c := range doc.Cases {
		if len(c.WantContains)+len(c.WantMissing) == 0 {
			t.Fatalf("preview fixture case %q declares no expected values; an empty want list turns the case into a guaranteed pass", c.Name)
		}
		// A blank highlight would preview whatever the zero id resolves to, and
		// a non-positive width renders nothing to assert on.
		testutil.RequireFixtureFields(t, "preview body", c.Name, []testutil.FixtureField{
			{Key: "highlight", Value: c.Highlight},
		})
		if c.Width <= 0 {
			t.Fatalf("preview fixture case %q declares width %d", c.Name, c.Width)
		}
		for _, want := range append(append([]string{}, c.WantContains...), c.WantMissing...) {
			if strings.TrimSpace(want) == "" {
				t.Fatalf("preview fixture case %q declares an empty needle; it matches everything or nothing regardless of the code", c.Name)
			}
		}
	}
	return doc
}

// previewTheme is the palette the preview renders in. Every case pins the dark
// mode so the assertions read one deterministic render rather than whichever
// mode happened to be built.
func previewTheme() theme.Theme { return theme.New(theme.ModeDark) }

// storedTurns builds the store read seam from the fixture: a session the
// fixture holds turns for reads back those turns, and any other session reads
// back none, which is the not-yet-imported case.
func storedTurns(t *testing.T, doc previewBodyDocument) kickstart.SessionTurnsFunc {
	t.Helper()
	byID := make(map[string][]ingest.Turn, len(doc.Stored))
	for id, rows := range doc.Stored {
		byID[id] = testutil.Turns(t, id, rows)
	}
	return func(sessionID string) ([]ingest.Turn, error) { return byID[sessionID], nil }
}

// recordedExchange is the reply the pane-level and program-level tests pair
// every stubbed prompt with. Those tests pin the PANE (its scroll, its focus,
// its place in the mounted step), and a one-turn session would give the
// viewport almost nothing to move over; what they need is a short conversation
// whose text is predictable.
const recordedExchange = "write to a temp directory first, then rename it into place."

// turnsFromPrompts builds the store read seam from a map of session id to the
// prompt that opened it, pairing each with a reply so the pane has a real
// two-voice exchange to lay out. A session the map does not name reads back no
// turns, which is the not-yet-imported case.
func turnsFromPrompts(prompts map[string]string) kickstart.SessionTurnsFunc {
	return func(sessionID string) ([]ingest.Turn, error) {
		prompt, ok := prompts[sessionID]
		if !ok {
			return nil, nil
		}
		return []ingest.Turn{
			{Index: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, Content: prompt},
			{Index: 1, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText, Content: recordedExchange},
		}, nil
	}
}

// flattenPane strips the styling, folds the gutter rails away, and joins wrapped
// rows, so a phrase the pane split across a wrap point is still findable.
func flattenPane(s string) string {
	out := stripRender(s)
	out = strings.ReplaceAll(out, "│ ", "")
	out = strings.ReplaceAll(out, "│", "")
	return strings.Join(strings.Fields(out), " ")
}

// needle normalises an expected phrase the same way flattenPane normalises the
// pane, so a multi-word expectation is compared against one row of text.
func needle(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestListingPreview_BodyPerRowKind drives the REAL preview source over the
// fixture listing: an imported session shows its WHOLE recorded transcript,
// role tags and highlighted code included, a not-yet-imported one says so
// plainly, a session with no branch omits the branch line, and a project or
// branch row explains what to highlight instead. No case shows the session
// title, which the transcript's own first turn already says.
func TestListingPreview_BodyPerRowKind(t *testing.T) {
	t.Parallel()
	doc := loadPreviewBodies(t)

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			// One preview PER case, not one shared across them: the renderer
			// behind it carries an unsynchronised per-turn cache because it is
			// only ever drawn from the UI goroutine, so sharing one across
			// parallel cases would be racing on a contract the pane keeps.
			preview := kickstart.NewListingPreview(previewTheme(), doc.Listings, storedTurns(t, doc))
			body, err := preview.Body(c.Highlight)
			if err != nil {
				t.Fatalf("load preview for %q: %v", c.Highlight, err)
			}
			if body == nil {
				t.Fatalf("preview for %q loaded no body at all", c.Highlight)
			}
			got := flattenPane(body.Render(c.Width))
			for _, want := range c.WantContains {
				if !strings.Contains(got, needle(want)) {
					t.Errorf("preview must contain %q; got:\n%s", want, got)
				}
			}
			for _, missing := range c.WantMissing {
				if strings.Contains(got, needle(missing)) {
					t.Errorf("preview must not contain %q; got:\n%s", missing, got)
				}
			}
		})
	}
}

// TestListingPreview_HighlightsCodeInTheRecordedReply proves the pane reaches
// the markdown renderer and its code highlighter over REAL turn data, rather
// than printing the fence characters. The fixture's code arrives in a LATER
// turn, behind a plain opening prompt, which is the arrangement a first-message
// preview could never have shown.
func TestListingPreview_HighlightsCodeInTheRecordedReply(t *testing.T) {
	t.Parallel()
	doc := loadPreviewBodies(t)
	preview := kickstart.NewListingPreview(previewTheme(), doc.Listings, storedTurns(t, doc))

	body, err := preview.Body(transcriptSessionID)
	if err != nil {
		t.Fatalf("load preview: %v", err)
	}
	got := body.Render(72)

	if strings.Contains(stripRender(got), "```") {
		t.Errorf("the reply's code fence was printed literally instead of being rendered:\n%s", stripRender(got))
	}
	// The keyword must be carried by its OWN style run: highlighting colors the
	// keyword apart from the code around it, and unstyled code would not.
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("nothing in the pane is styled at all:\n%q", got)
	}
	keyword := paneStyleRun(got, "func")
	if keyword == "" {
		t.Errorf("the keyword %q in the recorded reply is not syntax-highlighted; got:\n%q", "func", got)
	}
}

// paneStyleRun returns the escape sequence immediately preceding word, or the
// empty string when the word is not carried by a style run of its own.
func paneStyleRun(rendered, word string) string {
	idx := strings.Index(rendered, word)
	if idx <= 0 {
		return ""
	}
	prefix := rendered[:idx]
	start := strings.LastIndex(prefix, "\x1b[")
	if start < 0 || strings.Contains(prefix[start:], "m") && !strings.HasSuffix(prefix, "m") {
		return ""
	}
	return prefix[start:]
}

// transcriptSessionID is the fixture session whose stored turns carry the
// multi-turn conversation with code in the reply.
const transcriptSessionID = "stored-1"

// TestListingPreview_LaysOutAtTheWidthItIsDrawnWith proves the load is
// width-INDEPENDENT: kit.BodySource hands the source no width at all, so the
// same loaded body must lay out differently at two widths and must return the
// first width's layout again when asked for it again. This is the regression
// guard for the stale-width bug - a body laid out at load time bakes in
// whatever width was current when the load was issued, which on mount is before
// the pane has been sized.
func TestListingPreview_LaysOutAtTheWidthItIsDrawnWith(t *testing.T) {
	t.Parallel()
	doc := loadPreviewBodies(t)
	preview := kickstart.NewListingPreview(previewTheme(), doc.Listings, storedTurns(t, doc))

	body, err := preview.Body(transcriptSessionID)
	if err != nil {
		t.Fatalf("load preview: %v", err)
	}

	const narrow, wide = 34, 96
	narrowOut := body.Render(narrow)
	wideOut := body.Render(wide)
	if narrowOut == wideOut {
		t.Fatalf("the preview rendered identically at %d and %d cells; the draw width is being ignored", narrow, wide)
	}
	if strings.Count(narrowOut, "\n") <= strings.Count(wideOut, "\n") {
		t.Errorf("the narrow render (%d rows) is not taller than the wide one (%d rows)",
			strings.Count(narrowOut, "\n")+1, strings.Count(wideOut, "\n")+1)
	}
	if again := body.Render(narrow); again != narrowOut {
		t.Error("re-rendering at the first width returned a different layout than the first time")
	}
	for _, width := range []int{narrow, wide} {
		for i, line := range strings.Split(body.Render(width), "\n") {
			if w := lipgloss.Width(line); w > width {
				t.Errorf("at width %d, line %d is %d cells wide: %q", width, i, w, stripRender(line))
			}
		}
	}
}

// TestListingPreview_SanitizesRecordedControlCharacters proves a recorded
// transcript cannot repaint the screen or break the pane's row measurements: an
// escape sequence captured from a tool's colored output is stripped, and the
// carriage returns and tabs around it are normalised, before anything is laid
// out.
func TestListingPreview_SanitizesRecordedControlCharacters(t *testing.T) {
	t.Parallel()
	doc := loadPreviewBodies(t)
	const hostile = "first\r\nsecond\tthird\rfourth \x1b[2J\x1b]0;retitled\x07 done"
	preview := kickstart.NewListingPreview(previewTheme(), doc.Listings,
		func(string) ([]ingest.Turn, error) {
			return []ingest.Turn{{
				Index: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, Content: hostile,
			}}, nil
		})

	body, err := preview.Body(doc.Listings[0].SessionID)
	if err != nil {
		t.Fatalf("load preview: %v", err)
	}
	got := body.Render(40)

	for _, bad := range []string{"\r", "\t", "\x1b[2J", "\x1b]0;", "retitled", "\x07"} {
		if strings.Contains(got, bad) {
			t.Errorf("rendered pane still carries %q; got:\n%q", bad, got)
		}
	}
	// The words around the stripped sequences survive: sanitising removes what a
	// terminal would ACT on, not the conversation.
	for _, want := range []string{"first", "second", "third", "fourth", "done"} {
		if !strings.Contains(flattenPane(got), want) {
			t.Errorf("rendered pane dropped %q along with the control characters; got:\n%q", want, got)
		}
	}
}

// TestListingPreview_ReadFailureSurfaces proves a failed store read is reported
// rather than swallowed, so the pane can render its actionable message instead
// of an empty body that looks like a session with nothing in it.
func TestListingPreview_ReadFailureSurfaces(t *testing.T) {
	t.Parallel()
	doc := loadPreviewBodies(t)
	wantErr := fmt.Errorf("database is locked")
	preview := kickstart.NewListingPreview(previewTheme(), doc.Listings,
		func(string) ([]ingest.Turn, error) { return nil, wantErr })

	if _, err := preview.Body(doc.Listings[0].SessionID); err == nil {
		t.Fatal("a failed transcript read must surface as an error")
	}
}

// TestListingPreview_NoStoreStillNamesTheSession proves the first-run path: with
// no local store at all the preview still names the highlighted session by its
// harness and project and says it is not imported yet, rather than failing or
// rendering blank.
func TestListingPreview_NoStoreStillNamesTheSession(t *testing.T) {
	t.Parallel()
	doc := loadPreviewBodies(t)
	preview := kickstart.NewListingPreview(previewTheme(), doc.Listings, nil)

	body, err := preview.Body(doc.Listings[0].SessionID)
	if err != nil {
		t.Fatalf("load preview: %v", err)
	}
	got := flattenPane(body.Render(50))
	for _, want := range []string{"harness: claude code", "project: acme/tool", "not imported yet"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview must contain %q; got:\n%s", want, got)
		}
	}
}
