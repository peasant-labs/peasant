package ingest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
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

const (
	indexWriteBatchLimit        = 64
	annotationFlushSessionLimit = 256
	annotationFlushWriteLimit   = 4096
	annotationFlushInterval     = 500 * time.Millisecond
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
	Session      DiscoveredSession
	Status       DiffStatus
	retainedOnly bool // No current native discovery context accompanies this stored session.
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
	// Diagnostics carries local nonfatal maintenance warnings to post-render
	// reporting. It is not a new HTTP/WS or command JSON contract field.
	Diagnostics []DiagnosticEntry `json:"-"`
}

// PipelineSummary holds aggregate counts for a pipeline run.
type PipelineSummary struct {
	New               int
	Updated           int
	Unchanged         int
	Active            int
	Errors            int
	Indexed           int                           // sessions successfully indexed into session_entries
	Computed          int                           // sessions whose metrics were (re)computed
	StoreError        error                         // non-nil if DB insert failed; pipeline continued normally
	HarvesterVersions map[Harness]HarvesterVersions // current targets, not successful per-session producer stamps
	MetadataVersion   int                           // CurrentSchemaVersion used this run
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
	// Prevent the same invocation's stale-index sweep from bypassing a failed
	// reconciliation. This is execution state, not a serialized success marker.
	mirrorPending bool
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
	// Harness restricts stored-session maintenance independently from discovery
	// source paths. Nil allows all registered harnesses.
	Harness *Harness
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
	captureRevision int64
	capturedSource  *captureFileSystem
	// published reports that this run committed the artifact being indexed,
	// so its full-content capture is attributed to the new ingest rather than
	// to the retained snapshot it would otherwise be read from.
	published            bool
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

	// contentRecoveries holds this run's completed retained-content repairs,
	// keyed by session, so the index log and summary can report them.
	contentRecoveries map[SessionID]contentRecovery

	// locationCache is pre-populated before the DIFF stage via BulkLookupSessionLocations.
	// It maps SessionID → SessionLocation (host_slug + parent_id) for sessions already
	// in the DB, enabling O(1) fast-path lookups in findMetadataPath without per-session
	// DB round-trips. Nil before Run() populates it.
	locationCache map[SessionID]SessionLocation

	// storeWriteMu is the fallback serializer for SQLite write phases that do not
	// have a storeWriteLane. Streamed ingest and reindex paths use a lane so all
	// concurrent DB INSERT, INDEX, COMPUTE, and ANNOTATE writers share one queue.
	storeWriteMu sync.Mutex

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
	// Retained artifacts reconciled during this invocation are candidates for
	// input-based work selection, not unconditional requests to run an indexer.
	reconciledArtifacts []SessionID

	// discoveryDiagnostics accumulates per-location discovery failures reported
	// by adapters during discover(), copied into every PipelineResult so a
	// skipped database is visible even though discovery stayed non-fatal.
	discoveryDiagnostics []DiscoveryDiagnostic
	diagnosticsMu        sync.Mutex
	diagnostics          []DiagnosticEntry
	diagnosticSet        map[DiagnosticEntry]struct{}

	// v2 analytics stages (all optional; nil = skip stage).
	redactor               TextRedactor                  // REDACT stage: applied before writing metadata to disk
	indexers               map[Harness]TranscriptIndexer // INDEX stage: parses transcripts into session_entries
	harvesterVersions      map[Harness]HarvesterVersions // current targets, independent from stored producer stamps
	metricsStore           MetricsStore                  // INDEX stage: persists session_entries
	analyzer               SessionAnalyzer               // COMPUTE stage: computes metrics + insights
	logger                 IngestLogger                  // AUDIT stage: records ingest run to ingest_log
	indexLogger            IndexLogger                   // INDEX stage: records per-session indexing outcomes to index_log
	gitAnalyzer            GitDiffAnalyzer               // EXTRACT+WRITE stage: commit detection (optional)
	commitTranscriptReader CommitTranscriptReader

	classifier SessionClassifier // ANNOTATE stage: runs classifiers + persists results (optional; nil = skip).
}

type storeWriteJob struct {
	run  func()
	done chan struct{}
}

type storeWriteLane struct {
	jobs chan storeWriteJob
	wg   sync.WaitGroup
}

func newStoreWriteLane(buffer int) *storeWriteLane {
	if buffer < 1 {
		buffer = 1
	}
	lane := &storeWriteLane{jobs: make(chan storeWriteJob, buffer)}
	lane.wg.Add(1)
	go func() {
		defer lane.wg.Done()
		for job := range lane.jobs {
			job.run()
			close(job.done)
		}
	}()
	return lane
}

func (lane *storeWriteLane) do(run func()) {
	if lane == nil {
		run()
		return
	}
	done := make(chan struct{})
	lane.jobs <- storeWriteJob{run: run, done: done}
	<-done
}

func (lane *storeWriteLane) close() {
	if lane == nil {
		return
	}
	close(lane.jobs)
	lane.wg.Wait()
}

func (p *Pipeline) runStoreWrite(lane *storeWriteLane, run func()) {
	if lane != nil {
		lane.do(run)
		return
	}
	p.storeWriteMu.Lock()
	defer p.storeWriteMu.Unlock()
	run()
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
	if err := p.validateHarvesterVersions(); err != nil {
		return nil, err
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
func (p *Pipeline) Run(ctx context.Context) (result *PipelineResult, err error) {
	p.resetDiagnostics()
	p.locationCache = nil
	p.seqCursorCache = nil
	p.reconciledArtifacts = nil
	defer func() {
		if result != nil {
			result.Diagnostics = p.snapshotDiagnostics()
		}
	}()
	if err := p.validateHarvesterVersions(); err != nil {
		return nil, err
	}
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pipeline start: %w", err)
	}
	if !p.config.DryRun {
		p.reconcileManagedArtifacts(ctx)
	} else {
		p.inspectDryRunArtifacts(ctx)
	}

	// REINDEX mode: alternative code path that scans peasant-sync output
	// instead of discovering from source providers.
	if p.config.Reindex {
		return p.runReindex(ctx, start)
	}

	prog := p.config.Progress

	// Stage 1: DISCOVER
	discoverProfileStart := time.Now()
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDiscover})
	allSessions, err := p.discover(ctx)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiscover, Err: err})
		p.recordIndexProfileStage(StageDiscover, discoverProfileStart, 0, 0)
		if ctx.Err() != nil || !p.hasUsableRetainedSession(ctx) {
			if ctx.Err() == nil && p.hasStoredMetricRefresh() {
				// Native discovery can fail even though stored entries remain
				// sufficient for downstream retry. Preserve the discovery error.
				_, _, _, days := p.refreshStoredMetrics(ctx)
				if len(days) > 0 {
					if insightErr := p.analyzer.ComputeInsights(ctx, slices.Collect(maps.Keys(days))); insightErr != nil {
						slog.Warn("harvest: refresh stored-session daily summaries", "error", insightErr)
					}
				}
			}
			return nil, fmt.Errorf("pipeline discover: %w", err)
		}
		p.reportDiagnostic(DiagnosticEntry{ErrorType: "native_discovery_unavailable", Location: "native discovery", Message: err.Error() + "; continuing maintenance of usable retained sessions", Remediation: "Restore access to the configured harness sources and retry harvest to acquire unseen native changes."})
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiscover, Done: len(allSessions), Total: len(allSessions)})
	p.recordIndexProfileStage(StageDiscover, discoverProfileStart, len(allSessions), len(allSessions))
	prepareProfileStart := time.Now()
	if p.config.PrepareSessionFilter != nil {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("pipeline prepare session filter after discovery: %w", err)
		}
		if err := p.config.PrepareSessionFilter(ctx, allSessions); err != nil {
			p.recordIndexProfileStage(StagePrepare, prepareProfileStart, 0, len(allSessions))
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
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pipeline prepare session filter: %w", err)
	}
	p.originResolve, p.originResolveErr = p.resolveStoredOrigins(ctx)
	if err := pipelineCancellation(ctx, p.originResolveErr); err != nil {
		return nil, fmt.Errorf("pipeline prepare stored origins: %w", err)
	}

	// Pre-DIFF: bulk-load session locations from DB to avoid per-session queries
	// in findMetadataPath. A single SELECT ... WHERE session_id IN (...) replaces
	// 4010× ReadDir + N× Stat calls, making the DIFF stage O(1) per session.
	if p.store != nil {
		ids := make([]SessionID, len(allSessions))
		for i, s := range allSessions {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("pipeline prepare session locations: %w", err)
			}
			ids[i] = s.SessionID
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("pipeline prepare session locations: %w", err)
		}
		cache, err := p.store.BulkLookupSessionLocations(ctx, ids)
		if err := pipelineCancellation(ctx, err); err != nil {
			return nil, fmt.Errorf("pipeline prepare session locations: %w", err)
		}
		if err == nil {
			p.locationCache = cache
		}
		// On error, paths fall back to a walk. Before any rewrite, processSession
		// still checks the authoritative stored schema with a bounded lookup.

		// Load the OpenCode change cursor when the store records it, so the DIFF
		// stage can re-ingest a session whose newest event sequence moved past the
		// last ingested value even when no time column changed. A store without the
		// cursor capability keeps the clock-only behaviour.
		if cursorStore, ok := p.store.(OpenCodeSeqCursorStore); ok {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("pipeline prepare session cursors: %w", err)
			}
			cursors, cursorErr := cursorStore.BulkLookupOpenCodeSeqCursors(ctx, ids)
			if err := pipelineCancellation(ctx, cursorErr); err != nil {
				return nil, fmt.Errorf("pipeline prepare session cursors: %w", err)
			}
			if cursorErr == nil {
				p.seqCursorCache = cursors
			}
		}
	}
	p.recordIndexProfileStage(StagePrepare, prepareProfileStart, len(allSessions), len(allSessions))

	// Stage 2: DIFF
	diffProfileStart := time.Now()
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: len(allSessions)})
	diffResult, err := p.diff(ctx, allSessions, prog)
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiff, Done: len(diffResult.Sessions), Total: len(allSessions), Err: err})
	p.recordIndexProfileStage(StageDiff, diffProfileStart, len(diffResult.Sessions), len(allSessions))
	if err != nil {
		return nil, fmt.Errorf("pipeline diff: %w", err)
	}

	// Stage 3: FILTER + Stage 4a: EXTRACT + WRITE
	filterProfileStart := time.Now()
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageFilter, Total: len(diffResult.Sessions)})

	// (indexedMeta defined at package level for use by helper methods)

	// Separate sessions into two buckets:
	//   skipped — unselected or unchanged captured sources: no processing.
	//   toProcess — new or changed sources, including active sessions.
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

	for index, entry := range diffResult.Sessions {
		advanceFilter := func() {
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageFilter, Done: index + 1, Total: len(diffResult.Sessions)})
		}
		// AllowedSessionIDs filter: skip sessions not in the allowed set.
		if p.config.AllowedSessionIDs != nil && !p.config.AllowedSessionIDs[entry.Session.SessionID] {
			recordDryRun(entry, DiffUnchanged)
			sessionResults = append(sessionResults, SessionResult{
				SessionID:  entry.Session.SessionID,
				Harness:    entry.Session.Harness,
				ParentUUID: entry.Session.ParentUUID,
				Status:     DiffUnchanged,
			})
			advanceFilter()
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
			advanceFilter()
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
					advanceFilter()
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
				advanceFilter()
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
				advanceFilter()
				continue
			}
		}

		// A supported source the discovery hint could not prove unchanged is
		// decided inside the bounded root worker, with captured bytes, never
		// captured into the whole-batch discovery maps. A source the hint
		// proves unchanged (known clock, no newer schema, no captured evidence
		// to re-compare) is recorded as-is: no native read, no stat.
		if supportsSessionCapture(entry.Session) && entry.Status != DiffUnchanged && !p.config.DryRun {
			toProcessEntries = append(toProcessEntries, entry)
			advanceFilter()
			continue
		}
		if supportsSessionCapture(entry.Session) && entry.Status != DiffUnchanged && p.config.DryRun {
			// Dry-run has no workers or staging. Compare one temporary capture
			// at a time, retaining only the classification in its report.
			captured, captureErr := p.captureSession(ctx, entry.Session)
			if captureErr != nil {
				entry.Status = DiffUpdated
			} else {
				if captured.Session != nil {
					entry.Session = *captured.Session
				}
				status, err := p.classifyCapturedSession(ctx, entry.Session, captured)
				if err != nil {
					return nil, err
				}
				entry.Status = status
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
			// Activity is diagnostic only; capture a finite source view by default.
			toProcessEntries = append(toProcessEntries, entry)
		default: // DiffNew, DiffUpdated
			recordDryRun(entry, entry.Status)
			toProcessEntries = append(toProcessEntries, entry)
		}
		advanceFilter()
	}
	priorEntries := len(toProcessEntries)
	toProcessEntries = p.appendStoredAdapterWork(ctx, toProcessEntries, allSessions)
	adapterQueued := make(map[SessionID]bool, len(toProcessEntries)-priorEntries)
	for _, entry := range toProcessEntries[priorEntries:] {
		adapterQueued[entry.Session.SessionID] = true
	}
	keptResults := sessionResults[:0]
	for _, result := range sessionResults {
		if !adapterQueued[result.SessionID] {
			keptResults = append(keptResults, result)
		}
	}
	sessionResults = keptResults
	keptDryRun := dryRunSessions[:0]
	for _, result := range dryRunSessions {
		if !adapterQueued[result.SessionID] {
			keptDryRun = append(keptDryRun, result)
		}
	}
	dryRunSessions = keptDryRun
	for _, entry := range toProcessEntries[priorEntries:] {
		recordDryRun(entry, entry.Status)
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageFilter, Done: len(diffResult.Sessions), Total: len(diffResult.Sessions)})
	p.recordIndexProfileStage(StageFilter, filterProfileStart, len(toProcessEntries), len(diffResult.Sessions))
	toProcess := len(toProcessEntries)

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
		result.Summary.HarvesterVersions = maps.Clone(p.versionTargets())
		result.Summary.MetadataVersion = int(CurrentSchemaVersion)
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
	// Every session can independently fail reconciliation. The controller reads
	// errors after workers finish, so reserve the complete bounded run's capacity
	// rather than assuming full drain batches while producers run concurrently.
	errChSize := len(toProcessEntries) + 1
	if errChSize < 16 {
		errChSize = 16
	}
	errCh := make(chan error, errChSize)
	indexQueueSize := workers * 2
	if indexQueueSize < 1 {
		indexQueueSize = 1
	}
	indexCh := make(chan streamedIndexWork, indexQueueSize)
	indexDoneCh := make(chan DrainBatch, errChSize)
	downstreamCh := make(chan indexedMeta, indexQueueSize)
	writeLane := newStoreWriteLane(indexQueueSize)

	var wg sync.WaitGroup
	var drainResults []SessionResult
	var drainIndexed []indexedMeta
	var drainIndexLogEntries []IndexLogEntry
	var drainDownstream streamedDownstreamResult

	// Stage 4a: EXTRACT+WRITE workers goroutine.
	// Processes all root entries and their subtrees in parallel, writing to staging.
	wg.Add(1)
	go func() {
		defer wg.Done()
		extractProfileStart := time.Now()
		runParallel(ctx.Err, rootEntries, workers, func(entry DiffEntry) workerResult {
			// Process root.
			wr := p.processSession(ctx, entry)
			done := int(extractDoneAtomic.Add(1))
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageExtract, Done: done, Total: toProcess})
			staging.Add(wr)
			// The root's heap payload must not outlive transfer to the arena,
			// including while this worker walks a large descendant subtree.
			wr.transcriptData = nil
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
		p.recordIndexProfileStage(StageExtract, extractProfileStart, extractDone, toProcess)
		workersDone.Store(true)
	}()

	// Stage 4b: Consumer goroutine — DB INSERT + INDEX coordination.
	// Drains staging, inserts to DB, and streams eligible sessions to INDEX.
	wg.Add(1)
	go func() {
		defer wg.Done()
		dbInsertProfileStart := time.Now()
		defer close(indexCh) // signal INDEX goroutine to stop when consumer exits
		emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDBInsert, Total: len(toProcessEntries)})
		drainResults = p.drainLoop(ctx, staging, &workersDone, indexCh, indexDoneCh, errCh, prog, len(toProcessEntries), writeLane)
		emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDBInsert, Done: len(drainResults), Total: len(toProcessEntries)})
		p.recordIndexProfileStage(StageDBInsert, dbInsertProfileStart, len(drainResults), len(toProcessEntries))
	}()

	// INDEX goroutine: reads streamed metadata from consumer and indexes each session.
	// Arena data is still valid until parser workers finish reading each drain batch.
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: len(toProcessEntries)})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(downstreamCh)
		indexProfileStart := time.Now()
		drainIndexed, drainIndexLogEntries = p.indexLoop(ctx, indexCh, indexDoneCh, prog, IndexOutcomeIndexed, "pipeline", downstreamCh, writeLane)
		p.recordIndexProfileStage(StageIndex, indexProfileStart, len(drainIndexed), len(toProcessEntries))
	}()

	// COMPUTE + ANNOTATE goroutine: starts as soon as INDEX stores one session.
	wg.Add(1)
	go func() {
		defer wg.Done()
		drainDownstream = p.runStreamedDownstream(ctx, downstreamCh, prog, len(toProcessEntries), "pipeline", writeLane)
	}()

	// Controller: wait for all goroutines to complete.
	wg.Wait()
	writeLane.close()
	close(errCh)

	// Collect store errors (last error wins — existing behavior).
	var storeErr error
	for err := range errCh {
		storeErr = err
	}

	// Merge goroutine session results into main result slice.
	sessionResults = append(sessionResults, drainResults...)

	// Stage 4c: maintain stored indexes independently of native discovery.
	// Newly reconciled and older-producer sessions are checked first; the managed
	// inventory also covers missing proofs and failures at an unchanged revision.
	if p.metricsStore != nil {
		backfilled, backfillErr := p.backfillIncompleteContent(ctx)
		if backfillErr != nil {
			slog.Warn("pipeline: incomplete content recovery", "error", backfillErr)
		}
		p.contentRecoveries = backfilled
		staleIDs, staleErr := p.metricsStore.ListStaleIndexSessions(ctx, p.indexerTargets())
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
		for _, im := range drainIndexed {
			queued[im.session.SessionID] = true
		}
		for _, result := range drainResults {
			if result.mirrorPending {
				queued[result.SessionID] = true
			}
		}
		candidates := append([]SessionID(nil), p.reconciledArtifacts...)
		candidates = append(candidates, staleIDs...)
		// A recovered session is content-repaired, not complete: it still gets
		// the same adapter/indexer evaluation as every other eligible target. An
		// already-current session sees an equal input hash and writes nothing.
		for _, sid := range candidates {
			if queued[sid] {
				continue
			}

			// Reconstruct DiscoveredSession from peasant-sync metadata.
			reconstructed, startMs, transcriptPath, metadataErr := p.reconstructFromMetadata(ctx, sid)
			if metadataErr != nil {
				p.reportMetadataRefusal(string(sid), metadataErr)
				slog.Warn("pipeline: retained metadata refused", "session_id", sid, "error", metadataErr)
				continue // Unsupported is not missing: never bypass through DB source info.
			}
			if reconstructed == nil {
				// Fallback: reconstruct from DB source_path/source_format.
				// This handles sessions (e.g. subagents) that were indexed from
				// the original source but don't have peasant-sync metadata.
				reconstructed, startMs, transcriptPath = p.reconstructFromSourceInfo(ctx, sid)
				if reconstructed == nil {
					continue
				}
			}
			queued[sid] = true
			if p.indexTargetNeedsWork(ctx, reindexTarget{session: *reconstructed, startMs: startMs, transcriptPath: transcriptPath}) {
				indexSessions = append(indexSessions, indexedMeta{session: *reconstructed, startMs: startMs, outputTranscriptPath: transcriptPath})
			}
		}
		// An earlier invocation may have mirrored files but failed indexing at
		// the same producer revision. Inspect retained inputs, not artifact/index
		// hash domains against one another or only this run's changed-file list.
		for _, target := range p.scanPeasantSyncSessions(ctx) {
			if queued[target.session.SessionID] {
				continue
			}
			queued[target.session.SessionID] = true
			if p.indexTargetNeedsWork(ctx, target) {
				indexSessions = append(indexSessions, indexedMeta{session: target.session, startMs: target.startMs, outputTranscriptPath: target.transcriptPath})
			}
		}
	}

	// Stages 5-9: INDEX, COMPUTE, CLEANUP, REPORT, AUDIT (shared with runReindex).
	return p.indexComputeAndFinalize(ctx, indexSessions, drainIndexed, sessionResults, storeErr, start, append(drainIndexLogEntries, p.contentRecoveryLogEntries()...), IndexOutcomeIndexed, "pipeline", &drainDownstream)
}

// contentRecoveryReason labels a retained-content repair in the index log.
// Recovery reuses the reindexed outcome with this reason: no new outcome
// value and no new JSON shape, but the repair is visible and counted.
const contentRecoveryReason = "content recovered from retained input"

// contentRecoveryLogEntries reports this run's completed retained-content
// repairs as index log entries, so a repair a preliminary sweep performed
// does not disappear from the run's reported outcomes.
func (p *Pipeline) contentRecoveryLogEntries() []IndexLogEntry {
	if len(p.contentRecoveries) == 0 {
		return nil
	}
	ids := make([]SessionID, 0, len(p.contentRecoveries))
	for id := range p.contentRecoveries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	entries := make([]IndexLogEntry, 0, len(ids))
	for _, id := range ids {
		recovery := p.contentRecoveries[id]
		reason := contentRecoveryReason
		entries = append(entries, p.makeIndexLogEntry(indexedMeta{session: recovery.session, outputTranscriptPath: recovery.session.SourcePath.String()}, IndexOutcomeReindexed, recovery.entries, recovery.recoveredAt, &reason, nil))
	}
	return entries
}

// drainLoop is the consumer goroutine for stage 4b (DB INSERT).
//
// It loops until all workers have finished and the staging buffer is empty,
// draining completed workerResults in batches. For each batch it:
//  1. Performs DB INSERT (InsertSessions + UpsertSessionCommits) — best-effort.
//  2. Calls staging.Commit(ids) — marks sessions as DB-committed so their
//     children become eligible for the next Drain (Commit BEFORE Ack is deliberate).
//  3. Streams each indexable session to INDEX with a drain-batch completion token.
//  4. Calls staging.AckBatch(batch) only after INDEX parser workers finish reading
//     every streamed session that belongs to the batch.
//
// Store errors are sent to errCh (buffered); the controller collects them after wg.Wait.
// Bounded backoff (1ms sleep) is used when the buffer is empty but workers are still running.
func (p *Pipeline) drainLoop(
	ctx context.Context,
	staging *StagingBuffer,
	workersDone *atomic.Bool,
	indexCh chan<- streamedIndexWork,
	indexDoneCh <-chan DrainBatch,
	errCh chan<- error,
	prog *ProgressState,
	toProcess int,
	writeLane *storeWriteLane,
) (sessionResults []SessionResult) {
	pendingAckBatches := 0
	dbInsertDone := 0
	ackBatch := func(batch DrainBatch) {
		staging.AckBatch(batch)
		if pendingAckBatches > 0 {
			pendingAckBatches--
		}
	}
	drainReadyAcks := func() {
		for {
			select {
			case batch := <-indexDoneCh:
				ackBatch(batch)
			default:
				return
			}
		}
	}
	sendIndexWork := func(work streamedIndexWork) {
		for {
			select {
			case indexCh <- work:
				return
			case batch := <-indexDoneCh:
				ackBatch(batch)
			}
		}
	}
	waitForPendingAcks := func() {
		for pendingAckBatches > 0 {
			batch := <-indexDoneCh
			ackBatch(batch)
		}
	}

	for {
		drainReadyAcks()
		done := workersDone.Load()
		batch := staging.Drain()

		if len(batch.Results) > 0 {
			var batchMetas []indexedMeta
			var committedIDs []SessionID
			for index := range batch.Results {
				wr := &batch.Results[index]
				indexReady := wr.result.Error == nil
				if wr.result.Error == nil && wr.artifact != nil {
					publisher, err := p.artifactPublisher(writeLane)
					if err == nil {
						var reconciled *ManagedArtifact
						reconciled, err = publisher.Reconcile(ctx, wr.artifact)
						if err == nil && reconciled == nil {
							err = fmt.Errorf("artifact reconciliation for %s restored prior files instead of this worker's candidate", wr.result.SessionID)
						}
						if err == nil {
							wr.artifact = reconciled
							wr.meta = &reconciled.Metadata
						}
					}
					if err != nil {
						indexReady = false
						wr.result.mirrorPending = true
						failure := fmt.Errorf("reconcile committed session %s: %w; recovery state was retained for a later harvest", wr.result.SessionID, err)
						p.reportDiagnostic(artifactRecoveryDiagnostic(string(wr.result.SessionID), failure))
						var mirrorFailure *artifactMirrorError
						if errors.As(err, &mirrorFailure) {
							// Complete file publication still succeeded. Preserve its
							// counts, but do not authorize indexing on a failed mirror.
							errCh <- failure
						} else {
							wr.result.Error = failure
						}
					}
				}
				if indexReady && wr.result.OutputPath != "" && wr.meta != nil {
					batchMetas = append(batchMetas, indexedMeta{
						session:              sessionFromWorkerResult(*wr),
						startMs:              wr.startMs,
						outputTranscriptPath: wr.outputTranscriptPath,
						transcriptData:       wr.transcriptData,
						capturedSource:       wr.capturedSource,
						published:            wr.artifact != nil,
					})
				}
				sessionResults = append(sessionResults, wr.result)
				committedIDs = append(committedIDs, wr.result.SessionID)
			}

			// Commit BEFORE Ack — unlocks children for next Drain sooner.
			staging.Commit(committedIDs...)

			if len(batchMetas) == 0 {
				staging.AckBatch(batch)
			} else {
				batch.Metas = batchMetas
				completion := newIndexBatchCompletion(batch, len(batchMetas))
				pendingAckBatches++
				for _, im := range batchMetas {
					sendIndexWork(streamedIndexWork{meta: im, batch: completion})
				}
			}
			dbInsertDone += len(batch.Results)
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageDBInsert, Done: dbInsertDone, Total: toProcess})
			continue
		}

		if done {
			// Workers finished and buffer is empty — release all INDEX-owned arena batches.
			waitForPendingAcks()
			break
		}
		// Bounded backoff: avoid spinning when buffer is empty but workers still running.
		time.Sleep(1 * time.Millisecond)
	}
	return sessionResults
}

type streamedIndexWork struct {
	meta  indexedMeta
	batch *indexBatchCompletion
}

type streamedDownstreamResult struct {
	Computed         int
	ComputeDone      int
	AnnotateDone     int
	ComputeDuration  time.Duration
	AnnotateDuration time.Duration
	Days             map[string]bool
}

type indexBatchCompletion struct {
	batch     DrainBatch
	remaining atomic.Int64
}

func newIndexBatchCompletion(batch DrainBatch, workItems int) *indexBatchCompletion {
	if workItems < 1 {
		panic("ingest: streamed INDEX batch must have at least one work item")
	}
	completion := &indexBatchCompletion{batch: batch}
	completion.remaining.Store(int64(workItems))
	return completion
}

func (completion *indexBatchCompletion) completeWorkItem() (DrainBatch, bool) {
	if completion == nil {
		return DrainBatch{}, false
	}
	remaining := completion.remaining.Add(-1)
	if remaining < 0 {
		panic("ingest: streamed INDEX batch completion underflow")
	}
	return completion.batch, remaining == 0
}

// indexLoop is the INDEX goroutine for stage 4b.
//
// It reads per-session work from indexCh. Parser workers run with bounded
// parallelism, and the goroutine that consumes parsed results performs all
// SQLite writes serially. A drain batch is signalled on indexDoneCh after every
// parser that can read its arena-backed transcript data has completed.
//
// outcome and logPrefix are forwarded to the writer for log annotation; use
// IndexOutcomeIndexed/"pipeline" for normal ingest.
//
// indexLoop has no panic recovery: crashes are intentional (per design decision).
func (p *Pipeline) indexLoop(
	ctx context.Context,
	indexCh <-chan streamedIndexWork,
	indexDoneCh chan<- DrainBatch,
	prog *ProgressState,
	outcome IndexOutcome,
	logPrefix string,
	downstreamCh chan<- indexedMeta,
	writeLane *storeWriteLane,
) (indexed []indexedMeta, logEntries []IndexLogEntry) {
	workers := parallelWorkers(p.config)
	if workers < 1 {
		workers = 1
	}
	parsedCh := make(chan indexParseResult, workers)
	var activeParses atomic.Int64
	var maxActiveParses atomic.Int64
	var parserWG sync.WaitGroup
	parserWG.Add(workers)
	for range workers {
		go func() {
			defer parserWG.Done()
			for work := range indexCh {
				result := p.parseIndexMeta(ctx, work.meta, &activeParses, &maxActiveParses, logPrefix)
				if batch, complete := work.batch.completeWorkItem(); complete {
					indexDoneCh <- batch
				}
				parsedCh <- result
			}
		}()
	}
	go func() {
		parserWG.Wait()
		close(parsedCh)
	}()

	profileEnabled := p.config.IndexProfiler != nil && p.indexers != nil && p.metricsStore != nil
	profileSessions := []IndexProfileSession(nil)
	profileBatch := IndexProfileBatch{Source: logPrefix, QueueCapacity: cap(indexCh)}
	indexDone := 0
	pending := make([]indexParseResult, 0, indexWriteBatchLimit)
	flushPending := func(results []indexParseResult) {
		flush := p.flushIndexParseResults(ctx, results, outcome, logPrefix, writeLane)
		for i, indexedResult := range flush.indexed {
			indexed = append(indexed, indexedResult)
			if flush.logEntries[i].SessionID != "" {
				logEntries = append(logEntries, flush.logEntries[i])
			}
			indexDone++
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageIndex, Done: indexDone})
			if indexedResult.indexed && downstreamCh != nil {
				select {
				case downstreamCh <- indexedResult:
				case <-ctx.Done():
				}
			}
		}
		if profileEnabled {
			for _, profileSession := range flush.profileSessions {
				profileBatch.Sessions++
				profileBatch.WorkItems++
				profileBatch.Entries += profileSession.Entries
				profileBatch.Bytes += profileSession.Bytes
				profileBatch.ParseDuration += profileSession.ParseDuration
				profileSessions = append(profileSessions, profileSession)
			}
			profileBatch.WriteDuration += flush.writeDuration
			profileBatch.WriteTxs += flush.writeTxs
			profileBatch.WriteSavepoints += flush.writeSavepoints
			profileBatch.WriteSkipped += flush.writeSkipped
			profileBatch.WriteStats.Add(flush.writeStats)
		}
	}
	drainIndexParseResults(parsedCh, pending, flushPending)
	if profileEnabled && profileBatch.WorkItems > 0 {
		profileBatch.MaxParseWorkers = int(maxActiveParses.Load())
		p.config.IndexProfiler.Record(profileBatch, profileSessions)
	}
	return indexed, logEntries
}

type indexParseResult struct {
	fullContent bool
	// partial reports that the strict parser refused the transcript and the
	// tolerant projection was stored instead, as an incomplete capture whose
	// recorded reason is strictRefusal. Previews show it; nothing certifies it.
	partial       bool
	strictRefusal string
	// refusalCode is why the strict parser refused, as the value the selector
	// reads back to decide that re-trying cannot help.
	refusalCode   ContentCaptureFailureCode
	im            indexedMeta
	input         *CapturedIndexInput
	output        indexformat.Result
	entryCount    int
	startedAt     int64
	logEntry      IndexLogEntry
	parseDuration time.Duration
	bytes         int64
}

// permanentRefusalCode names the refusal when this build can never clear it,
// and ContentCaptureNoFailure when it might.
//
// A session whose retained transcript is marked as missing records is refused
// for that reason whatever else the parser would have said, so it is checked
// first: the strict parser stops at the omission before it can reach a record
// it does not represent.
func permanentRefusalCode(session DiscoveredSession, err error) ContentCaptureFailureCode {
	if session.ContentOmitted {
		return ContentCaptureSourceRecordsOmitted
	}
	var unrepresented *UnrepresentedRecordError
	if errors.As(err, &unrepresented) {
		return ContentCaptureStrictRefused
	}
	return ContentCaptureNoFailure
}

// permanentRefusalDiagnostic tells the user what was stored and what it costs
// them, in the words of the cause. Both causes leave previews working and
// both refuse export and publication; they differ in what would fix them, and
// for an omitted record nothing the user does to this transcript will.
func permanentRefusalDiagnostic(sid SessionID, code ContentCaptureFailureCode, err error) DiagnosticEntry {
	entry := DiagnosticEntry{
		ErrorType: "content_capture_incomplete", Location: string(sid),
		Message:     fmt.Sprintf("index session %s: the strict parser refused the transcript: %v; the represented entries were stored as an incomplete capture, so previews show them while export and publication stay refused until a complete capture exists", sid, err),
		Remediation: "Regenerate the source with a supported harness version or upgrade Peasant so every record is represented, then rerun harvest index --force.",
	}
	if code == ContentCaptureSourceRecordsOmitted {
		// Three ingest diagnostics raise this code and each has its own remedy:
		// reduce an oversized event and re-ingest, upgrade for a part type this
		// build cannot render, or accept that the native source never had the
		// missing parent. Which one happened is recorded in the session's
		// metadata, so this points there instead of naming a cause it cannot
		// tell apart, and it must not contradict what the message already
		// tells the user to do.
		entry.Remediation = "Ingest omitted source records from this transcript, so re-harvesting the same source omits them again. The session's metadata diagnostics name each omitted record and what fixes it; act on those, or accept the stored preview."
	}
	return entry
}

// exceedsIndexWriteBudget reports whether the pending write batch must be
// flushed BEFORE the next parsed result joins it.
//
// Two sites group parsed results into SQLite writes: the streaming drain that
// every session takes on an ordinary harvest and on a reindex, and indexBatch,
// which groups the stale-session waves. Both bound the same thing for the same
// reason, so they ask the same question here rather than each spelling it out:
// a batch stops at indexWriteBatchLimit sessions, and a non-empty batch stops
// before its full strings would exceed defaults.FullContentWriteBatchBytes.
//
// A single result larger than the whole budget is still written, alone: the
// batch it would join is flushed first, and the empty-batch term then admits
// it. Refusing it instead would lose the session.
func exceedsIndexWriteBudget(pendingCount int, pendingBytes, nextBytes int64) bool {
	if pendingCount >= indexWriteBatchLimit {
		return true
	}
	return pendingCount > 0 && pendingBytes+nextBytes > defaults.FullContentWriteBatchBytes
}

// drainIndexParseResults groups everything the parsers produce into write
// batches and hands each batch to flush, in order, until the channel closes.
//
// It takes results one at a time and then absorbs whatever else has already
// arrived, so a burst of small sessions becomes one write rather than many.
// The batch it accumulates is bounded by exceedsIndexWriteBudget, which is the
// memory bound on the primary harvest path: without it a long run of large
// transcripts is held in full, in memory, until the parsers stop.
//
// pending is the caller's reusable buffer; it is cleared before return.
func drainIndexParseResults(parsedCh <-chan indexParseResult, pending []indexParseResult, flush func([]indexParseResult)) {
	for {
		result, ok := <-parsedCh
		if !ok {
			break
		}
		pending = append(pending, result)
		pendingBytes := indexResultWriteBytes(result.output)
		parsedClosed := false
		// No bound is restated here: exceedsIndexWriteBudget owns the split and
		// applies it below, before each result joins the batch, so the batch
		// never grows past the limit or the budget however long this absorbs.
		// A second copy of those terms would be one more place to miss.
	drainParsed:
		for {
			select {
			case next, ok := <-parsedCh:
				if !ok {
					parsedClosed = true
					break drainParsed
				}
				nextBytes := indexResultWriteBytes(next.output)
				if exceedsIndexWriteBudget(len(pending), pendingBytes, nextBytes) {
					flush(pending)
					clear(pending)
					pending = pending[:0]
					pendingBytes = 0
				}
				pending = append(pending, next)
				pendingBytes += nextBytes
			default:
				break drainParsed
			}
		}
		flush(pending)
		clear(pending)
		pending = pending[:0]
		if parsedClosed {
			break
		}
	}
}

func indexResultWriteBytes(result indexformat.Result) int64 {
	if v1, ok := result.(indexformat.V1); ok {
		return fullEntryWriteBytes(v1.Entries)
	}
	return 0
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
		return p.parseIndexMeta(ctx, im, &activeParses, &maxActiveParses, logPrefix)
	}

	workers := max(1, parallelWorkers(p.config))
	// Retain at most one bounded parser wave, not every full session in a
	// reindex invocation. The writer further splits each wave by full bytes.
	waveSize := min(workers, indexWriteBatchLimit)
	profileSessions := make([]IndexProfileSession, 0, len(metas))
	profileBatch := IndexProfileBatch{Source: logPrefix, Sessions: len(metas), WorkItems: len(metas)}
	pending := make([]indexParseResult, 0, indexWriteBatchLimit)
	var pendingBytes int64
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		flush := p.flushIndexParseResults(ctx, pending, outcome, logPrefix, nil)
		indexed = append(indexed, flush.indexed...)
		logs = append(logs, flush.logEntries...)
		for _, profileSession := range flush.profileSessions {
			profileBatch.Entries += profileSession.Entries
			profileBatch.Bytes += profileSession.Bytes
			profileBatch.ParseDuration += profileSession.ParseDuration
			profileSessions = append(profileSessions, profileSession)
		}
		profileBatch.WriteDuration += flush.writeDuration
		profileBatch.WriteTxs += flush.writeTxs
		profileBatch.WriteSavepoints += flush.writeSavepoints
		profileBatch.WriteSkipped += flush.writeSkipped
		profileBatch.WriteStats.Add(flush.writeStats)
		clear(pending)
		pending = pending[:0]
		pendingBytes = 0
	}
	for start := 0; start < len(metas); start += waveSize {
		end := min(start+waveSize, len(metas))
		var parsed []indexParseResult
		if workers > 1 && end-start > 1 {
			parsed = runParallel(func() error { return nil }, metas[start:end], workers, parseOne)
		} else {
			parsed = []indexParseResult{parseOne(metas[start])}
		}
		for _, result := range parsed {
			size := indexResultWriteBytes(result.output)
			if exceedsIndexWriteBudget(len(pending), pendingBytes, size) {
				flushPending()
			}
			pending = append(pending, result)
			pendingBytes += size
		}
	}
	flushPending()
	profileBatch.MaxParseWorkers = int(maxActiveParses.Load())
	p.config.IndexProfiler.Record(profileBatch, profileSessions)
	return indexed, logs
}

func (p *Pipeline) parseIndexMeta(ctx context.Context, im indexedMeta, activeParses *atomic.Int64, maxActiveParses *atomic.Int64, logPrefix string) (result indexParseResult) {
	result = indexParseResult{im: im, startedAt: time.Now().UnixMilli(), bytes: p.indexProfileBytes(im)}
	defer func() {
		result.im.transcriptData = nil
		if im.capturedSource != nil {
			// Staging slots outlive parsing. Drop the detached source tree now,
			// not at the end of a potentially large ingest run.
			im.capturedSource.release()
			result.im.capturedSource = nil
		}
	}()
	if p.indexers == nil || p.metricsStore == nil {
		return result
	}
	if err := p.checkStoredMetadataVersion(ctx, im.session.SessionID); err != nil {
		p.reportMetadataRefusal(string(im.session.SessionID), err)
		slog.Warn(logPrefix+": stored metadata refused", "session_id", im.session.SessionID, "error", err)
		errMsg := err.Error()
		result.logEntry = p.makeIndexLogEntry(im, IndexOutcomeError, 0, result.startedAt, nil, &errMsg)
		return result
	}
	indexer, ok := p.indexers[im.session.Harness]
	if !ok {
		reason := "no indexer for provider"
		result.logEntry = p.makeIndexLogEntry(im, IndexOutcomeSkipped, 0, result.startedAt, &reason, nil)
		return result
	}

	active := activeParses.Add(1)
	if sourceIndexer, ok := indexer.(*OpenCodeIndexer); ok && im.capturedSource != nil {
		capturedIndexer := *sourceIndexer
		capturedIndexer.fs = im.capturedSource
		indexer = &capturedIndexer
	}
	recordIndexProfileMax(maxActiveParses, active)
	parseStart := time.Now()
	input, err := p.captureIndexInput(ctx, im, indexer)
	var output indexformat.Result
	parsed := false
	declared := p.versionTargets()[im.session.Harness].IndexVersion
	if err == nil {
		result.input = input
		// The write binds the index to the current metadata capture when the
		// input is one the capture vouches for; otherwise the caller's
		// revision stands, and a retained fallback's is unbound, so a tree
		// re-read without byte proof holds publication instead of certifying it.
		if input.bindsPublication() {
			result.im.captureRevision = input.expected.PublicationCaptureRevision
		}
		if p.capturedInputNeedsWork(input) {
			parsed = true
			output, err = parseCapturedIndexInput(ctx, indexer, input, declared)
			// A refusal NOTHING ABOUT THIS BUILD CAN LIFT is not an empty store:
			// the represented entries are stored as an incomplete capture so
			// previews can show them, export and publication stay refused until a
			// complete capture exists, and the refusal is recorded with the
			// capture and reported once. A malformed transcript stays a visible
			// error.
			//
			// Two causes qualify. The strict parser met a well-formed record this
			// build does not represent; or the retained transcript is KNOWN to be
			// missing records, because ingest removed a source record longer than
			// the scanner's line limit before writing the artifact. Re-reading
			// either one gives the same answer, and re-harvesting the same source
			// omits the same record again.
			if _, strict := indexer.(AuthoritativeTranscriptIndexer); err != nil && strict && declared == strictIndexFormat && ctx.Err() == nil {
				if code := permanentRefusalCode(im.session, err); code != ContentCaptureNoFailure {
					if tolerant, tolerantErr := input.ParseTolerant(ctx, indexer); tolerantErr == nil {
						result.partial, result.strictRefusal, result.refusalCode = true, err.Error(), code
						p.reportDiagnostic(permanentRefusalDiagnostic(im.session.SessionID, code, err))
						output, err = tolerant, nil
					}
				}
			}
		} else {
			reason := "stored index already matches captured input and current producer"
			result.logEntry = p.makeIndexLogEntry(im, IndexOutcomeSkipped, 0, result.startedAt, &reason, nil)
		}
		input.transcript, input.tree = nil, nil
	}
	// Only the strict format-1 capture path certifies complete content. A
	// declared non-strict format is stored as declared, never as a full capture.
	_, authoritative := indexer.(AuthoritativeTranscriptIndexer)
	result.fullContent = authoritative && err == nil && parsed && declared == strictIndexFormat && !result.partial
	result.parseDuration = time.Since(parseStart)
	activeParses.Add(-1)
	if err == nil && parsed {
		var version int
		version, err = indexformat.VersionOf(output)
		if err == nil && version != p.versionTargets()[im.session.Harness].IndexVersion {
			err = fmt.Errorf("indexer result format %d does not match declared format %d for harness %s; no entries were replaced; correct the indexer declaration or concrete output", version, p.versionTargets()[im.session.Harness].IndexVersion, im.session.Harness)
		}
	}
	if err != nil {
		var empty *unverifiedEmptyIndexError
		if errors.As(err, &empty) {
			p.reportDiagnostic(DiagnosticEntry{ErrorType: "index_empty_unverified", Location: string(im.session.SessionID), Message: empty.Error(), Remediation: "Restore readable source records, or use an indexer that verifies completed-empty input, and retry harvest."})
			reason := empty.Error()
			result.logEntry = p.makeIndexLogEntry(im, IndexOutcomeSkipped, 0, result.startedAt, &reason, nil)
			return result
		}
		slog.Warn(logPrefix+": index transcript", "session_id", im.session.SessionID, "error", err)
		p.reportIndexRefusal(im.session.SessionID, err)
		errMsg := err.Error()
		result.logEntry = p.makeIndexLogEntry(im, IndexOutcomeError, 0, result.startedAt, nil, &errMsg)
		return result
	}
	result.output = output
	if v1, ok := output.(indexformat.V1); ok {
		result.entryCount = len(v1.Entries)
	}
	return result
}

type indexWriteFlush struct {
	indexed         []indexedMeta
	logEntries      []IndexLogEntry
	profileSessions []IndexProfileSession
	writeDuration   time.Duration
	writeTxs        int
	writeSavepoints int
	writeSkipped    int
	writeStats      SessionEntryWriteStats
}

func (p *Pipeline) flushIndexParseResults(ctx context.Context, results []indexParseResult, outcome IndexOutcome, logPrefix string, writeLane *storeWriteLane) indexWriteFlush {
	batchStore, ok := p.metricsStore.(SessionEntryBatchStore)
	if !ok || len(results) == 0 {
		return p.flushIndexParseResultsOneByOne(ctx, results, outcome, logPrefix, writeLane)
	}
	return p.flushIndexParseResultsBatch(ctx, results, batchStore, outcome, logPrefix, writeLane)
}

func (p *Pipeline) flushIndexParseResultsOneByOne(ctx context.Context, results []indexParseResult, outcome IndexOutcome, logPrefix string, writeLane *storeWriteLane) indexWriteFlush {
	flush := indexWriteFlush{
		indexed:         make([]indexedMeta, 0, len(results)),
		logEntries:      make([]IndexLogEntry, 0, len(results)),
		profileSessions: make([]IndexProfileSession, 0, len(results)),
	}
	for _, result := range results {
		indexedMeta, logEntry, profileSession := p.writeIndexParseResult(ctx, result, outcome, logPrefix, writeLane)
		flush.indexed = append(flush.indexed, indexedMeta)
		flush.logEntries = append(flush.logEntries, logEntry)
		flush.profileSessions = append(flush.profileSessions, profileSession)
		flush.writeDuration += profileSession.WriteDuration
	}
	return flush
}

func (p *Pipeline) flushIndexParseResultsBatch(ctx context.Context, results []indexParseResult, batchStore SessionEntryBatchStore, outcome IndexOutcome, logPrefix string, writeLane *storeWriteLane) indexWriteFlush {
	flush := indexWriteFlush{
		indexed:         make([]indexedMeta, len(results)),
		logEntries:      make([]IndexLogEntry, len(results)),
		profileSessions: make([]IndexProfileSession, len(results)),
	}
	writes := make([]SessionEntryWrite, 0, len(results))
	writePositions := make([]int, 0, len(results))
	nowMs := time.Now().UnixMilli()
	for i, result := range results {
		if result.output == nil || p.metricsStore == nil {
			flush.indexed[i] = indexedMeta{session: result.im.session, startMs: result.im.startMs}
			flush.logEntries[i] = result.logEntry
			flush.profileSessions[i] = p.makeIndexProfileSession(result, result.logEntry, 0)
			continue
		}
		if result.input == nil {
			// Parsed output without its captured input has no expected state
			// and no input identity to prove. Refuse to stamp it: an error
			// outcome is visible; a fabricated empty capture would not be.
			err := fmt.Errorf("%s: session %s produced parsed output without a captured input, so the store cannot verify what was parsed; the stored index was preserved; capture the input through the ordinary index path and retry", logPrefix, result.im.session.SessionID)
			p.reportIndexRefusal(result.im.session.SessionID, err)
			errMsg := err.Error()
			logEntry := p.makeIndexLogEntry(result.im, IndexOutcomeError, 0, result.startedAt, nil, &errMsg)
			flush.indexed[i] = indexedMeta{session: result.im.session, startMs: result.im.startMs}
			flush.logEntries[i] = logEntry
			flush.profileSessions[i] = p.makeIndexProfileSession(result, logEntry, 0)
			continue
		}
		capture := SessionContentCaptureWrite{}
		if result.fullContent {
			capture = SessionContentCaptureWrite{Status: ContentCaptureComplete, SourceAuthority: contentAuthorityFor(result), TranscriptOrigin: result.im.session.TranscriptOrigin, CaptureFormat: ContentCaptureFormatFull, CapturedAtMs: nowMs}
		} else if result.partial {
			capture = SessionContentCaptureWrite{Status: ContentCaptureIncomplete, SourceAuthority: contentAuthorityFor(result), TranscriptOrigin: result.im.session.TranscriptOrigin, CaptureFormat: ContentCaptureFormatPreviewOnly, CapturedAtMs: nowMs, FailureCode: result.refusalCode, FailureMessage: result.strictRefusal}
		}
		writes = append(writes, SessionEntryWrite{
			CaptureRevision:    result.im.captureRevision,
			RequireFullContent: result.fullContent,
			ContentCapture:     capture,
			SessionID:          result.im.session.SessionID,
			Result:             result.output,
			IndexVersion:       p.versionTargets()[result.im.session.Harness].IndexVersion,
			IndexerVersion:     p.versionTargets()[result.im.session.Harness].IndexerVersion,
			IndexedAtMs:        nowMs,
			ExpectedState:      result.input.expected,
			IndexedInputHash:   &result.input.inputHash,
		})
		writePositions = append(writePositions, i)
	}

	writeResults := make([]SessionEntryWriteResult, len(writes))
	writeDurations := make([]time.Duration, len(writes))
	writeDuration := time.Duration(0)
	if len(writes) > 0 {
		order := make([]int, len(writes))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(i, j int) bool { return writes[order[i]].SessionID < writes[order[j]].SessionID })
		for _, i := range order {
			writeStart := time.Now()
			writeResults[i].SessionID = writes[i].SessionID
			// Hold one session's file ownership before entering the writer lane.
			// Parent/child ownership is never nested; each item commits atomically.
			err := p.withCurrentIndexInput(ctx, results[writePositions[i]].input, func() error {
				p.runStoreWrite(writeLane, func() {
					flush.writeTxs++
					flush.writeSavepoints++
					written := batchStore.IndexSessionEntryBatch(ctx, []SessionEntryWrite{writes[i]})
					if len(written) != 1 || written[0].SessionID != writes[i].SessionID {
						writeResults[i].Err = fmt.Errorf("%s: store did not return the one requested session %s", logPrefix, writes[i].SessionID)
					} else {
						writeResults[i] = written[0]
					}
				})
				return writeResults[i].Err
			})
			if err != nil {
				writeResults[i].Err = err
				writeResults[i].Written = false
			}
			writeDurations[i] = time.Since(writeStart)
			writeDuration += writeDurations[i]
		}
		flush.writeDuration = writeDuration
	}
	for i, writeResult := range writeResults {
		perSessionWriteDuration := writeDurations[i]
		flush.writeStats.Add(writeResult.Stats)
		if writeResult.Skipped {
			flush.writeSkipped++
		}
		position := writePositions[i]
		result := results[position]
		writeErr := writeResult.Err
		if writeErr == nil && !writeResult.Written {
			writeErr = fmt.Errorf("%s: store did not report session %s as written", logPrefix, result.im.session.SessionID)
		}
		if writeErr != nil {
			p.reportIndexRefusal(result.im.session.SessionID, writeErr)
			slog.Warn(logPrefix+": store session entries", "session_id", result.im.session.SessionID, "error", writeErr)
			errMsg := writeErr.Error()
			logEntry := p.makeIndexLogEntry(result.im, IndexOutcomeError, result.entryCount, result.startedAt, nil, &errMsg)
			flush.indexed[position] = indexedMeta{session: result.im.session, startMs: result.im.startMs}
			flush.logEntries[position] = logEntry
			flush.profileSessions[position] = p.makeIndexProfileSession(result, logEntry, perSessionWriteDuration)
			continue
		}
		result.entryCount = writeResult.EntriesCount
		logEntry := p.makeIndexLogEntry(result.im, outcome, result.entryCount, result.startedAt, nil, nil)
		flush.indexed[position] = indexedMeta{session: result.im.session, startMs: result.im.startMs, indexed: true}
		flush.logEntries[position] = logEntry
		flush.profileSessions[position] = p.makeIndexProfileSession(result, logEntry, perSessionWriteDuration)
	}
	return flush
}

// contentAuthorityFor names where a certified full capture's bytes came from:
// an artifact this run committed is new ingest, a native OpenCode directory
// tree is the provider source, and anything else was parsed from the retained
// managed snapshot. The label never claims a native read that did not happen.
func contentAuthorityFor(result indexParseResult) ContentSourceAuthority {
	switch {
	case result.im.published:
		return ContentSourceNewIngest
	case result.input != nil && result.input.kind == TranscriptSourceDirectory:
		return ContentSourceProviderSource
	}
	return ContentSourcePeasantSnapshot
}

func (p *Pipeline) makeIndexProfileSession(result indexParseResult, logEntry IndexLogEntry, writeDuration time.Duration) IndexProfileSession {
	return IndexProfileSession{
		SessionID:     result.im.session.SessionID,
		Harness:       result.im.session.Harness,
		SourcePath:    p.indexProfileSourcePath(result.im),
		Outcome:       logEntry.Outcome,
		Entries:       result.entryCount,
		Bytes:         result.bytes,
		ParseDuration: result.parseDuration,
		WriteDuration: writeDuration,
	}
}

func (p *Pipeline) writeIndexParseResult(ctx context.Context, result indexParseResult, outcome IndexOutcome, logPrefix string, writeLane *storeWriteLane) (indexedMeta, IndexLogEntry, IndexProfileSession) {
	im := result.im
	ok := false
	logEntry := result.logEntry
	writeDuration := time.Duration(0)
	entriesCount := result.entryCount
	if result.output != nil && p.metricsStore != nil {
		// A split entries/stamp fallback could overwrite last-good output and
		// report success after the producer stamp failed. Refuse before any write.
		errMsg := "index persistence requires atomic entry and indexer-state writes; configure a SessionEntryBatchStore and retry; existing entries were preserved"
		p.reportIndexRefusal(im.session.SessionID, errors.New(errMsg))
		slog.Warn(logPrefix+": store session entries", "session_id", im.session.SessionID, "error", errMsg)
		logEntry = p.makeIndexLogEntry(im, IndexOutcomeError, entriesCount, result.startedAt, nil, &errMsg)
	}

	indexedResult := indexedMeta{session: im.session, startMs: im.startMs, indexed: ok}
	profileSession := IndexProfileSession{
		SessionID:     im.session.SessionID,
		Harness:       im.session.Harness,
		SourcePath:    p.indexProfileSourcePath(im),
		Outcome:       logEntry.Outcome,
		Entries:       entriesCount,
		Bytes:         result.bytes,
		ParseDuration: result.parseDuration,
		WriteDuration: writeDuration,
	}
	return indexedResult, logEntry, profileSession
}

func recordIndexProfileMax(maxValue *atomic.Int64, value int64) {
	for {
		current := maxValue.Load()
		if value <= current || maxValue.CompareAndSwap(current, value) {
			return
		}
	}
}

func (p *Pipeline) recordIndexProfileStage(stage Stage, started time.Time, done int, total int) {
	if p == nil {
		return
	}
	p.config.IndexProfiler.RecordStage(stage, time.Since(started), done, total)
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
		if cache, ok := p.store.(ClaudeEvidenceCache); ok && !p.config.DryRun {
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
	if p.config.DryRun {
		return ResolveReport{}, nil
	}
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
func (p *Pipeline) diff(ctx context.Context, sessions []DiscoveredSession, prog *ProgressState) (DiffResult, error) {
	result := DiffResult{
		Sessions: make([]DiffEntry, 0, len(sessions)),
	}

	for index, session := range sessions {
		status, err := p.classifySession(ctx, session)
		if err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Sessions = append(result.Sessions, DiffEntry{
			Session: session,
			Status:  status,
		})
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Done: index + 1, Total: len(sessions)})
	}

	return result, ctx.Err()
}

// pipelineCancellation distinguishes cancellation from best-effort lookup errors.
// Dependencies may return a cancellation error before the parent context observes it.
func pipelineCancellation(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// identityMatchesLocation compares the complete adapter-derived project
// identity. An empty remote still carries a meaningful path-derived identity.
func identityMatchesLocation(meta *UnifiedMetadata, loc SessionLocation) bool {
	if meta == nil || string(meta.HostSlug) != loc.HostSlug || (loc.ProjectHash != "" && meta.Project.Hash != loc.ProjectHash) {
		return false
	}
	remote := ""
	if meta.Git.Remote != nil {
		remote = *meta.Git.Remote
	}
	storedRemote := ""
	if loc.GitRemote != nil {
		storedRemote = *loc.GitRemote
	}
	return NormalizeRemoteForMatch(remote) == NormalizeRemoteForMatch(storedRemote)
}

// ClassifyAgainstStore provides a preliminary discovery hint from source time
// and metadata schema version. The ingest pipeline supersedes this hint with
// captured evidence before deciding a supported source is unchanged. Callers
// such as Kickstart may use the hint to avoid redundant discovery work, but it
// is not proof that the source bytes were consumed.
//
// The caller supplies a location whose IngestedMs is set; a session with no
// store record is DiffNew by definition and never reaches here.
func ClassifyAgainstStore(session DiscoveredSession, loc SessionLocation, _ time.Duration) DiffStatus {
	if loc.SchemaVersion > CurrentSchemaVersion {
		return DiffUnchanged
	}
	if loc.IngestedMs != nil && *loc.IngestedMs > 0 {
		// Source modified more recently than DB ingested_ms: re-ingest.
		if session.ModTime.After(time.UnixMilli(*loc.IngestedMs)) {
			return DiffUpdated
		}
	}
	// Schema version behind current (DB value): re-ingest.
	if metadataNeedsNativeRefresh(loc.SchemaVersion) {
		return DiffUpdated
	}
	if loc.PublicationReadiness == PublicationNeedsIngest {
		return DiffUpdated
	}
	// Activity does not make an otherwise unchanged captured source fresh.
	return DiffUnchanged
}

// classifySession provides the discovery-time hint. Selected supported sources
// are classified again with captured bytes before the authoritative no-op.
func (p *Pipeline) classifySession(ctx context.Context, session DiscoveredSession) (DiffStatus, error) {
	return p.classifyCapturedSession(ctx, session, nil)
}

func supportsSessionCapture(session DiscoveredSession) bool {
	return session.TranscriptOrigin != TranscriptOriginFile || (session.SourceFormat == SourceFormatJSONL && session.Harness != HarnessStrike)
}

func (p *Pipeline) classifyCapturedSession(ctx context.Context, session DiscoveredSession, captured *MaterializedTranscript) (DiffStatus, error) {
	if err := ctx.Err(); err != nil {
		return DiffNew, err
	}
	existingMeta, guardErr := p.metadataForRewrite(session)
	if guardErr != nil {
		p.reportMetadataRefusal(string(session.SessionID), guardErr)
		return DiffUnchanged, nil
	}
	isActive := p.config.StalenessThreshold > 0 && time.Since(session.stalenessSourceTime()) < p.config.StalenessThreshold

	// Force always processes a session; activity is diagnostic, not exclusion.
	if p.config.Force {
		if isActive {
			return DiffActive, nil
		}
		return DiffNew, nil
	}
	// Extraction from retained input preserves an unknown acquisition clock.
	// Once its adapter revision is current, a discovered native file still needs acquisition: no
	// previous ingest time proves that its content was already consumed.
	if existingMeta != nil && !session.ModTime.IsZero() && (existingMeta.Timestamp.Ingested == nil || *existingMeta.Timestamp.Ingested <= 0) {
		if isActive {
			return DiffActive, nil
		}
		return DiffUpdated, nil
	}

	// DB-first freshness (v8+): use recorded clocks/schema when available. The
	// compatibility check above still inspects any existing metadata artifact so
	// stale DB mirror state cannot authorize overwriting a future file version.
	if loc, ok := p.locationCache[session.SessionID]; ok && loc.IngestedMs != nil {
		status := ClassifyAgainstStore(session, loc, p.config.StalenessThreshold)
		if captured == nil && status == DiffUpdated && loc.PublicationReadiness == PublicationNeedsIngest {
			// Publication readiness is a repair hint, not change evidence.
			// Before any capture it re-reads native input only for a session
			// that has no retained artifact to hold its input: a legacy row
			// with nothing under the managed tree can be repaired from the
			// native source alone. A session whose retained pair exists keeps
			// its retained-first path: its clock, schema, cursor and captured
			// evidence decide whether native input is read, and the ordinary
			// index write binds publication once a capture exists.
			settled := loc
			settled.PublicationReadiness = PublicationReady
			if ClassifyAgainstStore(session, settled, p.config.StalenessThreshold) == DiffUnchanged {
				if metaPath, err := p.findMetadataPath(ctx, session); err == nil && metaPath != "" {
					status = DiffUnchanged
				}
			}
		}
		if captured != nil && loc.SourceEvidenceSupported {
			status = DiffUnchanged
			if !bytes.Equal(loc.SourceFingerprint, captured.SourceFingerprint) || metadataNeedsNativeRefresh(loc.SchemaVersion) || loc.PublicationReadiness == PublicationNeedsIngest {
				status = DiffUpdated
			}
		}
		// Captured-source evidence decides a supported source, and only a
		// capture produces it. Before the capture (captured == nil) the clock
		// hint cannot prove a source that already holds a fingerprint
		// unchanged: an append can land between the previous capture and its
		// ingest stamp, so that row goes to the worker, whose captured
		// comparison above is the authority. A store that cannot hold the
		// evidence at all cannot prove the source unchanged either, so its
		// supported sources go to the worker as well. A row in an
		// evidence-holding store that has no fingerprint has nothing to
		// compare, so the clock hint stands and the session is left alone: no
		// native read, no stat. The first capture any clock, schema or force
		// reason triggers acquires the evidence. After a capture, a row that
		// still holds no fingerprint records what it read.
		if status == DiffUnchanged && supportsSessionCapture(session) &&
			(captured == nil && (!loc.SourceEvidenceSupported || len(loc.SourceFingerprint) > 0) ||
				captured != nil && loc.SourceEvidenceSupported && len(loc.SourceFingerprint) == 0) {
			if isActive {
				return DiffActive, nil
			}
			return DiffUpdated, nil
		}
		if status == DiffUnchanged && loc.ProjectHash != "" {
			if captured != nil {
				if !identityMatchesLocation(captured.Metadata, loc) {
					return DiffUpdated, nil
				}
			} else if factory, ok := p.adapters[session.Harness]; ok && p.git != nil && !supportsSessionCapture(session) {
				// Supported sources defer identity to the root worker's capture;
				// preliminary discovery must not read their payloads again.
				meta, err := factory(p.fs, p.git, p.salt).ExtractMetadata(ctx, session)
				if err != nil {
					return DiffUnchanged, err
				}
				if !identityMatchesLocation(meta, loc) {
					return DiffUpdated, nil
				}
			}
		}
		// The change cursor is an additional trigger on top of the clock: a session
		// the clock reports unchanged is re-ingested when its newest event sequence
		// moved past the last ingested value, catching an in-place rewrite that
		// moved no time column. It only fires for a session that already has a
		// stored cursor, so a first sighting never mass-re-ingests.
		if status == DiffUnchanged {
			if storedSeq, tracked := p.seqCursorCache[session.SessionID]; tracked && session.EventSeq > storedSeq {
				if isActive {
					return DiffActive, nil
				}
				return DiffUpdated, nil
			}
		}
		return status, nil
	}

	// File fallback: DB has no record for this session (pre-migration data or first run).
	// Read metadata.json from disk for backward compat (preserves pre-v8 behavior).
	metaPath, err := p.findMetadataPath(ctx, session)
	if err != nil {
		return DiffNew, err
	}
	if metaPath == "" {
		// No metadata file found; new session.
		// Still respect staleness for new sessions.
		if isActive {
			return DiffActive, nil
		}
		return DiffNew, nil
	}

	if err := ctx.Err(); err != nil {
		return DiffNew, err
	}
	data, err := p.fs.ReadFile(metaPath)
	if err := pipelineCancellation(ctx, err); err != nil {
		return DiffNew, err
	}
	if err != nil {
		if isActive {
			return DiffActive, nil
		}
		return DiffNew, nil
	}

	// Parse existing metadata to compare.
	var existing UnifiedMetadata
	if err := json.Unmarshal(data, &existing); err != nil {
		// Corrupt metadata: re-ingest.
		if isActive {
			return DiffActive, nil
		}
		return DiffNew, nil
	}
	if captured == nil && supportsSessionCapture(session) {
		// The owned marker beside the metadata is the file-only counterpart
		// of the store's captured fingerprint, and only the worker's capture
		// can compare it. A present marker sends the session to the worker,
		// whose comparison below settles it at the cost of one source read.
		// An absent marker beside an artifact this marker-writing generation
		// produced is interrupted evidence, unknown rather than current, so
		// the worker re-reads the source and writes the marker again. An
		// absent marker beside an older artifact predates the evidence: the
		// clock hint below stands and no native input is read.
		if _, markerErr := p.fs.Stat(fileCaptureEvidencePath(metaPath)); markerErr == nil || existing.SchemaVersion >= CurrentSchemaVersion {
			if isActive {
				return DiffActive, nil
			}
			return DiffUpdated, nil
		}
	}
	if captured != nil {
		// Private evidence is bound to the exact successful metadata file, whose
		// content hash also verifies the managed transcript. Legacy/missing or
		// interrupted evidence refreshes once; ingest audit time is not freshness.
		evidence, readErr := p.fs.ReadFile(fileCaptureEvidencePath(metaPath))
		digest := sha256.Sum256(data)
		want := append(fileCaptureEvidence(captured), digest[:]...)
		transcriptPath := filepath.Join(filepath.Dir(metaPath), fmt.Sprintf("%s--transcript.%s", session.SessionID, session.SourceFormat))
		transcript, transcriptErr := p.fs.ReadFile(transcriptPath)
		if readErr == nil && transcriptErr == nil && bytes.Equal(evidence, want) && existing.SchemaVersion >= CurrentSchemaVersion && existing.ContentHash == schema.ComputeTranscriptHash(transcript) {
			return DiffUnchanged, nil
		}
		return DiffUpdated, nil
	}

	// Source modified more recently than ingest time: re-ingest.
	if existing.Timestamp.Ingested != nil {
		ingestedMs := *existing.Timestamp.Ingested
		if ingestedMs > 0 {
			ingestedTime := time.UnixMilli(ingestedMs)
			if session.ModTime.After(ingestedTime) {
				if isActive {
					return DiffActive, nil
				}
				return DiffUpdated, nil
			}
		}
	}

	// Optional metadata fields do not make retained native evidence stale.
	if metadataNeedsNativeRefresh(existing.SchemaVersion) {
		if isActive {
			return DiffActive, nil
		}
		return DiffUpdated, nil
	}

	// Staleness check last: file may be being written but already ingested.
	if isActive {
		return DiffActive, nil
	}

	return DiffUnchanged, nil
}

// findMetadataPath searches for an existing metadata file for a session.
//
// Output structure is {outputDir}/{hostSlug}/{sessionId}/{sessionId}--metadata.json.
// Since the hostSlug is not known during the diff phase (it requires extraction),
// we walk one level of the outputDir looking for a matching sessionId directory.
// Returns empty string if no metadata file is found.
func (p *Pipeline) findMetadataPath(ctx context.Context, session DiscoveredSession) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
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
		if err := ctx.Err(); err != nil {
			return "", err
		}
		_, err := p.fs.Stat(candidate)
		if cancelErr := pipelineCancellation(ctx, err); cancelErr != nil {
			return "", cancelErr
		}
		if err == nil {
			return candidate, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", managedInputIOError(candidate, err)
		}
		// File not at expected path (e.g. output moved) — fall through to walk.
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	entries, err := p.fs.ReadDir(outputDir)
	if cancelErr := pipelineCancellation(ctx, err); cancelErr != nil {
		return "", cancelErr
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil // outputDir doesn't exist yet
		}
		return "", managedInputIOError(outputDir, err)
	}

	for _, hostEntry := range entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !hostEntry.IsDir() {
			continue
		}
		hostDir := fmt.Sprintf("%s/%s", outputDir, hostEntry.Name())

		// Check flat layout: {hostSlug}/{sessionID}/{metaFilename}
		candidate := fmt.Sprintf("%s/%s/%s", hostDir, session.SessionID, metaFilename)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		_, err := p.fs.Stat(candidate)
		if cancelErr := pipelineCancellation(ctx, err); cancelErr != nil {
			return "", cancelErr
		}
		if err == nil {
			return candidate, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", managedInputIOError(candidate, err)
		}

		// Check nested subagent layout: {hostSlug}/{parentID}/subagents/{sessionID}/{metaFilename}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		sessionEntries, err := p.fs.ReadDir(hostDir)
		if cancelErr := pipelineCancellation(ctx, err); cancelErr != nil {
			return "", cancelErr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", managedInputIOError(hostDir, err)
		}
		for _, sessionEntry := range sessionEntries {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if !sessionEntry.IsDir() {
				continue
			}
			nested := fmt.Sprintf("%s/%s/%s/%s/%s", hostDir, sessionEntry.Name(), defaults.DirSubagents.String(), session.SessionID, metaFilename)
			if err := ctx.Err(); err != nil {
				return "", err
			}
			_, err := p.fs.Stat(nested)
			if cancelErr := pipelineCancellation(ctx, err); cancelErr != nil {
				return "", cancelErr
			}
			if err == nil {
				return nested, nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "", managedInputIOError(nested, err)
			}
		}
	}

	return "", nil // not found
}

// fileCaptureEvidence is private local evidence, not part of wire metadata.
// Include pre-redaction identity so redacted logs also detect upstream repair.
func fileCaptureEvidence(captured *MaterializedTranscript) []byte {
	remote := ""
	if captured.Metadata.Git.Remote != nil {
		remote = NormalizeRemoteForMatch(*captured.Metadata.Git.Remote)
	}
	identity := sha256.Sum256([]byte(fmt.Sprintf("%q/%q/%q", captured.Metadata.HostSlug, captured.Metadata.Project.Hash, remote)))
	return append(append([]byte(nil), captured.SourceFingerprint...), identity[:]...)
}

func fileCaptureEvidencePath(metaPath string) string {
	return strings.TrimSuffix(metaPath, defaults.MetadataSuffix) + fileCaptureEvidenceSuffix
}

// fileCaptureEvidenceSuffix names the owned marker file beside a session's
// metadata; fileCaptureEvidenceName is that file's base name for one session.
const fileCaptureEvidenceSuffix = "--source-capture"

func fileCaptureEvidenceName(sid SessionID) string { return string(sid) + fileCaptureEvidenceSuffix }

// captureSession detaches source bytes and metadata before any managed writes.
// Legacy mutable multi-file formats retain their existing reader limitations.
func (p *Pipeline) captureSession(ctx context.Context, session DiscoveredSession) (*MaterializedTranscript, error) {
	factory, ok := p.adapters[session.Harness]
	if !ok {
		return nil, fmt.Errorf("no adapter for provider %s", session.Harness)
	}
	if err := session.TranscriptOrigin.Validate(); err != nil {
		return nil, err
	}
	adapterFS := p.fs
	var capturedSource *captureFileSystem
	var data []byte
	var piConsumedBytes int
	var err error
	if session.TranscriptOrigin == TranscriptOriginFile {
		if reader, ok := p.fs.(sourcePrefixReader); ok && session.SourceFormat == SourceFormatJSONL {
			data, err = reader.ReadSourcePrefix(session.SourcePath.String())
		} else {
			data, err = p.fs.ReadFile(session.SourcePath.String())
		}
		if err == nil && session.SourceFormat == SourceFormatJSONL && session.Harness != HarnessStrike {
			if session.Harness == HarnessPi {
				// Classify the original capture before any lossy prefix removal.
				// The adapter also receives these bytes so semantic validation and
				// incomplete-tail diagnostics describe the acquired view.
				var doc piDocument
				doc, err = parsePiDocument(ctx, data)
				piConsumedBytes = doc.consumedBytes
			} else {
				data, err = completeJSONLPrefix(data)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("capture transcript source for %s: %w; prior stored state remains unchanged; restore source readability and retry", session.SessionID, err)
		}
		if session.Harness != HarnessOpenCode {
			adapterFS = capturedSourceFileSystem{FileSystem: p.fs, path: session.SourcePath.String(), data: data}
		}
	}
	adapter := factory(adapterFS, p.git, p.salt)
	if session.Harness == HarnessOpenCode && session.TranscriptOrigin == TranscriptOriginFile {
		// Keep the factory's real SQLite opener, but detach JSON metadata and
		// message/part reads for the queued directory indexer.
		capturedSource = newCaptureFileSystem(p.fs)
		capturedSource.files[session.SourcePath.String()] = captureFile{data: data}
		if openCode, ok := adapter.(*OpenCodeAdapter); ok {
			openCode.fs = capturedSource
		}
	}
	if session.TranscriptOrigin != TranscriptOriginFile {
		if acquirer, ok := adapter.(CursorTranscriptMaterializer); ok {
			// The cursor materialization checks the consumed row against
			// discovery and acquires the cursor from the same read snapshot
			// as the transcript, so the acquisition evidence describes what
			// was read. Its diagnostics are the run's: an unavailable cursor
			// is reported once and stays unknown, never an acquired zero.
			acquired, err := acquirer.MaterializeTranscriptWithCursor(ctx, session)
			if err != nil {
				return nil, err
			}
			for _, diagnostic := range acquired.Diagnostics {
				p.reportDiagnostic(diagnostic)
			}
			captured := MaterializedTranscript{Metadata: acquired.Metadata, Data: acquired.Transcript, SourceFingerprint: acquired.SourceFingerprint, Session: acquired.Session}
			if acquired.EventSeq != nil {
				captured.EventSeq, captured.EventSeqObserved = *acquired.EventSeq, true
			}
			return &captured, nil
		}
		materializer, ok := adapter.(TranscriptMaterializer)
		if !ok {
			return nil, fmt.Errorf("materialize session %s: adapter lacks managed source support; no state written; use the production OpenCode adapter", session.SessionID)
		}
		captured, err := materializer.MaterializeTranscript(ctx, session)
		if err != nil {
			return nil, err
		}
		return &captured, nil
	}
	meta, err := adapter.ExtractMetadata(ctx, session)
	if err != nil {
		return nil, err
	}
	if session.Harness == HarnessPi {
		data = data[:piConsumedBytes]
	}
	captured := newMaterializedTranscript(meta, data, session.EventSeq)
	captured.capturedSource = capturedSource
	return &captured, nil
}

// processSession atomically writes the accepted capture and carries its bytes
// and evidence into the existing store and streamed indexing path.
func (p *Pipeline) processNativeSession(ctx context.Context, entry DiffEntry) workerResult {
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
	// Prefetch is only a discovery optimization. A missing or stale cache must
	// never authorize overwriting a newer stored schema, even under --force.
	if err := p.checkStoredRewriteVersion(ctx, session.SessionID, session.Harness); err != nil {
		p.reportMetadataRefusal(string(session.SessionID), err)
		result.Status = DiffUnchanged
		return workerResult{result: result}
	}
	if _, err := p.metadataForRewrite(session); err != nil {
		p.reportMetadataRefusal(string(session.SessionID), err)
		result.Status = DiffUnchanged
		return workerResult{result: result}
	}
	publisher, publishErr := p.artifactPublisher(nil)
	if publishErr != nil {
		return fail(publishErr)
	}
	metadataPath, pathErr := p.findMetadataPath(ctx, session)
	if pathErr != nil {
		return fail(pathErr)
	}
	observation, observeErr := publisher.Observe(ctx, session, metadataPath)
	if observeErr != nil {
		return fail(observeErr)
	}

	captured, err := p.captureSession(ctx, session)
	if err != nil {
		return fail(&adapterAcquisitionError{cause: err})
	}
	if captured.Session != nil {
		session = *captured.Session
		result.ParentUUID = session.ParentUUID
	}
	if supportsSessionCapture(session) && !p.config.Reindex {
		result.Status, err = p.classifyCapturedSession(ctx, session, captured)
		if err != nil {
			return fail(err)
		}
		if result.Status == DiffUnchanged {
			// Drain commits the parent and acknowledges the empty arena slot,
			// without inserting, rewriting metadata, or scheduling indexing.
			return workerResult{result: result}
		}
	}
	rawData, meta := captured.Data, captured.Metadata
	capturedSource := captured.capturedSource
	if meta.SessionID != session.SessionID {
		return fail(fmt.Errorf("capture session %s: source metadata identifies a different session; nothing was captured; restore the matching source and run peasant ingest", session.SessionID))
	}
	for _, diagnostic := range meta.Diagnostics.Warnings {
		if diagnostic.ErrorType == "sidecar_session_mismatch" {
			return fail(fmt.Errorf("capture session %s: Strike source sidecar identifies a different session; nothing was captured; correct the source identity and rerun peasant ingest", session.SessionID))
		}
	}
	var captureEvidence []byte
	if p.store == nil && supportsSessionCapture(session) {
		captureEvidence = fileCaptureEvidence(captured)
	}
	session.EventSeq = captured.EventSeq
	// A cursor is acquired evidence only when the materialization observed
	// one. An unobserved cursor stays nil, and nil preserves the stored value:
	// an unknown cursor never becomes an acquired zero.
	var acquiredEventSeq *int64
	if session.Harness == HarnessOpenCode && captured.EventSeqObserved {
		acquiredEventSeq = &captured.EventSeq
	}
	meta.ParentUUID = session.ParentUUID
	meta.Source.Format = session.SourceFormat
	adapterVersion := p.versionTargets()[session.Harness].AdapterVersion
	meta.AdapterVersion = &adapterVersion
	meta.DerivedAt = nil

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

	sourceFingerprint := captured.SourceFingerprint
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
	commitCaptureComplete := false
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
			// When no user email is configured, the detector returns the window
			// unfiltered by author and records a missing_user_email diagnostic.
			// Git failures become diagnostics inside CommitDetector.LayeredDetection.
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
			commitCaptureComplete = len(diags) == 0
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
	// DerivedAt stays absent when the complete file pair commits. The drain loop
	// adds it only after a successful transactional mirror of this same artifact.
	meta.MetadataHash = schema.ComputeMetadataHash(meta)

	metaJSON, marshalErr := json.Marshal(meta)
	if marshalErr != nil {
		return fail(errors.Join(marshalErr, p.fs.RemoveAll(tmpDir)))
	}
	artifact, captureErr := NewManagedArtifact(metaJSON, writeData)
	if captureErr != nil {
		return fail(errors.Join(captureErr, p.fs.RemoveAll(tmpDir)))
	}
	debugFiles := make(map[string][]byte)
	debugDir := filepath.Join(tmpDir, defaults.DirDebug.String())
	debugEntries, debugErr := p.fs.ReadDir(debugDir)
	if debugErr != nil && !errors.Is(debugErr, fs.ErrNotExist) {
		return fail(errors.Join(debugErr, p.fs.RemoveAll(tmpDir)))
	}
	for _, entry := range debugEntries {
		if entry.IsDir() {
			return fail(errors.Join(fmt.Errorf("prepare debug publication for session %s: unexpected nested directory", session.SessionID), p.fs.RemoveAll(tmpDir)))
		}
		data, readErr := p.fs.ReadFile(filepath.Join(debugDir, entry.Name()))
		if readErr != nil {
			return fail(errors.Join(readErr, p.fs.RemoveAll(tmpDir)))
		}
		debugFiles[entry.Name()] = data
	}
	publication := ArtifactPublication{Artifact: artifact, Observation: observation, DebugFiles: debugFiles, EventSeq: acquiredEventSeq, CWDProvenance: publicationCWDProvenance(meta, session), SourceFingerprint: sourceFingerprint, CommitCaptureComplete: commitCaptureComplete, SourceEvidence: captureEvidence}
	if session.Origin != "" {
		origin := session.Origin
		publication.Origin = &origin
	}
	// Event cursor evidence is supplied only by a materialization path that
	// proves acquisition; discovery's optional clock hint is not a success stamp.
	committed, commitErr := publisher.Publish(ctx, publication)
	cleanupErr := p.fs.RemoveAll(tmpDir)
	if commitErr != nil {
		return fail(errors.Join(commitErr, cleanupErr))
	}
	if cleanupErr != nil {
		p.reportDiagnostic(DiagnosticEntry{ErrorType: "artifact_cleanup", Location: tmpDir, Message: cleanupErr.Error(), Remediation: "Inspect the retained temporary extraction directory; the complete committed artifact was preserved."})
	}
	meta = &committed.Metadata

	result.OutputPath = sessionDir

	var startMs int64
	if meta.Timestamp.Start > 0 {
		startMs = meta.Timestamp.Start
	}
	outputTranscriptPath := fmt.Sprintf("%s/%s--transcript.%s", sessionDir, session.SessionID, ext)
	if session.Harness != HarnessOpenCode || session.TranscriptOrigin != TranscriptOriginFile {
		// File indexers already receive the exact managed bytes via the arena.
		// Only OpenCode's directory indexer needs the detached extraction tree.
		capturedSource = nil
	}

	return workerResult{
		commitCaptureComplete: commitCaptureComplete,
		result:                result,
		capturedSource:        capturedSource,
		cwdProvenance:         publicationCWDProvenance(meta, session),
		eventSeq:              session.EventSeq,
		meta:                  meta,
		sourceFingerprint:     sourceFingerprint,
		fileCaptureEvidence:   captureEvidence,
		transcriptData:        transcriptData,
		outputTranscriptPath:  outputTranscriptPath,
		artifact:              committed,
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

// replaceSessionDir replaces parent-owned output while preserving the child-owned
// subagents subtree. Files are installed with same-filesystem atomic Rename, not
// a copy/delete move. There is intentionally no directory swap or rollback: an
// interruption can leave a mixture of complete old/new parent files for normal
// ingest to refresh, but never displaces children into disposable staging. The
// caller may always discard src. Root-owns-subtree scheduling excludes writers.
func (p *Pipeline) replaceSessionDir(src, dst, sessionID string) error {
	wanted := make(map[string]bool)
	err := p.fs.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == defaults.DirSubagents.String() {
			return fmt.Errorf("staged parent output unexpectedly contains child directory %s", path)
		}
		wanted[rel] = true
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return p.fs.MkdirAll(target, defaults.PrivateDirPerm)
		}
		return p.fs.Rename(path, target)
	})
	if err != nil {
		return fmt.Errorf("install parent files at %s: %w; existing child output remains in place; fix filesystem access or free disk space and rerun ingest", dst, err)
	}

	// Only prune obsolete parent-owned files after every new file is installed.
	// Skip the entire child-owned tree, including children excluded by FILTER.
	var stale []string
	err = p.fs.WalkDir(dst, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		if rel == defaults.DirSubagents.String() {
			return fs.SkipDir
		}
		if rel != "." && !strings.HasPrefix(rel, sessionID+"--") && rel != defaults.DirDebug.String() && !strings.HasPrefix(rel, defaults.DirDebug.String()+"/") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !wanted[rel] {
			stale = append(stale, path)
			if entry.IsDir() {
				return fs.SkipDir
			}
		}
		return nil
	})
	if err == nil {
		for _, path := range stale {
			if err = p.fs.RemoveAll(path); err != nil {
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("prune obsolete parent files at %s: %w; installed files and child output remain in place; fix filesystem access and rerun ingest", dst, err)
	}
	return p.fs.RemoveAll(src)
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
) (indexformat.Result, error) {
	readFile := func() (indexformat.Result, error) {
		if strict, ok := indexer.(AuthoritativeTranscriptIndexer); ok {
			capture, err := strict.IndexTranscriptForCapture(ctx, session)
			return indexformat.V1{Entries: capture.Entries}, err
		}
		if versioned, ok := indexer.(VersionedTranscriptIndexer); ok {
			return versioned.IndexTranscriptResult(ctx, session)
		}
		entries, err := indexer.IndexTranscript(ctx, session)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, &unverifiedEmptyIndexError{session: session}
		}
		return indexformat.V1{Entries: entries}, nil
	}
	readBytes := func() (indexformat.Result, error) {
		if strict, ok := indexer.(AuthoritativeTranscriptIndexer); ok {
			capture, err := strict.IndexTranscriptBytesForCapture(ctx, session, transcriptData)
			return indexformat.V1{Entries: capture.Entries}, err
		}
		if versioned, ok := indexer.(VersionedTranscriptIndexer); ok {
			return versioned.IndexTranscriptBytesResult(ctx, session, transcriptData)
		}
		entries, err := indexer.IndexTranscriptBytes(ctx, session, transcriptData)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, &unverifiedEmptyIndexError{session: session}
		}
		return indexformat.V1{Entries: entries}, nil
	}
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
					"Fix: enable %s in your configuration (sources.%s.enabled: true) and re-run, so discovery resolves its storage root. If you are pointing at sessions with --source-harness and --source-path, name %s and give the path to its storage directory rather than to a single file.",
				session.SessionID, session.Harness, session.Harness, session.Harness, session.Harness, session.Harness)
		}
		return readFile()
	case TranscriptSourceFile:
		if transcriptData != nil {
			return readBytes()
		}
		return readFile()
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
		ContentOmitted: captureContentOmitted(wr.meta),
		EventSeq:       wr.eventSeq,
		SessionID:      wr.result.SessionID,
		Harness:        wr.result.Harness,
		ParentUUID:     parentUUID,
		SourceFormat:   sourceFormat,
		SourcePath:     sourcePath,
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

// moveSessionFiles leaves children at their stored locations until they are
// individually repaired. Only named session artifacts and its debug directory
// belong to this session; unrelated destination members are never replaced.
func (p *Pipeline) moveSessionFiles(src, dst, sessionID string) error {
	if err := p.fs.MkdirAll(dst, defaults.PrivateDirPerm); err != nil {
		return err
	}
	var paths []string
	if err := p.fs.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == src {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != defaults.DirDebug.String() && !strings.HasPrefix(rel, defaults.DirDebug.String()+"/") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(rel, sessionID+"--") || strings.HasPrefix(rel, defaults.DirDebug.String()+"/") {
			paths = append(paths, rel)
		}
		return nil
	}); err != nil {
		return err
	}
	// Check all collisions before moving any files.
	for _, rel := range paths {
		if _, err := p.fs.Stat(filepath.Join(dst, rel)); err == nil {
			oldData, err := p.fs.ReadFile(filepath.Join(src, rel))
			if err != nil {
				return err
			}
			newData, err := p.fs.ReadFile(filepath.Join(dst, rel))
			if err != nil {
				return err
			}
			if !bytes.Equal(oldData, newData) {
				return fmt.Errorf("repair artifacts: destination file %s conflicts with stored session %s; prior files retained; reconcile the duplicate file and retry ingest", filepath.Join(dst, rel), sessionID)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, rel := range paths {
		target := filepath.Join(dst, rel)
		if err := p.fs.MkdirAll(filepath.Dir(target), defaults.PrivateDirPerm); err != nil {
			return err
		}
		if err := p.fs.Rename(filepath.Join(src, rel), target); err != nil {
			return err
		}
	}
	return nil
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

func (p *Pipeline) runStreamedDownstream(ctx context.Context, indexedCh <-chan indexedMeta, prog *ProgressState, total int, logPrefix string, writeLane *storeWriteLane) (result streamedDownstreamResult) {
	result.Days = make(map[string]bool)
	if p.hasStoredMetricRefresh() {
		// Persistent maintenance runs once over stored pages after all index
		// writers finish, avoiding a second capture of newly indexed sessions.
		for im := range indexedCh {
			if im.startMs > 0 {
				result.Days[time.UnixMilli(im.startMs).UTC().Format("2006-01-02")] = true
			}
		}
		return result
	}
	var computeDuration time.Duration
	var annotateDuration time.Duration
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageCompute, Total: total})
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageAnnotate, Total: total})
	defer func() {
		result.ComputeDuration = computeDuration
		result.AnnotateDuration = annotateDuration
	}()

	profiled, useProfile := p.classifier.(ProfiledSessionClassifier)
	buffered, useBuffered := p.classifier.(BufferedSessionClassifier)
	var bufferedPending []SessionAnnotationBatch
	bufferedPendingWrites := 0
	var flushTimer *time.Timer
	var flushTimerC <-chan time.Time
	stopFlushTimer := func() {
		if flushTimer == nil {
			return
		}
		if !flushTimer.Stop() {
			select {
			case <-flushTimer.C:
			default:
			}
		}
		flushTimer = nil
		flushTimerC = nil
	}
	armFlushTimer := func() {
		if !useBuffered || len(bufferedPending) == 0 || flushTimer != nil {
			return
		}
		flushTimer = time.NewTimer(annotationFlushInterval)
		flushTimerC = flushTimer.C
	}
	recordAnnotationResult := func(batchResult SessionAnnotationBatchResult) {
		if batchResult.Err != nil {
			slog.Warn(logPrefix+": annotate indexed session",
				"session_id", batchResult.SessionID,
				"error", batchResult.Err,
				"what", "failed to annotate a session after its metrics step finished",
				"why", "the classifier or annotation store returned an error for this session",
				"user_impact", "this session may lack quality annotations in the web UI until ingest is run again",
				"how_to_fix", "re-run peasant harvest index --all; if the error repeats, inspect the named session and database")
		}
		result.AnnotateDone++
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageAnnotate, Done: result.AnnotateDone, Total: total})
	}
	flushBuffered := func() {
		if len(bufferedPending) == 0 {
			return
		}
		stopFlushTimer()
		annotateStarted := time.Now()
		var flushed []SessionAnnotationBatchResult
		p.runStoreWrite(writeLane, func() {
			flushed = buffered.FlushAnnotationBatches(ctx, bufferedPending, p.config.IndexProfiler)
		})
		annotateDuration += time.Since(annotateStarted)
		if len(flushed) > len(bufferedPending) {
			flushed = flushed[:len(bufferedPending)]
		}
		for _, batchResult := range flushed {
			recordAnnotationResult(batchResult)
		}
		if len(flushed) < len(bufferedPending) {
			for _, batch := range bufferedPending[len(flushed):] {
				recordAnnotationResult(SessionAnnotationBatchResult{
					SessionID: batch.SessionID,
					Err:       fmt.Errorf("%s: annotation batch flush returned %d result(s) for %d session batch(es)", logPrefix, len(flushed), len(bufferedPending)),
				})
			}
		}
		bufferedPending = bufferedPending[:0]
		bufferedPendingWrites = 0
	}
	defer stopFlushTimer()
	processBatch := func(batch []indexedMeta) bool {
		if len(batch) == 0 || ctx.Err() != nil {
			return false
		}
		ids := make([]SessionID, 0, len(batch))
		for _, im := range batch {
			ids = append(ids, im.session.SessionID)
			if im.startMs > 0 {
				day := time.Unix(im.startMs/1000, 0).UTC().Format("2006-01-02")
				result.Days[day] = true
			}
		}
		if p.analyzer != nil {
			computeStarted := time.Now()
			var n int
			var err error
			p.runStoreWrite(writeLane, func() {
				n, ids, err = p.computeReadyMetrics(ctx, ids)
			})
			computeDuration += time.Since(computeStarted)
			if err != nil {
				slog.Warn(logPrefix+": compute metrics for indexed sessions",
					"session_count", len(ids),
					"error", err,
					"what", "failed to compute metrics after indexed sessions became ready",
					"why", "the metrics engine or metrics store returned an error for this session batch",
					"user_impact", "these sessions can be indexed but may not show fresh metrics or quality annotations until ingest is run again",
					"how_to_fix", "re-run peasant harvest index --all; if the error repeats, inspect the named session and database")
			}
			result.Computed += n
		}
		result.ComputeDone += len(batch)
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageCompute, Done: result.ComputeDone, Total: total})

		if p.classifier != nil {
			if ctx.Err() != nil {
				return false
			}
			if useBuffered {
				profiler := p.config.IndexProfiler
				for _, sid := range ids {
					if ctx.Err() != nil {
						return false
					}
					annotateStarted := time.Now()
					batch, err := buffered.PrepareAnnotations(ctx, sid, profiler)
					annotateDuration += time.Since(annotateStarted)
					if err != nil {
						recordAnnotationResult(SessionAnnotationBatchResult{SessionID: sid, Err: err})
						continue
					}
					if batch.SessionID == "" {
						batch.SessionID = sid
					}
					if batch.Skipped || (len(batch.Writes) == 0 && batch.RunState == nil) {
						recordAnnotationResult(SessionAnnotationBatchResult{SessionID: batch.SessionID})
						continue
					}
					bufferedPending = append(bufferedPending, batch)
					bufferedPendingWrites += len(batch.Writes)
					if len(bufferedPending) >= annotationFlushSessionLimit || bufferedPendingWrites >= annotationFlushWriteLimit {
						flushBuffered()
					} else {
						armFlushTimer()
					}
				}
				return true
			}
			for _, sid := range ids {
				if ctx.Err() != nil {
					return false
				}
				annotateStarted := time.Now()
				var err error
				p.runStoreWrite(writeLane, func() {
					if useProfile && p.config.IndexProfiler != nil {
						err = profiled.AnnotateWithProfile(ctx, sid, p.config.IndexProfiler)
					} else {
						err = p.classifier.Annotate(ctx, sid)
					}
				})
				annotateDuration += time.Since(annotateStarted)
				if err != nil {
					slog.Warn(logPrefix+": annotate indexed session",
						"session_id", sid,
						"error", err,
						"what", "failed to annotate a session after its metrics step finished",
						"why", "the classifier or annotation store returned an error for this session",
						"user_impact", "this session may lack quality annotations in the web UI until ingest is run again",
						"how_to_fix", "re-run peasant harvest index --all; if the error repeats, inspect the named session and database")
				}
				result.AnnotateDone++
				emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageAnnotate, Done: result.AnnotateDone, Total: total})
			}
		}
		return true
	}

	pending := make([]indexedMeta, 0, indexWriteBatchLimit)

receiveLoop:
	for {
		var im indexedMeta
		select {
		case next, ok := <-indexedCh:
			if !ok {
				break receiveLoop
			}
			im = next
		case <-flushTimerC:
			flushTimer = nil
			flushTimerC = nil
			flushBuffered()
			continue
		}
		pending = append(pending, im)
	drainReady:
		for len(pending) < indexWriteBatchLimit {
			select {
			case next, ok := <-indexedCh:
				if !ok {
					break drainReady
				}
				pending = append(pending, next)
			default:
				break drainReady
			}
		}
		if !processBatch(pending) {
			return
		}
		pending = pending[:0]
	}
	flushBuffered()
	return
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
	priorDownstream *streamedDownstreamResult,
) (*PipelineResult, error) {
	prog := p.config.Progress

	// INDEX session_entries (best-effort, non-fatal).
	// successfullyIndexed tracks successfully stored index results. Persistent
	// downstream maintenance also checks sessions from earlier invocations.
	indexed := 0
	successfullyIndexed := make([]SessionID, 0, len(priorIndexed)+len(indexSessions))
	remainingSuccessfullyIndexed := make([]SessionID, 0, len(indexSessions))
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
		indexProfileStart := time.Now()
		batchIndexed, batchLogs := p.indexBatch(ctx, indexSessions, outcome, logPrefix)
		for i, result := range batchIndexed {
			if result.indexed {
				indexed++
				successfullyIndexed = append(successfullyIndexed, result.session.SessionID)
				remainingSuccessfullyIndexed = append(remainingSuccessfullyIndexed, result.session.SessionID)
			}
			if batchLogs[i].SessionID != "" {
				indexLogEntries = append(indexLogEntries, batchLogs[i])
			}
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageIndex, Done: priorDone + i + 1, Total: priorDone + len(indexSessions)})
		}
		p.recordIndexProfileStage(StageIndex, indexProfileStart, len(batchIndexed), len(indexSessions))
	}
	// A retained-content repair counts as indexed work once per session; a
	// session that was also indexed by the ordinary path is not counted twice.
	if len(p.contentRecoveries) > 0 {
		counted := make(map[SessionID]bool, len(successfullyIndexed))
		for _, sid := range successfullyIndexed {
			counted[sid] = true
		}
		for sid := range p.contentRecoveries {
			if !counted[sid] {
				indexed++
			}
		}
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageIndex, Done: indexed, Total: len(priorIndexed) + len(indexSessions)})

	// Persist index_log entries (best-effort).
	indexLogProfileStart := time.Now()
	if p.indexLogger != nil {
		for _, entry := range indexLogEntries {
			if err := p.indexLogger.LogIndexEntry(ctx, entry); err != nil {
				slog.Warn(logPrefix+": log index entry", "session_id", entry.SessionID, "error", err)
			}
		}
	}
	p.recordIndexProfileStage(StageIndexLog, indexLogProfileStart, len(indexLogEntries), len(indexLogEntries))

	// COMPUTE metrics + insights (best-effort, non-fatal).
	// Persistent stores inspect bounded pages even when no session needed indexing.
	computed := 0
	computeAlreadyDone := 0
	annotateAlreadyDone := 0
	streamedComputeDuration := time.Duration(0)
	streamedAnnotateDuration := time.Duration(0)
	streamedDaySet := make(map[string]bool)
	if priorDownstream != nil {
		computed = priorDownstream.Computed
		computeAlreadyDone = priorDownstream.ComputeDone
		annotateAlreadyDone = priorDownstream.AnnotateDone
		streamedComputeDuration = priorDownstream.ComputeDuration
		streamedAnnotateDuration = priorDownstream.AnnotateDuration
		for day := range priorDownstream.Days {
			streamedDaySet[day] = true
		}
	}
	computeProfileStart := time.Now()
	storedRefresh := p.hasStoredMetricRefresh()
	var storedChecked, storedAnnotated int
	var refreshedDays map[string]bool
	var readyForAnnotations []SessionID
	if priorDownstream == nil {
		emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageCompute, Total: len(successfullyIndexed)})
	} else if len(indexSessions) > 0 {
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageCompute, Done: computeAlreadyDone, Total: computeAlreadyDone + len(indexSessions)})
	}
	if storedRefresh {
		var n int
		n, storedChecked, storedAnnotated, refreshedDays = p.refreshStoredMetrics(ctx)
		computed += n
	}
	if p.analyzer != nil && (len(indexSessions) > 0 || len(priorIndexed) > 0 || len(refreshedDays) > 0) {
		computeTargets := successfullyIndexed
		if priorDownstream != nil {
			computeTargets = remainingSuccessfullyIndexed
		}
		if !storedRefresh && len(computeTargets) > 0 {
			n, ready, err := p.computeReadyMetrics(ctx, computeTargets)
			readyForAnnotations = ready
			if err != nil {
				slog.Warn(logPrefix+": compute metrics", "error", err)
			}
			computed += n
		}

		// Compute insights (daily summaries) for affected days.
		// Derive days from both drain-loop indexed and stale-session indexed metas
		// so this works even when p.store is nil (e.g. WithIndexers+WithAnalyzer only).
		daySet := make(map[string]bool)
		for day := range refreshedDays {
			daySet[day] = true
		}
		for _, im := range priorIndexed {
			if priorDownstream != nil && im.indexed {
				continue
			}
			if im.startMs > 0 {
				day := time.Unix(im.startMs/1000, 0).UTC().Format("2006-01-02")
				daySet[day] = true
			}
		}
		for day := range streamedDaySet {
			daySet[day] = true
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
	computeDoneTotal := len(successfullyIndexed)
	if priorDownstream != nil {
		computeDoneTotal = computeAlreadyDone + len(indexSessions)
	}
	if storedRefresh {
		computeDoneTotal = storedChecked
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageCompute, Done: computeDoneTotal, Total: computeDoneTotal})
	p.config.IndexProfiler.RecordStage(StageCompute, streamedComputeDuration+time.Since(computeProfileStart), computeDoneTotal, computeDoneTotal)

	// ANNOTATE sessions (best-effort, non-fatal).
	annotateProfileStart := time.Now()
	annotateDoneTotal := annotateAlreadyDone
	annotateTotal := len(successfullyIndexed)
	if priorDownstream != nil {
		annotateTotal = annotateAlreadyDone + len(indexSessions)
	}
	if priorDownstream == nil {
		emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageAnnotate, Total: len(successfullyIndexed)})
	} else if annotateTotal > annotateAlreadyDone {
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageAnnotate, Done: annotateAlreadyDone, Total: annotateTotal})
	}
	if !storedRefresh && p.classifier != nil && len(successfullyIndexed) > 0 {
		annotateTargets := successfullyIndexed
		if priorDownstream != nil {
			annotateTargets = remainingSuccessfullyIndexed
		}
		if p.analyzer != nil {
			annotateTargets = readyForAnnotations
		}
		if len(annotateTargets) > 0 {
			annotateProg := prog
			if priorDownstream != nil {
				annotateProg = nil
			}
			if err := p.stageAnnotate(ctx, annotateTargets, annotateProg); err != nil {
				slog.Warn(logPrefix+": annotate sessions", "error", err)
			}
			if priorDownstream != nil {
				for range annotateTargets {
					annotateDoneTotal++
					emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageAnnotate, Done: annotateDoneTotal, Total: annotateTotal})
				}
			}
		}
	}
	if priorDownstream == nil {
		annotateDoneTotal = len(successfullyIndexed)
	} else if p.classifier == nil {
		annotateDoneTotal = annotateTotal
	}
	if storedRefresh {
		annotateDoneTotal = storedAnnotated
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageAnnotate, Done: annotateDoneTotal, Total: annotateDoneTotal})
	p.config.IndexProfiler.RecordStage(StageAnnotate, streamedAnnotateDuration+time.Since(annotateProfileStart), annotateDoneTotal, annotateDoneTotal)

	// CLEANUP orphan .tmp-* directories.
	cleanupProfileStart := time.Now()
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
	p.recordIndexProfileStage(StageCleanup, cleanupProfileStart, 1, 1)

	// REPORT.
	reportProfileStart := time.Now()
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
	pipelineResult.Summary.HarvesterVersions = maps.Clone(p.versionTargets())
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
	p.recordIndexProfileStage(StageReport, reportProfileStart, len(pipelineResult.Sessions), len(pipelineResult.Sessions))

	// AUDIT — write ingest_log entry (best-effort, non-fatal).
	auditProfileStart := time.Now()
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
	p.recordIndexProfileStage(StageAudit, auditProfileStart, 1, 1)

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
	var indexVersion *int
	if declared := p.versionTargets()[im.session.Harness].IndexVersion; declared > 0 {
		indexVersion = &declared
	}
	return IndexLogEntry{
		SessionID:      im.session.SessionID,
		Harness:        im.session.Harness,
		Outcome:        outcome,
		IndexerVersion: p.versionTargets()[im.session.Harness].IndexerVersion,
		IndexVersion:   indexVersion,
		EntriesCount:   entriesCount,
		SourcePath:     sourcePath,
		OriginalRoot:   originalRoot,
		Reason:         reason,
		StartedAt:      startMs,
		FinishedAt:     &finishedAt,
		ErrorMessage:   errMsg,
	}
}

// sessionMetadataResult holds the parsed output of readSessionMetadata.
type sessionMetadataResult struct {
	session            DiscoveredSession
	startMs            int64
	transcriptPath     string
	originalSourcePath string // Source.FilePath from metadata (may be empty)
	refreshMetadata    bool   // Historical schema requires native evidence.
}

// readSessionMetadata reads and parses a metadata JSON file for a session in
// the given host directory, reconstructing a DiscoveredSession. Missing metadata
// or transcript returns nil. Incompatible versions and non-missing I/O failures
// return errors; corrupt historic content can use validated retained recovery.
//
// The logPrefix parameter is used for structured log messages on parse errors.
func (p *Pipeline) readSessionMetadata(hostDir string, sid SessionID, logPrefix string) (*sessionMetadataResult, error) {
	metaFilename := fmt.Sprintf("%s%s", sid, defaults.MetadataSuffix)
	metaPath := fmt.Sprintf("%s/%s/%s", hostDir, sid, metaFilename)
	data, err := p.fs.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		readErr := managedInputIOError(p.managedRelativePath(metaPath), err)
		p.reportMetadataRefusal(string(sid), readErr)
		slog.Warn(logPrefix+": managed metadata unreadable", "session_id", sid, "error", readErr)
		return nil, readErr
	}

	// Name the file the way every other reporter names it: relative to the
	// managed output directory. Diagnostics collapse by whole-value equality,
	// so an absolute spelling here is the one difference that turns a single
	// refusal, refused again by the index selection, into two warnings.
	meta, err := decodeManagedMetadata(data, p.managedRelativePath(metaPath))
	if err != nil {
		if isMetadataCompatibilityError(err) {
			p.reportMetadataRefusal(string(sid), err)
			slog.Warn(logPrefix+": incompatible metadata", "session_id", sid, "error", err)
			return nil, err
		}
		// Historic corrupt content can recover through the validated managed
		// envelope and compatible DB state. It is not a compatibility refusal.
		slog.Warn(logPrefix+": corrupt metadata; attempting retained recovery", "session_id", sid, "error", err)
		return nil, nil
	}

	// Determine SourceFormat from metadata.
	sourceFormat := meta.Source.Format
	if sourceFormat == "" {
		return nil, nil
	}

	// Build transcript path from output dir.
	transcriptPath := fmt.Sprintf("%s/%s/%s--transcript.%s",
		hostDir, sid, sid, string(sourceFormat))

	// Verify transcript exists.
	if _, err := p.fs.Stat(transcriptPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		readErr := managedInputIOError(transcriptPath, err)
		p.reportMetadataRefusal(string(sid), readErr)
		slog.Warn(logPrefix+": managed transcript unreadable", "session_id", sid, "error", readErr)
		return nil, readErr
	}

	// Look up OriginalRoot from config source paths for this provider.
	var originalRoot ResolvedPath
	if cfg, ok := p.config.Sources[Harness(meta.ModelHarness)]; ok && len(cfg.Paths) > 0 {
		originalRoot = cfg.Paths[0]
	}

	ds := DiscoveredSession{
		ContentOmitted: captureContentOmitted(meta),
		SessionID:      sid,
		Harness:        Harness(meta.ModelHarness),
		SourcePath:     ResolvedPath(transcriptPath),
		SourceFormat:   sourceFormat,
		OriginalRoot:   originalRoot,
	}
	if ds.Harness == HarnessOpenCode && sourceFormat == SourceFormatJSON && !p.config.DryRun {
		transcriptData, readErr := p.fs.ReadFile(transcriptPath)
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				return nil, nil
			}
			readErr = managedInputIOError(transcriptPath, readErr)
			p.reportMetadataRefusal(string(sid), readErr)
			slog.Warn(logPrefix+": managed transcript unreadable", "session_id", sid, "error", readErr)
			return nil, readErr
		}
		origin, recognitionErr := recognizeManagedOpenCodeProjection(transcriptData, sid)
		if recognitionErr != nil {
			p.reportMetadataRefusal(string(sid), recognitionErr)
			slog.Warn(logPrefix+": managed OpenCode projection is corrupt", "session_id", sid, "transcript_path", transcriptPath, "error", recognitionErr, "impact", "recovery stopped before legacy fallback so existing index state is not replaced with an empty corpus", "fix", "re-run harvest to regenerate the managed transcript")
			return nil, recognitionErr
		}
		ds.TranscriptOrigin = origin
	}
	if meta.ParentUUID != nil {
		ds.ParentUUID = meta.ParentUUID
	}

	refreshMetadata := metadataNeedsNativeRefresh(meta.SchemaVersion)
	if refreshMetadata && meta.AdapterVersion != nil {
		target := p.versionTargets()[meta.ModelHarness].AdapterVersion
		if *meta.AdapterVersion > target {
			// The same file, spelled the way every other reporter spells it, so
			// this refusal collapses with the same refusal from another path.
			adapterErr := &AdapterVersionError{Path: p.managedRelativePath(metaPath), Version: *meta.AdapterVersion, Target: target}
			p.reportMetadataRefusal(string(sid), adapterErr)
			slog.Warn(logPrefix+": retaining newer adapter output", "session_id", sid, "error", adapterErr)
			refreshMetadata = false
		}
	}
	return &sessionMetadataResult{
		session:            ds,
		startMs:            meta.Timestamp.Start,
		transcriptPath:     transcriptPath,
		originalSourcePath: meta.Source.FilePath,
		refreshMetadata:    refreshMetadata,
	}, nil
}

// reconstructFromMetadata attempts to reconstruct a DiscoveredSession from
// peasant-sync metadata for a session that needs re-indexing (stale index_version).
// Returns the reconstructed session, the start timestamp, and the transcript path.
// Missing input returns (nil, 0, "", nil). Unreadable metadata returns an error
// that prohibits fallback reconstruction from native source information.
//
// Optimization: queries the DB for host_slug and parent_id before scanning the
// filesystem. If the DB has a location record, jumps directly to the session
// directory. Falls back to full directory scan only when the session is not in
// the DB (e.g. pre-DB ingestion data).
func (p *Pipeline) reconstructFromMetadata(ctx context.Context, sid SessionID) (*DiscoveredSession, int64, string, error) {
	if err := p.checkStoredMetadataVersion(ctx, sid); err != nil {
		return nil, 0, "", err
	}
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
				result, err := p.readSessionMetadata(hostDir, sid, "pipeline")
				if err != nil {
					return nil, 0, "", err
				}
				if result != nil {
					return &result.session, result.startMs, result.transcriptPath, nil
				}
			} else {
				// Nested subagent layout: {hostSlug}/{parentID}/subagents/{sid}/
				subDir := fmt.Sprintf("%s/%s/%s", hostDir, parentID, defaults.DirSubagents.String())
				result, err := p.readSessionMetadata(subDir, sid, "pipeline")
				if err != nil {
					return nil, 0, "", err
				}
				if result != nil {
					return &result.session, result.startMs, result.transcriptPath, nil
				}
			}
			// DB had a record but the file was missing — fall through to full scan.
		}
	}

	// Slow path: scan all host directories (used when session is not in the DB).
	entries, err := p.fs.ReadDir(outputDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, "", nil
		}
		return nil, 0, "", managedInputIOError(outputDir, err)
	}

	for _, hostEntry := range entries {
		if !hostEntry.IsDir() || strings.HasPrefix(hostEntry.Name(), defaults.TempDirPrefix) {
			continue
		}
		hostDir := fmt.Sprintf("%s/%s", outputDir, hostEntry.Name())

		// Check flat layout first: {hostSlug}/{sid}/{metaFilename}
		result, err := p.readSessionMetadata(hostDir, sid, "pipeline")
		if err != nil {
			return nil, 0, "", err
		}
		if result != nil {
			return &result.session, result.startMs, result.transcriptPath, nil
		}

		// Check nested subagent layout: {hostSlug}/{parentID}/subagents/{sid}/{metaFilename}
		sessionEntries, readErr := p.fs.ReadDir(hostDir)
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				continue
			}
			return nil, 0, "", managedInputIOError(hostDir, readErr)
		}
		for _, sessionEntry := range sessionEntries {
			if !sessionEntry.IsDir() {
				continue
			}
			subHostDir := fmt.Sprintf("%s/%s/%s", hostDir, sessionEntry.Name(), defaults.DirSubagents.String())
			result, err := p.readSessionMetadata(subHostDir, sid, "pipeline")
			if err != nil {
				return nil, 0, "", err
			}
			if result != nil {
				return &result.session, result.startMs, result.transcriptPath, nil
			}
		}
	}

	return nil, 0, "", nil
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
	if err := p.checkStoredMetadataVersion(ctx, sid); err != nil {
		slog.Warn("reconstructFromSourceInfo: stored metadata refused", "session_id", sid, "error", err)
		p.reportMetadataRefusal(string(sid), err)
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
				p.reportMetadataRefusal(string(sid), recognitionErr)
				return nil, 0, ""
			}
			transcriptOrigin = origin
		} else if !errors.Is(readErr, fs.ErrNotExist) {
			slog.Warn("reconstructFromSourceInfo: managed transcript unreadable", "session_id", sid,
				"error", managedInputIOError(outputTranscriptPath, readErr))
			p.reportMetadataRefusal(string(sid), managedInputIOError(outputTranscriptPath, readErr))
			return nil, 0, ""
		}
	}

	session := &DiscoveredSession{
		SessionID:        sid,
		SourcePath:       resolvedSrc,
		SourceFormat:     sourceFormat,
		Harness:          provider,
		TranscriptOrigin: transcriptOrigin,
	}
	if sourceFormat == SourceFormatJSONL || transcriptOrigin == TranscriptOriginOpenCodeLegacySQLite || transcriptOrigin == TranscriptOriginOpenCodeCurrentSQLite {
		artifact, err := p.recoverRetainedMetadata(ctx, *session, outputTranscriptPath)
		if err != nil {
			p.reportMetadataRefusal(string(sid), err)
			slog.Warn("reconstructFromSourceInfo: retained metadata recovery refused", "session_id", sid, "error", err)
			return nil, 0, ""
		}
		if artifact == nil {
			return nil, 0, ""
		}
		session.ParentUUID = artifact.Metadata.ParentUUID
		return session, artifact.Metadata.Timestamp.Start, outputTranscriptPath
	}
	return session, 0, outputTranscriptPath
}

// runReindex implements the --reindex pipeline mode.
// Instead of discovering from source providers, it scans the peasant-sync output
// directory to find existing sessions, then re-processes those with stale or
// missing index data.
//
// Steps:
//  1. Scan peasant-sync output to enumerate all existing sessions (by reading metadata JSONs)
//  2. Filter to targeted sessions:
//     - Default: stale producer or missing/changed captured input
//     - With --force: all sessions matching the explicit harness/session/since filters
//  3. For each targeted session:
//     a. Index readable v9/v10 metadata from retained input without adapter refresh
//     b. For older metadata, try EXTRACT+WRITE from the original source; if missing,
//     warn and fall back to INDEX+COMPUTE from the existing transcript
//     c. Run INDEX+COMPUTE
//  4. Write index_log entries, populate PipelineResult.IndexLog
func (p *Pipeline) runReindex(ctx context.Context, start time.Time) (*PipelineResult, error) {
	prog := p.config.Progress
	backfilled, err := p.backfillIncompleteContent(ctx)
	if err != nil {
		return nil, fmt.Errorf("reindex content recovery: %w", err)
	}
	p.contentRecoveries = backfilled

	// Stage 1: DISCOVER — scan peasant-sync output.
	discoverProfileStart := time.Now()
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDiscover})
	// Content recovery repairs the stored capture only. Every scanned target,
	// recovered or not, still receives the adapter/indexer evaluation below.
	scanned := p.scanPeasantSyncSessions(ctx)
	if err := ctx.Err(); err != nil {
		emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiscover, Err: err})
		return nil, fmt.Errorf("pipeline reindex discovery: %w", err)
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiscover, Done: len(scanned), Total: len(scanned)})
	p.recordIndexProfileStage(StageDiscover, discoverProfileStart, len(scanned), len(scanned))
	prepareProfileStart := time.Now()
	p.recordIndexProfileStage(StagePrepare, prepareProfileStart, len(scanned), len(scanned))

	// Stage 2: DIFF — filter to targeted sessions.
	diffProfileStart := time.Now()
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: len(scanned)})
	diffDone := 0
	cancelDiff := func(err error) (*PipelineResult, error) {
		emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiff, Done: diffDone, Total: len(scanned), Err: err})
		p.recordIndexProfileStage(StageDiff, diffProfileStart, diffDone, len(scanned))
		return nil, fmt.Errorf("pipeline reindex diff: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return cancelDiff(err)
	}
	var targeted []reindexTarget
	for _, target := range scanned {
		if err := ctx.Err(); err != nil {
			return cancelDiff(err)
		}
		var needsWork bool
		if p.config.DryRun {
			// Plan from recorded SQL evidence without reading full transcripts.
			needsWork = p.dryRunIndexNeedsWork(ctx, target) || p.adapterTargetNeedsWork(ctx, target)
		} else {
			needsWork = p.adapterTargetNeedsWork(ctx, target) || p.indexTargetNeedsWork(ctx, target)
		}
		// A target whose evaluation was interrupted was not classified, so it
		// does not count as diff work this stage completed.
		if err := ctx.Err(); err != nil {
			return cancelDiff(err)
		}
		if needsWork {
			targeted = append(targeted, target)
		}
		diffDone++
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Done: diffDone, Total: len(scanned)})
	}
	if err := ctx.Err(); err != nil {
		return cancelDiff(err)
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDiff, Done: len(scanned), Total: len(scanned)})
	p.recordIndexProfileStage(StageDiff, diffProfileStart, len(targeted), len(scanned))

	// Stage 3: FILTER — explicit session and age constraints apply to both stale
	// and forced work. Saved discovery selection is not an access boundary over
	// already-stored sessions and is deliberately not applied here.
	filterProfileStart := time.Now()
	filterTotal := len(targeted)
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageFilter, Total: filterTotal})
	filtered := targeted[:0]
	for index, target := range targeted {
		allowed := p.config.AllowedSessionIDs == nil || p.config.AllowedSessionIDs[target.session.SessionID]
		// Retained metadata records the native session start. The managed file's
		// mtime is publication time, so it cannot establish a session's age.
		if allowed && p.config.Since != nil {
			allowed = !time.UnixMilli(target.startMs).Before(*p.config.Since)
		}
		if allowed {
			if err := p.checkStoredMetadataVersion(ctx, target.session.SessionID); err != nil {
				// The refusal is a run diagnostic, not a structured log line the
				// interactive renderer would have to suppress.
				p.reportMetadataRefusal(string(target.session.SessionID), err)
				allowed = false
			}
		}
		if allowed {
			filtered = append(filtered, target)
		}
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageFilter, Done: index + 1, Total: filterTotal})
	}
	targeted = filtered
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageFilter, Done: filterTotal, Total: filterTotal})
	p.recordIndexProfileStage(StageFilter, filterProfileStart, len(targeted), filterTotal)

	if p.config.DryRun {
		result := &PipelineResult{
			Duration: time.Since(start),
			Summary:  PipelineSummary{HarvesterVersions: maps.Clone(p.versionTargets()), MetadataVersion: int(CurrentSchemaVersion)},
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
	if p.store != nil {
		ids := make([]SessionID, 0, len(targeted))
		for _, target := range targeted {
			ids = append(ids, target.session.SessionID)
		}
		locations, err := p.store.BulkLookupSessionLocations(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("reindex: load stored session locations before extraction: %w; no sessions were rewritten; repair database access and retry", err)
		}
		p.locationCache = locations
	}
	var fallbackTargets []reindexTarget // sessions where source is missing → INDEX+COMPUTE only

	// Build maps for parent-before-child ordering (same pattern as normal pipeline).
	entryByID := make(map[SessionID]DiffEntry, len(targeted))
	childrenOf := make(map[SessionID][]SessionID, len(targeted))
	inBatch := make(map[SessionID]bool, len(targeted))
	// Reuse canonical discovery facts (notably Cursor workspace and SQLite
	// source kind) instead of reconstructing them from a managed sidecar.
	sourceSessions := make(map[SessionID]DiscoveredSession)
	for harness, config := range p.config.Sources {
		factory, ok := p.adapters[harness]
		if !ok || !config.Enabled {
			continue
		}
		sessions, err := factory(p.fs, p.git, p.salt).Discover(ctx, config)
		if err != nil {
			slog.Warn("reindex: source discovery unavailable", "harness", harness, "error", err)
			continue
		}
		for _, session := range sessions {
			sourceSessions[session.SessionID] = session
		}
	}

	for _, t := range targeted {
		if !t.refreshMetadata && !p.adapterTargetNeedsWork(ctx, t) {
			// harvest index --force is an explicit manual refresh: use a usable
			// native capture when the recorded source is still there, and
			// otherwise warn once and index from the retained input, keeping the
			// old artifact and adapter stamp. Routine version-driven indexing
			// stays retained-first and never reacquires native input here.
			// A native refresh needs the session as discovery saw it (workspace,
			// worktree, commit context); a locator rebuilt from the retained
			// sidecar alone would degrade attribution. Only a session this run
			// discovered is refreshed natively.
			if p.config.Force {
				discovered, found := sourceSessions[t.session.SessionID]
				if found && p.forcedNativeRefreshUsable(ctx, t, discovered) {
					entryByID[discovered.SessionID] = DiffEntry{Session: discovered, Status: DiffUpdated}
					inBatch[discovered.SessionID] = true
					continue
				}
				if !found {
					p.reportDiagnostic(DiagnosticEntry{
						ErrorType: "native_refresh_unavailable", Location: fmt.Sprintf("%s session %s forced refresh", t.session.Harness, t.session.SessionID),
						Message:     fmt.Sprintf("forced refresh of session %s: native discovery did not offer the session (recorded source %q), so the retained input was indexed instead; the previous artifact and adapter stamp were preserved", t.session.SessionID, t.originalSourcePath),
						Remediation: "Enable the harness source in the configuration and restore the original source, then rerun harvest index --force to refresh from native input.",
					})
				}
			}
			fallbackTargets = append(fallbackTargets, t)
			continue
		}
		// Preserve the existing historical-schema fallback report when native
		// refresh is required and its source is already known to be unavailable.
		if t.refreshMetadata {
			sourceExists := false
			if t.originalSourcePath != "" {
				_, err := p.fs.Stat(t.originalSourcePath)
				sourceExists = err == nil
			}
			if !sourceExists {
				fallbackTargets = append(fallbackTargets, t)
				continue
			}
		}
		sourceSession := p.nativeSessionForTarget(t, p.adapterTargetMetadata(ctx, t))
		entryByID[sourceSession.SessionID] = DiffEntry{Session: sourceSession, Status: DiffUpdated, retainedOnly: true}
		inBatch[sourceSession.SessionID] = true
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
		// Reserve every possible per-session reconciliation failure, as in Run.
		reindexErrChSize := extractTotal + 1
		if reindexErrChSize < 16 {
			reindexErrChSize = 16
		}
		reindexErrCh := make(chan error, reindexErrChSize)
		reindexQueueSize := workers * 2
		if reindexQueueSize < 1 {
			reindexQueueSize = 1
		}
		reindexIndexCh := make(chan streamedIndexWork, reindexQueueSize)
		reindexIndexDoneCh := make(chan DrainBatch, reindexErrChSize)
		reindexDownstreamCh := make(chan indexedMeta, reindexQueueSize)
		reindexWriteLane := newStoreWriteLane(reindexQueueSize)
		var reindexDownstream streamedDownstreamResult

		var reindexWg sync.WaitGroup

		// Stage 4a (reindex): EXTRACT+WRITE workers goroutine.
		reindexWg.Add(1)
		go func() {
			defer reindexWg.Done()
			extractProfileStart := time.Now()
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
			p.recordIndexProfileStage(StageExtract, extractProfileStart, int(extractDoneAtomic.Load()), extractTotal+len(fallbackTargets))
		}()

		// Stage 4b (reindex): Consumer goroutine — DB INSERT + INDEX coordination.
		// Uses the same drainLoop/indexLoop goroutine pattern as Run() for correctness:
		// Commit-before-Ack ordering, no runtime.Gosched() spin, proper error propagation.
		emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDBInsert, Total: extractTotal})
		reindexWg.Add(1)
		go func() {
			defer reindexWg.Done()
			dbInsertProfileStart := time.Now()
			defer close(reindexIndexCh)
			reindexDrainResults := p.drainLoop(ctx, staging, &reindexWorkersDone, reindexIndexCh, reindexIndexDoneCh, reindexErrCh, prog, extractTotal, reindexWriteLane)
			sessionResults = append(sessionResults, reindexDrainResults...)
			emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDBInsert, Done: len(reindexDrainResults), Total: extractTotal})
			p.recordIndexProfileStage(StageDBInsert, dbInsertProfileStart, len(reindexDrainResults), extractTotal)
		}()

		// Stage INDEX goroutine (reindex).
		emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: extractTotal})
		reindexWg.Add(1)
		go func() {
			defer reindexWg.Done()
			defer close(reindexDownstreamCh)
			indexProfileStart := time.Now()
			drainIndexed, drainIndexLogEntries = p.indexLoop(ctx, reindexIndexCh, reindexIndexDoneCh, prog, IndexOutcomeReindexed, "reindex", reindexDownstreamCh, reindexWriteLane)
			p.recordIndexProfileStage(StageIndex, indexProfileStart, len(drainIndexed), extractTotal)
		}()

		reindexWg.Add(1)
		go func() {
			defer reindexWg.Done()
			reindexDownstream = p.runStreamedDownstream(ctx, reindexDownstreamCh, prog, extractTotal, "reindex", reindexWriteLane)
		}()

		// Wait for all reindex goroutines to complete.
		reindexWg.Wait()
		reindexWriteLane.close()
		close(reindexErrCh)

		// Collect first store error (best-effort: pipeline continues on DB failure).
		var storeErr error
		for err := range reindexErrCh {
			if storeErr == nil {
				storeErr = err
			}
		}

		// Retained sessions need no EXTRACT+WRITE. Warn only when required native
		// refresh was unavailable, not for ordinary compatible metadata reuse.
		fallbackExtractProfileStart := time.Now()
		for _, t := range fallbackTargets {
			if t.refreshMetadata {
				indexLogEntries = append(indexLogEntries, p.reindexFallbackLog(t))
			}

			indexSessions = append(indexSessions, p.prepareReindexFallback(ctx, t))

			sessionResults = append(sessionResults, SessionResult{
				SessionID: t.session.SessionID,
				Harness:   t.session.Harness,
				Status:    DiffUpdated,
			})
			done := int(extractDoneAtomic.Add(1))
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageExtract, Done: done, Total: extractTotal + len(fallbackTargets)})
		}
		emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageExtract, Done: int(extractDoneAtomic.Load()), Total: extractTotal + len(fallbackTargets)})
		p.recordIndexProfileStage(StageExtract, fallbackExtractProfileStart, len(fallbackTargets), len(fallbackTargets))

		// Stages 5-9: INDEX, COMPUTE, CLEANUP, REPORT, AUDIT (shared with Run).
		return p.indexComputeAndFinalize(ctx, indexSessions, drainIndexed, sessionResults, storeErr, start, append(append(indexLogEntries, drainIndexLogEntries...), p.contentRecoveryLogEntries()...), IndexOutcomeReindexed, "reindex", &reindexDownstream)
	}

	// No extractable sessions — all are fallback. Process fallback sessions directly.
	extractProfileStart := time.Now()
	for _, t := range fallbackTargets {
		if t.refreshMetadata {
			indexLogEntries = append(indexLogEntries, p.reindexFallbackLog(t))
		}

		indexSessions = append(indexSessions, p.prepareReindexFallback(ctx, t))

		sessionResults = append(sessionResults, SessionResult{
			SessionID: t.session.SessionID,
			Harness:   t.session.Harness,
			Status:    DiffUpdated,
		})
		done := int(extractDoneAtomic.Add(1))
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageExtract, Done: done, Total: len(fallbackTargets)})
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageExtract, Done: int(extractDoneAtomic.Load()), Total: len(fallbackTargets)})
	p.recordIndexProfileStage(StageExtract, extractProfileStart, int(extractDoneAtomic.Load()), len(fallbackTargets))

	// DB INSERT — no extractable sessions means no store entries.
	dbInsertProfileStart := time.Now()
	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageDBInsert, Total: 0})
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageDBInsert, Done: 0, Total: 0})
	p.recordIndexProfileStage(StageDBInsert, dbInsertProfileStart, 0, 0)
	var storeErr error

	// Steps 3e-end: INDEX, COMPUTE, CLEANUP, REPORT, AUDIT (shared with Run).
	return p.indexComputeAndFinalize(ctx, indexSessions, drainIndexed, sessionResults, storeErr, start, append(append(indexLogEntries, drainIndexLogEntries...), p.contentRecoveryLogEntries()...), IndexOutcomeReindexed, "reindex", nil)
}

// forcedNativeRefreshUsable decides whether harvest index --force re-reads a
// discovered native source or indexes the retained input. Force is an
// explicit manual refresh, so the native source is captured when the retained
// input cannot settle the session: the source shows change evidence against
// the retained capture (a newer clock, a moved locator, a changed parent, an
// advanced cursor), or the retained input is a directory tree, which has no
// byte proof against the publication capture and so cannot bind publication
// when indexed on its own. A file source that shows no change is what the
// retained artifact already consumed; re-reading it is the original-source
// I/O the retained-first rule exists to avoid, so the retained input is
// indexed with no native read. A retained artifact produced by a newer adapter
// than this build is never replaced from native input: the newer producer's
// evidence is kept and its retained input is indexed as it is.
func (p *Pipeline) forcedNativeRefreshUsable(ctx context.Context, target reindexTarget, discovered DiscoveredSession) bool {
	metadata := p.adapterTargetMetadata(ctx, target)
	if metadata == nil {
		return false
	}
	if metadata.AdapterVersion != nil && *metadata.AdapterVersion > p.versionTargets()[target.session.Harness].AdapterVersion {
		return false
	}
	return p.nativeInputChanged(discovered, metadata) || p.retainedInputKind(target) == TranscriptSourceDirectory
}

// retainedInputKind reports the source kind the target's indexer reads for
// its retained input, or the zero kind when no indexer is registered.
func (p *Pipeline) retainedInputKind(target reindexTarget) TranscriptSourceKind {
	indexer, ok := p.indexers[target.session.Harness]
	if !ok {
		return TranscriptSourceKind(0)
	}
	im := indexedMeta{session: target.session, startMs: target.startMs, outputTranscriptPath: target.transcriptPath}
	if resolver, ok := indexer.(SessionTranscriptSourceResolver); ok {
		return resolver.TranscriptSourceKindFor(indexTargetSession(im))
	}
	return indexer.SourceKind()
}

// reindexTarget represents a session found in the peasant-sync output directory
// that is a candidate for re-indexing.
type reindexTarget struct {
	session            DiscoveredSession
	startMs            int64
	transcriptPath     string // path to the peasant-sync transcript on disk
	originalSourcePath string // path to the original source file (may not exist)
	refreshMetadata    bool   // historical metadata requires native extraction
}

func (p *Pipeline) reindexFallbackLog(target reindexTarget) IndexLogEntry {
	slog.Warn("reindex: original source missing, falling back to INDEX+COMPUTE",
		"session_id", target.session.SessionID,
		"harness", target.session.Harness,
		"expected_path", target.originalSourcePath,
		"stage", "EXTRACT+WRITE",
		"impact", "metadata not refreshed, indexing from existing peasant-sync transcript",
		"fix", "ensure harness data exists at configured source path",
	)
	reason := "original source missing; falling back to existing peasant-sync transcript"
	return p.makeIndexLogEntry(indexedMeta{
		session: target.session, startMs: target.startMs, outputTranscriptPath: target.transcriptPath,
	}, IndexOutcomeFallback, 0, time.Now().UnixMilli(), &reason, nil)
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
	if buffered, ok := p.classifier.(BufferedSessionClassifier); ok {
		return p.stageAnnotateBuffered(ctx, sessionIDs, prog, buffered)
	}
	workers := parallelWorkers(p.config)
	total := len(sessionIDs)
	profiled, useProfile := p.classifier.(ProfiledSessionClassifier)
	profiler := p.config.IndexProfiler

	var wg sync.WaitGroup
	ch := make(chan SessionID, workers)
	var done atomic.Int64

	// Spawn worker pool.
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sid := range ch {
				var err error
				if useProfile && profiler != nil {
					err = profiled.AnnotateWithProfile(ctx, sid, profiler)
				} else {
					err = p.classifier.Annotate(ctx, sid)
				}
				if err != nil {
					slog.Warn("pipeline: annotate session",
						"session_id", sid,
						"error", err,
						"what", "classifier failed to annotate session entries",
						"why", "classifier error, missing session_entries rows, or annotation persistence failure",
						"user_impact", "session annotations may be incomplete or recomputed on the next index run",
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

func (p *Pipeline) stageAnnotateBuffered(ctx context.Context, sessionIDs []SessionID, prog *ProgressState, classifier BufferedSessionClassifier) error {
	workers := parallelWorkers(p.config)
	total := len(sessionIDs)
	if total == 0 {
		return nil
	}
	profiler := p.config.IndexProfiler

	sessions := make(chan SessionID, workers)
	batches := make(chan SessionAnnotationBatch, workers)
	results := make(chan SessionAnnotationBatchResult, total)

	var resultWG sync.WaitGroup
	resultWG.Add(1)
	go func() {
		defer resultWG.Done()
		done := 0
		for result := range results {
			if result.Err != nil {
				slog.Warn("pipeline: annotate session",
					"session_id", result.SessionID,
					"error", result.Err,
					"what", "classifier failed to annotate session entries",
					"why", "classifier error, missing session_entries rows, or annotation persistence failure",
					"user_impact", "session annotations may be incomplete or recomputed on the next index run",
					"how_to_fix", "re-run peasant ingest index --force --session "+string(result.SessionID))
			}
			done++
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageAnnotate, Done: done, Total: total})
		}
	}()

	var prepareWG sync.WaitGroup
	for range workers {
		prepareWG.Add(1)
		go func() {
			defer prepareWG.Done()
			for sid := range sessions {
				batch, err := classifier.PrepareAnnotations(ctx, sid, profiler)
				if err != nil {
					results <- SessionAnnotationBatchResult{SessionID: sid, Err: err}
					continue
				}
				if batch.SessionID == "" {
					batch.SessionID = sid
				}
				if batch.Skipped || (len(batch.Writes) == 0 && batch.RunState == nil) {
					results <- SessionAnnotationBatchResult{SessionID: batch.SessionID}
					continue
				}
				batches <- batch
			}
		}()
	}

	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		ticker := time.NewTicker(annotationFlushInterval)
		defer ticker.Stop()
		var pending []SessionAnnotationBatch
		pendingWrites := 0
		flush := func() {
			if len(pending) == 0 {
				return
			}
			flushed := classifier.FlushAnnotationBatches(ctx, pending, profiler)
			if len(flushed) > len(pending) {
				flushed = flushed[:len(pending)]
			}
			for _, result := range flushed {
				results <- result
			}
			if len(flushed) < len(pending) {
				for _, batch := range pending[len(flushed):] {
					results <- SessionAnnotationBatchResult{
						SessionID: batch.SessionID,
						Err:       fmt.Errorf("pipeline: annotation batch flush returned %d result(s) for %d session batch(es)", len(flushed), len(pending)),
					}
				}
			}
			pending = pending[:0]
			pendingWrites = 0
		}
		for {
			select {
			case batch, ok := <-batches:
				if !ok {
					flush()
					return
				}
				pending = append(pending, batch)
				pendingWrites += len(batch.Writes)
				if len(pending) >= annotationFlushSessionLimit || pendingWrites >= annotationFlushWriteLimit {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()

	for _, sid := range sessionIDs {
		sessions <- sid
	}
	close(sessions)
	prepareWG.Wait()
	close(batches)
	writerWG.Wait()
	close(results)
	resultWG.Wait()
	return nil
}
