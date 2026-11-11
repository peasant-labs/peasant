package kickstart

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

// ProgressSource is the narrow pull boundary between the concurrent ingest
// pipeline and kickstart's independently ticking presentation. Snapshot returns
// a point-in-time copy; Reset begins a retry on the same source without changing
// pipeline ownership or introducing a push/event channel.
type ProgressSource interface {
	Snapshot() map[ingest.Stage]ingest.StageProgress
	Reset()
}

var _ ProgressSource = (*ingest.ProgressState)(nil)

// Clock is the wall-clock boundary used for whole-attempt and observed-stage
// elapsed time. Production supplies the real clock and tests may supply a
// deterministic one.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function such as time.Now to Clock.
type ClockFunc func() time.Time

// Now implements Clock.
func (f ClockFunc) Now() time.Time { return f() }

var _ Clock = ClockFunc(nil)

// TickFunc is the injected Bubble Tea tick boundary used to poll ProgressSource
// without blocking ingest or tying deterministic tests to real sleeps.
type TickFunc func(time.Duration, func(time.Time) tea.Msg) tea.Cmd

// nextStepDetails is the private terminal display catalog for one completion
// action. The closed set of actions is onboarding.NextStep, shared with the web
// setup; this catalog only says how the terminal words each one, so titles,
// commands, and details cannot be paired with an incoherent step or a
// fabricated address.
type nextStepDetails struct {
	title   string
	command string
	detail  string
}

// NextStepsFunc chooses completion instructions from the completed local ingest
// result. Program resolves each step through one canonical display catalog; the
// provider has no free-form command text, command runner, publisher, or other
// execution authority.
type NextStepsFunc func(result *ftue.IngestResult) []onboarding.NextStep

// DefaultNextSteps returns the four honest, display-only actions available
// after a local kickstart run. In particular, it does not invent a dashboard
// address: peasant web start is the component that can report the real address
// after the server has successfully bound and become ready.
func DefaultNextSteps(_ *ftue.IngestResult) []onboarding.NextStep {
	return onboarding.NextStepsTUI()
}

func canonicalNextStep(step onboarding.NextStep) (nextStepDetails, bool) {
	switch step {
	case onboarding.NextStepConfig:
		return nextStepDetails{
			title:   "modify configuration interactively",
			command: "peasant config",
			detail:  "opens the settings editor without importing or publishing",
		}, true
	case onboarding.NextStepWebStart:
		return nextStepDetails{
			title:   "open the local dashboard",
			command: "peasant web start",
			detail:  "starts the dashboard and prints or opens its actual url",
		}, true
	case onboarding.NextStepVillageLogin:
		return nextStepDetails{
			title:   "connect to peasant online later",
			command: "peasant village login",
			detail:  "connects this machine without publishing a transcript",
		}, true
	case onboarding.NextStepVillagePush:
		return nextStepDetails{
			title:   "publish later, explicitly",
			command: "peasant village push",
			detail:  "starts a separate explicit publish flow",
		}, true
	default:
		return nextStepDetails{}, false
	}
}

func validateNextSteps(steps []onboarding.NextStep) error {
	if len(steps) == 0 {
		return nextStepsActionableError("the completion provider returned no actions")
	}
	seen := make(map[onboarding.NextStep]bool, len(steps))
	for index, step := range steps {
		if !step.IsValid() {
			return nextStepsActionableError(fmt.Sprintf("action %d has unknown step %q", index+1, string(step)))
		}
		if seen[step] {
			return nextStepsActionableError(fmt.Sprintf("action %d repeats %q", index+1, step))
		}
		if _, present := canonicalNextStep(step); !present {
			return nextStepsActionableError(fmt.Sprintf("action %d has no terminal display catalog entry for %q", index+1, step))
		}
		seen[step] = true
	}
	return nil
}

func nextStepsActionableError(reason string) error {
	return fmt.Errorf(
		"completion guidance unavailable.\n"+
			"what: kickstart could not validate its display-only next steps.\n"+
			"why: %s.\n"+
			"where: kickstart completion provider validation.\n"+
			"when: after local setup completed and before rendering follow-up commands.\n"+
			"means: no unverified or incoherent command guidance was shown; setup remains complete.\n"+
			"fix: use supported unique onboarding.NextStep values and rerun kickstart.", reason)
}
