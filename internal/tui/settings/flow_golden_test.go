package settings

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/flow_render.yaml
var flowRenderData []byte

const (
	requiredFlowRenderCaseCount    = 20
	requiredFlowViewportStateCount = 3
	requiredFlowViewportThemeCount = 2
	requiredFlowViewportCaseCount  = 6
	requiredFlowViewportWidth      = 80
	requiredFlowViewportHeight     = 24
)

type flowRenderState string

const (
	flowRenderStep                       flowRenderState = "step"
	flowRenderReceipt                    flowRenderState = "receipt"
	flowRenderConfirm                    flowRenderState = "confirm"
	flowRenderHelp                       flowRenderState = "help"
	flowRenderViewportStepTop            flowRenderState = "viewport-step-top"
	flowRenderViewportStepBottom         flowRenderState = "viewport-step-bottom"
	flowRenderViewportReceiptErrorBottom flowRenderState = "viewport-receipt-error-bottom"
)

func (s flowRenderState) valid() bool {
	switch s {
	case flowRenderStep, flowRenderReceipt, flowRenderConfirm, flowRenderHelp,
		flowRenderViewportStepTop, flowRenderViewportStepBottom, flowRenderViewportReceiptErrorBottom:
		return true
	default:
		return false
	}
}

func (s flowRenderState) viewportState() bool {
	return s == flowRenderViewportStepTop || s == flowRenderViewportStepBottom ||
		s == flowRenderViewportReceiptErrorBottom
}

type flowRenderTheme string

const (
	flowRenderThemeDark  flowRenderTheme = "dark"
	flowRenderThemeLight flowRenderTheme = "light"
)

func (t flowRenderTheme) valid() bool {
	return t == flowRenderThemeDark || t == flowRenderThemeLight
}

type flowRenderCase struct {
	Name   string          `yaml:"name"`
	State  flowRenderState `yaml:"state"`
	Theme  flowRenderTheme `yaml:"theme"`
	Width  int             `yaml:"width"`
	Height int             `yaml:"height"`
}

type flowRenderDoc struct {
	ExpectedCaseCount          int              `yaml:"expectedCaseCount"`
	ExpectedViewportStateCount int              `yaml:"expectedViewportStateCount"`
	ExpectedViewportThemeCount int              `yaml:"expectedViewportThemeCount"`
	ExpectedViewportCaseCount  int              `yaml:"expectedViewportCaseCount"`
	ViewportWidth              int              `yaml:"viewportWidth"`
	ViewportHeight             int              `yaml:"viewportHeight"`
	Cases                      []flowRenderCase `yaml:"cases"`
}

func decodeFlowRenderDoc(data []byte) (flowRenderDoc, error) {
	var doc flowRenderDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/flow_render.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("flow_render.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != requiredFlowRenderCaseCount || len(doc.Cases) != requiredFlowRenderCaseCount {
		return doc, fmt.Errorf("flow render rows: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), requiredFlowRenderCaseCount)
	}
	if doc.ExpectedViewportStateCount != requiredFlowViewportStateCount ||
		doc.ExpectedViewportThemeCount != requiredFlowViewportThemeCount ||
		doc.ExpectedViewportCaseCount != requiredFlowViewportCaseCount ||
		doc.ViewportWidth != requiredFlowViewportWidth || doc.ViewportHeight != requiredFlowViewportHeight {
		return doc, fmt.Errorf(
			"flow viewport declarations: states=%d themes=%d cases=%d size=%dx%d, require %d/%d/%d/%dx%d",
			doc.ExpectedViewportStateCount, doc.ExpectedViewportThemeCount, doc.ExpectedViewportCaseCount,
			doc.ViewportWidth, doc.ViewportHeight, requiredFlowViewportStateCount,
			requiredFlowViewportThemeCount, requiredFlowViewportCaseCount,
			requiredFlowViewportWidth, requiredFlowViewportHeight)
	}

	names := map[string]bool{}
	pairs := map[flowRenderState]map[flowRenderTheme]int{}
	viewportCases := 0
	for _, row := range doc.Cases {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] {
			return doc, fmt.Errorf("flow render row has an empty or duplicate name %q", row.Name)
		}
		names[row.Name] = true
		if !row.State.valid() || !row.Theme.valid() {
			return doc, fmt.Errorf("flow render row %q has invalid state/theme %q/%q", row.Name, row.State, row.Theme)
		}
		if row.Width <= 0 || row.Height <= 0 {
			return doc, fmt.Errorf("flow render row %q has non-positive size %dx%d", row.Name, row.Width, row.Height)
		}
		if !row.State.viewportState() {
			continue
		}
		viewportCases++
		if row.Width != requiredFlowViewportWidth || row.Height != requiredFlowViewportHeight {
			return doc, fmt.Errorf("flow viewport row %q has size %dx%d, require %dx%d", row.Name,
				row.Width, row.Height, requiredFlowViewportWidth, requiredFlowViewportHeight)
		}
		if pairs[row.State] == nil {
			pairs[row.State] = map[flowRenderTheme]int{}
		}
		pairs[row.State][row.Theme]++
	}
	if viewportCases != requiredFlowViewportCaseCount || len(pairs) != requiredFlowViewportStateCount {
		return doc, fmt.Errorf("flow viewport rows/states=%d/%d, require %d/%d",
			viewportCases, len(pairs), requiredFlowViewportCaseCount, requiredFlowViewportStateCount)
	}
	if err := validateFlowViewportPair(pairs, flowRenderViewportStepTop); err != nil {
		return doc, err
	}
	if err := validateFlowViewportPair(pairs, flowRenderViewportStepBottom); err != nil {
		return doc, err
	}
	if err := validateFlowViewportPair(pairs, flowRenderViewportReceiptErrorBottom); err != nil {
		return doc, err
	}
	return doc, nil
}

func validateFlowViewportPair(pairs map[flowRenderState]map[flowRenderTheme]int, state flowRenderState) error {
	if pairs[state][flowRenderThemeDark] != 1 || pairs[state][flowRenderThemeLight] != 1 {
		return fmt.Errorf("flow viewport state %q requires exactly one dark and one light row; got dark=%d light=%d",
			state, pairs[state][flowRenderThemeDark], pairs[state][flowRenderThemeLight])
	}
	return nil
}

func loadFlowRenderDoc(t *testing.T) flowRenderDoc {
	t.Helper()
	doc, err := decodeFlowRenderDoc(flowRenderData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func themeFor(t *testing.T, name flowRenderTheme) theme.Theme {
	t.Helper()
	switch name {
	case flowRenderThemeDark:
		return theme.New(theme.ModeDark)
	case flowRenderThemeLight:
		return theme.New(theme.ModeLight)
	default:
		t.Fatalf("unknown theme %q", name)
		return theme.Theme{}
	}
}

// buildFlowForRender builds a deterministic flow driven into the requested
// state at the requested size.
func buildFlowForRender(t *testing.T, th theme.Theme, state flowRenderState, w, h int) Flow {
	t.Helper()
	d, err := NewDraft("/tmp/settings-golden/config.yaml", config.BaseConfig())
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	registry := testRegistry()
	var options []FlowOption
	if state == flowRenderViewportStepTop || state == flowRenderViewportStepBottom {
		registry = flowViewportStepRegistry(loadFlowViewportFixture(t))
	}
	if state == flowRenderViewportReceiptErrorBottom {
		document := loadFlowViewportFixture(t)
		registry = flowViewportReceiptRegistry(document)
		options = append(options, WithConsentSummary(func(ConsentContext) (ConsentSummary, error) {
			return ConsentSummary{Values: document.ReceiptValues, Effects: document.ReceiptEffects}, nil
		}))
	}
	f := NewFlow(th, registry, d, options...)
	f.SetSize(w, h)
	switch state {
	case flowRenderStep:
		// initial connection step
	case flowRenderReceipt:
		f = send(f, "space") // a change to summarize
		f = send(f, "tab")   // into advanced
		f = send(f, "tab")   // to receipt
	case flowRenderConfirm:
		f = send(f, "esc") // open exit-confirm overlay
	case flowRenderHelp:
		f = send(f, "?") // open the grouped keybinding help overlay
	case flowRenderViewportStepTop:
		// overflowing guided step at its initial viewport offset
	case flowRenderViewportStepBottom:
		f = send(f, "shift+g")
	case flowRenderViewportReceiptErrorBottom:
		f = send(f, "tab", "enter", "shift+g")
	default:
		t.Fatalf("unknown state %q", state)
	}
	return f
}

func mutateFlowRenderBytes(t *testing.T, data []byte, old, replacement string) []byte {
	t.Helper()
	mutated := bytes.Replace(data, []byte(old), []byte(replacement), 1)
	if bytes.Equal(mutated, data) {
		t.Fatalf("flow render mutation did not find %q", old)
	}
	return mutated
}

func mutateFlowRenderFixture(t *testing.T, old, replacement string) []byte {
	t.Helper()
	return mutateFlowRenderBytes(t, flowRenderData, old, replacement)
}

func TestFlowRenderFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), flowRenderData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted an unknown field")
	}
}

func TestFlowRenderFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), flowRenderData...), []byte("\n---\n{}\n")...)
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted a trailing document")
	}
}

func TestFlowRenderFixtureRejectsCoordinatedExactCountMutation(t *testing.T) {
	mutated := mutateFlowRenderFixture(t,
		"expectedCaseCount: 20", "expectedCaseCount: 19")
	mutated = mutateFlowRenderBytes(t, mutated,
		"  - {name: step-light-40x8, state: step, theme: light, width: 40, height: 8}\n", "")
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted a row removal coordinated with its YAML count")
	}
}

func TestFlowRenderFixtureRejectsMissingViewportThemePair(t *testing.T) {
	mutated := mutateFlowRenderFixture(t,
		"expectedCaseCount: 20", "expectedCaseCount: 19")
	mutated = mutateFlowRenderBytes(t, mutated, "expectedViewportCaseCount: 6", "expectedViewportCaseCount: 5")
	mutated = mutateFlowRenderBytes(t, mutated,
		"  - {name: viewport-step-top-light-80x24, state: viewport-step-top, theme: light, width: 80, height: 24}\n", "")
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted a missing light viewport row with coordinated declarations")
	}
}

func TestFlowRenderFixtureRejectsDuplicateName(t *testing.T) {
	mutated := mutateFlowRenderFixture(t, "name: receipt-light-80x10", "name: receipt-dark-80x10")
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted a duplicate row name")
	}
}

func TestFlowRenderFixtureRejectsDuplicateViewportPair(t *testing.T) {
	mutated := mutateFlowRenderFixture(t,
		"name: viewport-step-top-light-80x24, state: viewport-step-top, theme: light",
		"name: viewport-step-top-light-80x24, state: viewport-step-top, theme: dark")
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted a duplicate dark viewport pair")
	}
}

func TestFlowRenderFixtureRejectsInvalidState(t *testing.T) {
	mutated := mutateFlowRenderFixture(t, "state: step, theme: dark", "state: unknown, theme: dark")
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted an invalid state")
	}
}

func TestFlowRenderFixtureRejectsInvalidTheme(t *testing.T) {
	mutated := mutateFlowRenderFixture(t, "state: step, theme: dark", "state: step, theme: sepia")
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted an invalid theme")
	}
}

func TestFlowRenderFixtureRejectsNonPositiveDimensions(t *testing.T) {
	mutated := mutateFlowRenderFixture(t,
		"name: step-dark-80x10, state: step, theme: dark, width: 80, height: 10",
		"name: step-dark-80x10, state: step, theme: dark, width: 0, height: 10")
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted a non-positive width")
	}
}

func TestFlowRenderFixtureRejectsWrongViewportDimensions(t *testing.T) {
	mutated := mutateFlowRenderFixture(t,
		"name: viewport-step-top-dark-80x24, state: viewport-step-top, theme: dark, width: 80, height: 24",
		"name: viewport-step-top-dark-80x23, state: viewport-step-top, theme: dark, width: 80, height: 23")
	if _, err := decodeFlowRenderDoc(mutated); err == nil {
		t.Fatal("flow render fixture accepted a viewport row outside the required 80x24 dimensions")
	}
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
