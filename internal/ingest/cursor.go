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
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
)

const cursorAgentTranscriptsDir = "agent-transcripts"

// CursorAdapter discovers and extracts metadata from Cursor agent JSONL transcripts.
type CursorAdapter struct {
	fs   FileSystem
	git  GitResolver
	salt salt.Salt
}

var _ SourceAdapter = (*CursorAdapter)(nil)
var _ SessionLocator = (*CursorAdapter)(nil)

// NewCursorAdapter creates a CursorAdapter with injected dependencies.
func NewCursorAdapter(fs FileSystem, git GitResolver, s salt.Salt) *CursorAdapter {
	return &CursorAdapter{fs: fs, git: git, salt: s}
}

func (a *CursorAdapter) Harness() Harness {
	return HarnessCursor
}

type cursorJSONLLine struct {
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	Error      *string         `json:"error"`
	Role       string          `json:"role"`
	UUID       string          `json:"uuid"`
	ParentUUID string          `json:"parentUuid"`
	Timestamp  json.RawMessage `json:"timestamp"`
	StopReason string          `json:"stop_reason"`
	Message    struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message"`
	Content json.RawMessage `json:"content"`
}

func (line cursorJSONLLine) content() json.RawMessage {
	if len(line.Message.Content) > 0 {
		return line.Message.Content
	}
	return line.Content
}

type cursorContentBlock struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	ToolName  string          `json:"tool_name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Content   json.RawMessage `json:"content"`
	Input     json.RawMessage `json:"input"`
	IsError   bool            `json:"is_error"`
}

// cursorSourcePath is one native Cursor session file read from its path
// layout:
//
//	{root}/{workspace}/agent-transcripts/{id}/{id}.jsonl
//	{root}/{workspace}/agent-transcripts/{parent}/subagents/{id}.jsonl
//
// The path alone names the session, its workspace, its project root and, for a
// subagent, its parent, so one parser serves the walk, the by-name lookup and
// the derived ancestor probe.
type cursorSourcePath struct {
	Root      string
	Workspace string
	ID        SessionID
	ParentID  SessionID // empty for a root session
}

// parseCursorSourcePath reads a Cursor transcript path without touching the
// filesystem. It returns false for a path outside the two known layouts, so a
// caller never guesses that an unrelated file holds a session.
func parseCursorSourcePath(path string) (cursorSourcePath, bool) {
	clean := filepath.Clean(path)
	ext := defaults.ExtJSONL.String()
	base := filepath.Base(clean)
	if !strings.HasSuffix(base, ext) {
		return cursorSourcePath{}, false
	}
	id, err := NewSessionID(strings.TrimSuffix(base, ext))
	if err != nil {
		return cursorSourcePath{}, false
	}
	dir := filepath.Dir(clean)

	// Subagent session: {parent}/subagents/{id}.jsonl under agent-transcripts.
	if filepath.Base(dir) == defaults.DirSubagents.String() {
		parentDir := filepath.Dir(dir)
		transcriptsDir := filepath.Dir(parentDir)
		if filepath.Base(transcriptsDir) != cursorAgentTranscriptsDir {
			return cursorSourcePath{}, false
		}
		parentID, err := NewSessionID(filepath.Base(parentDir))
		if err != nil {
			return cursorSourcePath{}, false
		}
		workspaceDir := filepath.Dir(transcriptsDir)
		return cursorSourcePath{
			Root:      filepath.Dir(workspaceDir),
			Workspace: filepath.Base(workspaceDir),
			ID:        id,
			ParentID:  parentID,
		}, true
	}

	// Root session: {id}/{id}.jsonl under agent-transcripts.
	if filepath.Base(dir) != string(id) {
		return cursorSourcePath{}, false
	}
	transcriptsDir := filepath.Dir(dir)
	if filepath.Base(transcriptsDir) != cursorAgentTranscriptsDir {
		return cursorSourcePath{}, false
	}
	workspaceDir := filepath.Dir(transcriptsDir)
	return cursorSourcePath{
		Root:      filepath.Dir(workspaceDir),
		Workspace: filepath.Base(workspaceDir),
		ID:        id,
	}, true
}

// cursorSource is one native Cursor session file the enumeration found, before
// the shared build reads it.
type cursorSource struct {
	root      ResolvedPath
	workspace string
	id        SessionID
	path      string
	parent    *SessionID
	// info is the stat a caller already observed, so the build never stats one
	// path twice when the caller needed the result first.
	info os.FileInfo
}

// Discover walks configured Cursor IDE project roots looking for transcripts.
// With the default source root ~/.cursor/projects, Cursor IDE stores transcripts at:
//
//	~/.cursor/projects/{workspace}/agent-transcripts/{sessionId}/{sessionId}.jsonl
//	~/.cursor/projects/{workspace}/agent-transcripts/{parentId}/subagents/{sessionId}.jsonl
//
// It enumerates by walking each root and hands every file it finds to the same
// build DiscoverSessionAt uses, so the two paths cannot describe one session
// two ways.
func (a *CursorAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	var sessions []DiscoveredSession
	for _, basePath := range cfg.Paths {
		sources, err := a.enumerateCursorRoot(ctx, basePath)
		if err != nil {
			return nil, err
		}
		built, _ := a.buildCursorSessions(sources)
		sessions = append(sessions, built...)
	}
	return sessions, nil
}

// enumerateCursorRoot walks one configured root and returns the native Cursor
// session files it holds. It reads names only; the shared build reads each one.
// Roots are returned before their subagents, which is the order Discover's
// output has always had.
func (a *CursorAdapter) enumerateCursorRoot(ctx context.Context, root ResolvedPath) ([]cursorSource, error) {
	var roots, subagents []cursorSource
	rootText := string(root)
	walkErr := a.fs.WalkDir(rootText, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, defaults.ExtJSONL.String()) {
			return nil
		}
		parsed, ok := parseCursorSourcePath(path)
		if !ok || parsed.Root != rootText {
			return nil
		}
		source := cursorSource{root: root, workspace: parsed.Workspace, id: parsed.ID, path: path}
		if parsed.ParentID != "" {
			parent := parsed.ParentID
			source.parent = &parent
			subagents = append(subagents, source)
			return nil
		}
		roots = append(roots, source)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("cursor discover: walk %q: %w", rootText, walkErr)
	}
	return append(roots, subagents...), nil
}

// LookupSessionSources finds the transcript paths that can hold id BY NAME. It
// tries the two known layouts under each configured root and never walks a
// workspace: one directory read names the workspaces, one names the session
// directories, and one stat per candidate confirms the file.
func (a *CursorAdapter) LookupSessionSources(ctx context.Context, cfg SourceConfig, id SessionID) (SourceLookup, error) {
	if err := ctx.Err(); err != nil {
		return SourceLookup{}, err
	}
	if _, err := NewSessionID(string(id)); err != nil {
		return SourceLookup{}, fmt.Errorf("cursor lookup: %q is not a valid session id, so no source can be searched for it; where: ingest.CursorAdapter.LookupSessionSources; fix: pass a session id Cursor recorded; %w", id, err)
	}
	ext := defaults.ExtJSONL.String()
	var lookup SourceLookup
	for _, basePath := range cfg.Paths {
		if err := ctx.Err(); err != nil {
			return SourceLookup{}, err
		}
		root := string(basePath)
		transcriptsDirName := cursorAgentTranscriptsDir
		searchStart := len(lookup.Searched)
		lookup.Searched = append(lookup.Searched,
			SearchedSource{
				Harness:  HarnessCursor,
				Method:   SearchNamePattern,
				Location: cursorDisplayPath(filepath.Join(root, "*", transcriptsDirName, string(id), string(id)+ext)),
			},
			SearchedSource{
				Harness:  HarnessCursor,
				Method:   SearchNamePattern,
				Location: cursorDisplayPath(filepath.Join(root, "*", transcriptsDirName, "*", defaults.DirSubagents.String(), string(id)+ext)),
			},
		)
		failRoot := func(err error) (SourceLookup, error) {
			// Replace this root's two pattern entries with one failure entry:
			// a failed search is neither hidden nor doubled.
			lookup.Searched = append(lookup.Searched[:searchStart], cursorFailedSearch(root, err))
			return lookup, fmt.Errorf("cursor lookup: searching %q for session %q failed: %w; where: ingest.CursorAdapter.LookupSessionSources; when: reading the source directory or a candidate transcript path; impact: the session was not searched in this source, so the id may still be there and the miss text names the failure; fix: make the directory readable and retry", root, id, err)
		}
		workspaces, err := a.fs.ReadDir(root)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return failRoot(err)
		}

		transcriptDirs := make([]string, 0, len(workspaces))
		for _, workspace := range workspaces {
			if !workspace.IsDir() {
				continue
			}
			dir := filepath.Join(root, workspace.Name(), transcriptsDirName)
			transcriptDirs = append(transcriptDirs, dir)
			candidate := filepath.Join(dir, string(id), string(id)+ext)
			hit, err := a.cursorSourceExists(candidate)
			if err != nil {
				return failRoot(err)
			}
			if hit {
				resolved, err := NewResolvedPath(candidate)
				if err != nil {
					return lookup, fmt.Errorf("cursor lookup: resolve %q: %w", candidate, err)
				}
				lookup.Sources = append(lookup.Sources, resolved)
			}
		}
		for _, dir := range transcriptDirs {
			sessions, err := a.fs.ReadDir(dir)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return failRoot(err)
			}
			for _, session := range sessions {
				if !session.IsDir() {
					continue
				}
				candidate := filepath.Join(dir, session.Name(), defaults.DirSubagents.String(), string(id)+ext)
				hit, err := a.cursorSourceExists(candidate)
				if err != nil {
					return failRoot(err)
				}
				if hit {
					resolved, err := NewResolvedPath(candidate)
					if err != nil {
						return lookup, fmt.Errorf("cursor lookup: resolve %q: %w", candidate, err)
					}
					lookup.Sources = append(lookup.Sources, resolved)
				}
			}
		}
	}
	return lookup, nil
}

// cursorSourceExists reports whether path is a readable file. A missing path is
// not a failure: it is simply not a hit.
func (a *CursorAdapter) cursorSourceExists(path string) (bool, error) {
	info, err := a.fs.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir(), nil
}

// cursorFailedSearch records a lookup failure where a miss can name it.
func cursorFailedSearch(root string, err error) SearchedSource {
	return SearchedSource{
		Harness:  HarnessCursor,
		Method:   SearchFailed,
		Location: cursorDisplayPath(root),
		Detail:   firstLine(err.Error()),
	}
}

// DiscoverSessionAt builds the members for one located Cursor source. It reads
// the source, the subagents its layout names, and the stored children the core
// passed in; it never writes, so a refused run has nothing to undo. Stored
// children whose file is gone come back with no source and their reason rather
// than being dropped.
func (a *CursorAdapter) DiscoverSessionAt(ctx context.Context, cfg SourceConfig, at LocatedSource) (LocatedBuild, error) {
	if err := ctx.Err(); err != nil {
		return LocatedBuild{}, err
	}
	parsed, ok := parseCursorSourcePath(at.Path.String())
	if !ok || parsed.ID != at.ID {
		return LocatedBuild{}, fmt.Errorf("cursor: source %q does not hold session %q; the stored path is outside Cursor's two layouts or names another id; where: ingest.CursorAdapter.DiscoverSessionAt; fix: let the run look the session up by name and record it again; %w", at.Path, at.ID, ErrSourceGone)
	}
	info, err := a.fs.Stat(at.Path.String())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return LocatedBuild{}, fmt.Errorf("cursor: source %q no longer exists for session %q; where: ingest.CursorAdapter.DiscoverSessionAt; fix: let the run look the session up by name; %w", at.Path, at.ID, ErrSourceGone)
		}
		return LocatedBuild{}, fmt.Errorf("cursor: read source %q for session %q: %w", at.Path, at.ID, err)
	}
	if info.IsDir() {
		return LocatedBuild{}, fmt.Errorf("cursor: source %q is a directory, not a transcript for session %q; %w", at.Path, at.ID, ErrSourceGone)
	}
	root, err := NewResolvedPath(parsed.Root)
	if err != nil {
		return LocatedBuild{}, fmt.Errorf("cursor: source %q names project root %q, which is not an absolute path; %w", at.Path, parsed.Root, ErrSourceGone)
	}
	source := cursorSource{root: root, workspace: parsed.Workspace, id: at.ID, path: at.Path.String(), info: info}
	if parsed.ParentID != "" {
		parent := parsed.ParentID
		source.parent = &parent
	}
	sources := []cursorSource{source}
	if source.parent == nil {
		children, err := a.enumerateCursorSubagents(root, parsed.Workspace, at.ID)
		if err != nil {
			return LocatedBuild{}, err
		}
		sources = append(sources, children...)
	}
	missing, err := a.appendCursorStoredChildren(&sources, at)
	if err != nil {
		return LocatedBuild{}, err
	}
	sessions, built := a.buildCursorSessions(sources)
	members := make([]LocatedMember, 0, len(sources)+len(missing))
	for index, enumerated := range sources {
		builtIndex := built[index]
		if builtIndex < 0 {
			continue
		}
		member := LocatedMember{ID: enumerated.id, Harness: HarnessCursor, Source: &sessions[builtIndex], Role: CohortChild}
		if enumerated.id == at.ID {
			// The core knows why it located this source and sets the role.
			member.Role = cohortRoleUnset
			if at.Stored.Parent == nil {
				member.ParentProbe = cursorAncestorProbe(parsed)
			}
		}
		members = append(members, member)
	}
	members = append(members, missing...)
	return LocatedBuild{Members: members, Deferred: NoDeferredWrites{}}, nil
}

// enumerateCursorSubagents reads one root session's subagents directory. A
// missing directory is no children, not a failure; every other read error is
// returned so the core can name it.
func (a *CursorAdapter) enumerateCursorSubagents(root ResolvedPath, workspace string, parent SessionID) ([]cursorSource, error) {
	dir := filepath.Join(string(root), workspace, cursorAgentTranscriptsDir, string(parent), defaults.DirSubagents.String())
	entries, err := a.fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cursor: read subagents directory %q for session %q: %w", dir, parent, err)
	}
	ext := defaults.ExtJSONL.String()
	sources := make([]cursorSource, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ext) {
			continue
		}
		id, err := NewSessionID(strings.TrimSuffix(entry.Name(), ext))
		if err != nil {
			continue
		}
		parentID := parent
		sources = append(sources, cursorSource{
			root:      root,
			workspace: workspace,
			id:        id,
			path:      filepath.Join(dir, entry.Name()),
			parent:    &parentID,
		})
	}
	return sources, nil
}

// appendCursorStoredChildren adds the stored children the build must re-read.
// A stored child whose file is gone or is not a transcript comes back as an
// unreadable member, because the store still records it and the run's upkeep
// set must see it.
func (a *CursorAdapter) appendCursorStoredChildren(sources *[]cursorSource, at LocatedSource) ([]LocatedMember, error) {
	seen := make(map[SessionID]struct{}, len(*sources))
	for _, source := range *sources {
		seen[source.id] = struct{}{}
	}
	var missing []LocatedMember
	for _, child := range at.Stored.Children {
		if _, duplicate := seen[child.ID]; duplicate {
			continue
		}
		seen[child.ID] = struct{}{}
		parsed, ok := parseCursorSourcePath(child.Path.String())
		if !ok || parsed.ID != child.ID {
			missing = append(missing, cursorStoredChildUnreadable(child, UnreadableSourceGone))
			continue
		}
		info, err := a.fs.Stat(child.Path.String())
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				missing = append(missing, cursorStoredChildUnreadable(child, UnreadableSourceGone))
				continue
			}
			return nil, fmt.Errorf("cursor: read stored child %q for session %q: %w", child.Path, child.ID, err)
		}
		if info.IsDir() {
			missing = append(missing, cursorStoredChildUnreadable(child, UnreadableSourceGone))
			continue
		}
		root, err := NewResolvedPath(parsed.Root)
		if err != nil {
			missing = append(missing, cursorStoredChildUnreadable(child, UnreadableSourceGone))
			continue
		}
		source := cursorSource{root: root, workspace: parsed.Workspace, id: child.ID, path: child.Path.String(), info: info}
		if parsed.ParentID != "" {
			parent := parsed.ParentID
			source.parent = &parent
		}
		*sources = append(*sources, source)
	}
	return missing, nil
}

func cursorStoredChildUnreadable(child StoredSessionSource, reason UnreadableReason) LocatedMember {
	harness := child.Harness
	if harness == "" {
		harness = HarnessCursor
	}
	return LocatedMember{
		ID:         child.ID,
		Harness:    harness,
		Outcome:    LocateStoredUnreadable,
		Role:       CohortChild,
		Unreadable: reason,
	}
}

// cursorAncestorProbe derives one unstored parent's exact source file from the
// child's own path. It is a pure value: no stat, no directory read.
func cursorAncestorProbe(parsed cursorSourcePath) *AncestorProbe {
	if parsed.ParentID == "" {
		return nil
	}
	parentPath := filepath.Join(parsed.Root, parsed.Workspace, cursorAgentTranscriptsDir, string(parsed.ParentID), string(parsed.ParentID)+defaults.ExtJSONL.String())
	path, err := NewResolvedPath(parentPath)
	if err != nil {
		return nil
	}
	return &AncestorProbe{Kind: ProbePath, Harness: HarnessCursor, Path: path}
}

// buildCursorSessions is the one construction path for Discover and
// DiscoverSessionAt. It builds a session per source in source order and links
// each subagent's path onto its root's SubagentPaths, so a walk and a located
// source cannot describe the same session two ways. built[i] is the index in
// the returned sessions for sources[i], or -1 when the file was gone by the
// time the build read it.
func (a *CursorAdapter) buildCursorSessions(sources []cursorSource) ([]DiscoveredSession, []int) {
	built := make([]int, len(sources))
	rootIndex := make(map[SessionID]int, len(sources))
	sessions := make([]DiscoveredSession, 0, len(sources))
	for index, source := range sources {
		built[index] = -1
		session, ok := a.buildCursorSession(source)
		if !ok {
			continue
		}
		built[index] = len(sessions)
		sessions = append(sessions, session)
		if source.parent == nil {
			rootIndex[source.id] = len(sessions) - 1
		}
	}
	for index := range sessions {
		if sessions[index].ParentUUID == nil {
			continue
		}
		if root, ok := rootIndex[*sessions[index].ParentUUID]; ok {
			sessions[root].SubagentPaths = append(sessions[root].SubagentPaths, sessions[index].SourcePath)
		}
	}
	return sessions, built
}

// buildCursorSession constructs one discovered session. A root reads its cheap
// title hint; a subagent carries its parent instead, exactly as a full walk
// records them.
func (a *CursorAdapter) buildCursorSession(source cursorSource) (DiscoveredSession, bool) {
	info := source.info
	if info == nil {
		observed, err := a.fs.Stat(source.path)
		if err != nil {
			return DiscoveredSession{}, false
		}
		info = observed
	}
	resolved, err := NewResolvedPath(source.path)
	if err != nil {
		return DiscoveredSession{}, false
	}
	projectDir, projectName := a.decodeCursorWorkspace(string(source.root), source.workspace)
	session := DiscoveredSession{
		SessionID:     source.id,
		Harness:       HarnessCursor,
		SourcePath:    resolved,
		SourceFormat:  SourceFormatJSONL,
		OriginalRoot:  source.root,
		SubagentPaths: []ResolvedPath{},
		DebugPaths:    []ResolvedPath{},
		ModTime:       info.ModTime(),
		ProjectName:   projectName,
		CWD:           projectDir,
	}
	if source.parent != nil {
		parent := *source.parent
		session.ParentUUID = &parent
	} else {
		session.Title = a.extractCursorHints(source.path).title
	}
	return session, true
}

// cursorDisplayPath shortens a path under the user's home for a rendered
// searched location. It never holds transcript text.
func cursorDisplayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

type cursorSessionHints struct {
	title string
}

func (a *CursorAdapter) extractCursorHints(path string) cursorSessionHints {
	data, err := a.fs.ReadFile(path)
	if err != nil {
		return cursorSessionHints{}
	}
	scanner := newJSONLRecordScanner(data, defaults.MaxJSONLRecordBytes)
	for i := 0; i < 10 && scanner.Scan(); i++ {
		var line cursorJSONLLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if cursorLineRole(line) == RoleUser {
			if text := extractCursorText(line.content()); text != "" {
				return cursorSessionHints{title: simpleTitle(text, defaults.HarnessCursor)}
			}
		}
	}
	return cursorSessionHints{}
}

// ExtractMetadata reads the Cursor JSONL transcript and builds UnifiedMetadata.
func (a *CursorAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	data, err := a.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("cursor: read session %s: %w", session.SessionID, err)
	}

	meta := NewUnifiedMetadata()
	meta.SessionID = session.SessionID
	meta.ParentUUID = session.ParentUUID
	meta.ModelHarness = HarnessCursor
	meta.Source = SourceInfo{
		FilePath: string(session.SourcePath),
		Format:   SourceFormatJSONL,
	}

	startMs, endMs := parseCursorTranscriptMetadata(data, &meta)
	if startMs == 0 {
		if !session.CreatedAt.IsZero() {
			startMs = session.CreatedAt.UnixMilli()
		} else if !session.ModTime.IsZero() {
			startMs = session.ModTime.UnixMilli()
		}
	}
	if endMs == 0 && !session.ModTime.IsZero() {
		endMs = session.ModTime.UnixMilli()
	}
	if endMs < startMs {
		endMs = startMs
	}
	ingested := time.Now().UnixMilli()
	meta.Timestamp = TimestampInfo{
		Start:    startMs,
		End:      endMs,
		Ingested: &ingested,
	}
	if startMs > 0 && endMs >= startMs {
		meta.Stats.DurationMs = endMs - startMs
	}
	meta.Stats.SubagentCount = len(session.SubagentPaths)
	for _, sp := range session.SubagentPaths {
		subIDStr := strings.TrimSuffix(filepath.Base(string(sp)), defaults.ExtJSONL.String())
		subSID, err := NewSessionID(subIDStr)
		if err != nil {
			continue
		}
		meta.Subagents = append(meta.Subagents, SubagentRef{
			SessionID:  subSID,
			ParentUUID: session.SessionID,
		})
	}

	a.enrichCursorProject(ctx, &meta, session, session.CWD)
	return &meta, nil
}

func parseCursorTranscriptMetadata(data []byte, meta *UnifiedMetadata) (int64, int64) {
	var (
		firstTimestamp json.RawMessage
		lastTimestamp  json.RawMessage
		turnCount      int
		toolCount      int
		tokensIn       int
		tokensOut      int
	)

	scanner := newJSONLRecordScanner(data, defaults.MaxJSONLRecordBytes)
	for scanner.Scan() {
		// The reader is the one source of the line number, so a diagnostic after
		// a record the reader passed over still names the physical line.
		lineNum := scanner.Line()
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var line cursorJSONLLine
		if err := json.Unmarshal(raw, &line); err != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "parse_error",
				Location:    fmt.Sprintf("line %d", lineNum),
				Message:     fmt.Sprintf("failed to parse Cursor JSONL line: %v", err),
				Remediation: "Check the transcript file for corruption or truncation at the indicated line.",
			})
			continue
		}
		if len(line.Timestamp) > 0 && string(line.Timestamp) != "null" {
			if len(firstTimestamp) == 0 {
				firstTimestamp = append(json.RawMessage(nil), line.Timestamp...)
			}
			lastTimestamp = append(json.RawMessage(nil), line.Timestamp...)
		}
		role := cursorLineRole(line)
		if role == RoleUser || role == RoleAssistant {
			turnCount++
		}
		for _, block := range parseCursorContentBlocks(line.content()) {
			if block.Type == "tool_use" {
				toolCount++
			}
		}
		if meta.Model == "" && line.Message.Model != "" {
			if mid, err := NewModelID(line.Message.Model); err == nil {
				meta.Model = mid
			}
		}
		if line.Message.Usage != nil {
			tokensIn += line.Message.Usage.InputTokens
			tokensOut += line.Message.Usage.OutputTokens
		}
	}
	if err := scanner.Err(); err != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "read_error",
			Location:    fmt.Sprintf("line %d", scanner.Line()),
			Message:     fmt.Sprintf("scanner error reading Cursor transcript: %v", err),
			Remediation: "Verify the transcript file is not corrupted or truncated.",
		})
	}
	if meta.Model == "" {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "missing_model",
			Location:    fmt.Sprintf("session %s", meta.SessionID),
			Message:     "Cursor transcript omitted message.model; peasant will hold this session from village push because peasant online requires a recorded model.",
			Remediation: "Keep the transcript ingested for local analysis, but do not publish it until Cursor records model metadata or the source transcript is corrected.",
		})
	}

	meta.Stats.TurnCount = turnCount
	meta.Stats.ToolCallCount = toolCount
	meta.Stats.TokensIn = tokensIn
	meta.Stats.TokensOut = tokensOut
	return cursorTimestampMillis(firstTimestamp), cursorTimestampMillis(lastTimestamp)
}

func cursorTimestampMillis(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	if ts := parseIndexTimestamp(raw); ts != nil {
		return *ts
	}
	return 0
}

func (a *CursorAdapter) enrichCursorProject(ctx context.Context, meta *UnifiedMetadata, session DiscoveredSession, projectDir string) {
	if projectDir == "" {
		return
	}
	if a.git == nil {
		return
	}

	branchStr, branchErr := a.git.Branch(ctx, projectDir)
	branch := session.Branch
	if branch == "" && branchErr == nil {
		branch = branchStr
	}
	remoteURL, trackingStr := ResolveGitRemote(ctx, a.git, projectDir, branch, "")
	remoteErr := error(nil)
	// Walk up parent directories if direct remote lookup fails — handles cases where
	// projectDir decoded to a subdirectory of the actual repo root.
	if remoteErr != nil || remoteURL == "" {
		if walkedRemote, _, walkErr := a.git.WalkUpRemoteURL(ctx, projectDir); walkErr == nil && walkedRemote != "" {
			remoteURL = walkedRemote
			remoteErr = nil
		}
	}
	worktreeStr, worktreeErr := a.git.Worktree(ctx, projectDir)

	gitInfo := GitContext{}
	if branch != "" {
		gitInfo.Branch = &branch
	}
	if remoteErr == nil && remoteURL != "" {
		gitInfo.Remote = &remoteURL
	}
	if worktreeErr == nil && worktreeStr != "" {
		gitInfo.Worktree = &worktreeStr
	}
	if trackingStr != "" {
		gitInfo.Tracking = &trackingStr
	}
	meta.Git = gitInfo
	// A decoded workspace identifies project scope, not an exact session CWD.
	meta.CWD = ""

	projectPath := worktreeStr
	if projectPath == "" {
		projectPath = projectDir
	}
	// Prefer the repo name from git remote, then the slug-derived name from discovery,
	// then the filesystem basename as a last resort.
	projectName := RepoNameFromRemote(remoteURL)
	if projectName == "" {
		projectName = session.ProjectName
	}
	if projectName == "" {
		projectName = filepath.Base(projectPath)
	}

	projectHash, hostSlug, err := DeriveProjectIdentifiersWithGit(ctx, a.salt, a.git, remoteURL, projectPath)
	if err != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "derive_identity_error",
			Location:    fmt.Sprintf("session %s", session.SessionID),
			Message:     fmt.Sprintf("failed to derive project identifiers: %v", err),
			Remediation: "Check git remote URL or working directory path.",
		})
		meta.Project = ProjectInfo{FilePath: projectPath, Name: projectName}
		return
	}
	meta.HostSlug = hostSlug
	meta.Project = ProjectInfo{
		Hash:     projectHash,
		FilePath: projectPath,
		Name:     projectName,
	}
}

// decodeCursorWorkspace resolves a Cursor workspace name to the real project directory
// and a best-effort project name hint. Cursor encodes path separators, underscores,
// and spaces all as dashes (e.g. "/Users/foo/My_Proj" → "Users-foo-My-Proj").
//
// Cursor workspace folder names omit the leading "-" that Claude slugs include, so
// this prepends "-" before calling DecodeCursorSlug.
//
// Returns (projectDir, projectName):
//   - projectDir: the matched filesystem path (or the raw workspace path as fallback)
//   - projectName: repo/directory name for use when git resolution fails
//
// A partial decode names a project only when the matched directory is inside a
// git work tree: the unmatched tail is then a branch or worktree suffix of
// that repository (workspace "Users-foo-myrepo-feature-branch"). Outside a
// work tree the matched prefix is just a parent folder of a directory that is
// gone or could not be decoded, and attributing the session to it would be a
// guess (a session under "/tmp/claude-501/-Users-x/proj" became a project
// named "claude-501"), so the workspace stands for itself, as when nothing
// matched.
func (a *CursorAdapter) decodeCursorWorkspace(root, workspace string) (projectDir, projectName string) {
	encoded := "-" + workspace
	matched, unmatched := DecodeCursorSlug(encoded, a.dirExists)
	if matched == "" {
		return filepath.Join(root, workspace), workspace
	}
	if unmatched != "" {
		if !a.insideGitWorkTree(matched) {
			return filepath.Join(root, workspace), workspace
		}
		return matched, filepath.Base(matched)
	}
	return matched, filepath.Base(matched)
}

// insideGitWorkTree reports whether dir or one of its ancestors holds a .git
// entry (a directory, or the file a linked worktree carries).
func (a *CursorAdapter) insideGitWorkTree(dir string) bool {
	for {
		if _, err := a.fs.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// cursorSegmentVariants returns the candidate directory names to try for a given
// dash-encoded segment. Cursor encodes underscores and spaces as dashes, so each
// segment is tried as-is, with dashes replaced by underscores, and with dashes
// replaced by spaces (deduped when replacements produce no change).
func cursorSegmentVariants(segment string) []string {
	seen := make(map[string]bool, 3)
	variants := make([]string, 0, 3)
	for _, v := range []string{
		segment,
		strings.ReplaceAll(segment, "-", "_"),
		strings.ReplaceAll(segment, "-", " "),
	} {
		if !seen[v] {
			seen[v] = true
			variants = append(variants, v)
		}
	}
	return variants
}

// dirExists checks if path exists as a directory using the adapter's FileSystem.
func (a *CursorAdapter) dirExists(path string) bool {
	info, err := a.fs.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// CursorIndexer parses Cursor JSONL transcripts into SessionEntry slices.
type CursorIndexer struct {
	retainUnknown bool
	fs            FileSystem
	fullDepth     bool
	fullContent   bool
	// maxRecordBytes is the per-record read limit. Zero means the
	// production limit; a test injects a small one so it can prove the
	// over-limit path without building a record of production size.
	maxRecordBytes int
}

// WithCursorFullContent retains source prose before store preview normalization.
func WithCursorFullContent(enabled bool) CursorIndexerOption {
	return func(idx *CursorIndexer) { idx.fullContent = enabled }
}

// WithCursorMaxRecordBytes sets the per-record read limit. Zero keeps the
// production limit defaults.MaxJSONLRecordBytes. Passing the limit here
// keeps it out of any global, so tests that inject a small one stay safe
// to run in parallel.
func WithCursorMaxRecordBytes(limit int) CursorIndexerOption {
	return func(idx *CursorIndexer) { idx.maxRecordBytes = limit }
}

var _ TranscriptIndexer = (*CursorIndexer)(nil)
var _ VersionedTranscriptIndexer = (*CursorIndexer)(nil)

// IndexTranscriptResult verifies completion before authorizing persistent replacement.
func (idx *CursorIndexer) IndexTranscriptResult(ctx context.Context, session DiscoveredSession) (indexformat.Result, error) {
	completion := &indexCompletion{ctx: ctx, session: session}
	if err := ctx.Err(); err != nil {
		return nil, completion.failure(err)
	}
	data, err := idx.fs.ReadFile(session.SourcePath.String())
	if err != nil {
		return nil, completion.failure(err)
	}
	return idx.IndexTranscriptBytesResult(ctx, session, data)
}

// IndexTranscriptBytesResult consumes precisely the supplied transcript snapshot.
func (idx *CursorIndexer) IndexTranscriptBytesResult(ctx context.Context, session DiscoveredSession, data []byte) (indexformat.Result, error) {
	completion := &indexCompletion{ctx: ctx, session: session}
	entries, err := idx.parseJSONLWithCompletion(session.SessionID, data, completion)
	return completion.result(entries, err)
}

// SourceKind reports that Cursor's entries come from a single JSONL file; every entry is in its bytes.
func (idx *CursorIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

// CursorIndexerOption configures a CursorIndexer.
type CursorIndexerOption func(*CursorIndexer)

// WithCursorFullDepth enables or disables full-depth content block decomposition.
func WithCursorFullDepth(enabled bool) CursorIndexerOption {
	return func(idx *CursorIndexer) {
		idx.fullDepth = enabled
	}
}

// NewCursorIndexer creates a CursorIndexer for Cursor JSONL transcripts.
func NewCursorIndexer(fs FileSystem, opts ...CursorIndexerOption) *CursorIndexer {
	idx := &CursorIndexer{fs: fs}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

func (idx *CursorIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	data, err := idx.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("cursor indexer: read %s: %w", session.SourcePath, err)
	}
	return idx.parseJSONL(session.SessionID, data)
}

func (idx *CursorIndexer) IndexTranscriptBytes(_ context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	return idx.parseJSONL(session.SessionID, data)
}

func (idx *CursorIndexer) parseJSONL(sessionID SessionID, data []byte) ([]schema.SessionEntry, error) {
	return idx.parseJSONLWithCompletion(sessionID, data, nil)
}

func (idx *CursorIndexer) parseJSONLWithCompletion(sessionID SessionID, data []byte, completion *indexCompletion) ([]schema.SessionEntry, error) {
	var traversal unknownJSONLTraversal
	scanner := newJSONLRecordScanner(data, productionJSONLRecordLimit(idx.maxRecordBytes))

	var entries []schema.SessionEntry
	entryIndex := 0
	for scanner.Scan() {
		var placeholderErr error
		entries, placeholderErr = appendOmissionPlaceholders(entries, scanner, sessionID, HarnessCursor, &entryIndex)
		if placeholderErr != nil {
			return entries, placeholderErr
		}
		if completion != nil {
			completion.line = scanner.Line()
		}
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var unknown []RetainedUnknown
		if idx.retainUnknown {
			filtered, records, whole, err := prepareUnknownJSONL(HarnessCursor, scanner.Bytes(), scanner.Line(), &traversal)
			if err != nil {
				return nil, err
			}
			if whole {
				carrier, err := RetainedUnknownEntry(sessionID, entryIndex, records[0])
				if err != nil {
					return nil, err
				}
				entries = append(entries, carrier)
				entryIndex++
				continue
			}
			raw, unknown = filtered, records
		}
		var line cursorJSONLLine
		decodeErr := json.Unmarshal(raw, &line)
		if completion != nil {
			if err := completion.record(raw); err != nil {
				return nil, err
			}
			if decodeErr != nil {
				return nil, decodeErr
			}
			control, err := validateCursorTurnEnding(line)
			if err != nil {
				return nil, err
			}
			if !control && line.Role == "" && line.Message.Role == "" {
				return nil, fmt.Errorf("record has no transcript role")
			}
			if err := validateIndexContent(line.content()); err != nil {
				return nil, err
			}
			completion.recognized++
		}
		content := decodeCursorContent(line.content())
		entry, ok := cursorDecodedLineEntry(sessionID, entryIndex, raw, line, content, decodeErr, idx.fullContent)
		if !ok {
			entryIndex++
			continue
		}
		parentIndex := entryIndex
		if err := AttachRetainedUnknown(&entry, unknown); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		entryIndex++
		if idx.fullDepth {
			childEntries := decomposeCursorDecodedContent(sessionID, &entryIndex, parentIndex, line, content, idx.fullContent)
			if entry.Role == RoleSystem {
				for i := range childEntries {
					childEntries[i].Role = RoleSystem
					childEntries[i].EntryType = EntryTypeSystem
				}
			}
			entries = append(entries, childEntries...)
		}
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("cursor indexer: scanner error for %s: %w", sessionID, err)
	}
	if completion != nil && scanner.SawOversized() {
		return entries, uncertifiableOversizedRecord(scanner.Oversized(), scanner.limit)
	}
	entries, err := appendOmissionPlaceholders(entries, scanner, sessionID, HarnessCursor, &entryIndex)
	if err != nil {
		return entries, err
	}
	if completion != nil {
		completion.line = scanner.Line()
	}
	return entries, nil
}

func cursorLineEntry(sessionID SessionID, index int, raw []byte, line cursorJSONLLine, decodeErr error, fullContent bool) (schema.SessionEntry, bool) {
	return cursorDecodedLineEntry(sessionID, index, raw, line, decodeCursorContent(line.content()), decodeErr, fullContent)
}

type cursorDecodedContent struct {
	Blocks    []cursorContentBlock
	PlainText string
}

func decodeCursorContent(raw json.RawMessage) cursorDecodedContent {
	var content cursorDecodedContent
	if json.Unmarshal(raw, &content.PlainText) != nil {
		content.Blocks = parseCursorContentBlocks(raw)
	}
	return content
}

func cursorDecodedLineEntry(sessionID SessionID, index int, raw []byte, line cursorJSONLLine, content cursorDecodedContent, decodeErr error, fullContent bool) (schema.SessionEntry, bool) {
	if decodeErr != nil {
		return schema.SessionEntry{}, false
	}
	role := cursorLineRole(line)
	entry := schema.SessionEntry{
		SessionID:  sessionID,
		EntryIndex: index,
		Harness:    HarnessCursor,
		Role:       role,
		EntryType:  classifyCursorEntry(role),
	}
	if ts := parseIndexTimestamp(line.Timestamp); ts != nil {
		entry.TimestampMs = ts
	}
	rawLen := len(raw)
	entry.RawByteLength = &rawLen
	if line.UUID != "" {
		entry.EntryID = &line.UUID
	}
	if line.ParentUUID != "" {
		entry.ParentEntryID = &line.ParentUUID
	}
	if isCursorSpecialRecordKind(line.Type) && line.Status == "aborted" && line.Error != nil {
		entry.Role, entry.EntryType, entry.IsError = RoleSystem, EntryTypeError, true
		text := *line.Error
		if !fullContent {
			text = truncateString(text, defaults.ContentPreviewLimit)
		}
		entry.ContentPreview = &text
		return entry, true
	}

	blocks := content.Blocks
	plainText := content.PlainText
	if !fullContent {
		plainText = strings.TrimSpace(plainText)
	}
	var toolNames []string
	var preview strings.Builder
	var thinkingFallback string
	if plainText != "" {
		preview.WriteString(cursorCapturedText(plainText, fullContent))
	}
	for _, block := range blocks {
		switch block.Type {
		case "tool_use":
			entry.HasToolUse = true
			if block.Name != "" {
				toolNames = append(toolNames, block.Name)
			}
		case "thinking":
			entry.HasThinking = true
			if thinkingFallback == "" {
				thinkingFallback = firstNonEmpty(block.Thinking, block.Text)
			}
		case "text":
			if preview.Len() == 0 && block.Text != "" {
				preview.WriteString(cursorCapturedText(block.Text, fullContent))
			}
		case "tool_result":
			if block.IsError {
				entry.IsError = true
			}
			if preview.Len() == 0 {
				preview.WriteString(rawMessagePreview(block.Content))
			}
		}
	}
	if preview.Len() == 0 && thinkingFallback != "" {
		preview.WriteString(thinkingFallback)
	}
	if len(toolNames) > 0 {
		csv := strings.Join(toolNames, ",")
		entry.ToolNamesCSV = &csv
	}
	textToCheck := cursorSystemCheckText(plainText, blocks)
	if entry.Role == RoleUser && textToCheck != "" {
		if isSystemInjectedContent(textToCheck) {
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		} else if cmdName, cmdArgs, ok := parseSkillInvocation(textToCheck); ok {
			extra := `{"command_name":"` + jsonEscapeString(cmdName) + `"`
			if cmdArgs != "" {
				extra += `,"command_args":"` + jsonEscapeString(cmdArgs) + `"`
			}
			extra += `}`
			entry.Extra = &extra
		}
	}
	if entry.HasToolUse && entry.Role == RoleAssistant {
		entry.EntryType = EntryTypeToolUse
	}
	if entry.HasThinking && entry.EntryType == EntryTypeText && entry.Role == RoleAssistant {
		entry.EntryType = EntryTypeThinking
	}
	if entry.IsError && entry.Role == RoleTool {
		entry.EntryType = EntryTypeToolResult
	}
	if preview.Len() > 0 {
		p := cursorContentPreview(preview.String(), fullContent)
		entry.ContentPreview = &p
	}
	if line.Message.Usage != nil {
		if line.Message.Usage.InputTokens > 0 {
			v := line.Message.Usage.InputTokens
			entry.TokensIn = &v
		}
		if line.Message.Usage.OutputTokens > 0 {
			v := line.Message.Usage.OutputTokens
			entry.TokensOut = &v
		}
	}
	if line.StopReason != "" {
		sr := schema.StopReason(line.StopReason)
		if sr.IsValid() {
			entry.StopReason = &sr
		}
	}
	return entry, true
}

func decomposeCursorContentBlocks(sessionID SessionID, entryIndex *int, parentIndex int, raw []byte, fullContent bool) []schema.SessionEntry {
	var line cursorJSONLLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return nil
	}
	return decomposeCursorDecodedContent(sessionID, entryIndex, parentIndex, line, decodeCursorContent(line.content()), fullContent)
}

func decomposeCursorDecodedContent(sessionID SessionID, entryIndex *int, parentIndex int, line cursorJSONLLine, content cursorDecodedContent, fullContent bool) []schema.SessionEntry {
	blocks := content.Blocks
	if len(blocks) == 0 {
		return nil
	}
	role := cursorLineRole(line)
	var entries []schema.SessionEntry
	for _, block := range blocks {
		entry := schema.SessionEntry{
			SessionID:   sessionID,
			EntryIndex:  *entryIndex,
			Harness:     HarnessCursor,
			Role:        role,
			Depth:       1,
			ParentIndex: &parentIndex,
		}
		pt := block.Type
		entry.PartType = &pt
		switch block.Type {
		case "text":
			entry.EntryType = EntryTypeText
			if block.Text != "" {
				cleaned := cursorCapturedText(block.Text, fullContent)
				p := cursorContentPreview(cleaned, fullContent)
				entry.ContentPreview = &p
				if isSystemInjectedContent(block.Text) {
					entry.Role = RoleSystem
					entry.EntryType = EntryTypeSystem
				}
			}
		case "tool_use":
			entry.EntryType = EntryTypeToolUse
			entry.HasToolUse = true
			if block.Name != "" {
				entry.ToolNamesCSV = &block.Name
				if kind := classifyCursorToolKind(block.Name); kind != "" {
					entry.ToolKind = &kind
				}
			}
			if block.ID != "" {
				entry.ToolCallID = &block.ID
			}
			if len(block.Input) > 0 && string(block.Input) != "null" {
				s := string(block.Input)
				entry.ToolInput = &s
			}
		case "tool_result":
			entry.EntryType = EntryTypeToolResult
			entry.IsError = block.IsError
			if block.ToolUseID != "" {
				entry.ToolCallID = &block.ToolUseID
			}
			if output := rawMessagePreview(block.Content); output != "" {
				entry.ToolOutput = &output
			}
		case "thinking":
			entry.EntryType = EntryTypeThinking
			entry.HasThinking = true
			text := firstNonEmpty(block.Thinking, block.Text)
			if text != "" {
				p := cursorContentPreview(text, fullContent)
				entry.ContentPreview = &p
			}
			thinkLen := len(text)
			entry.RawByteLength = &thinkLen
		default:
			entry.EntryType = EntryTypeText
		}
		entries = append(entries, entry)
		*entryIndex++
	}
	return entries
}

func cursorLineRole(line cursorJSONLLine) Role {
	raw := line.Role
	if raw == "" {
		raw = line.Message.Role
	}
	role := Role(raw)
	if role.IsValid() {
		return role
	}
	switch raw {
	case "human":
		return RoleUser
	default:
		return RoleSystem
	}
}

func classifyCursorEntry(role Role) EntryType {
	switch role {
	case RoleUser, RoleAssistant:
		return EntryTypeText
	case RoleTool:
		return EntryTypeToolResult
	case RoleSystem:
		return EntryTypeSystem
	default:
		return EntryTypeText
	}
}

func parseCursorContentBlocks(raw json.RawMessage) []cursorContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []cursorContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	return nil
}

func extractCursorText(raw json.RawMessage) string {
	var candidate string
	text := extractCursorPlainText(raw)
	if text != "" {
		candidate = stripCursorQueryTag(strings.TrimSpace(text))
	} else {
		for _, block := range parseCursorContentBlocks(raw) {
			if block.Type == "text" && block.Text != "" {
				candidate = stripCursorQueryTag(strings.TrimSpace(block.Text))
				break
			}
		}
	}
	if candidate == "" {
		return ""
	}
	// Use only the first non-empty line so the picker shows a tidy one-liner.
	for _, line := range strings.Split(candidate, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 80 {
				line = line[:77] + "..."
			}
			return line
		}
	}
	return ""
}

func extractCursorPlainText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func cursorSystemCheckText(plainText string, blocks []cursorContentBlock) string {
	if plainText != "" {
		return plainText
	}
	if len(blocks) == 0 {
		return ""
	}
	for _, block := range blocks {
		if block.Type != "text" {
			return ""
		}
	}
	for _, block := range blocks {
		if block.Text != "" {
			return block.Text
		}
	}
	return ""
}

func rawMessagePreview(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
