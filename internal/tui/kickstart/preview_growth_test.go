package kickstart_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/peasant/internal/tui/transcriptview"
)

//go:embed testdata/preview_growth.yaml
var previewGrowthData []byte

// previewGrowthCase is one preview scrolled a fixed number of times, and what
// its RENDERED body must carry afterwards.
type previewGrowthCase struct {
	Name       string `yaml:"name"`
	FirstTurns int    `yaml:"firstTurns"`
	ChunkTurns int    `yaml:"chunkTurns"`
	Scrolls    int    `yaml:"scrolls"`
	// Continuable is whether this preview offers a scrolled continuation at all.
	// A false one is a session handed over whole.
	Continuable bool `yaml:"continuable"`
	// ExhaustAfterLastScroll makes the source report nothing more once the last
	// scroll has landed.
	ExhaustAfterLastScroll bool `yaml:"exhaustAfterLastScroll"`

	WantDrawnTurns           []int `yaml:"wantDrawnTurns"`
	WantNotDrawnTurns        []int `yaml:"wantNotDrawnTurns"`
	WantBodyGrowsEveryScroll bool  `yaml:"wantBodyGrowsEveryScroll"`
	WantOmissionNotice       bool  `yaml:"wantOmissionNotice"`
}

type previewGrowthDoc struct {
	RequiredCases []string            `yaml:"requiredCases"`
	Cases         []previewGrowthCase `yaml:"cases"`
}

func loadPreviewGrowthDoc(t *testing.T) previewGrowthDoc {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(previewGrowthData))
	dec.KnownFields(true)
	var doc previewGrowthDoc
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/preview_growth.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("preview_growth.yaml must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("the preview-growth fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.FirstTurns <= 0 || len(c.WantDrawnTurns) == 0 {
			t.Fatalf("the preview-growth fixture has an incomplete case: %+v", c)
		}
		if c.Continuable && (c.Scrolls <= 0 || c.ChunkTurns <= 0) {
			t.Fatalf("preview-growth case %q says it can be continued but never scrolls", c.Name)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("duplicate preview-growth case %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("the preview-growth fixture is missing required case %q", name)
		}
	}
	return doc
}

// growingTurnSource answers a preview whose turns grow one chunk per scroll.
// Its turns carry unmistakable text so an assertion can name a specific one.
type growingTurnSource struct {
	first    int
	chunk    int
	scrolls  int
	exhaust  bool
	loaded   int
	scrolled int
}

func (s *growingTurnSource) turns() []ingest.Turn {
	out := make([]ingest.Turn, 0, s.loaded)
	for i := range s.loaded {
		out = append(out, ingest.Turn{
			Index: i, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText,
			Content: growthTurnText(i),
		})
	}
	return out
}

func (s *growingTurnSource) Turns(string) ([]ingest.Turn, error) {
	if s.loaded == 0 {
		s.loaded = s.first
	}
	return s.turns(), nil
}

func (s *growingTurnSource) More(string) ([]ingest.Turn, bool, error) {
	s.scrolled++
	s.loaded += s.chunk
	return s.turns(), s.hasMore(), nil
}

func (s *growingTurnSource) hasMore() bool {
	if s.exhaust && s.scrolled >= s.scrolls {
		return false
	}
	return s.scrolled < s.scrolls
}

func (s *growingTurnSource) HasMore(string) bool { return s.hasMore() }

// growthTurnText names one turn unambiguously. The trailing marker keeps
// "turn 20" from matching inside "turn 200".
func growthTurnText(index int) string { return fmt.Sprintf("turn %d end", index) }

// TestListingPreview_TheDrawnBodyGrowsWithEveryScroll asserts what the pane
// DRAWS, not what it loaded.
//
// The defect this guards against passed every other gate: the continuation
// loaded and appended correctly while the pane drew only the first
// MaxRenderedTurns of what it held, so past that point scrolling changed
// nothing on screen.
func TestListingPreview_TheDrawnBodyGrowsWithEveryScroll(t *testing.T) {
	t.Parallel()
	doc := loadPreviewGrowthDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			const sessionID = "ses_growth"
			const width = 100
			listing := ftue.SessionListing{Harness: "claude-code", SessionID: sessionID}
			source := &growingTurnSource{first: c.FirstTurns, chunk: c.ChunkTurns, scrolls: c.Scrolls, exhaust: c.ExhaustAfterLastScroll}
			options := []kickstart.ListingPreviewOption{}
			if c.Continuable {
				options = append(options, kickstart.WithSessionMoreTurns(source.More, source.HasMore))
			}
			preview := kickstart.NewListingPreview(theme.New(theme.ModeDark),
				[]ftue.SessionListing{listing}, source.Turns, options...)

			body, err := preview.Body(sessionID)
			if err != nil {
				t.Fatalf("load the preview: %v", err)
			}
			drawn := len(strings.Split(body.Render(width), "\n"))
			for scroll := 1; scroll <= c.Scrolls; scroll++ {
				next, _, err := preview.MoreBody(sessionID)
				if err != nil {
					t.Fatalf("scroll %d: %v", scroll, err)
				}
				body = next
				grown := len(strings.Split(body.Render(width), "\n"))
				if c.WantBodyGrowsEveryScroll && grown <= drawn {
					t.Fatalf("scroll %d left the drawn body at %d lines, up from %d; the reader scrolled, more loaded, and the transcript on screen did not change",
						scroll, grown, drawn)
				}
				drawn = grown
			}

			out := flattenPreviewBody(body.Render(width))
			for _, index := range c.WantDrawnTurns {
				if want := growthTurnText(index); !strings.Contains(out, want) {
					t.Errorf("%q is loaded but not drawn; the reader cannot see what they scrolled for", want)
				}
			}
			for _, index := range c.WantNotDrawnTurns {
				if unwanted := growthTurnText(index); strings.Contains(out, unwanted) {
					t.Errorf("%q was drawn past the bound a whole handed-over session is under", unwanted)
				}
			}
			says := strings.Contains(out, "more turns not shown here")
			if says != c.WantOmissionNotice {
				t.Errorf("the pane says it left turns out = %v, want %v", says, c.WantOmissionNotice)
			}
			// Guard the guard: the fixture must actually reach past the draw
			// bound, or a case could pass without ever exercising it.
			if c.WantBodyGrowsEveryScroll && c.FirstTurns+c.ChunkTurns*c.Scrolls <= transcriptview.MaxRenderedTurns && len(c.WantDrawnTurns) > 0 {
				if c.Name == doc.RequiredCases[0] {
					t.Fatalf("case %q never crosses the %d-turn draw bound, so it cannot catch the defect it exists for",
						c.Name, transcriptview.MaxRenderedTurns)
				}
			}
		})
	}
}

// flattenPreviewBody strips styling and the gutter rail so an assertion can
// read the words the pane put on screen.
func flattenPreviewBody(s string) string {
	var b strings.Builder
	skip := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			skip = true
		case skip && r == 'm':
			skip = false
		case skip:
		case r == '│':
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
