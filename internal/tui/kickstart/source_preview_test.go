package kickstart_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/source_preview.yaml
var sourcePreviewData []byte

const (
	expectedSourcePreviewListingCount        = 3
	expectedSourcePreviewTranscriptLineCount = 3
	expectedSourcePreviewReaderCaseCount     = 5
	expectedSourcePreviewBodyCaseCount       = 3
	expectedSourcePreviewRenderCaseCount     = 4
)

// sourceTranscriptState says what the harness left on disk for one reader case.
type sourceTranscriptState string

const (
	sourceTranscriptPresent sourceTranscriptState = "present"
	sourceTranscriptEmpty   sourceTranscriptState = "empty"
	sourceTranscriptMissing sourceTranscriptState = "missing"
)

func (s sourceTranscriptState) valid() bool {
	switch s {
	case sourceTranscriptPresent, sourceTranscriptEmpty, sourceTranscriptMissing:
		return true
	default:
		return false
	}
}

// sourceReaderCase drives the reader over one transcript state.
type sourceReaderCase struct {
	Name              string                   `yaml:"name"`
	Harness           string                   `yaml:"harness"`
	Origin            ftue.SessionSourceOrigin `yaml:"origin"`
	Transcript        sourceTranscriptState    `yaml:"transcript"`
	WantTurnCount     int                      `yaml:"wantTurnCount"`
	WantErrorContains string                   `yaml:"wantErrorContains"`
}

// sourcePreviewBodyCase is what the pane must show for one highlighted row.
type sourcePreviewBodyCase struct {
	Name         string   `yaml:"name"`
	Highlight    string   `yaml:"highlight"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

// sourcePreviewRenderCase is one captured screen of the mounted step.
type sourcePreviewRenderCase struct {
	Name   string               `yaml:"name"`
	Theme  selectionRenderTheme `yaml:"theme"`
	Width  int                  `yaml:"width"`
	Height int                  `yaml:"height"`
}

type sourcePreviewDoc struct {
	ExpectedListingCount        int                       `yaml:"expectedListingCount"`
	ExpectedTranscriptLineCount int                       `yaml:"expectedTranscriptLineCount"`
	ExpectedReaderCaseCount     int                       `yaml:"expectedReaderCaseCount"`
	ExpectedBodyCaseCount       int                       `yaml:"expectedBodyCaseCount"`
	ExpectedRenderCaseCount     int                       `yaml:"expectedRenderCaseCount"`
	Width                       int                       `yaml:"width"`
	SourceSessionID             string                    `yaml:"sourceSessionId"`
	EmptySourceSessionID        string                    `yaml:"emptySourceSessionId"`
	PlainSessionID              string                    `yaml:"plainSessionId"`
	Transcript                  []string                  `yaml:"transcript"`
	Listings                    []ftue.SessionListing     `yaml:"listings"`
	ReaderCases                 []sourceReaderCase        `yaml:"readerCases"`
	BodyCases                   []sourcePreviewBodyCase   `yaml:"bodyCases"`
	RenderCases                 []sourcePreviewRenderCase `yaml:"renderCases"`
}

func decodeSourcePreview(data []byte) (sourcePreviewDoc, error) {
	var doc sourcePreviewDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/source_preview.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("source_preview.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedListingCount != expectedSourcePreviewListingCount || len(doc.Listings) != expectedSourcePreviewListingCount {
		return doc, fmt.Errorf("source preview listings: declared=%d actual=%d required=%d",
			doc.ExpectedListingCount, len(doc.Listings), expectedSourcePreviewListingCount)
	}
	if doc.ExpectedTranscriptLineCount != expectedSourcePreviewTranscriptLineCount || len(doc.Transcript) != expectedSourcePreviewTranscriptLineCount {
		return doc, fmt.Errorf("source preview transcript lines: declared=%d actual=%d required=%d",
			doc.ExpectedTranscriptLineCount, len(doc.Transcript), expectedSourcePreviewTranscriptLineCount)
	}
	if doc.ExpectedReaderCaseCount != expectedSourcePreviewReaderCaseCount || len(doc.ReaderCases) != expectedSourcePreviewReaderCaseCount {
		return doc, fmt.Errorf("source preview reader cases: declared=%d actual=%d required=%d",
			doc.ExpectedReaderCaseCount, len(doc.ReaderCases), expectedSourcePreviewReaderCaseCount)
	}
	if doc.ExpectedBodyCaseCount != expectedSourcePreviewBodyCaseCount || len(doc.BodyCases) != expectedSourcePreviewBodyCaseCount {
		return doc, fmt.Errorf("source preview body cases: declared=%d actual=%d required=%d",
			doc.ExpectedBodyCaseCount, len(doc.BodyCases), expectedSourcePreviewBodyCaseCount)
	}
	if doc.ExpectedRenderCaseCount != expectedSourcePreviewRenderCaseCount || len(doc.RenderCases) != expectedSourcePreviewRenderCaseCount {
		return doc, fmt.Errorf("source preview render cases: declared=%d actual=%d required=%d",
			doc.ExpectedRenderCaseCount, len(doc.RenderCases), expectedSourcePreviewRenderCaseCount)
	}
	if doc.Width <= 0 {
		return doc, fmt.Errorf("source preview width is %d; a pane with no width renders nothing to assert on", doc.Width)
	}
	ids := map[string]bool{}
	for _, listing := range doc.Listings {
		if listing.SessionID == "" || listing.Harness == "" || ids[listing.SessionID] {
			return doc, fmt.Errorf("source preview listing %q is empty or duplicated", listing.SessionID)
		}
		ids[listing.SessionID] = true
	}
	for _, id := range []string{doc.SourceSessionID, doc.EmptySourceSessionID, doc.PlainSessionID} {
		if !ids[id] {
			return doc, fmt.Errorf("source preview names session %q, which no listing carries", id)
		}
	}
	names := map[string]bool{}
	for _, c := range doc.ReaderCases {
		if c.Name == "" || names[c.Name] || !c.Transcript.valid() {
			return doc, fmt.Errorf("source preview reader case %q is empty, duplicated, or has an invalid transcript state", c.Name)
		}
		names[c.Name] = true
		// A success case over a real transcript must expect real turns.
		// Otherwise the row passes even when the reader parses nothing.
		if c.WantErrorContains == "" && c.Transcript == sourceTranscriptPresent && c.WantTurnCount <= 0 {
			return doc, fmt.Errorf("source preview reader case %q reads a transcript but expects no turns", c.Name)
		}
	}
	for _, c := range doc.BodyCases {
		if c.Name == "" || names[c.Name] || !ids[c.Highlight] {
			return doc, fmt.Errorf("source preview body case %q is empty, duplicated, or highlights an unknown session", c.Name)
		}
		names[c.Name] = true
		if len(c.WantContains)+len(c.WantMissing) == 0 {
			return doc, fmt.Errorf("source preview body case %q asserts nothing", c.Name)
		}
		for _, value := range append(append([]string{}, c.WantContains...), c.WantMissing...) {
			if strings.TrimSpace(value) == "" {
				return doc, fmt.Errorf("source preview body case %q declares an empty needle, which matches whatever the pane shows", c.Name)
			}
		}
	}
	for _, c := range doc.RenderCases {
		if c.Name == "" || names[c.Name] || !c.Theme.valid() || c.Width <= 0 || c.Height <= 0 {
			return doc, fmt.Errorf("source preview render case %q is empty, duplicated, or has an invalid theme or size", c.Name)
		}
		names[c.Name] = true
	}
	return doc, nil
}

func loadSourcePreviewDoc(t *testing.T) sourcePreviewDoc {
	t.Helper()
	doc, err := decodeSourcePreview(sourcePreviewData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// writeTranscript puts the fixture transcript where a harness would leave it.
// The file is the ONLY copy: nothing in these tests imports it.
func writeTranscript(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript %q: %v", path, err)
	}
	return path
}

// sourcePreviewListings points the fixture listings at real transcript files:
// the first session gets the recorded conversation, the second gets an empty
// file, and the third keeps no transcript location at all.
func sourcePreviewListings(t *testing.T, doc sourcePreviewDoc) []ftue.SessionListing {
	t.Helper()
	dir := t.TempDir()
	full := writeTranscript(t, dir, "recorded.jsonl", doc.Transcript)
	empty := writeTranscript(t, dir, "empty.jsonl", nil)

	listings := append([]ftue.SessionListing(nil), doc.Listings...)
	for i := range listings {
		switch listings[i].SessionID {
		case doc.SourceSessionID:
			listings[i].Source.Path = full
		case doc.EmptySourceSessionID:
			listings[i].Source.Path = empty
		}
	}
	return listings
}

// TestSourceTurns_ReadsTheHarnessTranscript drives the REAL reader over real
// files on disk: the harness indexer parses them, and the same fold the session
// viewer uses turns the entries into turns. Nothing is stubbed, and the failures
// that must stay visible are exercised beside the success.
func TestSourceTurns_ReadsTheHarnessTranscript(t *testing.T) {
	t.Parallel()
	doc := loadSourcePreviewDoc(t)

	for _, c := range doc.ReaderCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "recorded.jsonl")
			switch c.Transcript {
			case sourceTranscriptPresent:
				path = writeTranscript(t, dir, "recorded.jsonl", doc.Transcript)
			case sourceTranscriptEmpty:
				path = writeTranscript(t, dir, "recorded.jsonl", nil)
			case sourceTranscriptMissing:
				// The path stays unwritten, which is what a session removed
				// between the scan and the preview looks like.
			}
			listing := ftue.SessionListing{
				Harness:   c.Harness,
				SessionID: doc.SourceSessionID,
				Source:    ftue.SessionSource{Path: path, Origin: c.Origin},
			}
			reader := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, []ftue.SessionListing{listing})

			turns, err := reader.Turns(doc.SourceSessionID)
			if c.WantErrorContains != "" {
				if err == nil {
					t.Fatalf("read %d turns, want the failure %q", len(turns), c.WantErrorContains)
				}
				if !strings.Contains(err.Error(), c.WantErrorContains) {
					t.Fatalf("error %q must name %q", err, c.WantErrorContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("read the harness transcript: %v", err)
			}
			if len(turns) != c.WantTurnCount {
				t.Fatalf("read %d turns, want %d", len(turns), c.WantTurnCount)
			}
		})
	}
}

// TestSourceTurns_ReportsSessionsWithoutATranscript proves the reader stays
// silent about a session discovery found no transcript for. It is the state the
// pane reports as not imported, and it must not become an error.
func TestSourceTurns_ReportsSessionsWithoutATranscript(t *testing.T) {
	t.Parallel()
	doc := loadSourcePreviewDoc(t)
	reader := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, sourcePreviewListings(t, doc))

	if reader.Previewable(doc.PlainSessionID) {
		t.Error("a session with no transcript location must not be previewable from its source")
	}
	turns, err := reader.Turns(doc.PlainSessionID)
	if err != nil || len(turns) != 0 {
		t.Fatalf("read %d turns and error %v, want no turns and no error", len(turns), err)
	}
	if !reader.Previewable(doc.SourceSessionID) {
		t.Error("a session with a transcript on disk must be previewable from its source")
	}
}

// TestSourceTurns_CachesParsedSessionsWithinItsBound proves both halves of the
// in-memory cache, by deleting the file after the first read: a cached session
// answers without the file, and a session the bound evicted must read the file
// again. The cache holds parsed turns only, so nothing survives the process.
func TestSourceTurns_CachesParsedSessionsWithinItsBound(t *testing.T) {
	t.Parallel()
	doc := loadSourcePreviewDoc(t)
	listings := sourcePreviewListings(t, doc)
	reader := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, listings, kickstart.WithSourceTurnsCacheSize(1))

	first, err := reader.Turns(doc.SourceSessionID)
	if err != nil || len(first) == 0 {
		t.Fatalf("first read: %d turns, error %v", len(first), err)
	}
	path := sourcePathFor(t, listings, doc.SourceSessionID)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the transcript: %v", err)
	}
	cached, err := reader.Turns(doc.SourceSessionID)
	if err != nil {
		t.Fatalf("a cached session must answer without its file: %v", err)
	}
	if len(cached) != len(first) {
		t.Fatalf("cached read has %d turns, want %d", len(cached), len(first))
	}

	// One more session fills the bound of one and drops the first.
	if _, err := reader.Turns(doc.EmptySourceSessionID); err != nil {
		t.Fatalf("read the second session: %v", err)
	}
	if _, err := reader.Turns(doc.SourceSessionID); err == nil {
		t.Fatal("the evicted session must read its file again and report that the file is gone")
	}
}

func sourcePathFor(t *testing.T, listings []ftue.SessionListing, sessionID string) string {
	t.Helper()
	for _, listing := range listings {
		if listing.SessionID == sessionID {
			return listing.Source.Path
		}
	}
	t.Fatalf("no listing carries session %q", sessionID)
	return ""
}

// TestListingPreview_ShowsUnimportedSessions proves the pane end to end with no
// store at all: the reader is the only session source, and each of the three
// listings gets the answer that is true for it.
func TestListingPreview_ShowsUnimportedSessions(t *testing.T) {
	t.Parallel()
	doc := loadSourcePreviewDoc(t)
	listings := sourcePreviewListings(t, doc)
	reader := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, listings)
	preview := kickstart.NewListingPreview(theme.New(theme.ModeDark), listings, reader.Turns)

	for _, c := range doc.BodyCases {
		t.Run(c.Name, func(t *testing.T) {
			body, err := preview.Body(c.Highlight)
			if err != nil {
				t.Fatalf("preview body for %q: %v", c.Highlight, err)
			}
			got := flattenPreviewPane(body.Render(doc.Width))
			for _, want := range c.WantContains {
				if !strings.Contains(got, previewNeedle(want)) {
					t.Errorf("preview must contain %q; got:\n%s", want, got)
				}
			}
			for _, missing := range c.WantMissing {
				if strings.Contains(got, previewNeedle(missing)) {
					t.Errorf("preview must not contain %q; got:\n%s", missing, got)
				}
			}
		})
	}
}

// flattenPreviewPane strips styling and joins wrapped rows, so a phrase the pane
// split across a wrap point is still findable.
func flattenPreviewPane(s string) string {
	out := stripRender(s)
	// The pane draws a gutter rail beside wrapped body rows. Fold it away so a
	// phrase the pane split across rows is still one phrase here.
	out = strings.ReplaceAll(out, "│", " ")
	return strings.Join(strings.Fields(out), " ")
}

func previewNeedle(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestSourcePreview_RenderGolden captures the whole mounted step with the cursor
// on a session Peasant has not imported, in both themes and at two widths, so
// the transcript beside the tree is visible in the test artifact.
func TestSourcePreview_RenderGolden(t *testing.T) {
	doc := loadSourcePreviewDoc(t)
	for _, c := range doc.RenderCases {
		t.Run(c.Name, func(t *testing.T) {
			view := buildSourcePreviewStep(t, doc, c).View()
			if !strings.Contains(flattenPreviewPane(view), previewNeedle("write to a temp directory first")) {
				t.Fatalf("the pane shows no transcript for the highlighted session:\n%s", stripRender(view))
			}
			golden.RequireEqual(t, []byte(view))
		})
	}
}

// buildSourcePreviewStep drives the REAL mounted program - the same scanner
// source, registry, and preview the command wires - onto the session that has a
// harness transcript and no store row.
func buildSourcePreviewStep(t *testing.T, doc sourcePreviewDoc, c sourcePreviewRenderCase) kickstart.Program {
	t.Helper()
	th := theme.New(renderThemeFor(t, c.Theme))
	listings := sourcePreviewListings(t, doc)
	source := kickstart.NewScannerTreeSource(listings, withFixturePathResolver())
	reader := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, listings)
	preview := kickstart.NewListingPreview(th, listings, reader.Turns,
		kickstart.WithListingPreviewContextSource(source))

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	loaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	p := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:   th,
		Draft:   draft,
		Source:  source,
		Preview: preview,
	})
	p.SetSize(c.Width, c.Height)
	p = declineOAuth(t, p)
	return cursorOntoRecordedTranscript(t, p)
}

// cursorOntoRecordedTranscript steps the tree cursor down until the pane shows
// the recorded conversation. Deriving the number of steps keeps the capture
// correct when the tree adds or reorders a row.
func cursorOntoRecordedTranscript(t *testing.T, p kickstart.Program) kickstart.Program {
	t.Helper()
	const maxSteps = 12
	for i := 0; i < maxSteps; i++ {
		if strings.Contains(flattenPreviewPane(p.View()), previewNeedle("write to a temp directory first")) {
			return p
		}
		p = pressAndDrain(p, 'j')
	}
	t.Fatalf("no row within %d steps previews the recorded transcript:\n%s", maxSteps, stripRender(p.View()))
	return p
}
