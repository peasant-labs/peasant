package kit_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/previewsplit_continue.yaml
var previewContinueData []byte

// previewContinueCase is one scrolled continuation: what the source can serve,
// what the reader pressed, and what the pane must show and ask for.
type previewContinueCase struct {
	Name        string   `yaml:"name"`
	Continuable bool     `yaml:"continuable"`
	Chunks      int      `yaml:"chunks"`
	Presses     []string `yaml:"presses"`
	// MidPresses run while the chunk is in flight; AfterPresses run once it has
	// landed.
	MidPresses   []string `yaml:"midPresses"`
	AfterPresses []string `yaml:"afterPresses"`

	WantLoadingMoreInFlight bool `yaml:"wantLoadingMoreInFlight"`
	MoreRequests            int  `yaml:"moreRequests"`

	WantVisibleAfter     []string `yaml:"wantVisibleAfter"`
	WantMissingAfter     []string `yaml:"wantMissingAfter"`
	WantTopLineAfter     string   `yaml:"wantTopLineAfter"`
	WantLoadingMoreAfter bool     `yaml:"wantLoadingMoreAfter"`
	WantHighlight        string   `yaml:"wantHighlight"`
}

type previewContinueDoc struct {
	RequiredCases []string              `yaml:"requiredCases"`
	Width         int                   `yaml:"width"`
	Height        int                   `yaml:"height"`
	InitialLines  int                   `yaml:"initialLines"`
	ChunkLines    int                   `yaml:"chunkLines"`
	Cases         []previewContinueCase `yaml:"cases"`
}

func loadPreviewContinue(t *testing.T) previewContinueDoc {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(previewContinueData))
	dec.KnownFields(true)
	var doc previewContinueDoc
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/previewsplit_continue.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("previewsplit_continue.yaml must hold exactly one document")
	}
	if doc.Width <= 0 || doc.Height <= 1 {
		t.Fatalf("fixture declares a %dx%d region; the pane needs content rows and a chrome row", doc.Width, doc.Height)
	}
	if doc.InitialLines <= doc.Height+previewContinueThresholdRows {
		t.Fatalf("fixture declares %d initial lines for a %d-row pane; the body must overflow the pane by more than the threshold so a case can stop short of the end",
			doc.InitialLines, doc.Height)
	}
	if doc.ChunkLines <= 0 {
		t.Fatal("fixture declares no chunk length, so a continuation would add nothing")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" {
			t.Fatal("a continuation case declares no name")
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("duplicate continuation case %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("continuation fixture is missing required case %q", name)
		}
	}
	return doc
}

// previewContinueThresholdRows spells out the shipped threshold rather than
// importing it, so a silent change to how early the pane fetches is a failure
// here rather than a change nobody sees.
const previewContinueThresholdRows = 3

// loadingMoreLabel is the pane's own chrome while a chunk is in flight, spelled
// out for the same reason.
const loadingMoreLabel = "loading more..."

// growingSource serves a body that lengthens by a fixed number of lines each
// time it is continued, without changing a line already in it. It records how
// many times it was asked, which is what proves the pane asks when it should
// and stays quiet when it should not.
type growingSource struct {
	initial int
	chunk   int
	chunks  int

	mu       sync.Mutex
	served   int
	requests int
}

func (s *growingSource) lines() kit.PreviewBody {
	return linesBody{lines: numberedLines("body", s.initial+s.served*s.chunk)}
}

func (s *growingSource) Body(string) (kit.PreviewBody, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines(), nil
}

func (s *growingSource) Continuable(string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served < s.chunks
}

func (s *growingSource) MoreBody(string) (kit.PreviewBody, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if s.served < s.chunks {
		s.served++
	}
	return s.lines(), s.served < s.chunks, nil
}

func (s *growingSource) requested() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// uncontinuableSource implements only kit.BodySource, so a split over it must never
// try to continue.
type uncontinuableSource struct{ inner *growingSource }

func (s uncontinuableSource) Body(id string) (kit.PreviewBody, error) { return s.inner.Body(id) }

var (
	_ kit.ContinuableBodySource = (*growingSource)(nil)
	_ kit.BodySource            = uncontinuableSource{}
)

// TestPreviewSplit_ContinuesOnScroll drives the REAL split and asserts what a
// scroll toward the end of a continuable preview asks for and shows.
func TestPreviewSplit_ContinuesOnScroll(t *testing.T) {
	t.Parallel()
	doc := loadPreviewContinue(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			source := &growingSource{initial: doc.InitialLines, chunk: doc.ChunkLines, chunks: c.Chunks}
			var bodies kit.BodySource = source
			if !c.Continuable {
				bodies = uncontinuableSource{inner: source}
			}
			items := []kit.ListItem{kit.StringItem("alpha"), kit.StringItem("bravo"), kit.StringItem("charlie")}
			split := kit.NewPreviewSplitWithBodies(darkTheme(), kit.NewListLeftPane(kit.NewList(darkTheme(), items)), bodies)
			split.SetSize(doc.Width, doc.Height)
			split.Focus()
			for _, msg := range collectMsgs(split.Load()) {
				split, _ = split.Update(msg)
			}

			// Focus the preview so the movement keys reach the viewport.
			split, _ = split.Update(keyPress(t, "ctrl+l"))

			// Every command the presses produce is kept, not just the last: a
			// case that presses twice is asserting that the pane issued ONE
			// request, and dropping the first would hide a second.
			var pending []tea.Cmd
			for _, key := range c.Presses {
				var cmd tea.Cmd
				split, cmd = split.Update(keyPress(t, key))
				if cmd != nil {
					pending = append(pending, cmd)
				}
			}

			inFlight := stripANSI(split.View())
			if got := strings.Contains(inFlight, loadingMoreLabel); got != c.WantLoadingMoreInFlight {
				t.Errorf("while the chunk is in flight the pane says %q = %v, want %v; screen:\n%s",
					loadingMoreLabel, got, c.WantLoadingMoreInFlight, inFlight)
			}

			for _, key := range c.MidPresses {
				split, _ = split.Update(keyPress(t, key))
			}
			for _, cmd := range pending {
				for _, msg := range collectMsgs(cmd) {
					split, _ = split.Update(msg)
				}
			}
			for _, key := range c.AfterPresses {
				var cmd tea.Cmd
				split, cmd = split.Update(keyPress(t, key))
				for _, msg := range collectMsgs(cmd) {
					split, _ = split.Update(msg)
				}
			}

			final := stripANSI(split.View())
			assertPaneShows(t, "after the chunk landed", final, c.WantVisibleAfter, c.WantMissingAfter)
			if got := strings.Contains(final, loadingMoreLabel); got != c.WantLoadingMoreAfter {
				t.Errorf("after the chunk landed the pane says %q = %v, want %v; screen:\n%s",
					loadingMoreLabel, got, c.WantLoadingMoreAfter, final)
			}
			if c.WantTopLineAfter != "" {
				top := strings.TrimSpace(previewRowText(final, 0, doc.Width))
				if top != c.WantTopLineAfter {
					t.Errorf("the pane's first preview row is %q, want %q; appending moved the reader's place. screen:\n%s",
						top, c.WantTopLineAfter, final)
				}
			}
			if got := source.requested(); got != c.MoreRequests {
				t.Errorf("the pane asked for the next chunk %d times, want %d", got, c.MoreRequests)
			}
			if id, _ := split.HighlightedID(); id != c.WantHighlight {
				t.Errorf("highlight = %q, want %q", id, c.WantHighlight)
			}
		})
	}
}
