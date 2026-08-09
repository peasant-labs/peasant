package kickstart

import (
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

// TickFunc is the injected Bubble Tea tick boundary used to poll ProgressSource
// without blocking ingest or tying deterministic tests to real sleeps.
type TickFunc func(time.Duration, func(time.Time) tea.Msg) tea.Cmd

// NextStepKind is the closed set of display-only actions kickstart may show on
// successful completion. A next step carries no executable callback.
type NextStepKind int

const (
	NextStepUnknown NextStepKind = iota
	NextStepWebStart
	NextStepVillageLogin
	NextStepVillagePush
)

// IsValid reports whether k identifies one of the completion actions.
func (k NextStepKind) IsValid() bool {
	switch k {
	case NextStepWebStart, NextStepVillageLogin, NextStepVillagePush:
		return true
	default:
		return false
	}
}

// String returns the stable lower-case identity for k.
func (k NextStepKind) String() string {
	switch k {
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

// NextStep is one display-only completion instruction. Command is rendered as
// text and is never executed by Program.
type NextStep struct {
	Kind    NextStepKind
	Title   string
	Command string
	Detail  string
}

// NextStepsFunc derives completion instructions from the completed local ingest
// result. It has no command runner, publisher, or other execution authority.
type NextStepsFunc func(result *ftue.IngestResult) []NextStep
