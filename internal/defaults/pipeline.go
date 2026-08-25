package defaults

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
)

// Provider-specific filename prefixes.
const (
	ClaudeSubagentPrefix  = "agent-"
	OpenCodeSessionPrefix = "ses_"
)

// ContentPreviewLimit is the maximum character length for ContentPreview fields.
const ContentPreviewLimit = 2000

// OpenCodeManagedProjectionMaxBytes bounds the Peasant-managed OpenCode SQLite
// projection file the indexer reads from disk. The projection holds one
// session's normalized message and part rows as JSON, so it is small; a real
// projection is kilobytes to low megabytes. The bound exists as defense in
// depth: an OpenCode SQLite session's discovered source path is the provider
// database, and only the post-harvest managed projection ever belongs at the
// path the indexer reads. If any wiring mistake ever points the reader at the
// database instead of the projection, the reader refuses the oversized file
// rather than loading a multi-gigabyte database into memory and aborting the
// process. 64 MiB is far above any real projection and far below a database
// that would exhaust memory.
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

// Scanner buffer sizes for reading large JSONL lines.
const (
	ScannerMaxLine = 10 << 20 // 10 MiB
	ScannerInitBuf = 64 << 10 // 64 KiB
)

// WebAssetsSubdir is the embedded filesystem subdirectory for web assets.
const WebAssetsSubdir = "web/out"

// SPAFallbackFile is the default index file for single-page app routing.
const SPAFallbackFile = "/index.html"
