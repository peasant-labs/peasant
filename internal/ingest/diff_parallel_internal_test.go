package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/diff_parallel.yaml
var diffParallelYAML []byte

// diffParallelArm names when a cancellation arrives. It is the only axis the
// cancellation cases vary: before the slice, from an expired deadline, and from
// inside a managed-layout lookup while the pool is already classifying.
type diffParallelArm string

const (
	diffParallelCanceledBefore diffParallelArm = "canceled"
	diffParallelDeadline       diffParallelArm = "deadline"
	diffParallelDuringDiff     diffParallelArm = "during-diff"
)

type diffParallelSessionCase struct {
	Name                    string `yaml:"name"`
	SessionID               string `yaml:"sessionID"`
	ParentID                string `yaml:"parentID"`
	Stored                  bool   `yaml:"stored"`
	IngestedMs              int64  `yaml:"ingestedMs"`
	ModTimeMs               int64  `yaml:"modTimeMs"`
	SchemaVersion           int    `yaml:"schemaVersion"`
	AdapterVersion          int    `yaml:"adapterVersion"`
	SourceEvidenceSupported bool   `yaml:"sourceEvidenceSupported"`
	PublicationReadiness    string `yaml:"publicationReadiness"`
	TrackedCursor           bool   `yaml:"trackedCursor"`
	StoredSeq               int64  `yaml:"storedSeq"`
	EventSeq                int64  `yaml:"eventSeq"`
	ExpectedStatus          string `yaml:"expected_status"`
}

type diffParallelCancellationCase struct {
	Name              string          `yaml:"name"`
	Arm               diffParallelArm `yaml:"arm"`
	CancelAfterLookup int64           `yaml:"cancelAfterLookups"`
}

type diffParallelFixture struct {
	RequiredCases             []string                       `yaml:"required_cases"`
	RequiredCancellationCases []string                       `yaml:"required_cancellation_cases"`
	Sessions                  []diffParallelSessionCase      `yaml:"sessions"`
	Cancellations             []diffParallelCancellationCase `yaml:"cancellations"`
}

func loadDiffParallelFixture(t *testing.T) diffParallelFixture {
	t.Helper()
	var fixture diffParallelFixture
	decoder := yaml.NewDecoder(bytes.NewReader(diffParallelYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the diff parallelism fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("the diff parallelism fixture must hold exactly one YAML document: %v", err)
	}
	present := make(map[string]bool, len(fixture.Sessions))
	for _, session := range fixture.Sessions {
		if session.Name == "" || present[session.Name] {
			t.Fatalf("the diff parallelism fixture has an empty or repeated case name %q", session.Name)
		}
		present[session.Name] = true
	}
	for _, required := range fixture.RequiredCases {
		if !present[required] {
			t.Fatalf("required fixture case %q is missing; the rule it pins would stop being tested", required)
		}
	}
	presentCancellation := make(map[string]bool, len(fixture.Cancellations))
	for _, cancellation := range fixture.Cancellations {
		if cancellation.Name == "" || presentCancellation[cancellation.Name] {
			t.Fatalf("the diff parallelism fixture has an empty or repeated cancellation name %q", cancellation.Name)
		}
		presentCancellation[cancellation.Name] = true
		switch cancellation.Arm {
		case diffParallelCanceledBefore, diffParallelDeadline, diffParallelDuringDiff:
		default:
			t.Fatalf("cancellation case %q names the unknown arm %q; add the arm and its assertion, or correct the fixture", cancellation.Name, cancellation.Arm)
		}
		if (cancellation.Arm == diffParallelDuringDiff) != (cancellation.CancelAfterLookup > 0) {
			t.Fatalf("cancellation case %q disagrees with itself: arm %q cancels after %d lookups; only the during-diff arm cancels inside the slice", cancellation.Name, cancellation.Arm, cancellation.CancelAfterLookup)
		}
	}
	for _, required := range fixture.RequiredCancellationCases {
		if !presentCancellation[required] {
			t.Fatalf("required cancellation case %q is missing; the cancellation path it pins would stop being tested", required)
		}
	}
	return fixture
}

// buildDiffParallelPipeline turns the fixture descriptors into the discovery
// slice and the stored-location cache DIFF reads. The filesystem is injectable
// so a cancellation case can wedge the managed-layout lookup without changing
// the descriptors.
func buildDiffParallelPipeline(t *testing.T, cases []diffParallelSessionCase, filesystem FileSystem) (*Pipeline, []DiscoveredSession) {
	t.Helper()
	outputDir, err := NewResolvedPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pipeline := &Pipeline{
		fs:            filesystem,
		config:        PipelineConfig{OutputDir: outputDir, Parallelism: 4},
		locationCache: make(map[SessionID]SessionLocation, len(cases)),
	}
	discovered := make([]DiscoveredSession, 0, len(cases))
	for _, testCase := range cases {
		sessionID, err := NewSessionID(testCase.SessionID)
		if err != nil {
			t.Fatalf("fixture case %q has an invalid session id: %v", testCase.Name, err)
		}
		session := DiscoveredSession{
			SessionID:        sessionID,
			Harness:          HarnessOpenCode,
			TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite,
			ModTime:          time.UnixMilli(testCase.ModTimeMs),
			EventSeq:         testCase.EventSeq,
		}
		if testCase.ParentID != "" {
			parent, err := NewSessionID(testCase.ParentID)
			if err != nil {
				t.Fatalf("fixture case %q has an invalid parent id: %v", testCase.Name, err)
			}
			session.ParentUUID = &parent
		}
		if testCase.Stored {
			ingested := testCase.IngestedMs
			location := SessionLocation{
				IngestedMs:              &ingested,
				SchemaVersion:           testCase.SchemaVersion,
				SourceEvidenceSupported: testCase.SourceEvidenceSupported,
				PublicationReadiness:    PublicationReadiness(testCase.PublicationReadiness),
			}
			if testCase.AdapterVersion != 0 {
				adapter := testCase.AdapterVersion
				location.AdapterVersion = &adapter
			}
			pipeline.locationCache[sessionID] = location
		}
		if testCase.TrackedCursor {
			if pipeline.seqCursorCache == nil {
				pipeline.seqCursorCache = make(map[SessionID]int64)
			}
			pipeline.seqCursorCache[sessionID] = testCase.StoredSeq
		}
		discovered = append(discovered, session)
	}
	return pipeline, discovered
}

// TestDiffParallelMatchesSequentialAndPreservesOrder pins the two properties
// the worker pool must keep from the sequential walk it replaces: every status
// is unchanged, and every result stays at its discovery index so FILTER can
// still group a parent with the children that follow it.
func TestDiffParallelMatchesSequentialAndPreservesOrder(t *testing.T) {
	fixture := loadDiffParallelFixture(t)
	pipeline, sessions := buildDiffParallelPipeline(t, fixture.Sessions, &OSFileSystem{})

	sequential := make([]DiffStatus, len(sessions))
	for index, session := range sessions {
		status, err := pipeline.classifySession(context.Background(), session)
		if err != nil {
			t.Fatalf("sequential classify of %q: %v", session.SessionID, err)
		}
		sequential[index] = status
	}

	progress := NewProgressState()
	result, err := pipeline.diff(context.Background(), sessions, progress)
	if err != nil {
		t.Fatalf("parallel diff: %v", err)
	}
	if len(result.Sessions) != len(sessions) {
		t.Fatalf("parallel diff classified %d sessions, want %d", len(result.Sessions), len(sessions))
	}
	for index := range sessions {
		entry := result.Sessions[index]
		if entry.Session.SessionID != sessions[index].SessionID {
			t.Fatalf("entry %d holds session %q, want the discovery session %q; the pool reordered the slice", index, entry.Session.SessionID, sessions[index].SessionID)
		}
		if entry.Status != sequential[index] {
			t.Fatalf("session %q classified %s by the pool and %s sequentially; the result must not depend on the worker", sessions[index].SessionID, entry.Status, sequential[index])
		}
		if got, want := entry.Status.String(), fixture.Sessions[index].ExpectedStatus; got != want {
			t.Fatalf("session %q classified %q, fixture expects %q", sessions[index].SessionID, got, want)
		}
	}
	if got := progress.Snapshot()[StageDiff]; got.Done != len(sessions) || got.Total != len(sessions) {
		t.Fatalf("DIFF progress = %d/%d, want %d/%d", got.Done, got.Total, len(sessions), len(sessions))
	}
}

// cancellingLookupFS cancels the run from inside a managed-layout lookup. That
// reproduces a cancellation arriving while the pool is already classifying,
// without depending on timing between goroutines.
type cancellingLookupFS struct {
	FileSystem
	lookups     atomic.Int64
	cancelAfter int64
	cancel      context.CancelFunc
}

func (filesystem *cancellingLookupFS) note() {
	if filesystem.lookups.Add(1) == filesystem.cancelAfter {
		filesystem.cancel()
	}
}

func (filesystem *cancellingLookupFS) Stat(path string) (os.FileInfo, error) {
	filesystem.note()
	return filesystem.FileSystem.Stat(path)
}

func (filesystem *cancellingLookupFS) ReadDir(path string) ([]os.DirEntry, error) {
	filesystem.note()
	return filesystem.FileSystem.ReadDir(path)
}

// TestDiffStopsOnContextCancellation pins that DIFF returns the run's context
// error once the context is done: before the slice, from an expired deadline,
// and from inside a managed-layout lookup while the pool is already running.
// A context that is already done before the slice classifies nothing.
func TestDiffStopsOnContextCancellation(t *testing.T) {
	fixture := loadDiffParallelFixture(t)
	for _, testCase := range fixture.Cancellations {
		t.Run(testCase.Name, func(t *testing.T) {
			var filesystem FileSystem = &OSFileSystem{}
			var ctx context.Context
			var cancel context.CancelFunc
			var want error
			switch testCase.Arm {
			case diffParallelCanceledBefore:
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
				want = context.Canceled
			case diffParallelDeadline:
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			case diffParallelDuringDiff:
				ctx, cancel = context.WithCancel(context.Background())
				defer cancel()
				filesystem = &cancellingLookupFS{FileSystem: &OSFileSystem{}, cancelAfter: testCase.CancelAfterLookup, cancel: cancel}
				want = context.Canceled
			default:
				t.Fatalf("unknown cancellation arm %q", testCase.Arm)
			}
			pipeline, sessions := buildDiffParallelPipeline(t, fixture.Sessions, filesystem)
			result, err := pipeline.diff(ctx, sessions, NewProgressState())
			if !errors.Is(err, want) {
				t.Fatalf("cancelled diff returned %v, want %v", err, want)
			}
			// A context that was already done before the slice classifies
			// nothing. A cancellation that arrives from inside a lookup races
			// the workers already in flight, so only the error is asserted:
			// whatever they finished is returned with that error and the caller
			// discards it, exactly as the sequential walk did.
			if testCase.Arm != diffParallelDuringDiff && len(result.Sessions) != 0 {
				t.Fatalf("diff on a context that was already done classified %d sessions, want none", len(result.Sessions))
			}
		})
	}
}
