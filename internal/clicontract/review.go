package clicontract

import (
	"fmt"
	"slices"
	"strings"
)

// ReviewStatus is the outcome `sessions review` reports for one session. It is
// a closed set, shared by the command that prints it and the reader that
// filters on it.
type ReviewStatus string

const (
	// ReviewClean is a session reviewed in full with nothing found.
	ReviewClean ReviewStatus = "clean"
	// ReviewFindings is a session reviewed in full with residue found.
	ReviewFindings ReviewStatus = "findings"
	// ReviewError is a session that could not be reviewed.
	ReviewError ReviewStatus = "error"
)

// AllReviewStatuses is the closed set of review statuses.
var AllReviewStatuses = []ReviewStatus{ReviewClean, ReviewFindings, ReviewError}

// NewReviewStatus validates raw against the closed set.
func NewReviewStatus(raw string) (ReviewStatus, error) {
	status := ReviewStatus(raw)
	if !slices.Contains(AllReviewStatuses, status) {
		return "", fmt.Errorf("unknown review status %q (known: %s)", raw, joinCodes(AllReviewStatuses))
	}
	return status, nil
}

// DroppedSession is a session a review document named and the reader left out.
type DroppedSession struct {
	ID     string
	Status ReviewStatus
}

// SessionList is what a --sessions-from input selected: the ids to act on, and
// the sessions it named that must not be acted on.
type SessionList struct {
	IDs     []string
	Dropped []DroppedSession
}

// Warnings returns one session_refused warning per dropped session.
func (l SessionList) Warnings() []Warning {
	warnings := make([]Warning, 0, len(l.Dropped))
	for _, dropped := range l.Dropped {
		warnings = append(warnings, Warning{
			Code:      WarningSessionRefused,
			SessionID: dropped.ID,
			Message:   fmt.Sprintf("left out of this run: its review status is %s, and only a clean session passes through a review document", dropped.Status),
		})
	}
	return warnings
}

// NoCleanSessionsError is a review document with no clean session. It is also
// ErrNoSessionIDs: the list selects nothing and never widens.
type NoCleanSessionsError struct {
	Dropped []DroppedSession
}

// Error implements error.
func (e *NoCleanSessionsError) Error() string {
	return "the review document names no clean session: " + describeDropped(e.Dropped)
}

// Is makes the error match ErrNoSessionIDs.
func (e *NoCleanSessionsError) Is(target error) bool {
	return target == ErrNoSessionIDs
}

func describeDropped(dropped []DroppedSession) string {
	parts := make([]string, len(dropped))
	for i, session := range dropped {
		parts[i] = session.ID + " (" + string(session.Status) + ")"
	}
	return strings.Join(parts, ", ")
}

// parsedSession is one input element. Status is empty for every shape that
// carries none.
type parsedSession struct {
	ID     string
	Status ReviewStatus
}
