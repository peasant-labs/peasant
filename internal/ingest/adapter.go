package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
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
	SessionID         SessionID
	Harness           Harness
	SourcePath        ResolvedPath      // Path to the main transcript file
	SourceFormat      SourceFormat      // "jsonl" for Claude, "json" for OpenCode
	OriginalRoot      ResolvedPath      // Harness root for multi-directory access (e.g. OpenCode message/part)
	ParentUUID        *SessionID        // nil for root sessions
	SubagentPaths     []ResolvedPath    // Child session transcript paths
	DebugPaths        []ResolvedPath    // Debug artifact paths
	ModTime           time.Time         // Last modification time of source file
	ProjectName       string            // Human-readable project name (optional, populated during discovery when cheap to extract)
	Title             string            // Session title (optional, populated when available without extra I/O)
	Branch            string            // Git branch active when session ran (optional, from session data not current repo)
	CWD               string            // Working directory when session ran (optional, for git resolution fallback)
	CreatedAt         time.Time         // Session creation time when known (zero means use ModTime)
	DiscoveryWarnings []DiagnosticEntry // Non-fatal relationship issues found before metadata extraction
	TranscriptOrigin  TranscriptOrigin  // Typed materialization contract; zero means copy SourcePath as a transcript file.
	// Origin is who drove the session, as the harness adapter's evidence decided
	// it. An adapter that mines no origin evidence leaves it empty, and the
	// consumer resolves that to the visible fail-safe value.
	Origin sessionorigin.Origin
}

// AdapterFactory creates a SourceAdapter with injected dependencies.
// The salt parameter is used by adapters to compute salted project hashes via
// DeriveProjectIdentifiers. Stub adapters in tests may ignore it.
type AdapterFactory func(fs FileSystem, git GitResolver, s salt.Salt) SourceAdapter

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
