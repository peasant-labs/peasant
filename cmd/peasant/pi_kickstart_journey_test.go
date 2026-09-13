package main

import (
	"bytes"
	_ "embed"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_kickstart_journey.yaml
var piKickstartJourneyYAML []byte

type piKickstartJourney struct {
	GitCommands   [][]string `yaml:"gitCommands"`
	Name          string     `yaml:"name"`
	SessionID     string     `yaml:"sessionID"`
	Title         string     `yaml:"title"`
	Project       string     `yaml:"project"`
	Preview       string     `yaml:"preview"`
	Diagnostic    string     `yaml:"diagnostic"`
	Completion    string     `yaml:"completion"`
	Source        string     `yaml:"source"`
	CorruptSource string     `yaml:"corruptSource"`
}

func loadPiKickstartJourneys(t *testing.T) []piKickstartJourney {
	t.Helper()
	var document struct {
		RequiredNames []string             `yaml:"requiredNames"`
		Cases         []piKickstartJourney `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(piKickstartJourneyYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("journey fixture must contain one document: %v", err)
	}
	names := make([]string, 0, len(document.Cases))
	for _, row := range document.Cases {
		names = append(names, row.Name)
		if row.Source == "" || row.CorruptSource == "" || row.Title == "" || row.Project == "" || row.Preview == "" || row.Diagnostic == "" || row.Completion == "" {
			t.Fatal("journey must require source and observable outcomes")
		}
		if _, err := schema.NewSessionID(row.SessionID); err != nil {
			t.Fatal(err)
		}
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: document.RequiredNames}, names, "Pi kickstart journey"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// Only the terminal execution boundary is replaced. Discovery, preview loading,
// selection commit, post-consent command and its pipeline/store are production.
func TestPiKickstartMountedDiscoveryThroughStoredImport(t *testing.T) {
	for _, row := range loadPiKickstartJourneys(t) {
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			project := filepath.Join(root, row.Project)
			if err := os.MkdirAll(project, 0700); err != nil {
				t.Fatal(err)
			}
			for _, args := range row.GitCommands {
				command := exec.CommandContext(t.Context(), "git", args...)
				command.Dir = project
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("fixture git setup: %v: %s", err, output)
				}
			}
			sourceText := strings.ReplaceAll(row.Source, "/fixture/project", project)
			sources := filepath.Join(root, ".pi", "agent", "sessions", "project")
			if err := os.MkdirAll(sources, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(sources, "session.jsonl")
			if err := os.WriteFile(source, []byte(sourceText), 0600); err != nil {
				t.Fatal(err)
			}
			// A finished recording is older than the ordinary active-session gate.
			finished := time.Date(2025, time.January, 1, 0, 1, 0, 0, time.UTC)
			if err := os.Chtimes(source, finished, finished); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sources, "corrupt.jsonl"), []byte(row.CorruptSource), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := config.BaseConfig()
			cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected}
			cfg.Output.BasePath = filepath.Join(root, "managed")
			configPath := defaults.ResolveConfigFilePathWith(root).String()
			if err := config.SaveAtomic(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			deps := defaultKickstartCommandDeps()
			mounted := false
			deps.runModel = func(model tea.Model) error {
				mounted = true
				program := model.(kickstart.Model).Program()
				program.SetSize(120, 40)
				send := func(key tea.KeyPressMsg) {
					var command tea.Cmd
					program, command = program.Update(key)
					program = drainMountedLegacyProgram(t, program, command)
				}
				send(tea.KeyPressMsg{Code: tea.KeyEnter}) // continue locally
				view := flattenPane(program.View())
				if !strings.Contains(view, row.Diagnostic) || !strings.Contains(strings.ReplaceAll(view, " ", ""), "corrupt.jsonl") {
					t.Fatalf("mounted project preview hides skipped source: %s", view)
				}
				// Expand the actual discovered project and branch, then select the
				// session through the picker, not a pre-populated draft.
				for step := 0; step < 8 && !strings.Contains(flattenPane(program.View()), row.Preview); step++ {
					send(tea.KeyPressMsg{Code: tea.KeyRight})
					send(tea.KeyPressMsg{Code: 'j', Text: "j"})
				}
				send(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
				view = flattenPane(program.View())
				if !strings.Contains(view, row.Preview) || !strings.Contains(view, "harness: pi") || !strings.Contains(view, row.Title) || !strings.Contains(view, row.Project) {
					t.Fatalf("mounted discovered selection/source preview missing: %s", view)
				}
				if _, err := os.Stat(defaults.ResolveDBFilePathWith(root).String()); !os.IsNotExist(err) {
					t.Fatalf("preview imported before consent: %v", err)
				}
				for step := 0; step < 24 && !strings.Contains(flattenPane(program.View()), "review your changes"); step++ {
					if program.Phase() == kickstart.PhaseVisibility {
						send(tea.KeyPressMsg{Code: tea.KeyEnter})
					} else {
						send(tea.KeyPressMsg{Code: tea.KeyTab})
					}
				}
				if !strings.Contains(flattenPane(program.View()), "review your changes") {
					t.Fatal("mounted flow never reached consent receipt")
				}
				send(tea.KeyPressMsg{Code: tea.KeyEnter}) // commit -> real ingest command
				if program.Phase() != kickstart.PhaseDone || !strings.Contains(flattenPane(program.View()), row.Completion) {
					t.Fatalf("mounted import did not complete: %s", flattenPane(program.View()))
				}
				return nil
			}
			if _, err := executeWithDataDir(t, buildKickstartCommand(deps), root, nil); err != nil {
				t.Fatal(err)
			}
			if !mounted {
				t.Fatal("command did not mount guided kickstart")
			}
			saved, err := loadConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			selected := saved.Selection.Harnesses[schema.HarnessPi.String()].Sessions
			if saved.Selection.Mode != config.SelectionModeSelected || len(selected) != 1 || selected[0] != row.SessionID {
				t.Fatalf("mounted consent did not persist the explicit session choice: %+v", saved.Selection)
			}
			db, err := store.Open(defaults.ResolveDBFilePathWith(root).String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ids, err := db.AllSessionIDs(t.Context())
			if err != nil || len(ids) != 1 || ids[0] != row.SessionID {
				t.Fatalf("actual import must store selected valid session only: %v %v", ids, err)
			}
			session, err := api.NewStoreDataProvider(db, sessionvisibility.All()).SessionByID(t.Context(), row.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if session.Harness != schema.HarnessPi || len(session.Turns) != 2 || !strings.Contains(session.Turns[0].Content, row.Preview) {
				t.Fatalf("stored Pi transcript missing: %+v", session)
			}
			original, err := os.ReadFile(source)
			if err != nil || string(original) != sourceText {
				t.Fatal("kickstart changed native source")
			}
		})
	}
}
