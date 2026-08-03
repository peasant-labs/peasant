package defaults

// Config value defaults for the peasant ingest pipeline.
const (
	ConfigVersion               = 1
	ConfigStalenessThresholdSec = 60
)

// SourcePath is a typed default source directory path.
type SourcePath string

func (p SourcePath) String() string { return string(p) }

const (
	DefaultClaudePath   SourcePath = "~/.claude/projects"
	DefaultOpenCodePath SourcePath = "~/.local/share/opencode"
	DefaultCodexPath    SourcePath = "~/.codex/sessions"
	DefaultCursorPath   SourcePath = "~/.cursor/projects"
	DefaultStrikePath   SourcePath = "~/.strike/sessions"
)

// OutputPath is a typed default output base path.
type OutputPath string

func (p OutputPath) String() string { return string(p) }

// OutputSyncSubdir is the leaf directory (under the resolved data dir) that holds
// ingested transcripts. The default output base is ResolveOutputBasePath() =
// <data dir>/peasant-sync, which HONORS XDG_DATA_HOME the same way the DB path
// does — it is NOT a hardcoded ~/.local/share path, so XDG_DATA_HOME fully
// sandboxes ingest output. See defaults.ResolveOutputBasePath.
const OutputSyncSubdir = "peasant-sync"
