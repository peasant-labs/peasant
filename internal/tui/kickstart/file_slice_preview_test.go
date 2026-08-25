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
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

//go:embed testdata/file_slice_preview.yaml
var fileSlicePreviewData []byte

// fileSlicePreviewCase reads one transcript both ways and compares.
type fileSlicePreviewCase struct {
	Name       string `yaml:"name"`
	Transcript string `yaml:"transcript"`
	SliceBytes int64  `yaml:"slice_bytes"`
	// ExpectMultipleSlices is whether these bounds actually make the read take
	// more than one slice. Without it a case could pass by reading the whole
	// transcript at once and never touching a seam.
	ExpectMultipleSlices bool `yaml:"expect_multiple_slices"`
	// ExpectToolOutputInTurns requires the tool results to have survived onto
	// the turns that called them, which is what a split seam would lose.
	ExpectToolOutputInTurns bool `yaml:"expect_tool_output_in_turns"`
}

type fileSliceTranscript struct {
	Name  string   `yaml:"name"`
	Lines []string `yaml:"lines"`
}

type fileSlicePreviewDoc struct {
	RequiredCases []string               `yaml:"required_cases"`
	Cases         []fileSlicePreviewCase `yaml:"cases"`
	Transcripts   []fileSliceTranscript  `yaml:"transcripts"`
}

func loadFileSlicePreviewDoc(t *testing.T) fileSlicePreviewDoc {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(fileSlicePreviewData))
	dec.KnownFields(true)
	var doc fileSlicePreviewDoc
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/file_slice_preview.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("file_slice_preview.yaml must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("the file-slice fixture declares no required cases")
	}
	known := make(map[string][]string, len(doc.Transcripts))
	for _, transcript := range doc.Transcripts {
		if transcript.Name == "" || len(transcript.Lines) == 0 {
			t.Fatalf("the file-slice fixture has an empty transcript: %+v", transcript)
		}
		known[transcript.Name] = transcript.Lines
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.SliceBytes <= 0 {
			t.Fatalf("the file-slice fixture has an incomplete case: %+v", c)
		}
		if _, ok := known[c.Transcript]; !ok {
			t.Fatalf("file-slice case %q names transcript %q, which the fixture does not hold", c.Name, c.Transcript)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("duplicate file-slice case %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("the file-slice fixture is missing required case %q", name)
		}
	}
	return doc
}

// TestSourceTurns_ASlicedFileReadEqualsTheWholeFileRead is the property the
// whole file-paging design rests on: reading a transcript one bounded slice at
// a time must produce EXACTLY what reading it whole produces.
//
// It compares each turn's tool calls and their output as well as its role and
// content, because the way a seam breaks is that a tool call loses the result
// that answers it - a comparison of roles and prose would not notice.
func TestSourceTurns_ASlicedFileReadEqualsTheWholeFileRead(t *testing.T) {
	t.Parallel()
	doc := loadFileSlicePreviewDoc(t)
	byName := make(map[string][]string, len(doc.Transcripts))
	for _, transcript := range doc.Transcripts {
		byName[transcript.Name] = transcript.Lines
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			const sessionID = "4b0d2e17-6c58-4a39-8f21-0d7e3b9c5a12"
			path := writeTranscript(t, dir, "recorded.jsonl", byName[c.Transcript])
			listing := ftue.SessionListing{
				Harness:   string(ingest.HarnessClaudeCode),
				SessionID: sessionID,
				Source:    ftue.SessionSource{Path: path, Root: dir, Origin: ftue.SessionSourceOriginFile},
			}
			reader := func(slice int64) *kickstart.SourceTurns {
				return kickstart.NewSourceTurns(&ingest.OSFileSystem{}, []ftue.SessionListing{listing},
					kickstart.WithSourceTurnsGitResolver(testutil.NoGitResolver()),
					kickstart.WithSourceTurnsSalt(salt.Salt{}),
					kickstart.WithSourceTurnsSliceBudget(slice),
					kickstart.WithSourceTurnsFirstPageBudget(slice),
					kickstart.WithSourceTurnsPreviewBudget(slice))
			}

			// The reference: the whole-file read the preview used before paging.
			whole, err := reader(0).Turns(sessionID)
			if err != nil {
				t.Fatalf("read the transcript whole: %v", err)
			}
			if len(whole) == 0 {
				t.Fatal("the whole-file read produced no turns, so the comparison proves nothing")
			}

			// The sliced read, driven to exhaustion the way a reader scrolling does.
			sliced := reader(c.SliceBytes)
			turns, err := sliced.Turns(sessionID)
			if err != nil {
				t.Fatalf("read the first slice: %v", err)
			}
			scrolls := 0
			for sliced.HasMore(sessionID) {
				if scrolls++; scrolls > fileSliceMaxScrolls {
					t.Fatalf("the read still reported more after %d scrolls at a %d byte budget; it does not terminate",
						scrolls, c.SliceBytes)
				}
				turns, _, err = sliced.MoreTurns(sessionID)
				if err != nil {
					t.Fatalf("scroll %d: %v", scrolls, err)
				}
			}
			if (scrolls > 0) != c.ExpectMultipleSlices {
				t.Fatalf("the read took %d scrolls at a %d byte budget, want multiple slices = %v",
					scrolls, c.SliceBytes, c.ExpectMultipleSlices)
			}

			if len(whole) != len(turns) {
				t.Fatalf("the sliced read produced %d turns and the whole-file read %d", len(turns), len(whole))
			}
			for i := range whole {
				if got, want := fileSliceTurnSignature(turns[i]), fileSliceTurnSignature(whole[i]); got != want {
					t.Fatalf("turn %d differs after slicing:\n sliced: %s\n whole : %s", i, got, want)
				}
			}
			if c.ExpectToolOutputInTurns {
				assertToolOutputSurvived(t, turns)
			}
		})
	}
}

// fileSliceMaxScrolls stops a non-terminating read from hanging the suite. It
// is far above any fixture's real slice count.
const fileSliceMaxScrolls = 2000

// fileSliceTurnSignature is the whole rendered substance of one turn. It
// deliberately includes each tool call's OUTPUT: that is the thing a split seam
// loses, and a signature over role and content alone would call two turns equal
// while one of them had lost its tool result.
func fileSliceTurnSignature(turn ingest.Turn) string {
	var b strings.Builder
	fmt.Fprintf(&b, "role=%s depth=%d type=%s content=%q", turn.Role, turn.Depth, turn.EntryType, turn.Content)
	for _, call := range turn.ToolCalls {
		fmt.Fprintf(&b, " [tool id=%s name=%s error=%v output=%q]", call.ID, call.Name, call.IsError, call.Result)
	}
	return b.String()
}

// assertToolOutputSurvived proves the results really are on the turns, so the
// equivalence above is comparing something rather than two equally empty reads.
func assertToolOutputSurvived(t *testing.T, turns []ingest.Turn) {
	t.Helper()
	var outputs []string
	for _, turn := range turns {
		for _, call := range turn.ToolCalls {
			outputs = append(outputs, call.Result)
		}
	}
	joined := strings.Join(outputs, "\n")
	for _, marker := range []string{"UNMISTAKABLE_TOOL_OUTPUT", "SECOND_TOOL_OUTPUT"} {
		if !strings.Contains(joined, marker) {
			t.Errorf("the tool output %q is not on any turn; a slice boundary between a call and its result dropped it", marker)
		}
	}
}
