package kickstart_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

//go:embed testdata/preview_notice.yaml
var previewNoticeData []byte

// previewNoticeCase declares one session's turns, the note its reader reports,
// and what the rendered pane must and must not carry.
type previewNoticeCase struct {
	Name         string                 `yaml:"name"`
	SessionID    string                 `yaml:"session_id"`
	Turns        []testutil.TurnFixture `yaml:"turns"`
	Notice       string                 `yaml:"notice"`
	WantContains []string               `yaml:"want_contains"`
	WantMissing  []string               `yaml:"want_missing"`
}

type previewNoticeDoc struct {
	RequiredCases []string            `yaml:"required_cases"`
	Cases         []previewNoticeCase `yaml:"cases"`
}

func loadPreviewNoticeDoc(t *testing.T) previewNoticeDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(previewNoticeData))
	decoder.KnownFields(true)
	var doc previewNoticeDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode preview notice fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("preview notice fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("preview notice fixture declares no required cases")
	}
	present := make(map[string]struct{}, len(doc.Cases))
	for _, testCase := range doc.Cases {
		if testCase.Name == "" || testCase.SessionID == "" {
			t.Fatalf("preview notice fixture has an incomplete case: %+v", testCase)
		}
		if len(testCase.WantContains)+len(testCase.WantMissing) == 0 {
			t.Fatalf("preview notice case %q asserts nothing, so it passes whatever the pane renders", testCase.Name)
		}
		for _, needle := range append(append([]string{}, testCase.WantContains...), testCase.WantMissing...) {
			if strings.TrimSpace(needle) == "" {
				t.Fatalf("preview notice case %q declares an empty needle", testCase.Name)
			}
		}
		if _, duplicate := present[testCase.Name]; duplicate {
			t.Fatalf("preview notice fixture has a duplicate case name %q", testCase.Name)
		}
		present[testCase.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := present[name]; !ok {
			t.Fatalf("preview notice fixture is missing required case %q", name)
		}
	}
	return doc
}

// TestListingPreview_ShowsTheTruncationNoteWithItsTurns proves the note stands
// WITH the turns rather than instead of them, and that a session with no turns
// never carries one: a note about a prefix would be false beside an empty pane.
func TestListingPreview_ShowsTheTruncationNoteWithItsTurns(t *testing.T) {
	t.Parallel()
	doc := loadPreviewNoticeDoc(t)
	for _, testCase := range doc.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			listing := ftue.SessionListing{Harness: string(ingest.HarnessOpenCode), SessionID: testCase.SessionID, ProjectName: "peasant"}
			// A case with no turns models the un-imported pane, which
			// testutil.Turns refuses to build because an empty transcript has
			// nothing to assert on. The pane itself is what this case asserts on.
			var turns []ingest.Turn
			if len(testCase.Turns) > 0 {
				turns = testutil.Turns(t, testCase.SessionID, testCase.Turns)
			}
			preview := kickstart.NewListingPreview(previewTheme(), []ftue.SessionListing{listing},
				func(string) ([]ingest.Turn, error) { return turns, nil },
				kickstart.WithSessionPreviewNotice(func(string) string { return testCase.Notice }))
			body, err := preview.Body(testCase.SessionID)
			if err != nil {
				t.Fatalf("load the preview body: %v", err)
			}
			// The pane styles and wraps its prose, so a raw render interleaves
			// escape sequences and line breaks with the words. The assertions are
			// about what the reader SEES, so they run over the plain text.
			rendered := plainPreviewText(body.Render(120))
			for _, needle := range testCase.WantContains {
				if !strings.Contains(rendered, needle) {
					t.Errorf("rendered preview does not carry %q; rendered:\n%s", needle, rendered)
				}
			}
			for _, needle := range testCase.WantMissing {
				if strings.Contains(rendered, needle) {
					t.Errorf("rendered preview carries %q, which it must not; rendered:\n%s", needle, rendered)
				}
			}
		})
	}
}

// plainPreviewText reduces a rendered pane to the words on screen: no styling,
// and every run of whitespace collapsed, so a phrase the pane wrapped over two
// lines still reads as one phrase.
func plainPreviewText(rendered string) string {
	return strings.Join(strings.Fields(ansi.Strip(rendered)), " ")
}
