package onboarding

import (
	"fmt"
	"slices"
)

// NextStep is one display-only action offered after setup. It carries no
// callback and installs nothing.
//
// The web sends its finish menu over the wire as schema.LocalKickstartNextStep,
// so this set and the contract's are two declarations of the same tokens.
// testdata/next_steps.yaml and its test hold them equal both ways, and hold each
// surface's menu to the contract's: the web offers the contract's whole menu and
// the terminal the four it has copy for.
type NextStep string

const (
	NextStepUnknown           NextStep = ""
	NextStepConfig            NextStep = "config"
	NextStepWebStart          NextStep = "web-start"
	NextStepVillageLogin      NextStep = "village-login"
	NextStepVillagePush       NextStep = "village-push"
	NextStepAutoPublishConfig NextStep = "auto-publish-settings"
)

// AllNextSteps is the closed set.
var AllNextSteps = []NextStep{
	NextStepConfig,
	NextStepWebStart,
	NextStepVillageLogin,
	NextStepVillagePush,
	NextStepAutoPublishConfig,
}

// NewNextStep refuses an unknown token.
func NewNextStep(s string) (NextStep, error) {
	step := NextStep(s)
	if !step.IsValid() {
		return NextStepUnknown, fmt.Errorf(
			"read next step %q.\n"+
				"what: %q is not a completion action.\n"+
				"why: the closed set is config, web-start, village-login, village-push, and auto-publish-settings.\n"+
				"where: onboarding.NewNextStep.\n"+
				"when: a caller named a completion action.\n"+
				"means: the action was not accepted.\n"+
				"fix: use one of the closed next-step tokens.",
			s, s)
	}
	return step, nil
}

// IsValid reports whether s is a completion action.
func (s NextStep) IsValid() bool { return slices.Contains(AllNextSteps, s) }

// String returns the stable token, or "unknown".
func (s NextStep) String() string {
	if s.IsValid() {
		return string(s)
	}
	return "unknown"
}

// NextStepsTUI is the four actions the terminal completion screen shows, in order.
// It never includes auto-publish-settings.
func NextStepsTUI() []NextStep {
	return []NextStep{
		NextStepConfig,
		NextStepWebStart,
		NextStepVillageLogin,
		NextStepVillagePush,
	}
}

// NextStepsWeb is the terminal four plus the settings section where an
// auto-publish rule is chosen. The link is an offer, not an install.
func NextStepsWeb() []NextStep {
	return append(NextStepsTUI(), NextStepAutoPublishConfig)
}
