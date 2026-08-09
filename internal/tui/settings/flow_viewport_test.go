package settings

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/redact"
)

const (
	expectedFlowViewportExampleLines    = 16
	expectedFlowViewportReceiptValues   = 14
	expectedFlowViewportReceiptEffects  = 4
	expectedFlowViewportErrorLines      = 6
	expectedFlowViewportCases           = 8
	expectedFlowViewportLegacyMutations = 2
)

type flowViewportFixtureState string

const (
	flowViewportStep         flowViewportFixtureState = "step"
	flowViewportReceiptError flowViewportFixtureState = "receipt-error"
)

func (s flowViewportFixtureState) valid() bool {
	return s == flowViewportStep || s == flowViewportReceiptError
}

type flowViewportFixtureKey string

const (
	flowViewportKeyPageUp   flowViewportFixtureKey = "page-up"
	flowViewportKeyPageDown flowViewportFixtureKey = "page-down"
	flowViewportKeyTop      flowViewportFixtureKey = "top"
	flowViewportKeyBottom   flowViewportFixtureKey = "bottom"
	flowViewportKeyDown     flowViewportFixtureKey = "down"
)

func (k flowViewportFixtureKey) valid() bool {
	switch k {
	case flowViewportKeyPageUp, flowViewportKeyPageDown, flowViewportKeyTop, flowViewportKeyBottom, flowViewportKeyDown:
		return true
	default:
		return false
	}
}

type flowViewportFixtureAction string

const (
	flowViewportActionPageUp   flowViewportFixtureAction = "page-up"
	flowViewportActionPageDown flowViewportFixtureAction = "page-down"
	flowViewportActionTop      flowViewportFixtureAction = "top"
	flowViewportActionBottom   flowViewportFixtureAction = "bottom"
)

func (a flowViewportFixtureAction) actionID() (keymap.ActionID, bool) {
	switch a {
	case flowViewportActionPageUp:
		return keymap.ActionPageUp, true
	case flowViewportActionPageDown:
		return keymap.ActionPageDown, true
	case flowViewportActionTop:
		return keymap.ActionTop, true
	case flowViewportActionBottom:
		return keymap.ActionBottom, true
	default:
		return keymap.ActionUnknown, false
	}
}

type flowViewportFixtureCase struct {
	Name                 string                      `yaml:"name"`
	State                flowViewportFixtureState    `yaml:"state"`
	Keys                 []flowViewportFixtureKey    `yaml:"keys"`
	AssertDownFieldOwned bool                        `yaml:"assertDownFieldOwned"`
	WantContains         []string                    `yaml:"wantContains"`
	WantMissing          []string                    `yaml:"wantMissing"`
	WantActions          []flowViewportFixtureAction `yaml:"wantActions"`
	WantMissingActions   []flowViewportFixtureAction `yaml:"wantMissingActions"`
	WantScrolled         bool                        `yaml:"wantScrolled"`
	LegacyMutationProbe  bool                        `yaml:"legacyMutationProbe"`
	MutationReveal       string                      `yaml:"mutationReveal"`
}

type flowViewportFixtureDocument struct {
	ExpectedExampleLineCount    int                       `yaml:"expectedExampleLineCount"`
	ExpectedReceiptValueCount   int                       `yaml:"expectedReceiptValueCount"`
	ExpectedReceiptEffectCount  int                       `yaml:"expectedReceiptEffectCount"`
	ExpectedErrorLineCount      int                       `yaml:"expectedErrorLineCount"`
	ExpectedCaseCount           int                       `yaml:"expectedCaseCount"`
	ExpectedLegacyMutationCount int                       `yaml:"expectedLegacyMutationCount"`
	ExampleLines                []string                  `yaml:"exampleLines"`
	ReceiptValues               []string                  `yaml:"receiptValues"`
	ReceiptEffects              []string                  `yaml:"receiptEffects"`
	ErrorLines                  []string                  `yaml:"errorLines"`
	Cases                       []flowViewportFixtureCase `yaml:"cases"`
}

//go:embed testdata/flow_viewport.yaml
var flowViewportFixtureData []byte

func decodeFlowViewportFixture(data []byte) (flowViewportFixtureDocument, error) {
	var document flowViewportFixtureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/flow_viewport.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("found a second YAML document")
		}
		return document, fmt.Errorf("flow_viewport.yaml must hold exactly one document: %w", err)
	}
	if document.ExpectedExampleLineCount != expectedFlowViewportExampleLines || len(document.ExampleLines) != expectedFlowViewportExampleLines {
		return document, fmt.Errorf("flow viewport example lines: declared=%d actual=%d required=%d",
			document.ExpectedExampleLineCount, len(document.ExampleLines), expectedFlowViewportExampleLines)
	}
	if document.ExpectedReceiptValueCount != expectedFlowViewportReceiptValues || len(document.ReceiptValues) != expectedFlowViewportReceiptValues {
		return document, fmt.Errorf("flow viewport receipt values: declared=%d actual=%d required=%d",
			document.ExpectedReceiptValueCount, len(document.ReceiptValues), expectedFlowViewportReceiptValues)
	}
	if document.ExpectedReceiptEffectCount != expectedFlowViewportReceiptEffects || len(document.ReceiptEffects) != expectedFlowViewportReceiptEffects {
		return document, fmt.Errorf("flow viewport receipt effects: declared=%d actual=%d required=%d",
			document.ExpectedReceiptEffectCount, len(document.ReceiptEffects), expectedFlowViewportReceiptEffects)
	}
	if document.ExpectedErrorLineCount != expectedFlowViewportErrorLines || len(document.ErrorLines) != expectedFlowViewportErrorLines {
		return document, fmt.Errorf("flow viewport error lines: declared=%d actual=%d required=%d",
			document.ExpectedErrorLineCount, len(document.ErrorLines), expectedFlowViewportErrorLines)
	}
	if document.ExpectedCaseCount != expectedFlowViewportCases || len(document.Cases) != expectedFlowViewportCases {
		return document, fmt.Errorf("flow viewport cases: declared=%d actual=%d required=%d",
			document.ExpectedCaseCount, len(document.Cases), expectedFlowViewportCases)
	}
	if !nonEmptyFlowViewportStrings(document.ExampleLines, document.ReceiptValues, document.ReceiptEffects, document.ErrorLines) {
		return document, errors.New("flow viewport fixture has an empty example, receipt, or error line")
	}

	names := make(map[string]bool, len(document.Cases))
	mutations := 0
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] || !row.State.valid() ||
			len(row.WantContains) == 0 || len(row.WantActions) == 0 || len(row.WantMissingActions) == 0 ||
			!nonEmptyFlowViewportStrings(row.WantContains, row.WantMissing) {
			return document, fmt.Errorf("flow viewport case is incomplete or duplicated: %#v", row)
		}
		names[row.Name] = true
		for _, fixtureKey := range row.Keys {
			if !fixtureKey.valid() {
				return document, fmt.Errorf("flow viewport case %q has invalid key %q", row.Name, fixtureKey)
			}
		}
		for _, action := range append(append([]flowViewportFixtureAction(nil), row.WantActions...), row.WantMissingActions...) {
			if _, ok := action.actionID(); !ok {
				return document, fmt.Errorf("flow viewport case %q has invalid action %q", row.Name, action)
			}
		}
		if row.AssertDownFieldOwned && (len(row.Keys) == 0 || row.Keys[0] != flowViewportKeyDown || row.State != flowViewportStep) {
			return document, fmt.Errorf("flow viewport case %q cannot prove down-field ownership", row.Name)
		}
		if row.LegacyMutationProbe {
			mutations++
			if strings.TrimSpace(row.MutationReveal) == "" || !row.WantScrolled {
				return document, fmt.Errorf("flow viewport mutation %q has no scrolled reveal", row.Name)
			}
		}
	}
	if document.ExpectedLegacyMutationCount != expectedFlowViewportLegacyMutations || mutations != expectedFlowViewportLegacyMutations {
		return document, fmt.Errorf("flow viewport mutations: declared=%d actual=%d required=%d",
			document.ExpectedLegacyMutationCount, mutations, expectedFlowViewportLegacyMutations)
	}
	return document, nil
}

func nonEmptyFlowViewportStrings(groups ...[]string) bool {
	for _, group := range groups {
		for _, value := range group {
			if strings.TrimSpace(value) == "" {
				return false
			}
		}
	}
	return true
}

func loadFlowViewportFixture(t *testing.T) flowViewportFixtureDocument {
	t.Helper()
	document, err := decodeFlowViewportFixture(flowViewportFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func flowViewportRedactionAccessor() Accessor[redact.RedactionLevel] {
	return Accessor[redact.RedactionLevel]{
		Get: func(c *config.Config) redact.RedactionLevel { return c.Redaction.Level },
		Set: func(c *config.Config, value redact.RedactionLevel) { c.Redaction.Level = value },
	}
}

func flowViewportStepRegistry(document flowViewportFixtureDocument) Registry {
	return Registry{Sections: []Section{{
		Key:   "overflowing-guidance",
		Title: "overflowing guidance",
		Guide: &Guide{
			Intro: "inspect the complete derived example before choosing a level.",
			Hints: []string{"the current choice remains buffered", "paging moves prose, not the choice cursor"},
			Example: func(theme.Theme, *Draft) (string, error) {
				return strings.Join(document.ExampleLines, "\n"), nil
			},
		},
		Fields: []Field{WithDescription(
			Radio("redaction", "redaction level", flowViewportRedactionAccessor(),
				Option[redact.RedactionLevel]{Label: "minimal", Value: redact.Minimal, Description: "removes minimal material"},
				Option[redact.RedactionLevel]{Label: "standard", Value: redact.Standard, Description: "removes standard material"},
				Option[redact.RedactionLevel]{Label: "maximum", Value: redact.Maximum, Description: "removes standard material plus code identifiers"}),
			"choose one redaction level without moving the surrounding prose"),
		},
	}}}
}

type failingFlowViewportField struct {
	Field
	err error
}

func (f failingFlowViewportField) Validate(*Draft) error { return f.err }

func flowViewportReceiptRegistry(document flowViewportFixtureDocument) Registry {
	field := failingFlowViewportField{
		Field: Info("failing-boundary", func(*Draft) string { return "fixture validation remains mounted" }),
		err:   errors.New(strings.Join(document.ErrorLines, "\n")),
	}
	return Registry{Sections: []Section{{
		Key: "failing-receipt", Title: "failing receipt", Fields: []Field{field},
	}}}
}

func mountFlowViewportCase(t *testing.T, document flowViewportFixtureDocument, state flowViewportFixtureState) (Flow, *Draft) {
	t.Helper()
	path, loaded := writeConfigFile(t)
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open flow viewport draft: %v", err)
	}
	var flow Flow
	switch state {
	case flowViewportStep:
		flow = NewFlow(theme.New(theme.ModeDark), flowViewportStepRegistry(document), draft)
	case flowViewportReceiptError:
		flow = NewFlow(theme.New(theme.ModeDark), flowViewportReceiptRegistry(document), draft,
			WithConsentSummary(func(ConsentContext) (ConsentSummary, error) {
				return ConsentSummary{Values: document.ReceiptValues, Effects: document.ReceiptEffects}, nil
			}))
	default:
		t.Fatalf("unsupported flow viewport state %q", state)
	}
	flow.SetSize(80, 24)
	if state == flowViewportReceiptError {
		flow = send(flow, "tab", "enter")
		if !flow.OnReceipt() || flow.Err() == nil || flow.Committed() {
			t.Fatalf("failing viewport receipt state receipt/error/committed=%t/%v/%t",
				flow.OnReceipt(), flow.Err(), flow.Committed())
		}
	}
	return flow, draft
}

func driveFlowViewportKey(flow Flow, fixtureKey flowViewportFixtureKey) Flow {
	switch fixtureKey {
	case flowViewportKeyPageUp:
		return send(flow, "pgup")
	case flowViewportKeyPageDown:
		return send(flow, "pgdown")
	case flowViewportKeyTop:
		return send(flow, "g")
	case flowViewportKeyBottom:
		return send(flow, "shift+g")
	case flowViewportKeyDown:
		return send(flow, "down")
	default:
		return flow
	}
}

func TestFlowViewportKeepsGuidanceAndReceiptRecoveryReachable(t *testing.T) {
	document := loadFlowViewportFixture(t)
	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			flow, draft := mountFlowViewportCase(t, document, row.State)
			layout := kit.NewFrame(theme.New(theme.ModeDark)).WithTitle(flow.title()).WithFooter(" ")
			layout.SetSize(80, 24)
			raw := stripANSIForSettings(flow.renderBody(layout.InnerWidth(), layout.InnerHeight()))
			before := stripANSIForSettings(flow.View())
			if row.LegacyMutationProbe {
				if lines := strings.Count(raw, "\n") + 1; lines <= layout.InnerHeight() {
					t.Fatalf("flow viewport mutation %q is vacuous: raw body lines=%d inner height=%d", row.Name, lines, layout.InnerHeight())
				}
				if strings.Contains(before, row.MutationReveal) {
					t.Fatalf("flow viewport mutation %q reveal %q was already visible before paging:\n%s", row.Name, row.MutationReveal, before)
				}
			}

			for index, fixtureKey := range row.Keys {
				beforeOffset := flow.viewOffset
				flow = driveFlowViewportKey(flow, fixtureKey)
				if row.AssertDownFieldOwned && index == 0 {
					if flow.viewOffset != beforeOffset || draft.Working().Redaction.Level != redact.Standard {
						t.Fatalf("down key moved viewport or selected a value: offset=%d->%d level=%s",
							beforeOffset, flow.viewOffset, draft.Working().Redaction.Level)
					}
				}
			}

			plain := stripANSIForSettings(flow.View())
			for _, want := range row.WantContains {
				if !strings.Contains(plain, want) {
					t.Errorf("flow viewport does not contain %q:\n%s", want, plain)
				}
			}
			for _, missing := range row.WantMissing {
				if strings.Contains(plain, missing) {
					t.Errorf("flow viewport unexpectedly contains %q:\n%s", missing, plain)
				}
			}
			if (flow.viewOffset > 0) != row.WantScrolled {
				t.Errorf("flow viewport scrolled=%t offset=%d, want %t", flow.viewOffset > 0, flow.viewOffset, row.WantScrolled)
			}
			if lines := strings.Count(plain, "\n") + 1; lines > 24 {
				t.Errorf("flow viewport rendered %d lines at mounted height 24", lines)
			}
			available := actionSet(flow.availability().AvailableActions())
			assertFlowViewportActions(t, row.Name, available, row.WantActions, true)
			assertFlowViewportActions(t, row.Name, available, row.WantMissingActions, false)
			if row.State == flowViewportReceiptError && flow.Committed() {
				t.Fatal("paged failing receipt committed despite validation error")
			}
		})
	}
}

func assertFlowViewportActions(t *testing.T, name string, available map[keymap.ActionID]bool, actions []flowViewportFixtureAction, want bool) {
	t.Helper()
	for _, fixtureAction := range actions {
		action, ok := fixtureAction.actionID()
		if !ok {
			t.Fatalf("flow viewport case %q has unsupported action %q", name, fixtureAction)
		}
		if available[action] != want {
			t.Errorf("flow viewport case %q action %s available=%t, want %t", name, action, available[action], want)
		}
	}
}

func mutateFlowViewportCount(t *testing.T, field string, expected int) []byte {
	t.Helper()
	declared := []byte(fmt.Sprintf("%s: %d", field, expected))
	changed := []byte(fmt.Sprintf("%s: %d", field, expected+1))
	mutated := bytes.Replace(flowViewportFixtureData, declared, changed, 1)
	if bytes.Equal(mutated, flowViewportFixtureData) {
		t.Fatalf("flow viewport %s mutation did not alter the fixture", field)
	}
	return mutated
}

func TestFlowViewportFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), flowViewportFixtureData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeFlowViewportFixture(mutated); err == nil {
		t.Fatal("flow viewport fixture accepted an unknown field")
	}
}

func TestFlowViewportFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), flowViewportFixtureData...), []byte("\n---\n{}\n")...)
	if _, err := decodeFlowViewportFixture(mutated); err == nil {
		t.Fatal("flow viewport fixture accepted a trailing document")
	}
}

func TestFlowViewportFixturePinsCounts(t *testing.T) {
	assertFlowViewportCountMutationRejected(t, "expectedExampleLineCount", expectedFlowViewportExampleLines)
	assertFlowViewportCountMutationRejected(t, "expectedReceiptValueCount", expectedFlowViewportReceiptValues)
	assertFlowViewportCountMutationRejected(t, "expectedReceiptEffectCount", expectedFlowViewportReceiptEffects)
	assertFlowViewportCountMutationRejected(t, "expectedErrorLineCount", expectedFlowViewportErrorLines)
	assertFlowViewportCountMutationRejected(t, "expectedCaseCount", expectedFlowViewportCases)
	assertFlowViewportCountMutationRejected(t, "expectedLegacyMutationCount", expectedFlowViewportLegacyMutations)
}

func assertFlowViewportCountMutationRejected(t *testing.T, field string, expected int) {
	t.Helper()
	mutated := mutateFlowViewportCount(t, field, expected)
	if _, err := decodeFlowViewportFixture(mutated); err == nil {
		t.Fatalf("flow viewport fixture accepted changed %s", field)
	}
}
