package kickstart_test

import (
	"context"
	_ "embed"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

//go:embed testdata/guided/ingest_diagnostics.yaml
var ingestDiagnosticsYAML []byte

type ingestDiagnosticsDocument struct {
	RequiredNames []string `yaml:"requiredNames"`
	Viewports     []struct {
		Width  int `yaml:"width"`
		Height int `yaml:"height"`
	} `yaml:"viewports"`
	Cases []struct {
		Name          string                   `yaml:"name"`
		Errors        int                      `yaml:"errors"`
		Diagnostics   []ingest.DiagnosticEntry `yaml:"diagnostics"`
		WantText      []string                 `yaml:"wantText"`
		WantAbsent    []string                 `yaml:"wantAbsent"`
		WantRawAbsent []string                 `yaml:"wantRawAbsent"`
	} `yaml:"cases"`
}

func loadIngestDiagnosticsFixtures(t *testing.T) ingestDiagnosticsDocument {
	t.Helper()
	var document ingestDiagnosticsDocument
	decodeSingleKnownFieldsDocument(t, "testdata/guided/ingest_diagnostics.yaml", ingestDiagnosticsYAML, &document)
	required := []string{"single-warning", "multiple-long-warnings", "terminal-controls", "errors-remain-distinct", "no-warning"}
	if !reflect.DeepEqual(document.RequiredNames, required) {
		t.Fatal("diagnostic required-name manifest changed")
	}
	seen := map[string]bool{}
	for _, row := range document.Cases {
		if row.Name == "" || seen[row.Name] || len(row.WantText) == 0 {
			t.Fatalf("invalid diagnostic fixture %q", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing diagnostic fixture %q", name)
		}
	}
	sizes := map[string]bool{}
	for _, size := range document.Viewports {
		name := fmt.Sprintf("%dx%d", size.Width, size.Height)
		if size.Width <= 0 || size.Height <= 0 || sizes[name] {
			t.Fatalf("invalid diagnostic viewport %q", name)
		}
		sizes[name] = true
	}
	if !sizes["80x24"] || !sizes["120x40"] {
		t.Fatal("diagnostic fixtures must retain 80x24 and 120x40 viewports")
	}
	return document
}

func TestProgramKeepsIngestDiagnosticsReadableAndNonfatal(t *testing.T) {
	fixtures := loadIngestDiagnosticsFixtures(t)
	for _, row := range fixtures.Cases {
		for _, size := range fixtures.Viewports {
			t.Run(fmt.Sprintf("%s/%dx%d", row.Name, size.Width, size.Height), func(t *testing.T) {
				result := &ftue.IngestResult{New: 2, Updated: 1, Unchanged: 3, Errors: row.Errors, Diagnostics: row.Diagnostics}
				calls := 0
				program, _ := newTestProgram(t, kickstart.ProgramDeps{AlreadyConnected: true, Ingest: func(context.Context) (*ftue.IngestResult, error) { calls++; return result, nil }})
				program.SetSize(size.Width, size.Height)
				program = drainProgram(program, program.Init())
				program, command := advanceToCommit(program)
				program = drainCmds(t, program, command)
				if program.Phase() != kickstart.PhaseDone || program.IngestErr() != nil || !reflect.DeepEqual(program.IngestResult(), result) {
					t.Fatal("warning changed result or completion")
				}
				var seen strings.Builder
				initialView := program.View()
				for i := 0; i < 160; i++ {
					raw := program.View()
					for _, absent := range row.WantRawAbsent {
						if strings.Contains(raw, absent) {
							t.Fatalf("source control reached terminal: %q", absent)
						}
					}
					view := ansi.Strip(raw)
					if len(row.Diagnostics) > 0 {
						if len(strings.Split(view, "\n")) > size.Height {
							t.Fatalf("completion exceeds terminal height:\n%s", view)
						}
						if !strings.Contains(view, "quit") {
							t.Fatalf("completion lost exit footer:\n%s", view)
						}
						for _, line := range strings.Split(view, "\n") {
							if ansi.StringWidth(line) > size.Width {
								t.Fatalf("completion exceeds width: %q", line)
							}
						}
					}
					seen.WriteString(view)
					seen.WriteByte('\n')
					program, _ = program.Update(press(tea.KeyDown))
				}
				view := strings.Join(strings.Fields(seen.String()), " ")
				for _, want := range row.WantText {
					if !strings.Contains(view, want) {
						t.Errorf("completion omitted %q", want)
					}
				}
				for _, absent := range row.WantAbsent {
					if strings.Contains(view, absent) {
						t.Errorf("completion contains unsafe/false text %q", absent)
					}
				}
				if len(row.Diagnostics) > 0 {
					program, _ = program.Update(press('g'))
					if program.View() != initialView {
						t.Fatal("go-to-top did not restore initial completion")
					}
					program, _ = program.Update(press(tea.KeyPgDown))
					paged := program.View()
					program, _ = program.Update(press(tea.KeyPgUp))
					if program.View() != initialView {
						t.Fatal("page-up did not restore initial completion")
					}
					if row.Name == "multiple-long-warnings" && paged == initialView {
						t.Fatal("page-down did not reach overflow")
					}
					program, _ = program.Update(tea.KeyPressMsg{Code: 'G', Text: "G"})
					if !strings.Contains(ansi.Strip(program.View()), "peasant village push") {
						t.Fatal("go-to-bottom lost next steps")
					}
					program, _ = program.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
					program, _ = program.Update(tea.WindowSizeMsg{Width: size.Width, Height: size.Height})
					program, _ = program.Update(press('g'))
					if program.View() != initialView {
						t.Fatal("resize lost readable completion")
					}
				}
				program, command = program.Update(press(tea.KeyEnter))
				if command != nil || calls != 1 {
					t.Fatal("nonfatal warning triggered automatic retry")
				}
				_, command = program.Update(press('q'))
				if command == nil {
					t.Fatal("completion can no longer quit")
				}
			})
		}
	}
}
