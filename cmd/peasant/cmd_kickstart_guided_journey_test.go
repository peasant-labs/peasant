package main

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

const expectedMountedJourneyRows = 1

type mountedJourneyFixture struct {
	Name              string   `yaml:"name"`
	ConnectCopy       []string `yaml:"connectCopy"`
	ConsentCopy       []string `yaml:"consentCopy"`
	CompletionCopy    []string `yaml:"completionCopy"`
	ForbiddenCopy     []string `yaml:"forbiddenCopy"`
	WantIngestCalls   int      `yaml:"wantIngestCalls"`
	WantTerminalCalls int      `yaml:"wantTerminalCalls"`
}

type mountedJourneyDocument struct {
	ExpectedRowCount int                     `yaml:"expectedRowCount"`
	Rows             []mountedJourneyFixture `yaml:"rows"`
}

//go:embed testdata/kickstart_guided_journey.yaml
var mountedJourneyData []byte

func loadMountedJourneyDocument(t *testing.T) mountedJourneyDocument {
	t.Helper()
	var document mountedJourneyDocument
	decoder := yaml.NewDecoder(bytes.NewReader(mountedJourneyData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode mounted kickstart journey fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("mounted kickstart journey fixture must contain exactly one YAML document")
	}
	if document.ExpectedRowCount != expectedMountedJourneyRows || len(document.Rows) != expectedMountedJourneyRows {
		t.Fatalf("mounted journey rows: declared=%d actual=%d required=%d",
			document.ExpectedRowCount, len(document.Rows), expectedMountedJourneyRows)
	}
	seen := map[string]bool{}
	for _, row := range document.Rows {
		if strings.TrimSpace(row.Name) == "" || seen[row.Name] || len(row.ConnectCopy) != 2 || len(row.ConsentCopy) == 0 ||
			len(row.CompletionCopy) != 4 || len(row.ForbiddenCopy) == 0 || row.WantIngestCalls != 1 || row.WantTerminalCalls != 1 {
			t.Fatalf("mounted journey row is incomplete or duplicated: %#v", row)
		}
		seen[row.Name] = true
	}
	return document
}

func executeProgramBatchOnce(program kickstart.Program, command tea.Cmd) (kickstart.Program, bool) {
	if command == nil {
		return program, false
	}
	message := command()
	children, ok := message.(tea.BatchMsg)
	if !ok {
		children = tea.BatchMsg{func() tea.Msg { return message }}
	}
	quitEarly := false
	for _, child := range children {
		if child == nil {
			continue
		}
		message := child()
		if message == nil {
			continue
		}
		var next tea.Cmd
		program, next = program.Update(message)
		if program.Phase() == kickstart.PhaseDone && next != nil {
			_, quitEarly = next().(tea.QuitMsg)
		}
	}
	return program, quitEarly
}

func TestKickstartCommandMountsConsentLocalProgressAndPersistentCompletion(t *testing.T) {
	for _, row := range loadMountedJourneyDocument(t).Rows {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := defaults.ResolveConfigFilePathWith(dir).String()
			loaded := config.BaseConfig()
			loaded.Output.BasePath = filepath.Join(dir, "output")
			if err := config.SaveAtomic(configPath, loaded); err != nil {
				t.Fatalf("seed mounted kickstart config: %v", err)
			}

			deps := defaultKickstartCommandDeps()
			deps.discover = func(context.Context, string, string, *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing) {
				return ftue.ProviderInventory{}, nil
			}
			deps.existingUser = func(string) string { return "" }
			deps.readRetention = func() (int, bool) { return 90, true }
			deps.alreadyConnected = func(string) bool { return false }
			ingestCalls := 0
			deps.localIngest = func(*cobra.Command, string, []ftue.SessionListing) (kickstart.IngestFunc, kickstart.ProgressSource) {
				progress := ingest.NewProgressState()
				return func(context.Context) (*ftue.IngestResult, error) {
					ingestCalls++
					progress.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageDiscover, Total: 1})
					progress.Update(ingest.ProgressEvent{Kind: ingest.KindEnd, Stage: ingest.StageDiscover, Done: 1, Total: 1})
					return &ftue.IngestResult{New: 1}, nil
				}, progress
			}

			terminalCalls := 0
			deps.runModel = func(model tea.Model) error {
				terminalCalls++
				mounted, ok := model.(kickstart.Model)
				if !ok {
					t.Fatalf("mounted journey received %T, want kickstart.Model", model)
				}
				program := mounted.Program()
				program.SetSize(180, 50)
				connectView := strings.ToLower(ansiPattern.ReplaceAllString(program.View(), ""))
				choiceAt := strings.Index(connectView, "connect to a village now?")
				for _, want := range row.ConnectCopy {
					at := strings.Index(connectView, strings.ToLower(want))
					if at < 0 || at >= choiceAt {
						t.Errorf("mounted safety copy %q must precede the connection choice:\n%s", want, connectView)
					}
				}

				program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				for step := 0; step < 24; step++ {
					if program.Phase() == kickstart.PhaseVisibility {
						program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
						continue
					}
					view := strings.ToLower(ansiPattern.ReplaceAllString(program.View(), ""))
					if strings.Contains(view, "review your changes") {
						for _, want := range row.ConsentCopy {
							if !strings.Contains(view, strings.ToLower(want)) {
								t.Errorf("mounted consent does not contain %q:\n%s", want, view)
							}
						}
						break
					}
					program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyTab})
				}
				program, command := program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				program, quitEarly := executeProgramBatchOnce(program, command)
				if quitEarly {
					t.Fatal("mounted local completion quit before explicit exit")
				}
				completion := strings.ToLower(ansiPattern.ReplaceAllString(program.View(), ""))
				for _, want := range row.CompletionCopy {
					if !strings.Contains(completion, strings.ToLower(want)) {
						t.Errorf("mounted completion does not contain %q:\n%s", want, completion)
					}
				}
				for _, forbidden := range row.ForbiddenCopy {
					if strings.Contains(completion, strings.ToLower(forbidden)) {
						t.Errorf("mounted completion fabricates %q:\n%s", forbidden, completion)
					}
				}
				return nil
			}

			if _, err := executeWithDataDir(t, buildKickstartCommand(deps), dir, nil); err != nil {
				t.Fatalf("run mounted kickstart journey: %v", err)
			}
			if ingestCalls != row.WantIngestCalls || terminalCalls != row.WantTerminalCalls {
				t.Errorf("mounted ingest/terminal calls=%d/%d, want %d/%d",
					ingestCalls, terminalCalls, row.WantIngestCalls, row.WantTerminalCalls)
			}
		})
	}
}
