package discoveryprogress

import (
	_ "embed"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/spinner.yaml
var spinnerYAML []byte

//go:embed testdata/spinner.manifest.yaml
var spinnerManifest []byte

type spinnerPhaseFixture struct {
	Phase   string `yaml:"phase"`
	Done    int    `yaml:"done"`
	Total   int    `yaml:"total"`
	Rereads int    `yaml:"rereads"`
}

type spinnerCase struct {
	Name     string               `yaml:"name"`
	Previous *spinnerPhaseFixture `yaml:"previous"`
	Phase    string               `yaml:"phase"`
	Done     int                  `yaml:"done"`
	Total    int                  `yaml:"total"`
	Found    int                  `yaml:"found"`
	Rereads  []int                `yaml:"rereads"`
	Want     string               `yaml:"want"`
}

func loadSpinnerCases(t *testing.T) []spinnerCase {
	t.Helper()
	var fixture struct {
		Cases []spinnerCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(spinnerYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(spinnerManifest, "discovery spinner")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "discovery spinner"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

func TestSpinnerView(t *testing.T) {
	for _, c := range loadSpinnerCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			spinner := New(io.Discard, false)
			if previous := c.Previous; previous != nil {
				spinner.SetPhase(previous.Phase)
				spinner.SetProgress(previous.Done, previous.Total)
				spinner.SetDiscoveryRequiredRereads(previous.Rereads)
			}
			spinner.SetPhase(c.Phase)
			if c.Total > 0 {
				spinner.SetProgress(c.Done, c.Total)
			}
			for range c.Found {
				spinner.Increment()
			}
			for _, rereads := range c.Rereads {
				spinner.SetDiscoveryRequiredRereads(rereads)
			}
			if got := spinner.View(); got != c.Want {
				t.Fatalf("View() = %q, want %q", got, c.Want)
			}
		})
	}
}

// A nil spinner is what a quiet caller holds; every method must tolerate it.
func TestNilSpinnerIsQuiet(t *testing.T) {
	var spinner *Spinner
	spinner.SetPhase("phase")
	spinner.SetProgress(1, 2)
	spinner.SetDiscoveryRequiredRereads(1)
	spinner.Increment()
	spinner.Stop()
	if got := spinner.View(); got != "" {
		t.Fatalf("nil View() = %q, want empty", got)
	}
}

type lineWriter struct{ lines chan string }

func (w *lineWriter) Write(p []byte) (int, error) {
	w.lines <- string(p)
	return len(p), nil
}

// A terminal spinner redraws its line once per frame, cycling the frame set,
// and erases the line when stopped.
func TestTerminalSpinnerAdvancesFramesAndClearsOnStop(t *testing.T) {
	writer := &lineWriter{lines: make(chan string, 64)}
	spinner := New(writer, true)
	spinner.SetPhase("Discovering Claude Code sessions...")
	spinner.SetProgress(1, 2)
	spinner.SetDiscoveryRequiredRereads(1)
	ctx := testwait.Context(t)
	var drawn []string
	for len(drawn) < 2 {
		select {
		case line := <-writer.lines:
			drawn = append(drawn, line)
		case <-ctx.Done():
			spinner.Stop()
			t.Fatalf("terminal spinner drew %d lines, want 2", len(drawn))
		}
	}
	spinner.Stop()
	for i, line := range drawn {
		want := "\r\x1b[2K" + frames[i] + " Discovering Claude Code sessions... (1/2) · 1 required re-read"
		if line != want {
			t.Fatalf("frame %d = %q, want %q", i, line, want)
		}
	}
	var last string
	for len(writer.lines) > 0 {
		last = <-writer.lines
	}
	if last != "\r\x1b[2K" {
		t.Fatalf("last write after Stop = %q, want a line erase", last)
	}
}
