package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
)

// PiAdapter discovers normal Pi v3 JSONL recordings through the common pipeline.
type PiAdapter struct {
	fs          FileSystem
	git         GitResolver
	salt        salt.Salt
	diagnostics []DiscoveryDiagnostic
}

var _ SourceAdapter = (*PiAdapter)(nil)
var _ DiscoveryDiagnosticReporter = (*PiAdapter)(nil)

func NewPiAdapter(fs FileSystem, git GitResolver, s salt.Salt) *PiAdapter {
	return &PiAdapter{fs: fs, git: git, salt: s}
}
func (a *PiAdapter) Harness() Harness { return HarnessPi }
func (a *PiAdapter) DiscoveryDiagnostics() []DiscoveryDiagnostic {
	return append([]DiscoveryDiagnostic(nil), a.diagnostics...)
}

type piCandidate struct {
	session DiscoveredSession
	size    int64
}

func (a *PiAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	a.diagnostics = nil
	if !cfg.Enabled {
		return nil, nil
	}
	winners := make(map[SessionID]piCandidate)
	for _, root := range cfg.Paths {
		paths, err := a.candidatePaths(root.String())
		if err != nil {
			if !os.IsNotExist(err) {
				a.diagnose(root.String(), err)
			}
			continue
		}
		for _, path := range paths {
			if resolver, ok := a.fs.(interface{ EvalSymlinks(string) (string, error) }); ok {
				canonical, err := resolver.EvalSymlinks(path)
				if err != nil {
					a.diagnose(path, err)
					continue
				}
				path = canonical
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			data, err := readPiSource(ctx, a.fs, path)
			if err != nil {
				a.diagnose(path, err)
				continue
			}
			doc, err := parsePiDocument(ctx, data)
			if err != nil {
				a.diagnose(path, err)
				continue
			}
			sid, err := NewSessionID(doc.header.ID)
			if err != nil {
				a.diagnose(path, err)
				continue
			}
			// Validate the candidate before ordering it: a newer corrupt transcript
			// must never replace a valid older copy with the same header identity.
			if _, err := NewPiIndexer(a.fs).project(doc, sid); err != nil {
				a.diagnose(path, err)
				continue
			}
			info, err := a.fs.Stat(path)
			if err != nil {
				a.diagnose(path, err)
				continue
			}
			resolved, err := NewResolvedPath(filepath.Clean(path))
			if err != nil {
				a.diagnose(path, err)
				continue
			}
			created := info.ModTime()
			if timestamp := parseIndexTimestamp(doc.header.Timestamp); timestamp != nil {
				created = time.UnixMilli(*timestamp)
			}
			candidate := piCandidate{size: info.Size(), session: DiscoveredSession{SessionID: sid, Harness: HarnessPi, SourcePath: resolved, OriginalRoot: root, SourceFormat: SourceFormatJSONL, TranscriptOrigin: TranscriptOriginFile, CWD: doc.header.CWD, ProjectName: filepath.Base(doc.header.CWD), Title: doc.title, CreatedAt: created, ModTime: info.ModTime(), DiscoveryWarnings: doc.warnings}}
			previous, exists := winners[sid]
			if !exists || candidate.session.ModTime.After(previous.session.ModTime) || candidate.session.ModTime.Equal(previous.session.ModTime) && (candidate.size > previous.size || candidate.size == previous.size && candidate.session.SourcePath.String() < previous.session.SourcePath.String()) {
				winners[sid] = candidate
			}
		}
	}
	result := make([]DiscoveredSession, 0, len(winners))
	for _, candidate := range winners {
		result = append(result, candidate.session)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SessionID.String() < result[j].SessionID.String() })
	return result, nil
}

func (a *PiAdapter) diagnose(path string, err error) {
	a.diagnostics = append(a.diagnostics, DiscoveryDiagnostic{Provider: HarnessPi, Code: "pi_source_rejected", Location: path, Summary: "Pi source skipped during discovery", Detail: fmt.Sprintf("PiAdapter.Discover could not accept this source: %v; other sources continue and prior stored sessions remain; restore a readable v3 recording and retry harvest", err)})
}

func (a *PiAdapter) candidatePaths(root string) ([]string, error) {
	info, err := a.fs.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if filepath.Ext(root) == defaults.ExtJSONL.String() {
			return []string{root}, nil
		}
		return nil, nil
	}
	children, err := a.fs.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, child := range children {
		path := filepath.Join(root, child.Name())
		info, err := a.fs.Stat(path) // Follow first-level project symlinks, never recursively walk them.
		if err != nil {
			a.diagnose(path, err)
			continue
		}
		if !info.IsDir() {
			if filepath.Ext(path) == defaults.ExtJSONL.String() {
				paths = append(paths, path)
			}
			continue
		}
		files, err := a.fs.ReadDir(path)
		if err != nil {
			a.diagnose(path, err)
			continue
		}
		for _, file := range files {
			if !file.IsDir() && filepath.Ext(file.Name()) == defaults.ExtJSONL.String() {
				paths = append(paths, filepath.Join(path, file.Name()))
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (a *PiAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	data, err := readPiSource(ctx, a.fs, session.SourcePath.String())
	if err != nil {
		return nil, err
	}
	doc, err := parsePiDocument(ctx, data)
	if err != nil {
		return nil, err
	}
	if doc.header.ID != session.SessionID.String() {
		return nil, piSourceError("PiAdapter.ExtractMetadata", 0, fmt.Errorf("header identity changed after discovery"))
	}
	rows, err := NewPiIndexer(a.fs).project(doc, session.SessionID)
	if err != nil {
		return nil, err
	}
	meta := NewUnifiedMetadata()
	meta.SessionID, meta.ModelHarness, meta.CWD, meta.Version = session.SessionID, HarnessPi, doc.header.CWD, "3"
	meta.Source = SourceInfo{FilePath: session.SourcePath.String(), Format: SourceFormatJSONL}
	meta.Diagnostics.Warnings = doc.warnings
	meta.Timestamp.Start, meta.Timestamp.End = session.CreatedAt.UnixMilli(), session.ModTime.UnixMilli()
	if timestamp := parseIndexTimestamp(doc.header.Timestamp); timestamp != nil {
		meta.Timestamp.Start = *timestamp
	}
	for _, entry := range doc.active {
		if timestamp := parseIndexTimestamp(entry.Timestamp); timestamp != nil && *timestamp > meta.Timestamp.End {
			meta.Timestamp.End = *timestamp
		}
	}
	if meta.Timestamp.End < meta.Timestamp.Start {
		meta.Timestamp.End = meta.Timestamp.Start
	}
	meta.Stats.DurationMs = meta.Timestamp.End - meta.Timestamp.Start
	for _, row := range rows {
		if IsPiCarrier(row) {
			continue
		}
		if row.Depth == 0 {
			meta.Stats.TurnCount++
		}
		if row.EntryType == EntryTypeToolUse {
			meta.Stats.ToolCallCount++
		}
		if row.Role != RoleAssistant || row.Depth != 0 {
			continue
		}
		// The publication preflight needs the session model as well as per-turn
		// observations. Use the first actual assistant observation, never state
		// changes or configuration, just as other file-backed harnesses do.
		if meta.Model == "" {
			extra, _, err := DecodePiEntryExtra(row)
			if err != nil {
				return nil, piSourceError("PiAdapter.ExtractMetadata model", 0, err)
			}
			if extra.ModelID != "" {
				model, err := NewModelID(string(extra.ModelID))
				if err != nil {
					return nil, piSourceError("PiAdapter.ExtractMetadata model", 0, err)
				}
				meta.Model = model
			}
		}
		const maxSafe = 9007199254740991
		if row.TokensIn != nil {
			if meta.Stats.TokensIn > maxSafe-*row.TokensIn {
				return nil, piSourceError("metadata usage", 0, fmt.Errorf("assistant input sum exceeds JS-safe range"))
			}
			meta.Stats.TokensIn += *row.TokensIn
		}
		if row.TokensOut != nil {
			if meta.Stats.TokensOut > maxSafe-*row.TokensOut {
				return nil, piSourceError("metadata usage", 0, fmt.Errorf("assistant output sum exceeds JS-safe range"))
			}
			meta.Stats.TokensOut += *row.TokensOut
		}
	}
	remote, _ := a.git.RemoteURL(ctx, doc.header.CWD)
	branch, _ := a.git.Branch(ctx, doc.header.CWD)
	worktree, _ := a.git.Worktree(ctx, doc.header.CWD)
	if worktree == "" {
		worktree = doc.header.CWD
	}
	if remote != "" {
		meta.Git.Remote = &remote
	}
	if branch != "" {
		meta.Git.Branch = &branch
	}
	meta.Git.Worktree = &worktree
	hash, host, err := DeriveProjectIdentifiersWithGit(ctx, a.salt, a.git, remote, doc.header.CWD)
	if err != nil {
		return nil, piSourceError("PiAdapter.ExtractMetadata project identity", 0, err)
	}
	meta.Project = ProjectInfo{Hash: hash, FilePath: doc.header.CWD, Name: filepath.Base(doc.header.CWD)}
	meta.HostSlug = host
	return &meta, nil
}
