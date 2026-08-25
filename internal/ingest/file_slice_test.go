package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
)

//go:embed testdata/file_slice.yaml
var fileSliceData []byte

// fileSliceCase describes one synthetic transcript file and the budget it is
// read at.
type fileSliceCase struct {
	Name      string `yaml:"name"`
	LineBytes int    `yaml:"line_bytes"`
	LineCount int    `yaml:"line_count"`
	// OversizedLineBytes, when set, makes one line that long so a case can
	// cross its budget with a single record.
	OversizedLineBytes   int   `yaml:"oversized_line_bytes"`
	BudgetBytes          int64 `yaml:"budget_bytes"`
	MaxSlices            int   `yaml:"max_slices"`
	ExpectMultipleSlices bool  `yaml:"expect_multiple_slices"`
	NoTrailingNewline    bool  `yaml:"no_trailing_newline"`
}

type fileSliceDoc struct {
	RequiredCases []string        `yaml:"required_cases"`
	Cases         []fileSliceCase `yaml:"cases"`
}

func loadFileSliceDoc(t *testing.T) fileSliceDoc {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(fileSliceData))
	dec.KnownFields(true)
	var doc fileSliceDoc
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/file_slice.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("file_slice.yaml must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("the file-slice fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.LineBytes <= 0 || c.LineCount <= 0 || c.BudgetBytes <= 0 || c.MaxSlices <= 0 {
			t.Fatalf("the file-slice fixture has an incomplete case: %+v", c)
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

// TestFileTranscriptSlicer proves the four properties a sliced file read rests
// on: every slice ends on a line, the slices are the file, the read
// terminates, and an oversized line arrives whole.
func TestFileTranscriptSlicer(t *testing.T) {
	t.Parallel()
	doc := loadFileSliceDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			path, content := writeFileSliceFixture(t, c)
			session := ingest.DiscoveredSession{
				SessionID:        fileSliceSessionID(t),
				Harness:          ingest.HarnessClaudeCode,
				SourcePath:       ingest.ResolvedPath(path),
				OriginalRoot:     ingest.ResolvedPath(filepath.Dir(path)),
				TranscriptOrigin: ingest.TranscriptOriginFile,
			}
			slicer := ingest.NewFileTranscriptSlicer(&ingest.OSFileSystem{})
			if !slicer.Supported() {
				t.Fatal("the production filesystem cannot read a byte range, so nothing here can be sliced")
			}

			var rebuilt []byte
			var cursor ingest.TranscriptSliceCursor
			slices, oversizedWhole := 0, false
			for slices < c.MaxSlices {
				slice, err := slicer.MaterializeTranscriptSlice(t.Context(), session, c.BudgetBytes, cursor)
				if err != nil {
					t.Fatalf("slice %d: %v", slices+1, err)
				}
				slices++
				if len(slice.Data) > 0 {
					last := slice.Data[len(slice.Data)-1]
					atEnd := int64(len(rebuilt)+len(slice.Data)) == int64(len(content))
					if last != '\n' && !atEnd {
						t.Fatalf("slice %d ends with %q rather than a newline, and it is not the end of the file; a reader would see half a record",
							slices, last)
					}
					if strings.Contains(string(slice.Data), fileSliceOversizedMarker) {
						oversizedWhole = oversizedWhole || bytes.Count(slice.Data, []byte(fileSliceOversizedMarker)) == 1 &&
							len(slice.Data) >= c.OversizedLineBytes
					}
				}
				rebuilt = append(rebuilt, slice.Data...)
				cursor = slice.Next
				if !slice.More {
					break
				}
				if len(slice.Data) == 0 {
					t.Fatalf("slice %d read nothing yet reported more to come; the read cannot make progress", slices)
				}
			}
			if cursor.FileOffset() != int64(len(content)) {
				t.Fatalf("the read stopped at byte %d of a %d byte file after %d slices", cursor.FileOffset(), len(content), slices)
			}
			if !bytes.Equal(rebuilt, content) {
				t.Fatalf("the slices rebuild %d bytes of a %d byte file; content was lost or repeated", len(rebuilt), len(content))
			}
			if (slices > 1) != c.ExpectMultipleSlices {
				t.Fatalf("the file took %d slices at a %d byte budget, want multiple slices = %v",
					slices, c.BudgetBytes, c.ExpectMultipleSlices)
			}
			if c.OversizedLineBytes > 0 && !oversizedWhole {
				t.Fatal("the oversized line was not delivered whole in one slice")
			}
		})
	}
}

// fileSliceOversizedMarker starts the one line a case makes longer than its
// whole budget, so an assertion can find it.
const fileSliceOversizedMarker = "OVERSIZED"

// writeFileSliceFixture builds the case's transcript file and returns its path
// and its exact bytes.
func writeFileSliceFixture(t *testing.T, c fileSliceCase) (string, []byte) {
	t.Helper()
	var b bytes.Buffer
	for i := range c.LineCount {
		if c.OversizedLineBytes > 0 && i == c.LineCount/2 {
			b.WriteString(fileSliceOversizedMarker)
			b.WriteString(strings.Repeat("x", c.OversizedLineBytes-len(fileSliceOversizedMarker)))
		} else {
			line := fmt.Sprintf("line-%04d-", i)
			if len(line) < c.LineBytes {
				line += strings.Repeat("y", c.LineBytes-len(line))
			}
			b.WriteString(line)
		}
		if i < c.LineCount-1 || !c.NoTrailingNewline {
			b.WriteByte('\n')
		}
	}
	path := filepath.Join(t.TempDir(), "recorded.jsonl")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, b.Bytes()
}

func fileSliceSessionID(t *testing.T) ingest.SessionID {
	t.Helper()
	id, err := ingest.NewSessionID("4b0d2e17-6c58-4a39-8f21-0d7e3b9c5a12")
	if err != nil {
		t.Fatal(err)
	}
	return id
}
