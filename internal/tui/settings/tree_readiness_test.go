package settings

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	expectedTreeReadinessCaseCount          = 7
	expectedTreeReadinessNodeCount          = 4
	expectedTreeReadinessMutationProbeCount = 4
)

//go:embed testdata/tree_readiness.yaml
var treeReadinessData []byte

type readinessNodeState string

const (
	readinessNodeUnchecked readinessNodeState = "unchecked"
	readinessNodePartial   readinessNodeState = "partial"
	readinessNodeChecked   readinessNodeState = "checked"
)

func (s readinessNodeState) valid() bool {
	switch s {
	case readinessNodeUnchecked, readinessNodePartial, readinessNodeChecked:
		return true
	default:
		return false
	}
}

type readinessSourceResult string

const (
	readinessSourceForest readinessSourceResult = "forest"
	readinessSourceEmpty  readinessSourceResult = "empty"
	readinessSourceError  readinessSourceResult = "error"
)

func (r readinessSourceResult) valid() bool {
	switch r {
	case readinessSourceForest, readinessSourceEmpty, readinessSourceError:
		return true
	default:
		return false
	}
}

type readinessDelivery string

const (
	readinessSpinnerBeforeResult readinessDelivery = "spinner-before-result"
	readinessForeignBeforeResult readinessDelivery = "foreign-before-result"
	readinessResultBeforeSpinner readinessDelivery = "result-before-spinner"
	readinessResultOnly          readinessDelivery = "result-only"
	readinessHoldResult          readinessDelivery = "hold-result"
)

func (d readinessDelivery) valid() bool {
	switch d {
	case readinessSpinnerBeforeResult, readinessForeignBeforeResult,
		readinessResultBeforeSpinner, readinessResultOnly, readinessHoldResult:
		return true
	default:
		return false
	}
}

type readinessValidation string

const (
	readinessValidationReady   readinessValidation = "ready"
	readinessValidationLoading readinessValidation = "loading"
	readinessValidationFailed  readinessValidation = "failed"
)

func (v readinessValidation) valid() bool {
	switch v {
	case readinessValidationReady, readinessValidationLoading, readinessValidationFailed:
		return true
	default:
		return false
	}
}

type readinessNode struct {
	ID       string             `yaml:"id"`
	Label    string             `yaml:"label"`
	State    readinessNodeState `yaml:"state"`
	Meta     map[string]string  `yaml:"meta"`
	Children []readinessNode    `yaml:"children"`
}

type readinessCase struct {
	Name               string                 `yaml:"name"`
	ConfigExists       bool                   `yaml:"configExists"`
	SourceResult       readinessSourceResult  `yaml:"sourceResult"`
	Delivery           readinessDelivery      `yaml:"delivery"`
	InitialSelection   config.SelectionConfig `yaml:"initialSelection"`
	ExpectedSelection  config.SelectionConfig `yaml:"expectedSelection"`
	ExpectedValidation readinessValidation    `yaml:"expectedValidation"`
	AttemptCommit      bool                   `yaml:"attemptCommit"`
	MutationProbe      bool                   `yaml:"mutationProbe"`
	WantErrorContains  []string               `yaml:"wantErrorContains"`
}

type readinessDocument struct {
	ExpectedCaseCount          int             `yaml:"expectedCaseCount"`
	ExpectedNodeCount          int             `yaml:"expectedNodeCount"`
	ExpectedMutationProbeCount int             `yaml:"expectedMutationProbeCount"`
	Width                      int             `yaml:"width"`
	Height                     int             `yaml:"height"`
	SourceError                string          `yaml:"sourceError"`
	Forest                     []readinessNode `yaml:"forest"`
	Cases                      []readinessCase `yaml:"cases"`
}

func decodeTreeReadiness(data []byte) (readinessDocument, error) {
	var doc readinessDocument
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/tree_readiness.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("found a second YAML document")
		}
		return doc, fmt.Errorf("tree_readiness.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != expectedTreeReadinessCaseCount || len(doc.Cases) != expectedTreeReadinessCaseCount {
		return doc, fmt.Errorf("tree_readiness.yaml cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedTreeReadinessCaseCount)
	}
	if doc.ExpectedNodeCount != expectedTreeReadinessNodeCount || readinessNodeCount(doc.Forest) != expectedTreeReadinessNodeCount {
		return doc, fmt.Errorf("tree_readiness.yaml nodes: declared=%d actual=%d required=%d",
			doc.ExpectedNodeCount, readinessNodeCount(doc.Forest), expectedTreeReadinessNodeCount)
	}
	if doc.ExpectedMutationProbeCount != expectedTreeReadinessMutationProbeCount {
		return doc, fmt.Errorf("tree_readiness.yaml mutation probes: declared=%d required=%d",
			doc.ExpectedMutationProbeCount, expectedTreeReadinessMutationProbeCount)
	}
	if doc.Width <= 0 || doc.Height <= 0 || doc.SourceError == "" {
		return doc, errors.New("tree_readiness.yaml must declare a positive region and source error")
	}
	seenNodes := map[string]bool{}
	if err := validateReadinessNodes(doc.Forest, seenNodes); err != nil {
		return doc, err
	}
	seenCases := map[string]bool{}
	mutationProbes := 0
	for _, c := range doc.Cases {
		if c.Name == "" || seenCases[c.Name] || !c.SourceResult.valid() || !c.Delivery.valid() ||
			!c.InitialSelection.Mode.IsValid() || !c.ExpectedSelection.Mode.IsValid() || !c.ExpectedValidation.valid() {
			return doc, fmt.Errorf("tree_readiness.yaml contains an invalid or duplicate case: %#v", c)
		}
		seenCases[c.Name] = true
		if c.ExpectedValidation != readinessValidationReady && (!c.AttemptCommit || len(c.WantErrorContains) == 0) {
			return doc, fmt.Errorf("tree_readiness.yaml non-ready case %q must attempt commit and assert its error", c.Name)
		}
		if c.MutationProbe {
			mutationProbes++
		}
	}
	if mutationProbes != expectedTreeReadinessMutationProbeCount {
		return doc, fmt.Errorf("tree_readiness.yaml mutation probes: actual=%d required=%d",
			mutationProbes, expectedTreeReadinessMutationProbeCount)
	}
	return doc, nil
}

func readinessNodeCount(nodes []readinessNode) int {
	count := 0
	for _, node := range nodes {
		count += 1 + readinessNodeCount(node.Children)
	}
	return count
}

func validateReadinessNodes(nodes []readinessNode, seen map[string]bool) error {
	for _, node := range nodes {
		if node.ID == "" || node.Label == "" || !node.State.valid() || seen[node.ID] {
			return fmt.Errorf("tree_readiness.yaml contains an invalid or duplicate node: %#v", node)
		}
		seen[node.ID] = true
		if err := validateReadinessNodes(node.Children, seen); err != nil {
			return err
		}
	}
	return nil
}

func loadTreeReadiness(t *testing.T) readinessDocument {
	t.Helper()
	doc, err := decodeTreeReadiness(treeReadinessData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

type readinessSource struct {
	result readinessSourceResult
	roots  []*kit.TreeNode
	err    error
	calls  int
}

var _ kit.TreeSource = (*readinessSource)(nil)

func (s *readinessSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch s.result {
	case readinessSourceForest:
		return s.roots, nil
	case readinessSourceEmpty:
		return nil, nil
	case readinessSourceError:
		return nil, s.err
	default:
		return nil, fmt.Errorf("unsupported readiness source result %q", s.result)
	}
}

func readinessForest(t *testing.T, nodes []readinessNode) []*kit.TreeNode {
	t.Helper()
	forest := make([]*kit.TreeNode, 0, len(nodes))
	for _, node := range nodes {
		forest = append(forest, readinessTreeNode(t, node))
	}
	return forest
}

func readinessTreeNode(t *testing.T, node readinessNode) *kit.TreeNode {
	t.Helper()
	state := kit.Unchecked
	switch node.State {
	case readinessNodeUnchecked:
		state = kit.Unchecked
	case readinessNodePartial:
		state = kit.Partial
	case readinessNodeChecked:
		state = kit.Checked
	default:
		t.Fatalf("unsupported readiness node state %q", node.State)
	}
	children := make([]*kit.TreeNode, 0, len(node.Children))
	for _, child := range node.Children {
		children = append(children, readinessTreeNode(t, child))
	}
	return &kit.TreeNode{ID: node.ID, Label: node.Label, State: state, Meta: node.Meta, Children: children}
}

func readinessDraft(t *testing.T, c readinessCase) (*Draft, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.BaseConfig()
	cfg.Selection = c.InitialSelection
	loaded := cfg
	var before []byte
	if c.ConfigExists {
		if err := config.SaveAtomic(path, cfg); err != nil {
			t.Fatalf("seed readiness config: %v", err)
		}
		var err error
		before, err = os.ReadFile(path)
		if err != nil {
			t.Fatalf("read readiness config: %v", err)
		}
		loaded, err = config.Parse(before)
		if err != nil {
			t.Fatalf("parse readiness config: %v", err)
		}
	}
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open readiness draft: %v", err)
	}
	return draft, path, before
}

func readinessFlow(t *testing.T, doc readinessDocument, c readinessCase) (Flow, *Draft, *treeField, *readinessSource, tea.Cmd, tea.Cmd, string, []byte) {
	t.Helper()
	draft, path, before := readinessDraft(t, c)
	source := &readinessSource{
		result: c.SourceResult,
		roots:  readinessForest(t, doc.Forest),
		err:    errors.New(doc.SourceError),
	}
	field := Tree("selection", "transcripts", selectionAccessor(), source, WithDraftSelectionState()).(*treeField)
	registry := Registry{Sections: []Section{{Key: "selection", Title: "select transcripts", Fields: []Field{field}}}}
	flow := NewFlow(theme.New(theme.ModeDark), registry, draft)
	flow.SetSize(doc.Width, doc.Height)
	initCmd := flow.Init()
	if initCmd == nil {
		t.Fatal("mounted readiness flow returned no init command")
	}
	initMsg := initCmd()
	batch, ok := initMsg.(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("mounted readiness flow init produced %T with %d commands, want a two-command load/spinner batch", initMsg, len(batch))
	}
	return flow, draft, field, source, batch[0], batch[1], path, before
}

func selectionsMatchConfig(a, b config.SelectionConfig) bool {
	return a.AutoIngestNewBranches == b.AutoIngestNewBranches && selectionsEqual(
		TreeSelection{Mode: a.Mode, Harnesses: a.Harnesses},
		TreeSelection{Mode: b.Mode, Harnesses: b.Harnesses},
	)
}

func requireReadinessSelection(t *testing.T, stage string, got, want config.SelectionConfig) {
	t.Helper()
	if !selectionsMatchConfig(got, want) {
		t.Fatalf("%s selection = %#v, want %#v", stage, got, want)
	}
}

func readinessConfigBytes(t *testing.T, cfg *config.Config) []byte {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal readiness config: %v", err)
	}
	return data
}

func deliverReadinessCase(t *testing.T, c readinessCase, flow Flow, draft *Draft, field *treeField, loadCmd, spinnerCmd tea.Cmd) Flow {
	t.Helper()
	workingBefore := readinessConfigBytes(t, draft.Working())
	deliver := func(msg tea.Msg) {
		var cmd tea.Cmd
		flow, cmd = flow.Update(msg)
		_ = cmd
	}
	requireInitial := func(stage string) {
		requireReadinessSelection(t, stage, draft.Working().Selection, c.InitialSelection)
		if got := readinessConfigBytes(t, draft.Working()); !bytes.Equal(got, workingBefore) {
			t.Fatalf("%s changed the buffered config before a successful result", stage)
		}
		if err := field.Validate(draft); err == nil || !strings.Contains(err.Error(), "not ready") {
			t.Fatalf("%s validation = %v, want an actionable not-ready error", stage, err)
		}
	}
	switch c.Delivery {
	case readinessSpinnerBeforeResult:
		deliver(spinnerCmd())
		requireInitial("spinner before result")
		deliver(loadCmd())
	case readinessForeignBeforeResult:
		deliver(tea.WindowSizeMsg{Width: 111, Height: 29})
		requireInitial("foreign message before result")
		deliver(loadCmd())
	case readinessResultBeforeSpinner:
		deliver(loadCmd())
		afterResult := draft.Working().Selection
		deliver(spinnerCmd())
		requireReadinessSelection(t, "late spinner", draft.Working().Selection, afterResult)
	case readinessResultOnly:
		deliver(loadCmd())
	case readinessHoldResult:
		// Keep both commands pending to exercise commit validation while loading.
	default:
		t.Fatalf("unsupported readiness delivery %q", c.Delivery)
	}
	return flow
}

func TestTreeFieldReadinessPreservesDraftAcrossAsyncOrdering(t *testing.T) {
	doc := loadTreeReadiness(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			flow, draft, field, source, loadCmd, spinnerCmd, path, before := readinessFlow(t, doc, c)
			workingBefore := readinessConfigBytes(t, draft.Working())
			flow = deliverReadinessCase(t, c, flow, draft, field, loadCmd, spinnerCmd)

			expectedCalls := 1
			if c.Delivery == readinessHoldResult {
				expectedCalls = 0
			}
			if source.calls != expectedCalls {
				t.Fatalf("source calls = %d, want %d", source.calls, expectedCalls)
			}
			requireReadinessSelection(t, "final", draft.Working().Selection, c.ExpectedSelection)
			preservesWorking := selectionsMatchConfig(c.InitialSelection, c.ExpectedSelection)
			if preservesWorking {
				if got := readinessConfigBytes(t, draft.Working()); !bytes.Equal(got, workingBefore) {
					t.Fatal("readiness handling changed buffered config bytes in a preservation case")
				}
			}

			validationErr := field.Validate(draft)
			switch c.ExpectedValidation {
			case readinessValidationReady:
				if validationErr != nil {
					t.Fatalf("ready field did not validate: %v", validationErr)
				}
			case readinessValidationLoading, readinessValidationFailed:
				if validationErr == nil {
					t.Fatal("non-ready field unexpectedly validated")
				}
				for _, want := range c.WantErrorContains {
					if !strings.Contains(validationErr.Error(), want) {
						t.Errorf("validation error does not contain %q: %v", want, validationErr)
					}
				}
			default:
				t.Fatalf("unsupported expected validation %q", c.ExpectedValidation)
			}
			requireReadinessSelection(t, "after validation", draft.Working().Selection, c.ExpectedSelection)
			if preservesWorking {
				if got := readinessConfigBytes(t, draft.Working()); !bytes.Equal(got, workingBefore) {
					t.Fatal("validation changed buffered config bytes in a preservation case")
				}
			}

			if c.AttemptCommit {
				flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
				if !flow.OnReceipt() {
					t.Fatal("readiness flow did not reach receipt")
				}
				flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				if flow.Committed() {
					t.Fatal("non-ready selection committed")
				}
				if flow.Err() == nil {
					t.Fatal("blocked readiness commit exposed no actionable error")
				}
				requireReadinessSelection(t, "blocked commit", draft.Working().Selection, c.ExpectedSelection)
				if got := readinessConfigBytes(t, draft.Working()); !bytes.Equal(got, workingBefore) {
					t.Fatal("blocked readiness commit changed buffered config bytes")
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read config after blocked commit: %v", err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("blocked readiness commit changed config bytes")
				}
			}
		})
	}
}

func TestTreeReadinessFixtureMutationProbesPrematureEmptyDerivation(t *testing.T) {
	doc := loadTreeReadiness(t)
	probes := 0
	for _, c := range doc.Cases {
		if !c.MutationProbe {
			continue
		}
		probes++
		erased := FromTreeNodes(nil).ToSelectionConfig(c.InitialSelection.AutoIngestNewBranches)
		if selectionsMatchConfig(c.InitialSelection, erased) {
			t.Fatalf("mutation probe %q would not catch premature empty-forest derivation", c.Name)
		}
	}
	if probes != expectedTreeReadinessMutationProbeCount {
		t.Fatalf("mutation probes = %d, want %d", probes, expectedTreeReadinessMutationProbeCount)
	}
}

func TestTreeReadinessFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), treeReadinessData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeTreeReadiness(mutated); err == nil {
		t.Fatal("tree readiness fixture accepted an unknown field")
	}
}

func TestTreeReadinessFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), treeReadinessData...), []byte("\n---\n{}\n")...)
	if _, err := decodeTreeReadiness(mutated); err == nil {
		t.Fatal("tree readiness fixture accepted a trailing document")
	}
}

func TestTreeReadinessFixtureEnforcesExactCaseCount(t *testing.T) {
	mutated := bytes.Replace(treeReadinessData, []byte("expectedCaseCount: 7"), []byte("expectedCaseCount: 8"), 1)
	if _, err := decodeTreeReadiness(mutated); err == nil {
		t.Fatal("tree readiness fixture accepted a changed declared case count")
	}
}

func TestTreeReadinessFixtureEnforcesExactNodeCount(t *testing.T) {
	mutated := bytes.Replace(treeReadinessData, []byte("expectedNodeCount: 4"), []byte("expectedNodeCount: 5"), 1)
	if _, err := decodeTreeReadiness(mutated); err == nil {
		t.Fatal("tree readiness fixture accepted a changed declared node count")
	}
}

func TestTreeReadinessFixtureEnforcesExactMutationProbeCount(t *testing.T) {
	mutated := bytes.Replace(treeReadinessData, []byte("expectedMutationProbeCount: 4"), []byte("expectedMutationProbeCount: 5"), 1)
	if _, err := decodeTreeReadiness(mutated); err == nil {
		t.Fatal("tree readiness fixture accepted a changed declared mutation-probe count")
	}
}
