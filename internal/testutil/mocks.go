package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// --- Constants ---

const (
	TestSessionUUID    = "99d59925-36bc-424c-a789-8be54d9702ba"
	TestSessionUUID2   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	TestSubagentID     = "agent-a3aee4f"
	TestOpenCodeSesID  = "ses_3cd91f52effeXd3QAJ54jOyzv5"
	TestCodexSessionID = "cccccccc-dddd-eeee-ffff-000011112222"
	TestGitRemote      = "git@github.com:testuser/testrepo.git"
	TestHostSlug       = "github.com--testuser--testrepo"
	TestEmail          = "test@example.com"
)

// pullRefFixtures is the loaded canonical pull-ref fixture, embedded in the
// github.com/peasant-labs/schema module (loaded via schema.LoadPullRefFixtures).
// TestVillageHost /
// TestTranscriptUUID RE-EXPORT its self-contained `values` so the fixture is the
// single source of truth and a schema/value change touches one YAML file, not the
// inlined literals here. The fixture adopted these exact values, so the
// re-export is a zero-behaviour-change move (asserted by pullFixtureValuesMatch
// below + the existing pull/store tests staying green).
var pullRefFixtures = mustLoadPullRefFixtures()

func mustLoadPullRefFixtures() *schema.PullRefFixtures {
	f, err := schema.LoadPullRefFixtures()
	if err != nil {
		panic("testutil: load pull ref fixtures (schema.LoadPullRefFixtures, github.com/peasant-labs/schema): " + err.Error())
	}
	return f
}

// Village-pull (V34) test fixtures. Shared so a schema change to the pulled_*
// tables touches one place, not N inlined literals. TestTranscriptUUID /
// TestVillageHost are RE-EXPORTED from the canonical YAML fixture `values`
// (see pullRefFixtures); the remaining literals stay here.
var (
	// TestVillageHost re-exports values.village_host from the pull-ref fixture.
	TestVillageHost = pullRefFixtures.VillageHost()
	// TestTranscriptUUID re-exports values.uuid_lower from the pull-ref fixture; a
	// canonical lowercase-hex UUID accepted by schema.NewTranscriptID.
	TestTranscriptUUID = pullRefFixtures.UUIDLower()
)

const (
	TestVillageHost2     = "other-village.example.com"
	TestTranscriptUUID2  = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	TestContentHash      = "a3f5c9e1b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c6e8b0d2f4a6"
	TestContentHash2     = "b4e6dac2c3e5f7b9d1f3c5e7f9b1d3f5c7e9f1b3d5f7b9d1f3c5e7f9b1d3f5c7"
	TestContentHash3     = "c5f7ecd3d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c6e8b0d2f4a6c8"
	TestOwnerUserID      = "user-owner-001"
	TestOwnerUsername    = "alice"
	TestAuthorUserID     = "user-author-002"
	TestAuthorUsername   = "bob"
	TestPullAuthorUserID = "user-self-003" // requester's own ID — used for own-author exclusion tests
)

// Typed village-pull fixtures. Pull-surface consumers key off schema.TranscriptID
// rather than the bare UUID string; providing the typed forms here removes the
// repeated `schema.TranscriptID(testutil.TestTranscriptUUID)` cast from every
// consumer file.
var (
	TestTranscriptID  = schema.TranscriptID(TestTranscriptUUID)
	TestTranscriptID2 = schema.TranscriptID(TestTranscriptUUID2)
)

var TestSessionStartTime = time.Date(2026, 2, 24, 9, 15, 0, 0, time.UTC)

// Named constants for StubGitResolver defaults.
const (
	TestDefaultBranch      = "main"
	TestDefaultWorktreeDir = "/home/test/testrepo"
	TestDefaultTracking    = "origin/main"
	// TestProjectName is the project name derived from TestDefaultWorktreeDir (filepath.Base).
	TestProjectName   = "testrepo"
	ErrNotGitRepo     = "not a git repository"
	ErrNoRemoteOrigin = "remote origin not found"
)

var TestModel = ingest.ModelID("claude-opus-4-6")

// TestProjectHash is the canonical 64-hex (lowercase) project hash used across
// push/ingest tests. Single fixture so a schema change touches one place, not N
// inlined literals.
var TestProjectHash = ingest.ProjectHash("abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234abcdef12345678")

// Entry type test fixtures.
var (
	TestEntryTypeText       = ingest.EntryTypeText
	TestEntryTypeToolUse    = ingest.EntryTypeToolUse
	TestEntryTypeToolResult = ingest.EntryTypeToolResult
	TestEntryTypeThinking   = ingest.EntryTypeThinking
	TestEntryTypeSystem     = ingest.EntryTypeSystem
	TestEntryTypeError      = ingest.EntryTypeError
	TestEntryTypeResult     = ingest.EntryTypeResult
)

// Outcome test fixtures.
var (
	TestOutcomeResolved = ingest.OutcomeResolved
	TestOutcomePartial  = ingest.OutcomePartial
	TestOutcomeFailed   = ingest.OutcomeFailed
)

// Role test fixtures.
var (
	TestRoleUser      = ingest.RoleUser
	TestRoleAssistant = ingest.RoleAssistant
	TestRoleTool      = ingest.RoleTool
	TestRoleSystem    = ingest.RoleSystem
)

// --- MemFS ---

// MemFS is an in-memory filesystem for testing.
// All methods are safe for concurrent use (protected by mu).
type MemFS struct {
	mu       sync.RWMutex
	Files    map[string][]byte
	Dirs     map[string]bool
	ModTimes map[string]time.Time
}

var _ ingest.FileSystem = (*MemFS)(nil)

// NewMemFS creates an empty MemFS with root "/" pre-created.
func NewMemFS() *MemFS {
	m := &MemFS{
		Files:    make(map[string][]byte),
		Dirs:     make(map[string]bool),
		ModTimes: make(map[string]time.Time),
	}
	m.Dirs["/"] = true
	return m
}

func (m *MemFS) ReadFile(path string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.Files[path]
	if !ok {
		return nil, &os.PathError{Op: defaults.FSOpOpen, Path: path, Err: os.ErrNotExist}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

func (m *MemFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// auto-create parent dirs (inline, lock already held)
	m.mkdirAllLocked(filepath.Dir(path), defaults.PublicDirPerm)
	cp := make([]byte, len(data))
	copy(cp, data)
	m.Files[path] = cp
	m.ModTimes[path] = time.Now()
	return nil
}

// mkdirAllLocked is the lock-free inner implementation of MkdirAll.
// Must be called with m.mu held (write lock).
func (m *MemFS) mkdirAllLocked(path string, _ os.FileMode) {
	clean := filepath.Clean(path)
	parts := strings.Split(clean, string(os.PathSeparator))
	cur := ""
	for _, p := range parts {
		if p == "" {
			cur = "/"
		} else {
			if cur == "/" {
				cur = "/" + p
			} else {
				cur = cur + "/" + p
			}
		}
		m.Dirs[cur] = true
	}
}

func (m *MemFS) MkdirAll(path string, perm os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mkdirAllLocked(path, perm)
	return nil
}

// memFileInfo implements os.FileInfo for MemFS entries.
type memFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

func (fi *memFileInfo) Name() string       { return fi.name }
func (fi *memFileInfo) Size() int64        { return fi.size }
func (fi *memFileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *memFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *memFileInfo) IsDir() bool        { return fi.isDir }
func (fi *memFileInfo) Sys() any           { return nil }

// statLocked returns FileInfo for path. Must be called with m.mu held (at least RLock).
func (m *MemFS) statLocked(path string) (os.FileInfo, error) {
	if m.Dirs[path] {
		return &memFileInfo{
			name:    filepath.Base(path),
			mode:    os.ModeDir | defaults.PublicDirPerm,
			modTime: m.ModTimes[path],
			isDir:   true,
		}, nil
	}
	data, ok := m.Files[path]
	if !ok {
		return nil, &os.PathError{Op: defaults.FSOpStat, Path: path, Err: os.ErrNotExist}
	}
	return &memFileInfo{
		name:    filepath.Base(path),
		size:    int64(len(data)),
		mode:    defaults.PublicFilePerm,
		modTime: m.ModTimes[path],
		isDir:   false,
	}, nil
}

func (m *MemFS) Stat(path string) (os.FileInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.statLocked(path)
}

func (m *MemFS) Lstat(path string) (os.FileInfo, error) {
	return m.Stat(path)
}

// memDirEntry implements os.DirEntry backed by memFileInfo.
type memDirEntry struct {
	info *memFileInfo
}

func (e *memDirEntry) Name() string               { return e.info.Name() }
func (e *memDirEntry) IsDir() bool                { return e.info.IsDir() }
func (e *memDirEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e *memDirEntry) Info() (os.FileInfo, error) { return e.info, nil }

func (m *MemFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	root = filepath.Clean(root)

	// Take a snapshot of paths and their FileInfo under the lock, then
	// release before calling the user-supplied fn (which may call back
	// into MemFS methods and would deadlock if we held the lock).
	m.mu.RLock()
	type entry struct {
		path string
		info os.FileInfo
		err  error
	}
	var paths []string
	for p := range m.Dirs {
		if p == root || strings.HasPrefix(p, root+"/") {
			paths = append(paths, p)
		}
	}
	for p := range m.Files {
		if strings.HasPrefix(p, root+"/") || p == root {
			paths = append(paths, p)
		}
	}
	// deduplicate
	seen := make(map[string]bool)
	unique := paths[:0]
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	sort.Strings(unique)
	snapshot := make([]entry, len(unique))
	for i, p := range unique {
		info, err := m.statLocked(p)
		snapshot[i] = entry{path: p, info: info, err: err}
	}
	m.mu.RUnlock()

	// Walk the snapshot (lock released; fn may re-enter MemFS methods safely).
	var skipPrefix string
	for _, e := range snapshot {
		p := e.path
		if skipPrefix != "" && strings.HasPrefix(p, skipPrefix) {
			continue
		}
		skipPrefix = ""

		if e.err != nil {
			if err2 := fn(p, nil, e.err); err2 != nil {
				return err2
			}
			continue
		}
		fi, ok := e.info.(*memFileInfo)
		if !ok {
			return fmt.Errorf("WalkDir: unexpected FileInfo type %T for path %s", e.info, p)
		}
		de := &memDirEntry{info: fi}
		if err := fn(p, de, nil); err != nil {
			if errors.Is(err, fs.SkipDir) {
				if de.IsDir() {
					skipPrefix = p + "/"
				} else {
					skipPrefix = filepath.Dir(p) + "/"
				}
				continue
			}
			if errors.Is(err, fs.SkipAll) {
				return nil
			}
			return err
		}
	}
	return nil
}

// Rename moves a single file or directory node from oldpath to newpath.
//
// KNOWN LIMITATION (I4): When renaming a directory, only the directory entry
// itself is moved—child files and subdirectories are NOT relocated. This differs
// from os.Rename which atomically moves the entire subtree. The pipeline works
// around this via renameDir() which does a recursive walk+copy+remove.
func (m *MemFS) Rename(oldpath, newpath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// move file
	if data, ok := m.Files[oldpath]; ok {
		m.Files[newpath] = data
		m.ModTimes[newpath] = m.ModTimes[oldpath]
		delete(m.Files, oldpath)
		delete(m.ModTimes, oldpath)
		return nil
	}
	// move dir (node only, not children — see limitation above)
	if m.Dirs[oldpath] {
		m.Dirs[newpath] = true
		delete(m.Dirs, oldpath)
		return nil
	}
	return &os.PathError{Op: defaults.FSOpRename, Path: oldpath, Err: os.ErrNotExist}
}

func (m *MemFS) ReadDir(path string) ([]os.DirEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	path = filepath.Clean(path)
	if !m.Dirs[path] {
		return nil, &os.PathError{Op: defaults.FSOpReadDir, Path: path, Err: os.ErrNotExist}
	}

	seen := make(map[string]bool)
	var entries []os.DirEntry

	prefix := path + "/"

	// direct child dirs
	for p := range m.Dirs {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		if !seen[rest] {
			seen[rest] = true
			info, _ := m.statLocked(p)
			fi, ok := info.(*memFileInfo)
			if !ok {
				return nil, fmt.Errorf("ReadDir: unexpected FileInfo type %T for path %s", info, p)
			}
			entries = append(entries, &memDirEntry{info: fi})
		}
	}

	// direct child files
	for p := range m.Files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		if !seen[rest] {
			seen[rest] = true
			info, _ := m.statLocked(p)
			fi, ok := info.(*memFileInfo)
			if !ok {
				return nil, fmt.Errorf("ReadDir: unexpected FileInfo type %T for path %s", info, p)
			}
			entries = append(entries, &memDirEntry{info: fi})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	return entries, nil
}

func (m *MemFS) Remove(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Files[path]; ok {
		delete(m.Files, path)
		delete(m.ModTimes, path)
		return nil
	}
	if m.Dirs[path] {
		delete(m.Dirs, path)
		return nil
	}
	return &os.PathError{Op: defaults.FSOpRemove, Path: path, Err: os.ErrNotExist}
}

func (m *MemFS) RemoveAll(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = filepath.Clean(path)
	prefix := path + "/"
	for p := range m.Files {
		if p == path || strings.HasPrefix(p, prefix) {
			delete(m.Files, p)
			delete(m.ModTimes, p)
		}
	}
	for p := range m.Dirs {
		if p == path || strings.HasPrefix(p, prefix) {
			delete(m.Dirs, p)
		}
	}
	return nil
}

func (m *MemFS) CopyFile(src, dst string, perm os.FileMode) error {
	// Read src under RLock, then write dst under write lock.
	m.mu.RLock()
	data, ok := m.Files[src]
	var cp []byte
	if ok {
		cp = make([]byte, len(data))
		copy(cp, data)
	}
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("copy %q -> %q: src not found", src, dst)
	}
	// WriteFile acquires the write lock internally.
	return m.WriteFile(dst, cp, perm)
}

// --- StubGitResolver ---

// StubGitResolver is a test double for GitResolver.
// Per-method error fields allow simulating partial failures
// (e.g. no remote but email works).
type StubGitResolver struct {
	Remote             string
	BranchName         string
	WorktreeDir        string
	TrackingBranchName string
	Email              string
	// WalkUpRemoteResult is the (remoteURL, resolvedDir) pair returned by WalkUpRemoteURL.
	// If both are empty and WalkUpRemoteErr is nil, WalkUpRemoteURL returns ("", "", nil)
	// (no remote found anywhere — the walk-up succeeded but found nothing).
	WalkUpRemoteResult [2]string // [remoteURL, resolvedDir]
	// Per-method error overrides. If non-nil, the corresponding method returns this error.
	RemoteErr         error
	BranchErr         error
	WorktreeErr       error
	TrackingBranchErr error
	EmailErr          error
	WalkUpRemoteErr   error
}

var _ ingest.GitResolver = (*StubGitResolver)(nil)

// DefaultGitResolver returns a StubGitResolver with sensible test defaults.
func DefaultGitResolver() *StubGitResolver {
	return &StubGitResolver{
		Remote:             TestGitRemote,
		BranchName:         TestDefaultBranch,
		WorktreeDir:        TestDefaultWorktreeDir,
		TrackingBranchName: TestDefaultTracking,
		Email:              TestEmail,
	}
}

// NoGitResolver returns a StubGitResolver that always errors (simulates no git repo).
func NoGitResolver() *StubGitResolver {
	return &StubGitResolver{
		RemoteErr:         fmt.Errorf(ErrNotGitRepo),
		BranchErr:         fmt.Errorf(ErrNotGitRepo),
		WorktreeErr:       fmt.Errorf(ErrNotGitRepo),
		TrackingBranchErr: fmt.Errorf(ErrNotGitRepo),
		EmailErr:          fmt.Errorf(ErrNotGitRepo),
	}
}

// NoRemoteGitResolver returns a StubGitResolver where RemoteURL fails
// but Branch, Worktree, and UserEmail succeed. Simulates a repo with no remote origin.
func NoRemoteGitResolver() *StubGitResolver {
	return &StubGitResolver{
		BranchName:         TestDefaultBranch,
		WorktreeDir:        TestDefaultWorktreeDir,
		TrackingBranchName: TestDefaultTracking,
		Email:              TestEmail,
		RemoteErr:          fmt.Errorf(ErrNoRemoteOrigin),
	}
}

func (s *StubGitResolver) RemoteURL(_ context.Context, _ string) (string, error) {
	if s.RemoteErr != nil {
		return "", s.RemoteErr
	}
	return s.Remote, nil
}

func (s *StubGitResolver) Branch(_ context.Context, _ string) (string, error) {
	if s.BranchErr != nil {
		return "", s.BranchErr
	}
	return s.BranchName, nil
}

func (s *StubGitResolver) Worktree(_ context.Context, _ string) (string, error) {
	if s.WorktreeErr != nil {
		return "", s.WorktreeErr
	}
	return s.WorktreeDir, nil
}

func (s *StubGitResolver) TrackingBranch(_ context.Context, _ string) (string, error) {
	if s.TrackingBranchErr != nil {
		return "", s.TrackingBranchErr
	}
	return s.TrackingBranchName, nil
}

func (s *StubGitResolver) UserEmail(_ context.Context) (string, error) {
	if s.EmailErr != nil {
		return "", s.EmailErr
	}
	return s.Email, nil
}

func (s *StubGitResolver) WalkUpRemoteURL(_ context.Context, _ string) (string, string, error) {
	if s.WalkUpRemoteErr != nil {
		return "", "", s.WalkUpRemoteErr
	}
	return s.WalkUpRemoteResult[0], s.WalkUpRemoteResult[1], nil
}

// --- StubSessionStore ---

// StubSessionStore is a test double for ingest.SessionStore.
// Per-method error fields allow simulating insert failures.
type StubSessionStore struct {
	InsertedEntries  []ingest.StoreEntry
	UpsertedCommits  map[ingest.SessionID][]ingest.CommitInfo
	Closed           bool
	InsertErr        error
	CloseErr         error
	UpsertCommitsErr error
	// CleanupOrphanProjectsCalled is set to true when CleanupOrphanProjects is called.
	CleanupOrphanProjectsCalled bool
	// CleanupOrphanProjectsErr is the error returned by CleanupOrphanProjects.
	CleanupOrphanProjectsErr error
	// LookupSessionLocationFunc allows per-test injection. Nil = return empty strings.
	LookupSessionLocationFunc func(ctx context.Context, sessionID ingest.SessionID) (string, string, error)
	// LocationsByID allows pre-populating BulkLookupSessionLocations responses with
	// full SessionLocation values (including IngestedMs and SchemaVersion). Takes
	// precedence over LookupSessionLocationFunc for bulk lookups. Nil = use func.
	LocationsByID map[ingest.SessionID]ingest.SessionLocation
	// OnInsert is called by InsertSessions before recording the entries. Useful for
	// observing side effects at DB INSERT time (e.g., asserting file not yet written).
	OnInsert func(entries []ingest.StoreEntry)
}

var _ ingest.SessionStore = (*StubSessionStore)(nil)

func (s *StubSessionStore) InsertSessions(_ context.Context, entries []ingest.StoreEntry) error {
	if s.InsertErr != nil {
		return s.InsertErr
	}
	if s.OnInsert != nil {
		s.OnInsert(entries)
	}
	s.InsertedEntries = append(s.InsertedEntries, entries...)
	return nil
}

func (s *StubSessionStore) LookupSessionLocation(ctx context.Context, sessionID ingest.SessionID) (string, string, error) {
	if s.LocationsByID != nil {
		if loc, ok := s.LocationsByID[sessionID]; ok {
			return loc.HostSlug, loc.ParentID, nil
		}
		return "", "", nil
	}
	if s.LookupSessionLocationFunc != nil {
		return s.LookupSessionLocationFunc(ctx, sessionID)
	}
	return "", "", nil
}

func (s *StubSessionStore) BulkLookupSessionLocations(_ context.Context, sessionIDs []ingest.SessionID) (map[ingest.SessionID]ingest.SessionLocation, error) {
	result := make(map[ingest.SessionID]ingest.SessionLocation, len(sessionIDs))
	if s.LocationsByID != nil {
		for _, id := range sessionIDs {
			if loc, ok := s.LocationsByID[id]; ok {
				result[id] = loc
			}
		}
		return result, nil
	}
	if s.LookupSessionLocationFunc != nil {
		for _, id := range sessionIDs {
			hostSlug, parentID, err := s.LookupSessionLocationFunc(context.Background(), id)
			if err != nil {
				continue
			}
			if hostSlug != "" {
				result[id] = ingest.SessionLocation{HostSlug: hostSlug, ParentID: parentID}
			}
		}
	}
	return result, nil
}

func (s *StubSessionStore) UpsertSessionCommits(_ context.Context, sessionID ingest.SessionID, commits []ingest.CommitInfo) error {
	if s.UpsertCommitsErr != nil {
		return s.UpsertCommitsErr
	}
	if s.UpsertedCommits == nil {
		s.UpsertedCommits = make(map[ingest.SessionID][]ingest.CommitInfo)
	}
	s.UpsertedCommits[sessionID] = commits
	return nil
}

// CleanupOrphanProjectsCalled records whether CleanupOrphanProjects was called.
// Embed in StubSessionStore fields for test assertions.
func (s *StubSessionStore) CleanupOrphanProjects(_ context.Context) error {
	s.CleanupOrphanProjectsCalled = true
	return s.CleanupOrphanProjectsErr
}

func (s *StubSessionStore) Close() error {
	if s.CloseErr != nil {
		return s.CloseErr
	}
	s.Closed = true
	return nil
}

// --- StubMetricsStore ---

// StubMetricsStore is a test double for ingest.MetricsStore.
// Per-method error fields allow simulating failures.
type StubMetricsStore struct {
	IndexedEntries             map[ingest.SessionID][]schema.SessionEntry
	SavedMetrics               map[ingest.SessionID]*ingest.SessionMetrics
	UpdatedDays                []string
	IndexErr                   error
	ExistErr                   error
	SaveErr                    error
	GetErr                     error
	MetricsExistFn             func(ingest.SessionID, int) (bool, error)
	ListErr                    error
	UpdateErr                  error
	UpdateIndexErr             error
	StaleIndexSessions         []ingest.SessionID
	StaleIndexErr              error
	IndexStates                map[ingest.SessionID]int // tracks index_version per session
	ListStaleCalledWithVersion int                      // last currentVersion argument passed to ListStaleIndexSessions
	// SourceInfoByID allows per-session injection for LookupSourceInfo.
	// Nil = return empty (fallback skipped).
	SourceInfoByID map[ingest.SessionID]struct {
		SourcePath   string
		SourceFormat ingest.SourceFormat
		Harness      string
	}
	// LookupSessionLocationFunc allows per-test injection for MetricsStore.LookupSessionLocation.
	// Nil = return empty strings (existing behavior).
	LookupSessionLocationFunc func(ctx context.Context, sessionID ingest.SessionID) (string, string, error)
	TitleHarness              schema.Harness
	TitleProjectPath          string
	TitleContextErr           error
}

var _ ingest.MetricsStore = (*StubMetricsStore)(nil)

// NewStubMetricsStore creates a ready-to-use StubMetricsStore.
func NewStubMetricsStore() *StubMetricsStore {
	return &StubMetricsStore{
		IndexedEntries:   make(map[ingest.SessionID][]schema.SessionEntry),
		SavedMetrics:     make(map[ingest.SessionID]*ingest.SessionMetrics),
		IndexStates:      make(map[ingest.SessionID]int),
		TitleHarness:     schema.HarnessClaudeCode,
		TitleProjectPath: "/home/test/project",
	}
}

func (s *StubMetricsStore) GetTitleContext(_ context.Context, _ ingest.SessionID) (schema.Harness, string, error) {
	if s.TitleContextErr != nil {
		return "", "", s.TitleContextErr
	}
	return s.TitleHarness, s.TitleProjectPath, nil
}

func (s *StubMetricsStore) IndexSessionEntries(_ context.Context, sessionID ingest.SessionID, entries []schema.SessionEntry) error {
	if s.IndexErr != nil {
		return s.IndexErr
	}
	s.IndexedEntries[sessionID] = entries
	return nil
}

func (s *StubMetricsStore) SessionEntriesExist(_ context.Context, sessionID ingest.SessionID) (bool, error) {
	if s.ExistErr != nil {
		return false, s.ExistErr
	}
	_, ok := s.IndexedEntries[sessionID]
	return ok, nil
}

func (s *StubMetricsStore) SaveMetrics(_ context.Context, m *ingest.SessionMetrics) error {
	if s.SaveErr != nil {
		return s.SaveErr
	}
	s.SavedMetrics[m.SessionID] = m
	return nil
}

func (s *StubMetricsStore) GetMetrics(_ context.Context, sessionID ingest.SessionID) (*ingest.SessionMetrics, error) {
	if s.GetErr != nil {
		return nil, s.GetErr
	}
	m, ok := s.SavedMetrics[sessionID]
	if !ok {
		return nil, nil
	}
	return m, nil
}

func (s *StubMetricsStore) MetricsExist(_ context.Context, sessionID ingest.SessionID, computeVersion int) (bool, error) {
	if s.MetricsExistFn != nil {
		return s.MetricsExistFn(sessionID, computeVersion)
	}
	m, ok := s.SavedMetrics[sessionID]
	if !ok {
		return false, nil
	}
	return m.ComputeVersion != nil && *m.ComputeVersion >= computeVersion, nil
}

func (s *StubMetricsStore) ListEntries(_ context.Context, sessionID ingest.SessionID) ([]schema.SessionEntry, error) {
	if s.ListErr != nil {
		return nil, s.ListErr
	}
	entries, ok := s.IndexedEntries[sessionID]
	if !ok {
		return nil, nil
	}
	return entries, nil
}

func (s *StubMetricsStore) UpdateDailySummary(_ context.Context, days []string) error {
	if s.UpdateErr != nil {
		return s.UpdateErr
	}
	s.UpdatedDays = append(s.UpdatedDays, days...)
	return nil
}

func (s *StubMetricsStore) UpdateIndexState(_ context.Context, sessionID ingest.SessionID, version int, _ int64) error {
	if s.UpdateIndexErr != nil {
		return s.UpdateIndexErr
	}
	s.IndexStates[sessionID] = version
	return nil
}

func (s *StubMetricsStore) ListStaleIndexSessions(_ context.Context, currentVersion int) ([]ingest.SessionID, error) {
	s.ListStaleCalledWithVersion = currentVersion
	if s.StaleIndexErr != nil {
		return nil, s.StaleIndexErr
	}
	return s.StaleIndexSessions, nil
}

func (s *StubMetricsStore) LookupSessionLocation(ctx context.Context, sid ingest.SessionID) (string, string, error) {
	if s.LookupSessionLocationFunc != nil {
		return s.LookupSessionLocationFunc(ctx, sid)
	}
	// Default: return empty — falls back to directory scan in reconstructFromMetadata.
	return "", "", nil
}

func (s *StubMetricsStore) LookupSourceInfo(_ context.Context, sid ingest.SessionID) (string, ingest.SourceFormat, string, error) {
	if s.SourceInfoByID != nil {
		if info, ok := s.SourceInfoByID[sid]; ok {
			return info.SourcePath, info.SourceFormat, info.Harness, nil
		}
	}
	// Default: return empty — reconstructFromSourceInfo will skip.
	return "", "", "", nil
}

// --- StubAdapter ---

// StubAdapter is a test double for SourceAdapter.
type StubAdapter struct {
	ProviderValue ingest.Harness
	Sessions      []ingest.DiscoveredSession
	Metadata      map[ingest.SessionID]*ingest.UnifiedMetadata
	DiscoverErr   error
	ExtractErr    error
}

var _ ingest.SourceAdapter = (*StubAdapter)(nil)

func (a *StubAdapter) Harness() ingest.Harness { return a.ProviderValue }

func (a *StubAdapter) Discover(_ context.Context, _ ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	if a.DiscoverErr != nil {
		return nil, a.DiscoverErr
	}
	return a.Sessions, nil
}

func (a *StubAdapter) ExtractMetadata(_ context.Context, s ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	if a.ExtractErr != nil {
		return nil, a.ExtractErr
	}
	m, ok := a.Metadata[s.SessionID]
	if !ok {
		return nil, fmt.Errorf("no metadata for session %s", s.SessionID)
	}
	return m, nil
}

// --- StubRedactor ---

// StubRedactor is a test double for ingest.TextRedactor.
// It tracks calls and optionally transforms metadata.
// Called tracks RedactMetadata invocations; JSONCalled tracks RedactJSON invocations.
type StubRedactor struct {
	Called     int
	JSONCalled int
	Err        error
}

var _ ingest.TextRedactor = (*StubRedactor)(nil)

func (r *StubRedactor) RedactMetadata(meta *ingest.UnifiedMetadata) *ingest.UnifiedMetadata {
	r.Called++
	if meta == nil {
		return nil
	}
	// Return a copy with a marker indicating redaction was applied.
	redacted := *meta
	redacted.Project.Name = "<REDACTED:" + meta.Project.Name + ">"
	redacted.Project.FilePath = "<REDACTED>"
	return &redacted
}

// RedactJSON is a pass-through stub: returns the value unchanged.
// Tests that need a visible transform should use a custom TextRedactor implementation.
func (r *StubRedactor) RedactJSON(value any) any {
	r.JSONCalled++
	return value
}

// Level returns "standard" as the stub redaction level.
func (r *StubRedactor) Level() string {
	return "standard"
}

// --- NoopRedactor ---

// NoopRedactor is a test double for ingest.TextRedactor (and satisfies
// redact.JSONRedactor) that returns every value UNCHANGED. Unlike
// StubRedactor, it never rewrites metadata fields, so a test can supply it
// wherever production code now requires a non-nil redactor (see
// push.NewPipeline's nil-redactor refusal) while keeping raw-value
// assertions written against the pre-refusal behavior valid.
//
// NoopRedactor carries no mutable state: the push pipeline invokes a shared
// redactor from multiple goroutines (one per concurrent upload), so a
// call-counting field here would be a data race under `go test -race`.
// Tests that need to observe call counts should use StubRedactor instead
// (single-goroutine use only) or add their own synchronized double.
type NoopRedactor struct{}

var _ ingest.TextRedactor = (*NoopRedactor)(nil)

// RedactMetadata returns meta unchanged (a shallow copy of the pointer's
// value, matching the "returns a copy, never mutates the original" contract).
func (r *NoopRedactor) RedactMetadata(meta *ingest.UnifiedMetadata) *ingest.UnifiedMetadata {
	if meta == nil {
		return nil
	}
	copied := *meta
	return &copied
}

// RedactJSON returns value unchanged.
func (r *NoopRedactor) RedactJSON(value any) any {
	return value
}

// Level returns "standard" as the noop redactor's reported level.
func (r *NoopRedactor) Level() string {
	return "standard"
}

// RuleSetVersion returns "0.0.0-noop" — this double applies no rules.
func (r *NoopRedactor) RuleSetVersion() string {
	return "0.0.0-noop"
}

// BoolPtr returns a pointer to b. Convenience for constructing tri-state
// config fields (e.g. config.PushFieldVisibility.GitRemote) in test literals,
// where Go's struct-literal syntax cannot take the address of a bool constant
// directly.
func BoolPtr(b bool) *bool {
	return &b
}

// RuleSetVersion returns "1.0.0" as the stub rule set version.
func (r *StubRedactor) RuleSetVersion() string {
	return "1.0.0"
}

// --- StubIndexer ---

// StubIndexer is a test double for ingest.TranscriptIndexer.
// Returns preconfigured entries per session, or an error.
// CalledWith records the DiscoveredSession passed to each IndexTranscript call,
// keyed by SessionID. Initialise CalledWith to enable call recording; if nil,
// recording is skipped.
type StubIndexer struct {
	Entries    map[ingest.SessionID][]schema.SessionEntry
	Err        error
	CalledWith map[ingest.SessionID]ingest.DiscoveredSession
	// Kind is the source kind this stub reports, returned VERBATIM.
	//
	// It has no default. An earlier version mapped the zero value to a file
	// source, which made the stub disagree with production about the one input
	// production refuses: a test double that forgot to declare a kind stayed green
	// while the real dispatch would have errored. A double whose contract differs
	// from the code it stands in for cannot prove anything about that code, so
	// every construction states its kind - exactly as every real indexer does.
	Kind ingest.TranscriptSourceKind
}

var _ ingest.TranscriptIndexer = (*StubIndexer)(nil)

// SourceKind reports the configured kind with no substitution, so an undeclared
// one reaches the dispatch and is refused there, as it would be in production.
func (idx *StubIndexer) SourceKind() ingest.TranscriptSourceKind {
	return idx.Kind
}

func (idx *StubIndexer) IndexTranscript(_ context.Context, session ingest.DiscoveredSession) ([]schema.SessionEntry, error) {
	if idx.CalledWith != nil {
		idx.CalledWith[session.SessionID] = session
	}
	if idx.Err != nil {
		return nil, idx.Err
	}
	return idx.Entries[session.SessionID], nil
}

// IndexTranscriptBytes delegates to IndexTranscript; the bytes argument is unused
// because StubIndexer returns pre-configured entries keyed by session ID.
func (idx *StubIndexer) IndexTranscriptBytes(ctx context.Context, session ingest.DiscoveredSession, _ []byte) ([]schema.SessionEntry, error) {
	return idx.IndexTranscript(ctx, session)
}

// --- StubAnalyzer ---

// StubAnalyzer is a test double for ingest.SessionAnalyzer.
// Tracks calls and returns configurable results.
type StubAnalyzer struct {
	ComputedSessionIDs []ingest.SessionID
	InsightDays        []string
	ComputeCount       int
	ComputeErr         error
	InsightsErr        error
}

var _ ingest.SessionAnalyzer = (*StubAnalyzer)(nil)

func (a *StubAnalyzer) ComputeMetrics(_ context.Context, sessionIDs []ingest.SessionID) (int, error) {
	if a.ComputeErr != nil {
		return 0, a.ComputeErr
	}
	a.ComputedSessionIDs = append(a.ComputedSessionIDs, sessionIDs...)
	if a.ComputeCount > 0 {
		return a.ComputeCount, nil
	}
	return len(sessionIDs), nil
}

func (a *StubAnalyzer) ComputeInsights(_ context.Context, days []string) error {
	if a.InsightsErr != nil {
		return a.InsightsErr
	}
	a.InsightDays = append(a.InsightDays, days...)
	return nil
}

// --- StubSessionClassifier ---

// StubSessionClassifier is a test double for ingest.SessionClassifier.
// It records every sessionID passed to Annotate for assertion in tests.
// Err, if non-nil, is returned for every Annotate call.
type StubSessionClassifier struct {
	mu               sync.Mutex
	Annotated        []ingest.SessionID
	ProfiledCalls    []ingest.SessionID
	ProfiledProfiler *ingest.IndexProfiler
	Err              error // returned for every Annotate call if non-nil
}

var _ ingest.SessionClassifier = (*StubSessionClassifier)(nil)
var _ ingest.ProfiledSessionClassifier = (*StubSessionClassifier)(nil)

func (s *StubSessionClassifier) Annotate(_ context.Context, sessionID ingest.SessionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Annotated = append(s.Annotated, sessionID)
	return s.Err
}

func (s *StubSessionClassifier) AnnotateWithProfile(_ context.Context, sessionID ingest.SessionID, profiler *ingest.IndexProfiler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Annotated = append(s.Annotated, sessionID)
	s.ProfiledCalls = append(s.ProfiledCalls, sessionID)
	s.ProfiledProfiler = profiler
	return s.Err
}

// --- StubIngestLogger ---

// StubIngestLogger is a test double for ingest.IngestLogger.
type StubIngestLogger struct {
	Entries []ingest.IngestLogEntry
	Err     error
}

var _ ingest.IngestLogger = (*StubIngestLogger)(nil)

func (l *StubIngestLogger) LogIngestRun(_ context.Context, entry ingest.IngestLogEntry) error {
	if l.Err != nil {
		return l.Err
	}
	l.Entries = append(l.Entries, entry)
	return nil
}

// --- StubIndexLogger ---

// StubIndexLogger is a test double for ingest.IndexLogger.
type StubIndexLogger struct {
	Entries []ingest.IndexLogEntry
	Err     error
}

var _ ingest.IndexLogger = (*StubIndexLogger)(nil)

func (l *StubIndexLogger) LogIndexEntry(_ context.Context, entry ingest.IndexLogEntry) error {
	if l.Err != nil {
		return l.Err
	}
	l.Entries = append(l.Entries, entry)
	return nil
}

// --- StubPushStore ---

// StubPushStore is a test double for push candidate and pipeline persistence.
// Per-method error fields allow simulating failures.
// Sessions holds the list returned by UnpushedSessions / UnpushedSessionsByProvider.
// AllSessions holds the list returned by AllPushableSessions.
// HeldSessions holds the list returned by SessionsWithoutMetrics.
//
// StubPushStore is safe for concurrent use from multiple goroutines (the push
// pipeline may persist receipts and write its audit log concurrently via errgroup).
type StubPushStore struct {
	mu sync.Mutex

	Sessions            []ingest.PushSessionRow
	AllSessions         []ingest.PushSessionRow
	HeldSessions        []ingest.HeldSession
	SavedPublicationIDs []ingest.SessionID
	// SavedPublicationLicense captures the applied license from the most recent
	// authoritative receipt so tests can assert it was persisted.
	SavedPublicationLicense schema.License
	PushLogs                []ingest.PushLogEntry
	// Metrics holds pre-mapped QualityMetrics keyed by SessionID.
	Metrics map[ingest.SessionID]*schema.QualityMetrics
	// Entries holds session entries keyed by SessionID, returned by ListEntries.
	Entries map[ingest.SessionID][]schema.SessionEntry
	// Associations holds durable current commit associations keyed by session ID.
	Associations        map[ingest.SessionID][]ingest.CurrentCommitAssociation
	Publications        map[string]store.PublicationRecord
	PublicationAttempts []store.PublicationAttemptDiagnostic

	UnpushedErr        error
	SavePublicationErr error
	InsertLogErr       error
	HeldErr            error
	// GetQualityMetricsErr is returned by GetQualityMetrics when non-nil.
	GetQualityMetricsErr error
	// ListEntriesErr is returned by ListEntries when non-nil.
	ListEntriesErr        error
	ListEntriesFailOnCall int
	ListEntriesCalls      int
	// AssociationsErr is returned by ListCurrentSessionCommitAssociations when non-nil.
	AssociationsErr error
}

func (s *StubPushStore) SavePublication(_ context.Context, record store.PublicationRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SavePublicationErr != nil {
		return s.SavePublicationErr
	}
	if s.Publications == nil {
		s.Publications = map[string]store.PublicationRecord{}
	}
	s.Publications[stubPublicationKey(record.VillageOrigin, record.OwnerUserID, record.ProjectHash, record.SessionID)] = record
	s.SavedPublicationIDs = append(s.SavedPublicationIDs, ingest.SessionID(record.SessionID))
	if record.Receipt.Applied.License != nil {
		s.SavedPublicationLicense = *record.Receipt.Applied.License
	}
	return nil
}
func (s *StubPushStore) Publication(_ context.Context, origin, owner string, projectHash schema.ProjectHash, sessionID string) (*store.PublicationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.Publications[stubPublicationKey(origin, owner, projectHash, sessionID)]
	if !ok {
		return nil, nil
	}
	return &record, nil
}

func stubPublicationKey(origin, owner string, projectHash schema.ProjectHash, sessionID string) string {
	return strings.Join([]string{origin, owner, projectHash.String(), sessionID}, "\x00")
}

func (s *StubPushStore) RecordPublicationAttempt(_ context.Context, diagnostic store.PublicationAttemptDiagnostic) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PublicationAttempts = append(s.PublicationAttempts, diagnostic)
	return nil
}

func (s *StubPushStore) UnpushedSessions(_ context.Context) ([]ingest.PushSessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Sessions, s.UnpushedErr
}

func (s *StubPushStore) UnpushedSessionsByProvider(_ context.Context, provider string) ([]ingest.PushSessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.UnpushedErr != nil {
		return nil, s.UnpushedErr
	}
	var filtered []ingest.PushSessionRow
	for _, sess := range s.Sessions {
		if sess.ModelHarness == provider {
			filtered = append(filtered, sess)
		}
	}
	return filtered, nil
}

func (s *StubPushStore) AllPushableSessions(_ context.Context) ([]ingest.PushSessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AllSessions, s.UnpushedErr
}

func (s *StubPushStore) InsertPushLog(_ context.Context, entry ingest.PushLogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.InsertLogErr != nil {
		return s.InsertLogErr
	}
	s.PushLogs = append(s.PushLogs, entry)
	return nil
}

func (s *StubPushStore) SessionsWithoutMetrics(_ context.Context) ([]ingest.HeldSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.HeldSessions, s.HeldErr
}

func (s *StubPushStore) GetQualityMetrics(_ context.Context, sessionID ingest.SessionID) (*schema.QualityMetrics, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.GetQualityMetricsErr != nil {
		return nil, s.GetQualityMetricsErr
	}
	if s.Metrics == nil {
		return nil, nil
	}
	return s.Metrics[sessionID], nil
}

func (s *StubPushStore) ListEntries(_ context.Context, sessionID ingest.SessionID) ([]schema.SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ListEntriesCalls++
	if s.ListEntriesFailOnCall > 0 && s.ListEntriesCalls == s.ListEntriesFailOnCall {
		return nil, s.ListEntriesErr
	}
	if s.ListEntriesErr != nil && s.ListEntriesFailOnCall == 0 {
		return nil, s.ListEntriesErr
	}
	if s.Entries == nil {
		return nil, nil
	}
	return s.Entries[sessionID], nil
}

func (s *StubPushStore) ListCurrentSessionCommitAssociations(_ context.Context, sessionID ingest.SessionID) ([]ingest.CurrentCommitAssociation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AssociationsErr != nil {
		return nil, s.AssociationsErr
	}
	if s.Associations == nil {
		return nil, nil
	}
	return s.Associations[sessionID], nil
}

// --- StubPublisher ---

// StubPublishCall records a single call to StubPublisher.Publish.
// TranscriptBody captures the fully-read transcript body bytes so tests can
// assert on the uploaded content shape (e.g. the structured TranscriptContent
// envelope).
type StubPublishCall struct {
	MetadataJSON   []byte
	Filename       string
	TranscriptBody []byte
}

// StubPublisher is a test double for legacy and authoritative Village publishing.
// Results is consumed in FIFO order (one result per Publish call).
// StatusCode is returned on every call (defaults to 201 if zero).
// Err is returned on every call when non-nil.
//
// StubPublisher is safe for concurrent use from multiple goroutines (the push
// pipeline may call Publish concurrently via errgroup).
type StubPublisher struct {
	mu sync.Mutex

	Results    []*ingest.PublishResult
	StatusCode int
	Err        error

	Calls              []StubPublishCall
	AuthoritativeCalls []schema.AuthoritativePublishRequest
	ReceiptContentHash schema.TranscriptContentHash
	ReceiptFingerprint schema.PublishRequestFingerprint

	// Schema-version preflight double (push version-negotiation gate).
	// SchemaVersionResp is returned by GetSchemaVersion; nil means the village
	// advertised no window. SchemaVersionErr forces a transport error.
	// SchemaVersionCalls counts invocations so tests can assert the (formerly
	// dead) preflight is actually called.
	SchemaVersionResp   *schema.SchemaVersionResponse
	SchemaVersionErr    error
	SchemaVersionStatus int
	SchemaVersionCalls  int
}

// GetSchemaVersion is the village schema-version preflight double. Together with
// PublishAuthoritative it makes StubPublisher satisfy the push transport contract.
func (s *StubPublisher) GetSchemaVersion(_ context.Context) (*schema.SchemaVersionResponse, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SchemaVersionCalls++
	status := s.SchemaVersionStatus
	if status == 0 {
		status = 200
	}
	if s.SchemaVersionErr != nil {
		return nil, status, s.SchemaVersionErr
	}
	return s.SchemaVersionResp, status, nil
}

func (s *StubPublisher) Publish(_ context.Context, metadataJSON []byte, transcriptBody io.Reader, filename string) (*ingest.PublishResult, int, error) {
	var body []byte
	if transcriptBody != nil {
		body, _ = io.ReadAll(transcriptBody)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.Calls = append(s.Calls, StubPublishCall{MetadataJSON: metadataJSON, Filename: filename, TranscriptBody: body})
	if s.Err != nil {
		return nil, s.StatusCode, s.Err
	}
	var result *ingest.PublishResult
	if len(s.Results) > 0 {
		result = s.Results[0]
		s.Results = s.Results[1:]
	}
	sc := s.StatusCode
	if sc == 0 {
		sc = 201
	}
	return result, sc, nil
}

func (s *StubPublisher) PublishAuthoritative(ctx context.Context, request schema.AuthoritativePublishRequest, transcriptBody io.Reader, filename string) (schema.AuthoritativePublishResponse, int, error) {
	s.mu.Lock()
	s.AuthoritativeCalls = append(s.AuthoritativeCalls, request)
	s.mu.Unlock()
	metadata, err := json.Marshal(request)
	if err != nil {
		return schema.AuthoritativePublishResponse{}, 0, err
	}
	_, status, publishErr := s.Publish(ctx, metadata, transcriptBody, filename)
	if publishErr != nil {
		return schema.AuthoritativePublishResponse{}, status, publishErr
	}
	raw, err := AuthoritativePublishReceiptFromRequest(request, status == 201)
	if err != nil {
		return schema.AuthoritativePublishResponse{}, status, err
	}
	response, err := schema.DecodePublishResponse(raw)
	if s.ReceiptContentHash != "" {
		response.ContentHash = s.ReceiptContentHash
	}
	if s.ReceiptFingerprint != "" {
		response.RequestOperationFingerprint = s.ReceiptFingerprint
	}
	return response, status, err
}
func (s *StubPublisher) UpdateOwner(_ context.Context, id schema.TranscriptID, request schema.OwnerTranscriptUpdateRequest) (schema.OwnerTranscriptUpdateResponse, int, error) {
	visibility := schema.TranscriptUpdateVisibility(schema.VisibilityPrivate)
	if request.Visibility != nil {
		visibility = *request.Visibility
	}
	return schema.OwnerTranscriptUpdateResponse{TranscriptID: id, TranscriptURL: "https://village.example/transcripts/" + id.String(), Visibility: visibility, UpdatedAt: 1}, 200, nil
}

// --- StubGitDiffAnalyzer ---

// StubGitDiffAnalyzer is a test mock for ingest.GitDiffAnalyzer.
// Configure FileContents to return specific file contents for (file, commit) pairs.
// Configure Commits to return commits for GetSessionCommits.
// Configure CommitInfos to return full metadata for GetSessionCommitsWithMetadata.
type StubGitDiffAnalyzer struct {
	// FileContents maps "file@commit" to file contents.
	FileContents map[string][]byte
	// Commits to return from GetSessionCommits (hashes only).
	Commits []string
	// CommitInfos to return from GetSessionCommitsWithMetadata.
	CommitInfos []ingest.CommitInfo
	// Errors per method.
	GetFileErr            error
	GetCommitsErr         error
	GetCommitsWithMetaErr error
}

var _ ingest.GitDiffAnalyzer = (*StubGitDiffAnalyzer)(nil)

func (s *StubGitDiffAnalyzer) GetFileAtCommit(_ context.Context, _, file, commit string) ([]byte, error) {
	if s.GetFileErr != nil {
		return nil, s.GetFileErr
	}
	key := file + "@" + commit
	content, ok := s.FileContents[key]
	if !ok {
		return nil, fmt.Errorf("file %s not found at commit %s", file, commit)
	}
	return content, nil
}

func (s *StubGitDiffAnalyzer) GetSessionCommits(_ context.Context, _ string, _, _ time.Time) ([]string, error) {
	if s.GetCommitsErr != nil {
		return nil, s.GetCommitsErr
	}
	return s.Commits, nil
}

func (s *StubGitDiffAnalyzer) GetSessionCommitsWithMetadata(_ context.Context, _ string, _, _ time.Time) ([]ingest.CommitInfo, error) {
	if s.GetCommitsWithMetaErr != nil {
		return nil, s.GetCommitsWithMetaErr
	}
	if s.CommitInfos == nil {
		return []ingest.CommitInfo{}, nil
	}
	return s.CommitInfos, nil
}

// ---------------------------------------------------------------------------
// Annotation test fixtures
// ---------------------------------------------------------------------------

// Annotation type_id constants for the 4 seed annotation types from V13 migration.
// Use these in tests instead of bare string literals (V21: no stringly-typed APIs).
const (
	TestTypeIDSessionApproval = "quality.session_approval"
	TestTypeIDSessionOutcome  = "quality.session_outcome"
	TestTypeIDUserFrustration = "quality.user_frustration"
	TestTypeIDSessionScope    = "metadata.session_scope"

	// Entry-level annotation type IDs (V18 migration).
	TestTypeIDFrustrationSignal  = "quality.frustration_signal"
	TestTypeIDResolutionEvidence = "quality.resolution_evidence"
)

// DefaultAnnotationType returns a canonical test schema.ValueDomain for an enumerated
// binary annotation type (approve/deny), mirroring the quality.session_approval seed type.
// Use this in tests that need a fully-specified ValueDomain without hitting the DB.
func DefaultAnnotationType() schema.ValueDomain {
	return schema.ValueDomain{
		Kind:              schema.DomainEnumerated,
		Datatype:          schema.DatatypeText,
		PermissibleValues: []string{"approve", "deny"},
	}
}

// DefaultAnnotatorParams returns canonical CreateAnnotatorParams for a rule-based
// system annotator. Use this in tests that need to insert an annotator.
func DefaultAnnotatorParams() schema.AnnotatorKind {
	return schema.AnnotatorRule
}

// --- StubGitRepository ---

// Shared fixture values for StubGitRepository. Use these instead of inline
// literals so a schema change touches one place, not N test files.
const (
	// TestFeatureBranch is the seeded open feature branch.
	TestFeatureBranch = "feat/graph-cache"
	// TestMergedBranchName is the seeded merged branch.
	TestMergedBranchName = "feat/project-overview"
	// TestBaseCommitHash is the seeded merge-base hash (40 hex chars).
	TestBaseCommitHash = "1111111111111111111111111111111111111111"
	// TestHeadCommitHash is the seeded feature-branch tip hash.
	TestHeadCommitHash = "2222222222222222222222222222222222222222"
	// TestMergeCommitHash is the seeded merge-commit hash for TestMergedBranchName.
	TestMergeCommitHash = "3333333333333333333333333333333333333333"
	// TestRenameOldPath / TestRenameNewPath are the seeded rename pair.
	TestRenameOldPath = "internal/api/handler.go"
	TestRenameNewPath = "internal/api/handlers.go"
)

// StubGitRepository is a test double for gitops.Repository.
// Per-method result fields hold the seeded data; per-method error fields
// allow simulating partial failures (e.g. branches list but no merge data).
//
// Lookup semantics mirror real git where it matters:
//   - BranchState errors for unseeded branches (real git errors on unknown refs;
//     returning nil, nil would hide misuse behind nil-pointer panics).
//   - FileAtCommit errors for unseeded "commit:path" keys (real git errors when
//     a file is missing at a commit).
//   - FilesAtCommit returns the seeded subset of the requested paths; unseeded
//     paths are simply absent (matching the batch contract: missing paths are
//     not errors).
//   - ListFiles / Commits / CommitsInRange return empty results for unseeded
//     keys (empty is a valid answer for these queries); use the Err fields to
//     simulate failures.
//   - DiffStats returns DiffStatsResult verbatim for any base/head pair and
//     errors when unseeded (it returns a pointer; nil, nil would hide misuse
//     behind nil-pointer panics).
type StubGitRepository struct {
	DefaultBranchName string
	// BranchList is returned by Branches verbatim (seed it already excluding
	// the default branch, matching the Repository contract).
	BranchList []string
	// BranchStates maps branch name → state.
	BranchStates map[string]*gitops.BranchState
	// Merged is returned by MergedBranches, capped at the requested limit.
	Merged []gitops.MergedBranch
	// FileContents maps "commit:path" → file content.
	FileContents map[string][]byte
	// FilesByRef maps ref → tracked file list.
	FilesByRef map[string][]string
	// CommitsByRef maps ref → commit metadata (newest first), capped at the
	// requested limit.
	CommitsByRef map[string][]gitops.Commit
	// RangeHashes maps "base..head" → hashes ahead of the merge-base.
	RangeHashes map[string][]string
	// DiffStatsResult is returned by DiffStats verbatim for any base/head
	// pair; DiffStats errors when it is nil and DiffStatsErr is unset.
	DiffStatsResult *gitops.DiffStats
	// DiffHunksResult is returned by DiffHunks for any base/head pair; when a
	// non-empty paths filter is passed, only matching files are returned. Nil is
	// a valid result (no diffs) — DiffHunks does not error on nil.
	DiffHunksResult []gitops.FileDiff
	// BlameByRefPath maps "ref\x00path" → per-line commit hashes (line 1 at [0]).
	BlameByRefPath map[string][]string
	// RevertedByRef maps ref → the set of commit hashes reverted on that ref.
	RevertedByRef map[string]map[string]bool

	// Per-method error overrides. If non-nil, the corresponding method returns this error.
	DefaultBranchErr  error
	BranchesErr       error
	BranchStateErr    error
	DiffStatsErr      error
	DiffHunksErr      error
	MergedBranchesErr error
	FileAtCommitErr   error
	FilesAtCommitErr  error
	ListFilesErr      error
	CommitsErr        error
	CommitsInRangeErr error
}

var _ gitops.Repository = (*StubGitRepository)(nil)

// DefaultGitRepository returns a StubGitRepository seeded with a typical
// repo: a default branch, one open feature branch (ahead 2 / behind 1, with
// modify + add + rename changes), one merged branch, commit metadata, and
// per-file diff stats mirroring the feature branch's ChangedFiles.
func DefaultGitRepository() *StubGitRepository {
	startMs := TestSessionStartTime.UnixMilli()
	renameOld := TestRenameOldPath
	return &StubGitRepository{
		DefaultBranchName: TestDefaultBranch,
		BranchList:        []string{TestFeatureBranch},
		BranchStates: map[string]*gitops.BranchState{
			TestFeatureBranch: {
				Name:        TestFeatureBranch,
				MergeBase:   TestBaseCommitHash,
				AheadCount:  2,
				BehindCount: 1,
				ChangedFiles: []gitops.FileChange{
					{Path: "internal/ingest/pipeline.go", Status: gitops.FileStatusModified},
					{Path: "internal/gitops/repo.go", Status: gitops.FileStatusAdded},
					{Path: TestRenameNewPath, Status: gitops.FileStatusRenamed, OldPath: &renameOld},
				},
			},
		},
		Merged: []gitops.MergedBranch{
			{Name: TestMergedBranchName, MergedAtMs: startMs, MergeCommit: TestMergeCommitHash},
		},
		FileContents: map[string][]byte{
			TestBaseCommitHash + ":internal/ingest/pipeline.go": []byte("package ingest\n"),
		},
		FilesByRef: map[string][]string{
			TestDefaultBranch: {"internal/ingest/pipeline.go", TestRenameOldPath},
			TestFeatureBranch: {"internal/ingest/pipeline.go", "internal/gitops/repo.go", TestRenameNewPath},
		},
		CommitsByRef: map[string][]gitops.Commit{
			TestDefaultBranch: {
				{Hash: TestBaseCommitHash, Subject: "add pipeline", TimeMs: startMs, AuthorEmail: TestEmail},
			},
			TestFeatureBranch: {
				{Hash: TestHeadCommitHash, Subject: "feature work", TimeMs: startMs, AuthorEmail: TestEmail},
				{Hash: TestBaseCommitHash, Subject: "add pipeline", TimeMs: startMs, AuthorEmail: TestEmail},
			},
		},
		RangeHashes: map[string][]string{
			TestBaseCommitHash + ".." + TestFeatureBranch: {TestHeadCommitHash},
		},
		// One row per ChangedFiles entry on the feature branch; totals are the
		// column sums.
		DiffStatsResult: &gitops.DiffStats{
			LinesAdded:   133,
			LinesRemoved: 5,
			PerFile: []gitops.FileDiffStat{
				{Path: "internal/ingest/pipeline.go", Added: 12, Removed: 4},
				{Path: "internal/gitops/repo.go", Added: 120},
				{Path: TestRenameNewPath, Added: 1, Removed: 1},
			},
		},
	}
}

// NoGitRepository returns a StubGitRepository where every method errors
// (simulates a project whose canonical_cwd is not a git repo).
func NoGitRepository() *StubGitRepository {
	return &StubGitRepository{
		DefaultBranchErr:  fmt.Errorf(ErrNotGitRepo),
		BranchesErr:       fmt.Errorf(ErrNotGitRepo),
		BranchStateErr:    fmt.Errorf(ErrNotGitRepo),
		DiffStatsErr:      fmt.Errorf(ErrNotGitRepo),
		DiffHunksErr:      fmt.Errorf(ErrNotGitRepo),
		MergedBranchesErr: fmt.Errorf(ErrNotGitRepo),
		FileAtCommitErr:   fmt.Errorf(ErrNotGitRepo),
		FilesAtCommitErr:  fmt.Errorf(ErrNotGitRepo),
		ListFilesErr:      fmt.Errorf(ErrNotGitRepo),
		CommitsErr:        fmt.Errorf(ErrNotGitRepo),
		CommitsInRangeErr: fmt.Errorf(ErrNotGitRepo),
	}
}

func (s *StubGitRepository) DefaultBranch(_ context.Context) (string, error) {
	if s.DefaultBranchErr != nil {
		return "", s.DefaultBranchErr
	}
	return s.DefaultBranchName, nil
}

func (s *StubGitRepository) Branches(_ context.Context) ([]string, error) {
	if s.BranchesErr != nil {
		return nil, s.BranchesErr
	}
	return s.BranchList, nil
}

func (s *StubGitRepository) BranchState(_ context.Context, branch string) (*gitops.BranchState, error) {
	if s.BranchStateErr != nil {
		return nil, s.BranchStateErr
	}
	state, ok := s.BranchStates[branch]
	if !ok {
		return nil, fmt.Errorf("stub: no branch state seeded for %q", branch)
	}
	return state, nil
}

func (s *StubGitRepository) DiffStats(_ context.Context, base, head string) (*gitops.DiffStats, error) {
	if s.DiffStatsErr != nil {
		return nil, s.DiffStatsErr
	}
	if s.DiffStatsResult == nil {
		return nil, fmt.Errorf("stub: no diff stats seeded for %s...%s", base, head)
	}
	return s.DiffStatsResult, nil
}

func (s *StubGitRepository) DiffHunks(_ context.Context, _, _ string, paths []string, _ int) ([]gitops.FileDiff, error) {
	if s.DiffHunksErr != nil {
		return nil, s.DiffHunksErr
	}
	if len(paths) == 0 {
		return s.DiffHunksResult, nil
	}
	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[p] = true
	}
	var out []gitops.FileDiff
	for _, fd := range s.DiffHunksResult {
		if want[fd.Path] {
			out = append(out, fd)
		}
	}
	return out, nil
}

func (s *StubGitRepository) BlameCommits(_ context.Context, ref, path string) ([]string, error) {
	if s.BlameByRefPath == nil {
		return []string{}, nil
	}
	return s.BlameByRefPath[ref+"\x00"+path], nil
}

func (s *StubGitRepository) RevertedCommits(_ context.Context, ref string) (map[string]bool, error) {
	if s.RevertedByRef == nil {
		return map[string]bool{}, nil
	}
	if set, ok := s.RevertedByRef[ref]; ok {
		return set, nil
	}
	return map[string]bool{}, nil
}

func (s *StubGitRepository) MergedBranches(_ context.Context, limit int) ([]gitops.MergedBranch, error) {
	if s.MergedBranchesErr != nil {
		return nil, s.MergedBranchesErr
	}
	merged := s.Merged
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

func (s *StubGitRepository) FileAtCommit(_ context.Context, commit, path string) ([]byte, error) {
	if s.FileAtCommitErr != nil {
		return nil, s.FileAtCommitErr
	}
	content, ok := s.FileContents[commit+":"+path]
	if !ok {
		return nil, fmt.Errorf("stub: no content seeded for %s:%s", commit, path)
	}
	return content, nil
}

func (s *StubGitRepository) FilesAtCommit(_ context.Context, commit string, paths []string) (map[string][]byte, error) {
	if s.FilesAtCommitErr != nil {
		return nil, s.FilesAtCommitErr
	}
	contents := make(map[string][]byte, len(paths))
	for _, p := range paths {
		if c, ok := s.FileContents[commit+":"+p]; ok {
			contents[p] = c
		}
	}
	return contents, nil
}

func (s *StubGitRepository) ListFiles(_ context.Context, ref string) ([]string, error) {
	if s.ListFilesErr != nil {
		return nil, s.ListFilesErr
	}
	return s.FilesByRef[ref], nil
}

func (s *StubGitRepository) Commits(_ context.Context, ref string, limit int) ([]gitops.Commit, error) {
	if s.CommitsErr != nil {
		return nil, s.CommitsErr
	}
	commits := s.CommitsByRef[ref]
	if limit > 0 && len(commits) > limit {
		commits = commits[:limit]
	}
	return commits, nil
}

func (s *StubGitRepository) CommitsInRange(_ context.Context, base, head string) ([]string, error) {
	if s.CommitsInRangeErr != nil {
		return nil, s.CommitsInRangeErr
	}
	return s.RangeHashes[base+".."+head], nil
}
