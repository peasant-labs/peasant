package defaults

import "time"

// FileExt is a typed file extension.
type FileExt string

func (e FileExt) String() string { return string(e) }

const (
	ExtJSONL FileExt = ".jsonl"
	ExtJSON  FileExt = ".json"
	ExtGit   FileExt = ".git"
)

// Pipeline filename patterns.
const (
	MetadataSuffix   = "--metadata.json"
	TranscriptPrefix = "--transcript."
	TempDirPrefix    = ".tmp-"
	UntrackedPrefix  = "__peasant-untracked__"
	TempSuffixLen    = 8
)

// DirName is a typed directory name component.
type DirName string

func (d DirName) String() string { return string(d) }

const (
	DirSubagents       DirName = "subagents"
	DirDebug           DirName = "debug"
	OpenCodeDirStorage DirName = "storage"
	OpenCodeDirSession DirName = "session"
	OpenCodeDirMessage DirName = "message"
	OpenCodeDirPart    DirName = "part"
	OpenCodeDirProject DirName = "project"
	// ClaudeDirWorkflows is the directory under a Claude session's subagents
	// directory where the workflow runtime keeps one directory per run.
	ClaudeDirWorkflows DirName = "workflows"
)

// DebugArtifactSuffixes is the CLOSED set of extensions a debug output Peasant
// writes into a session's debug directory carries: ".json" for a captured tool
// output and ".log" for a harness log.
//
// Ownership inside that directory is decided by the name, not by the directory:
// a publication may retire a file it owns, and the directory is an ordinary one
// a user or another tool can write into, so a name outside this set is left
// alone exactly like a file beside the artifact. Widening the set widens what a
// publication may delete, so a new debug output takes one of these extensions
// rather than adding a third.
func DebugArtifactSuffixes() []string { return []string{".json", ".log"} }

// Provider-specific filename prefixes.
const (
	ClaudeSubagentPrefix = "agent-"
	// ClaudeWorkflowRunPrefix starts the name of a Claude workflow run
	// directory; the whole directory name is the run id.
	ClaudeWorkflowRunPrefix = "wf_"
	OpenCodeSessionPrefix   = "ses_"
)

// ContentPreviewLimit is the maximum UTF-8 byte length for ContentPreview fields.
const ContentPreviewLimit = 2000

// FullContentWriteBatchBytes is a soft full-string budget for ingest flushes.
// A single oversized session may occupy a batch without truncation.
const FullContentWriteBatchBytes int64 = 32 << 20

// SQLiteBusyTimeout is the open path's PRAGMA busy_timeout: another process
// waits at most this long for the single SQLite writer. It mirrors the fixed
// 5 s in internal/store (store.go and readonly.go). The write budgets derive
// from it: a writer-lane hold must stay below it, so the hold target carries
// a safety factor. If the PRAGMA ever becomes configurable, this constant and
// the hold-target derivation move with it.
const SQLiteBusyTimeout = 5 * time.Second

// Write-budget defaults. Every value carries its derivation in the harmonized
// content-model design (§0.5); the sandbox run re-derives them by measurement.
// They are the values a WriteConfig resolves to when the user names no
// write.* key.
const (
	// WriteDefaultHoldTarget is 0.8 × SQLiteBusyTimeout: any transaction's
	// hold must stay below the open path's busy_timeout, and the safety
	// factor leaves scheduling headroom.
	WriteDefaultHoldTarget = SQLiteBusyTimeout * 4 / 5
	// WriteDefaultActivationSessions is the hold target divided by the
	// measured per-session commit cost. The sandbox measured budget-
	// conforming activation commits at 60 ms (126 entries) and 350 ms
	// (828 entries) against the 4 s hold, so a 64-session batch of
	// byte-conforming sessions commits far under it; the byte cap stays
	// the binding bound. An oversized conversion commits alone (the
	// 71 MiB largest session measured 6.6 s).
	WriteDefaultActivationSessions = 64
	// WriteDefaultBatchBytes is the staging memory bound (the legacy drain's
	// soft full-string budget). A session whose objects exceed it stages
	// alone in budget-sized transactions.
	WriteDefaultBatchBytes = FullContentWriteBatchBytes
	// WriteDefaultBatchSessions is a convenience cap alongside the byte cap;
	// the byte cap is the binding bound.
	WriteDefaultBatchSessions = 64
	// WriteDefaultBufferBytes is the per-worker pre-allocated buffer cap,
	// fixed at run start and never grown. The sandbox measured the largest
	// single prepared unit at 14.2 MiB (the largest entry_json; the largest
	// content blob is 7.1 MiB), so the cap sits above it at 16 MiB; a unit
	// above the cap still bypasses the shared buffers under the total
	// staged-memory cap.
	WriteDefaultBufferBytes = 16 << 20
	// WriteDefaultFlushIntervalMs bounds the write lane's idle wait before it
	// commits a partial batch: the push-profiler and legacy ANNOTATE flush
	// interval. The sandbox confirmed the mechanism (the flush-on-interval
	// gate proves the lane reads the knob at 50 ms and hour bounds) with no
	// contrary evidence, so the profiler interval stands.
	WriteDefaultFlushIntervalMs = 500
	// WriteDefaultSweepRows is the hold target divided by the measured
	// delete rate: the sandbox deleted 5,000 wide chunk rows in 0.04 s
	// (about 119 k rows/s; narrow rows about 245 k rows/s), so the
	// configured batch holds about two orders of magnitude under the
	// 4 s hold.
	WriteDefaultSweepRows = 5000
	// WriteDefaultHarvestTarget is half the measured warm-harvest baseline
	// for the cohort. The 40-minute baseline stands and the integrated
	// build beats it: a full-discovery warm run on the sandbox processed a
	// 1,512-session live delta in 6m49s (1,465 updated, 47 dirty-record
	// errors; COMPUTE swept all 19,119 sessions in 3m41s), so the 20-minute
	// target holds with headroom on the new pipeline's skip paths.
	WriteDefaultHarvestTarget = 20 * time.Minute
)

// OrdinaryHarvestContentBudgetBytes bounds how many input bytes one ordinary
// `peasant harvest` charges to the one-time full-content capture. The capture
// is resumable across runs with no cursor, so a large backlog is worked down a
// bounded slice at a time instead of stalling a single harvest. `peasant
// harvest index` runs it to the end with no budget.
const OrdinaryHarvestContentBudgetBytes int64 = 1 << 30 // 1 GiB

// OpenCodeManagedProjectionMaxBytes is the size at which an OpenCode session's
// payload stops being small. It bounds the kickstart PREVIEW, which shows a
// prefix and says how much it left out.
//
// It is no longer a gate on reading a managed projection. It used to be: the
// reader refused a projection past this size as defense in depth, because an
// OpenCode SQLite session's discovered source path is the provider database and
// a wiring mistake could point the reader at a multi-gigabyte database. That
// defense is now a content test, which identifies a database from its first
// bytes without reading it, because a size test also failed the long sessions
// whose projections are legitimately large, and losing a whole session is not
// something a size may do.
const OpenCodeManagedProjectionMaxBytes = 64 << 20 // 64 MiB

// OpenCodePreviewMaterializeMaxBytes bounds how much OpenCode session payload
// the kickstart preview materializes before it stops and marks the result
// truncated. Only the preview path uses this bound; ingest and harvest still
// materialize the whole session.
//
// The preview reads a session directly from the provider database, so an
// especially long session can hold hundreds of megabytes of part or message
// payload. Without a bound the preview accumulates every row, marshals the
// whole projection, and re-parses it, which multiplies one session into
// several gigabytes of live heap. This bound stops that: the preview shows a
// prefix and names how much of the session it left out.
//
// The value equals OpenCodeManagedProjectionMaxBytes on purpose. That constant
// is the largest managed projection the indexer will read from disk, so a
// preview that shows at most this many payload bytes never renders more than a
// valid managed projection could hold. A single, shared 64 MiB ceiling keeps
// the preview and the stored-projection paths consistent.
const OpenCodePreviewMaterializeMaxBytes = OpenCodeManagedProjectionMaxBytes

// OpenCodePreviewFirstPageMaxBytes bounds the first, quickly-read slice of an
// OpenCode session the kickstart preview paints before the full bounded read
// finishes. Only the preview path uses this bound.
//
// The whole bound exists to put turns on screen fast, so it is set from what a
// larger bound actually buys. Measured against a real OpenCode store holding a
// session of 2.2 GiB of message and part payload:
//
//	64 KiB   about 0.44 s   34 turns
//	128 KiB  about 1.37 s   48 turns
//	1 MiB    about 1.32 s   48 turns
//
// The cliff between the first two rows is one single message row of 25 MiB. A
// bound is spent BEFORE a row is taken, not after, so any bound past the point
// where that row is reached pays for the whole row - to read it, and again to
// parse it - and every larger bound then costs the same. 64 KiB stops short of
// it and returns several screenfuls of turns in under half a second, which is
// the point of the slice. The turns the larger bounds add arrive moments later
// anyway, in the full bounded read this slice is only covering for.
//
// A session whose whole payload fits inside this bound is read ONCE: the slice
// is then the entire session, and no second read runs.
const OpenCodePreviewFirstPageMaxBytes = 64 << 10 // 64 KiB

// OpenCodePreviewSliceMaxBytes bounds ONE continuation of the kickstart
// preview: the chunk of a session that loads when the reader scrolls to the
// bottom of the pane and asks for more. Only the preview path uses it.
//
// A very long session does not fit any single bound, so the preview reads it
// under one and then extends it as the reader scrolls. This value is what each
// of those extensions costs. It is set from what a real read takes: measured
// against an OpenCode store holding a session of 2.2 GiB of message and part
// payload, a continuation at this bound completes in about half a second to a
// second and adds several turns, which keeps the pane responsive to a held-down
// scroll while still making real progress through the session.
//
// It is far smaller than OpenCodePreviewMaterializeMaxBytes on purpose. That
// bound governs the FIRST read, which stands alone and should show as much as
// it safely can; this one governs a read the reader is waiting on, and a
// continuation that took as long as the first read would feel like the pane had
// stopped.
//
// The live payload of one continuation is bounded by this value plus the
// message share of it plus one oversized source row, and the turns the preview
// retains grow only as far as the reader scrolls.
const OpenCodePreviewSliceMaxBytes = 8 << 20 // 8 MiB

// MaxJSONLRecordBytes is the largest single JSONL record Peasant reads,
// redacts and indexes whole. The rule is the same for every JSONL harness
// (Claude Code, Codex, Cursor, Strike, Pi) and for the per-record reads of
// the OpenCode sources: a record up to this size is processed in full; a
// record over it is omitted without being loaded, is reported with an
// actionable diagnostic and a placeholder entry at its position, and the
// session still ingests and is stored partial. No size ever fails a session
// and no record is ever silently dropped.
//
// ScannerInitBuf is the starting read buffer. Readers grow from it on demand
// up to MaxJSONLRecordBytes, so an ordinary transcript never allocates the
// maximum.
//
// RedactScannerMaxLineBytes is the line limit the redact stage runs with. The
// redaction engine reads a record as a LINE, through a bufio.Scanner, and a
// Scanner refuses a line it cannot hold WITH its newline terminator. The
// largest record Peasant keeps is exactly MaxJSONLRecordBytes long, so its line
// is one byte longer, and a scanner limit equal to the record limit would
// refuse the very record the filter kept and fail the whole session. It is
// derived here, once, so the two limits cannot drift apart.
const (
	MaxJSONLRecordBytes       = 256 << 20 // 256 MiB
	ScannerInitBuf            = 64 << 10  // 64 KiB
	RedactScannerMaxLineBytes = MaxJSONLRecordBytes + 1
)

// WebAssetsSubdir is the embedded filesystem subdirectory for web assets.
const WebAssetsSubdir = "web/out"

// SPAFallbackFile is the default index file for single-page app routing.
const SPAFallbackFile = "/index.html"
