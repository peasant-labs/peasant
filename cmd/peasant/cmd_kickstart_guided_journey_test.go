package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

const (
	expectedMountedJourneyRows          = 1
	expectedMountedPreambleMutationRows = 3
	mountedAcceptedUsefulOptionalLine   = "these are useful, optional next commands after local setup."
	mountedAcceptedPurposeLine          = "open the local dashboard, connect to a village, or explicitly publish later."
	mountedAcceptedNoExecutionLine      = "kickstart runs none of them."
)

type mountedJourneyFixture struct {
	Name               string   `yaml:"name"`
	ConnectCopy        []string `yaml:"connectCopy"`
	ConsentCopy        []string `yaml:"consentCopy"`
	ProgressCopy       []string `yaml:"progressCopy"`
	ProgressForbidden  []string `yaml:"progressForbidden"`
	CompletionPreamble []string `yaml:"completionPreamble"`
	CompletionCopy     []string `yaml:"completionCopy"`
	ForbiddenCopy      []string `yaml:"forbiddenCopy"`
	WantIngestCalls    int      `yaml:"wantIngestCalls"`
	WantTerminalCalls  int      `yaml:"wantTerminalCalls"`
}

type mountedPreambleMutationFixture struct {
	Name  string   `yaml:"name"`
	Lines []string `yaml:"lines"`
}

type mountedJourneyDocument struct {
	ExpectedRowCount              int                              `yaml:"expectedRowCount"`
	Rows                          []mountedJourneyFixture          `yaml:"rows"`
	ExpectedPreambleMutationCount int                              `yaml:"expectedPreambleMutationCount"`
	PreambleMutations             []mountedPreambleMutationFixture `yaml:"preambleMutations"`
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
			len(row.ProgressCopy) != 3 || len(row.ProgressForbidden) == 0 || len(row.CompletionPreamble) != 3 ||
			len(row.CompletionCopy) != 4 || len(row.ForbiddenCopy) == 0 || row.WantIngestCalls != 1 || row.WantTerminalCalls != 1 {
			t.Fatalf("mounted journey row is incomplete or duplicated: %#v", row)
		}
		if err := validateMountedCompletionPreamble(row.CompletionPreamble); err != nil {
			t.Fatalf("mounted journey row does not declare the accepted completion preamble: %v", err)
		}
		seen[row.Name] = true
	}
	if document.ExpectedPreambleMutationCount != expectedMountedPreambleMutationRows ||
		len(document.PreambleMutations) != expectedMountedPreambleMutationRows {
		t.Fatalf("mounted preamble mutation rows: declared=%d actual=%d required=%d",
			document.ExpectedPreambleMutationCount, len(document.PreambleMutations), expectedMountedPreambleMutationRows)
	}
	mutationNames := map[string]bool{}
	for _, row := range document.PreambleMutations {
		if strings.TrimSpace(row.Name) == "" || mutationNames[row.Name] || len(row.Lines) != 3 {
			t.Fatalf("mounted preamble mutation row is incomplete or duplicated: %#v", row)
		}
		mutationNames[row.Name] = true
	}
	return document
}

func validateMountedCompletionPreamble(lines []string) error {
	expected := mountedAcceptedCompletionPreambleLines()
	if len(lines) != len(expected) {
		return fmt.Errorf("preamble lines=%d required=%d", len(lines), len(expected))
	}
	for index, line := range lines {
		if line != expected[index] {
			return fmt.Errorf("preamble line %d=%q required=%q", index+1, line, expected[index])
		}
	}
	return nil
}

func mountedAcceptedCompletionPreambleLines() []string {
	return []string{
		mountedAcceptedUsefulOptionalLine,
		mountedAcceptedPurposeLine,
		mountedAcceptedNoExecutionLine,
	}
}

func TestMountedJourneyFixtureRejectsKeywordPreservingPreambleMutations(t *testing.T) {
	for _, mutation := range loadMountedJourneyDocument(t).PreambleMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			if err := validateMountedCompletionPreamble(mutation.Lines); err == nil {
				t.Fatal("mounted fixture accepted a keyword-preserving completion preamble mutation")
			}
			mutatedRender := strings.Join(append(append([]string(nil), mutation.Lines...), "peasant web start"), "\n")
			if err := validateMountedRenderedCompletionPreamble(mutatedRender, "peasant web start"); err == nil {
				t.Fatal("mounted render accepted a keyword-preserving completion preamble mutation")
			}
		})
	}
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
			ingestStarted := make(chan struct{})
			releaseIngest := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseIngest) }) }
			defer release()
			deps.localIngest = func(*cobra.Command, string, []ftue.SessionListing) (kickstart.IngestFunc, kickstart.ProgressSource) {
				progress := ingest.NewProgressState()
				return func(context.Context) (*ftue.IngestResult, error) {
					ingestCalls++
					progress.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageDiscover})
					progress.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiscover, Done: 1})
					close(ingestStarted)
					<-releaseIngest
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

				program, startup := program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				program = drainMountedKickstartStartup(program, startup)
				for step := 0; step < 24; step++ {
					if program.Phase() == kickstart.PhaseVisibility {
						var resume tea.Cmd
						program, resume = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
						program = drainMountedKickstartStartup(program, resume)
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
				if command == nil {
					t.Fatal("mounted receipt confirmation did not start local ingest")
				}
				message := command()
				children, ok := message.(tea.BatchMsg)
				if !ok || len(children) != 3 {
					t.Fatalf("mounted local ingest command produced %T with %d children, want a three-command batch", message, len(children))
				}
				messages := make(chan tea.Msg, len(children))
				for _, child := range children {
					child := child
					go func() {
						if child != nil {
							messages <- child()
						}
					}()
				}
				select {
				case <-ingestStarted:
				case <-time.After(2 * time.Second):
					t.Fatal("mounted local ingest did not reach its blocking progress boundary")
				}
				// The ingest command remains blocked. The other two messages are the
				// real progress and spinner ticks from Program's production batch.
				for range len(children) - 1 {
					select {
					case message := <-messages:
						program, _ = program.Update(message)
					case <-time.After(2 * time.Second):
						t.Fatal("mounted Program did not deliver its live progress ticks")
					}
				}
				if program.Phase() != kickstart.PhaseIngest {
					t.Fatalf("mounted Program phase=%s before ingest release, want ingest", program.Phase())
				}
				progressView := strings.ToLower(ansiPattern.ReplaceAllString(program.View(), ""))
				for _, want := range row.ProgressCopy {
					if !strings.Contains(progressView, strings.ToLower(want)) {
						t.Errorf("mounted live progress does not contain %q:\n%s", want, progressView)
					}
				}
				for _, forbidden := range row.ProgressForbidden {
					if strings.Contains(progressView, strings.ToLower(forbidden)) {
						t.Errorf("mounted live progress contains premature or unsupported copy %q:\n%s", forbidden, progressView)
					}
				}

				release()
				select {
				case message := <-messages:
					var next tea.Cmd
					program, next = program.Update(message)
					if next != nil {
						if _, quitEarly := next().(tea.QuitMsg); quitEarly {
							t.Fatal("mounted local completion quit before explicit exit")
						}
					}
				case <-time.After(2 * time.Second):
					t.Fatal("mounted local ingest did not complete after release")
				}
				completion := ansiPattern.ReplaceAllString(program.View(), "")
				assertMountedCompletionPreamble(t, completion, row)
				completionLower := strings.ToLower(completion)
				for _, want := range row.CompletionCopy {
					if !strings.Contains(completionLower, strings.ToLower(want)) {
						t.Errorf("mounted completion does not contain %q:\n%s", want, completion)
					}
				}
				for _, forbidden := range row.ForbiddenCopy {
					if strings.Contains(completionLower, strings.ToLower(forbidden)) {
						t.Errorf("mounted completion fabricates %q:\n%s", forbidden, completion)
					}
				}
				program, _ = program.Update(tea.WindowSizeMsg{Width: 181, Height: 51})
				persistentCompletion := ansiPattern.ReplaceAllString(program.View(), "")
				assertMountedCompletionPreamble(t, persistentCompletion, row)
				persistentCompletionLower := strings.ToLower(persistentCompletion)
				for _, want := range row.CompletionCopy {
					if !strings.Contains(persistentCompletionLower, strings.ToLower(want)) {
						t.Errorf("mounted completion did not persist %q after resize:\n%s", want, persistentCompletion)
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

func assertMountedCompletionPreamble(t *testing.T, view string, row mountedJourneyFixture) {
	t.Helper()
	if err := validateMountedRenderedCompletionPreamble(view, row.CompletionCopy[0]); err != nil {
		t.Fatalf("mounted completion preamble is invalid: %v\n%s", err, view)
	}
}

func validateMountedRenderedCompletionPreamble(view, firstCommand string) error {
	lines := strings.Split(view, "\n")
	previous := -1
	for _, expected := range mountedAcceptedCompletionPreambleLines() {
		at := mountedExactRenderedLineIndex(lines, expected)
		if at < 0 {
			return fmt.Errorf("mounted completion does not contain exact preamble line %q", expected)
		}
		if at <= previous {
			return fmt.Errorf("mounted completion preamble is out of order at %q", expected)
		}
		previous = at
	}
	firstCommandAt := mountedExactRenderedLineIndex(lines, firstCommand)
	if firstCommandAt < 0 || previous >= firstCommandAt {
		return fmt.Errorf("mounted completion preamble must precede its command list")
	}
	return nil
}

func mountedExactRenderedLineIndex(lines []string, want string) int {
	for index, line := range lines {
		if line == want {
			return index
		}
	}
	return -1
}

func drainMountedKickstartStartup(program kickstart.Program, command tea.Cmd) kickstart.Program {
	if command == nil {
		return program
	}
	message := command()
	commands, batched := message.(tea.BatchMsg)
	if !batched {
		commands = tea.BatchMsg{func() tea.Msg { return message }}
	}
	for _, child := range commands {
		if child == nil {
			continue
		}
		if message := child(); message != nil {
			program, _ = program.Update(message)
		}
	}
	return program
}
