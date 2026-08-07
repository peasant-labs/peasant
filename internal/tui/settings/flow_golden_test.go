package settings

import (
	"bytes"
	_ "embed"
	"io"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/flow_render.yaml
var flowRenderData []byte

type flowRenderCase struct {
	Name   string `yaml:"name"`
	State  string `yaml:"state"`
	Theme  string `yaml:"theme"`
	Width  int    `yaml:"width"`
	Height int    `yaml:"height"`
}

type flowRenderDoc struct {
	ExpectedCaseCount int              `yaml:"expectedCaseCount"`
	Cases             []flowRenderCase `yaml:"cases"`
}

func loadFlowRenderDoc(t *testing.T) flowRenderDoc {
	t.Helper()
	var doc flowRenderDoc
	dec := yaml.NewDecoder(bytes.NewReader(flowRenderData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/flow_render.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("flow_render.yaml must hold exactly one document")
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	return doc
}

func themeFor(t *testing.T, name string) theme.Theme {
	t.Helper()
	switch name {
	case "dark":
		return theme.New(theme.ModeDark)
	case "light":
		return theme.New(theme.ModeLight)
	default:
		t.Fatalf("unknown theme %q", name)
		return theme.Theme{}
	}
}

// buildFlowForRender builds a deterministic flow driven into the requested
// state at the requested size.
func buildFlowForRender(t *testing.T, th theme.Theme, state string, w, h int) Flow {
	t.Helper()
	d, err := NewDraft("/tmp/settings-golden/config.yaml", config.BaseConfig())
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	f := NewFlow(th, testRegistry(), d)
	f.SetSize(w, h)
	switch state {
	case "step":
		// initial connection step
	case "receipt":
		f = send(f, "space") // a change to summarize
		f = send(f, "tab")   // into advanced
		f = send(f, "tab")   // to receipt
	case "confirm":
		f = send(f, "esc") // open exit-confirm overlay
	case "help":
		f = send(f, "?") // open the grouped keybinding help overlay
	default:
		t.Fatalf("unknown state %q", state)
	}
	return f
}

func TestFlow_RenderGolden(t *testing.T) {
	doc := loadFlowRenderDoc(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			th := themeFor(t, c.Theme)
			f := buildFlowForRender(t, th, c.State, c.Width, c.Height)
			golden.RequireEqual(t, []byte(f.View()))
		})
	}
}

// TestFlow_RenderWidthInvariant proves every rendered line fits the width the
// flow was sized to, in every case — the goldens cannot bake in an overflow.
func TestFlow_RenderWidthInvariant(t *testing.T) {
	doc := loadFlowRenderDoc(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			th := themeFor(t, c.Theme)
			f := buildFlowForRender(t, th, c.State, c.Width, c.Height)
			for i, line := range splitLines(f.View()) {
				if got := lipgloss.Width(line); got > c.Width {
					t.Errorf("line %d is %d cells, exceeds width %d:\n%q", i, got, c.Width, line)
				}
			}
		})
	}
}
