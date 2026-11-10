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

// SelectionDeriver turns answered node ids into the saved selection. The
// selection model installs one. Until it is installed, an id-based write fails
// closed and a mapping answer still writes mode and harnesses directly.
type SelectionDeriver interface {
	DeriveSelection(forest Forest, ids []string, autoIngest bool, residual Residual) (config.SelectionConfig, error)
}

type unsetSelectionDeriver struct{}

func (unsetSelectionDeriver) DeriveSelection(Forest, []string, bool, Residual) (config.SelectionConfig, error) {
	return config.SelectionConfig{}, fmt.Errorf(
		"derive a saved selection from answered node ids.\n" +
			"what: the selection deriver is not installed.\n" +
			"why: node-id answers are converted by the selection model, and that model is not registered yet.\n" +
			"where: onboarding selection field write.\n" +
			"when: an answer named node ids instead of a selection mapping.\n" +
			"means: the selection was not written and no other field was changed by this write.\n" +
			"fix: install the selection deriver with UseSelectionDeriver before applying an id-based selection answer.")
}

var selectionDeriver SelectionDeriver = unsetSelectionDeriver{}

// UseSelectionDeriver installs the selection model. A nil deriver restores the
// fail-closed default. Mapping answers do not use it.
func UseSelectionDeriver(deriver SelectionDeriver) {
	if deriver == nil {
		selectionDeriver = unsetSelectionDeriver{}
		return
	}
	selectionDeriver = deriver
}

// DeriveSelection converts answered node ids. It fails closed until the
// selection model is installed.
func DeriveSelection(forest Forest, ids []string, autoIngest bool, residual Residual) (config.SelectionConfig, error) {
	return selectionDeriver.DeriveSelection(forest, ids, autoIngest, residual)
}
