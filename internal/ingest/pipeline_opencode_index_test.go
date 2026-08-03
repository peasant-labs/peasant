package ingest_test

import (
	"context"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestPipeline_OpenCodeSessionsAreIndexedOnFreshIngest is a regression guard for a
// silent, total indexing failure.
//
// OpenCode is the one harness whose transcript is a DIRECTORY, not a file: its
// entries live in message/*.json and part/*.json under a storage root, so its
// indexer resolves that root instead of reading a single transcript. The root is
// carried on the discovered session as OriginalRoot.
//
// The pipeline dropped it. Between discovery and indexing, the session is rebuilt
// from the worker result, and that rebuild copied SessionID, harness, parent,
// format, and source path - but not OriginalRoot. With it empty, the indexer's
// root resolution falls back to deriving a root from the source path, which the
// pipeline has just overwritten with the path of the written copy. It therefore
// walks up from the output tree looking for a message directory, finds nothing,
// and returns no entries AND NO ERROR - a deliberate "an empty session is not a
// failure" path.
//
// WHAT THAT COST A USER, corrected. An earlier version of this comment said every
// freshly ingested OpenCode session was stored with zero entries and that nothing
// said so. A real run shows otherwise: the drain loop indexes nothing and records
// a skipped row, and the stale-index sweep then re-indexes the session in the same
// run, so it ends with its entries. The cost is a wasted indexing pass and an
// index_log row reporting a skip nobody can act on, on every ingest of every
// directory-based session - not an empty session. The fix is still worth having;
// the justification was wrong, which is the class of defect this slice spent its
// whole life closing, in a comment written to explain closing it.
//
// WHAT THIS TEST THEREFORE PROVES, and what it does not. It drives the REAL
// pipeline with the REAL OpenCode indexer over a real OpenCode file tree and
// asserts on entries actually written to the store, so it holds the drain-loop
// path to indexing what it was handed. It does NOT model the recovery: the stub
// metrics store reports no stale sessions, so the sweep finds nothing to re-index
// and cannot mask a regression here. That is what makes the assertion sharp, and
// it is also why this test is not evidence about the user-visible outcome above.
func TestPipeline_OpenCodeSessionsAreIndexedOnFreshIngest(t *testing.T) {
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	const projectHash = "openprojecthash"
	session := setupOpenCodeFixture(t, mfs, testutil.TestSessionUUID, projectHash)
	addOpenCodeMessage(t, mfs, testutil.TestSessionUUID, "msg_one", string(ingest.RoleUser), 10, 0)
	addOpenCodePart(t, mfs, "msg_one", "prt_one")
	addOpenCodeMessage(t, mfs, testutil.TestSessionUUID, "msg_two", string(ingest.RoleAssistant), 0, 20)
	addOpenCodePart(t, mfs, "msg_two", "prt_two")

	sid := session.SessionID
	meta := makeMinimalMeta(t, string(sid))
	meta.Source.FilePath = session.SourcePath.String()
	meta.Source.Format = schema.SourceFormat(ingest.SourceFormatJSON)
	meta.ModelHarness = schema.Harness(defaults.HarnessOpenCode)
	session.ModTime = time.Now().Add(-1 * time.Hour)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		defaults.HarnessOpenCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
		),
	}

	metricsStore := testutil.NewStubMetricsStore()
	cfg := makePipelineConfig(testOutputDir)
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		defaults.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{session.OriginalRoot}},
	}
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
		// The REAL indexer, not a stub: a stub would have returned entries from a
		// map regardless of which root the pipeline handed it, which is exactly the
		// blindness that let this ship.
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			defaults.HarnessOpenCode: ingest.NewOpenCodeIndexer(mfs),
		}),
		ingest.WithMetricsStore(metricsStore),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New == 0 {
		t.Fatalf("nothing was imported, so this test cannot say anything about indexing; summary=%+v", result.Summary)
	}

	entries, listErr := metricsStore.ListEntries(context.Background(), sid)
	if listErr != nil {
		t.Fatalf("read the indexed entries back: %v", listErr)
	}
	if len(entries) == 0 {
		t.Fatalf("the pipeline stored ZERO indexed entries for an OpenCode session whose message and part files both "+
			"exist, on the drain-loop path this test drives. In production the stale-index sweep would re-index it later "+
			"in the same run, so the user-visible cost is a wasted pass and a misleading skipped row rather than an empty "+
			"session - but the sweep is a second chance, not the contract, and this is the pass that is meant to work. "+
			"The likely cause is that "+
			"the storage root the indexer needs was not carried through to the indexing step, leaving it to derive one "+
			"from a source path the pipeline had already replaced with the written copy's path.\nsummary=%+v",
			result.Summary)
	}
	// Both messages must be present, not just one: a root that resolved far enough
	// to find a single file would otherwise read as success.
	if len(entries) < 2 {
		t.Errorf("stored %d indexed entries for a session with two messages; a partially resolved root is still a "+
			"silently truncated transcript", len(entries))
	}
}

// recordingIndexer notes which of the two index entry points the pipeline chose.
//
// Both return the same entries, so the pipeline behaves identically either way -
// which is precisely why this needs its own guard: the dispatch is a CONTRACT, and
// a contract with no observable consequence is one that drifts back silently. It
// did drift, and the consequence only became visible once a second thing broke.
type recordingIndexer struct {
	kind        ingest.TranscriptSourceKind
	entries     []schema.SessionEntry
	bytesCalls  int
	bytesNonNil int
	fileCalls   int
	seenRoots   []ingest.ResolvedPath
	seenPaths   []ingest.ResolvedPath
}

var _ ingest.TranscriptIndexer = (*recordingIndexer)(nil)

func (idx *recordingIndexer) SourceKind() ingest.TranscriptSourceKind { return idx.kind }

func (idx *recordingIndexer) IndexTranscript(_ context.Context, session ingest.DiscoveredSession) ([]schema.SessionEntry, error) {
	idx.fileCalls++
	idx.seenRoots = append(idx.seenRoots, session.OriginalRoot)
	idx.seenPaths = append(idx.seenPaths, session.SourcePath)
	return idx.entries, nil
}

func (idx *recordingIndexer) IndexTranscriptBytes(_ context.Context, session ingest.DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	idx.bytesCalls++
	if len(data) > 0 {
		idx.bytesNonNil++
	}
	idx.seenRoots = append(idx.seenRoots, session.OriginalRoot)
	idx.seenPaths = append(idx.seenPaths, session.SourcePath)
	return idx.entries, nil
}
