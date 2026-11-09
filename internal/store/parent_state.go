package store

import "fmt"

// ParentState is the store-only projection of the ONE parent lane on a session
// row: it says where the session sits under its logical parent, never which
// cached row a join can use. It is not a wire field; the page and the reader
// derive their answers from it.
//
// The state is trigger-maintained (migration V70) from the same evidence the
// parent-answer readers render, so the column and those readers cannot
// disagree. No write site sets it.
type ParentState string

const (
	// ParentStateRoot is a session whose evidence names no logical parent.
	ParentStateRoot ParentState = "root"
	// ParentStateResolved is a session with a logical parent. The parent may
	// name a session the store does not hold: an admitted child still resolves.
	ParentStateResolved ParentState = "resolved"
	// ParentStateUnresolved is a known spawned child whose parent cannot be
	// named. It is neither a root nor a resolved child of a named parent, and
	// it takes precedence over a parent_id cache fallback.
	ParentStateUnresolved ParentState = "unresolved"
)

// AllParentStates is the exact closed set, in the order the migration CHECK
// lists it.
var AllParentStates = []ParentState{
	ParentStateRoot,
	ParentStateResolved,
	ParentStateUnresolved,
}

// IsValid reports whether the value is a member of AllParentStates.
func (p ParentState) IsValid() bool {
	switch p {
	case ParentStateRoot, ParentStateResolved, ParentStateUnresolved:
		return true
	default:
		return false
	}
}

// NewParentState validates a raw state at an input boundary. The stored CHECK
// and this constructor carry the same closed set; an unknown value fails here
// instead of reaching a reader that would have to guess.
func NewParentState(raw string) (ParentState, error) {
	value := ParentState(raw)
	if !value.IsValid() {
		return "", fmt.Errorf("store.NewParentState: value %q is outside the closed parent-state set %v; a session's parent lane is the trigger-maintained projection of the logical-parent rule; use one of the three states", raw, AllParentStates)
	}
	return value, nil
}

func (p ParentState) String() string { return string(p) }
