package ingest

import (
	"context"

	"github.com/peasant-labs/schema"
)

// CurrentCommitAssociation is the push-facing projection of a durable local
// association. It carries no session ID because the enclosing publish identity
// supplies it on the wire.
type CurrentCommitAssociation struct {
	ID                 schema.AssociationID
	ObservedCommitHash string
}

// SessionLocation holds the host_slug, parent_id, and diff-relevant DB columns
// for a session already in the database. Populated by BulkLookupSessionLocations
// before the DIFF stage so classifySession can use DB state without reading metadata.json.
type SessionLocation struct {
	HostSlug      string
	ParentID      string // empty string if the session has no parent
	IngestedMs    *int64 // nil if unknown; populated from DB ingested_ms column
	SchemaVersion int    // 0 if unknown; populated from DB schema_version column
}

// SessionLocationLookup is satisfied by anything that can answer where a
// session id already lives in the store: its host slug and parent id, or
// that it is not stored yet. It names exactly the one method SessionStore
// and MetricsStore below both already declare for their own reasons; this
// narrow form lets a consumer that needs only this one capability (for
// example, confirming a cross-run linking candidate is really persisted
// before pointing a child at it) depend on that capability alone, without
// pulling in either wider store interface.
type SessionLocationLookup interface {
	// LookupSessionLocation returns the host_slug and parent_id for a known
	// session. Returns empty strings and nil error if the session is not in
	// the DB yet.
	LookupSessionLocation(ctx context.Context, sessionID SessionID) (hostSlug string, parentID string, err error)
}

// SessionStore abstracts SQLite persistence for the pipeline.
// Defined in ingest (not store) to maintain the DI direction:
// store implements this interface; pipeline depends on it.
type SessionStore interface {
	InsertSessions(ctx context.Context, entries []StoreEntry) error
	// LookupSessionLocation returns the host_slug and parent_id for a known session.
	// Returns empty strings and nil error if the session is not in the DB yet.
	LookupSessionLocation(ctx context.Context, sessionID SessionID) (hostSlug string, parentID string, err error)
	// BulkLookupSessionLocations returns host_slug and parent_id for all given
	// session IDs in a single query. Sessions not found in the DB are omitted
	// from the returned map (not an error). This is the preferred method for
	// pre-populating a cache before the DIFF stage to avoid per-session queries.
	BulkLookupSessionLocations(ctx context.Context, sessionIDs []SessionID) (map[SessionID]SessionLocation, error)
	// UpsertSessionCommits atomically replaces all commit rows for a session
	// (DELETE + INSERT within a transaction). An empty commits slice deletes
	// all existing rows for the session without inserting new ones.
	// Non-fatal in the pipeline: callers log on error and continue.
	UpsertSessionCommits(ctx context.Context, sessionID SessionID, commits []CommitInfo) error
	// CleanupOrphanProjects deletes projects and related daily_summary_by_project
	// rows that have no corresponding session in the sessions table. This can occur
	// when sessions are deleted from the DB (e.g., via prune) but their project
	// dimension rows are left behind.
	// Best-effort: callers should log and continue on error.
	CleanupOrphanProjects(ctx context.Context) error
	Close() error
}

// StoreEntry pairs extracted metadata with its discovered session.
type StoreEntry struct {
	Metadata *UnifiedMetadata
	Session  DiscoveredSession
}

// MetricsStore abstracts the analytics read/write path for session entries
// and metrics. Defined in ingest (not store) to maintain the DI direction:
// store implements this interface; metrics engine depends on it.
type MetricsStore interface {
	// IndexSessionEntries atomically replaces all entries for a session
	// (DELETE + INSERT within a transaction).
	IndexSessionEntries(ctx context.Context, sessionID SessionID, entries []schema.SessionEntry) error
	// SessionEntriesExist returns true if session_entries rows exist for the session.
	SessionEntriesExist(ctx context.Context, sessionID SessionID) (bool, error)
	// SaveMetrics persists a SessionMetrics row via INSERT OR REPLACE.
	SaveMetrics(ctx context.Context, metrics *SessionMetrics) error
	// GetMetrics returns the SessionMetrics for a session, or nil if none exists.
	GetMetrics(ctx context.Context, sessionID SessionID) (*SessionMetrics, error)
	// GetTitleContext returns the session harness and the most precise stored
	// project root: session worktree first, then project canonical cwd.
	GetTitleContext(ctx context.Context, sessionID SessionID) (schema.Harness, string, error)
	// MetricsExist returns true if session_metrics exists for the session
	// with compute_version >= the given version.
	MetricsExist(ctx context.Context, sessionID SessionID, computeVersion int) (bool, error)
	// ListEntries returns all session_entries for a session ordered by entry_index.
	ListEntries(ctx context.Context, sessionID SessionID) ([]schema.SessionEntry, error)
	// UpdateDailySummary recomputes daily_summary aggregations for the given days.
	// Decoupled from InsertSessions so the pipeline controls when aggregation runs.
	UpdateDailySummary(ctx context.Context, days []string) error
	// UpdateIndexState sets the index_version and indexed_at for a session
	// after successful indexing. indexed_at is in Unix milliseconds.
	UpdateIndexState(ctx context.Context, sessionID SessionID, version int, indexedAtMs int64) error
	// ListStaleIndexSessions returns session IDs where index_version < currentVersion.
	// Used by the post-FILTER auto-detect step to find sessions needing re-indexing.
	ListStaleIndexSessions(ctx context.Context, currentVersion int) ([]SessionID, error)
	// LookupSessionLocation returns the host_slug and parent_id for a session.
	// Returns ("", "", nil) if the session is not found in the DB.
	// Used by reconstructFromMetadata to avoid scanning all host directories.
	LookupSessionLocation(ctx context.Context, sessionID SessionID) (hostSlug string, parentID string, err error)
	// LookupSourceInfo returns source_path, source_format, and model_harness for a session.
	// Returns ("", "", "", nil) if the session is not found.
	// Used by reconstructFromSourceInfo as a fallback when peasant-sync metadata is missing.
	LookupSourceInfo(ctx context.Context, sessionID SessionID) (sourcePath string, sourceFormat SourceFormat, provider string, err error)
}

// TranscriptSourceKind is where a harness's ENTRIES come from.
//
// It is the one axis on which the indexers genuinely differ, and naming it once
// is what stops each caller from guessing. Callers used to guess by asking whether
// in-memory bytes happened to be available, which is a question about the CALLER's
// state rather than about the indexer, and it got OpenCode wrong: OpenCode writes
// a single JSON session file, so bytes were always available, so the bytes path
// was always chosen, and the indexer threw them away because its entries are not
// in them.
type TranscriptSourceKind int

const (
	// TranscriptSourceKindUnknown is the zero value, and the kind of any indexer
	// that has not declared one. Indexing REFUSES it.
	//
	// It fails closed for the same reason the redaction policy refuses a level it
	// has no disposition for: the compile-time interface guard forces the METHOD
	// to exist, and can force nothing about the VALUE it returns, so an indexer
	// that forgets - or mis-writes - its declaration arrives here. Treating that
	// as a file source is a guess, and the guess is wrong for exactly the harness
	// shape this type was introduced for: a directory-source harness that writes
	// one JSON session file has bytes available, so it would be handed them, and
	// would discard them and index nothing while the import reported success.
	TranscriptSourceKindUnknown TranscriptSourceKind = iota
	// TranscriptSourceFile means the entries are in the transcript bytes, so the
	// indexer can parse them in memory and never needs to touch the provider.
	TranscriptSourceFile
	// TranscriptSourceDirectory means the entries are spread over a tree under a
	// provider root, so there are no single-file bytes that contain them. The
	// caller must preserve DiscoveredSession.OriginalRoot; a root derived from the
	// source path points at the written copy, not the provider.
	TranscriptSourceDirectory
)

// AllTranscriptSourceKinds is every kind the dispatch has to answer for,
// including the zero value.
//
// The zero value is a member on purpose: its arm is a refusal, and a refusal
// nobody exercises is indistinguishable from a fall-through. The dispatch corpus
// walks this set, so a kind added here without an arm - or an arm quietly folded
// into another - is a test failure rather than a behaviour nobody checked.
//
// That corpus is its only consumer today, which is a considered stopping point
// rather than an oversight: this states a contract about the type rather than
// giving a test an entry point it otherwise lacks. The stronger form is an
// enumeration a production predicate DERIVES from, since it then cannot drift
// from the type without breaking the build. Prefer that if a predicate here can
// be expressed over the set.
var AllTranscriptSourceKinds = []TranscriptSourceKind{
	TranscriptSourceKindUnknown,
	TranscriptSourceFile,
	TranscriptSourceDirectory,
}

func (k TranscriptSourceKind) String() string {
	switch k {
	case TranscriptSourceFile:
		return "file"
	case TranscriptSourceDirectory:
		return "directory"
	}
	return "unknown"
}

// TranscriptIndexer parses a harness's transcript into SessionEntry slices.
// One implementation per harness.
type TranscriptIndexer interface {
	// SourceKind reports where this indexer's entries come from, so a caller
	// dispatches on the indexer's own contract rather than on what it happens to
	// have loaded.
	SourceKind() TranscriptSourceKind

	// IndexTranscript reads the transcript itself: the file at session.SourcePath
	// for a file source, or the tree under session.OriginalRoot for a directory
	// source.
	IndexTranscript(ctx context.Context, session DiscoveredSession) ([]schema.SessionEntry, error)

	// IndexTranscriptBytes parses a transcript already held in data, avoiding a
	// second read after the ingest write.
	//
	// Only a TranscriptSourceFile indexer is called this way. A directory source
	// has no bytes that contain its entries, so handing it any would mean handing
	// it something to discard - which is what used to happen, and which hid the
	// fact that its root had been lost.
	IndexTranscriptBytes(ctx context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error)
}

// SessionTranscriptSourceResolver lets an indexer select its typed source per
// discovered session when one harness has more than one physical
// representation. The generic pipeline owns dispatch; provider indexers own the
// representation decision.
type SessionTranscriptSourceResolver interface {
	TranscriptSourceKindFor(DiscoveredSession) TranscriptSourceKind
}

// MetricsComputer computes metrics for sessions. Returns the count of sessions
// that were actually (re)computed.
type MetricsComputer interface {
	ComputeMetrics(ctx context.Context, sessionIDs []SessionID) (int, error)
}

// InsightsComputer recomputes daily_summary aggregations for the given days.
type InsightsComputer interface {
	ComputeInsights(ctx context.Context, days []string) error
}

// SessionAnalyzer composes MetricsComputer and InsightsComputer.
// The metrics Engine implements both; Pipeline takes a single SessionAnalyzer field.
type SessionAnalyzer interface {
	MetricsComputer
	InsightsComputer
}

// TextRedactor applies privacy-preserving redaction to session metadata and transcripts.
// redact.DefaultRedactor implements this interface.
type TextRedactor interface {
	// RedactMetadata returns a deep copy of meta with sensitive fields redacted.
	// The original pointer is never mutated.
	RedactMetadata(meta *UnifiedMetadata) *UnifiedMetadata
	// RedactJSON recursively applies RedactText to all string values in a JSON-decoded
	// value tree (string, []any, map[string]any). Non-string scalar types (bool, float64,
	// json.Number, nil) pass through unchanged. Used by redactTranscript to redact
	// transcript files in-place after copy to tmpDir.
	RedactJSON(value any) any
	// Level returns the redaction level as a string (e.g. "minimal", "standard", "maximum").
	Level() string
	// RuleSetVersion returns the semantic version of the compiled rule set (e.g. "1.1.0").
	// Populated into RedactionInfo.RuleSetVersion on every redaction write.
	RuleSetVersion() string
}

// ModelInfo represents a model entry from the models reference table.
type ModelInfo struct {
	ModelID               string
	ProviderKey           string
	DisplayName           string
	Family                *string
	ContextWindow         *int
	MaxOutput             *int
	Reasoning             bool
	ToolCall              bool
	CostInputPerMTok      *float64
	CostOutputPerMTok     *float64
	CostReasoningPerMTok  *float64
	CostCacheReadPerMTok  *float64
	CostCacheWritePerMTok *float64
	ReleaseDate           *string
	LastSynced            string
}

// ModelsSyncer abstracts the model registry for enrichment queries.
// Implemented by *store.Store. Used by MetricFuncs that need model data
// (e.g., computeContextUtilization, computeCost) via the closure pattern.
type ModelsSyncer interface {
	// SyncModels upserts model entries into the models table.
	SyncModels(ctx context.Context, models []ModelInfo) error
	// GetModel returns a single model by ID and provider key, or nil if not found.
	GetModel(ctx context.Context, modelID, providerKey string) (*ModelInfo, error)
	// GetContextWindow returns the context window for any matching model.
	// Second return is false if model not found (not an error).
	GetContextWindow(ctx context.Context, modelID string) (int, bool, error)
}

// IngestLogger records pipeline audit log entries.
// NOTE: The impl plan (S9-L1) specified adding LogIngestRun to MetricsStore.
// A separate interface was chosen for single-responsibility: audit logging
// is orthogonal to metrics storage, and independent injection makes testing
// easier. Both are satisfied by *store.Store in production.
type IngestLogger interface {
	LogIngestRun(ctx context.Context, entry IngestLogEntry) error
}

// IndexLogger records per-session indexing audit log entries.
// Separate from IngestLogger because index_log entries are written during the
// INDEX stage (before ingest_log row exists). Both are satisfied by *store.Store.
type IndexLogger interface {
	LogIndexEntry(ctx context.Context, entry IndexLogEntry) error
}

// SessionClassifier annotates sessions using registered classifiers.
// Implemented by metrics.ClassifierAnnotator; injected into Pipeline via WithClassifier.
// Annotate is best-effort: per-session errors are logged and skipped, never fatal.
type SessionClassifier interface {
	// Annotate runs all classifiers for the given session and persists non-nil results.
	Annotate(ctx context.Context, sessionID SessionID) error
}

// SessionAnnotationParams holds the inputs for creating a session-level annotation.
// Uses ingest-package types to keep the DI boundary clean: store implements
// AnnotationStore without creating an import cycle.
type SessionAnnotationParams struct {
	SessionID        string
	AnnotatorID      string // V16: UUID FK to annotators.id
	AnnotationTypeID string // V16: UUID FK to annotation_types.id
	Value            string
	Confidence       *float64
	Reason           *string
	Provenance       *schema.Provenance
}

// EntryAnnotationParams holds the inputs for creating an entry-level annotation.
// Uses ingest-package types to keep the DI boundary clean: store implements
// AnnotationStore without creating an import cycle.
type EntryAnnotationParams struct {
	SessionID        string
	EntryIndex       int
	EndIndex         int    // half-open [start, end); 0 = single-entry (EntryIndex + 1)
	AnnotatorID      string // V16: UUID FK to annotators.id
	AnnotationTypeID string // V16: UUID FK to annotation_types.id
	Value            string
	Confidence       *float64
	Reason           *string
	Provenance       *schema.Provenance
}

// CreateAnnotationParams holds the inputs for creating an annotation.
// Exactly one of SessionID or EntryTarget must be set.
// Uses ingest-package types to keep the DI boundary clean: store implements
// AnnotationStore without creating an import cycle.
type CreateAnnotationParams struct {
	// Exactly one target arm must be set.
	SessionID   *string      // ARM 1: session-level annotation
	EntryTarget *EntryTarget // ARM 2: entry-level annotation

	AnnotatorID      string // UUID FK to annotators.id
	AnnotationTypeID string // UUID FK to annotation_types.id
	Value            string

	Confidence *float64
	Reason     *string
	Provenance *schema.Provenance
}

// EntryTarget is the compound target for entry-level annotations.
// Defined here (ingest) for use in CreateAnnotationParams.
type EntryTarget struct {
	SessionID  string
	EntryIndex int
	EndIndex   int // half-open [start, end); 0 means single-entry (entry_index + 1)
}

// AnnotationDedupResult indicates the outcome of a deduplication check when
// persisting an annotation. Used by persistResult to decide whether to skip,
// supersede, or create a new annotation.
type AnnotationDedupResult int

const (
	// DedupCreate indicates no existing annotation was found; a new one should be created.
	DedupCreate AnnotationDedupResult = iota
	// DedupSkip indicates an existing annotation with the same content hash was found;
	// no new annotation is needed.
	DedupSkip
	// DedupSupersede indicates an existing annotation with a different content hash was
	// found; the old annotation should be superseded by the new one.
	DedupSupersede
)

// String returns the string representation of the dedup result.
func (d AnnotationDedupResult) String() string {
	switch d {
	case DedupCreate:
		return "create"
	case DedupSkip:
		return "skip"
	case DedupSupersede:
		return "supersede"
	default:
		return "unknown"
	}
}

// FindAnnotationParams holds the query parameters for looking up an existing
// annotation by its logical identity (type + target + annotator). Used by
// FindExistingAnnotation to support content-hash-based deduplication.
type FindAnnotationParams struct {
	// AnnotationTypeID is the UUID FK to annotation_types.id.
	AnnotationTypeID string
	// AnnotatorID is the UUID FK to annotators.id.
	AnnotatorID string
	// SessionID is set for session-level annotations.
	SessionID *string
	// EntryIndex is set for entry-level annotations (along with SessionID).
	EntryIndex *int
}

// ExistingAnnotation holds the ID and content hash of an annotation found by
// FindExistingAnnotation. Used to decide whether to skip or supersede.
type ExistingAnnotation struct {
	// ID is the UUID primary key of the existing annotation.
	ID string
	// ContentHash is the SHA3-256 hash stored on the existing annotation.
	// Empty string if no content_hash has been computed yet.
	ContentHash string
}

// AnnotationStore abstracts annotation write operations for the pipeline ANNOTATE stage.
// Defined in ingest (not store) to maintain the DI direction:
// store implements this interface; the pipeline depends on it.
// *store.Store satisfies this interface via CreateSessionAnnotation, CreateEntryAnnotation,
// and GetAnnotatorIDByName.
type AnnotationStore interface {
	// CreateSessionAnnotation persists a session-level annotation and returns its UUID.
	// annotatorID and annotationTypeID must reference existing rows.
	CreateSessionAnnotation(ctx context.Context, p SessionAnnotationParams) (string, error)
	// CreateEntryAnnotation persists an entry-level annotation and returns its UUID.
	// The target entry span is [EntryIndex, EndIndex); EndIndex=0 defaults to EntryIndex+1.
	CreateEntryAnnotation(ctx context.Context, p EntryAnnotationParams) (string, error)
	// GetAnnotatorIDByName returns the UUID for the annotator with the given name.
	// Returns "", nil if no annotator with that name exists.
	GetAnnotatorIDByName(ctx context.Context, name string) (string, error)
	// GetAnnotationTypeID returns the UUID for the annotation type with the given type_id string.
	// Returns "", nil if no annotation type with that type_id exists.
	GetAnnotationTypeID(ctx context.Context, typeID string) (string, error)
	// FindExistingAnnotation looks up the most recent non-superseded annotation
	// matching the given (annotation_type_id, annotator_id, target) triple.
	// Returns nil, nil if no matching annotation exists.
	FindExistingAnnotation(ctx context.Context, p FindAnnotationParams) (*ExistingAnnotation, error)
	// SupersedeAnnotation marks the annotation with oldID as superseded by newID.
	// Sets superseded_by = newID and updated_at = now on the old annotation.
	SupersedeAnnotation(ctx context.Context, oldID, newID string) error
	// UpdateContentHash sets the content_hash column on the given annotation.
	// Called after creating a new annotation to persist its computed content hash.
	UpdateContentHash(ctx context.Context, annotationID, contentHash string) error
	// CreateAnnotationAndSupersede creates a new annotation, supersedes oldID, and
	// sets the content_hash — all within a single SQLite transaction. oldID is the
	// UUID of the annotation to supersede; contentHash is the hash to set on the new
	// annotation. Returns the new annotation's UUID.
	CreateAnnotationAndSupersede(ctx context.Context, params CreateAnnotationParams, oldID string, contentHash string) (string, error)
}

// RecordedProject is one project ingestion has stamped sessions with: the
// canonical identity and the working directory that produced it.
//
// It exists because those two are not interchangeable. An identity derived from
// an origin remote is shared by every clone; an identity derived from a path
// belongs to exactly the directory a session was recorded in, which is not
// necessarily a repository root. Deciding whether a recorded project belongs to
// a given repository needs both values.
type RecordedProject struct {
	// Hash is the canonical project identity carried by the sessions.
	Hash string
	// CanonicalCwd is the shortest working directory observed for Hash, or ""
	// when none was recorded.
	CanonicalCwd string
}

// HeldSession is a session that cannot be pushed yet because its metrics have
// not been computed.
//
// It carries the project identity, not just the ID, because the notice built
// from it is per-repository: a hook firing in one repository must not print
// another repository's session identifiers on every commit.
type HeldSession struct {
	// SessionID is the held session's identifier.
	SessionID string
	// ProjectHash is the canonical project identity the session carries.
	ProjectHash string
}

// PushSessionRow carries all fields needed by the push pipeline.
// Returned by push candidate query methods. Includes push-specific fields
// (IngestedMs, PushedAt, SourceFilePath, SourceFormat) that are not
// on the display-oriented SessionRow in the store package.
type PushSessionRow struct {
	SessionID      string
	ParentID       string // empty if the session has no parent; subagents resolve their on-disk path under {parentID}/subagents/
	ModelHarness   string
	ModelID        string
	HostSlug       string
	ProjectHash    string
	ProjectName    string
	ProjectPath    string // raw session worktree, falling back to the project's canonical cwd
	StartMs        int64
	EndMs          int64
	IngestedMs     int64
	PushedAt       *int64 // nil = never pushed
	SourceFilePath string
	SourceFormat   string
	GitBranch      *string
	GitRemote      string // git remote URL from host_slugs (empty if unknown); used for branch-aware selection
	ToolVersion    *string
	TurnCount      int
	ToolCalls      int
	InputTokens    int
	OutputTokens   int
	TokensTotal    int
	DurationMs     int64
}

// IsSelectedByBranch reports the branch-aware selection result for this row
// using the legacy positional matcher. Clone-aware command paths must resolve
// ProjectPath across the complete row cohort and call MatchBranchCandidate
// instead. A nil GitBranch is treated as an unknown branch ("").
func (r PushSessionRow) IsSelectedByBranch(sel SelectionMatcher) BranchMatch {
	branch := ""
	if r.GitBranch != nil {
		branch = *r.GitBranch
	}
	return sel.MatchBranch(Harness(r.ModelHarness), r.GitRemote, r.ProjectName, branch, SessionID(r.SessionID))
}

// PushLogEntry is a single row in the push_log table.
// One row is written per peasant push run (at end of push pipeline).
type PushLogEntry struct {
	StartedAt       int64
	FinishedAt      *int64
	VillageURL      string
	SessionsPushed  int
	SessionsUpdated int
	SessionsSkipped int
	SessionsFailed  int
	ErrorMessage    *string
	UserID          string
	Username        string
}

// PublishResult holds the server-assigned transcript identifier
// returned after a successful upload.
type PublishResult struct {
	TranscriptID string
}

// AnnotationQueryStore abstracts the annotation query operations needed by the annotation push pipeline.
// Defined in ingest (not store) to maintain the DI direction:
// store implements this interface; the annotation push function depends on it.
type AnnotationQueryStore interface {
	// ListSystemAnnotations returns all non-superseded annotations whose type has
	// system origin (OriginSystem). Used by the push pipeline to collect annotations
	// that the village can accept; push rejects unknown or user-defined type_ids.
	ListSystemAnnotations(ctx context.Context) ([]AnnotationPushRow, error)

	// ListSupersededAnnotations returns all SUPERSEDED (superseded_by IS NOT NULL)
	// system-origin annotations. It is the retraction source:
	// ListSystemAnnotations filters superseded_by IS NULL, so it cannot serve this.
	// Each returned row carries the SAME content-bearing fields the annotation was
	// originally pushed with, so recomputing its content hash yields the hash the
	// village stored — letting the client retract exactly what it locally retired.
	ListSupersededAnnotations(ctx context.Context) ([]AnnotationPushRow, error)
}

// AnnotationPushRow is the push-specific view of an annotation.
// Carries the subset of fields needed to construct an AnnotationPushItem.
type AnnotationPushRow struct {
	ID                   string
	TargetKind           schema.TargetKind
	TargetAssociationID  *schema.AssociationID
	AssociationSessionID *SessionID // local selection context; never serialized on the wire
	SessionID            *string
	EntryIndex           *int
	EntryEndIndex        *int
	AnnotationID         *string
	ProjectHash          *string
	TypeID               string
	Value                string
	IsPrimary            bool
	Confidence           *float64
	Reason               *string
	AnnotatorName        string
	Provenance           *schema.Provenance
	ContentHash          *string // Pre-computed if available, nil otherwise.
}
