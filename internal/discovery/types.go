package discovery

import (
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
)

// SessionListing is owned by onboarding.
type SessionListing = onboarding.SessionListing

// StageProgress reports progress for a single pipeline stage.
type StageProgress struct {
	Done   int
	Total  int
	Ended  bool
	HasErr bool
}

// ProviderDiscovery carries the inventory and configured state for one harness.
type ProviderDiscovery struct {
	SessionCount int
	Enabled      bool
	State        DiscoveryState
	Detail       string
}

// DiscoveryState distinguishes an honest empty scan from a harness that could
// not be inspected. The zero value is ready for callers that construct
// inventory entries directly.
type DiscoveryState string

const (
	DiscoveryReady       DiscoveryState = ""
	DiscoveryUnavailable DiscoveryState = "unavailable"
	DiscoveryFailed      DiscoveryState = "failed"
)

// IsOperational reports that the harness was inspected.
func (s DiscoveryState) IsOperational() bool { return s == DiscoveryReady }

// ProviderInventory is the typed discovery result consumed by setup.
type ProviderInventory map[defaults.Harness]ProviderDiscovery

// SubagentRelation is the parent-to-subagent relation over the full discovered
// set, including sessions the picker never lists. It resolves a parent row's
// child count. Nothing named in the relation is rendered, becomes a selectable
// row, or reaches ingest.
//
// A key is present for every session discovery surfaced, with an empty value
// when that session spawned nothing. A lookup miss means discovery never
// surfaced that session, so a bare identifier cannot invent a child.
type SubagentRelation map[string][]string

// ProjectIdentity is owned by onboarding.
type ProjectIdentity = onboarding.ProjectIdentity

// PreparedSessionListing is owned by onboarding.
type PreparedSessionListing = onboarding.PreparedSessionListing

// KnownSession is owned by ingest, so the import filter reads the same record.
type KnownSession = ingest.KnownSession

// KnownSessionIndex is owned by ingest, so the tree and the import filter read
// one index.
type KnownSessionIndex = ingest.KnownSessionIndex

// ScanProgress receives phase and count updates while a scan runs. A nil
// progress value is ignored. Implementations must tolerate a nil receiver.
type ScanProgress interface {
	SetPhase(phase string)
	SetProgress(done, total int)
}
