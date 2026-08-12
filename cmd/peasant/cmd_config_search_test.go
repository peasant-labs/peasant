package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"
)

const (
	expectedConfigSearchCaseCount  = 4
	expectedConfigSearchHintCount  = 4
	expectedConfigSearchModalCount = 2
	expectedConfigSearchTailCount  = 1
)

//go:embed testdata/config-screen/search.yaml
var configSearchFixtureData []byte

type configSearchCase struct {
	Name          string `yaml:"name"`
	Input         string `yaml:"input"`
	WantEditing   string `yaml:"wantEditing"`
	WantKept      string `yaml:"wantKept"`
	ForbiddenHint string `yaml:"forbiddenHint"`
}

type configSearchTailCase struct {
	Name        string `yaml:"name"`
	Input       string `yaml:"input"`
	Width       int    `yaml:"width"`
	Height      int    `yaml:"height"`
	WantEditing string `yaml:"wantEditing"`
}

type configSearchDocument struct {
	ExpectedCaseCount           int                    `yaml:"expectedCaseCount"`
	ExpectedRequiredHintCount   int                    `yaml:"expectedRequiredHintCount"`
	ExpectedForbiddenModalCount int                    `yaml:"expectedForbiddenModalCount"`
	ExpectedTailCaseCount       int                    `yaml:"expectedTailCaseCount"`
	RequiredHints               []string               `yaml:"requiredHints"`
	ForbiddenModals             []string               `yaml:"forbiddenModals"`
	Cases                       []configSearchCase     `yaml:"cases"`
	TailCases                   []configSearchTailCase `yaml:"tailCases"`
}

func decodeConfigSearchFixture(data []byte) (configSearchDocument, error) {
	var document configSearchDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/config-screen/search.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("config search fixture must hold exactly one document: %w", err)
	}
	if document.ExpectedCaseCount != expectedConfigSearchCaseCount || len(document.Cases) != expectedConfigSearchCaseCount ||
		document.ExpectedRequiredHintCount != expectedConfigSearchHintCount || len(document.RequiredHints) != expectedConfigSearchHintCount ||
		document.ExpectedForbiddenModalCount != expectedConfigSearchModalCount || len(document.ForbiddenModals) != expectedConfigSearchModalCount {
		return document, fmt.Errorf("config search fixture counts are not pinned: cases=%d/%d hints=%d/%d modals=%d/%d",
			document.ExpectedCaseCount, len(document.Cases), document.ExpectedRequiredHintCount, len(document.RequiredHints),
			document.ExpectedForbiddenModalCount, len(document.ForbiddenModals))
	}
	if document.ExpectedTailCaseCount != expectedConfigSearchTailCount || len(document.TailCases) != expectedConfigSearchTailCount {
		return document, fmt.Errorf("config search tail cases are not pinned: declared=%d actual=%d required=%d",
			document.ExpectedTailCaseCount, len(document.TailCases), expectedConfigSearchTailCount)
	}
	seenNames := map[string]bool{}
	seenInputs := map[string]bool{}
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || seenNames[row.Name] || len([]rune(row.Input)) != 1 || seenInputs[row.Input] ||
			row.WantEditing == "" || row.WantKept == "" || row.ForbiddenHint == "" {
			return document, fmt.Errorf("config search fixture contains an invalid or duplicate row: %#v", row)
		}
		seenNames[row.Name] = true
		seenInputs[row.Input] = true
	}
	for _, values := range [][]string{document.RequiredHints, document.ForbiddenModals} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return document, fmt.Errorf("config search fixture contains an empty required assertion")
			}
		}
	}
	tailNames := map[string]bool{}
	for _, row := range document.TailCases {
		if strings.TrimSpace(row.Name) == "" || tailNames[row.Name] || row.Input == "" || row.Width <= 0 || row.Height <= 0 || row.WantEditing == "" {
			return document, fmt.Errorf("config search tail fixture contains an invalid or duplicate row: %#v", row)
		}
		tailNames[row.Name] = true
	}
	return document, nil
}

func plainConfigScreen(model tea.Model) string {
	return ansiPattern.ReplaceAllString(model.View().Content, "")
}

func TestConfigCommandMountedScreenSearchParity(t *testing.T) {
	document, err := decodeConfigSearchFixture(configSearchFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			world := newConfigScreenWorld(t, 90)
			deps := world.dependencies(t)
			mountedRunCalls := 0
			deps.run = func(model tea.Model) (tea.Model, error) {
				mountedRunCalls++
				model = configScreenDrain(model, model.Init())
				model = configScreenUpdate(model, tea.WindowSizeMsg{Width: 120, Height: 30})
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: tea.KeyEnter})
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: '/', Text: "/"})
				r := []rune(row.Input)[0]
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: r, Text: row.Input})

				editing := plainConfigScreen(model)
				assertMountedSelectionSearch(t, editing)
				if !strings.Contains(editing, row.WantEditing) {
					t.Errorf("mounted config search does not contain %q:\n%s", row.WantEditing, editing)
				}
				if strings.Contains(editing, row.ForbiddenHint) {
					t.Errorf("mounted config footer advertises shadowed hint %q:\n%s", row.ForbiddenHint, editing)
				}
				for _, forbiddenModal := range document.ForbiddenModals {
					if strings.Contains(editing, forbiddenModal) {
						t.Errorf("mounted config search opened forbidden modal %q:\n%s", forbiddenModal, editing)
					}
				}
				for _, hint := range document.RequiredHints {
					if !strings.Contains(editing, hint) {
						t.Errorf("mounted config footer omits live search hint %q:\n%s", hint, editing)
					}
				}

				model = configScreenUpdate(model, tea.KeyPressMsg{Code: tea.KeyBackspace})
				deleted := plainConfigScreen(model)
				assertMountedSelectionSearch(t, deleted)
				if strings.Contains(deleted, row.WantEditing) || !strings.Contains(deleted, "search: ▏") {
					t.Errorf("mounted config delete did not remove query %q:\n%s", row.Input, deleted)
				}
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: r, Text: row.Input})
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: tea.KeyEnter})
				kept := plainConfigScreen(model)
				assertMountedSelectionSearch(t, kept)
				if !strings.Contains(kept, row.WantKept) {
					t.Errorf("mounted config keep does not contain %q:\n%s", row.WantKept, kept)
				}
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: '/', Text: "/"})
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: tea.KeyEscape})
				cleared := plainConfigScreen(model)
				assertMountedSelectionSearch(t, cleared)
				if strings.Contains(cleared, row.WantEditing) || strings.Contains(cleared, row.WantKept) {
					t.Errorf("mounted config clear retained search state for %q:\n%s", row.Input, cleared)
				}
				return model, nil
			}

			if _, err := executeConfigScreenCommand(t, buildConfigCommand(deps), world, "config"); err != nil {
				t.Fatalf("execute mounted config search: %v", err)
			}
			if mountedRunCalls != 1 {
				t.Fatalf("mounted config runner calls=%d want=1", mountedRunCalls)
			}
			if got := mustReadConfigScreenFile(t, world.configPath); !bytes.Equal(got, world.initialConfig) {
				t.Fatal("mounted config search changed persisted config bytes")
			}
			if got := mustReadConfigScreenFile(t, world.claudePath); !bytes.Equal(got, world.initialClaude) {
				t.Fatal("mounted config search changed persisted Claude settings bytes")
			}
		})
	}
}

func TestConfigCommandMountedSearchKeepsLatestGraphemesAndCaret(t *testing.T) {
	t.Parallel()

	document, err := decodeConfigSearchFixture(configSearchFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range document.TailCases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			world := newConfigScreenWorld(t, 90)
			deps := world.dependencies(t)
			deps.run = func(model tea.Model) (tea.Model, error) {
				model = configScreenDrain(model, model.Init())
				model = configScreenUpdate(model, tea.WindowSizeMsg{Width: row.Width, Height: row.Height})
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: tea.KeyEnter})
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: '/', Text: "/"})
				input := []rune(row.Input)[0]
				model = configScreenUpdate(model, tea.KeyPressMsg{Code: input, Text: row.Input})
				view := plainConfigScreen(model)
				if !strings.Contains(view, row.WantEditing) {
					t.Errorf("mounted config search does not retain its latest graphemes and caret %q:\n%s", row.WantEditing, view)
				}
				return model, nil
			}
			if _, err := executeConfigScreenCommand(t, buildConfigCommand(deps), world, "config"); err != nil {
				t.Fatalf("execute mounted config tail search: %v", err)
			}
		})
	}
}

func assertMountedSelectionSearch(t *testing.T, view string) {
	t.Helper()
	if got := strings.Count(view, "search:"); got != 1 {
		t.Errorf("mounted selection contains %d search bars, want exactly one:\n%s", got, view)
	}
	for _, forbidden := range []string{
		"transcripts", "scope:", "previous scope", "next scope", "search scope",
		"tracked =", "imported =", "selected sessions:", "hidden by filters:", "view only:",
	} {
		if strings.Contains(view, forbidden) {
			t.Errorf("mounted selection contains removed text %q:\n%s", forbidden, view)
		}
	}
}

func TestConfigCommandSearchFixtureGuards(t *testing.T) {
	if _, err := decodeConfigSearchFixture(append(append([]byte(nil), configSearchFixtureData...), []byte("\nunknownField: true\n")...)); err == nil {
		t.Fatal("config search fixture accepted an unknown field")
	}
	if _, err := decodeConfigSearchFixture(append(append([]byte(nil), configSearchFixtureData...), []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("config search fixture accepted a trailing document")
	}
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedConfigSearchCaseCount))
	changed := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedConfigSearchCaseCount+1))
	mutated := bytes.Replace(configSearchFixtureData, declared, changed, 1)
	if bytes.Equal(mutated, configSearchFixtureData) {
		t.Fatal("config search exact-count mutation did not alter the fixture")
	}
	if _, err := decodeConfigSearchFixture(mutated); err == nil {
		t.Fatal("config search fixture accepted a changed exact count")
	}
}
