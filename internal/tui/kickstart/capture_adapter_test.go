package kickstart

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

//go:embed testdata/capture_adapter.yaml
var adapterFixture []byte

type adapterCase struct {
	Name      string `yaml:"name"`
	SourceCap int64  `yaml:"source_cap"`
	TurnCap   int    `yaml:"turn_cap"`
	Notice    string `yaml:"notice"`
}

func TestTerminalCaptureAdapter(t *testing.T) {
	var doc struct {
		Required []string      `yaml:"required_names"`
		Cases    []adapterCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(adapterFixture, &doc); err != nil {
		t.Fatal(err)
	}
	cases := map[string]adapterCase{}
	for _, c := range doc.Cases {
		if _, exists := cases[c.Name]; exists {
			t.Fatalf("duplicate %q", c.Name)
		}
		cases[c.Name] = c
	}
	for _, name := range doc.Required {
		if _, ok := cases[name]; !ok {
			t.Fatalf("missing %q", name)
		}
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Name {
			case "terminal-options-are-not-web-options":
				opts := TerminalCaptureOptions()
				if opts.SourceCapBytes != c.SourceCap || opts.TurnCap != c.TurnCap {
					t.Fatalf("options %+v", opts)
				}
			case "terminal-source-cap-cuts-with-notice", "lru-eviction-closes-capture":
				dir := t.TempDir()
				path := filepath.Join(dir, "session.jsonl")
				var data strings.Builder
				for i := 0; i < 30; i++ {
					fmt.Fprintf(&data, "{\"type\":\"user\",\"uuid\":\"u%d\",\"message\":{\"role\":\"user\",\"content\":\"hello %d\"}}\n", i, i)
				}
				if err := os.WriteFile(path, []byte(data.String()), 0600); err != nil {
					t.Fatal(err)
				}
				var listings []ftue.SessionListing
				for i := 0; i < 5; i++ {
					listings = append(listings, ftue.SessionListing{SessionID: fmt.Sprintf("4b0d2e17-6c58-4a39-8f21-%012d", i), Harness: string(ingest.HarnessClaudeCode), Source: ftue.SessionSource{Origin: ftue.SessionSourceOriginFile, Path: path, Root: dir}})
				}
				opts := TerminalCaptureOptions()
				opts.FirstReadBytes = 100
				opts.SliceReadBytes = 100
				if c.Name == "terminal-source-cap-cuts-with-notice" {
					opts.SourceCapBytes = 500
				}
				reader := NewSourceTurns(&ingest.OSFileSystem{}, listings, WithSourceTurnsCaptureOptions(opts))
				if c.Name == "terminal-source-cap-cuts-with-notice" {
					_, _, err := reader.FirstTurns(listings[0].SessionID)
					if err != nil {
						t.Fatal(err)
					}
					for reader.HasMore(listings[0].SessionID) {
						if _, _, err = reader.MoreTurns(listings[0].SessionID); err != nil {
							t.Fatal(err)
						}
					}
					if !strings.Contains(reader.EndNotice(listings[0].SessionID), c.Notice) {
						t.Fatalf("notice %q", reader.EndNotice(listings[0].SessionID))
					}
					return
				}
				for i := 0; i < 4; i++ {
					if _, _, err := reader.FirstTurns(listings[i].SessionID); err != nil {
						t.Fatal(err)
					}
				}
				old := reader.entries[listings[0].SessionID].capture.Load()
				if old == nil || old.RetainedBytes() == 0 {
					t.Fatal("capture has no retained prefix")
				}
				if _, _, err := reader.FirstTurns(listings[4].SessionID); err != nil {
					t.Fatal(err)
				}
				if old.RetainedBytes() != 0 {
					t.Fatal("eviction did not close capture")
				}
			case "source-changed-sentence":
				if !strings.Contains(SourceChangedNotice, c.Notice) {
					t.Fatal(SourceChangedNotice)
				}
			case "late-result-sentence":
				if !strings.Contains(LateResultNotice, c.Notice) {
					t.Fatal(LateResultNotice)
				}
			case "grown-sentence":
				if !strings.Contains(GrownNotice, c.Notice) {
					t.Fatal(GrownNotice)
				}
			case "unavailable-sentence":
				if !strings.Contains(sourcepreview.ErrUnavailable.Error(), c.Notice) {
					t.Fatal(sourcepreview.ErrUnavailable)
				}
			case "oversized-record-sentence":
				// The capture's oversized-record sentence is propagated unchanged.
				reader := NewSourceTurns(&ingest.OSFileSystem{}, nil)
				entry := reader.entryFor("test")
				reader.apply(entry, nil, sourcepreview.Page{Continuation: sourcepreview.ContinuationCut, Notice: c.Notice})
				if reader.EndNotice("test") != c.Notice {
					t.Fatal("cut notice lost")
				}
			default:
				t.Fatalf("unknown case %q", c.Name)
			}
		})
	}
}
