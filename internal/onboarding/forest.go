package onboarding

import (
	"fmt"
	"slices"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// ScanState is the discovery scan's progress. The tree validator accepts only done.
type ScanState uint8

const (
	ScanUnknown ScanState = iota
	ScanIdle
	ScanRunning
	ScanDone
	ScanError
)

// AllScanStates is the closed set, excluding the unknown zero value.
var AllScanStates = []ScanState{ScanIdle, ScanRunning, ScanDone, ScanError}

// NewScanState accepts idle, running, done, or error.
func NewScanState(s string) (ScanState, error) {
	for _, state := range AllScanStates {
		if state.String() == s {
			return state, nil
		}
	}
	return ScanUnknown, fmt.Errorf(
		"read scan state %q.\n"+
			"what: %q is not a scan state.\n"+
			"why: the closed set is idle, running, done, and error.\n"+
			"where: onboarding.NewScanState.\n"+
			"when: a caller named the discovery scan.\n"+
			"means: the scan state was not accepted.\n"+
			"fix: use idle, running, done, or error.",
		s, s)
}

// IsValid reports whether s is a real scan state.
func (s ScanState) IsValid() bool { return slices.Contains(AllScanStates, s) }

// String returns the wire token.
func (s ScanState) String() string {
	switch s {
	case ScanIdle:
		return "idle"
	case ScanRunning:
		return "running"
	case ScanDone:
		return "done"
	case ScanError:
		return "error"
	default:
		return "unknown"
	}
}

// NodeKind is the closed set of forest rows.
type NodeKind string

const (
	NodeKindUnknown NodeKind = ""
	NodeKindProject NodeKind = "project"
	NodeKindGroup   NodeKind = "group"
	NodeKindBranch  NodeKind = "branch"
	NodeKindSession NodeKind = "session"
)

// AllNodeKinds is the closed set.
var AllNodeKinds = []NodeKind{NodeKindProject, NodeKindGroup, NodeKindBranch, NodeKindSession}

// NewNodeKind refuses an unknown token.
func NewNodeKind(s string) (NodeKind, error) {
	kind := NodeKind(s)
	if !kind.IsValid() {
		return NodeKindUnknown, fmt.Errorf(
			"read forest node kind %q.\n"+
				"what: %q is not a node kind.\n"+
				"why: the closed set is project, group, branch, and session.\n"+
				"where: onboarding.NewNodeKind.\n"+
				"when: a caller named a forest row.\n"+
				"means: the kind was not accepted.\n"+
				"fix: use project, group, branch, or session.",
			s, s)
	}
	return kind, nil
}

// IsValid reports whether k is a real node kind.
func (k NodeKind) IsValid() bool { return slices.Contains(AllNodeKinds, k) }

// String returns the wire token, or "unknown".
func (k NodeKind) String() string {
	if k.IsValid() {
		return string(k)
	}
	return "unknown"
}

// NodeState is the component tri-state. Locked is a separate boolean, not a fourth state.
type NodeState string

const (
	NodeStateUnknown NodeState = ""
	NodeStateNone    NodeState = "none"
	NodeStateSome    NodeState = "some"
	NodeStateAll     NodeState = "all"
)

// AllNodeStates is the closed set.
var AllNodeStates = []NodeState{NodeStateNone, NodeStateSome, NodeStateAll}

// NewNodeState refuses an unknown token.
func NewNodeState(s string) (NodeState, error) {
	state := NodeState(s)
	if !state.IsValid() {
		return NodeStateUnknown, fmt.Errorf(
			"read forest node state %q.\n"+
				"what: %q is not a node state.\n"+
				"why: the closed set is none, some, and all. A locked row is a boolean, not a fourth state.\n"+
				"where: onboarding.NewNodeState.\n"+
				"when: a caller named a row's check state.\n"+
				"means: the state was not accepted.\n"+
				"fix: use none, some, or all.",
			s, s)
	}
	return state, nil
}

// IsValid reports whether s is none, some, or all.
func (s NodeState) IsValid() bool { return slices.Contains(AllNodeStates, s) }

// String returns the wire token, or "unknown".
func (s NodeState) String() string {
	if s.IsValid() {
		return string(s)
	}
	return "unknown"
}

// Labels are display-only annotations. They are not identity.
type Labels struct {
	Project  string
	Location string
}

// Node is one row of the selection forest. Candidate is the identity
// ingest.SelectionMatcher consumes, so a later restore does not parse identity
// from the label.
type Node struct {
	ID         string
	ParentID   string
	Kind       NodeKind
	Label      string
	Labels     Labels
	Harness    defaults.Harness
	SessionID  string
	ChildCount int
	Candidate  ingest.DiscoveryCandidate
	State      NodeState
	Locked     bool
	// ExplicitBranch is editor provenance: a saved rule named branches, so a
	// later derive must not widen those branches to every branch.
	ExplicitBranch bool
	// ExplicitSession is editor provenance: this session is selected only by
	// its saved id, so a later derive must not widen it into a branch or project.
	ExplicitSession bool
	// Identity is the opaque project identity used to prove remote and name
	// uniqueness. It is not a display label and is never persisted.
	Identity string
}

// Forest is the whole discovered tree. There is no cap and no cursor.
type Forest struct {
	Nodes []Node
	Total int
}

// SelectedForest is the answered set of node ids. It is not a SelectionConfig.
type SelectedForest struct {
	IDs []string
}

// Residual is the part of a saved selection the current scan cannot show.
type Residual struct {
	Harnesses map[string]config.SelectionHarnessConfig
	Branches  []ResidualBranch
}

// ResidualBranch is one saved branch policy the current scan cannot show.
// Nil Branches means every branch, so a merge adds nothing for that row.
type ResidualBranch struct {
	Harness  string
	Project  config.ProjectSelection
	Branches []string
}
