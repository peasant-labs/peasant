package discovery

import (
	"time"

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

// KnownSession is what a previous ingest recorded for one session. A scan
// reuses it instead of walking git again when the source has not changed.
type KnownSession struct {
	GitRemote     string
	Branch        string
	GitWorktree   string
	CanonicalCwd  string
	Title         string
	IngestedMs    int64
	SchemaVersion int
}

// WorkingDirectory returns the most specific project directory already
// recorded. A per-session Git worktree wins over the project-level canonical
// directory. Discovery still wins over both when the current source carries a
// working directory.
func (s KnownSession) WorkingDirectory() string {
	if s.GitWorktree != "" {
		return s.GitWorktree
	}
	return s.CanonicalCwd
}

// KnownSessionIndex answers whether a discovered session can reuse a recorded
// git resolution. A nil index means every session is resolved the full way.
// Implementations must not require this package to import a database type.
type KnownSessionIndex interface {
	Reusable(session ingest.DiscoveredSession, staleness time.Duration) (KnownSession, bool)
}

// ScanProgress receives phase and count updates while a scan runs. A nil
// progress value is ignored. Implementations must tolerate a nil receiver.
type ScanProgress interface {
	SetPhase(phase string)
	SetProgress(done, total int)
}

// ScanRereadProgress is an optional extension for a progress display that can
// name how many sources the current discovery has had to read again so far. A
// negative count means the current adapter does not report one.
type ScanRereadProgress interface {
	SetDiscoveryRequiredRereads(count int)
}
