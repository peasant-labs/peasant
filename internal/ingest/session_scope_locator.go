package ingest

import (
	"context"
	"errors"
)

// ErrSourceGone reports that a located source path no longer holds the session
// it was recorded for. A selected session that hits it is a miss when it was
// never stored; a stored one is LocateStoredUnreadable instead.
var ErrSourceGone = errors.New("the source path no longer holds this session")

// SessionLocator is the optional capability an adapter implements to take part
// in a scoped run. Every production adapter implements it.
type SessionLocator interface {
	// LookupSessionSources finds the native sources that can hold id BY NAME: a
	// bounded pattern, a stat, or one row per candidate database. It never reads
	// a transcript. No hit returns empty Sources and a nil error; a returned
	// error means the lookup failed, and the core appends the SearchFailed entry
	// itself, so an adapter never returns a SearchFailed entry of its own for a
	// failure it reports. A lookup that could not read one candidate may return
	// a *SourceLookupFailure naming that candidate, so the miss's fix line points
	// at the source that failed rather than at a configured root.
	LookupSessionSources(ctx context.Context, cfg SourceConfig, id SessionID) (SourceLookup, error)

	// DiscoverSessionAt builds the members for one located source: it enumerates
	// by name (the source, its layout relatives, the sources it names, and
	// at.Stored) and calls the same build-and-link function Discover calls.
	// Stored relatives come only from at.Stored, and it writes nothing: writes
	// are returned in Deferred. It returns ErrSourceGone (wrapped) when at.Path
	// no longer holds at.ID; any other read error is returned too. A stored
	// child whose file is gone or unreadable comes back as a member with a nil
	// Source and its reason, never dropped. The core sets each member's role and
	// outcome; the adapter leaves both at their zero value.
	DiscoverSessionAt(ctx context.Context, cfg SourceConfig, at LocatedSource) (LocatedBuild, error)
}

// SourceLookup is what one harness lookup found by name.
type SourceLookup struct {
	// Sources holds the matching sources. An empty slice means no hit in this
	// harness.
	Sources []ResolvedPath
	// Searched records what was searched, for the miss error.
	Searched []SearchedSource
}

// SearchMethod says how one harness searched for a session id.
type SearchMethod uint8

const (
	searchMethodUnset SearchMethod = iota // invalid
	// SearchStoredPath means the harness searched the path the store records.
	SearchStoredPath
	// SearchNamePattern means the harness matched a bounded name pattern.
	SearchNamePattern
	// SearchSQLiteRow means the harness read one row per candidate database.
	SearchSQLiteRow
	// SearchUnsupported means the harness cannot search by id at all.
	SearchUnsupported
	// SearchHarnessDisabled means the harness is not enabled, so it was not
	// searched.
	SearchHarnessDisabled
	// SearchFailed means the lookup returned an error; Detail holds its first
	// line.
	SearchFailed
)

// IsValid reports whether the method is one of the declared values.
func (m SearchMethod) IsValid() bool {
	switch m {
	case SearchStoredPath, SearchNamePattern, SearchSQLiteRow, SearchUnsupported,
		SearchHarnessDisabled, SearchFailed:
		return true
	default:
		return false
	}
}

// searchable reports whether the method actually searched for the id.
func (m SearchMethod) searchable() bool {
	switch m {
	case SearchStoredPath, SearchNamePattern, SearchSQLiteRow:
		return true
	default:
		return false
	}
}

// UnsupportedReason says why a harness cannot be searched by session id. It is
// set only for SearchUnsupported.
type UnsupportedReason uint8

const (
	unsupportedReasonUnset UnsupportedReason = iota // invalid
	// IDOnlyInFileHeader means the id appears only in the file header (pi).
	IDOnlyInFileHeader
	// IDOnlyInFileBody means the id appears only in the file body (OpenCode
	// legacy JSON).
	IDOnlyInFileBody
)

// IsValid reports whether the reason is one of the declared values.
func (r UnsupportedReason) IsValid() bool {
	switch r {
	case IDOnlyInFileHeader, IDOnlyInFileBody:
		return true
	default:
		return false
	}
}

// String returns a short name for the reason, for logs and reports.
func (r UnsupportedReason) String() string {
	switch r {
	case IDOnlyInFileHeader:
		return "id only in file header"
	case IDOnlyInFileBody:
		return "id only in file body"
	default:
		return "unknown"
	}
}

// SearchedSource is one place a harness searched for a session id, kept so a
// miss can say where the id was looked for.
type SearchedSource struct {
	Harness Harness
	Method  SearchMethod
	// Location is the root plus pattern, or the database path, rendered with a
	// tilde. It never holds transcript text.
	Location string
	// Reason is set only for SearchUnsupported.
	Reason UnsupportedReason
	// Detail is set only for SearchFailed: the first error line. It never holds
	// transcript text.
	Detail string
}

// LocatedSource is one source a lookup resolved by name, handed to
// DiscoverSessionAt together with the stored relatives the store already knows.
type LocatedSource struct {
	ID     SessionID
	Path   ResolvedPath
	Stored StoredRelatives
}

// StoredRelatives is what the store already records about a located source's
// family.
type StoredRelatives struct {
	// Parent is the store's one logical parent, or nil when the parent is
	// unstored or the session is a root.
	Parent *SessionID
	// Children holds every stored session whose logical parent is the located
	// id.
	Children []StoredSessionSource
}

// StoredSessionSource is one stored session the locator can re-read.
type StoredSessionSource struct {
	ID          SessionID
	Harness     Harness
	Path        ResolvedPath
	Format      SourceFormat
	Parent      *SessionID
	ProjectHash ProjectHash
}

// LocatedBuild is one located source's built members plus the side effects the
// adapter deferred.
type LocatedBuild struct {
	// Members are the built members. The core sets each member's role and
	// outcome; the adapter leaves both at their zero value.
	Members  []LocatedMember
	Deferred DeferredWrites
}

// DeferredWrites is a discovery side effect applied only after every selected
// id is located, so a refused run writes nothing. Only Claude has one today
// (the evidence and spawns upsert).
type DeferredWrites interface {
	Apply(ctx context.Context) error
}

// NoDeferredWrites is the DeferredWrites of an adapter that writes nothing
// during a scoped discovery.
type NoDeferredWrites struct{}

// Apply does nothing and never fails.
func (NoDeferredWrites) Apply(context.Context) error { return nil }

var _ DeferredWrites = NoDeferredWrites{}

// ProbeKind is how a child's adapter says where one unstored ancestor can be,
// computed from the child's own source. The core executes exactly the declared
// kind and refuses any other.
type ProbeKind uint8

const (
	probeKindUnset ProbeKind = iota // invalid
	// ProbePath is one stat of Path; never a listing.
	ProbePath
	// ProbeRow is one LookupSessionSources call; zero readdir.
	ProbeRow
	// ProbeCatalogWindow is one catalog row, then the child's own date folder
	// and the previous day: at most two readdirs, never a walk of the tree.
	ProbeCatalogWindow
)

// IsValid reports whether the kind is one of the declared values.
func (k ProbeKind) IsValid() bool {
	switch k {
	case ProbePath, ProbeRow, ProbeCatalogWindow:
		return true
	default:
		return false
	}
}

// AncestorProbe says where one unstored ancestor can be found.
type AncestorProbe struct {
	Kind ProbeKind
	// Harness is the harness of the ancestor's source.
	Harness Harness
	// Path is the parent's exact source file for ProbePath.
	Path ResolvedPath
	// ID is the parent's id, set for ProbeRow and for ProbeCatalogWindow.
	ID SessionID
}

// ClaudeBuildMode is the one switch the shared Claude build takes. The two
// switches it replaces always moved together, and one of their four
// combinations was the wrong-parent hazard itself.
type ClaudeBuildMode uint8

const (
	claudeBuildModeUnset ClaudeBuildMode = iota // invalid
	// ClaudeBuildFullWalk is Discover: prune and upsert every walked root, and
	// link teammates from the whole cache.
	ClaudeBuildFullWalk
	// ClaudeBuildScoped is DiscoverSessionAt: upsert cohort paths only (deferred),
	// create teammate links through the two indexed checks, and never remove a
	// stored link.
	ClaudeBuildScoped
)

// IsValid reports whether the mode is one of the declared values.
func (m ClaudeBuildMode) IsValid() bool {
	switch m {
	case ClaudeBuildFullWalk, ClaudeBuildScoped:
		return true
	default:
		return false
	}
}
