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
	expectedScreenAsyncCaseCount             = 2
	expectedScreenAsyncModalCount            = 1
	expectedScreenAsyncTextPreservationCount = 1
	expectedScreenAsyncCommitCount           = 1
)

//go:embed testdata/screen/async-routing.yaml
var screenAsyncRoutingData []byte

type screenAsyncScenario string

const (
	screenAsyncDiscardModal   screenAsyncScenario = "discard-modal"
	screenAsyncNonCurrentText screenAsyncScenario = "non-current-text"
)

func (s screenAsyncScenario) valid() bool {
	return s == screenAsyncDiscardModal || s == screenAsyncNonCurrentText
}

type screenAsyncCase struct {
	Name            string              `yaml:"name"`
	Scenario        screenAsyncScenario `yaml:"scenario"`
	InitialText     string              `yaml:"initialText"`
	SessionID       string              `yaml:"sessionID"`
	ExpectLoadCount int                 `yaml:"expectLoadCount"`
	ExpectReady     bool                `yaml:"expectReady"`
	ExpectCommit    bool                `yaml:"expectCommit"`
}

type screenAsyncDocument struct {
	ExpectedCaseCount             int               `yaml:"expectedCaseCount"`
	ExpectedModalCaseCount        int               `yaml:"expectedModalCaseCount"`
	ExpectedTextPreservationCount int               `yaml:"expectedTextPreservationCount"`
	ExpectedCommitCount           int               `yaml:"expectedCommitCount"`
	Cases                         []screenAsyncCase `yaml:"cases"`
}

func decodeScreenAsyncRouting(data []byte) (screenAsyncDocument, error) {
	var document screenAsyncDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/screen/async-routing.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("screen async routing fixture must hold exactly one document: %w", err)
	}
	if document.ExpectedCaseCount != expectedScreenAsyncCaseCount || len(document.Cases) != expectedScreenAsyncCaseCount {
		return document, fmt.Errorf("screen async routing cases: declared=%d actual=%d required=%d", document.ExpectedCaseCount, len(document.Cases), expectedScreenAsyncCaseCount)
	}
	counts := map[screenAsyncScenario]int{}
	commitCount := 0
	seen := map[string]bool{}
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || seen[row.Name] || !row.Scenario.valid() || strings.TrimSpace(row.InitialText) == "" ||
			strings.TrimSpace(row.SessionID) == "" || row.ExpectLoadCount != 1 || !row.ExpectReady {
			return document, fmt.Errorf("screen async routing fixture contains an invalid or duplicate row: %#v", row)
		}
		seen[row.Name] = true
		counts[row.Scenario]++
		if row.ExpectCommit {
			commitCount++
		}
	}
	if document.ExpectedModalCaseCount != expectedScreenAsyncModalCount || counts[screenAsyncDiscardModal] != expectedScreenAsyncModalCount ||
		document.ExpectedTextPreservationCount != expectedScreenAsyncTextPreservationCount || counts[screenAsyncNonCurrentText] != expectedScreenAsyncTextPreservationCount {
		return document, fmt.Errorf("screen async scenario counts are not pinned: modal=%d text=%d", counts[screenAsyncDiscardModal], counts[screenAsyncNonCurrentText])
	}
	if document.ExpectedCommitCount != expectedScreenAsyncCommitCount || commitCount != expectedScreenAsyncCommitCount {
		return document, fmt.Errorf("screen async commit cases: declared=%d actual=%d required=%d", document.ExpectedCommitCount, commitCount, expectedScreenAsyncCommitCount)
	}
	return document, nil
}

func loadScreenAsyncRouting(t *testing.T) screenAsyncDocument {
	t.Helper()
	document, err := decodeScreenAsyncRouting(screenAsyncRoutingData)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

type countedScreenTreeSource struct {
	id    string
	loads atomic.Int32
}

func (s *countedScreenTreeSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
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
				Label: "async fixture session",
				Meta:  map[string]string{MetaHarness: "fixture"},
			}},
		}},
	}}, nil
}

func screenAsyncTextAccessor() Accessor[string] {
	return Accessor[string]{
		Get: func(cfg *config.Config) string { return cfg.Village.URL },
		Set: func(cfg *config.Config, value string) { cfg.Village.URL = value },
	}
}

func screenAsyncToggleAccessor() Accessor[bool] {
	return Accessor[bool]{
		Get: func(cfg *config.Config) bool { return cfg.Selection.AutoIngestNewBranches },
		Set: func(cfg *config.Config, value bool) { cfg.Selection.AutoIngestNewBranches = value },
	}
}

func marshalScreenWorking(t *testing.T, draft *Draft) []byte {
	t.Helper()
	data, err := yaml.Marshal(draft.Working())
	if err != nil {
		t.Fatalf("marshal working config: %v", err)
	}
	return data
}

func screenTreeContainsID(roots []*kit.TreeNode, id string) bool {
	for _, root := range roots {
		if root.ID == id || screenTreeContainsID(root.Children, id) {
			return true
		}
	}
	return false
}

func ownedTreeResult(t *testing.T, command tea.Cmd, field *treeField) tea.Msg {
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
		t.Fatal("screen Init produced no owned tree result")
	}
	return message
}

func mountedScreenAsyncCase(t *testing.T, row screenAsyncCase) (Screen, *Draft, *treeField, *countedScreenTreeSource, []byte, tea.Msg) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.BaseConfig()
	cfg.Village.URL = row.InitialText
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed screen async config: %v", err)
	}
	draft, err := NewDraft(path, cfg)
	if err != nil {
		t.Fatalf("open screen async draft: %v", err)
	}
	source := &countedScreenTreeSource{id: row.SessionID}
	tree := Tree("selection", "transcripts", selectionAccessor(), source, WithDraftSelectionState()).(*treeField)
	text := Text("village-url", "village url", screenAsyncTextAccessor())
	if _, ok := text.(asyncField); ok {
		t.Fatal("ordinary Text field unexpectedly implements asyncField")
	}
	toggle := Toggle("auto-ingest", "auto-ingest", screenAsyncToggleAccessor())
	if _, ok := toggle.(asyncField); ok {
		t.Fatal("ordinary Toggle field unexpectedly implements asyncField")
	}
	registry := Registry{Sections: []Section{
		{Key: "selection", Title: "selection", Fields: []Field{tree}},
		{Key: "connection", Title: "connection", Fields: []Field{text}},
	}}
	screen := NewScreen(theme.New(theme.ModeDark), registry, draft)
	screen.SetSize(100, 28)
	before := marshalScreenWorking(t, draft)
	result := ownedTreeResult(t, screen.Init(), tree)
	return screen, draft, tree, source, before, result
}

func TestScreenRoutesOwnedAsyncBeforeDiscardModalAndPreservesText(t *testing.T) {
	for _, row := range loadScreenAsyncRouting(t).Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			screen, draft, field, source, before, result := mountedScreenAsyncCase(t, row)
			if row.Scenario == screenAsyncDiscardModal {
				screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				if !screen.confirming {
					t.Fatal("screen did not open discard confirmation")
				}
			}

			screen, _ = screen.Update(result)
			if field.forestReady != row.ExpectReady {
				t.Fatalf("tree ready=%t want=%t", field.forestReady, row.ExpectReady)
			}
			if got := int(source.loads.Load()); got != row.ExpectLoadCount {
				t.Fatalf("source loads=%d want=%d", got, row.ExpectLoadCount)
			}
			if !screenTreeContainsID(field.tree.Roots(), row.SessionID) {
				t.Fatalf("ready tree does not contain session %q: %#v", row.SessionID, field.tree.Roots())
			}
			if got := marshalScreenWorking(t, draft); !bytes.Equal(got, before) {
				t.Fatalf("owned tree result changed unrelated Draft bytes\nbefore=%s\nafter=%s", before, got)
			}
			if got := draft.Working().Village.URL; got != row.InitialText {
				t.Fatalf("non-current text=%q want byte-preserved %q", got, row.InitialText)
			}

			if row.Scenario == screenAsyncDiscardModal {
				screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				if screen.confirming {
					t.Fatal("default-no discard confirmation did not cancel")
				}
				if !field.forestReady || int(source.loads.Load()) != row.ExpectLoadCount {
					t.Fatal("cancelling discard lost the accepted one-load tree state")
				}
			}
			if row.ExpectCommit {
				var command tea.Cmd
				screen, command = screen.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
				saved, ok := runResult(command).(SavedMsg)
				if !ok || saved.Draft() != draft || !screen.savePending {
					t.Fatalf("post-modal save=%T/%p pending=%t want SavedMsg for Draft %p", saved, saved.Draft(), screen.savePending, draft)
				}
			}
		})
	}
}

func TestGeneralFieldRoutePreservesNonCurrentPrepopulatedText(t *testing.T) {
	document := loadScreenAsyncRouting(t)
	var row screenAsyncCase
	for _, candidate := range document.Cases {
		if candidate.Scenario == screenAsyncNonCurrentText {
			row = candidate
			break
		}
	}
	if row.Name == "" {
		t.Fatal("screen async fixture has no non-current text preservation row")
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.BaseConfig()
	cfg.Village.URL = row.InitialText
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed general-route config: %v", err)
	}
	draft, err := NewDraft(path, cfg)
	if err != nil {
		t.Fatalf("open general-route draft: %v", err)
	}
	source := &countedScreenTreeSource{id: row.SessionID}
	tree := Tree("selection", "transcripts", selectionAccessor(), source, WithDraftSelectionState()).(*treeField)
	text := Text("village-url", "village url", screenAsyncTextAccessor())
	flow := NewFlow(theme.New(theme.ModeDark), Registry{Sections: []Section{
		{Key: "selection", Title: "selection", Fields: []Field{tree}},
		{Key: "connection", Title: "connection", Fields: []Field{text}},
	}}, draft)
	flow.SetSize(100, 28)
	before := marshalScreenWorking(t, draft)
	flow = drainInit(flow)
	_ = flow
	if !tree.forestReady || int(source.loads.Load()) != row.ExpectLoadCount {
		t.Fatalf("general route tree ready/loads=%t/%d want true/%d", tree.forestReady, source.loads.Load(), row.ExpectLoadCount)
	}
	if got := marshalScreenWorking(t, draft); !bytes.Equal(got, before) {
		t.Fatalf("general non-key route changed unrelated Draft bytes\nbefore=%s\nafter=%s", before, got)
	}
	if got := draft.Working().Village.URL; got != row.InitialText {
		t.Fatalf("general non-key route changed non-current text=%q want %q", got, row.InitialText)
	}
}

func TestScreenAsyncRoutingFixtureGuards(t *testing.T) {
	if _, err := decodeScreenAsyncRouting(append(append([]byte(nil), screenAsyncRoutingData...), []byte("\nunknownField: true\n")...)); err == nil {
		t.Fatal("screen async routing fixture accepted an unknown field")
	}
	if _, err := decodeScreenAsyncRouting(append(append([]byte(nil), screenAsyncRoutingData...), []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("screen async routing fixture accepted a trailing document")
	}
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedScreenAsyncCaseCount))
	changed := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedScreenAsyncCaseCount+1))
	mutated := bytes.Replace(screenAsyncRoutingData, declared, changed, 1)
	if bytes.Equal(mutated, screenAsyncRoutingData) {
		t.Fatal("screen async routing count mutation did not alter the fixture")
	}
	if _, err := decodeScreenAsyncRouting(mutated); err == nil {
		t.Fatal("screen async routing fixture accepted a changed exact count")
	}
}
