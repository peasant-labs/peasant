package ingest_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ingestprogress"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/progress_bytes.yaml
var progressBytesYAML []byte

//go:embed testdata/progress_bytes_invalid.yaml
var invalidProgressBytesYAML []byte

//go:embed testdata/progress_bytes_empty_estimate.yaml
var emptyEstimateYAML []byte

type progressBytesFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []struct {
		Name         string `yaml:"name"`
		Total        int64  `yaml:"total"`
		Delta        int64  `yaml:"delta"`
		WantDone     int64  `yaml:"wantDone"`
		WantText     string `yaml:"wantText"`
		AbsentText   string `yaml:"absentText"`
		WantEstimate string `yaml:"wantEstimate"`
	} `yaml:"cases"`
}

func decodeProgressBytes(data []byte) (progressBytesFixture, error) {
	var fixture progressBytesFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, err
	}
	if len(fixture.RequiredNames) == 0 {
		return fixture, fmt.Errorf("requiredNames must not be empty")
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			return fixture, fmt.Errorf("empty or duplicate case %q", c.Name)
		}
		seen[c.Name] = true
		if c.WantEstimate == "" {
			return fixture, fmt.Errorf("case %q has an empty wantEstimate", c.Name)
		}
	}
	required := make(map[string]bool)
	for _, name := range fixture.RequiredNames {
		if name == "" || required[name] {
			return fixture, fmt.Errorf("empty or duplicate required name %q", name)
		}
		required[name] = true
		if !seen[name] {
			return fixture, fmt.Errorf("missing required case %q", name)
		}
	}
	return fixture, nil
}

func TestProgressBytesFixtureRejectsMisspelledKey(t *testing.T) {
	_, err := decodeProgressBytes(invalidProgressBytesYAML)
	if err == nil || !strings.Contains(err.Error(), "wantEstimatte") {
		t.Fatalf("misspelled key: got %v, want unknown-field error", err)
	}
}

func TestProgressBytesFixtureRejectsEmptyEstimate(t *testing.T) {
	_, err := decodeProgressBytes(emptyEstimateYAML)
	if err == nil || !strings.Contains(err.Error(), "empty wantEstimate") {
		t.Fatalf("empty estimate: got %v", err)
	}
}

func TestProgressBytes(t *testing.T) {
	fixture, err := decodeProgressBytes(progressBytesYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			state := ingest.NewProgressState()
			now := time.Unix(100, 0)
			model := ingestprogress.New(ingestprogress.Options{Theme: theme.New(theme.ModeDark), StartedAt: now})
			model.SetSize(120, 40)
			state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageExtract, Total: 2, BytesTotal: c.Total})
			model, _ = model.Update(ingestprogress.ObserveMsg{At: now, Snapshot: state.Snapshot()})
			state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageExtract, Delta: 1, BytesDelta: c.Delta})
			snapshot := state.Snapshot()
			if got := snapshot[ingest.StageExtract].BytesDone; got != c.WantDone {
				t.Fatalf("bytes done = %d, want %d", got, c.WantDone)
			}
			model, _ = model.Update(ingestprogress.ObserveMsg{At: now.Add(time.Second), Snapshot: snapshot})
			view := ansi.Strip(model.View())
			if c.WantText != "" && !strings.Contains(view, c.WantText) {
				t.Errorf("missing %q in %q", c.WantText, view)
			}
			if c.AbsentText != "" && strings.Contains(view, c.AbsentText) {
				t.Errorf("unexpected %q in %q", c.AbsentText, view)
			}
			if !strings.Contains(view, c.WantEstimate) {
				t.Errorf("missing %q in %q", c.WantEstimate, view)
			}
		})
	}
}
