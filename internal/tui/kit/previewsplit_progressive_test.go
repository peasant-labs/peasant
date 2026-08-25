package kit_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/previewsplit_progressive.yaml
var previewProgressiveData []byte

// previewProgressiveCase is one two-step load: what the source answers, what
// the user did between the two steps, and what the pane must show after each.
type previewProgressiveCase struct {
	Name        string   `yaml:"name"`
	Progressive bool     `yaml:"progressive"`
	More        bool     `yaml:"more"`
	MidPresses  []string `yaml:"midPresses"`

	WantFirstVisible     []string `yaml:"wantFirstVisible"`
	WantFirstMissing     []string `yaml:"wantFirstMissing"`
	WantFirstRestPending bool     `yaml:"wantFirstRestPending"`

	WantFinalVisible     []string `yaml:"wantFinalVisible"`
	WantFinalMissing     []string `yaml:"wantFinalMissing"`
	WantFinalRestPending bool     `yaml:"wantFinalRestPending"`
	// WantFinalTopLine is the content of the pane's FIRST preview row after the
	// second step. It is what pins the scroll rule: an empty value skips the
	// assertion for a case the rule does not speak about.
	WantFinalTopLine string `yaml:"wantFinalTopLine"`

	WantWholeBodyRead bool   `yaml:"wantWholeBodyRead"`
	WantHighlight     string `yaml:"wantHighlight"`
}

type previewProgressiveDoc struct {
	RequiredCases []string                 `yaml:"requiredCases"`
	Width         int                      `yaml:"width"`
	Height        int                      `yaml:"height"`
	SliceLines    int                      `yaml:"sliceLines"`
	WholeLines    int                      `yaml:"wholeLines"`
	Items         []string                 `yaml:"items"`
	Cases         []previewProgressiveCase `yaml:"cases"`
}

func loadPreviewProgressive(t *testing.T) previewProgressiveDoc {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(previewProgressiveData))
	dec.KnownFields(true)
	var doc previewProgressiveDoc
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/previewsplit_progressive.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("previewsplit_progressive.yaml must hold exactly one document")
	}
	if doc.Width <= 0 || doc.Height <= 1 {
		t.Fatalf("fixture declares a %dx%d region; the pane needs a content row and a chrome row", doc.Width, doc.Height)
	}
	if doc.SliceLines <= doc.Height {
		t.Fatalf("fixture declares %d slice lines for a %d-row pane; the slice must overflow the pane so a case can scroll inside it",
			doc.SliceLines, doc.Height)
	}
	if doc.WholeLines <= doc.SliceLines {
		t.Fatalf("fixture declares %d whole-body lines against %d slice lines; the whole body must be the larger read",
			doc.WholeLines, doc.SliceLines)
	}
	if len(doc.Items) == 0 {
		t.Fatal("fixture declares no list items, so there is no highlight to load a preview for")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" {
			t.Fatal("a progressive-load case declares no name")
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("duplicate progressive-load case %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("progressive-load fixture is missing required case %q", name)
		}
	}
	return doc
}

// linesBody is a PreviewBody of fixed, already-laid-out lines.
type linesBody struct{ lines []string }

func (b linesBody) Render(int) string { return strings.Join(b.lines, "\n") }

func numberedLines(prefix string, count int) []string {
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, fmt.Sprintf("%s-%02d", prefix, i))
	}
	return out
}

// twoStepSource answers a preview in two steps. It records whether the whole
// body was ever read, which is what proves a session that fits its first read
// is never read twice.
type twoStepSource struct {
	slice linesBody
	whole linesBody
	more  bool

	mu        sync.Mutex
	wholeRead bool
}

func (s *twoStepSource) Body(string) (kit.PreviewBody, error) {
	s.mu.Lock()
	s.wholeRead = true
	s.mu.Unlock()
	return s.whole, nil
}

func (s *twoStepSource) FirstBody(string) (kit.PreviewBody, bool, error) {
	return s.slice, s.more, nil
}

func (s *twoStepSource) readWholeBody() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wholeRead
}

// plainSource implements only kit.BodySource, so a split over it must keep the
// single-step load untouched.
type plainSource struct{ inner *twoStepSource }

func (s plainSource) Body(id string) (kit.PreviewBody, error) { return s.inner.Body(id) }

var (
	_ kit.ProgressiveBodySource = (*twoStepSource)(nil)
	_ kit.BodySource            = plainSource{}
)

// TestPreviewSplit_ProgressiveLoad drives the REAL split through both steps of
// a two-step preview and asserts what the pane shows after each.
func TestPreviewSplit_ProgressiveLoad(t *testing.T) {
	t.Parallel()
	doc := loadPreviewProgressive(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			source := &twoStepSource{
				slice: linesBody{lines: numberedLines("slice", doc.SliceLines)},
				whole: linesBody{lines: numberedLines("whole", doc.WholeLines)},
				more:  c.More,
			}
			var bodies kit.BodySource = source
			if !c.Progressive {
				bodies = plainSource{inner: source}
			}
			items := make([]kit.ListItem, 0, len(doc.Items))
			for _, label := range doc.Items {
				items = append(items, kit.StringItem(label))
			}
			split := kit.NewPreviewSplitWithBodies(darkTheme(), kit.NewListLeftPane(kit.NewList(darkTheme(), items)), bodies)
			split.SetSize(doc.Width, doc.Height)
			split.Focus()

			// Step one: run the load and apply its result.
			var follow tea.Cmd
			for _, msg := range collectMsgs(split.Load()) {
				var cmd tea.Cmd
				split, cmd = split.Update(msg)
				if cmd != nil {
					follow = cmd
				}
			}
			first := stripANSI(split.View())
			assertPaneShows(t, "after the first step", first, c.WantFirstVisible, c.WantFirstMissing)
			assertRestPending(t, "after the first step", first, c.WantFirstRestPending)

			// What the user did while the rest was still being read.
			for _, key := range c.MidPresses {
				split, _ = split.Update(keyPress(t, key))
			}

			// Step two: deliver the whole body.
			for _, msg := range collectMsgs(follow) {
				split, _ = split.Update(msg)
			}
			final := stripANSI(split.View())
			assertPaneShows(t, "after the second step", final, c.WantFinalVisible, c.WantFinalMissing)
			assertRestPending(t, "after the second step", final, c.WantFinalRestPending)

			if c.WantFinalTopLine != "" {
				top := strings.TrimSpace(previewRowText(final, 0, doc.Width))
				if top != c.WantFinalTopLine {
					t.Errorf("the pane's first preview row is %q, want %q; screen:\n%s", top, c.WantFinalTopLine, final)
				}
			}
			if got := source.readWholeBody(); got != c.WantWholeBodyRead {
				t.Errorf("the whole body was read = %v, want %v", got, c.WantWholeBodyRead)
			}
			if id, _ := split.HighlightedID(); id != c.WantHighlight {
				t.Errorf("highlight = %q, want %q", id, c.WantHighlight)
			}
		})
	}
}

// restPendingLabel is the pane's own chrome while it reads the rest. The test
// spells it out rather than importing it, so a silent rewording of the shipped
// label is a failure here rather than a change nobody sees.
const restPendingLabel = "loading the rest..."

func assertRestPending(t *testing.T, when, screen string, want bool) {
	t.Helper()
	if got := strings.Contains(screen, restPendingLabel); got != want {
		t.Errorf("%s the pane says %q = %v, want %v; screen:\n%s", when, restPendingLabel, got, want, screen)
	}
}

func assertPaneShows(t *testing.T, when, screen string, visible, missing []string) {
	t.Helper()
	for _, want := range visible {
		if !strings.Contains(screen, want) {
			t.Errorf("%s the pane must show %q; screen:\n%s", when, want, screen)
		}
	}
	for _, unwanted := range missing {
		if strings.Contains(screen, unwanted) {
			t.Errorf("%s the pane must not show %q; screen:\n%s", when, unwanted, screen)
		}
	}
}

// previewRowText returns the preview (right-hand) part of one rendered row.
func previewRowText(screen string, row, width int) string {
	lines := strings.Split(screen, "\n")
	if row >= len(lines) {
		return ""
	}
	line := lines[row]
	// The right pane is everything past the list pane and the one-cell divider.
	// Find the divider column by the marker the split draws there.
	if idx := strings.IndexAny(line, "<>│"); idx >= 0 && idx+1 <= len(line) {
		return line[idx+1:]
	}
	return line
}
