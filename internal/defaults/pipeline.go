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

// Scanner buffer sizes for reading large JSONL lines.
const (
	ScannerMaxLine = 10 << 20 // 10 MiB
	ScannerInitBuf = 64 << 10 // 64 KiB
)

// WebAssetsSubdir is the embedded filesystem subdirectory for web assets.
const WebAssetsSubdir = "web/out"

// SPAFallbackFile is the default index file for single-page app routing.
const SPAFallbackFile = "/index.html"
