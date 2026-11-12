package onboarding_test

import (
	_ "embed"
	"reflect"
	"testing"

	"github.com/peasant-labs/schema"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/next_steps.yaml
var nextStepsData []byte

type nextStepsDocument struct {
	RequiredNames []string `yaml:"requiredNames"`
	WebOnly       []string `yaml:"webOnly"`
	Unknown       []string `yaml:"unknown"`
	Cases         []struct {
		Name string   `yaml:"name"`
		Menu string   `yaml:"menu"`
		Want []string `yaml:"want"`
	} `yaml:"cases"`
}

func tokens(steps []onboarding.NextStep) []string {
	out := make([]string, len(steps))
	for index, step := range steps {
		out[index] = string(step)
	}
	return out
}

// TestNextStepMenusAreTheContractsSet holds each surface's menu to the fixture
// and the fixture to the contract: the two declarations of the set are equal
// both ways, the contract's whole menu is exactly what the web offers, and the
// terminal's four are that menu without the steps only the web offers.
func TestNextStepMenusAreTheContractsSet(t *testing.T) {
	t.Parallel()
	var document nextStepsDocument
	if err := testutil.DecodeNamedFixtureYAML(nextStepsData, &document); err != nil {
		t.Fatalf("decode testdata/next_steps.yaml: %v", err)
	}
	contract := make([]string, len(schema.AllLocalKickstartNextSteps))
	for index, step := range schema.AllLocalKickstartNextSteps {
		contract[index] = string(step)
	}

	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			var got []onboarding.NextStep
			switch testCase.Menu {
			case "terminal":
				got = onboarding.NextStepsTUI()
			case "web":
				got = onboarding.NextStepsWeb()
			default:
				t.Fatalf("case %q names the menu %q; the fixture knows terminal and web", testCase.Name, testCase.Menu)
			}
			if !reflect.DeepEqual(tokens(got), testCase.Want) {
				t.Fatalf("%s menu = %v, want %v", testCase.Menu, tokens(got), testCase.Want)
			}
		})
	}

	t.Run("the-two-declarations-of-the-set-are-equal-both-ways", func(t *testing.T) {
		t.Parallel()
		if got := tokens(onboarding.AllNextSteps); !reflect.DeepEqual(got, contract) {
			t.Fatalf("onboarding.AllNextSteps = %v, but the contract's menu is %v; a step was added or removed on one side only", got, contract)
		}
		for _, token := range contract {
			if _, err := onboarding.NewNextStep(token); err != nil {
				t.Errorf("onboarding refuses the contract's step %q: %v", token, err)
			}
		}
		for _, step := range onboarding.AllNextSteps {
			if _, err := schema.NewLocalKickstartNextStep(string(step)); err != nil {
				t.Errorf("the contract refuses onboarding's step %q: %v", step, err)
			}
		}
		for _, token := range document.Unknown {
			if _, err := onboarding.NewNextStep(token); err == nil {
				t.Errorf("onboarding accepts %q, which is no step", token)
			}
			if _, err := schema.NewLocalKickstartNextStep(token); err == nil {
				t.Errorf("the contract accepts %q, which is no step", token)
			}
		}
	})

	t.Run("the-contract-menu-is-what-the-web-offers", func(t *testing.T) {
		t.Parallel()
		if got := tokens(onboarding.NextStepsWeb()); !reflect.DeepEqual(got, contract) {
			t.Fatalf("the web offers %v, but the contract's finish menu is %v; decide where a new step is offered", got, contract)
		}
	})

	t.Run("the-terminal-four-are-the-contract-menu-without-the-web-only-steps", func(t *testing.T) {
		t.Parallel()
		webOnly := map[string]bool{}
		for _, token := range document.WebOnly {
			webOnly[token] = true
		}
		var terminal []string
		for _, token := range contract {
			if !webOnly[token] {
				terminal = append(terminal, token)
			}
		}
		if got := tokens(onboarding.NextStepsTUI()); !reflect.DeepEqual(got, terminal) {
			t.Fatalf("the terminal offers %v, but the contract's menu without the web-only steps %v is %v", got, document.WebOnly, terminal)
		}
	})
}
