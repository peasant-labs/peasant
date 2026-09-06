package ingestprogress_test

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ingestprogress"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/inline.yaml
var inlineYAML []byte

type inlineFixture struct {
	Name     string   `yaml:"name"`
	Theme    string   `yaml:"theme"`
	Width    int      `yaml:"width"`
	Height   int      `yaml:"height"`
	Contains []string `yaml:"contains"`
	Absent   []string `yaml:"absent"`
}

func loadInlineFixtures(t *testing.T) []inlineFixture {
	t.Helper()
	var doc struct {
		Required []string        `yaml:"required_cases"`
		Cases    []inlineFixture `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(inlineYAML))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] || c.Width < 1 || c.Height < 1 || len(c.Contains) == 0 {
			t.Fatalf("invalid fixture %+v", c)
		}
		seen[c.Name] = true
	}
	if len(doc.Required) == 0 {
		t.Fatal("missing required-name manifest")
	}
	for _, name := range doc.Required {
		if !seen[name] {
			t.Fatalf("missing required case %q", name)
		}
	}
	return doc.Cases
}

func TestInlineProgressLayout(t *testing.T) {
	for _, c := range loadInlineFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			mode, err := theme.ModeFromConfig(c.Theme)
			if err != nil {
				t.Fatal(err)
			}
			state := ingest.NewProgressState()
			state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageIndex, Total: 10})
			state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageIndex, Done: 4, Total: 10})
			started := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
			model := ingestprogress.NewModel(state, animation.IngestAnimation(), theme.New(mode), started, nil)
			updated, _ := model.Update(tea.WindowSizeMsg{Width: c.Width, Height: c.Height})
			updated, _ = updated.Update(ingestprogress.TickMsg(started.Add(8 * time.Second)))
			view := updated.View()
			if view.AltScreen {
				t.Fatal("harvest must stay inline in the user's terminal")
			}
			plain := ansi.Strip(view.Content)
			for _, want := range c.Contains {
				if !strings.Contains(plain, want) {
					t.Errorf("missing %q:\n%s", want, plain)
				}
			}
			for _, absent := range c.Absent {
				if strings.Contains(plain, absent) {
					t.Errorf("unexpected %q:\n%s", absent, plain)
				}
			}
			lines := strings.Split(plain, "\n")
			if len(lines) > c.Height {
				t.Errorf("rendered %d rows into %d", len(lines), c.Height)
			}
			if c.Height == 40 && len(lines) == c.Height {
				t.Error("inline progress padded to full screen height")
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > c.Width {
					t.Errorf("row exceeds terminal width: %q", line)
				}
			}
			if view.Content != updated.View().Content {
				t.Error("View mutated clock or rendering state")
			}
		})
	}
}
