package settings

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	expectedFlowAsyncCaseCount   = 1
	expectedFlowAsyncModalCount  = 1
	expectedFlowAsyncCommitCount = 1
)

type flowAsyncCase struct {
	Name            string `yaml:"name"`
	InitialText     string `yaml:"initialText"`
	SessionID       string `yaml:"sessionID"`
	ExpectLoadCount int    `yaml:"expectLoadCount"`
	ExpectReady     bool   `yaml:"expectReady"`
	ExpectCommit    bool   `yaml:"expectCommit"`
}

type flowAsyncDocument struct {
	ExpectedCaseCount   int             `yaml:"expectedCaseCount"`
	ExpectedModalCount  int             `yaml:"expectedModalCaseCount"`
	ExpectedCommitCount int             `yaml:"expectedCommitCount"`
	Cases               []flowAsyncCase `yaml:"cases"`
}

//go:embed testdata/flow_async_routing.yaml
var flowAsyncRoutingData []byte

func decodeFlowAsyncDocument(data []byte) (flowAsyncDocument, error) {
	var document flowAsyncDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/flow_async_routing.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("flow async routing fixture must hold exactly one document: %w", err)
	}
	if document.ExpectedCaseCount != expectedFlowAsyncCaseCount || len(document.Cases) != expectedFlowAsyncCaseCount ||
		document.ExpectedModalCount != expectedFlowAsyncModalCount || document.ExpectedCommitCount != expectedFlowAsyncCommitCount {
		return document, fmt.Errorf(
			"flow async routing counts: declared cases/modal/commit=%d/%d/%d actual cases=%d required=%d/%d/%d",
			document.ExpectedCaseCount, document.ExpectedModalCount, document.ExpectedCommitCount, len(document.Cases),
			expectedFlowAsyncCaseCount, expectedFlowAsyncModalCount, expectedFlowAsyncCommitCount)
	}
	seen := map[string]bool{}
	modalCount := 0
	commitCount := 0
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || seen[row.Name] || strings.TrimSpace(row.InitialText) == "" ||
			strings.TrimSpace(row.SessionID) == "" || row.ExpectLoadCount != 1 || !row.ExpectReady {
			return document, fmt.Errorf("flow async routing fixture contains an invalid or duplicate row: %#v", row)
		}
		seen[row.Name] = true
		modalCount++
		if row.ExpectCommit {
			commitCount++
		}
	}
	if modalCount != expectedFlowAsyncModalCount || commitCount != expectedFlowAsyncCommitCount {
		return document, fmt.Errorf("flow async routing actual modal/commit counts=%d/%d, want %d/%d",
			modalCount, commitCount, expectedFlowAsyncModalCount, expectedFlowAsyncCommitCount)
	}
	return document, nil
}

func loadFlowAsyncDocument(t *testing.T) flowAsyncDocument {
	t.Helper()
	document, err := decodeFlowAsyncDocument(flowAsyncRoutingData)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

type countedFlowTreeSource struct {
	id    string
	loads atomic.Int32
}

func (s *countedFlowTreeSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.loads.Add(1)
	return []*kit.TreeNode{{
		ID:    "example/project",
		Label: "example/project",
		Meta:  map[string]string{MetaRemote: "git@example.invalid:project.git"},
		Children: []*kit.TreeNode{{
			ID:    "main",
			Label: "main",
			Meta:  map[string]string{MetaBranch: "main"},
			Children: []*kit.TreeNode{{
				ID:    s.id,
				Label: "flow async fixture session",
				Meta:  map[string]string{MetaHarness: "fixture"},
			}},
		}},
	}}, nil
}

func flowAsyncTextAccessor() Accessor[string] {
	return Accessor[string]{
		Get: func(cfg *config.Config) string { return cfg.Village.URL },
		Set: func(cfg *config.Config, value string) { cfg.Village.URL = value },
	}
}

func flowAsyncWorkingBytes(t *testing.T, draft *Draft) []byte {
	t.Helper()
	data, err := yaml.Marshal(draft.Working())
	if err != nil {
		t.Fatalf("marshal flow async working draft: %v", err)
	}
	return data
}

func flowTreeContainsID(roots []*kit.TreeNode, id string) bool {
	for _, root := range roots {
		if root.ID == id || flowTreeContainsID(root.Children, id) {
			return true
		}
	}
	return false
}

func flowOwnedTreeResult(t *testing.T, command tea.Cmd, field *treeField) tea.Msg {
	t.Helper()
	var visit func(tea.Cmd) (tea.Msg, bool)
	visit = func(cmd tea.Cmd) (tea.Msg, bool) {
		if cmd == nil {
			return nil, false
		}
		message := cmd()
		if batch, ok := message.(tea.BatchMsg); ok {
			for _, child := range batch {
				if result, found := visit(child); found {
					return result, true
				}
			}
			return nil, false
		}
		if _, tick := message.(spinner.TickMsg); tick {
			return nil, false
		}
		return message, field.tree.OwnsAsync(message)
	}
	message, ok := visit(command)
	if !ok {
		t.Fatal("Flow.Init produced no owned tree result")
	}
	return message
}

func TestFlowRoutesOwnedAsyncBeforeExitConfirmationAndCommits(t *testing.T) {
	for _, row := range loadFlowAsyncDocument(t).Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			cfg := config.BaseConfig()
			cfg.Village.URL = row.InitialText
			if err := config.SaveAtomic(path, cfg); err != nil {
				t.Fatalf("seed flow async config: %v", err)
			}
			draft, err := NewDraft(path, cfg)
			if err != nil {
				t.Fatalf("open flow async draft: %v", err)
			}
			source := &countedFlowTreeSource{id: row.SessionID}
			field := Tree("selection", "transcripts", selectionAccessor(), source, WithDraftSelectionState()).(*treeField)
			registry := Registry{Sections: []Section{
				{Key: "selection", Title: "selection", Fields: []Field{field}},
				{Key: "connection", Title: "connection", Fields: []Field{
					Text("village-url", "village url", flowAsyncTextAccessor()),
				}},
			}}
			flow := NewFlow(theme.New(theme.ModeDark), registry, draft)
			flow.SetSize(100, 28)
			beforeWorking := flowAsyncWorkingBytes(t, draft)
			result := flowOwnedTreeResult(t, flow.Init(), field)

			flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			if !flow.Confirming() {
				t.Fatal("Flow did not open exit confirmation")
			}
			flow, _ = flow.Update(result)
			if !flow.Confirming() {
				t.Fatal("owned async delivery closed the exit confirmation")
			}
			if field.forestReady != row.ExpectReady || !flowTreeContainsID(field.tree.Roots(), row.SessionID) {
				t.Fatalf("tree readiness/contents=%t/%t, want ready session %q", field.forestReady,
					flowTreeContainsID(field.tree.Roots(), row.SessionID), row.SessionID)
			}
			if got := int(source.loads.Load()); got != row.ExpectLoadCount {
				t.Fatalf("source loads=%d, want %d", got, row.ExpectLoadCount)
			}
			if afterWorking := flowAsyncWorkingBytes(t, draft); !bytes.Equal(beforeWorking, afterWorking) {
				t.Fatalf("owned async delivery changed Draft bytes\nbefore=%s\nafter=%s", beforeWorking, afterWorking)
			}
			if got := draft.Working().Village.URL; got != row.InitialText {
				t.Fatalf("non-current text=%q, want byte-preserved %q", got, row.InitialText)
			}

			flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if flow.Confirming() || flow.Exited() {
				t.Fatalf("default-no modal result did not resume Flow: confirming=%t exited=%t", flow.Confirming(), flow.Exited())
			}
			flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			if !flow.OnReceipt() {
				t.Fatalf("ready Flow did not reach receipt; section=%q step=%d", flow.CurrentSectionKey(), flow.Step())
			}
			flow, command := flow.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if row.ExpectCommit && (!flow.Committed() || command == nil || flow.Err() != nil) {
				t.Fatalf("post-modal commit=%t commandNil=%t err=%v", flow.Committed(), command == nil, flow.Err())
			}
			committed, err := config.Parse(mustRead(t, path))
			if err != nil {
				t.Fatalf("parse flow async committed config: %v", err)
			}
			if committed.Village.URL != row.InitialText {
				t.Fatalf("committed text=%q, want %q", committed.Village.URL, row.InitialText)
			}
		})
	}
}

func TestFlowAsyncRoutingFixtureGuards(t *testing.T) {
	if _, err := decodeFlowAsyncDocument(append(append([]byte(nil), flowAsyncRoutingData...), []byte("\nunknownField: true\n")...)); err == nil {
		t.Fatal("flow async routing fixture accepted an unknown field")
	}
	if _, err := decodeFlowAsyncDocument(append(append([]byte(nil), flowAsyncRoutingData...), []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("flow async routing fixture accepted a trailing document")
	}
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedFlowAsyncCaseCount))
	changed := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedFlowAsyncCaseCount+1))
	mutated := bytes.Replace(flowAsyncRoutingData, declared, changed, 1)
	if bytes.Equal(mutated, flowAsyncRoutingData) {
		t.Fatal("flow async routing count mutation did not alter the fixture")
	}
	if _, err := decodeFlowAsyncDocument(mutated); err == nil {
		t.Fatal("flow async routing fixture accepted a changed exact count")
	}
}
