// Package harnesslayout declares where coding tools that Peasant does not yet
// ingest keep their session metadata and transcripts, and captures the
// structure of those files from a real machine.
//
// A capture records typed session metadata and a value-free shape of every
// artifact: field paths, JSON types, and record-kind counts. It never records
// transcript text, so a shape-only report is safe to attach to a public issue
// when a maintainer builds the full ingest adapter.
//
// The package is deliberately independent of internal/ingest. A tool listed
// here is not a schema.Harness: promoting it to an ingested harness follows the
// bestiary and schema contract ceremony described in docs/bestiary.md.
package harnesslayout

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"sync"
	"time"

	"github.com/peasant-labs/schema"
)

// Tool identifies a coding tool whose storage layout is declared here. The
// constants live beside each tool's layout declaration.
type Tool string

// Format is the encoding of one artifact on disk.
type Format string

const (
	FormatJSONL    Format = Format(schema.SourceFormatJSONL)
	FormatJSON     Format = Format(schema.SourceFormatJSON)
	FormatSQLite   Format = "sqlite"
	FormatMarkdown Format = "markdown"
	FormatText     Format = "text"
)

// Role states what an artifact contributes to a session.
type Role string

const (
	RoleTranscript Role = "transcript"
	RoleMetadata   Role = "metadata"
	RoleIndex      Role = "index"
	RoleAuxiliary  Role = "auxiliary"
)

// OS names a GOOS value that a root applies to.
type OS string

const (
	OSLinux   OS = "linux"
	OSDarwin  OS = "darwin"
	OSWindows OS = "windows"
)

// Root is one default location of a tool's session store. Path may start
// with {home} or {config}; see Env.Resolve. An empty OS list applies to every
// operating system.
type Root struct {
	OS   []OS
	Path string
}

// Artifact describes one kind of file inside a root. Pattern is a
// slash-separated fs.Glob pattern relative to the root.
type Artifact struct {
	Name        string
	Pattern     string
	Format      Format
	Role        Role
	Description string
}

// Layout is the declared storage layout of one tool.
type Layout struct {
	Tool        Tool
	DisplayName string
	// Sessions states what one session is on disk, in one sentence.
	Sessions  string
	Roots     []Root
	Artifacts []Artifact
	// Sources lists the public references the layout was derived from.
	Sources []string
	Probe   Probe
}

// Source is one resolved root. FS is rooted at Dir. Dir is the operating
// system path and is empty for in-memory test sources; probes that must open
// a SQLite database need it.
type Source struct {
	FS  fs.FS
	Dir string
}

// SessionRef names one discovered session. Paths are relative to the source
// root and list every artifact that belongs to the session.
type SessionRef struct {
	ID    string
	Paths []string
}

// Probe discovers sessions in a source and captures their layout.
type Probe interface {
	Discover(ctx context.Context, src Source) ([]SessionRef, error)
	Capture(ctx context.Context, src Source, ref SessionRef) (Capture, error)
}

// Metadata is the harness-neutral session metadata a probe can recover.
// Zero values mean the tool did not record the field.
type Metadata struct {
	SessionID     string    `json:"sessionId,omitempty"`
	Title         string    `json:"title,omitempty"`
	ProjectPath   string    `json:"projectPath,omitempty"`
	StartedAt     time.Time `json:"startedAt,omitzero"`
	UpdatedAt     time.Time `json:"updatedAt,omitzero"`
	Models        []string  `json:"models,omitempty"`
	UserTurns     int       `json:"userTurns"`
	AssistantMsgs int       `json:"assistantMessages"`
	ToolCalls     int       `json:"toolCalls"`
}

// Capture is the result of probing one session.
type Capture struct {
	Tool     Tool            `json:"tool"`
	Session  string          `json:"session"`
	Metadata Metadata        `json:"metadata"`
	Shapes   []ArtifactShape `json:"artifacts"`
}

// WithoutMetadataValues returns the capture with every metadata value that
// could identify a user, a project, or a conversation removed. Counts, model
// identifiers, and the shapes stay.
func (c Capture) WithoutMetadataValues() Capture {
	c.Session = ""
	c.Metadata.SessionID = ""
	c.Metadata.Title = ""
	c.Metadata.ProjectPath = ""
	c.Metadata.StartedAt = time.Time{}
	c.Metadata.UpdatedAt = time.Time{}
	shapes := make([]ArtifactShape, len(c.Shapes))
	for i, shape := range c.Shapes {
		shape.Path = ""
		shapes[i] = shape
	}
	c.Shapes = shapes
	return c
}

var (
	registryMu sync.RWMutex
	registry   = map[Tool]Layout{}
)

// Register adds a layout. Each tool file calls it from init; a duplicate or
// incomplete declaration is a programming error and panics.
func Register(layout Layout) {
	if err := layout.validate(); err != nil {
		panic(err)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[layout.Tool]; exists {
		panic(fmt.Sprintf("harnesslayout: tool %q registered twice", layout.Tool))
	}
	registry[layout.Tool] = layout
}

// Layouts returns every registered layout ordered by tool.
func Layouts() []Layout {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Layout, 0, len(registry))
	for _, layout := range registry {
		out = append(out, layout)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out
}

// NewTool validates a raw tool name against the registered layouts.
func NewTool(raw string) (Tool, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	for tool := range registry {
		if string(tool) == raw {
			return tool, nil
		}
	}
	known := make([]string, 0, len(registry))
	for tool := range registry {
		known = append(known, string(tool))
	}
	sort.Strings(known)
	return "", fmt.Errorf("unknown layout tool %q (known: %v)", raw, known)
}

// Lookup returns the layout of a validated tool.
func Lookup(tool Tool) (Layout, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	layout, ok := registry[tool]
	return layout, ok
}

func (l Layout) validate() error {
	switch {
	case l.Tool == "":
		return fmt.Errorf("harnesslayout: layout without tool")
	case l.DisplayName == "":
		return fmt.Errorf("harnesslayout: %s: missing display name", l.Tool)
	case l.Sessions == "":
		return fmt.Errorf("harnesslayout: %s: missing session description", l.Tool)
	case len(l.Roots) == 0:
		return fmt.Errorf("harnesslayout: %s: no roots", l.Tool)
	case len(l.Artifacts) == 0:
		return fmt.Errorf("harnesslayout: %s: no artifacts", l.Tool)
	case len(l.Sources) == 0:
		return fmt.Errorf("harnesslayout: %s: no sources", l.Tool)
	case l.Probe == nil:
		return fmt.Errorf("harnesslayout: %s: no probe", l.Tool)
	}
	for _, artifact := range l.Artifacts {
		if _, err := fs.Glob(emptyFS{}, artifact.Pattern); err != nil {
			return fmt.Errorf("harnesslayout: %s: artifact %q: %w", l.Tool, artifact.Name, err)
		}
		if artifact.Name == "" || artifact.Format == "" || artifact.Role == "" {
			return fmt.Errorf("harnesslayout: %s: incomplete artifact %q", l.Tool, artifact.Pattern)
		}
	}
	return nil
}

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
