package kickstart_test

import (
	_ "embed"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

const expectedNextStepRows = 4

type nextStepFixtureKind string

const (
	fixtureNextStepUnknown      nextStepFixtureKind = "unknown"
	fixtureNextStepConfig       nextStepFixtureKind = "config"
	fixtureNextStepWebStart     nextStepFixtureKind = "web-start"
	fixtureNextStepVillageLogin nextStepFixtureKind = "village-login"
	fixtureNextStepVillagePush  nextStepFixtureKind = "village-push"
)

func (k nextStepFixtureKind) programKind() (kickstart.NextStepKind, bool) {
	switch k {
	case fixtureNextStepUnknown:
		return kickstart.NextStepUnknown, true
	case fixtureNextStepConfig:
		return kickstart.NextStepConfig, true
	case fixtureNextStepWebStart:
		return kickstart.NextStepWebStart, true
	case fixtureNextStepVillageLogin:
		return kickstart.NextStepVillageLogin, true
	case fixtureNextStepVillagePush:
		return kickstart.NextStepVillagePush, true
	default:
		return kickstart.NextStepUnknown, false
	}
}

type nextStepFixture struct {
	Name         string                `yaml:"name"`
	Valid        bool                  `yaml:"valid"`
	Kinds        []nextStepFixtureKind `yaml:"kinds"`
	WantPreamble []string              `yaml:"wantPreamble"`
	WantContains []string              `yaml:"wantContains"`
	WantMissing  []string              `yaml:"wantMissing"`
}

type nextStepDocument struct {
	ExpectedRowCount int               `yaml:"expectedRowCount"`
	Rows             []nextStepFixture `yaml:"rows"`
}

//go:embed testdata/guided/next_steps.yaml
var nextStepData []byte

func loadNextStepDocument(t *testing.T) nextStepDocument {
	t.Helper()
	var document nextStepDocument
	decodeSingleKnownFieldsDocument(t, "testdata/guided/next_steps.yaml", nextStepData, &document)
	if document.ExpectedRowCount != expectedNextStepRows || len(document.Rows) != expectedNextStepRows {
		t.Fatalf("next-step rows: declared=%d actual=%d required=%d",
			document.ExpectedRowCount, len(document.Rows), expectedNextStepRows)
	}
	seen := map[string]bool{}
	for _, row := range document.Rows {
		if strings.TrimSpace(row.Name) == "" || seen[row.Name] || len(row.WantContains) == 0 {
			t.Fatalf("next-step row is incomplete or duplicated: %#v", row)
		}
		seen[row.Name] = true
		for _, kind := range row.Kinds {
			if _, valid := kind.programKind(); !valid {
				t.Fatalf("next-step row %q has unsupported fixture kind %q", row.Name, kind)
			}
		}
		if row.Valid {
			if err := validateCompletionPreamble(row.WantPreamble); err != nil {
				t.Fatalf("next-step row %q has invalid completion preamble: %v", row.Name, err)
			}
		} else if len(row.WantPreamble) != 0 {
			t.Fatalf("invalid next-step row %q expects a preamble without a command list", row.Name)
		}
	}
	return document
}

func TestProgramValidatesTypedNextStepProvider(t *testing.T) {
	providerType := reflect.TypeOf(kickstart.NextStepsFunc(nil))
	if providerType.NumOut() != 1 || providerType.Out(0) != reflect.TypeOf([]kickstart.NextStepKind{}) {
		t.Fatalf("NextStepsFunc output = %v, want []kickstart.NextStepKind", providerType.Out(0))
	}

	for _, row := range loadNextStepDocument(t).Rows {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			kinds := make([]kickstart.NextStepKind, 0, len(row.Kinds))
			for _, fixtureKind := range row.Kinds {
				kind, _ := fixtureKind.programKind()
				kinds = append(kinds, kind)
			}
			program, _ := newTestProgram(t, kickstart.ProgramDeps{
				AlreadyConnected: true,
				NextSteps: func(*ftue.IngestResult) []kickstart.NextStepKind {
					return append([]kickstart.NextStepKind(nil), kinds...)
				},
			})
			program = drainProgram(program, program.Init())
			program, _ = advanceToCommit(program)
			rendered := stripRender(program.View())
			if row.Valid {
				assertCompletionPreamble(t, rendered)
			}
			view := strings.ToLower(rendered)
			for _, want := range row.WantContains {
				if !strings.Contains(view, strings.ToLower(want)) {
					t.Errorf("completion does not contain %q:\n%s", want, view)
				}
			}
			for _, forbidden := range row.WantMissing {
				if strings.Contains(view, strings.ToLower(forbidden)) {
					t.Errorf("completion contains forbidden guidance %q:\n%s", forbidden, view)
				}
			}
			if gotValid := program.NextStepsErr() == nil; gotValid != row.Valid {
				t.Errorf("next-step error=%v does not match fixture outcome", program.NextStepsErr())
			}
		})
	}
}
