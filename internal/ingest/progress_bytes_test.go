package ingest_test

import (
	_ "embed"
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

func TestProgressBytes(t *testing.T) {
	var fixture progressBytesFixture
	if err := yaml.Unmarshal(progressBytesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if seen[c.Name] {
			t.Fatalf("duplicate case %q", c.Name)
		}
		seen[c.Name] = true
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
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing case %q", name)
		}
	}
}
