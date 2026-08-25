package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/salt"
)

// SourceAdapter abstracts provider-specific session discovery and metadata extraction.
// Implementations: ClaudeAdapter (S7), OpenCodeAdapter (S8).
type SourceAdapter interface {
	// Harness returns which provider this adapter handles.
	Harness() Harness
	// Discover finds all sessions under the configured source paths.
	Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error)
	// ExtractMetadata extracts UnifiedMetadata from a discovered session.
	ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error)
}

// TranscriptMaterializer converts a typed provider source into the managed
// transcript bytes Peasant stores. Most adapters ingest a transcript file
// directly and do not implement this interface. It exists for sources such as
// legacy OpenCode SQLite, where copying database bytes would not produce a
// transcript.
type TranscriptMaterializer interface {
	MaterializeTranscript(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, []byte, error)
}

// BoundedTranscriptMaterializer materializes a preview-only prefix of a session
// under a byte budget. The kickstart preview uses it so an especially long
// session cannot exhaust memory: the materializer probes the session's payload
// size first, materializes the whole session when it fits the budget, and
// otherwise stops after the budget is reached and reports the truncation. It is
// distinct from TranscriptMaterializer because ingest and harvest always
// materialize the whole session, never a bounded prefix.
type BoundedTranscriptMaterializer interface {
	MaterializeTranscriptBounded(ctx context.Context, session DiscoveredSession, budgetBytes int64) (*UnifiedMetadata, []byte, MaterializeTruncation, error)
}

// FirstPageTranscriptMaterializer materializes a small LEADING slice of a
// session under a byte budget, without first measuring the whole session. The
// kickstart preview uses it to paint turns while the full bounded read is still
// running: measuring an especially long session costs seconds on its own,
// because summing the payload length of rows held on overflow pages reads those
// pages, so a read that skips the measurement returns in a fraction of the time.
//
// It is distinct from BoundedTranscriptMaterializer for exactly that reason.
// The bounded read measures first so it can NAME what it left out; the
// first-page read cannot name anything and does not try. It reports only
// whether the session continues past the slice it returned, which is what a
// caller needs to decide whether a fuller read must follow.
type FirstPageTranscriptMaterializer interface {
	// MaterializeTranscriptFirstPage returns the managed bytes of the leading
	// slice, and reports whether the session holds more rows past it. A false
	// more means the slice IS the whole session.
	MaterializeTranscriptFirstPage(ctx context.Context, session DiscoveredSession, budgetBytes int64) (metadata *UnifiedMetadata, data []byte, more bool, err error)
}

// MaterializeTruncationUnit names what a bounded materialization counted toward
// its budget, so a truncation note can say "parts" for a legacy session and
// "messages" for a current one.
type MaterializeTruncationUnit uint8

const (
	// MaterializeUnitNone is the zero unit of an untruncated result.
	MaterializeUnitNone MaterializeTruncationUnit = iota
	// MaterializeUnitRows counts legacy source rows. A legacy session carries
	// payload in both its message and its part table, and a materialization
	// reads both, so its budget counts rows of either kind rather than parts
	// alone.
	MaterializeUnitRows
	// MaterializeUnitMessages counts current message rows.
	MaterializeUnitMessages
	// MaterializeUnitLines counts lines of a line-oriented transcript file,
	// which is what a file-origin slice reads and what its note counts.
	MaterializeUnitLines
)

// String renders the unit for a preview note.
func (u MaterializeTruncationUnit) String() string {
	switch u {
	case MaterializeUnitRows:
		return "rows"
	case MaterializeUnitMessages:
		return "messages"
	case MaterializeUnitLines:
		return "lines"
	default:
		return ""
	}
}

// MaterializeTruncation reports whether a bounded materialization stopped early
// and, when it did, how much of the session it left out. Truncated is false for
// a session that fit the budget, and the remaining fields are then zero. When
// Truncated is true, IncludedRows of TotalRows of the counted Unit were
// materialized, TotalBytes is the whole session's measured payload size, and
// BudgetBytes
// is the bound that stopped it. The preview renders these into a plain note; the
// full session still ingests normally.
type MaterializeTruncation struct {
	Truncated bool
	Unit      MaterializeTruncationUnit
	// BudgetBytes is the bound the read ran under. IncludedBytes is what the
	// read actually took, which can be less: a read that reserves part of its
	// budget for one kind of row stops the other kind short of the bound.
	// A note about what the reader SEES quotes IncludedBytes.
	BudgetBytes   int64
	IncludedBytes int64
	TotalBytes    int64
	IncludedRows  int64
	TotalRows     int64
}

// TranscriptOrigin identifies how a discovered session's managed transcript
// must be obtained. The zero value preserves the existing file-copy behavior.
type TranscriptOrigin uint8

const (
	TranscriptOriginFile TranscriptOrigin = iota
	TranscriptOriginOpenCodeLegacySQLite
	TranscriptOriginOpenCodeCurrentSQLite
)

// Validate rejects unknown origins at the pipeline trust boundary.
func (o TranscriptOrigin) Validate() error {
	switch o {
	case TranscriptOriginFile, TranscriptOriginOpenCodeLegacySQLite, TranscriptOriginOpenCodeCurrentSQLite:
		return nil
	default:
		return fmt.Errorf("transcript origin %d is outside the supported closed set", o)
	}
}

// SourceConfig holds per-provider discovery configuration.
type SourceConfig struct {
	Paths   []ResolvedPath
	Enabled bool
}

// DiscoveredSession represents a session found during discovery.
type DiscoveredSession struct {
	SessionID     SessionID
	Harness       Harness
	SourcePath    ResolvedPath   // Path to the main transcript file
	SourceFormat  SourceFormat   // "jsonl" for Claude, "json" for OpenCode
	OriginalRoot  ResolvedPath   // Harness root for multi-directory access (e.g. OpenCode message/part)
	ParentUUID    *SessionID     // nil for root sessions
	SubagentPaths []ResolvedPath // Child session transcript paths
	DebugPaths    []ResolvedPath // Debug artifact paths
	ModTime       time.Time      // Changed time of the source: when its content last changed
	ActiveModTime time.Time      // Source file/WAL mtime for the staleness (active) gate; zero falls back to ModTime
	ProjectName   string         // Human-readable project name (optional, populated during discovery when cheap to extract)
	// ProjectWorktree is the project's canonical root path, resolved from the
	// OpenCode project tables when present. It refines project naming and worktree
	// grouping without changing CWD, which stays the session's own directory. It
	// is empty when the project tables do not attribute the session.
	ProjectWorktree string
	Title           string // Session title (optional, populated when available without extra I/O)
	Branch          string // Git branch active when session ran (optional, from session data not current repo)
	Agent           string // Agent label for a subagent session (optional; OpenCode records it on the session row, like a Claude teammate's agent type)
	// Slug is the harness-generated session name, used as the display-name
	// fallback when Title is empty. Version, TokensIn, TokensOut, and Cost carry
	// the session-level aggregates the harness records on the session row, so
	// metadata reports them without folding entries. They stay empty or zero when
	// the source does not expose them.
	Slug      string
	Version   string
	TokensIn  int
	TokensOut int
	Cost      float64
	// EventSeq is the session's newest event sequence, a monotonic per-session
	// counter that advances on every mutation, including an in-place rewrite that
	// moves no time column. Change detection re-ingests a session whose EventSeq
	// exceeds the last ingested value. It is zero when the source has no event
	// sequence to read.
	EventSeq          int64
	CWD               string            // Working directory when session ran (optional, for git resolution fallback)
	CreatedAt         time.Time         // Session creation time when known (zero means use ModTime)
	DiscoveryWarnings []DiagnosticEntry // Non-fatal relationship issues found before metadata extraction
	TranscriptOrigin  TranscriptOrigin  // Typed materialization contract; zero means copy SourcePath as a transcript file.
}

// stalenessSourceTime returns the source time the staleness (active) gate reads.
// It is the file or WAL mtime when the adapter recorded one separately from the
// changed clock, and otherwise the ModTime. An OpenCode SQLite winner reports a
// changed clock in ModTime and the raw file/WAL mtime in ActiveModTime, so a
// session still being written classifies active on its file mtime even when its
// changed clock is older.
func (s DiscoveredSession) stalenessSourceTime() time.Time {
	if !s.ActiveModTime.IsZero() {
		return s.ActiveModTime
	}
	return s.ModTime
}

// AdapterFactory creates a SourceAdapter with injected dependencies.
// The salt parameter is used by adapters to compute salted project hashes via
// DeriveProjectIdentifiers. Stub adapters in tests may ignore it.
type AdapterFactory func(fs FileSystem, git GitResolver, s salt.Salt) SourceAdapter

// DiscoveryDiagnostic reports one source location an adapter could not fully
// enumerate during discovery. Discovery stays non-fatal per location, so the
// run continues; this record makes the skipped location visible in the result
// rather than only in a log line.
type DiscoveryDiagnostic struct {
	Provider Harness
	Code     string
	Location string
	Summary  string
	Detail   string
}

// DiscoveryDiagnosticReporter is the optional capability an adapter implements
// to surface per-location discovery failures after Discover returns. The
// pipeline collects these into its result so a caller sees a database that was
// skipped, instead of a silently short session count.
type DiscoveryDiagnosticReporter interface {
	DiscoveryDiagnostics() []DiscoveryDiagnostic
}

// DefaultAdapterRegistry maps providers to their adapter factories.
var DefaultAdapterRegistry = map[Harness]AdapterFactory{
	HarnessClaudeCode: func(fs FileSystem, git GitResolver, s salt.Salt) SourceAdapter {
		return NewClaudeAdapter(fs, git, s)
	},
	HarnessOpenCode: func(fs FileSystem, git GitResolver, s salt.Salt) SourceAdapter {
		return NewOpenCodeAdapter(fs, git, s)
	},
	HarnessCodex: func(fs FileSystem, git GitResolver, s salt.Salt) SourceAdapter {
		return NewCodexAdapter(fs, git, s)
	},
	HarnessCursor: func(fs FileSystem, git GitResolver, s salt.Salt) SourceAdapter {
		return NewCursorAdapter(fs, git, s)
	},
	HarnessStrike: func(fs FileSystem, git GitResolver, s salt.Salt) SourceAdapter {
		return NewStrikeAdapter(fs, git, s)
	},
}
