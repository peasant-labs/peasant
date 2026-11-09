package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
)

// CodexAdapter discovers and extracts metadata from OpenAI Codex CLI JSONL rollouts.
//
// On-disk layout (Codex CLI ≥ 0.133):
//
//	{root}/YYYY/MM/DD/rollout-{ISO-timestamp}-{uuid}.jsonl
//
// Each line is {"timestamp", "type", "payload"}. The first line is always
// "session_meta" and carries id, cwd, model_provider, and an optional git
// context. Subsequent lines mix "turn_context" (model + effort per turn),
// "response_item" (the OpenAI Responses API stream), and "event_msg" (UI
// events including the periodic "token_count" totals).
type CodexAdapter struct {
	fs          FileSystem
	git         GitResolver
	salt        salt.Salt
	windowNow   func() time.Time
	diagnostics []DiscoveryDiagnostic

	// rolloutCatalog is the derived rollout catalog the full walk keeps
	// current. A nil value leaves the walk writing nothing, which is the
	// store-free run's and every test's default.
	rolloutCatalog CodexRolloutCatalogStore
}

var _ SourceAdapter = (*CodexAdapter)(nil)
var _ SessionLocator = (*CodexAdapter)(nil)

// CodexAdapterOption injects one optional CodexAdapter dependency. Production
// constructs the adapter without options; a test fixes what would otherwise
// come from the wall clock.
type CodexAdapterOption func(*CodexAdapter)

// WithCodexWindowClock fixes the clock the by-name date window is anchored to.
// The window is the anchor's own day and the day before it, so a test that
// names the anchor makes the two listed folders — and the whole lookup — the
// same on every run. A nil clock keeps the wall clock.
func WithCodexWindowClock(now func() time.Time) CodexAdapterOption {
	return func(a *CodexAdapter) {
		if now != nil {
			a.windowNow = now
		}
	}
}

// NewCodexAdapter creates a CodexAdapter with injected dependencies.
func NewCodexAdapter(fs FileSystem, git GitResolver, s salt.Salt, options ...CodexAdapterOption) *CodexAdapter {
	adapter := &CodexAdapter{fs: fs, git: git, salt: s}
	for _, option := range options {
		option(adapter)
	}
	return adapter
}

// Harness returns HarnessCodex.
func (a *CodexAdapter) Harness() Harness {
	return HarnessCodex
}

// windowAnchor is the day the by-name date window is measured from.
func (a *CodexAdapter) windowAnchor() time.Time {
	if a.windowNow != nil {
		return a.windowNow()
	}
	return time.Now()
}

// codexRolloutFilePrefix identifies rollout files in the Codex sessions tree.
const codexRolloutFilePrefix = "rollout-"

// codexAncestorProbeKind is the one ancestor-probe kind the Codex adapter
// declares. A Codex rollout names its parent in session_meta, and the layout
// fixes where that parent's file can be, so an unstored parent costs one
// catalogued row and then at most the two date folders this adapter's window
// names — never a walk of the sessions tree. The kind is the adapter's
// property: it is declared here, beside the probe that carries it.
const codexAncestorProbeKind = ProbeCatalogWindow

// Discover walks each configured source path looking for rollout-*.jsonl files
// nested four segments deep (YYYY/MM/DD/file). Symlinks and other files are
// silently skipped. The session ID is the trailing UUID embedded in the
// filename; ExtractMetadata cross-checks it against the session_meta payload.
//
// Every discovered session comes from buildCodexSession, the same build a
// located source uses, so a file gets one stat, one read, and one
// DiscoveredSession value however it was found.
//
// For each rollout we also populate DiscoveredSession.Title/ProjectName/Branch/
// CWD so the FTUE wizard and `peasant ingest --verbose` can render meaningful
// rows. Titles come from sibling session_index.jsonl when present; project,
// branch, and cwd come from the rollout's first line (session_meta).
func (a *CodexAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	a.diagnostics = nil
	var sessions []DiscoveredSession
	var enumerated []RolloutFile

	for _, basePath := range cfg.Paths {
		// Read sibling session_index.jsonl once per root. Missing/corrupt index
		// = no titles (graceful degradation; wizard falls back to date).
		titles := map[ResolvedPath]map[string]string{
			basePath: a.loadCodexSessionTitles(string(basePath)),
		}
		sources, err := a.enumerateCodexRollouts(ctx, basePath)
		if err != nil {
			return nil, err
		}
		// Every enumerated path is catalogued, not only the ones that survive
		// the build: the catalog answers the duplicate and parent questions,
		// and both are about rollout files, not about stored sessions.
		enumerated = append(enumerated, codexRolloutFiles(sources)...)
		sessions = append(sessions, a.buildCodexSessions(sources, titles)...)
	}

	a.maintainCodexRolloutCatalog(ctx, cfg.Paths, enumerated)

	return sessions, nil
}

// codexRolloutSource is one rollout file a source root holds, as enumeration
// names it: the path and the root it was found under. It carries no read data;
// buildCodexSession reads the file.
type codexRolloutSource struct {
	path ResolvedPath
	root ResolvedPath
}

// enumerateCodexRollouts lists the rollout files one configured root holds, in
// walk order. It opens no file: every listed source is read once, by
// buildCodexSession.
func (a *CodexAdapter) enumerateCodexRollouts(ctx context.Context, root ResolvedPath) ([]codexRolloutSource, error) {
	var sources []codexRolloutSource

	walkErr := a.fs.WalkDir(string(root), func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// Keep walking, but name a directory permissions hid so the
			// run does not just report fewer sessions.
			a.noteUnreadable(path, err)
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, defaults.ExtJSONL.String()) {
			return nil
		}

		rel, relErr := filepath.Rel(string(root), path)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))

		// Expect YYYY/MM/DD/rollout-...-{uuid}.jsonl — depth 4.
		if len(parts) != 4 {
			return nil
		}
		if _, ok := codexSessionIDFromFilename(parts[3]); !ok {
			return nil
		}

		sources = append(sources, codexRolloutSource{path: ResolvedPath(path), root: root})
		return nil
	})

	if walkErr != nil {
		return nil, fmt.Errorf("CodexAdapter.Discover: walk %q: %w", root, walkErr)
	}
	return sources, nil
}

// codexSessionBuild is one rollout file after the shared build.
type codexSessionBuild struct {
	// Session is the discovered member both discovery paths use.
	Session DiscoveredSession
	// ParentID is the parent the rollout's session_meta names, set only when
	// the caller asked for it; empty otherwise, or when the rollout names no
	// usable session id.
	ParentID SessionID
	// ReadError is the failure to read the rollout for its hints. A file that
	// cannot be read is still a discovered session with no hints, so the walk
	// keeps the member; the located-source path reports the error instead,
	// because it told the store the path was readable.
	ReadError error
}

// codexBuildNeed says what a caller must get back beyond the discovered
// session, so a fact nobody reads is never parsed.
type codexBuildNeed uint8

const (
	// codexBuildNeedUnset is the invalid zero. It is unreachable rather than
	// refusable: buildCodexSession is unexported and both of its callers name a
	// declared value, so no caller can ask for a build without saying whether it
	// needs the parent.
	codexBuildNeedUnset codexBuildNeed = iota
	// codexBuildSessionOnly is the full walk, which does not carry the parent a
	// rollout names, so it never parses for it.
	codexBuildSessionOnly
	// codexBuildWithParent is a located source, whose ancestor probe needs the
	// parent its session_meta names.
	codexBuildWithParent
)

// needsParent reports whether this build must return the parent the rollout
// names.
func (n codexBuildNeed) needsParent() bool { return n == codexBuildWithParent }

// buildCodexSessions turns enumerated rollout files into discovered sessions.
// A file this build cannot name or stat yields no member: a source that cannot
// be identified cannot be processed.
func (a *CodexAdapter) buildCodexSessions(sources []codexRolloutSource, titles map[ResolvedPath]map[string]string) []DiscoveredSession {
	sessions := make([]DiscoveredSession, 0, len(sources))
	for _, source := range sources {
		build, err := a.buildCodexSession(source, titles[source.root], codexBuildSessionOnly)
		if err != nil {
			continue
		}
		sessions = append(sessions, build.Session)
	}
	return sessions
}

// buildCodexSession turns one rollout file into a discovered session. It is the
// one place the DiscoveredSession literal is written and the one place a rollout
// file is read for its session_meta hints, so the full walk and the
// located-source path cannot drift apart.
func (a *CodexAdapter) buildCodexSession(source codexRolloutSource, titles map[string]string, need codexBuildNeed) (codexSessionBuild, error) {
	rawID, ok := codexSessionIDFromFilename(filepath.Base(string(source.path)))
	if !ok {
		return codexSessionBuild{}, fmt.Errorf(
			"ingest: the Codex rollout file name %q read from %q carries no session id; a rollout is named rollout-{timestamp}-{uuid}.jsonl, so this build cannot tell which session the file holds; where: ingest.CodexAdapter.buildCodexSession in internal/ingest/codex.go; when: building a discovered session; impact: no member is built for the file; fix: restore the rollout under its original name or remove it, then rerun the harvest",
			filepath.Base(string(source.path)), source.path)
	}
	sessionID, err := NewSessionID(rawID)
	if err != nil {
		return codexSessionBuild{}, fmt.Errorf("CodexAdapter.buildCodexSession: session id %q in %q: %w", rawID, source.path, err)
	}

	info, err := a.fs.Stat(string(source.path))
	if err != nil {
		return codexSessionBuild{}, fmt.Errorf("CodexAdapter.buildCodexSession: stat %q: %w", source.path, err)
	}

	data, readErr := a.fs.ReadFile(string(source.path))
	hints := codexSessionHints{}
	parentID := SessionID("")
	if readErr == nil {
		hints = codexHintsFromRollout(data)
		if need.needsParent() {
			parentID = codexParentThreadID(data)
		}
	}

	return codexSessionBuild{
		Session: DiscoveredSession{
			SessionID:     sessionID,
			Harness:       HarnessCodex,
			SourcePath:    source.path,
			SourceFormat:  SourceFormatJSONL,
			OriginalRoot:  source.root,
			SubagentPaths: []ResolvedPath{},
			DebugPaths:    []ResolvedPath{},
			ModTime:       info.ModTime(),
			Title:         titles[rawID],
			ProjectName:   hints.projectName,
			Branch:        hints.branch,
			CWD:           hints.cwd,
		},
		ParentID:  parentID,
		ReadError: readErr,
	}, nil
}

// LookupSessionSources finds the rollout files that can hold id by name: the
// two date folders a rollout of that session can be in, and nothing else. It
// never walks and never reads a transcript. Each distinct configured root is
// listed once, however the roots are written: a root named twice would claim
// the same rollout twice, and a second claim on one path reads downstream as a
// duplicate session.
//
// The window is the lookup clock's own day and the day before it: a rollout is
// written into the folder of the day its session started, so a lookup that runs
// after midnight still finds a rollout that began before it. No third folder is
// listed, however old the id is; a rollout outside the window is not found by
// this lookup.
//
// A failed lookup is recorded ONCE, by the core: on a returned error the core
// appends the SearchFailed entry and carries the miss, so this method never
// appends one itself. It returns the entries it gathered for the roots it
// completed, so the miss still names where else the id was searched. Every
// adapter's lookup obeys the same convention.
func (a *CodexAdapter) LookupSessionSources(ctx context.Context, cfg SourceConfig, id SessionID) (SourceLookup, error) {
	if err := ctx.Err(); err != nil {
		return SourceLookup{}, err
	}
	if id == "" {
		return SourceLookup{}, fmt.Errorf(
			"ingest: CodexAdapter.LookupSessionSources was given an empty session id; an empty id can never match a rollout file, so the lookup could only ever report a miss; where: ingest.CodexAdapter.LookupSessionSources in internal/ingest/codex.go; when: locating a named session by name; impact: no Codex source is searched; fix: pass the session id the user named")
	}

	anchor := a.windowAnchor()
	var lookup SourceLookup
	listed := make(map[ResolvedPath]struct{}, len(cfg.Paths))

	for _, root := range cfg.Paths {
		if _, duplicate := listed[root]; duplicate {
			continue
		}
		listed[root] = struct{}{}

		folders := codexRolloutWindow(root, anchor)
		for _, folder := range folders {
			entries, err := a.fs.ReadDir(string(folder))
			if err != nil {
				// A folder that does not exist holds no rollout. Every other
				// failure is a lookup failure, which the core records; this
				// lookup keeps the entries it already collected.
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return lookup, fmt.Errorf("CodexAdapter.LookupSessionSources: list %q: %w", folder, err)
			}
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				if nameID, ok := codexSessionIDFromFilename(entry.Name()); !ok || nameID != string(id) {
					continue
				}
				lookup.Sources = append(lookup.Sources, ResolvedPath(filepath.Join(string(folder), entry.Name())))
			}
		}
		lookup.Searched = append(lookup.Searched, SearchedSource{
			Harness:  HarnessCodex,
			Method:   SearchNamePattern,
			Location: codexSearchLocation(root, id),
		})
	}

	slices.Sort(lookup.Sources)
	return lookup, nil
}

// codexRolloutWindow returns the two date folders a rollout can be in: the
// anchor's own day and the day before it. Two folders cover a session that
// began before midnight; there is never a third, and the tree is never walked.
func codexRolloutWindow(root ResolvedPath, anchor time.Time) [2]ResolvedPath {
	return [2]ResolvedPath{
		codexDayFolder(root, anchor),
		codexDayFolder(root, anchor.AddDate(0, 0, -1)),
	}
}

// codexDayFolder is the {root}/YYYY/MM/DD folder a rollout written on day lives
// in.
func codexDayFolder(root ResolvedPath, day time.Time) ResolvedPath {
	return ResolvedPath(filepath.Join(string(root), day.Format("2006"), day.Format("01"), day.Format("02")))
}

// codexSearchLocation is the pattern one configured root was searched with, for
// the miss error's searched block: the day folders the layout nests under the
// root, then the rollout file name a session's id appears in. The block renders
// one line per source and composes it from this pattern alone, so it must not
// carry a resolved folder or a file that happens to exist.
func codexSearchLocation(root ResolvedPath, id SessionID) string {
	return filepath.Join(
		codexDisplayPath(root), "*", "*", "*",
		codexRolloutFilePrefix+"*-"+string(id)+defaults.ExtJSONL.String(),
	)
}

// codexDisplayPath renders a source path with the home directory shortened, the
// way the searched block shows every location.
func codexDisplayPath(path ResolvedPath) string {
	raw := string(path)
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(raw, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(raw, home)
	}
	return raw
}

// DiscoverSessionAt builds the members one located Codex source carries: the
// source itself, every stored child the store records, and the one ancestor
// probe its session_meta names. It reads only what it builds and writes
// nothing: its deferred writes are empty.
//
// A Codex source lays no related session out beside itself, so there are no
// layout relatives to enumerate, and a rollout names its parent rather than its
// children, so the store's recorded children are the only children there are.
// That is why related members come from at.Stored alone.
func (a *CodexAdapter) DiscoverSessionAt(ctx context.Context, cfg SourceConfig, at LocatedSource) (LocatedBuild, error) {
	if err := ctx.Err(); err != nil {
		return LocatedBuild{}, err
	}
	if at.ID == "" {
		return LocatedBuild{}, fmt.Errorf(
			"ingest: CodexAdapter.DiscoverSessionAt was given a located source with no session id at %q; a located source must name the session it holds so the member can be matched to the store row; where: ingest.CodexAdapter.DiscoverSessionAt in internal/ingest/codex.go; when: building a located source; impact: nothing was read and no member was built; fix: pass the id the store or the lookup resolved",
			at.Path)
	}
	if nameID, ok := codexSessionIDFromFilename(filepath.Base(string(at.Path))); !ok || nameID != string(at.ID) {
		return LocatedBuild{}, codexSourceGoneError(at.Path, at.ID)
	}

	root, ok := codexEnumeratedRoot(cfg, at.Path)
	if !ok {
		return LocatedBuild{}, fmt.Errorf(
			"ingest: the located Codex path %q lies outside every configured Codex source root; a located source must be attributed to the root the full walk would have found it under, or the member would carry a different OriginalRoot than discovery gives it; where: ingest.CodexAdapter.DiscoverSessionAt in internal/ingest/codex.go; when: resolving the source root of the located path; impact: nothing was read and no member was built; fix: restore the recorded source under a configured Codex source path, or update the configured paths so the recorded location is inside one",
			at.Path)
	}

	// One session_index.jsonl read per distinct root, shared with the stored
	// children, so a child under the same root cannot cost a second read.
	titles := map[ResolvedPath]map[string]string{
		root: a.loadCodexSessionTitles(string(root)),
	}
	titlesFor := func(childRoot ResolvedPath) map[string]string {
		if cached, ok := titles[childRoot]; ok {
			return cached
		}
		loaded := a.loadCodexSessionTitles(string(childRoot))
		titles[childRoot] = loaded
		return loaded
	}

	build, err := a.buildCodexSession(codexRolloutSource{path: at.Path, root: root}, titlesFor(root), codexBuildWithParent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return LocatedBuild{}, codexSourceGoneError(at.Path, at.ID)
		}
		return LocatedBuild{}, fmt.Errorf("CodexAdapter.DiscoverSessionAt: build %q: %w", at.Path, err)
	}
	if build.ReadError != nil {
		return LocatedBuild{}, fmt.Errorf("CodexAdapter.DiscoverSessionAt: read %q: %w", at.Path, build.ReadError)
	}

	members := make([]LocatedMember, 0, 1+len(at.Stored.Children))
	members = append(members, LocatedMember{
		ID:          at.ID,
		Harness:     HarnessCodex,
		Source:      &build.Session,
		ParentProbe: codexAncestorProbe(build, at.Stored.Parent),
	})
	for _, child := range at.Stored.Children {
		members = append(members, a.codexStoredChildMember(cfg, child, titlesFor))
	}

	return LocatedBuild{Members: members, Deferred: NoDeferredWrites{}}, nil
}

// codexSourceGoneError is the one statement of "this recorded path no longer
// holds the session it was recorded for". It wraps ErrSourceGone, so the core
// takes the by-name lookup path for the id.
func codexSourceGoneError(path ResolvedPath, id SessionID) error {
	return fmt.Errorf(
		"ingest: the located Codex path %q no longer holds session %s; a Codex rollout carries its session id only in its file name, so a path that is gone, holds no rollout name, or holds a different id is not this session's source; where: ingest.CodexAdapter.DiscoverSessionAt in internal/ingest/codex.go; when: re-reading the path a previous discovery recorded; impact: no member was built from this path; fix: relocate the rollout, or let the by-name lookup find the source it is in now: %w",
		path, id, ErrSourceGone)
}

// codexAncestorProbe is the one probe a located Codex source declares for its
// unstored parent, or nil when there is nothing to locate.
func codexAncestorProbe(build codexSessionBuild, storedParent *SessionID) *AncestorProbe {
	if storedParent != nil {
		// The store already records the parent, so no ancestor has to be found.
		return nil
	}
	if build.ParentID == "" || build.ParentID == build.Session.SessionID {
		return nil
	}
	return &AncestorProbe{Kind: codexAncestorProbeKind, Harness: HarnessCodex, ID: build.ParentID}
}

// codexStoredChildMember builds one stored child of a located source. A child
// whose file is gone or unreadable comes back as a member with no source and
// its reason, never dropped: the stored transcript stays, and the store-side
// upkeep a full run does for the row still runs.
func (a *CodexAdapter) codexStoredChildMember(cfg SourceConfig, child StoredSessionSource, titlesFor func(ResolvedPath) map[string]string) LocatedMember {
	member := LocatedMember{ID: child.ID, Harness: child.Harness}

	if child.Harness != HarnessCodex {
		// A stored child of another harness is not a Codex source: this adapter
		// cannot read it, and the child's own adapter would. That is a local
		// read failure the run continues past — the row keeps its stored
		// transcript and the store-side upkeep a full run gives it — not a gone
		// source, because the file may be exactly where the store says, and not
		// a disabled harness, because the harness was never the reason.
		reason := "a source with no recorded harness cannot be read by the codex adapter"
		if child.Harness != "" {
			reason = fmt.Sprintf("a %s source cannot be read by the codex adapter", child.Harness)
		}
		member.Unreadable = UnreadableSourceFailed
		member.Detail = firstLineOr(reason, "the source cannot be read by this adapter")
		return member
	}
	if nameID, ok := codexSessionIDFromFilename(filepath.Base(string(child.Path))); !ok || nameID != string(child.ID) {
		member.Unreadable = UnreadableSourceGone
		return member
	}
	root, ok := codexEnumeratedRoot(cfg, child.Path)
	if !ok {
		member.Unreadable = UnreadableSourceFailed
		member.Detail = firstLineOr("the recorded path is outside every configured codex source root", "the source cannot be attributed to a configured root")
		return member
	}

	build, err := a.buildCodexSession(codexRolloutSource{path: child.Path, root: root}, titlesFor(root), codexBuildSessionOnly)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			member.Unreadable = UnreadableSourceGone
			return member
		}
		member.Unreadable = UnreadableSourceFailed
		member.Detail = firstLineOr(err.Error(), "the source could not be read")
		return member
	}
	if build.ReadError != nil {
		member.Unreadable = UnreadableSourceFailed
		member.Detail = firstLineOr(build.ReadError.Error(), "the source could not be read")
		return member
	}

	member.Source = &build.Session
	return member
}

// codexEnumeratedRoot returns the configured source root that holds path,
// preferring the deepest one. A path can be inside more than one configured
// root — a user may configure both the Codex home and its sessions tree — but
// only the deepest root admits it at the four-segment depth the layout gives a
// rollout, so the deepest containing root is the one the full walk would have
// attributed it to.
func codexEnumeratedRoot(cfg SourceConfig, path ResolvedPath) (ResolvedPath, bool) {
	var best ResolvedPath
	found := false
	for _, root := range cfg.Paths {
		rel, err := filepath.Rel(string(root), string(path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if !found || len(root) > len(best) {
			best = root
			found = true
		}
	}
	return best, found
}

// codexSessionIndexFile is the filename of the sidecar index Codex CLI writes
// alongside the sessions tree (one level above the dated YYYY/MM/DD/ folders).
// It carries human-readable thread names per session id and is the cheapest
// source for the wizard's session-list rendering.
const codexSessionIndexFile = "session_index.jsonl"

// codexSessionIndexEntry is one line of session_index.jsonl.
type codexSessionIndexEntry struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
}

// loadCodexSessionTitles reads {root}/../session_index.jsonl and returns a
// session_id → thread_name map. Returns an empty (non-nil) map on any error;
// the index is opt-in and missing/corrupt = no titles, not a discovery
// failure.
func (a *CodexAdapter) loadCodexSessionTitles(root string) map[string]string {
	titles := map[string]string{}
	indexPath := filepath.Join(filepath.Dir(root), codexSessionIndexFile)

	data, err := a.fs.ReadFile(indexPath)
	if err != nil {
		return titles
	}

	scanner := newJSONLRecordScanner(data, defaults.MaxJSONLRecordBytes)

	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var entry codexSessionIndexEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue // skip individual bad lines without failing the whole index
		}
		if entry.ID != "" && entry.ThreadName != "" {
			titles[entry.ID] = entry.ThreadName
		}
	}
	return titles
}

// codexSessionHints holds metadata extracted from a rollout's session_meta line.
// All fields are best-effort; zero values are valid (caller renders fallbacks).
type codexSessionHints struct {
	cwd         string
	branch      string
	projectName string
}

// codexHintsFromRollout peeks the first non-empty line of a rollout and parses
// session_meta for cwd and git.branch. Project name is the basename of cwd.
// Returns zero hints on any parse error (the wizard will fall back to
// UUID/date display, identical to pre-hint behavior).
func codexHintsFromRollout(data []byte) codexSessionHints {
	scanner := newJSONLRecordScanner(data, defaults.MaxJSONLRecordBytes)

	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var env codexRolloutLine
		if err := json.Unmarshal(raw, &env); err != nil {
			return codexSessionHints{}
		}
		if env.Type != codexTypeSessionMeta {
			return codexSessionHints{}
		}
		var sm codexSessionMeta
		if err := json.Unmarshal(env.Payload, &sm); err != nil {
			return codexSessionHints{}
		}
		hints := codexSessionHints{cwd: sm.CWD}
		if sm.CWD != "" {
			hints.projectName = filepath.Base(sm.CWD)
		}
		if sm.Git != nil {
			hints.branch = sm.Git.Branch
		}
		return hints
	}
	return codexSessionHints{}
}

// codexParentThreadID reads the parent a rollout's session_meta names, through
// the same authority parse the native history uses, so a top-level
// parent_thread_id and the nested source.subagent.thread_spawn form resolve
// identically. A value that is not a session id names no usable parent.
func codexParentThreadID(data []byte) SessionID {
	authority, ok := parseCodexNativeAuthority(data)
	if !ok {
		return ""
	}
	raw := authority.nativeParentThreadID()
	if raw == "" {
		return ""
	}
	id, err := NewSessionID(raw)
	if err != nil {
		return ""
	}
	return id
}

// codexSessionIDFromFilename extracts the session id a rollout file name
// carries: the trailing UUID of rollout-{ISO-timestamp}-{uuid}.jsonl. The ISO
// timestamp uses hyphens for time separators
// (rollout-2026-05-29T21-14-24-{uuid}.jsonl), so the last five
// hyphen-delimited segments are the UUID (8-4-4-4-12). It reports false for a
// name that is not a rollout file, so the walk's admission test and the by-name
// window match read a file name exactly the same way.
func codexSessionIDFromFilename(filename string) (string, bool) {
	if !strings.HasPrefix(filename, codexRolloutFilePrefix) || !strings.HasSuffix(filename, defaults.ExtJSONL.String()) {
		return "", false
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(filename, codexRolloutFilePrefix), defaults.ExtJSONL.String())
	segments := strings.Split(stem, "-")
	if len(segments) < 5 {
		return "", false
	}
	tail := segments[len(segments)-5:]
	return strings.Join(tail, "-"), true
}

// ---------------------------------------------------------------------------
// Rollout line shapes
// ---------------------------------------------------------------------------

// codexRolloutLine is the outer envelope every rollout line shares.
type codexRolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexSessionMeta is the payload of a session_meta line. Captured once at
// session start.
//
// Source is intentionally json.RawMessage, not string: on a top-level CLI
// session it is a plain string ("cli"), but on a subagent/thread-spawn
// session (Codex CLI's spawned-agent feature) it is a nested object
// (`{"subagent":{"thread_spawn":{...}}}`). A strictly-typed string field
// here made json.Unmarshal return a *json.UnmarshalTypeError for THIS field
// alone, and the caller treated any non-nil error as "the whole session_meta
// failed to parse" — discarding the otherwise fully-populated struct
// (including a perfectly good CWD) and falling back to the rollout file's
// own containing directory as the project cwd. Subagent-spawned Codex sessions
// would then be attributed to their ~/.codex/sessions/YYYY/MM/DD folder instead
// of the real project.
// Source itself is never read (kept only so the field survives on the
// struct for future use/debugging without reintroducing the type mismatch).
type codexSessionMeta struct {
	ID            string           `json:"id"`
	Timestamp     string           `json:"timestamp"`
	CWD           string           `json:"cwd"`
	Originator    string           `json:"originator"`
	CLIVersion    string           `json:"cli_version"`
	Source        json.RawMessage  `json:"source"`
	ModelProvider string           `json:"model_provider"`
	Git           *codexGitContext `json:"git,omitempty"`
}

// codexGitContext mirrors the session_meta.payload.git block.
type codexGitContext struct {
	CommitHash    string `json:"commit_hash"`
	Branch        string `json:"branch"`
	RepositoryURL string `json:"repository_url"`
}

// codexTurnContext is the payload of a turn_context line. The model and
// effort can differ per turn; we capture them on the first turn_context and
// update if a later turn changes model (rare in practice).
type codexTurnContext struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
	TurnID string `json:"turn_id"`
}

// codexEventPayload is the payload of an event_msg line, discriminated by
// nested type. We only parse fields we care about (token_count totals and
// task_complete duration).
type codexEventPayload struct {
	Type        string          `json:"type"`
	Info        *codexEventInfo `json:"info,omitempty"`
	CompletedAt *int64          `json:"completed_at,omitempty"`
	DurationMs  *int64          `json:"duration_ms,omitempty"`
}

// codexEventInfo carries token-usage totals on token_count events.
type codexEventInfo struct {
	TotalTokenUsage *codexTokenUsage `json:"total_token_usage,omitempty"`
	LastTokenUsage  *codexTokenUsage `json:"last_token_usage,omitempty"`
}

// codexTokenUsage matches the OpenAI Responses API usage block.
type codexTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// codexResponseItem is the payload of a response_item line, discriminated by
// nested type (message, function_call, function_call_output, reasoning,
// custom_tool_call, custom_tool_call_output).
type codexResponseItem struct {
	Type string `json:"type"`
	Role string `json:"role,omitempty"`
}

// CodexRole is the wire-format role on Codex response_item.message payloads.
type CodexRole string

func (r CodexRole) String() string { return string(r) }

// Recognised payload.type values for response_item / event_msg dispatch.
const (
	codexTypeSessionMeta = "session_meta"
	codexTypeTurnContext = "turn_context"
	codexTypeEventMsg    = "event_msg"
	codexTypeResponse    = "response_item"

	codexResponseMessage       = "message"
	codexResponseAgentMessage  = "agent_message"
	codexResponseFunctionCall  = "function_call"
	codexResponseCustomCall    = "custom_tool_call"
	codexResponseReasoning     = "reasoning"
	codexResponseFunctionOut   = "function_call_output"
	codexResponseCustomCallOut = "custom_tool_call_output"

	codexRoleUser      CodexRole = "user"
	codexRoleAssistant CodexRole = "assistant"
	// codexRoleDeveloper is Codex's system-injected developer prompt (permissions,
	// AGENTS.md context). Distinct from user/assistant on the wire.
	codexRoleDeveloper CodexRole = "developer"

	codexEventTokenCount   = "token_count"
	codexEventTaskComplete = "task_complete"
)

// ExtractMetadata reads the rollout JSONL and builds UnifiedMetadata. The
// first line must be session_meta; later lines are scanned for the model
// (turn_context), turn/tool counts (response_item.message / function_call /
// custom_tool_call), and the running token-usage total (event_msg.token_count).
// The session's end timestamp is the last line's timestamp.
func (a *CodexAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	data, err := a.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("CodexAdapter.ExtractMetadata: read %q: %w", session.SourcePath, err)
	}

	meta := NewUnifiedMetadata()
	meta.SessionID = session.SessionID
	meta.ParentUUID = nil // Codex has no native subagent model.
	meta.ModelHarness = HarnessCodex
	meta.Source = SourceInfo{
		FilePath: string(session.SourcePath),
		Format:   SourceFormatJSONL,
	}

	sessionMeta, err := parseCodexTranscriptMetadata(ctx, data, &meta)
	if err != nil {
		return nil, err
	}

	// Git context — Codex captures it once in session_meta. When present, we
	// skip the GitResolver subprocess and derive identifiers directly from the
	// recorded remote. cwd from session_meta is the real project path (no slug
	// decoding needed, unlike Claude).
	cwd := ""
	var remoteURL string
	if sessionMeta != nil {
		cwd = sessionMeta.CWD
		if sessionMeta.Git != nil {
			remoteURL = sessionMeta.Git.RepositoryURL
			if sessionMeta.Git.Branch != "" {
				b := sessionMeta.Git.Branch
				meta.Git.Branch = &b
			}
			if remoteURL != "" {
				r := remoteURL
				meta.Git.Remote = &r
			}
		}
	}
	meta.CWD = cwd
	if cwd == "" {
		cwd = filepath.Dir(string(session.SourcePath))
	}
	var recordedBranch string
	if meta.Git.Branch != nil {
		recordedBranch = *meta.Git.Branch
	}
	remoteURL, tracking := ResolveGitRemote(ctx, a.git, cwd, recordedBranch, remoteURL)
	if remoteURL != "" {
		meta.Git.Remote = &remoteURL
	}
	if tracking != "" {
		meta.Git.Tracking = &tracking
	}

	projectHash, hostSlug, derErr := DeriveProjectIdentifiers(a.salt, remoteURL, cwd)
	if derErr != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "derive_identity_error",
			Location:    fmt.Sprintf("session %s", session.SessionID),
			Message:     fmt.Sprintf("failed to derive project identifiers: %v", derErr),
			Remediation: "Check the recorded git remote URL or working directory path.",
		})
	} else {
		meta.HostSlug = hostSlug
	}

	meta.Project = ProjectInfo{
		Hash:     projectHash,
		FilePath: cwd,
		Name:     filepath.Base(cwd),
	}

	return &meta, nil
}

func parseCodexTranscriptMetadata(ctx context.Context, data []byte, meta *UnifiedMetadata) (*codexSessionMeta, error) {
	var (
		sessionMeta    *codexSessionMeta
		lastTimestamp  string
		lastTotalUsage *codexTokenUsage
		turnCount      int
		toolCount      int
		gotSessionMeta bool
	)

	scanner := newJSONLRecordScanner(data, defaults.MaxJSONLRecordBytes)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The reader is the one source of the line number, so a diagnostic after
		// a record the reader passed over still names the physical line.
		lineNum := scanner.Line()
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}

		var env codexRolloutLine
		if err := json.Unmarshal(raw, &env); err != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "parse_error",
				Location:    fmt.Sprintf("line %d", lineNum),
				Message:     fmt.Sprintf("failed to parse rollout line: %v", err),
				Remediation: "Check the rollout file for corruption or truncation at the indicated line.",
			})
			continue
		}

		if env.Timestamp != "" {
			lastTimestamp = env.Timestamp
		}

		switch env.Type {
		case codexTypeSessionMeta:
			var sm codexSessionMeta
			if err := json.Unmarshal(env.Payload, &sm); err == nil {
				// Subagent rollouts can include a later parent envelope. Like
				// DecodeCodexThreadEvidence, only the first metadata envelope
				// owns this session's identity and context. Still decode every
				// later payload so malformed records keep their diagnostics.
				if !gotSessionMeta {
					if sm.ID != "" && sm.ID != meta.SessionID.String() {
						return nil, fmt.Errorf("Codex metadata capture for %s: session_meta.id disagrees with the discovered filename identity; no capture was written; restore the matching rollout and rerun peasant ingest", meta.SessionID)
					}
					sessionMeta = &sm
					gotSessionMeta = true
				}
			} else {
				meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
					ErrorType:   "parse_error",
					Location:    fmt.Sprintf("line %d", lineNum),
					Message:     fmt.Sprintf("failed to parse session_meta payload: %v", err),
					Remediation: "Verify the Codex CLI version that produced this rollout.",
				})
			}

		case codexTypeTurnContext:
			var tc codexTurnContext
			if err := json.Unmarshal(env.Payload, &tc); err == nil && tc.Model != "" {
				if mid, mErr := NewModelID(tc.Model); mErr == nil {
					meta.Model = mid
				}
			}
			// The first non-empty effort seeds the session thinking level;
			// later turn_context values are recorded as in-session changes.
			if tc.Effort != "" {
				mapping, diagnostic := mapCodexEffort(tc.Effort)
				appendThinkingLevelDiagnostic(meta, diagnostic, 0)
				applyThinkingLevelObservation(meta, mapping)
			}

		case codexTypeResponse:
			var ri codexResponseItem
			if err := json.Unmarshal(env.Payload, &ri); err != nil {
				continue
			}
			switch ri.Type {
			case codexResponseMessage:
				if role := CodexRole(ri.Role); role == codexRoleUser || role == codexRoleAssistant {
					turnCount++
				}
			case codexResponseFunctionCall, codexResponseCustomCall:
				toolCount++
			}

		case codexTypeEventMsg:
			var ev codexEventPayload
			if err := json.Unmarshal(env.Payload, &ev); err != nil {
				continue
			}
			if ev.Type == codexEventTokenCount && ev.Info != nil && ev.Info.TotalTokenUsage != nil {
				lastTotalUsage = ev.Info.TotalTokenUsage
			}
		}
	}

	if err := scanner.Err(); err != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "read_error",
			Location:    fmt.Sprintf("line %d", scanner.Line()),
			Message:     fmt.Sprintf("scanner error reading rollout: %v", err),
			Remediation: "Verify the rollout file is not corrupted or truncated.",
		})
	}

	if !gotSessionMeta {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "missing_session_meta",
			Location:    fmt.Sprintf("session %s", meta.SessionID),
			Message:     "rollout did not begin with a session_meta line",
			Remediation: "Rollout may be truncated or from an incompatible Codex CLI version.",
		})
	}

	if sessionMeta != nil {
		meta.Version = sessionMeta.CLIVersion
	}

	// Timestamps: prefer session_meta.timestamp for start; fall back to the
	// first line's envelope timestamp if session_meta is missing.
	startStr := ""
	if sessionMeta != nil {
		startStr = sessionMeta.Timestamp
	}
	startMs := parseTimestampMillis(startStr)
	endMs := parseTimestampMillis(lastTimestamp)
	ingested := time.Now().UnixMilli()
	meta.Timestamp = TimestampInfo{
		Start:    startMs,
		End:      endMs,
		Ingested: &ingested,
	}
	if startMs > 0 && endMs >= startMs {
		meta.Stats.DurationMs = endMs - startMs
	}

	// Stats.
	meta.Stats.TurnCount = turnCount
	meta.Stats.ToolCallCount = toolCount
	if lastTotalUsage != nil {
		meta.Stats.TokensIn = lastTotalUsage.InputTokens
		meta.Stats.TokensOut = lastTotalUsage.OutputTokens
		if lastTotalUsage.CachedInputTokens > 0 {
			v := lastTotalUsage.CachedInputTokens
			meta.Stats.CachedReadTokens = &v
		}
		if lastTotalUsage.ReasoningOutputTokens > 0 {
			v := lastTotalUsage.ReasoningOutputTokens
			meta.Stats.ThoughtTokens = &v
		}
	}

	return sessionMeta, nil
}

var _ DiscoveryDiagnosticReporter = (*CodexAdapter)(nil)

// DiscoveryDiagnostics names the source locations the last Discover could not read.
func (a *CodexAdapter) DiscoveryDiagnostics() []DiscoveryDiagnostic {
	return append([]DiscoveryDiagnostic(nil), a.diagnostics...)
}

func (a *CodexAdapter) noteUnreadable(path string, err error) {
	if d, ok := unreadableSourceDiagnostic(HarnessCodex, path, err); ok {
		a.diagnostics = append(a.diagnostics, d)
	}
}
