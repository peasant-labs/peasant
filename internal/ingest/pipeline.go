package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// DiffStatus categorizes a discovered session for incremental ingestion.
type DiffStatus int

const (
	DiffNew       DiffStatus = iota // No existing metadata
	DiffUpdated                     // Source newer or schema version changed
	DiffUnchanged                   // Already ingested, no changes
	DiffActive                      // Source file modified < staleness threshold ago
)

// String returns a human-readable name for the DiffStatus.
func (s DiffStatus) String() string {
	switch s {
	case DiffNew:
		return "new"
	case DiffUpdated:
		return "updated"
	case DiffUnchanged:
		return "unchanged"
	case DiffActive:
		return "active"
	default:
		return "unknown"
	}
}

// DiffResult pairs each discovered session with its diff status.
type DiffResult struct {
	Sessions []DiffEntry
}

// DiffEntry associates a DiscoveredSession with its computed DiffStatus.
type DiffEntry struct {
	Session DiscoveredSession
	Status  DiffStatus
}

// PipelineResult summarizes a pipeline run.
type PipelineResult struct {
	Summary  PipelineSummary
	Sessions []SessionResult
	Duration time.Duration
	IndexLog []IndexLogEntry // per-session indexing outcomes (populated during INDEX stage)
	// DiscoveryDiagnostics names source locations an adapter could not fully
	// enumerate. Discovery stayed non-fatal per location, so the run continued;
	// these records make each skipped location visible to the caller.
	DiscoveryDiagnostics []DiscoveryDiagnostic
}

// PipelineSummary holds aggregate counts for a pipeline run.
type PipelineSummary struct {
	New             int
	Updated         int
	Unchanged       int
	Active          int
	Errors          int
	Indexed         int   // sessions successfully indexed into session_entries
	Computed        int   // sessions whose metrics were (re)computed
	StoreError      error // non-nil if DB insert failed; pipeline continued normally
	IndexVersion    int   // CurrentIndexVersion used this run
	MetadataVersion int   // CurrentSchemaVersion used this run
	// ReminedEvidenceRecords is how many cached discovery evidence records this
	// run had to mine again. It is greater than zero on the first run after an
	// upgrade that added a field the cached records do not carry, and zero on
	// every warm run afterwards.
	ReminedEvidenceRecords int
	// OriginResolve is what the stored-origin resolve pass did before this run
	// wrote anything of its own.
	OriginResolve ResolveReport
	// OriginResolveError is non-nil when that pass could not finish. The run
	// continues: an unjudged row keeps the visible fail-safe value and is listed
	// again next time, so a failure here delays a verdict rather than losing one.
	OriginResolveError error
}

// SessionResult records the outcome of processing a single session.
type SessionResult struct {
	SessionID  SessionID
	Harness    Harness
	ParentUUID *SessionID // non-nil for subagent sessions
	Status     DiffStatus // Classified status from diff phase; does NOT change on processing error.
	OutputPath string     // Final output directory (empty for dry-run, skipped, or error).
	Error      error      // non-nil if processing failed; check Error before trusting Status.
}

// PipelineConfig holds runtime configuration for the pipeline.
type PipelineConfig struct {
	Sources            map[Harness]SourceConfig
	OutputDir          ResolvedPath
	Force              bool // re-ingest everything
	IncludeActive      bool // include sessions still being written
	StalenessThreshold time.Duration
	DryRun             bool
	Reindex            bool // scan peasant-sync output and re-process sessions with stale or missing index data
	// Parallelism controls the number of concurrent session workers.
	// 0 means "use runtime.NumCPU()". Set to 1 for sequential (legacy) behavior.
	Parallelism int
	// IndexProfiler receives opt-in INDEX timing observations.
	// Nil means no INDEX profiling.
	IndexProfiler *IndexProfiler
	// Progress receives ProgressEvents from each pipeline stage.
	// Nil means no progress reporting.
	Progress *ProgressState
	// AllowedSessionIDs restricts which sessions are processed.
	// nil means all sessions pass the filter (backward compatible).
	// Non-nil means only sessions in the map are processed.
	// An empty non-nil map means NO sessions pass.
	AllowedSessionIDs map[SessionID]bool
	// Since filters sessions by age: only sessions with ModTime (or CreatedAt)
	// at or after this time are processed.
	// nil means no time-based filter (backward compatible).
	Since *time.Time
	// PrepareSessionFilter receives the complete discovered-session cohort once,
	// immediately after DISCOVER and before any SessionFilter call. It lets a
	// filter establish cohort-wide identity multiplicity without per-session lazy
	// matching. nil means no preparation is required.
	PrepareSessionFilter func(context.Context, []DiscoveredSession) error
	// SessionFilter optionally restricts which discovered sessions are processed.
	// Called during the FILTER stage after DISCOVER and DIFF. Returns true to
	// include the session. nil means no filter (all sessions pass).
	// Typically built from the config selection index by the CLI layer.
	SessionFilter func(DiscoveredSession) bool
	// SessionExclusionFilter reports whether exact prepared deny evidence applies
	// to a session. It runs for roots and children before parent inheritance, so a
	// selected parent cannot re-admit an exactly denied child. nil means no exact
	// exclusion filter.
	SessionExclusionFilter func(DiscoveredSession) bool
}

// Discoverer discovers sessions from configured sources.
type Discoverer interface {
	Discover(ctx context.Context) ([]DiscoveredSession, error)
}

// DiffResolver classifies discovered sessions into diff categories.
type DiffResolver interface {
	Diff(sessions []DiscoveredSession) DiffResult
}

// MetadataExtractor extracts metadata from a session.
type MetadataExtractor interface {
	ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error)
}

// SessionWriter writes processed session output atomically.
type SessionWriter interface {
	WriteSession(ctx context.Context, entry DiffEntry, meta *UnifiedMetadata) (string, error)
}

// OrphanCleaner removes stale temporary directories.
type OrphanCleaner interface {
	CleanOrphans()
}

// indexedMeta pairs a discovered session with its extracted start timestamp
// and the output transcript path for use in the INDEX and COMPUTE stages.
//
// outputTranscriptPath points at the copy Peasant wrote, NOT at the provider's
// own file, so the indexer reads what was stored. transcriptData, when non-nil,
// holds those same bytes in memory (from the workerResult arena) so the indexer
// can skip a second disk read.
//
// Both are the transcript AS RECORDED. No level a user can choose redacts
// transcript content at ingest: the pipeline's redactor is applied to METADATA
// before it is written, and it has no production caller today (see WithRedactor).
// What protects a publish is metadata redaction on the outward push path plus the
// village's server-side secret scan. This is stated here because the comment it
// replaces asserted the reverse as settled fact, and that assertion - repeated
// one layer downstream - is what got the outward safety-net re-redaction deleted.
type indexedMeta struct {
	session              DiscoveredSession
	startMs              int64
	outputTranscriptPath string // final on-disk path: {sessionDir}/{sessionId}--transcript.{ext}
	transcriptData       []byte // nil = read from outputTranscriptPath; non-nil = use directly
	indexed              bool   // true if already indexed in the drain loop (skip INDEX, include in COMPUTE)
}

// Pipeline composes all ingest stages with dependency injection.
// It is the concrete implementation of Discoverer, DiffResolver, and OrphanCleaner.
type Pipeline struct {
	fs       FileSystem
	git      GitResolver
	adapters map[Harness]AdapterFactory
	config   PipelineConfig
	store    SessionStore // nil = skip DB insert (backward compatible)
	salt     salt.Salt    // per-installation HMAC salt for project hash derivation

	// locationCache is pre-populated before the DIFF stage via BulkLookupSessionLocations.
	// It maps SessionID → SessionLocation (host_slug + parent_id) for sessions already
	// in the DB, enabling O(1) fast-path lookups in findMetadataPath without per-session
	// DB round-trips. Nil before Run() populates it.
	locationCache map[SessionID]SessionLocation

	// reminedEvidence is how many cached evidence records this run's discovery
	// had to mine again, collected from the adapters that can report it.
	reminedEvidence int

	// originResolve and originResolveErr hold what the stored-origin pass did
	// this run. They live on the pipeline rather than in Run because the report
	// is assembled by a helper the reindex path shares, where the pass does not
	// run at all and the zero report is the truthful answer.
	originResolve    ResolveReport
	originResolveErr error

	// seqCursorCache maps SessionID → the last ingested OpenCode event sequence,
	// pre-loaded before the DIFF stage when the store records the change cursor. A
	// session whose current sequence exceeds its cached cursor is re-ingested even
	// when no time column moved, closing the in-place-rewrite blind spot. Nil when
	// the store does not record the cursor, which keeps the clock-only behaviour.
	seqCursorCache map[SessionID]int64

	// discoveryDiagnostics accumulates per-location discovery failures reported
	// by adapters during discover(), copied into every PipelineResult so a
	// skipped database is visible even though discovery stayed non-fatal.
	discoveryDiagnostics []DiscoveryDiagnostic

	// v2 analytics stages (all optional; nil = skip stage).
	redactor               TextRedactor                  // REDACT stage: applied before writing metadata to disk
	indexers               map[Harness]TranscriptIndexer // INDEX stage: parses transcripts into session_entries
	metricsStore           MetricsStore                  // INDEX stage: persists session_entries
	analyzer               SessionAnalyzer               // COMPUTE stage: computes metrics + insights
	logger                 IngestLogger                  // AUDIT stage: records ingest run to ingest_log
	indexLogger            IndexLogger                   // INDEX stage: records per-session indexing outcomes to index_log
	gitAnalyzer            GitDiffAnalyzer               // EXTRACT+WRITE stage: commit detection (optional)
	commitTranscriptReader CommitTranscriptReader

	classifier SessionClassifier // ANNOTATE stage: runs classifiers + persists results (optional; nil = skip).
}

// PipelineOption configures optional pipeline behavior.
type PipelineOption func(*Pipeline)

// WithStore injects a SessionStore for DB persistence.
// When set, successfully processed sessions are batch-inserted into the store
// after all disk writes complete. Insert failures are non-fatal.
func WithStore(s SessionStore) PipelineOption {
	return func(p *Pipeline) { p.store = s }
}

// WithSalt injects a per-installation HMAC salt for project hash derivation.
// When set, DeriveProjectIdentifiers uses HMAC-SHA256(salt, normalizedRemote)
// instead of the zero salt, making project hashes opaque and
// installation-specific.
func WithSalt(s salt.Salt) PipelineOption {
	return func(p *Pipeline) { p.salt = s }
}

// WithRedactor injects a TextRedactor into the ingest pipeline.
//
// When set, it redacts metadata via RedactMetadata and transforms transcript
// content: processSession runs it over each whole transcript body for the
// single-file source formats before those bytes are written and indexed.
//
// No production constructor currently passes this option, so normal ingest writes
// transcript content as recorded. Adding a production caller also requires the
// onboarding privacy disclosure to explain automatic content redaction.
func WithRedactor(r TextRedactor) PipelineOption {
	return func(p *Pipeline) { p.redactor = r }
}

// WithIndexers injects per-provider TranscriptIndexers.
// When set (along with MetricsStore), transcripts are parsed into session_entries
// after disk writes. Indexing errors are non-fatal.
func WithIndexers(idx map[Harness]TranscriptIndexer) PipelineOption {
	return func(p *Pipeline) { p.indexers = idx }
}

// WithMetricsStore injects a MetricsStore for session_entries persistence.
// Required by the INDEX stage to store indexed entries.
func WithMetricsStore(ms MetricsStore) PipelineOption {
	return func(p *Pipeline) { p.metricsStore = ms }
}

// WithAnalyzer injects a SessionAnalyzer for metrics and insights computation.
// When set, ComputeMetrics and ComputeInsights run after indexing.
// Computation errors are non-fatal.
func WithAnalyzer(a SessionAnalyzer) PipelineOption {
	return func(p *Pipeline) { p.analyzer = a }
}

// WithLogger injects an IngestLogger for audit trail recording.
// When set, each pipeline run writes an IngestLogEntry after the REPORT stage.
// Logging errors are non-fatal.
func WithLogger(l IngestLogger) PipelineOption {
	return func(p *Pipeline) { p.logger = l }
}

// WithIndexLogger injects an IndexLogger for per-session index audit recording.
// When set, each session indexing attempt writes an IndexLogEntry during the INDEX stage.
// Logging errors are non-fatal.
func WithIndexLogger(l IndexLogger) PipelineOption {
	return func(p *Pipeline) { p.indexLogger = l }
}

// WithGitDiffAnalyzer injects a GitDiffAnalyzer for commit detection.
// When set, each processed session runs timestamp-based commit detection
// to populate GitContext.Commits in metadata and the session_commits table.
// Detection errors are non-fatal: warnings are appended to metadata.Diagnostics.Warnings.
func WithGitDiffAnalyzer(a GitDiffAnalyzer) PipelineOption {
	return func(p *Pipeline) { p.gitAnalyzer = a }
}

// WithCommitTranscriptReader injects the bounded reader used by commit command
// validation. Database origins never call it.
func WithCommitTranscriptReader(reader CommitTranscriptReader) PipelineOption {
	return func(p *Pipeline) { p.commitTranscriptReader = reader }
}

// WithClassifier injects a SessionClassifier for the ANNOTATE stage.
// When set, classifiers run after COMPUTE for each successfully indexed session.
// Annotation errors are non-fatal.
func WithClassifier(c SessionClassifier) PipelineOption {
	return func(p *Pipeline) { p.classifier = c }
}

// NewPipeline constructs a Pipeline with injected dependencies.
// Returns an error if adapters is empty.
func NewPipeline(fs FileSystem, git GitResolver, adapters map[Harness]AdapterFactory, cfg PipelineConfig, opts ...PipelineOption) (*Pipeline, error) {
	if len(adapters) == 0 {
		return nil, fmt.Errorf("NewPipeline: adapters map must not be empty")
	}
	p := &Pipeline{
		fs:       fs,
		git:      git,
		adapters: adapters,
		config:   cfg,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Run executes the full ingest pipeline and returns a summary result.
//
// Stages:
//  1. DISCOVER: For each enabled provider, create adapter via factory, call Discover()
//  2. DIFF: Categorize each discovered session
//  3. FILTER: Skip unchanged/active sessions (unless overridden)
//  4. EXTRACT + REDACT + WRITE: Extract metadata, redact, and atomically write output
//  5. INDEX: Parse transcripts into session_entries (best-effort)
//  6. COMPUTE: Compute metrics + insights for indexed sessions (best-effort)
//  7. CLEANUP: Remove orphan .tmp-* directories
//  8. REPORT: Return PipelineResult
//  9. AUDIT: Write ingest_log entry (best-effort)
func (p *Pipeline) Run(ctx context.Context) (*PipelineResult, error) {
	start := time.Now()

	// REINDEX mode: alternative code path that scans peasant-sync output
	// instead of discovering from source providers.
	if p.config.Reindex {
		return p.runReindex(ctx, start)
	}

	prog := p.config.Progress

	// Stage 1: DISCOVER
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDiscover})
	allSessions, err := p.discover(ctx)
	if err != nil {
		emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiscover, Err: err})
		return nil, fmt.Errorf("pipeline discover: %w", err)
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiscover, Done: len(allSessions), Total: len(allSessions)})
	if p.config.PrepareSessionFilter != nil {
		if err := p.config.PrepareSessionFilter(ctx, allSessions); err != nil {
			return nil, fmt.Errorf("pipeline prepare session filter after discovery: %w", err)
		}
	}

	// Fill in the verdict on every session an EARLIER run stored, before this
	// run writes any session of its own.
	//
	// The order is the safety property, not a convenience: the pass sees only
	// rows a previous run persisted, and a parent identifier is written at
	// insert time and never updated afterwards, so a row this pass finalises
	// cannot acquire a parent later. It rides the discovery that has just run,
	// so the evidence cache it reads is already warm and no transcript is opened
	// twice.
	p.originResolve, p.originResolveErr = p.resolveStoredOrigins(ctx)

	// Pre-DIFF: bulk-load session locations from DB to avoid per-session queries
	// in findMetadataPath. A single SELECT ... WHERE session_id IN (...) replaces
	// 4010× ReadDir + N× Stat calls, making the DIFF stage O(1) per session.
	if p.store != nil {
		ids := make([]SessionID, len(allSessions))
		for i, s := range allSessions {
			ids[i] = s.SessionID
		}
		cache, err := p.store.BulkLookupSessionLocations(ctx, ids)
		if err == nil {
			p.locationCache = cache
		}
		// On error, locationCache stays nil and findMetadataPath falls back to walk.

		// Load the OpenCode change cursor when the store records it, so the DIFF
		// stage can re-ingest a session whose newest event sequence moved past the
		// last ingested value even when no time column changed. A store without the
		// cursor capability keeps the clock-only behaviour.
		if cursorStore, ok := p.store.(OpenCodeSeqCursorStore); ok {
			if cursors, cursorErr := cursorStore.BulkLookupOpenCodeSeqCursors(ctx, ids); cursorErr == nil {
				p.seqCursorCache = cursors
			}
		}
	}

	// Stage 2: DIFF
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: len(allSessions)})
	diffResult := p.diff(allSessions)
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiff, Done: len(diffResult.Sessions), Total: len(diffResult.Sessions)})

	// Stage 3: FILTER + Stage 4a: EXTRACT + WRITE
	//
	// Pre-pass: identify active sessions that are required as parents by
	// sessions that will be processed (New, Updated). These active parents
	// must be ingested so the DB FK constraint on parent_id is satisfied.
	requiredParents := make(map[SessionID]bool)
	toProcess := 0
	for _, entry := range diffResult.Sessions {
		if entry.Status == DiffNew || entry.Status == DiffUpdated {
			toProcess++
			if entry.Session.ParentUUID != nil {
				requiredParents[*entry.Session.ParentUUID] = true
			}
		} else if entry.Status == DiffActive {
			toProcess++ // may be included as required parent
		}
	}

	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageFilter, Total: len(diffResult.Sessions)})
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageFilter, Done: toProcess, Total: len(diffResult.Sessions)})

	// (indexedMeta defined at package level for use by helper methods)

	// Separate sessions into two buckets:
	//   skipped  — Unchanged or Active-but-not-required: recorded as-is, no processing.
	//   toProcess — New, Updated, and Active-required: run through processSession.
	var sessionResults []SessionResult
	var indexSessions []indexedMeta // sessions to index after write
	var toProcessEntries []DiffEntry
	dryRunSessions := make([]SessionResult, 0, len(diffResult.Sessions))
	recordDryRun := func(entry DiffEntry, status DiffStatus) {
		if !p.config.DryRun {
			return
		}
		dryRunSessions = append(dryRunSessions, SessionResult{
			SessionID:  entry.Session.SessionID,
			Harness:    entry.Session.Harness,
			ParentUUID: entry.Session.ParentUUID,
			Status:     status,
		})
	}

	// Track which root sessions passed the selection filter so subagents
	// can inherit their parent's fate (rejected parent → rejected children).
	filterPassedParents := make(map[SessionID]bool)

	for _, entry := range diffResult.Sessions {
		// AllowedSessionIDs filter: skip sessions not in the allowed set.
		if p.config.AllowedSessionIDs != nil && !p.config.AllowedSessionIDs[entry.Session.SessionID] {
			recordDryRun(entry, DiffUnchanged)
			sessionResults = append(sessionResults, SessionResult{
				SessionID:  entry.Session.SessionID,
				Harness:    entry.Session.Harness,
				ParentUUID: entry.Session.ParentUUID,
				Status:     DiffUnchanged,
			})
			continue
		}

		// Exact child denials take precedence over inherited parent admission.
		// The callback is lookup-only over the cohort prepared after discovery.
		if p.config.SessionExclusionFilter != nil && p.config.SessionExclusionFilter(entry.Session) {
			recordDryRun(entry, DiffUnchanged)
			sessionResults = append(sessionResults, SessionResult{
				SessionID:  entry.Session.SessionID,
				Harness:    entry.Session.Harness,
				ParentUUID: entry.Session.ParentUUID,
				Status:     DiffUnchanged,
			})
			continue
		}

		// SessionFilter: skip sessions rejected by the selection index filter.
		// Root sessions are checked against the filter; subagents inherit their
		// parent's result — if the parent was rejected, the subagent is too.
		if p.config.SessionFilter != nil {
			if entry.Session.ParentUUID == nil {
				if !p.config.SessionFilter(entry.Session) {
					recordDryRun(entry, DiffUnchanged)
					sessionResults = append(sessionResults, SessionResult{
						SessionID:  entry.Session.SessionID,
						Harness:    entry.Session.Harness,
						ParentUUID: entry.Session.ParentUUID,
						Status:     DiffUnchanged,
					})
					continue
				}
				filterPassedParents[entry.Session.SessionID] = true
			} else if !filterPassedParents[*entry.Session.ParentUUID] {
				recordDryRun(entry, DiffUnchanged)
				sessionResults = append(sessionResults, SessionResult{
					SessionID:  entry.Session.SessionID,
					Harness:    entry.Session.Harness,
					ParentUUID: entry.Session.ParentUUID,
					Status:     DiffUnchanged,
				})
				continue
			}
		}

		// Since filter: skip sessions older than the cutoff.
		if p.config.Since != nil {
			sessionTime := entry.Session.ModTime
			if !entry.Session.CreatedAt.IsZero() {
				sessionTime = entry.Session.CreatedAt
			}
			if sessionTime.Before(*p.config.Since) {
				recordDryRun(entry, DiffUnchanged)
				sessionResults = append(sessionResults, SessionResult{
					SessionID:  entry.Session.SessionID,
					Harness:    entry.Session.Harness,
					ParentUUID: entry.Session.ParentUUID,
					Status:     DiffUnchanged,
				})
				continue
			}
		}

		switch entry.Status {
		case DiffUnchanged:
			recordDryRun(entry, DiffUnchanged)
			sessionResults = append(sessionResults, SessionResult{
				SessionID:  entry.Session.SessionID,
				Harness:    entry.Session.Harness,
				ParentUUID: entry.Session.ParentUUID,
				Status:     DiffUnchanged,
			})
		case DiffActive:
			recordDryRun(entry, DiffActive)
			if !p.config.IncludeActive && !requiredParents[entry.Session.SessionID] {
				sessionResults = append(sessionResults, SessionResult{
					SessionID:  entry.Session.SessionID,
					Harness:    entry.Session.Harness,
					ParentUUID: entry.Session.ParentUUID,
					Status:     DiffActive,
				})
			} else {
				toProcessEntries = append(toProcessEntries, entry)
			}
		default: // DiffNew, DiffUpdated
			recordDryRun(entry, entry.Status)
			toProcessEntries = append(toProcessEntries, entry)
		}
	}

	// Dry-run uses the same allowed-session, time, positive-selection, exact-
	// denial, and parent-inheritance decisions as a real run. It stops only after
	// that shared FILTER pass and performs no extraction, write, or store action.
	if p.config.DryRun {
		result := &PipelineResult{
			Duration:             time.Since(start),
			Sessions:             dryRunSessions,
			DiscoveryDiagnostics: p.discoveryDiagnostics,
		}
		result.Summary.ReminedEvidenceRecords = p.reminedEvidence
		result.Summary.OriginResolve = p.originResolve
		result.Summary.OriginResolveError = p.originResolveErr
		for _, session := range dryRunSessions {
			switch session.Status {
			case DiffNew:
				result.Summary.New++
			case DiffUpdated:
				result.Summary.Updated++
			case DiffUnchanged:
				result.Summary.Unchanged++
			case DiffActive:
				result.Summary.Active++
			}
		}
		return result, nil
	}

	// Stage 4a: EXTRACT + WRITE — parallel pool, one goroutine per root session.
	//
	// Each root session owns its full subagent subtree. Within one goroutine,
	// the root is processed first, then each child is processed in series.
	// This eliminates the directory race where a parent's RemoveAll races with
	// a child's MkdirAll on the same {hostSlug}/{parentID}/ path.
	//
	// Parent→children index: built in O(n). Only root entries (no in-batch
	// parent) are dispatched to runParallel; children and all descendants are
	// processed inline by the root's goroutine via BFS.
	//
	// The StagingBuffer still enforces parent-before-child ordering at DB
	// INSERT time (FK constraint), just as before.
	entryByID := make(map[SessionID]DiffEntry, len(toProcessEntries))
	childrenOf := make(map[SessionID][]SessionID, len(toProcessEntries))
	inBatch := make(map[SessionID]bool, len(toProcessEntries))
	externalParents := make(map[SessionID]struct{})
	for _, e := range toProcessEntries {
		inBatch[e.Session.SessionID] = true
		entryByID[e.Session.SessionID] = e
	}
	var rootEntries []DiffEntry
	for _, e := range toProcessEntries {
		if e.Session.ParentUUID != nil {
			pid := *e.Session.ParentUUID
			if inBatch[pid] {
				childrenOf[pid] = append(childrenOf[pid], e.Session.SessionID)
				continue // child: will be processed by its root's goroutine
			}
			externalParents[pid] = struct{}{}
		}
		rootEntries = append(rootEntries, e)
	}

	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageExtract, Total: toProcess})
	var extractDoneAtomic atomic.Int64
	workers := parallelWorkers(p.config)

	// StagingBuffer holds completed workerResults until their parent is DB-committed.
	// Capacity = number of sessions to process; arena defaults to 2 GiB, overridable
	// via EnvArenaSizeBytes (tests set a few MiB — see resolveArenaSizeBytes).
	staging := NewStagingBuffer(len(toProcessEntries)+1, resolveArenaSizeBytes(DefaultArenaSizeBytes))
	for parentID := range externalParents {
		// The parent is outside this batch, so DB insertion is the authority on
		// whether it already exists. Mark it committed only for staging order.
		// A parent that is neither in this batch nor already stored does not
		// fail an FK: the store skips the child row instead, so discovery must
		// not set a ParentUUID whose parent it did not discover.
		staging.Commit(parentID)
	}

	// Fan out: one goroutine per root. Each goroutine processes the root then
	// its entire subtree via BFS, so no two goroutines ever write to the same
	// {hostSlug}/{parentID}/ directory tree concurrently.
	// Stage 4a: EXTRACT+WRITE — workers run in a goroutine so drain can run
	// concurrently. StagingBuffer is MPMC: Add (workers) and Drain/Commit (main
	// goroutine) must overlap so the ring buffer can recycle arena space.
	// Running runParallel synchronously and draining after it returns would
	// deadlock once the 2 GiB arena fills — workers spin in copyToArena waiting
	// for arenaTail to advance, but drain only starts after runParallel returns.
	var workersDone atomic.Bool
	indexCh := make(chan DrainBatch, 1)
	indexDoneCh := make(chan struct{}, 1)
	// errCh buffer sized to the maximum number of drain batches that can be
	// emitted (ceil(toProcess/DefaultMaxDrainBatch)), plus a safety margin.
	// Without dynamic sizing the channel could block the drain goroutine if
	// every batch fails DB INSERT — stalling the consumer until the controller
	// drains errCh (which only runs after wg.Wait finishes), causing deadlock.
	errChSize := len(toProcessEntries)/DefaultMaxDrainBatch + 2
	if errChSize < 16 {
		errChSize = 16
	}
	errCh := make(chan error, errChSize)

	var wg sync.WaitGroup
	var drainResults []SessionResult
	var drainIndexed []indexedMeta
	var drainIndexLogEntries []IndexLogEntry

	// Stage 4a: EXTRACT+WRITE workers goroutine.
	// Processes all root entries and their subtrees in parallel, writing to staging.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runParallel(ctx.Err, rootEntries, workers, func(entry DiffEntry) workerResult {
			// Process root.
			wr := p.processSession(ctx, entry)
			done := int(extractDoneAtomic.Add(1))
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageExtract, Done: done, Total: toProcess})
			staging.Add(wr)
			// BFS over subtree: process all descendants inline (same goroutine →
			// no directory races on the parent's {hostSlug}/{parentID}/ tree).
			queue := childrenOf[entry.Session.SessionID]
			for len(queue) > 0 {
				childID := queue[0]
				queue = queue[1:]
				childEntry := entryByID[childID]
				cwr := p.processSession(ctx, childEntry)
				childDone := int(extractDoneAtomic.Add(1))
				emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageExtract, Done: childDone, Total: toProcess})
				staging.Add(cwr)
				// Enqueue grandchildren (if any).
				queue = append(queue, childrenOf[childID]...)
			}
			// Nil out transcriptData: the arena already holds a copy (via
			// staging.Add → copyToArena). runParallel stores the return value
			// in its results slice, so keeping the heap bytes alive here
			// doubles memory usage for every session until runParallel returns.
			wr.transcriptData = nil
			return wr
		})
		extractDone := int(extractDoneAtomic.Load())
		emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageExtract, Done: extractDone, Total: toProcess})
		workersDone.Store(true)
	}()

	// Stage 4b: Consumer goroutine — DB INSERT + INDEX coordination.
	// Drains staging, inserts to DB, coordinates handoff to INDEX goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(indexCh) // signal INDEX goroutine to stop when consumer exits
		emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDBInsert, Total: len(toProcessEntries)})
		drainResults = p.drainLoop(ctx, staging, &workersDone, indexCh, indexDoneCh, errCh, prog, len(toProcessEntries))
		emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDBInsert, Done: len(drainResults), Total: len(toProcessEntries)})
	}()

	// INDEX goroutine: reads indexed metadata from consumer and indexes each session.
	// Arena data is still valid (AckBatch has not been called yet when indexLoop reads).
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: len(toProcessEntries)})
	wg.Add(1)
	go func() {
		defer wg.Done()
		drainIndexed, drainIndexLogEntries = p.indexLoop(ctx, indexCh, indexDoneCh, prog, IndexOutcomeIndexed, "pipeline")
	}()

	// Controller: wait for all goroutines to complete.
	wg.Wait()
	close(errCh)

	// Collect store errors (last error wins — existing behavior).
	var storeErr error
	for err := range errCh {
		storeErr = err
	}

	// Merge goroutine session results into main result slice.
	sessionResults = append(sessionResults, drainResults...)

	// Stage 4c: AUTO-DETECT stale index sessions (post-FILTER).
	// Query DB for sessions with index_version < CurrentIndexVersion.
	// For each stale session, read metadata from peasant-sync output to reconstruct
	// a DiscoveredSession, then append to indexSessions (skips EXTRACT+WRITE,
	// goes straight to INDEX+COMPUTE).
	if p.metricsStore != nil {
		staleIDs, staleErr := p.metricsStore.ListStaleIndexSessions(ctx, CurrentIndexVersion)
		if staleErr != nil {
			slog.Warn("pipeline: list stale index sessions",
				"error", staleErr,
				"what", "failed to query sessions needing re-indexing",
				"why", "DB query error on sessions table",
				"user_impact", "sessions indexed with older logic will not be auto-upgraded",
				"how_to_fix", "run peasant ingest index --all to force re-index all sessions")
		}
		// Build a set of already-queued session IDs for O(1) lookup.
		queued := make(map[SessionID]bool, len(indexSessions))
		for _, im := range indexSessions {
			queued[im.session.SessionID] = true
		}
		for _, sid := range staleIDs {
			if queued[sid] {
				continue
			}

			// Reconstruct DiscoveredSession from peasant-sync metadata.
			reconstructed, startMs, transcriptPath := p.reconstructFromMetadata(ctx, sid)
			if reconstructed == nil {
				// Fallback: reconstruct from DB source_path/source_format.
				// This handles sessions (e.g. subagents) that were indexed from
				// the original source but don't have peasant-sync metadata.
				reconstructed, startMs, transcriptPath = p.reconstructFromSourceInfo(ctx, sid)
				if reconstructed == nil {
					continue
				}
			}
			indexSessions = append(indexSessions, indexedMeta{
				session:              *reconstructed,
				startMs:              startMs,
				outputTranscriptPath: transcriptPath,
			})
		}
	}

	// Stages 5-9: INDEX, COMPUTE, CLEANUP, REPORT, AUDIT (shared with runReindex).
	return p.indexComputeAndFinalize(ctx, indexSessions, drainIndexed, sessionResults, storeErr, start, drainIndexLogEntries, IndexOutcomeIndexed, "pipeline")
}

// drainLoop is the consumer goroutine for stage 4b (DB INSERT).
//
// It loops until all workers have finished and the staging buffer is empty,
// draining completed workerResults in batches. For each batch it:
//  1. Performs DB INSERT (InsertSessions + UpsertSessionCommits) — best-effort.
//  2. Calls staging.Commit(ids) — marks sessions as DB-committed so their
//     children become eligible for the next Drain (Commit BEFORE Ack is deliberate).
//  3. Waits for the previous INDEX batch to complete via indexDoneCh.
//  4. Calls staging.AckBatch(prev) — frees arena space only AFTER INDEX has read it.
//  5. Sends the current batch (with Metas populated) to the INDEX goroutine via indexCh.
//
// Store errors are sent to errCh (buffered); the controller collects them after wg.Wait.
// Bounded backoff (1ms sleep) is used when the buffer is empty but workers are still running.
func (p *Pipeline) drainLoop(
	ctx context.Context,
	staging *StagingBuffer,
	workersDone *atomic.Bool,
	indexCh chan<- DrainBatch,
	indexDoneCh <-chan struct{},
	errCh chan<- error,
	prog *ProgressState,
	toProcess int,
) (sessionResults []SessionResult) {
	var pendingAck *DrainBatch
	dbInsertDone := 0

	for {
		done := workersDone.Load()
		batch := staging.Drain()

		if len(batch.Results) > 0 {
			var storeBatch []StoreEntry
			var batchMetas []indexedMeta
			var committedIDs []SessionID
			for _, wr := range batch.Results {
				sessionResults = append(sessionResults, wr.result)
				if wr.result.Error == nil && wr.result.OutputPath != "" && wr.meta != nil {
					batchMetas = append(batchMetas, indexedMeta{
						session:              sessionFromWorkerResult(wr),
						startMs:              wr.startMs,
						outputTranscriptPath: wr.outputTranscriptPath,
						transcriptData:       wr.transcriptData,
					})
					if p.store != nil {
						storeBatch = append(storeBatch, StoreEntry{
							Metadata: wr.meta,
							Session:  sessionFromWorkerResult(wr),
						})
					}
				}
				committedIDs = append(committedIDs, wr.result.SessionID)
			}

			// DB INSERT (best-effort).
			if p.store != nil && len(storeBatch) > 0 {
				if err := p.store.InsertSessions(ctx, storeBatch); err != nil {
					errCh <- fmt.Errorf("store insert (%d sessions): %w", len(storeBatch), err)
				} else {
					// Persist session commits (non-fatal): runs only after InsertSessions
					// succeeds so the FK constraint on session_commits(session_id) is satisfied.
					// Called unconditionally (including empty slice) so that a --force re-ingest
					// that finds 0 commits deletes stale DB rows, keeping JSON and DB in sync.
					cursorStore, cursorStoreOK := p.store.(OpenCodeSeqCursorStore)
					for _, entry := range storeBatch {
						if err := p.store.UpsertSessionCommits(ctx, entry.Metadata.SessionID, entry.Metadata.Git.Commits); err != nil {
							slog.Warn("pipeline: upsert session_commits",
								"session_id", entry.Metadata.SessionID,
								"error", err)
						}
						// Record the OpenCode change cursor for a session just ingested,
						// so a later in-place rewrite that bumps the sequence without
						// moving a time column re-ingests it. Non-fatal.
						if cursorStoreOK && entry.Session.Harness == HarnessOpenCode {
							if err := cursorStore.UpsertOpenCodeSeqCursor(ctx, entry.Metadata.SessionID, entry.Session.EventSeq); err != nil {
								slog.Warn("pipeline: upsert opencode_session_seq_cursor",
									"session_id", entry.Metadata.SessionID,
									"error", err)
							}
						}
					}
				}
			}

			// WRITE metadata.json (v8 write order: DB INSERT first, file second).
			//
			// For each successful worker result, set DerivedAt (if store configured)
			// and write metadata.json to the session directory. This is the canonical
			// write path for metadata — processSession writes only the transcript.
			//
			// Best-effort: a metadata write failure is logged and recorded in
			// SessionResult.Error, but does not abort the pipeline.
			for i := range batch.Results {
				wr := &batch.Results[i]
				if wr.result.Error != nil || wr.meta == nil || wr.metaFilename == "" || wr.sessionDir == "" {
					continue
				}
				// Set DerivedAt to mark metadata.json as derived from DB state (v8+).
				// Nil when no store is configured (file is primary artifact, not a cache).
				if p.store != nil {
					ts := time.Now().UnixMilli()
					wr.meta.DerivedAt = &ts
				}
				metaJSON, err := json.Marshal(wr.meta)
				if err != nil {
					slog.Warn("pipeline: marshal metadata",
						"session_id", wr.result.SessionID,
						"error", err)
					continue
				}
				metaPath := fmt.Sprintf("%s/%s", wr.sessionDir, wr.metaFilename)
				if err := p.fs.WriteFile(metaPath, metaJSON, defaults.PrivateFilePerm); err != nil {
					slog.Warn("pipeline: write metadata.json",
						"session_id", wr.result.SessionID,
						"path", metaPath,
						"error", err)
				}
			}

			// Commit BEFORE Ack — unlocks children for next Drain sooner.
			staging.Commit(committedIDs...)

			// Wait for previous INDEX batch to finish, then release its arena space.
			if pendingAck != nil {
				<-indexDoneCh
				staging.AckBatch(*pendingAck)
			}

			// Send current batch to INDEX goroutine (arena data still valid — not yet Acked).
			batch.Metas = batchMetas
			indexCh <- batch
			pendingAck = &batch
			dbInsertDone += len(batch.Results)
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageDBInsert, Done: dbInsertDone, Total: toProcess})
			continue
		}

		if done {
			// Workers finished and buffer is empty — wait for final INDEX batch then exit.
			if pendingAck != nil {
				<-indexDoneCh
				staging.AckBatch(*pendingAck)
			}
			break
		}
		// Bounded backoff: avoid spinning when buffer is empty but workers still running.
		time.Sleep(1 * time.Millisecond)
	}
	return sessionResults
}

// indexLoop is the INDEX goroutine for stage 4b.
//
// It reads DrainBatch values from indexCh (sent by the consumer goroutine). With
// multiple workers it parses the batch in parallel with runParallel, then writes
// entries serially so SQLite still has one writer. Arena data is still valid
// while parsers read it - the consumer waits for indexDoneCh before AckBatch can
// release this batch.
//
// outcome and logPrefix are forwarded to the batch indexer for log annotation;
// use IndexOutcomeIndexed/"pipeline" for normal ingest and
// IndexOutcomeReindexed/"reindex" for --reindex runs.
//
// indexLoop has no panic recovery: crashes are intentional (per design decision).
func (p *Pipeline) indexLoop(
	ctx context.Context,
	indexCh <-chan DrainBatch,
	indexDoneCh chan<- struct{},
	prog *ProgressState,
	outcome IndexOutcome,
	logPrefix string,
) (indexed []indexedMeta, logEntries []IndexLogEntry) {
	indexDone := 0
	for batch := range indexCh {
		batchIndexed, batchLogs := p.indexBatch(ctx, batch.Metas, outcome, logPrefix)
		for i, result := range batchIndexed {
			logEntry := batchLogs[i]
			if logEntry.SessionID != "" {
				logEntries = append(logEntries, logEntry)
			}
			indexed = append(indexed, result)
			indexDone++
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageIndex, Done: indexDone})
		}
		indexDoneCh <- struct{}{} // signal batch complete to consumer
	}
	return
}

type indexParseResult struct {
	im            indexedMeta
	entries       []schema.SessionEntry
	startedAt     int64
	logEntry      IndexLogEntry
	parseDuration time.Duration
	bytes         int64
}

// indexBatch parses a batch, then serializes SQLite writes through one goroutine.
func (p *Pipeline) indexBatch(ctx context.Context, metas []indexedMeta, outcome IndexOutcome, logPrefix string) ([]indexedMeta, []IndexLogEntry) {
	indexed := make([]indexedMeta, 0, len(metas))
	logs := make([]IndexLogEntry, 0, len(metas))
	if len(metas) == 0 {
		return indexed, logs
	}
	if p.indexers == nil || p.metricsStore == nil {
		for _, im := range metas {
			indexed = append(indexed, indexedMeta{session: im.session, startMs: im.startMs})
			logs = append(logs, IndexLogEntry{})
		}
		return indexed, logs
	}

	var activeParses atomic.Int64
	var maxActiveParses atomic.Int64
	parseOne := func(im indexedMeta) indexParseResult {
		result := indexParseResult{im: im, startedAt: time.Now().UnixMilli(), bytes: p.indexProfileBytes(im)}
		indexer, ok := p.indexers[im.session.Harness]
		if !ok {
			reason := "no indexer for provider"
			result.logEntry = p.makeIndexLogEntry(im, IndexOutcomeSkipped, 0, result.startedAt, &reason, nil)
			return result
		}

		active := activeParses.Add(1)
		recordIndexProfileMax(&maxActiveParses, active)
		parseStart := time.Now()
		entries, err := indexWithSourceKind(ctx, indexer, indexTargetSession(im), im.transcriptData)
		result.parseDuration = time.Since(parseStart)
		activeParses.Add(-1)
		if err != nil {
			slog.Warn(logPrefix+": index transcript", "session_id", im.session.SessionID, "error", err)
			errMsg := err.Error()
			result.logEntry = p.makeIndexLogEntry(im, IndexOutcomeError, 0, result.startedAt, nil, &errMsg)
			return result
		}
		if len(entries) == 0 {
			reason := "no entries returned"
			result.logEntry = p.makeIndexLogEntry(im, IndexOutcomeSkipped, 0, result.startedAt, &reason, nil)
			return result
		}
		result.entries = entries
		return result
	}

	workers := parallelWorkers(p.config)
	var parsed []indexParseResult
	if workers > 1 && len(metas) > 1 {
		parsed = runParallel(func() error { return nil }, metas, workers, parseOne)
	} else {
		parsed = make([]indexParseResult, 0, len(metas))
		for _, im := range metas {
			parsed = append(parsed, parseOne(im))
		}
	}

	profileSessions := make([]IndexProfileSession, 0, len(parsed))
	profileBatch := IndexProfileBatch{Source: logPrefix, Sessions: len(parsed), MaxParseWorkers: int(maxActiveParses.Load())}
	for _, result := range parsed {
		im := result.im
		ok := false
		logEntry := result.logEntry
		writeDuration := time.Duration(0)
		entriesCount := len(result.entries)
		if entriesCount > 0 {
			writeStart := time.Now()
			if err := p.metricsStore.IndexSessionEntries(ctx, im.session.SessionID, result.entries); err != nil {
				writeDuration = time.Since(writeStart)
				slog.Warn(logPrefix+": store session entries", "session_id", im.session.SessionID, "error", err)
				errMsg := err.Error()
				logEntry = p.makeIndexLogEntry(im, IndexOutcomeError, entriesCount, result.startedAt, nil, &errMsg)
			} else {
				indexedAtMs := time.Now().UnixMilli()
				if err := p.metricsStore.UpdateIndexState(ctx, im.session.SessionID, CurrentIndexVersion, indexedAtMs); err != nil {
					slog.Warn(logPrefix+": update index state", "session_id", im.session.SessionID, "error", err)
				}
				writeDuration = time.Since(writeStart)
				ok = true
				logEntry = p.makeIndexLogEntry(im, outcome, entriesCount, result.startedAt, nil, nil)
			}
		}

		indexed = append(indexed, indexedMeta{session: im.session, startMs: im.startMs, indexed: ok})
		logs = append(logs, logEntry)
		profileBatch.Entries += entriesCount
		profileBatch.Bytes += result.bytes
		profileBatch.ParseDuration += result.parseDuration
		profileBatch.WriteDuration += writeDuration
		profileSessions = append(profileSessions, IndexProfileSession{
			SessionID:     im.session.SessionID,
			Harness:       im.session.Harness,
			SourcePath:    p.indexProfileSourcePath(im),
			Outcome:       logEntry.Outcome,
			Entries:       entriesCount,
			Bytes:         result.bytes,
			ParseDuration: result.parseDuration,
			WriteDuration: writeDuration,
		})
	}
	p.config.IndexProfiler.Record(profileBatch, profileSessions)
	return indexed, logs
}

func recordIndexProfileMax(maxValue *atomic.Int64, value int64) {
	for {
		current := maxValue.Load()
		if value <= current || maxValue.CompareAndSwap(current, value) {
			return
		}
	}
}

func (p *Pipeline) indexProfileBytes(im indexedMeta) int64 {
	if len(im.transcriptData) > 0 {
		return int64(len(im.transcriptData))
	}
	if im.outputTranscriptPath == "" {
		return 0
	}
	info, err := p.fs.Stat(im.outputTranscriptPath)
	if err != nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

func (p *Pipeline) indexProfileSourcePath(im indexedMeta) string {
	if im.outputTranscriptPath != "" {
		return im.outputTranscriptPath
	}
	return string(im.session.SourcePath)
}

// collectSourcePaths gathers all configured source directory paths from the
// pipeline's provider configurations and returns them as a semicolon-joined
// string pointer (nil if no paths are configured).
func collectSourcePaths(sources map[Harness]SourceConfig) *string {
	var paths []string
	for _, cfg := range sources {
		if !cfg.Enabled {
			continue
		}
		for _, p := range cfg.Paths {
			paths = append(paths, p.String())
		}
	}
	if len(paths) == 0 {
		return nil
	}
	joined := strings.Join(paths, ";")
	return &joined
}

// discover runs all enabled provider adapters and collects discovered sessions.
// If some providers succeed and others fail, partial results are returned.
// Only returns an error if ALL providers fail.
func (p *Pipeline) discover(ctx context.Context) ([]DiscoveredSession, error) {
	var all []DiscoveredSession
	var providerErrors []error
	enabledCount := 0
	for provider, factory := range p.adapters {
		cfg, ok := p.config.Sources[provider]
		if !ok || !cfg.Enabled {
			continue
		}
		enabledCount++
		adapter := factory(p.fs, p.git, p.salt)
		// The local store doubles as the discovery evidence cache, so an
		// unchanged transcript is never read and parsed again.
		if cache, ok := p.store.(ClaudeEvidenceCache); ok {
			AttachClaudeEvidenceCache(adapter, cache)
		}
		// The local store also answers where a session id already lives, so
		// cross-run linking can confirm a candidate spawner is really
		// persisted before trusting it.
		if p.store != nil {
			AttachSessionLocationLookup(adapter, p.store)
		}
		sessions, err := adapter.Discover(ctx, cfg)
		// Per-location discovery failures are collected whether or not the whole
		// provider errored, so a partially-enumerated provider still reports the
		// databases it skipped.
		if reporter, ok := adapter.(DiscoveryDiagnosticReporter); ok {
			p.discoveryDiagnostics = append(p.discoveryDiagnostics, reporter.DiscoveryDiagnostics()...)
		}
		if err != nil {
			providerErrors = append(providerErrors, fmt.Errorf("discover %s: %w", provider, err))
			continue
		}
		// Read the re-mine count immediately after the Discover it describes:
		// DiscoveryStatistics scopes it to the most recent call, and the next
		// provider's adapter is a different object with its own count.
		if stats, ok := adapter.(DiscoveryStatistics); ok {
			p.reminedEvidence += stats.ReminedCount()
		}
		all = append(all, sessions...)
	}
	// If all enabled providers failed, return a combined error.
	if len(providerErrors) > 0 && len(providerErrors) == enabledCount {
		return nil, fmt.Errorf("all providers failed: %w", errors.Join(providerErrors...))
	}
	return all, nil
}

// resolveStoredOrigins runs the stored-row origin pass for this run.
//
// It is best effort, like the other store-backed stages: a store that cannot
// answer leaves its rows unjudged, and an unjudged row keeps the visible
// fail-safe value and is listed again on the next run. Losing a verdict is not
// possible here; only delaying one is.
func (p *Pipeline) resolveStoredOrigins(ctx context.Context) (ResolveReport, error) {
	backing, ok := p.store.(OriginResolverStore)
	if !ok {
		return ResolveReport{}, nil
	}
	cache, _ := p.store.(ClaudeEvidenceCache)
	resolver, err := NewOriginResolver(backing, cache, p.originEvidenceMiners())
	if err != nil {
		return ResolveReport{}, err
	}
	report, err := resolver.ResolveStoredOrigins(ctx, OriginRuleVersion)
	if err != nil {
		slog.Warn("ingest: resolve stored session origins",
			"error", err,
			"what", "some stored sessions did not get an origin verdict this run",
			"why", "the resolve pass stopped on a store error",
			"user_impact", "those sessions stay visible in every list, which is the fail-safe value, until a later run judges them",
			"how_to_fix", "re-run peasant ingest; the pass resumes at the first row it did not reach")
	}
	return report, err
}

// originEvidenceMiners builds the per-harness miners the resolve pass uses to
// re-read a stored transcript.
//
// It walks EVERY registered adapter, not only the enabled ones. A harness whose
// source is switched off still recorded transcripts that this build knows how to
// read, and treating those rows as unmineable would finalise them on the stored
// preview alone - exactly the outcome the degraded watermark exists to prevent.
func (p *Pipeline) originEvidenceMiners() map[Harness]OriginEvidenceMiner {
	miners := make(map[Harness]OriginEvidenceMiner, len(p.adapters))
	for harness, factory := range p.adapters {
		if miner, ok := factory(p.fs, p.git, p.salt).(OriginEvidenceMiner); ok {
			miners[harness] = miner
		}
	}
	return miners
}

// diff categorizes each discovered session.
func (p *Pipeline) diff(sessions []DiscoveredSession) DiffResult {
	result := DiffResult{
		Sessions: make([]DiffEntry, 0, len(sessions)),
	}

	for _, session := range sessions {
		status := p.classifySession(session)
		result.Sessions = append(result.Sessions, DiffEntry{
			Session: session,
			Status:  status,
		})
	}

	return result
}

// ClassifyAgainstStore returns the DiffStatus of a discovered session that the
// store already holds a record for: it compares the source file against the
// recorded ingest timestamp and metadata schema version, honouring the same
// staleness threshold. It is the DB-first branch of classifySession, exported so
// a caller that already knows what the store recorded (the kickstart re-scan)
// asks the pipeline's own diff rule instead of writing a second one that can
// drift from it.
//
// The caller supplies a location whose IngestedMs is set; a session with no
// store record is DiffNew by definition and never reaches here.
func ClassifyAgainstStore(session DiscoveredSession, loc SessionLocation, stalenessThreshold time.Duration) DiffStatus {
	isActive := stalenessThreshold > 0 && time.Since(session.stalenessSourceTime()) < stalenessThreshold

	if loc.IngestedMs != nil && *loc.IngestedMs > 0 {
		// Source modified more recently than DB ingested_ms: re-ingest.
		if session.ModTime.After(time.UnixMilli(*loc.IngestedMs)) {
			if isActive {
				return DiffActive
			}
			return DiffUpdated
		}
	}
	// Schema version behind current (DB value): re-ingest.
	if loc.SchemaVersion < CurrentSchemaVersion {
		if isActive {
			return DiffActive
		}
		return DiffUpdated
	}
	// Staleness check last.
	if isActive {
		return DiffActive
	}
	return DiffUnchanged
}

// classifySession determines the DiffStatus for a single session.
//
// The implementation uses a single staleness threshold check
// (time.Since(ModTime) < threshold). A future debounce for actively written
// sessions would need to poll ModTime twice with a bounded delay.
//
// Order of precedence (per spec):
//  1. --force flag: always DiffNew (but DiffActive takes priority over force if
//     IncludeActive is false, per "respect staleness unless --include-active")
//  2. No existing metadata: DiffNew
//  3. Source newer than last ingest: DiffUpdated
//  4. Schema version behind CurrentSchemaVersion: DiffUpdated
//  5. Source within staleness threshold (still being written): DiffActive
//  6. Otherwise: DiffUnchanged
func (p *Pipeline) classifySession(session DiscoveredSession) DiffStatus {
	isActive := p.config.StalenessThreshold > 0 && time.Since(session.stalenessSourceTime()) < p.config.StalenessThreshold

	// Force: re-ingest, but only if we're also including active sessions (or
	// the session is not active). With --force and without --include-active,
	// active sessions are still skipped.
	if p.config.Force {
		if isActive {
			return DiffActive
		}
		return DiffNew
	}

	// DB-first diff (v8+): if locationCache has a record with IngestedMs and SchemaVersion,
	// use DB state to classify the session without reading metadata.json from disk.
	// This is the primary code path for sessions already in the DB.
	if loc, ok := p.locationCache[session.SessionID]; ok && loc.IngestedMs != nil {
		status := ClassifyAgainstStore(session, loc, p.config.StalenessThreshold)
		// The change cursor is an additional trigger on top of the clock: a session
		// the clock reports unchanged is re-ingested when its newest event sequence
		// moved past the last ingested value, catching an in-place rewrite that
		// moved no time column. It only fires for a session that already has a
		// stored cursor, so a first sighting never mass-re-ingests.
		if status == DiffUnchanged {
			if storedSeq, tracked := p.seqCursorCache[session.SessionID]; tracked && session.EventSeq > storedSeq {
				if isActive {
					return DiffActive
				}
				return DiffUpdated
			}
		}
		return status
	}

	// File fallback: DB has no record for this session (pre-migration data or first run).
	// Read metadata.json from disk for backward compat (preserves pre-v8 behavior).
	metaPath := p.findMetadataPath(session)
	if metaPath == "" {
		// No metadata file found; new session.
		// Still respect staleness for new sessions.
		if isActive {
			return DiffActive
		}
		return DiffNew
	}

	data, err := p.fs.ReadFile(metaPath)
	if err != nil {
		if isActive {
			return DiffActive
		}
		return DiffNew
	}

	// Parse existing metadata to compare.
	var existing UnifiedMetadata
	if err := json.Unmarshal(data, &existing); err != nil {
		// Corrupt metadata: re-ingest.
		if isActive {
			return DiffActive
		}
		return DiffNew
	}

	// Source modified more recently than ingest time: re-ingest.
	if existing.Timestamp.Ingested != nil {
		ingestedMs := *existing.Timestamp.Ingested
		if ingestedMs > 0 {
			ingestedTime := time.UnixMilli(ingestedMs)
			if session.ModTime.After(ingestedTime) {
				if isActive {
					return DiffActive
				}
				return DiffUpdated
			}
		}
	}

	// Schema version behind current: re-ingest.
	if existing.SchemaVersion < CurrentSchemaVersion {
		if isActive {
			return DiffActive
		}
		return DiffUpdated
	}

	// Staleness check last: file may be being written but already ingested.
	if isActive {
		return DiffActive
	}

	return DiffUnchanged
}

// findMetadataPath searches for an existing metadata file for a session.
//
// Output structure is {outputDir}/{hostSlug}/{sessionId}/{sessionId}--metadata.json.
// Since the hostSlug is not known during the diff phase (it requires extraction),
// we walk one level of the outputDir looking for a matching sessionId directory.
// Returns empty string if no metadata file is found.
func (p *Pipeline) findMetadataPath(session DiscoveredSession) string {
	outputDir := string(p.config.OutputDir)
	metaFilename := fmt.Sprintf("%s%s", session.SessionID, defaults.MetadataSuffix)

	// Fast path: if the session is in the pre-populated location cache, we know
	// its host_slug. Construct the path directly and Stat just that one file.
	// The cache is populated by BulkLookupSessionLocations before the DIFF stage,
	// replacing 4010× ReadDir with a single DB query + O(1) map lookups.
	if loc, ok := p.locationCache[session.SessionID]; ok && loc.HostSlug != "" {
		var candidate string
		if loc.ParentID != "" {
			candidate = fmt.Sprintf("%s/%s/%s/%s/%s/%s", outputDir, loc.HostSlug, loc.ParentID, defaults.DirSubagents.String(), session.SessionID, metaFilename)
		} else {
			candidate = fmt.Sprintf("%s/%s/%s/%s", outputDir, loc.HostSlug, session.SessionID, metaFilename)
		}
		if _, err := p.fs.Stat(candidate); err == nil {
			return candidate
		}
		// File not at expected path (e.g. output moved) — fall through to walk.
	}

	entries, err := p.fs.ReadDir(outputDir)
	if err != nil {
		return "" // outputDir doesn't exist yet
	}

	for _, hostEntry := range entries {
		if !hostEntry.IsDir() {
			continue
		}
		hostDir := fmt.Sprintf("%s/%s", outputDir, hostEntry.Name())

		// Check flat layout: {hostSlug}/{sessionID}/{metaFilename}
		candidate := fmt.Sprintf("%s/%s/%s", hostDir, session.SessionID, metaFilename)
		if _, err := p.fs.Stat(candidate); err == nil {
			return candidate
		}

		// Check nested subagent layout: {hostSlug}/{parentID}/subagents/{sessionID}/{metaFilename}
		sessionEntries, err := p.fs.ReadDir(hostDir)
		if err != nil {
			continue
		}
		for _, sessionEntry := range sessionEntries {
			if !sessionEntry.IsDir() {
				continue
			}
			nested := fmt.Sprintf("%s/%s/%s/%s/%s", hostDir, sessionEntry.Name(), defaults.DirSubagents.String(), session.SessionID, metaFilename)
			if _, err := p.fs.Stat(nested); err == nil {
				return nested
			}
		}
	}

	return "" // not found
}

// processSession extracts metadata and atomically writes output for one session.
// Returns a workerResult carrying the SessionResult, metadata, redacted transcript
// bytes (for JSONL/JSON providers — nil for directory-based providers), the output
// transcript path, and the session start timestamp.
//
// The transcript bytes returned are the copy written to disk, which is the
// transcript as recorded - no level a user can choose redacts content here.
// Callers may store them in a StagingBuffer arena to avoid re-reading from disk
// during the INDEX stage.
func (p *Pipeline) processSession(ctx context.Context, entry DiffEntry) workerResult {
	session := entry.Session
	result := SessionResult{
		SessionID:  session.SessionID,
		Harness:    session.Harness,
		ParentUUID: session.ParentUUID,
		Status:     entry.Status,
	}
	fail := func(err error) workerResult {
		result.Error = err
		return workerResult{result: result}
	}

	// Find the adapter for this provider.
	factory, ok := p.adapters[session.Harness]
	if !ok {
		return fail(fmt.Errorf("no adapter for provider %s", session.Harness))
	}
	adapter := factory(p.fs, p.git, p.salt)

	if err := session.TranscriptOrigin.Validate(); err != nil {
		return fail(fmt.Errorf("prepare transcript for session %s failed before source access: %w; the session was not written or stored; update the discovering adapter to return a supported typed origin", session.SessionID, err))
	}
	var rawData []byte
	var meta *UnifiedMetadata
	var err error
	if session.TranscriptOrigin != TranscriptOriginFile {
		materializer, ok := adapter.(TranscriptMaterializer)
		if !ok {
			return fail(fmt.Errorf("materialize transcript for session %s failed before source access: typed transcript origin %d requires a managed materializer but adapter %T has none; raw database bytes were not read or copied and no managed state was written; use the production OpenCode adapter", session.SessionID, session.TranscriptOrigin, adapter))
		}
		meta, rawData, err = materializer.MaterializeTranscript(ctx, session)
	} else {
		meta, err = adapter.ExtractMetadata(ctx, session)
	}
	if err != nil {
		return fail(fmt.Errorf("extract metadata and transcript for %s: %w", session.SessionID, err))
	}

	// Set ingested timestamp.
	ingested := time.Now().UnixMilli()
	meta.Timestamp.Ingested = &ingested

	// Read host slug from metadata (populated by adapter).
	if meta.HostSlug == "" {
		return fail(fmt.Errorf("empty hostSlug in metadata for %s", session.SessionID))
	}
	hostSlug := meta.HostSlug

	// Compute output paths.
	outputDir := string(p.config.OutputDir)
	parentID := ""
	if session.ParentUUID != nil {
		parentID = string(*session.ParentUUID)
	}
	sessionDir := SessionDir(outputDir, string(hostSlug), string(session.SessionID), parentID)
	metaFilename := fmt.Sprintf("%s%s", session.SessionID, defaults.MetadataSuffix)

	// Determine transcript filename: {sessionId}--transcript.{ext}
	ext := string(session.SourceFormat)
	transcriptFilename := fmt.Sprintf("%s--transcript.%s", session.SessionID, ext)

	// Create a temp directory for atomic write.
	tmpSuffix, err := randomHex(defaults.TempSuffixLen)
	if err != nil {
		return fail(fmt.Errorf("generate temp suffix for %s: %w", session.SessionID, err))
	}
	// NOTE (M14): tmpDir is placed at {outputDir}/.tmp-{sessionId}-{random}.
	// cleanOrphans() scans {outputDir} for .tmp-* prefixed dirs. If tmpDir placement
	// changes, cleanOrphans() must be updated to match.
	// NOTE (M2): RFC Section 7.3 specifies temp dirs under {basePath}/{hostSlug}/.tmp-...,
	// but we place them at {basePath}/.tmp-... for simplicity. This is functionally equivalent
	// since both locations are on the same filesystem (same-device rename guarantee).
	tmpDir := fmt.Sprintf("%s/%s%s-%s", outputDir, defaults.TempDirPrefix, session.SessionID, tmpSuffix)

	// Create temp dir.
	if err := p.fs.MkdirAll(tmpDir, defaults.PrivateDirPerm); err != nil {
		return fail(fmt.Errorf("create temp dir for %s: %w", session.SessionID, err))
	}

	// Read transcript, optionally redact, and write to temp dir in one pass.
	// The redacted bytes are returned in workerResult.transcriptData so the
	// caller (StagingBuffer path) can hand them to the indexer without a
	// second disk read.
	//
	// For JSONL/JSON providers (Claude): redact produces the output bytes
	// directly; we write those bytes to disk. transcriptData = redacted bytes.
	// For directory-based providers (OpenCode): no single transcript file to
	// redact here; transcriptData = nil (indexer reads from outputTranscriptPath).
	tmpTranscriptPath := fmt.Sprintf("%s/%s", tmpDir, transcriptFilename)
	var transcriptData []byte

	if session.TranscriptOrigin == TranscriptOriginFile {
		rawData, err = p.fs.ReadFile(string(session.SourcePath))
		if err != nil {
			result.Error = errors.Join(
				fmt.Errorf("read transcript for %s: %w", session.SessionID, err),
				p.fs.RemoveAll(tmpDir),
			)
			return workerResult{result: result}
		}
	}
	if session.Harness == HarnessStrike && session.SourceFormat == SourceFormatJSONL {
		var diagnostics []DiagnosticEntry
		rawData, diagnostics = filterStrikeOversizedRecords(rawData, session.SourcePath.String())
		if len(diagnostics) > 0 {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, diagnostics...)
			partial := true
			meta.Diagnostics.Partial = &partial
		}
	}

	// Redact in-place: produce output bytes once, write once, keep for caller.
	writeData := rawData
	if p.redactor != nil {
		switch session.SourceFormat {
		case SourceFormatJSONL:
			redacted, redactErr := redact.RedactJSONLBytes(p.redactor, rawData, redact.WithRedactScannerBufSize(defaults.ScannerInitBuf, defaults.ScannerMaxLine))
			if redactErr != nil {
				result.Error = errors.Join(
					fmt.Errorf("redact transcript for %s: %w", session.SessionID, redactErr),
					p.fs.RemoveAll(tmpDir),
				)
				return workerResult{result: result}
			}
			writeData = redacted
		case SourceFormatJSON:
			writeData = redact.RedactJSONDocBytes(p.redactor, rawData)
		}
	}
	// For JSONL/JSON providers, keep the redacted bytes for the indexer.
	switch session.SourceFormat {
	case SourceFormatJSONL, SourceFormatJSON:
		transcriptData = writeData
	}

	if err := p.fs.WriteFile(tmpTranscriptPath, writeData, defaults.PrivateFilePerm); err != nil {
		result.Error = errors.Join(
			fmt.Errorf("write transcript for %s: %w", session.SessionID, err),
			p.fs.RemoveAll(tmpDir),
		)
		return workerResult{result: result}
	}

	// Copy debug files if present.
	if len(session.DebugPaths) > 0 {
		debugDir := fmt.Sprintf("%s/%s", tmpDir, defaults.DirDebug.String())
		if err := p.fs.MkdirAll(debugDir, defaults.PrivateDirPerm); err != nil {
			result.Error = errors.Join(
				fmt.Errorf("create debug dir for %s: %w", session.SessionID, err),
				p.fs.RemoveAll(tmpDir),
			)
			return workerResult{result: result}
		}
		for _, dp := range session.DebugPaths {
			dstPath := fmt.Sprintf("%s/%s", debugDir, filepath.Base(string(dp)))
			if err := p.fs.CopyFile(string(dp), dstPath, defaults.PrivateFilePerm); err != nil {
				meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
					ErrorType:   "copy_failed",
					Location:    string(dp),
					Message:     fmt.Sprintf("failed to copy debug file: %v", err),
					Remediation: "Check that the source debug file exists and is readable.",
				})
				partial := true
				meta.Diagnostics.Partial = &partial
			}
		}
	}

	// Commit detection: populate GitContext.Commits before metadata serialization.
	// Non-fatal: errors are recorded as diagnostic warnings so the audit trail
	// reflects git failures without blocking session ingestion.
	if p.gitAnalyzer != nil {
		// Resolve repo path: prefer Worktree (linked-worktree repos), fall back to
		// Project.FilePath (standard repos, which is the common case). Both point
		// to a valid git working directory for the session.
		repoPath := ""
		if meta.Git.Worktree != nil && *meta.Git.Worktree != "" {
			repoPath = *meta.Git.Worktree
		} else if meta.Project.FilePath != "" {
			repoPath = meta.Project.FilePath
		}

		if repoPath != "" && meta.Timestamp.End != 0 {
			// If user email is not configured (empty string), all commits will be
			// filtered out since no commit will match an empty author email.
			// A diagnostic warning is emitted by CommitDetector.LayeredDetection
			// if the git operation itself fails.
			// UserEmail with a short timeout: git config reads ~/.gitconfig and
			// should complete in milliseconds. A 2-second cap guards against
			// hangs caused by locked config files or slow/network filesystems.
			emailCtx, emailCancel := context.WithTimeout(ctx, 2*time.Second)
			userEmail, _ := p.git.UserEmail(emailCtx)
			emailCancel()
			detector := newCommitDetectorWithReader(p.gitAnalyzer, userEmail, p.commitTranscriptReader)
			sessionStart := time.UnixMilli(meta.Timestamp.Start)
			sessionEnd := time.UnixMilli(meta.Timestamp.End)
			// File origins use the provider transcript. Current SQLite uses only
			// the deterministic managed projection already written in the private
			// temporary directory. The shipped legacy SQLite path remains
			// timestamp-only. DB/WAL/SHM paths never reach parsing.
			transcriptPath := ""
			if session.TranscriptOrigin == TranscriptOriginFile {
				transcriptPath = session.SourcePath.String()
			} else if session.TranscriptOrigin == TranscriptOriginOpenCodeCurrentSQLite {
				transcriptPath = tmpTranscriptPath
			}
			commits, diags := detector.LayeredDetection(ctx, repoPath, sessionStart, sessionEnd, transcriptPath)
			meta.Git.Commits = commits
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, diags...)
		}
		// Sessions with End == 0 are active (still running). Commit detection is
		// skipped silently — this is expected, not an error condition.
	}

	// REDACT metadata before writing to disk.
	if p.redactor != nil {
		meta = p.redactor.RedactMetadata(meta)
		// The host slug is a LOCATOR, not content, so it survives redaction here.
		//
		// sessionDir above was built from the pre-redaction slug, and the directory
		// has already been created under it. Redaction rewrites meta.HostSlug -
		// entropy detection replaces the whole slug of a repository with no origin
		// remote, because its 8-hex segment is HMAC-derived and looks like a secret
		// - so the database row and the metadata file would record a slug that no
		// directory on disk uses. Push resolves the metadata path FROM that stored
		// slug, so it would look for a directory that was never written and fail
		// with "metadata file missing or unreadable" on every attempt, forever.
		// Restoring it keeps the three records of one location identical.
		//
		// This does NOT weaken the wire: push re-redacts metadata immediately
		// before upload, so the slug is redacted on the way out while the local
		// copy stays resolvable. That split is the deferred-redaction design, and
		// it is the reason this line is not the bug it can look like - do not
		// "fix" it by letting the redacted value stand.
		meta.HostSlug = hostSlug
	}

	// Compute ContentHash from the final transcript bytes (post-redaction if redacted).
	meta.ContentHash = schema.ComputeTranscriptHash(writeData)

	// Set RedactionInfo based on whether the redactor ran on transcript bytes.
	if p.redactor != nil {
		now := time.Now().UnixMilli()
		meta.Redaction = RedactionInfo{
			Applied:             true,
			Level:               p.redactor.Level(),
			RuleSetVersion:      p.redactor.RuleSetVersion(),
			RedactedAtMs:        &now,
			ContentHashAtRedact: meta.ContentHash,
		}
	} else {
		meta.Redaction = RedactionInfo{Applied: false}
	}

	// Compute MetadataHash after all content-bearing fields are set.
	// NOTE: DerivedAt is NOT set here. It will be set in drainLoop after DB INSERT
	// (v8 write order: DB first, metadata.json second). For the no-store path,
	// metadata.json is written by the pipeline finalization step without DerivedAt.
	meta.MetadataHash = schema.ComputeMetadataHash(meta)

	// tmpDir holds transcript (and optional debug files) only — NOT metadata.json.
	// metadata.json is written after the atomic rename in drainLoop (store path)
	// or the no-store finalization path, ensuring DB INSERT always precedes file write.

	// Ensure parent directory of final session dir exists.
	parentDir := filepath.Dir(sessionDir)
	if err := p.fs.MkdirAll(parentDir, defaults.PrivateDirPerm); err != nil {
		result.Error = errors.Join(
			fmt.Errorf("create output dir for %s: %w", session.SessionID, err),
			p.fs.RemoveAll(tmpDir),
		)
		return workerResult{result: result}
	}

	// KNOWN LIMITATION (M11): RemoveAll + renameDir is not atomic. There is a brief
	// window where the session directory does not exist. For MVP this is acceptable
	// since ingestion runs as a single sequential process. If concurrent readers are
	// added, this should use a two-phase approach (rename old to .old, rename new to
	// final, then remove .old).
	if _, err := p.fs.Stat(sessionDir); err == nil {
		if err := p.fs.RemoveAll(sessionDir); err != nil {
			result.Error = errors.Join(
				fmt.Errorf("remove old session dir for %s: %w", session.SessionID, err),
				p.fs.RemoveAll(tmpDir),
			)
			return workerResult{result: result}
		}
	}

	// Atomic rename: move temp dir to final location.
	// FileSystem.Rename may not move directory contents recursively (MemFS),
	// so we use a recursive move implementation.
	if err := p.renameDir(tmpDir, sessionDir); err != nil {
		result.Error = errors.Join(
			fmt.Errorf("rename temp dir for %s: %w", session.SessionID, err),
			p.fs.RemoveAll(tmpDir),
		)
		return workerResult{result: result}
	}

	result.OutputPath = sessionDir

	var startMs int64
	if meta.Timestamp.Start > 0 {
		startMs = meta.Timestamp.Start
	}
	outputTranscriptPath := fmt.Sprintf("%s/%s--transcript.%s", sessionDir, session.SessionID, ext)

	return workerResult{
		result:               result,
		meta:                 meta,
		transcriptData:       transcriptData,
		outputTranscriptPath: outputTranscriptPath,
		// Carried from the DISCOVERED session, which is the only place it exists.
		// The index step's session is rebuilt from this result and its source path
		// is replaced with the written copy's, so a directory-based harness has no
		// way to recover its provider root once this is dropped.
		originalRoot:     session.OriginalRoot,
		transcriptOrigin: session.TranscriptOrigin,
		startMs:          startMs,
		metaFilename:     metaFilename,
		sessionDir:       sessionDir,
	}
}

// indexTargetSession is the session handed to an indexer at the INDEX stage.
//
// Its source path is the written copy rather than the provider original, so a
// file-source indexer reads what Peasant stored. Its OriginalRoot is left ALONE:
// a directory-source indexer resolves its tree from that root, and overwriting or
// dropping it leaves the indexer deriving a root from the output tree, where it
// finds nothing and reports no error.
func indexTargetSession(im indexedMeta) DiscoveredSession {
	indexed := im.session
	indexed.SourcePath = ResolvedPath(im.outputTranscriptPath)
	return indexed
}

// indexWithSourceKind runs an indexer over the source its own contract declares.
//
// The choice used to be made by asking whether in-memory bytes happened to be
// available. That is a question about the caller, not the indexer, and it silently
// mismatched the one harness whose entries are not in its transcript file: bytes
// were always available for it, so the bytes path was always taken, and its
// indexer discarded them and read a provider tree instead. Dispatching on the
// declared kind means the argument an indexer is handed is the argument it uses.
//
// An indexer that declares NOTHING is refused rather than assumed to be a file
// source, because assuming reproduces that same defect for the next harness that
// forgets. Every arm here either indexes from a declared source or returns an
// error; none of them guesses.
func indexWithSourceKind(
	ctx context.Context,
	indexer TranscriptIndexer,
	session DiscoveredSession,
	transcriptData []byte,
) ([]schema.SessionEntry, error) {
	sourceKind := indexer.SourceKind()
	if resolver, ok := indexer.(SessionTranscriptSourceResolver); ok {
		sourceKind = resolver.TranscriptSourceKindFor(session)
	}
	switch sourceKind {
	case TranscriptSourceDirectory:
		// No single file holds this harness's entries, so there is nothing to pass
		// in memory and nothing to save by trying.
		if session.OriginalRoot == "" {
			return nil, fmt.Errorf(
				"ingest: cannot index session %q: its harness stores entries under a provider root, and this run has no root for it.\n"+
					"What went wrong: %s keeps its messages in a directory tree rather than in the transcript file, so indexing needs that directory. This run never had it - either the harness is not enabled in your configuration, so discovery never resolved its root, or the session was reached by a path that does not carry one.\n"+
					"Where: ingest.indexWithSourceKind, for harness %s.\n"+
					"When: at the INDEX stage, after the transcript copy was already written.\n"+
					"Means: this session imported but has no indexed entries, so it is empty in the viewer, in search, in metrics, and in anything published. Other sessions in this run are unaffected.\n"+
					"Fix: enable %s in your configuration (sources.%s.enabled: true) and re-run, so discovery resolves its storage root. If you are pointing at sessions with --source-provider and --source-path, name %s and give the path to its storage directory rather than to a single file.",
				session.SessionID, session.Harness, session.Harness, session.Harness, session.Harness, session.Harness)
		}
		return indexer.IndexTranscript(ctx, session)
	case TranscriptSourceFile:
		if len(transcriptData) > 0 {
			return indexer.IndexTranscriptBytes(ctx, session, transcriptData)
		}
		return indexer.IndexTranscript(ctx, session)
	case TranscriptSourceKindUnknown:
		return nil, fmt.Errorf(
			"ingest: cannot index session %q: its indexer did not declare where its entries come from.\n"+
				"What went wrong: the indexer returned the zero TranscriptSourceKind, which is an absent declaration rather than a choice.\n"+
				"Where: ingest.indexWithSourceKind, for harness %s.\n"+
				"When: at the INDEX stage, after the transcript copy was already written.\n"+
				"Means: no entries were indexed for this session, and the run says so instead of guessing. Guessing is what "+
				"this dispatch exists to stop: assuming a file source for a harness whose entries live in a provider tree "+
				"hands the indexer bytes it must discard, and the session is then stored empty - invisible in the viewer, "+
				"in search, in metrics, and in anything published - while the import reports success.\n"+
				"Fix: return TranscriptSourceFile or TranscriptSourceDirectory from this indexer's SourceKind method.",
			session.SessionID, session.Harness)
	}
	// The unhandled-kind arm. It is NOT covered by the dispatch corpus and cannot
	// be: the corpus's spelling table is exhaustive over AllTranscriptSourceKinds
	// by construction and its loader rejects any other value, so no row can reach
	// here while that guard holds. Said plainly so this does not read as an arm
	// somebody forgot to test - it is reachable only by adding a kind to the enum
	// without adding an arm, which is the case it exists for.
	return nil, fmt.Errorf(
		"ingest: cannot index session %q: its indexer reported the unhandled source kind %q.\n"+
			"What went wrong: a transcript source kind was added without teaching the dispatch what to do with it.\n"+
			"Where: ingest.indexWithSourceKind, for harness %s.\n"+
			"When: at the INDEX stage.\n"+
			"Means: no entries were indexed for this session; the run fails loudly rather than storing an empty transcript.\n"+
			"Fix: add a case for the new kind in indexWithSourceKind.",
		session.SessionID, sourceKind, session.Harness)
}

// sessionFromWorkerResult reconstructs the DiscoveredSession carried by a workerResult.
// The session fields needed downstream (SessionID, Harness, ParentUUID, SourceFormat,
// SourcePath) are preserved on result and meta; we recover them here.
func sessionFromWorkerResult(wr workerResult) DiscoveredSession {
	var parentUUID *SessionID
	if wr.result.ParentUUID != nil {
		pid := *wr.result.ParentUUID
		parentUUID = &pid
	}
	var sourceFormat SourceFormat
	var sourcePath ResolvedPath
	if wr.meta != nil {
		sourceFormat = SourceFormat(wr.meta.Source.Format)
		if wr.meta.Source.FilePath != "" {
			sourcePath = ResolvedPath(wr.meta.Source.FilePath)
		}
	}
	return DiscoveredSession{
		SessionID:    wr.result.SessionID,
		Harness:      wr.result.Harness,
		ParentUUID:   parentUUID,
		SourceFormat: sourceFormat,
		SourcePath:   sourcePath,
		// Without this a directory-based harness indexes nothing on the drain-loop
		// pass: the caller replaces SourcePath with the written copy's path, and a
		// root derived from that points into the output tree. The stale-index sweep
		// then recovers the session later in the same run, so the cost is a wasted
		// pass and a misleading skipped row rather than an empty session - measured,
		// after an earlier comment here claimed otherwise.
		OriginalRoot:     wr.originalRoot,
		TranscriptOrigin: wr.transcriptOrigin,
	}
}

// renameDir moves a directory tree from src to dst using the FileSystem interface.
// This is necessary because MemFS.Rename only handles the directory node itself,
// not its contents. For production (OSFileSystem), os.Rename handles everything.
// We implement a portable recursive move: create dst dir, move files, remove src dir.
func (p *Pipeline) renameDir(src, dst string) error {
	// First, ensure dst parent exists.
	if err := p.fs.MkdirAll(dst, defaults.PrivateDirPerm); err != nil {
		return fmt.Errorf("renameDir: mkdir %s: %w", dst, err)
	}

	// Walk src and copy all files to dst, then remove src.
	walkErr := p.fs.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == src {
			return nil // skip root itself
		}

		// Compute relative path within src.
		rel := strings.TrimPrefix(path, src+"/")
		dstPath := fmt.Sprintf("%s/%s", dst, rel)

		if d.IsDir() {
			return p.fs.MkdirAll(dstPath, defaults.PrivateDirPerm)
		}

		// Move file: copy then remove original.
		if err := p.fs.CopyFile(path, dstPath, defaults.PrivateFilePerm); err != nil {
			return fmt.Errorf("renameDir copy %s -> %s: %w", path, dstPath, err)
		}
		return nil
	})

	if walkErr != nil {
		return errors.Join(walkErr, p.fs.RemoveAll(dst))
	}

	// Remove src directory tree.
	return p.fs.RemoveAll(src)
}

// cleanOrphans removes .tmp-* directories from the output dir root.
// COUPLING (M14): This scan location must match the tmpDir placement in processSession().
// Currently both use outputDir as the parent. Changing one without the other will
// leave orphans or delete valid directories.
func (p *Pipeline) cleanOrphans() {
	outputDir := string(p.config.OutputDir)
	entries, err := p.fs.ReadDir(outputDir)
	if err != nil {
		return // output dir may not exist yet
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), defaults.TempDirPrefix) {
			orphanPath := fmt.Sprintf("%s/%s", outputDir, entry.Name())
			_ = p.fs.RemoveAll(orphanPath) // best-effort: cleanOrphans is advisory; Temporal retries will supersede orphans
		}
	}
}

// indexComputeAndFinalize runs the shared INDEX, COMPUTE, CLEANUP, REPORT, and AUDIT
// stages for both normal ingest and reindex pipelines.
//
// Parameters:
//   - indexSessions: sessions to index (from EXTRACT+WRITE or reindex scan)
//   - sessionResults: per-session outcomes from upstream processing
//   - storeErr: non-nil if DB insert failed in an earlier stage
//   - start: pipeline start time (for Duration and AUDIT)
//   - priorIndexLogEntries: pre-existing index log entries from caller (e.g. fallback entries from reindex); may be nil
//   - outcome: IndexOutcomeIndexed (normal ingest) or IndexOutcomeReindexed (reindex)
//   - logPrefix: "pipeline" or "reindex" for structured log messages
func (p *Pipeline) indexComputeAndFinalize(
	ctx context.Context,
	indexSessions []indexedMeta,
	priorIndexed []indexedMeta,
	sessionResults []SessionResult,
	storeErr error,
	start time.Time,
	priorIndexLogEntries []IndexLogEntry,
	outcome IndexOutcome,
	logPrefix string,
) (*PipelineResult, error) {
	prog := p.config.Progress

	// INDEX session_entries (best-effort, non-fatal).
	// successfullyIndexed tracks only sessions that had entries returned AND were
	// stored successfully. These are the only sessions passed to ComputeMetrics.
	indexed := 0
	successfullyIndexed := make([]SessionID, 0, len(priorIndexed)+len(indexSessions))
	for _, im := range priorIndexed {
		if im.indexed {
			indexed++
			successfullyIndexed = append(successfullyIndexed, im.session.SessionID)
		}
	}
	// priorDone is the count of sessions already processed by indexLoop() goroutine.
	// KindStart for StageIndex was emitted before the goroutines launched; we must NOT
	// re-emit KindStart here (it resets Done to 0, wiping goroutine progress).
	// Instead, if there are additional stale sessions, update the total via KindAdvance.
	priorDone := len(priorIndexed)
	indexLogEntries := append([]IndexLogEntry(nil), priorIndexLogEntries...)
	if len(indexSessions) > 0 {
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageIndex, Done: priorDone, Total: priorDone + len(indexSessions)})
	}
	if p.indexers != nil && p.metricsStore != nil {
		batchIndexed, batchLogs := p.indexBatch(ctx, indexSessions, outcome, logPrefix)
		for i, result := range batchIndexed {
			if result.indexed {
				indexed++
				successfullyIndexed = append(successfullyIndexed, result.session.SessionID)
			}
			if batchLogs[i].SessionID != "" {
				indexLogEntries = append(indexLogEntries, batchLogs[i])
			}
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageIndex, Done: priorDone + i + 1, Total: priorDone + len(indexSessions)})
		}
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageIndex, Done: indexed, Total: len(priorIndexed) + len(indexSessions)})

	// Persist index_log entries (best-effort).
	if p.indexLogger != nil {
		for _, entry := range indexLogEntries {
			if err := p.indexLogger.LogIndexEntry(ctx, entry); err != nil {
				slog.Warn(logPrefix+": log index entry", "session_id", entry.SessionID, "error", err)
			}
		}
	}

	// COMPUTE metrics + insights (best-effort, non-fatal).
	// Gate on len(indexSessions) > 0 (sessions that were written this run),
	// not on indexed > 0 (which would miss sessions whose entries already exist
	// from a prior run). The engine handles idempotency via MetricsExist.
	computed := 0
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageCompute, Total: len(successfullyIndexed)})
	if p.analyzer != nil && (len(indexSessions) > 0 || len(priorIndexed) > 0) {
		// Compute metrics only for sessions that were successfully indexed this run.
		n, err := p.analyzer.ComputeMetrics(ctx, successfullyIndexed)
		if err != nil {
			slog.Warn(logPrefix+": compute metrics", "error", err)
		}
		computed = n

		// Compute insights (daily summaries) for affected days.
		// Derive days from both drain-loop indexed and stale-session indexed metas
		// so this works even when p.store is nil (e.g. WithIndexers+WithAnalyzer only).
		daySet := make(map[string]bool)
		for _, im := range priorIndexed {
			if im.startMs > 0 {
				day := time.Unix(im.startMs/1000, 0).UTC().Format("2006-01-02")
				daySet[day] = true
			}
		}
		for _, im := range indexSessions {
			if im.startMs > 0 {
				day := time.Unix(im.startMs/1000, 0).UTC().Format("2006-01-02")
				daySet[day] = true
			}
		}
		if len(daySet) > 0 {
			days := make([]string, 0, len(daySet))
			for d := range daySet {
				days = append(days, d)
			}
			if err := p.analyzer.ComputeInsights(ctx, days); err != nil {
				slog.Warn(logPrefix+": compute insights", "error", err)
			}
		}
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageCompute, Done: computed, Total: len(successfullyIndexed)})

	// ANNOTATE sessions (best-effort, non-fatal).
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageAnnotate, Total: len(successfullyIndexed)})
	if p.classifier != nil && len(successfullyIndexed) > 0 {
		if err := p.stageAnnotate(ctx, successfullyIndexed, prog); err != nil {
			slog.Warn(logPrefix+": annotate sessions", "error", err)
		}
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageAnnotate, Done: len(successfullyIndexed), Total: len(successfullyIndexed)})

	// CLEANUP orphan .tmp-* directories.
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageCleanup})
	p.cleanOrphans()
	// Clean up orphan project rows (projects with no remaining sessions).
	// Best-effort: non-fatal; projects from deleted/pruned sessions are removed
	// so that daily_summary_by_project and the session viewer remain consistent.
	if p.store != nil {
		if err := p.store.CleanupOrphanProjects(ctx); err != nil {
			slog.Warn(logPrefix+": cleanup orphan projects",
				"error", err,
				"what", "failed to remove project rows with zero sessions",
				"why", "DB error during DELETE from projects or daily_summary_by_project",
				"user_impact", "stale project names may appear in the session viewer filter list",
				"how_to_fix", "re-run peasant ingest; if persistent, delete the DB at ~/.local/share/peasant/peasant.db and re-ingest")
		}
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageCleanup})

	// REPORT.
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageReport})
	pipelineResult := &PipelineResult{
		Sessions:             sessionResults,
		Duration:             time.Since(start),
		IndexLog:             indexLogEntries,
		DiscoveryDiagnostics: p.discoveryDiagnostics,
	}
	pipelineResult.Summary.StoreError = storeErr
	pipelineResult.Summary.Indexed = indexed
	pipelineResult.Summary.Computed = computed
	pipelineResult.Summary.IndexVersion = CurrentIndexVersion
	pipelineResult.Summary.MetadataVersion = int(CurrentSchemaVersion)
	pipelineResult.Summary.ReminedEvidenceRecords = p.reminedEvidence
	pipelineResult.Summary.OriginResolve = p.originResolve
	pipelineResult.Summary.OriginResolveError = p.originResolveErr
	for _, sr := range sessionResults {
		if sr.Error != nil {
			pipelineResult.Summary.Errors++
			continue
		}
		switch sr.Status {
		case DiffNew:
			pipelineResult.Summary.New++
		case DiffUpdated:
			pipelineResult.Summary.Updated++
		case DiffUnchanged:
			pipelineResult.Summary.Unchanged++
		case DiffActive:
			pipelineResult.Summary.Active++
		}
	}

	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageReport, Done: len(pipelineResult.Sessions), Total: len(pipelineResult.Sessions)})

	// AUDIT — write ingest_log entry (best-effort, non-fatal).
	if p.logger != nil {
		finishedAt := time.Now().UnixMilli()
		logEntry := IngestLogEntry{
			StartedAt:         start.UnixMilli(),
			FinishedAt:        &finishedAt,
			SessionsNew:       pipelineResult.Summary.New,
			SessionsUpdated:   pipelineResult.Summary.Updated,
			SessionsUnchanged: pipelineResult.Summary.Unchanged,
			SessionsError:     pipelineResult.Summary.Errors,
			IndexedCount:      pipelineResult.Summary.Indexed,
			ComputedCount:     pipelineResult.Summary.Computed,
			SourcePath:        collectSourcePaths(p.config.Sources),
		}
		if err := p.logger.LogIngestRun(ctx, logEntry); err != nil {
			slog.Warn(logPrefix+": log ingest run", "error", err)
		}
	}

	return pipelineResult, nil
}

// makeIndexLogEntry creates an IndexLogEntry from an indexedMeta with the given outcome.
func (p *Pipeline) makeIndexLogEntry(im indexedMeta, outcome IndexOutcome, entriesCount int, startMs int64, reason *string, errMsg *string) IndexLogEntry {
	finishedAt := time.Now().UnixMilli()
	var sourcePath *string
	if sp := string(im.session.SourcePath); sp != "" {
		sourcePath = &sp
	}
	var originalRoot *string
	if or := string(im.session.OriginalRoot); or != "" {
		originalRoot = &or
	}
	return IndexLogEntry{
		SessionID:    im.session.SessionID,
		Harness:      im.session.Harness,
		Outcome:      outcome,
		IndexVersion: CurrentIndexVersion,
		EntriesCount: entriesCount,
		SourcePath:   sourcePath,
		OriginalRoot: originalRoot,
		Reason:       reason,
		StartedAt:    startMs,
		FinishedAt:   &finishedAt,
		ErrorMessage: errMsg,
	}
}

// sessionMetadataResult holds the parsed output of readSessionMetadata.
type sessionMetadataResult struct {
	session            DiscoveredSession
	startMs            int64
	transcriptPath     string
	originalSourcePath string // Source.FilePath from metadata (may be empty)
}

// readSessionMetadata reads and parses a metadata JSON file for a session in
// the given host directory, reconstructing a DiscoveredSession. Returns nil
// if the metadata is not found, cannot be parsed, or the transcript file does
// not exist on disk.
//
// The logPrefix parameter is used for structured log messages on parse errors.
func (p *Pipeline) readSessionMetadata(hostDir string, sid SessionID, logPrefix string) *sessionMetadataResult {
	metaFilename := fmt.Sprintf("%s%s", sid, defaults.MetadataSuffix)
	metaPath := fmt.Sprintf("%s/%s/%s", hostDir, sid, metaFilename)
	data, err := p.fs.ReadFile(metaPath)
	if err != nil {
		return nil
	}

	var meta UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		slog.Warn(logPrefix+": metadata parse error", "session_id", sid, "error", err)
		return nil
	}

	// Determine SourceFormat from metadata.
	sourceFormat := meta.Source.Format
	if sourceFormat == "" {
		return nil
	}

	// Build transcript path from output dir.
	transcriptPath := fmt.Sprintf("%s/%s/%s--transcript.%s",
		hostDir, sid, sid, string(sourceFormat))

	// Verify transcript exists.
	if _, err := p.fs.Stat(transcriptPath); err != nil {
		return nil
	}

	// Look up OriginalRoot from config source paths for this provider.
	var originalRoot ResolvedPath
	if cfg, ok := p.config.Sources[Harness(meta.ModelHarness)]; ok && len(cfg.Paths) > 0 {
		originalRoot = cfg.Paths[0]
	}

	ds := DiscoveredSession{
		SessionID:    sid,
		Harness:      Harness(meta.ModelHarness),
		SourcePath:   ResolvedPath(transcriptPath),
		SourceFormat: sourceFormat,
		OriginalRoot: originalRoot,
	}
	if ds.Harness == HarnessOpenCode && sourceFormat == SourceFormatJSON {
		if transcriptData, readErr := p.fs.ReadFile(transcriptPath); readErr == nil {
			origin, recognitionErr := recognizeManagedOpenCodeProjection(transcriptData, sid)
			if recognitionErr != nil {
				slog.Warn(logPrefix+": managed OpenCode projection is corrupt", "session_id", sid, "transcript_path", transcriptPath, "error", recognitionErr, "impact", "recovery stopped before legacy fallback so existing index state is not replaced with an empty corpus", "fix", "re-run harvest to regenerate the managed transcript")
				return nil
			}
			ds.TranscriptOrigin = origin
		}
	}
	if meta.ParentUUID != nil {
		ds.ParentUUID = meta.ParentUUID
	}

	return &sessionMetadataResult{
		session:            ds,
		startMs:            meta.Timestamp.Start,
		transcriptPath:     transcriptPath,
		originalSourcePath: meta.Source.FilePath,
	}
}

// reconstructFromMetadata attempts to reconstruct a DiscoveredSession from
// peasant-sync metadata for a session that needs re-indexing (stale index_version).
// Returns the reconstructed session, the start timestamp, and the transcript path.
// Returns (nil, 0, "") if reconstruction fails.
//
// Optimization: queries the DB for host_slug and parent_id before scanning the
// filesystem. If the DB has a location record, jumps directly to the session
// directory. Falls back to full directory scan only when the session is not in
// the DB (e.g. pre-DB ingestion data).
func (p *Pipeline) reconstructFromMetadata(ctx context.Context, sid SessionID) (*DiscoveredSession, int64, string) {
	outputDir := string(p.config.OutputDir)

	// Fast path: query DB for the session's host_slug and parent_id.
	if p.metricsStore != nil {
		hostSlug, parentID, lookupErr := p.metricsStore.LookupSessionLocation(ctx, sid)
		if lookupErr != nil {
			slog.Warn("pipeline: lookup session location", "session_id", sid, "error", lookupErr)
			// Fall through to full scan below.
		} else if hostSlug != "" {
			hostDir := fmt.Sprintf("%s/%s", outputDir, hostSlug)
			if parentID == "" {
				// Flat layout: {hostSlug}/{sid}/
				result := p.readSessionMetadata(hostDir, sid, "pipeline")
				if result != nil {
					return &result.session, result.startMs, result.transcriptPath
				}
			} else {
				// Nested subagent layout: {hostSlug}/{parentID}/subagents/{sid}/
				subDir := fmt.Sprintf("%s/%s/%s", hostDir, parentID, defaults.DirSubagents.String())
				result := p.readSessionMetadata(subDir, sid, "pipeline")
				if result != nil {
					return &result.session, result.startMs, result.transcriptPath
				}
			}
			// DB had a record but the file was missing — fall through to full scan.
		}
	}

	// Slow path: scan all host directories (used when session is not in the DB).
	entries, err := p.fs.ReadDir(outputDir)
	if err != nil {
		return nil, 0, ""
	}

	for _, hostEntry := range entries {
		if !hostEntry.IsDir() || strings.HasPrefix(hostEntry.Name(), defaults.TempDirPrefix) {
			continue
		}
		hostDir := fmt.Sprintf("%s/%s", outputDir, hostEntry.Name())

		// Check flat layout first: {hostSlug}/{sid}/{metaFilename}
		result := p.readSessionMetadata(hostDir, sid, "pipeline")
		if result != nil {
			return &result.session, result.startMs, result.transcriptPath
		}

		// Check nested subagent layout: {hostSlug}/{parentID}/subagents/{sid}/{metaFilename}
		sessionEntries, readErr := p.fs.ReadDir(hostDir)
		if readErr != nil {
			continue
		}
		for _, sessionEntry := range sessionEntries {
			if !sessionEntry.IsDir() {
				continue
			}
			subHostDir := fmt.Sprintf("%s/%s/%s", hostDir, sessionEntry.Name(), defaults.DirSubagents.String())
			result := p.readSessionMetadata(subHostDir, sid, "pipeline")
			if result != nil {
				return &result.session, result.startMs, result.transcriptPath
			}
		}
	}

	return nil, 0, ""
}

// reconstructFromSourceInfo builds a DiscoveredSession from the DB's source_path
// and source_format columns. This is a fallback for sessions (e.g. subagents) that
// were indexed from the original source but don't have peasant-sync metadata.
//
// The third return value is the peasant-sync output transcript path (not the raw
// source_path). It requires knowing the host_slug, which is looked up from the DB
// via LookupSessionLocation. Returns (nil, 0, "") if any required field is missing.
func (p *Pipeline) reconstructFromSourceInfo(ctx context.Context, sid SessionID) (*DiscoveredSession, int64, string) {
	if p.metricsStore == nil {
		return nil, 0, ""
	}
	sourcePath, sourceFormat, providerStr, err := p.metricsStore.LookupSourceInfo(ctx, sid)
	if err != nil || sourcePath == "" {
		return nil, 0, ""
	}

	provider := Harness(providerStr)
	if !provider.IsKnown() {
		slog.Warn("reconstructFromSourceInfo: invalid provider", "session_id", sid, "provider", providerStr)
		return nil, 0, ""
	}

	resolvedSrc, err := NewResolvedPath(sourcePath)
	if err != nil {
		slog.Warn("reconstructFromSourceInfo: invalid source path", "session_id", sid, "error", err)
		return nil, 0, ""
	}

	// Look up host_slug and parent_id to construct the peasant-sync output transcript path.
	hostSlug, parentID, lookupErr := p.metricsStore.LookupSessionLocation(ctx, sid)
	if lookupErr != nil || hostSlug == "" {
		slog.Warn("reconstructFromSourceInfo: cannot determine host_slug", "session_id", sid, "error", lookupErr)
		return nil, 0, ""
	}

	outputDir := string(p.config.OutputDir)
	var outputTranscriptPath string
	if parentID != "" {
		// Subagent layout: {outputDir}/{hostSlug}/{parentID}/subagents/{sid}/{sid}--transcript.{ext}
		outputTranscriptPath = fmt.Sprintf("%s/%s/%s/%s/%s/%s--transcript.%s",
			outputDir, hostSlug, parentID, defaults.DirSubagents.String(), sid, sid, string(sourceFormat))
	} else {
		// Flat layout: {outputDir}/{hostSlug}/{sid}/{sid}--transcript.{ext}
		outputTranscriptPath = fmt.Sprintf("%s/%s/%s/%s--transcript.%s",
			outputDir, hostSlug, sid, sid, string(sourceFormat))
	}
	transcriptOrigin := TranscriptOriginFile
	if provider == HarnessOpenCode && sourceFormat == SourceFormatJSON {
		if transcriptData, readErr := p.fs.ReadFile(outputTranscriptPath); readErr == nil {
			origin, recognitionErr := recognizeManagedOpenCodeProjection(transcriptData, sid)
			if recognitionErr != nil {
				slog.Warn("reconstructFromSourceInfo: managed OpenCode projection is corrupt", "session_id", sid, "transcript_path", outputTranscriptPath, "error", recognitionErr, "impact", "recovery stopped before legacy fallback so existing index state is not replaced with an empty corpus", "fix", "re-run harvest to regenerate the managed transcript")
				return nil, 0, ""
			}
			transcriptOrigin = origin
		}
	}

	return &DiscoveredSession{
		SessionID:        sid,
		SourcePath:       resolvedSrc,
		SourceFormat:     sourceFormat,
		Harness:          provider,
		TranscriptOrigin: transcriptOrigin,
	}, 0, outputTranscriptPath
}

// runReindex implements the --reindex pipeline mode.
// Instead of discovering from source providers, it scans the peasant-sync output
// directory to find existing sessions, then re-processes those with stale or
// missing index data.
//
// Steps:
//  1. Scan peasant-sync output to enumerate all existing sessions (by reading metadata JSONs)
//  2. Filter to targeted sessions:
//     - Default: sessions with index_version < CurrentIndexVersion (via DB query)
//     - With --force: ALL sessions
//  3. For each targeted session:
//     a. Try EXTRACT+WRITE from original source (if source file exists)
//     b. If original source missing: log structured warning, record fallback outcome,
//     fall back to INDEX+COMPUTE from existing peasant-sync transcript
//     c. Run INDEX+COMPUTE
//  4. Write index_log entries, populate PipelineResult.IndexLog
func (p *Pipeline) runReindex(ctx context.Context, start time.Time) (*PipelineResult, error) {
	prog := p.config.Progress

	// Stage 1: DISCOVER — scan peasant-sync output.
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDiscover})
	scanned := p.scanPeasantSyncSessions()
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiscover, Done: len(scanned), Total: len(scanned)})

	// Stage 2: DIFF — filter to targeted sessions.
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: len(scanned)})
	var targeted []reindexTarget
	if p.config.Force {
		// --force --reindex: target ALL sessions.
		targeted = scanned
	} else {
		// --reindex only: target sessions with stale index_version.
		staleSet := make(map[SessionID]bool)
		if p.metricsStore != nil {
			staleIDs, err := p.metricsStore.ListStaleIndexSessions(ctx, CurrentIndexVersion)
			if err != nil {
				slog.Warn("reindex: list stale index sessions", "error", err)
			}
			for _, sid := range staleIDs {
				staleSet[sid] = true
			}
		}
		for _, t := range scanned {
			if staleSet[t.session.SessionID] {
				targeted = append(targeted, t)
			}
		}
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiff, Done: len(targeted), Total: len(scanned)})

	// Stage 3: FILTER — emit targeted count.
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageFilter, Total: len(scanned)})
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageFilter, Done: len(targeted), Total: len(scanned)})

	if p.config.DryRun {
		result := &PipelineResult{
			Duration: time.Since(start),
		}
		for _, t := range targeted {
			result.Sessions = append(result.Sessions, SessionResult{
				SessionID: t.session.SessionID,
				Harness:   t.session.Harness,
				Status:    DiffUpdated, // reindex treats all targeted sessions as "updated"
			})
			result.Summary.Updated++
		}
		return result, nil
	}

	// Stage 4: EXTRACT+WRITE — separate into extractable (source exists) vs fallback (source missing).
	var fallbackTargets []reindexTarget // sessions where source is missing → INDEX+COMPUTE only

	// Build maps for parent-before-child ordering (same pattern as normal pipeline).
	entryByID := make(map[SessionID]DiffEntry, len(targeted))
	childrenOf := make(map[SessionID][]SessionID, len(targeted))
	inBatch := make(map[SessionID]bool, len(targeted))

	for _, t := range targeted {
		sourceExists := false
		if t.originalSourcePath != "" {
			if _, err := p.fs.Stat(t.originalSourcePath); err == nil {
				sourceExists = true
			}
		}

		if sourceExists {
			sourceSession := t.session
			sourceSession.SourcePath = ResolvedPath(t.originalSourcePath)
			entry := DiffEntry{
				Session: sourceSession,
				Status:  DiffUpdated,
			}
			entryByID[sourceSession.SessionID] = entry
			inBatch[sourceSession.SessionID] = true
		} else {
			fallbackTargets = append(fallbackTargets, t)
		}
	}

	// Identify root vs child entries for the extract batch.
	var rootEntries []DiffEntry
	for _, e := range entryByID {
		if e.Session.ParentUUID != nil {
			pid := *e.Session.ParentUUID
			if inBatch[pid] {
				childrenOf[pid] = append(childrenOf[pid], e.Session.SessionID)
				continue // child: will be processed by its root's goroutine
			}
		}
		rootEntries = append(rootEntries, e)
	}

	// Parallel EXTRACT+WRITE via runParallel + StagingBuffer.
	var sessionResults []SessionResult
	var indexSessions []indexedMeta
	var indexLogEntries []IndexLogEntry
	var drainIndexed []indexedMeta
	var drainIndexLogEntries []IndexLogEntry
	extractTotal := len(entryByID) // all extractable sessions (roots + children)

	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageExtract, Total: extractTotal + len(fallbackTargets)})
	var extractDoneAtomic atomic.Int64
	workers := parallelWorkers(p.config)

	if len(rootEntries) > 0 {
		staging := NewStagingBuffer(extractTotal+1, resolveArenaSizeBytes(DefaultArenaSizeBytes))

		var reindexWorkersDone atomic.Bool
		reindexIndexCh := make(chan DrainBatch, 1)
		reindexIndexDoneCh := make(chan struct{}, 1)
		// errCh buffer: ceil(extractTotal/DefaultMaxDrainBatch) + safety margin,
		// same rationale as Run() — prevents blocking the consumer when every batch fails.
		reindexErrChSize := extractTotal/DefaultMaxDrainBatch + 2
		if reindexErrChSize < 16 {
			reindexErrChSize = 16
		}
		reindexErrCh := make(chan error, reindexErrChSize)

		var reindexWg sync.WaitGroup

		// Stage 4a (reindex): EXTRACT+WRITE workers goroutine.
		reindexWg.Add(1)
		go func() {
			defer reindexWg.Done()
			runParallel(ctx.Err, rootEntries, workers, func(entry DiffEntry) workerResult {
				wr := p.processSession(ctx, entry)
				done := int(extractDoneAtomic.Add(1))
				emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageExtract, Done: done, Total: extractTotal + len(fallbackTargets)})
				staging.Add(wr)
				// BFS over subtree: process children inline (same goroutine → no directory races).
				queue := childrenOf[entry.Session.SessionID]
				for len(queue) > 0 {
					childID := queue[0]
					queue = queue[1:]
					childEntry := entryByID[childID]
					cwr := p.processSession(ctx, childEntry)
					childDone := int(extractDoneAtomic.Add(1))
					emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageExtract, Done: childDone, Total: extractTotal + len(fallbackTargets)})
					staging.Add(cwr)
					queue = append(queue, childrenOf[childID]...)
				}
				// Release heap transcript bytes — arena already has the copy.
				wr.transcriptData = nil
				return wr
			})
			reindexWorkersDone.Store(true)
		}()

		// Stage 4b (reindex): Consumer goroutine — DB INSERT + INDEX coordination.
		// Uses the same drainLoop/indexLoop goroutine pattern as Run() for correctness:
		// Commit-before-Ack ordering, no runtime.Gosched() spin, proper error propagation.
		emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDBInsert, Total: extractTotal})
		reindexWg.Add(1)
		go func() {
			defer reindexWg.Done()
			defer close(reindexIndexCh)
			reindexDrainResults := p.drainLoop(ctx, staging, &reindexWorkersDone, reindexIndexCh, reindexIndexDoneCh, reindexErrCh, prog, extractTotal)
			sessionResults = append(sessionResults, reindexDrainResults...)
			emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDBInsert, Done: len(reindexDrainResults), Total: extractTotal})
		}()

		// Stage INDEX goroutine (reindex).
		emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: extractTotal})
		reindexWg.Add(1)
		go func() {
			defer reindexWg.Done()
			drainIndexed, drainIndexLogEntries = p.indexLoop(ctx, reindexIndexCh, reindexIndexDoneCh, prog, IndexOutcomeReindexed, "reindex")
		}()

		// Wait for all reindex goroutines to complete.
		reindexWg.Wait()
		close(reindexErrCh)

		// Collect first store error (best-effort: pipeline continues on DB failure).
		var storeErr error
		for err := range reindexErrCh {
			if storeErr == nil {
				storeErr = err
			}
		}

		// Process fallback sessions (source missing — no EXTRACT+WRITE, straight to INDEX+COMPUTE).
		for _, t := range fallbackTargets {
			slog.Warn("reindex: original source missing, falling back to INDEX+COMPUTE",
				"session_id", t.session.SessionID,
				"provider", t.session.Harness,
				"expected_path", t.originalSourcePath,
				"stage", "EXTRACT+WRITE",
				"impact", "metadata not refreshed, indexing from existing peasant-sync transcript",
				"fix", "ensure provider data exists at configured source path",
			)

			indexStartMs := time.Now().UnixMilli()
			reason := "original source missing; falling back to existing peasant-sync transcript"
			indexLogEntries = append(indexLogEntries, p.makeIndexLogEntry(indexedMeta{
				session:              t.session,
				startMs:              t.startMs,
				outputTranscriptPath: t.transcriptPath,
			}, IndexOutcomeFallback, 0, indexStartMs, &reason, nil))

			indexSessions = append(indexSessions, indexedMeta{
				session:              t.session,
				startMs:              t.startMs,
				outputTranscriptPath: t.transcriptPath,
			})

			sessionResults = append(sessionResults, SessionResult{
				SessionID: t.session.SessionID,
				Harness:   t.session.Harness,
				Status:    DiffUpdated,
			})
			done := int(extractDoneAtomic.Add(1))
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageExtract, Done: done, Total: extractTotal + len(fallbackTargets)})
		}
		emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageExtract, Done: int(extractDoneAtomic.Load()), Total: extractTotal + len(fallbackTargets)})

		// Stages 5-9: INDEX, COMPUTE, CLEANUP, REPORT, AUDIT (shared with Run).
		return p.indexComputeAndFinalize(ctx, indexSessions, drainIndexed, sessionResults, storeErr, start, append(indexLogEntries, drainIndexLogEntries...), IndexOutcomeReindexed, "reindex")
	}

	// No extractable sessions — all are fallback. Process fallback sessions directly.
	for _, t := range fallbackTargets {
		slog.Warn("reindex: original source missing, falling back to INDEX+COMPUTE",
			"session_id", t.session.SessionID,
			"provider", t.session.Harness,
			"expected_path", t.originalSourcePath,
			"stage", "EXTRACT+WRITE",
			"impact", "metadata not refreshed, indexing from existing peasant-sync transcript",
			"fix", "ensure provider data exists at configured source path",
		)

		indexStartMs := time.Now().UnixMilli()
		reason := "original source missing; falling back to existing peasant-sync transcript"
		indexLogEntries = append(indexLogEntries, p.makeIndexLogEntry(indexedMeta{
			session:              t.session,
			startMs:              t.startMs,
			outputTranscriptPath: t.transcriptPath,
		}, IndexOutcomeFallback, 0, indexStartMs, &reason, nil))

		indexSessions = append(indexSessions, indexedMeta{
			session:              t.session,
			startMs:              t.startMs,
			outputTranscriptPath: t.transcriptPath,
		})

		sessionResults = append(sessionResults, SessionResult{
			SessionID: t.session.SessionID,
			Harness:   t.session.Harness,
			Status:    DiffUpdated,
		})
		done := int(extractDoneAtomic.Add(1))
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageExtract, Done: done, Total: len(fallbackTargets)})
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageExtract, Done: int(extractDoneAtomic.Load()), Total: len(fallbackTargets)})

	// DB INSERT — no extractable sessions means no store entries.
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDBInsert, Total: 0})
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDBInsert, Done: 0, Total: 0})
	var storeErr error

	// Steps 3e-end: INDEX, COMPUTE, CLEANUP, REPORT, AUDIT (shared with Run).
	return p.indexComputeAndFinalize(ctx, indexSessions, drainIndexed, sessionResults, storeErr, start, append(indexLogEntries, drainIndexLogEntries...), IndexOutcomeReindexed, "reindex")
}

// reindexTarget represents a session found in the peasant-sync output directory
// that is a candidate for re-indexing.
type reindexTarget struct {
	session            DiscoveredSession
	startMs            int64
	transcriptPath     string // path to the peasant-sync transcript on disk
	originalSourcePath string // path to the original source file (may not exist)
}

// scanPeasantSyncSessions walks the peasant-sync output directory and enumerates
// all existing sessions by reading their metadata JSON files.
// Returns a list of reindexTargets with reconstructed session info and original source paths.
func (p *Pipeline) scanPeasantSyncSessions() []reindexTarget {
	outputDir := string(p.config.OutputDir)
	entries, err := p.fs.ReadDir(outputDir)
	if err != nil {
		return nil // output dir doesn't exist yet
	}

	var targets []reindexTarget
	for _, hostEntry := range entries {
		if !hostEntry.IsDir() || strings.HasPrefix(hostEntry.Name(), defaults.TempDirPrefix) {
			continue
		}
		hostDir := fmt.Sprintf("%s/%s", outputDir, hostEntry.Name())
		sessionEntries, err := p.fs.ReadDir(hostDir)
		if err != nil {
			continue
		}
		for _, sessionEntry := range sessionEntries {
			if !sessionEntry.IsDir() {
				continue
			}
			// Validate directory name as a SessionID (skip invalid names).
			sid, err := NewSessionID(sessionEntry.Name())
			if err != nil {
				continue
			}

			smr := p.readSessionMetadata(hostDir, sid, "reindex")
			if smr == nil {
				continue
			}

			targets = append(targets, reindexTarget{
				session:            smr.session,
				startMs:            smr.startMs,
				transcriptPath:     smr.transcriptPath,
				originalSourcePath: smr.originalSourcePath,
			})

			// Also scan for nested subagent sessions under {sessionDir}/subagents/.
			subagentsDir := fmt.Sprintf("%s/%s/%s", hostDir, sessionEntry.Name(), defaults.DirSubagents.String())
			subEntries, subErr := p.fs.ReadDir(subagentsDir)
			if subErr != nil {
				continue // no subagents/ dir or unreadable — fine
			}
			for _, subEntry := range subEntries {
				if !subEntry.IsDir() {
					continue
				}
				subSID, subSIDErr := NewSessionID(subEntry.Name())
				if subSIDErr != nil {
					continue
				}
				// readSessionMetadata builds: {hostDir}/{sid}/{metaFilename},
				// so pass {hostDir}/{parentID}/subagents as the "hostDir"
				// to produce the correct nested path.
				subHostDir := subagentsDir
				smr := p.readSessionMetadata(subHostDir, subSID, "reindex")
				if smr == nil {
					continue
				}
				slog.Debug("reindex: discovered subagent session",
					"parent_session_id", sid,
					"subagent_session_id", subSID,
					"host_slug", hostEntry.Name(),
				)
				targets = append(targets, reindexTarget{
					session:            smr.session,
					startMs:            smr.startMs,
					transcriptPath:     smr.transcriptPath,
					originalSourcePath: smr.originalSourcePath,
				})
			}
		}
	}

	return targets
}

// randomHex returns n random bytes encoded as a hex string.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// redactTranscript applies p.redactor.RedactJSON to every JSON value in the transcript
// file at path, rewriting the file in place. p.redactor must be non-nil.
//
// For JSONL (SourceFormatJSONL): each line is independently decoded with UseNumber()
// and re-encoded. Unparseable lines are passed through unchanged so a single bad
// line does not corrupt the file.
//
// For JSON (SourceFormatJSON): the entire document is decoded with UseNumber() and
// re-encoded. If the document cannot be decoded, the file is left unchanged.
//
// UseNumber() prevents float64 precision loss for JSON integers > 2^53 (e.g.
// large Unix timestamps in milliseconds used by some transcript formats).
func (p *Pipeline) redactTranscript(path string, format SourceFormat) error {
	data, err := p.fs.ReadFile(path)
	if err != nil {
		return fmt.Errorf("redactTranscript read %s: %w", path, err)
	}

	var out []byte
	switch format {
	case SourceFormatJSONL:
		var redactErr error
		out, redactErr = redact.RedactJSONLBytes(p.redactor, data, redact.WithRedactScannerBufSize(defaults.ScannerInitBuf, defaults.ScannerMaxLine))
		if redactErr != nil {
			return fmt.Errorf("redactTranscript JSONL %s: %w", path, redactErr)
		}
	case SourceFormatJSON:
		out = redact.RedactJSONDocBytes(p.redactor, data)
	default:
		return nil // unknown format: leave unchanged
	}

	if err := p.fs.WriteFile(path, out, defaults.PrivateFilePerm); err != nil {
		return fmt.Errorf("redactTranscript write %s: %w", path, err)
	}
	return nil
}

// stageAnnotate is the ANNOTATE stage: runs classifiers over session entries and
// persists results via AnnotationStore. Best-effort: per-session errors are logged
// and skipped, never fatal.
func (p *Pipeline) stageAnnotate(ctx context.Context, sessionIDs []SessionID, prog *ProgressState) error {
	if p.classifier == nil {
		return nil
	}
	workers := parallelWorkers(p.config)
	total := len(sessionIDs)

	var wg sync.WaitGroup
	ch := make(chan SessionID, workers)
	var done atomic.Int64

	// Spawn worker pool.
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sid := range ch {
				if err := p.classifier.Annotate(ctx, sid); err != nil {
					slog.Warn("pipeline: annotate session",
						"session_id", sid,
						"error", err,
						"what", "classifier failed to annotate session entries",
						"why", "classifier error or missing session_entries rows",
						"user_impact", "session will lack quality annotations in the web UI",
						"how_to_fix", "re-run peasant ingest index --force --session "+string(sid))
				}
				n := int(done.Add(1))
				emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageAnnotate, Done: n, Total: total})
			}
		}()
	}

	// Feed sessions into the channel.
	for _, sid := range sessionIDs {
		ch <- sid
	}
	close(ch)
	wg.Wait()
	return nil
}
