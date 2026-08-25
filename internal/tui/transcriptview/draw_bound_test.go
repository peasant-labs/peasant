package transcriptview_test

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
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/peasant/internal/tui/transcriptview"
)

//go:embed testdata/draw_bound.yaml
var drawBoundData []byte

// drawBoundCase is one document and what the renderer must draw of it.
type drawBoundCase struct {
	Name      string `yaml:"name"`
	Unbounded bool   `yaml:"unbounded"`
	// TurnsOverBound sizes the document relative to MaxRenderedTurns.
	TurnsOverBound   int `yaml:"turnsOverBound"`
	WantOmittedCount int `yaml:"wantOmittedCount"`
	// WantDrawnAtOffset and WantNotDrawnAtOffset name turn numbers relative to
	// MaxRenderedTurns: -1 is the last turn the bound allows, 0 is the first one
	// past it.
	WantDrawnAtOffset    []int `yaml:"wantDrawnAtOffset"`
	WantNotDrawnAtOffset []int `yaml:"wantNotDrawnAtOffset"`
}

type drawBoundDoc struct {
	RequiredCases []string        `yaml:"requiredCases"`
	Cases         []drawBoundCase `yaml:"cases"`
}

func loadDrawBoundDoc(t *testing.T) drawBoundDoc {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(drawBoundData))
	dec.KnownFields(true)
	var doc drawBoundDoc
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/draw_bound.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("draw_bound.yaml must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("the draw-bound fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" {
			t.Fatal("a draw-bound case declares no name")
		}
		if len(c.WantDrawnAtOffset) == 0 {
			t.Fatalf("draw-bound case %q names no turn that must be drawn", c.Name)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("duplicate draw-bound case %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("the draw-bound fixture is missing required case %q", name)
		}
	}
	return doc
}

// TestRender_DrawBoundFollowsHowTheTurnsGotHere proves the renderer draws a
// whole handed-over transcript up to its bound and says what it left out, and
// draws EVERY turn of a transcript the reader scrolled for.
//
// The second half is a regression: a preview that loaded turns by scrolling
// used to draw the first MaxRenderedTurns of them and summarize the rest away,
// so scrolling loaded content the reader could never see.
func TestRender_DrawBoundFollowsHowTheTurnsGotHere(t *testing.T) {
	t.Parallel()
	doc := loadDrawBoundDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			total := transcriptview.MaxRenderedTurns + c.TurnsOverBound
			turns := make([]ingest.Turn, 0, total)
			for i := range total {
				turns = append(turns, ingest.Turn{
					Index: i, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText,
					Content: drawBoundTurnText(i),
				})
			}
			renderer := transcriptview.New(theme.New(theme.ModeDark))
			document := renderer.Document(turns)
			if c.Unbounded {
				document = renderer.UnboundedDocument(turns)
			}
			if document.TurnCount() != len(turns) {
				t.Errorf("TurnCount = %d, want %d - the count reports the whole transcript, drawn or not",
					document.TurnCount(), len(turns))
			}
			out := flatten(document.Render(wideWidth))

			omission := fmt.Sprintf("(%d more turns not shown here)", c.WantOmittedCount)
			if c.WantOmittedCount > 0 {
				if !strings.Contains(out, omission) {
					t.Errorf("the pane must say %q; a transcript that just stops reads as a session that stopped", omission)
				}
			} else if strings.Contains(out, "more turns not shown here") {
				t.Errorf("the pane claims it left turns out, but every turn of this document must be drawn; got:\n%s", out)
			}
			for _, offset := range c.WantDrawnAtOffset {
				want := drawBoundTurnText(transcriptview.MaxRenderedTurns + offset)
				if !strings.Contains(out, want) {
					t.Errorf("%q was not drawn; the reader loaded it and cannot see it", want)
				}
			}
			for _, offset := range c.WantNotDrawnAtOffset {
				unwanted := drawBoundTurnText(transcriptview.MaxRenderedTurns + offset)
				if strings.Contains(out, unwanted) {
					t.Errorf("%q was drawn past the bound this document is under", unwanted)
				}
			}
		})
	}
}

// drawBoundTurnText names one turn unambiguously. The trailing marker keeps
// "turn 20" from matching inside "turn 200", which would let a bounded render
// pass an assertion about a turn it never drew.
func drawBoundTurnText(index int) string {
	return fmt.Sprintf("turn %d end", index)
}
