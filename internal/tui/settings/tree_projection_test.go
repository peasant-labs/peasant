package settings

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/tree_projection.yaml
var treeProjectionData []byte

type projectionSession struct {
	ID      string `yaml:"id"`
	Label   string `yaml:"label"`
	Harness string `yaml:"harness"`
	Remote  string `yaml:"remote"`
	Project string `yaml:"project"`
	Branch  string `yaml:"branch"`
}

type projectionRowAssertion struct {
	Label        string   `yaml:"label"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

type projectionCase struct {
	Name                 string                   `yaml:"name"`
	Keys                 []string                 `yaml:"keys"`
	ExpectPane           string                   `yaml:"expectPane"`
	ExpectScope          string                   `yaml:"expectScope"`
	ExpectMode           string                   `yaml:"expectMode"`
	ExpectQuery          string                   `yaml:"expectQuery"`
	ExpectSelected       []string                 `yaml:"expectSelected"`
	ExpectUnselected     []string                 `yaml:"expectUnselected"`
	CheckHiddenSelected  bool                     `yaml:"checkHiddenSelected"`
	ExpectHiddenSelected int                      `yaml:"expectHiddenSelected"`
	WantVisibleRows      []string                 `yaml:"wantVisibleRows"`
	WantMissingRows      []string                 `yaml:"wantMissingRows"`
	WantViewContains     []string                 `yaml:"wantViewContains"`
	WantViewMissing      []string                 `yaml:"wantViewMissing"`
	RowAssertions        []projectionRowAssertion `yaml:"rowAssertions"`
}

type projectionDocument struct {
	Fixture              string                 `yaml:"fixture"`
	ExpectedSessionCount int                    `yaml:"expectedSessionCount"`
	ExpectedCaseCount    int                    `yaml:"expectedCaseCount"`
	Width                int                    `yaml:"width"`
	Height               int                    `yaml:"height"`
	SavedSelection       config.SelectionConfig `yaml:"savedSelection"`
	ImportedSessionIDs   []string               `yaml:"importedSessionIDs"`
	Sessions             []projectionSession    `yaml:"sessions"`
	Cases                []projectionCase       `yaml:"cases"`
}

func decodeTreeProjection(data []byte) (projectionDocument, error) {
	var doc projectionDocument
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/tree_projection.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("tree_projection.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedSessionCount != len(doc.Sessions) || len(doc.Sessions) == 0 {
		return doc, fmt.Errorf("tree_projection.yaml expectedSessionCount=%d but has %d sessions", doc.ExpectedSessionCount, len(doc.Sessions))
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		return doc, fmt.Errorf("tree_projection.yaml expectedCaseCount=%d but has %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	if doc.Fixture == "" || doc.Width <= 0 || doc.Height <= 0 || !doc.SavedSelection.Mode.IsValid() {
		return doc, fmt.Errorf("tree_projection.yaml must declare a fixture, positive region, and valid saved mode")
	}
	seenSessions := map[string]bool{}
	for _, session := range doc.Sessions {
		if session.ID == "" || session.Label == "" || session.Harness == "" || session.Branch == "" || seenSessions[session.ID] {
			return doc, fmt.Errorf("tree_projection.yaml contains an invalid or duplicate session: %#v", session)
		}
		seenSessions[session.ID] = true
	}
	seenCases := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || c.ExpectScope == "" || c.ExpectMode == "" || seenCases[c.Name] {
			return doc, fmt.Errorf("tree_projection.yaml contains an invalid or duplicate case: %#v", c)
		}
		seenCases[c.Name] = true
		if len(c.ExpectSelected)+len(c.ExpectUnselected)+len(c.WantVisibleRows)+len(c.WantMissingRows)+len(c.WantViewContains)+len(c.WantViewMissing)+len(c.RowAssertions) == 0 {
			return doc, fmt.Errorf("tree_projection.yaml case %q has no observable assertion", c.Name)
		}
		for _, row := range c.RowAssertions {
			if row.Label == "" || len(row.WantContains)+len(row.WantMissing) == 0 {
				return doc, fmt.Errorf("tree_projection.yaml case %q has an empty row assertion", c.Name)
			}
		}
	}
	return doc, nil
}

func loadTreeProjection(t *testing.T) projectionDocument {
	t.Helper()
	doc, err := decodeTreeProjection(treeProjectionData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

type projectionSource struct {
	fixture  string
	imported map[string]bool
}

func (s projectionSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roots, err := scannerfix.Load(s.fixture)
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		walkNodes(root, func(node *kit.TreeNode) {
			if !s.imported[node.ID] {
				return
			}
			if node.Meta == nil {
				node.Meta = map[string]string{}
			}
			node.Meta[MetaIngested] = MetaIngestedValue
		})
	}
	return roots, nil
}

type projectionPreviewSource struct{}

type projectionPreviewBody string

func (b projectionPreviewBody) Render(_ int) string { return string(b) }

func (projectionPreviewSource) Body(id string) (kit.PreviewBody, error) {
	return projectionPreviewBody("preview for " + id), nil
}

func mountedProjectionFlow(t *testing.T, doc projectionDocument) (Flow, *Draft, *treeField) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.BaseConfig()
	cfg.Selection = doc.SavedSelection
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed projection config: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read projection config: %v", err)
	}
	loaded, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse projection config: %v", err)
	}
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	imported := map[string]bool{}
	for _, id := range doc.ImportedSessionIDs {
		imported[id] = true
	}
	field := Tree("selection", "transcripts", selectionAccessor(), projectionSource{fixture: doc.Fixture, imported: imported},
		WithFacet(MetaHarness, "harness"), WithPreviewBodySource(projectionPreviewSource{}))
	reg := Registry{Sections: []Section{{Key: "transcripts", Title: "select transcripts", Fields: []Field{field}}}}
	flow := NewFlow(theme.New(theme.ModeDark), reg, draft)
	flow.SetSize(doc.Width, doc.Height)
	flow = drainInit(flow)
	return flow, draft, field.(*treeField)
}

func projectionKey(t *testing.T, value string) tea.KeyPressMsg {
	t.Helper()
	switch value {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "ctrl+h":
		return tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl}
	case "ctrl+l":
		return tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}
	default:
		runes := []rune(value)
		if len(runes) != 1 {
			t.Fatalf("projection fixture names unsupported key %q", value)
		}
		return tea.KeyPressMsg{Code: runes[0], Text: value}
	}
}

func driveProjectionFlow(t *testing.T, flow Flow, keys []string) Flow {
	t.Helper()
	for _, value := range keys {
		var cmd tea.Cmd
		flow, cmd = flow.Update(projectionKey(t, value))
		for _, msg := range runAll(cmd) {
			flow, _ = flow.Update(msg)
		}
	}
	return flow
}

func lineContaining(view, label string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, label) {
			return line
		}
	}
	return ""
}

func projectionSelects(sel config.SelectionConfig, session projectionSession) bool {
	match := config.CompileSelectionMatcher(sel).MatchDiscovery(
		ingest.Harness(session.Harness), session.Remote, session.Project, session.Branch,
		ingest.SessionID(session.ID), sel.AutoIngestNewBranches)
	return match == ingest.BranchMatchYes
}

func projectionSessionByID(t *testing.T, sessions []projectionSession, id string) projectionSession {
	t.Helper()
	for _, session := range sessions {
		if session.ID == id {
			return session
		}
	}
	t.Fatalf("projection fixture has no session %q", id)
	return projectionSession{}
}

func TestFlow_TreeProjectionPreservesCanonicalSelection(t *testing.T) {
	doc := loadTreeProjection(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			flow, draft, field := mountedProjectionFlow(t, doc)
			flow = driveProjectionFlow(t, flow, c.Keys)
			view := stripANSIForSettings(flow.View())

			if got := field.tree.Scope().String(); got != c.ExpectScope {
				t.Errorf("scope = %q, want %q", got, c.ExpectScope)
			}
			state := field.tree.FilterState()
			if got := state.Mode.String(); got != c.ExpectMode || state.Query != c.ExpectQuery {
				t.Errorf("filter = %s/%q, want %s/%q", got, state.Query, c.ExpectMode, c.ExpectQuery)
			}
			if c.ExpectPane != "" && field.split.ActivePane().String() != c.ExpectPane {
				t.Errorf("active pane = %s, want %s", field.split.ActivePane(), c.ExpectPane)
			}

			selection := draft.Working().Selection
			for _, id := range c.ExpectSelected {
				if !projectionSelects(selection, projectionSessionByID(t, doc.Sessions, id)) {
					t.Errorf("current selection does not include %q: %#v", id, selection)
				}
			}
			for _, id := range c.ExpectUnselected {
				if projectionSelects(selection, projectionSessionByID(t, doc.Sessions, id)) {
					t.Errorf("current selection unexpectedly includes %q: %#v", id, selection)
				}
			}
			if c.CheckHiddenSelected {
				want := fmt.Sprintf("hidden by filters: %d", c.ExpectHiddenSelected)
				if !strings.Contains(view, want) {
					t.Errorf("view does not report %q:\n%s", want, view)
				}
			}

			for _, want := range c.WantVisibleRows {
				if !strings.Contains(view, want) {
					t.Errorf("visible row %q missing:\n%s", want, view)
				}
			}
			for _, missing := range c.WantMissingRows {
				if strings.Contains(view, missing) {
					t.Errorf("filtered row %q is still visible:\n%s", missing, view)
				}
			}
			for _, want := range c.WantViewContains {
				if !strings.Contains(view, want) {
					t.Errorf("view must contain %q:\n%s", want, view)
				}
			}
			for _, missing := range c.WantViewMissing {
				if strings.Contains(view, missing) {
					t.Errorf("view must not contain %q:\n%s", missing, view)
				}
			}
			for _, assertion := range c.RowAssertions {
				line := lineContaining(view, assertion.Label)
				if line == "" {
					t.Errorf("row %q is absent:\n%s", assertion.Label, view)
					continue
				}
				for _, want := range assertion.WantContains {
					if !strings.Contains(line, want) {
						t.Errorf("row %q must contain %q: %s", assertion.Label, want, line)
					}
				}
				for _, missing := range assertion.WantMissing {
					if strings.Contains(line, missing) {
						t.Errorf("row %q must not contain %q: %s", assertion.Label, missing, line)
					}
				}
			}
		})
	}
}

func stripANSIForSettings(value string) string {
	var out strings.Builder
	inEscape := false
	for _, r := range value {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func TestTreeProjectionFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), treeProjectionData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeTreeProjection(mutated); err == nil {
		t.Fatal("tree projection fixture accepted an unknown field")
	}
}

func TestTreeProjectionFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), treeProjectionData...), []byte("\n---\n{}\n")...)
	if _, err := decodeTreeProjection(mutated); err == nil {
		t.Fatal("tree projection fixture accepted a trailing document")
	}
}

func TestTreeProjectionFixtureEnforcesRowCount(t *testing.T) {
	mutated := bytes.Replace(treeProjectionData, []byte("expectedCaseCount: 7"), []byte("expectedCaseCount: 8"), 1)
	if _, err := decodeTreeProjection(mutated); err == nil {
		t.Fatal("tree projection fixture accepted a mismatched row-count guard")
	}
}
