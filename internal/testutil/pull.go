package testutil

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// FailingFS is a per-method error-injecting FileSystem decorator.
// ---------------------------------------------------------------------------

// FailingFS wraps an ingest.FileSystem and injects an error on a chosen method.
// It is the shared test double for the pull pipeline's compensation-FAILURE
// branch: wrap a MemFS, set RemoveAllErr, and
// the pipeline's compensating fs.RemoveAll(pullDir) fails — exercising the
// "error names the orphan dir + --force repair" path. Living in testutil (not
// inlined in a test) means a schema change touches one place.
//
// Each *Err field, when non-nil, is returned INSTEAD of delegating to Inner. A
// nil field delegates transparently, so callers fail exactly one operation while
// the rest of the filesystem behaves normally. RemoveAllOnPaths optionally
// restricts RemoveAllErr to RemoveAll calls whose path is in the set (so a
// pipeline that legitimately RemoveAll's a temp dir still succeeds, but the
// compensating RemoveAll of the pull dir fails).
type FailingFS struct {
	Inner ingest.FileSystem

	ReadFileErr  error
	WriteFileErr error
	MkdirAllErr  error
	StatErr      error
	LstatErr     error
	WalkDirErr   error
	RenameErr    error
	ReadDirErr   error
	RemoveErr    error
	RemoveAllErr error
	CopyFileErr  error

	// RemoveAllOnPaths, when non-empty, restricts RemoveAllErr to RemoveAll calls
	// whose exact path is a key in this set. Empty ⇒ RemoveAllErr applies to ALL
	// RemoveAll calls.
	RemoveAllOnPaths map[string]bool

	// RemoveAllSkipFirstN suppresses RemoveAllErr for the first N matching RemoveAll
	// calls (then fails the rest). This distinguishes the pipeline's harmless
	// pre-rename clear of a (possibly absent) pull dir from the LATER compensating
	// RemoveAll of the same path after a DB-TX failure: set RemoveAllSkipFirstN=1 to
	// fail only the compensation. 0 ⇒ fail from the first matching call.
	RemoveAllSkipFirstN int
	removeAllMatchCount int

	// RemoveAllCalls records every RemoveAll path (in call order) for assertions.
	RemoveAllCalls []string
}

var _ ingest.FileSystem = (*FailingFS)(nil)

// NewFailingFS wraps inner with no injected errors (delegates everything). Set
// the *Err fields to inject failures.
func NewFailingFS(inner ingest.FileSystem) *FailingFS {
	return &FailingFS{Inner: inner}
}

func (f *FailingFS) ReadFile(path string) ([]byte, error) {
	if f.ReadFileErr != nil {
		return nil, f.ReadFileErr
	}
	return f.Inner.ReadFile(path)
}

func (f *FailingFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	if f.WriteFileErr != nil {
		return f.WriteFileErr
	}
	return f.Inner.WriteFile(path, data, perm)
}

func (f *FailingFS) MkdirAll(path string, perm os.FileMode) error {
	if f.MkdirAllErr != nil {
		return f.MkdirAllErr
	}
	return f.Inner.MkdirAll(path, perm)
}

func (f *FailingFS) Stat(path string) (os.FileInfo, error) {
	if f.StatErr != nil {
		return nil, f.StatErr
	}
	return f.Inner.Stat(path)
}

func (f *FailingFS) Lstat(path string) (os.FileInfo, error) {
	if f.LstatErr != nil {
		return nil, f.LstatErr
	}
	return f.Inner.Lstat(path)
}

func (f *FailingFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	if f.WalkDirErr != nil {
		return f.WalkDirErr
	}
	return f.Inner.WalkDir(root, fn)
}

func (f *FailingFS) Rename(oldpath, newpath string) error {
	if f.RenameErr != nil {
		return f.RenameErr
	}
	return f.Inner.Rename(oldpath, newpath)
}

func (f *FailingFS) ReadDir(path string) ([]os.DirEntry, error) {
	if f.ReadDirErr != nil {
		return nil, f.ReadDirErr
	}
	return f.Inner.ReadDir(path)
}

func (f *FailingFS) Remove(path string) error {
	if f.RemoveErr != nil {
		return f.RemoveErr
	}
	return f.Inner.Remove(path)
}

func (f *FailingFS) RemoveAll(path string) error {
	f.RemoveAllCalls = append(f.RemoveAllCalls, path)
	if f.RemoveAllErr != nil && (len(f.RemoveAllOnPaths) == 0 || f.RemoveAllOnPaths[path]) {
		f.removeAllMatchCount++
		if f.removeAllMatchCount > f.RemoveAllSkipFirstN {
			return f.RemoveAllErr
		}
	}
	return f.Inner.RemoveAll(path)
}

func (f *FailingFS) CopyFile(src, dst string, perm os.FileMode) error {
	if f.CopyFileErr != nil {
		return f.CopyFileErr
	}
	return f.Inner.CopyFile(src, dst, perm)
}

// ---------------------------------------------------------------------------
// StubVillageReader — test double for the pull pipeline's narrow VillageReader
// ---------------------------------------------------------------------------

// StubVillageReader is a configurable test double for the pull pipeline's narrow
// village-reader dependency (NegotiatePull + the four pure-data
// GETs). It records call counts so tests can assert the NEGOTIATE-exactly-once
// invariant and the conditional-GET (If-None-Match) wiring.
//
// Per-method error fields inject failures (set them to the village.ErrPull*
// sentinels to exercise the PullStatus mapping). ContentBody is returned as the
// blob bytes; ContentETag as the served-blob ETag; ContentErr (e.g.
// village.ErrNotModified) forces the 304 path.
type StubVillageReader struct {
	// NegotiateErr, when non-nil, fails the NEGOTIATE stage.
	NegotiateErr   error
	NegotiateCalls int

	// ListResponses is returned (in order) by successive ListPullableTranscripts
	// calls; the last is repeated once exhausted. ListErr fails the call.
	ListResponses []*schema.PullListResponse
	ListErr       error
	ListCalls     int

	// Meta is returned by GetPullTranscript (keyed by transcript ID); MetaErr
	// fails it. MetaByID takes precedence when non-nil.
	Meta      *schema.PullTranscriptInfo
	MetaByID  map[schema.TranscriptID]*schema.PullTranscriptInfo
	MetaErr   error
	MetaCalls int

	// ContentBody/ContentETag are returned by GetPullTranscriptContent on success.
	// ContentErr (e.g. village.ErrNotModified or a sentinel) fails/short-circuits.
	// LastIfNoneMatch records the conditional-GET header the pipeline sent.
	ContentBody     []byte
	ContentETag     string
	ContentErr      error
	ContentCalls    int
	LastIfNoneMatch string

	// Annotations is returned by GetPullTranscriptAnnotations (keyed by transcript
	// ID via AnnotationsByID, else the flat Annotations). AnnotationsErr fails it.
	Annotations      []schema.PullAnnotation
	AnnotationsByID  map[schema.TranscriptID][]schema.PullAnnotation
	AnnotationsErr   error
	AnnotationsCalls int
}

func (s *StubVillageReader) NegotiatePull(_ context.Context) error {
	s.NegotiateCalls++
	return s.NegotiateErr
}

func (s *StubVillageReader) ListPullableTranscripts(_ context.Context, _, _ int) (*schema.PullListResponse, error) {
	s.ListCalls++
	if s.ListErr != nil {
		return nil, s.ListErr
	}
	idx := s.ListCalls - 1
	if idx >= len(s.ListResponses) {
		if len(s.ListResponses) == 0 {
			return &schema.PullListResponse{}, nil
		}
		idx = len(s.ListResponses) - 1
	}
	return s.ListResponses[idx], nil
}

func (s *StubVillageReader) GetPullTranscript(_ context.Context, id schema.TranscriptID) (*schema.PullTranscriptInfo, error) {
	s.MetaCalls++
	if s.MetaErr != nil {
		return nil, s.MetaErr
	}
	if s.MetaByID != nil {
		if m, ok := s.MetaByID[id]; ok {
			return m, nil
		}
	}
	return s.Meta, nil
}

func (s *StubVillageReader) GetPullTranscriptContent(_ context.Context, _ schema.TranscriptID, ifNoneMatch string) (io.ReadCloser, string, error) {
	s.ContentCalls++
	s.LastIfNoneMatch = ifNoneMatch
	if s.ContentErr != nil {
		return nil, s.ContentETag, s.ContentErr
	}
	return io.NopCloser(bytes.NewReader(s.ContentBody)), s.ContentETag, nil
}

func (s *StubVillageReader) GetPullTranscriptAnnotations(_ context.Context, id schema.TranscriptID) ([]schema.PullAnnotation, error) {
	s.AnnotationsCalls++
	if s.AnnotationsErr != nil {
		return nil, s.AnnotationsErr
	}
	if s.AnnotationsByID != nil {
		if a, ok := s.AnnotationsByID[id]; ok {
			return a, nil
		}
	}
	return s.Annotations, nil
}

// ---------------------------------------------------------------------------
// StubPullStore — test double for the pull pipeline's PullStore dependency
// ---------------------------------------------------------------------------

// StubPullStore is a test double for the pull pipeline's PullStore. It
// records the CommitPull payload (so tests can assert the exact transcript +
// annotation rows the pipeline built) and lets tests inject a CommitPull error
// to drive the DB-TX-failure compensation branch.
type StubPullStore struct {
	// Commits records every CommitPull payload in call order.
	Commits []store.PullCommit
	// CommitErr, when non-nil, fails CommitPull (the DB-TX-failure injection).
	CommitErr error

	// Upserts records every UpsertPulledAnnotations batch in call order.
	Upserts [][]store.PulledAnnotationRow
	// Upsert{Created,Updated,Skipped} are the counts returned by
	// UpsertPulledAnnotations; UpsertErr fails it.
	UpsertCreated int
	UpsertUpdated int
	UpsertSkipped int
	UpsertErr     error

	// ListRows / GetRow back the read path.
	ListRows []store.PulledTranscriptRow
	ListErr  error
	GetRow   *store.PulledTranscriptRow
	GetErr   error
}

func (s *StubPullStore) CommitPull(_ context.Context, commit store.PullCommit) error {
	s.Commits = append(s.Commits, commit)
	return s.CommitErr
}

func (s *StubPullStore) UpsertPulledAnnotations(_ context.Context, annotations []store.PulledAnnotationRow) (created, updated, skipped int, err error) {
	s.Upserts = append(s.Upserts, annotations)
	if s.UpsertErr != nil {
		return 0, 0, 0, s.UpsertErr
	}
	return s.UpsertCreated, s.UpsertUpdated, s.UpsertSkipped, nil
}

func (s *StubPullStore) ListPulledTranscripts(_ context.Context) ([]store.PulledTranscriptRow, error) {
	return s.ListRows, s.ListErr
}

func (s *StubPullStore) GetPulledTranscript(_ context.Context, _ string, _ schema.TranscriptID) (*store.PulledTranscriptRow, error) {
	return s.GetRow, s.GetErr
}

// ---------------------------------------------------------------------------
// FixedClock — deterministic Clock for pull tests
// ---------------------------------------------------------------------------

// FixedClock is a deterministic Clock returning Millis for NowUnixMilli().
type FixedClock struct{ Millis int64 }

// NowUnixMilli returns the fixed millisecond timestamp.
func (c FixedClock) NowUnixMilli() int64 { return c.Millis }

// TestPulledAtMillis is the canonical fixed pull timestamp used across pull tests.
const TestPulledAtMillis int64 = 1_750_000_000_000

// NewFixedClock returns a FixedClock at TestPulledAtMillis.
func NewFixedClock() FixedClock { return FixedClock{Millis: TestPulledAtMillis} }
