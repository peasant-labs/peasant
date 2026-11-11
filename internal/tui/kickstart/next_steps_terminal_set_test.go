package kickstart_test

import (
	_ "embed"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

type nextStepTerminalSetCase struct {
	Name        string   `yaml:"name"`
	Steps       []string `yaml:"steps"`
	WantError   []string `yaml:"wantError"`
	WantMissing []string `yaml:"wantMissing"`
}

type nextStepTerminalSetDocument struct {
	RequiredNames []string                  `yaml:"requiredNames"`
	Cases         []nextStepTerminalSetCase `yaml:"cases"`
}

//go:embed testdata/guided/next_steps_terminal_set.yaml
var nextStepTerminalSetData []byte

// TestProgramRefusesCompletionStepsTheTerminalHasNoCopyFor drives the real
// completion screen with steps the terminal does not word. The closed set is the
// setup model's, so a provider can name a step the web setup alone offers or a
// token outside the set; both must stop at an actionable message that names the
// step, never render a step without its command.
func TestProgramRefusesCompletionStepsTheTerminalHasNoCopyFor(t *testing.T) {
	t.Parallel()
	var document nextStepTerminalSetDocument
	if err := testutil.DecodeNamedFixtureYAML(nextStepTerminalSetData, &document); err != nil {
		t.Fatalf("decode testdata/guided/next_steps_terminal_set.yaml: %v", err)
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			// A raw conversion, not onboarding.NewNextStep: the case injects the
			// token the constructor would refuse, to reach the validation behind it.
			steps := make([]onboarding.NextStep, 0, len(row.Steps))
			for _, token := range row.Steps {
				steps = append(steps, onboarding.NextStep(token))
			}
			program, _ := newTestProgram(t, kickstart.ProgramDeps{
				AlreadyConnected: true,
				NextSteps: func(*ftue.IngestResult) []onboarding.NextStep {
					return append([]onboarding.NextStep(nil), steps...)
				},
			})
			program = drainProgram(program, program.Init())
			program, _ = advanceToCommit(program)

			err := program.NextStepsErr()
			if err == nil {
				t.Fatalf("steps %v were accepted; the terminal has no copy for all of them", row.Steps)
			}
			for _, want := range row.WantError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not contain %q:\n%s", want, err)
				}
			}
			view := strings.ToLower(stripRender(program.View()))
			for _, forbidden := range row.WantMissing {
				if strings.Contains(view, strings.ToLower(forbidden)) {
					t.Errorf("the completion screen shows %q beside a refused step list:\n%s", forbidden, view)
				}
			}
		})
	}
}

// TestDefaultNextStepsAreTheSharedTerminalSet holds the default provider to the
// setup model's terminal set and proves the screen words every step in it, so a
// step added to the shared set without terminal copy stops here rather than at
// the end of a user's first run.
func TestDefaultNextStepsAreTheSharedTerminalSet(t *testing.T) {
	t.Parallel()
	shared := onboarding.NextStepsTUI()
	if got := kickstart.DefaultNextSteps(nil); !reflect.DeepEqual(got, shared) {
		t.Fatalf("DefaultNextSteps = %v, want the shared terminal set %v", got, shared)
	}
	program, _ := newTestProgram(t, kickstart.ProgramDeps{AlreadyConnected: true})
	program = drainProgram(program, program.Init())
	program, _ = advanceToCommit(program)
	if err := program.NextStepsErr(); err != nil {
		t.Fatalf("the shared terminal set %v does not render on the completion screen: %v", shared, err)
	}
}
