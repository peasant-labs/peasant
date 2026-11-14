package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
)

// PiAdapter discovers normal Pi v3 JSONL recordings through the common pipeline.
type PiAdapter struct {
	discoveryProgressAttachment
	fs          FileSystem
	git         GitResolver
	salt        salt.Salt
	diagnostics []DiscoveryDiagnostic
}

var _ SourceAdapter = (*PiAdapter)(nil)
var _ DiscoveryProgressReporting = (*PiAdapter)(nil)
var _ DiscoveryDiagnosticReporter = (*PiAdapter)(nil)
var _ SessionLocator = (*PiAdapter)(nil)

func NewPiAdapter(fs FileSystem, git GitResolver, s salt.Salt) *PiAdapter {
	return &PiAdapter{fs: fs, git: git, salt: s}
}
func (a *PiAdapter) Harness() Harness { return HarnessPi }
func (a *PiAdapter) DiscoveryDiagnostics() []DiscoveryDiagnostic {
	return append([]DiscoveryDiagnostic(nil), a.diagnostics...)
}

// piSource is one candidate Pi recording. root is the configured source root it
// was enumerated under, so a member carries the same OriginalRoot whichever
// path found it. want is set only for a located source: it is the id the caller
// asked for, and the build refuses a recording whose header names another
// session instead of building a member for it.
type piSource struct {
	root ResolvedPath
	path string
	want SessionID
}

type piCandidate struct {
	session DiscoveredSession
	size    int64
}

func (a *PiAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	a.diagnostics = nil
	if !cfg.Enabled {
		discoveryTotal(a.progress, HarnessPi, 0)
		return nil, nil
	}
	var sources []piSource
	for _, root := range cfg.Paths {
		paths, err := a.candidatePaths(root.String())
		if err != nil {
			if !os.IsNotExist(err) {
				a.diagnose(root.String(), err)
			}
			continue
		}
		for _, path := range paths {
			sources = append(sources, piSource{root: root, path: path})
		}
	}
	discoveryTotal(a.progress, HarnessPi, len(sources))
	return a.buildPiSessions(ctx, sources, a.progress)
}

// buildPiSessions is the one construction path for Discover and
// DiscoverSessionAt. It keeps the newest copy of one header identity, so the
// full walk and a located source cannot describe a session two ways. A located
// source (want set) returns the failure instead of skipping it, because the
// core decides whether a stored session is unreadable and a selected one is a
// miss.
func (a *PiAdapter) buildPiSessions(ctx context.Context, sources []piSource, progress DiscoveryProgress) ([]DiscoveredSession, error) {
	winners := make(map[SessionID]piCandidate)
	for i, source := range sources {
		if i > 0 {
			discoveryAdvance(progress, HarnessPi)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := source.path
		// fail reports one source that could not be built: a located source
		// returns the error to the core, a walked candidate is a diagnostic and
		// the walk continues.
		fail := func(err error) error {
			if source.want != "" {
				return piLocatedSourceError(source, err)
			}
			a.diagnose(path, err)
			return nil
		}
		if resolver, ok := a.fs.(interface{ EvalSymlinks(string) (string, error) }); ok {
			canonical, err := resolver.EvalSymlinks(path)
			if err != nil {
				if failErr := fail(err); failErr != nil {
					return nil, failErr
				}
				continue
			}
			path = canonical
		}
		data, err := readPiSource(ctx, a.fs, path)
		if err != nil {
			if failErr := fail(err); failErr != nil {
				return nil, failErr
			}
			continue
		}
		doc, err := parsePiDocument(ctx, data)
		if err != nil {
			if failErr := fail(err); failErr != nil {
				return nil, failErr
			}
			continue
		}
		sid, err := NewSessionID(doc.header.ID)
		if err != nil {
			if failErr := fail(err); failErr != nil {
				return nil, failErr
			}
			continue
		}
		// A recording whose header names another session is not this session's
		// source. A moved or replaced file is gone for this id, never a member
		// carrying someone else's transcript.
		if source.want != "" && sid != source.want {
			return nil, piSourceGoneError(ResolvedPath(source.path), source.want, "the file header names another session")
		}
		// Validate the candidate before ordering it: a newer corrupt transcript
		// must never replace a valid older copy with the same header identity.
		if _, _, err := NewPiIndexer(a.fs).project(doc, sid); err != nil {
			if failErr := fail(err); failErr != nil {
				return nil, failErr
			}
			continue
		}
		info, err := a.fs.Stat(path)
		if err != nil {
			if failErr := fail(err); failErr != nil {
				return nil, failErr
			}
			continue
		}
		resolved, err := NewResolvedPath(filepath.Clean(path))
		if err != nil {
			if failErr := fail(err); failErr != nil {
				return nil, failErr
			}
			continue
		}
		created := info.ModTime()
		if timestamp := parseIndexTimestamp(doc.header.Timestamp); timestamp != nil {
			created = time.UnixMilli(*timestamp)
		}
		candidate := piCandidate{size: info.Size(), session: DiscoveredSession{SessionID: sid, Harness: HarnessPi, SourcePath: resolved, OriginalRoot: source.root, SourceFormat: SourceFormatJSONL, TranscriptOrigin: TranscriptOriginFile, CWD: doc.header.CWD, ProjectName: filepath.Base(doc.header.CWD), Title: doc.title, CreatedAt: created, ModTime: info.ModTime(), DiscoveryWarnings: doc.warnings}}
		previous, exists := winners[sid]
		if !exists || candidate.session.ModTime.After(previous.session.ModTime) || candidate.session.ModTime.Equal(previous.session.ModTime) && (candidate.size > previous.size || candidate.size == previous.size && candidate.session.SourcePath.String() < previous.session.SourcePath.String()) {
			winners[sid] = candidate
		}
	}
	if len(sources) > 0 {
		discoveryAdvance(progress, HarnessPi)
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

// LookupSessionSources reports that Pi cannot be searched by session id at all.
// A recording carries its id only in its first record's header, so no file name
// holds it and no bounded name pattern can answer the question. The limitation
// is a property of the format, not of a directory, so one entry states it and it
// is reported whether or not a root exists today.
//
// It reads nothing: no file is stat'd, listed, opened, or read, and no header is
// scanned. That is the point of the entry -- "no such session" and "this source
// cannot be searched that way" are different answers, and a run must never
// report the first after doing the second. The entry names no location because
// it searched none.
func (a *PiAdapter) LookupSessionSources(ctx context.Context, _ SourceConfig, id SessionID) (SourceLookup, error) {
	if err := ctx.Err(); err != nil {
		return SourceLookup{}, err
	}
	if _, err := NewSessionID(string(id)); err != nil {
		return SourceLookup{}, fmt.Errorf("ingest: PiAdapter.LookupSessionSources was given %q, which is not a session id; no Pi source can be searched under a name no session can hold; where: ingest.PiAdapter.LookupSessionSources in internal/ingest/pi.go; when: locating a named session by name; impact: no Pi source is searched; fix: pass the session id the user named: %w", id, err)
	}
	return SourceLookup{Searched: []SearchedSource{{
		Harness: HarnessPi,
		Method:  SearchUnsupported,
		Reason:  IDOnlyInFileHeader,
	}}}, nil
}

// DiscoverSessionAt builds the member for one located Pi recording. It re-reads
// the stored path and requires the header id to equal the requested id: a file
// whose header names another session is not this session's source, so it is
// gone for this id rather than a member carrying someone else's transcript.
//
// Pi records no parent and no children, so the located recording is the only
// member, at.Stored describes a family the Pi source cannot re-read, and there
// is no ancestor probe: the harness cannot derive where a parent is from a
// recording, and the absent probe costs zero I/O. The role and the outcome of
// the member are the core's, so this leaves both at zero.
func (a *PiAdapter) DiscoverSessionAt(ctx context.Context, cfg SourceConfig, at LocatedSource) (LocatedBuild, error) {
	if err := ctx.Err(); err != nil {
		return LocatedBuild{}, err
	}
	if at.ID == "" {
		return LocatedBuild{}, fmt.Errorf("ingest: PiAdapter.DiscoverSessionAt was given a located source with no session id at %q; a located source must name the session it holds so the member can be matched to its store row; where: ingest.PiAdapter.DiscoverSessionAt in internal/ingest/pi.go; when: building a located source; impact: nothing was read and no member was built; fix: pass the id the store or the lookup resolved", at.Path)
	}
	if _, err := NewSessionID(string(at.ID)); err != nil {
		return LocatedBuild{}, fmt.Errorf("ingest: PiAdapter.DiscoverSessionAt was given %q, which is not a session id; no Pi recording can hold a name no session can have; where: ingest.PiAdapter.DiscoverSessionAt in internal/ingest/pi.go; when: building a located source; impact: nothing was read and no member was built; fix: pass the id the store or the lookup resolved: %w", at.ID, err)
	}
	root, ok := piEnumeratedRoot(cfg, at.Path)
	if !ok {
		return LocatedBuild{}, fmt.Errorf("ingest: the located Pi path %q lies outside every configured Pi source root; a located source must be attributed to the root the full walk would have found it under, or the member would carry a different OriginalRoot than discovery gives it; where: ingest.PiAdapter.DiscoverSessionAt in internal/ingest/pi.go; when: resolving the source root of the located path; impact: nothing was read and no member was built; fix: restore the recording under a configured Pi source path, or add the path's root to the configured source paths", at.Path)
	}
	sessions, err := a.buildPiSessions(ctx, []piSource{{root: root, path: at.Path.String(), want: at.ID}}, nil)
	if err != nil {
		return LocatedBuild{}, err
	}
	if len(sessions) != 1 || sessions[0].SessionID != at.ID {
		return LocatedBuild{}, piSourceGoneError(at.Path, at.ID, "the recording did not build as this session")
	}
	member := LocatedMember{ID: at.ID, Harness: HarnessPi, Source: &sessions[0]}
	return LocatedBuild{Members: []LocatedMember{member}, Deferred: NoDeferredWrites{}}, nil
}

// piEnumeratedRoot returns the configured source root a located path hangs
// from, so a located member carries the same OriginalRoot a full walk gives it.
// A configured root may name one recording rather than a directory, which is why
// equality counts as well as containment.
func piEnumeratedRoot(cfg SourceConfig, path ResolvedPath) (ResolvedPath, bool) {
	for _, configured := range cfg.Paths {
		if configured == path {
			return configured, true
		}
		rel, err := filepath.Rel(string(configured), string(path))
		if err != nil || filepath.IsAbs(rel) {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return configured, true
	}
	return "", false
}

// piLocatedSourceError is the error a located build returns for a recording it
// could not build. A path that does not exist is gone, because the store's
// record no longer has a source; any other failure is returned as a read
// failure, which the core records as an unreadable stored session rather than a
// miss.
func piLocatedSourceError(source piSource, cause error) error {
	if errors.Is(cause, os.ErrNotExist) {
		return piSourceGoneError(ResolvedPath(source.path), source.want, "the file no longer exists")
	}
	return fmt.Errorf("ingest: the located Pi recording %q for session %s could not be read; the path exists but cannot be built as this session's source, so the member cannot be built from it; where: ingest.PiAdapter.DiscoverSessionAt in internal/ingest/pi.go; when: re-reading the path a previous discovery recorded; impact: no member was built from this path, and the core records the read failure; fix: make the recording readable and retry: %w", source.path, source.want, cause)
}

// piSourceGoneError is the one statement of "this recorded path no longer holds
// the session it was recorded for". It wraps ErrSourceGone, so the core treats
// the stored session as unreadable rather than processing a foreign transcript.
func piSourceGoneError(path ResolvedPath, id SessionID, because string) error {
	return fmt.Errorf("ingest: the located Pi recording %q no longer holds session %s because %s; a Pi recording carries its session id only in its file header, so a path whose header names another session is not this session's source; where: ingest.PiAdapter.DiscoverSessionAt in internal/ingest/pi.go; when: re-reading the path a previous discovery recorded; impact: no member was built from this path; fix: restore the recording, or run harvest once without --session so a moved or replaced one is discovered again: %w", path, id, because, ErrSourceGone)
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
	meta, err := piTranscriptMetadata(doc, session)
	if err != nil {
		return nil, err
	}
	remote, _ := a.git.RemoteURL(ctx, doc.header.CWD)
	branch := ResolveGitBranch(ctx, a.git, doc.header.CWD, session.Branch)
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
	return meta, nil
}

// piTranscriptMetadata reads only the captured document; native project and Git
// attribution stay in ExtractMetadata and are preserved during stored refresh.
func piTranscriptMetadata(doc piDocument, session DiscoveredSession) (*UnifiedMetadata, error) {
	rows, thinkingDiagnostics, err := NewPiIndexer(nil).project(doc, session.SessionID)
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
	// The projection maps every native observation exactly once and returns the
	// resulting diagnostics, so the extraction rules are not re-derived here.
	for _, diagnostic := range thinkingDiagnostics {
		appendThinkingLevelDiagnostic(&meta, diagnostic, 0)
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
		extra, _, err := DecodePiEntryExtra(row)
		if err != nil {
			return nil, piSourceError("PiAdapter.ExtractMetadata assistant evidence", 0, err)
		}
		// The publication preflight needs the session model as well as per-turn
		// observations. Use the first actual assistant observation, never state
		// changes or configuration, just as other file-backed harnesses do.
		if meta.Model == "" && extra.ModelID != "" {
			model, err := NewModelID(string(extra.ModelID))
			if err != nil {
				return nil, piSourceError("PiAdapter.ExtractMetadata model", 0, err)
			}
			meta.Model = model
		}
		// The governing level of each depth-0 assistant row is one ordered
		// observation: the first valid one seeds the session and later distinct
		// governing levels are recorded as in-session changes in source order.
		applyThinkingLevelObservation(&meta, thinkingLevelMapping{Level: extra.ThinkingLevel, Raw: extra.ThinkingLevelRaw})
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
	return &meta, nil
}
