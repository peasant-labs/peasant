package kickstart

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/ingest"
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

// NextStepKind is the closed set of display-only actions kickstart may show on
// successful completion. A next step carries no executable callback.
type NextStepKind int

const (
	NextStepUnknown NextStepKind = iota
	NextStepConfig
	NextStepWebStart
	NextStepVillageLogin
	NextStepVillagePush
)

// IsValid reports whether k identifies one of the completion actions.
func (k NextStepKind) IsValid() bool {
	switch k {
	case NextStepConfig, NextStepWebStart, NextStepVillageLogin, NextStepVillagePush:
		return true
	default:
		return false
	}
}

// String returns the stable lower-case identity for k.
func (k NextStepKind) String() string {
	switch k {
	case NextStepConfig:
		return "config"
	case NextStepWebStart:
		return "web-start"
	case NextStepVillageLogin:
		return "village-login"
	case NextStepVillagePush:
		return "village-push"
	default:
		return "unknown"
	}
}

// nextStepDetails is the private display catalog for one typed completion
// action. Providers return only NextStepKind, so titles, commands, and details
// cannot be paired with an incoherent kind or a fabricated address.
type nextStepDetails struct {
	title   string
	command string
	detail  string
}

// NextStepsFunc chooses typed completion instructions from the completed local
// ingest result. Program resolves each kind through one canonical display
// catalog; the provider has no free-form command text, command runner,
// publisher, or other execution authority.
type NextStepsFunc func(result *ftue.IngestResult) []NextStepKind

// DefaultNextSteps returns the four honest, display-only actions available
// after a local kickstart run. In particular, it does not invent a dashboard
// address: peasant web start is the component that can report the real address
// after the server has successfully bound and become ready.
func DefaultNextSteps(_ *ftue.IngestResult) []NextStepKind {
	return []NextStepKind{
		NextStepConfig,
		NextStepWebStart,
		NextStepVillageLogin,
		NextStepVillagePush,
	}
}

func canonicalNextStep(kind NextStepKind) (nextStepDetails, bool) {
	switch kind {
	case NextStepConfig:
		return nextStepDetails{
			title:   "modify configuration interactively",
			command: "peasant config",
			detail:  "opens the settings editor without importing or publishing",
		}, true
	case NextStepWebStart:
		return nextStepDetails{
			title:   "open the local dashboard",
			command: "peasant web start",
			detail:  "starts the dashboard and prints or opens its actual url",
		}, true
	case NextStepVillageLogin:
		return nextStepDetails{
			title:   "connect to a village later",
			command: "peasant village login",
			detail:  "connects this machine without publishing a transcript",
		}, true
	case NextStepVillagePush:
		return nextStepDetails{
			title:   "publish later, explicitly",
			command: "peasant village push",
			detail:  "starts a separate explicit publish flow",
		}, true
	default:
		return nextStepDetails{}, false
	}
}

func validateNextSteps(kinds []NextStepKind) error {
	if len(kinds) == 0 {
		return nextStepsActionableError("the completion provider returned no actions")
	}
	seen := make(map[NextStepKind]bool, len(kinds))
	for index, kind := range kinds {
		if !kind.IsValid() {
			return nextStepsActionableError(fmt.Sprintf("action %d has unknown kind %d", index+1, kind))
		}
		if seen[kind] {
			return nextStepsActionableError(fmt.Sprintf("action %d repeats %q", index+1, kind))
		}
		if _, present := canonicalNextStep(kind); !present {
			return nextStepsActionableError(fmt.Sprintf("action %d has no canonical display catalog entry for %q", index+1, kind))
		}
		seen[kind] = true
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
			"fix: use supported unique NextStepKind values and rerun kickstart.", reason)
}

// stageObservation is presentation-only state derived from successive progress
// snapshots. It deliberately does not feed back into the ingest pipeline.
type stageObservation struct {
	startedAt time.Time
	lastAt    time.Time
	lastDone  int
	lastTotal int
	progress  ingest.StageProgress

	estimateEligible bool
	estimateValid    bool
	estimate         time.Duration
}
