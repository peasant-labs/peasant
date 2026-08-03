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

// Scanner buffer sizes for reading large JSONL lines.
const (
	ScannerMaxLine = 10 << 20 // 10 MiB
	ScannerInitBuf = 64 << 10 // 64 KiB
)

// WebAssetsSubdir is the embedded filesystem subdirectory for web assets.
const WebAssetsSubdir = "web/out"

// SPAFallbackFile is the default index file for single-page app routing.
const SPAFallbackFile = "/index.html"
