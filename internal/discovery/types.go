package discovery

import (
	"fmt"
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

// ProjectIdentity is the stable exact-selection identity of one discovered
// harness/worktree pair. Display labels, remotes, and project names are
// excluded: two harnesses or two physical worktrees remain distinct persisted
// scopes even when one repository row presents them together.
type ProjectIdentity struct {
	Harness   ingest.Harness
	ClonePath ingest.ClonePath
}

// String returns an unambiguous tree key. The harness is length-prefixed so a
// path cannot collide with a different harness/path pair. This value is identity
// data and is never rendered as row text.
func (i ProjectIdentity) String() string {
	if !i.Available() {
		return ""
	}
	harness := i.Harness.String()
	return fmt.Sprintf("%d:%s%s", len(harness), harness, i.ClonePath.String())
}

// Available reports that both the harness and the physical clone path are set.
func (i ProjectIdentity) Available() bool {
	return i.Harness != "" && i.ClonePath != ""
}

// PreparedSessionListing carries one discovered session together with the
// physical project identity and complete-cohort multiplicity evidence. The
// forest and the post-save ingest callback consume this same value so they
// cannot disagree about clone identity.
type PreparedSessionListing struct {
	Listing            SessionListing
	ProjectIdentity    ProjectIdentity
	RepositoryIdentity ingest.RepositoryIdentity
	Candidate          ingest.DiscoveryCandidate
}

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
