package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_lane.yaml
var refreshLaneYAML []byte

//go:embed testdata/refresh_lane.manifest.yaml
var refreshLaneManifestYAML []byte

//go:embed testdata/refresh_lane_strike.jsonl
var refreshLaneStrikeTranscript []byte

type refreshLaneCase struct {
	Name           string   `yaml:"name"`
	Harness        string   `yaml:"harness"`
	Source         string   `yaml:"source"`
	StoredAdapter  string   `yaml:"stored_adapter"`
	FileAdapter    string   `yaml:"file_adapter"`
	MetadataSchema string   `yaml:"metadata_schema"`
	Pair           string   `yaml:"pair"`
	Force          bool     `yaml:"force"`
	Indexer        string   `yaml:"indexer"`
	Refusal        string   `yaml:"refusal"`
	RefusalIndexer string   `yaml:"refusal_indexer"`
	Publication    string   `yaml:"publication"`
	WantReason     string   `yaml:"want_reason"`
	WantLane       string   `yaml:"want_lane"`
	WantNativeRead bool     `yaml:"want_native_read"`
	WantPairWrites []string `yaml:"want_pair_writes"`
	WantIndexed    bool     `yaml:"want_indexed"`
	WantBound      bool     `yaml:"want_bound"`
}

type refreshLaneFixture struct {
	SessionID    string            `yaml:"session_id"`
	Transcript   []string          `yaml:"transcript"`
	AppendRecord string            `yaml:"append_record"`
	Cases        []refreshLaneCase `yaml:"cases"`
}

func loadRefreshLaneFixture(t *testing.T) refreshLaneFixture {
	t.Helper()
	var fixture refreshLaneFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(refreshLaneYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode refresh_lane.yaml: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || present[testCase.Name] {
			t.Fatalf("refresh_lane.yaml has an empty or repeated case name %q", testCase.Name)
		}
		present[testCase.Name] = true
		if _, err := ingest.NewRefreshReason(testCase.WantReason); err != nil {
			t.Fatalf("case %s: %v", testCase.Name, err)
		}
		if _, err := ingest.NewRefreshLane(testCase.WantLane); err != nil {
			t.Fatalf("case %s: %v", testCase.Name, err)
		}
		if testCase.Harness != string(ingest.HarnessClaudeCode) && testCase.Harness != string(ingest.HarnessStrike) {
			t.Fatalf("case %s names harness %q; use claude or strike", testCase.Name, testCase.Harness)
		}
		if len(testCase.WantPairWrites) > 0 {
			for _, name := range testCase.WantPairWrites {
				if name != "metadata" && name != "transcript" {
					t.Fatalf("case %s names pair write %q; use metadata or transcript", testCase.Name, name)
				}
			}
		}
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(refreshLaneManifestYAML, &manifest); err != nil {
		t.Fatalf("decode refresh_lane manifest: %v", err)
	}
	if err := testutil.RequireFixtureNames("refresh_lane.yaml", "cases", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// refreshLaneBench is one synthetic source tree, one real store, and the
// production pipeline over them. Every run uses the same session and source
// paths; each case mutates stored state between runs.
type refreshLaneBench struct {
	t              *testing.T
	fixture        refreshLaneFixture
	fs             *testutil.CountingFS
	database       *store.Store
	harness        ingest.Harness
	sessionID      ingest.SessionID
	sourceRoot     string
	sourcePath     string
	outputDir      string
	versions       map[ingest.Harness]ingest.HarvesterVersions
	metadataPath   string
	transcriptPath string
}

func newRefreshLaneBench(t *testing.T, fixture refreshLaneFixture, testCase refreshLaneCase) *refreshLaneBench {
	t.Helper()
	mem := testutil.NewMemFS()
	fs := testutil.NewCountingFS(mem)
	database := storetest.OpenWith(t)
	t.Cleanup(func() { _ = database.Close() })

	root := "/refresh-lane"
	sessionID, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Join(root, "source")
	sourcePath := filepath.Join(sourceRoot, "-workspace", fixture.SessionID+".jsonl")
	transcript := strings.Join(fixture.Transcript, "\n") + "\n"
	if ingest.Harness(testCase.Harness) == ingest.HarnessStrike {
		transcript = string(refreshLaneStrikeTranscript)
	}
	if err := fs.WriteFile(sourcePath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(root, "output")
	bench := &refreshLaneBench{
		t: t, fixture: fixture, fs: fs, database: database,
		harness: ingest.Harness(testCase.Harness), sessionID: sessionID,
		sourceRoot: sourceRoot, sourcePath: sourcePath, outputDir: outputDir,
		versions: cloneHarvesterVersions(),
	}
	result := bench.run(true, true)
	if result.Summary.New != 1 || result.Summary.StoreError != nil {
		t.Fatalf("%s: baseline harvest did not ingest the synthetic session: %+v; diagnostics %+v", testCase.Name, result.Summary, result.Diagnostics)
	}
	location := bench.location()
	bench.metadataPath = ingest.SessionMetadataPath(outputDir, location.HostSlug, string(sessionID), location.ParentID)
	bench.transcriptPath = filepath.Join(ingest.SessionDir(outputDir, location.HostSlug, string(sessionID), location.ParentID), string(sessionID)+"--transcript."+string(ingest.SourceFormatJSONL))
	return bench
}

func (b *refreshLaneBench) location() ingest.SessionLocation {
	b.t.Helper()
	locations, err := b.database.BulkLookupSessionLocations(b.t.Context(), []ingest.SessionID{b.sessionID})
	if err != nil {
		b.t.Fatal(err)
	}
	location, ok := locations[b.sessionID]
	if !ok {
		b.t.Fatalf("no stored location for %s", b.sessionID)
	}
	return location
}

func (b *refreshLaneBench) sourceTranscript() string {
	if b.harness == ingest.HarnessStrike {
		return string(refreshLaneStrikeTranscript)
	}
	return strings.Join(b.fixture.Transcript, "\n") + "\n"
}

// pipeline builds one production pipeline over the bench filesystem and store.
func (b *refreshLaneBench) pipeline(force bool, discover bool) *ingest.Pipeline {
	b.t.Helper()
	cfg := makePipelineConfig(b.outputDir)
	cfg.StalenessThreshold = 0
	cfg.Force = force
	if !discover {
		cfg.Sources = map[ingest.Harness]ingest.SourceConfig{}
	} else {
		cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
			b.harness: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(b.sourceRoot)}},
		}
	}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		b.harness: b.adapterFactory(),
	}
	indexers := map[ingest.Harness]ingest.TranscriptIndexer{
		b.harness: b.indexer(),
	}
	pipeline, err := newTestPipeline(b.fs, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithStore(b.database), ingest.WithMetricsStore(b.database),
		ingest.WithIndexLogger(b.database), ingest.WithLogger(b.database),
		ingest.WithIndexers(indexers), ingest.WithHarvesterVersions(b.versions))
	if err != nil {
		b.t.Fatal(err)
	}
	return pipeline
}

func (b *refreshLaneBench) adapterFactory() ingest.AdapterFactory {
	if b.harness == ingest.HarnessClaudeCode {
		return ingest.DefaultAdapterRegistry[ingest.HarnessClaudeCode]
	}
	return func(fs ingest.FileSystem, _ ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
		return &refreshLaneStubAdapter{
			harness: b.harness, fs: fs, sourcePath: b.sourcePath, sessionID: b.sessionID,
			build: func() *ingest.UnifiedMetadata {
				meta := makeMinimalMeta(b.t, string(b.sessionID))
				meta.ModelHarness = b.harness
				meta.Source.FilePath = b.sourcePath
				meta.Source.Format = ingest.SourceFormatJSONL
				return meta
			},
		}
	}
}

func (b *refreshLaneBench) indexer() ingest.TranscriptIndexer {
	return ingest.NewIndexerRegistry(b.fs, ingest.IndexerRegistryOptions{})[b.harness]
}

func (b *refreshLaneBench) run(force bool, discover bool) *ingest.PipelineResult {
	b.t.Helper()
	pipeline := b.pipeline(force, discover)
	result, err := pipeline.Run(b.t.Context())
	if err != nil {
		b.t.Fatalf("harvest: %v", err)
	}
	return result
}

func (b *refreshLaneBench) runStoredOnly() *ingest.PipelineResult {
	b.t.Helper()
	return b.run(false, false)
}

func (b *refreshLaneBench) lastRun() int64 {
	b.t.Helper()
	return refreshLaneLastRun(b.t, b.database)
}

func refreshLaneLastRun(t *testing.T, database *store.Store) int64 {
	t.Helper()
	conn, err := database.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Pool().Put(conn)
	var id int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(max(id), 0) FROM ingest_log`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			id = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// applyMutation puts the stored row, pair, source and targets into the state
// one fixture case names. It runs after the baseline harvest, so every case
// starts from a current, bound, indexed session.
func (b *refreshLaneBench) applyMutation(testCase refreshLaneCase) {
	b.t.Helper()
	location := b.location()

	if testCase.StoredAdapter == "behind" {
		target := b.versions[b.harness]
		target.AdapterVersion++
		b.versions[b.harness] = target
	}
	if testCase.Indexer == "behind" {
		target := b.versions[b.harness]
		target.IndexerVersion++
		b.versions[b.harness] = target
	}
	if testCase.MetadataSchema == "native_refresh" {
		execSessionSQL(b.t, b.database, `UPDATE sessions SET schema_version=8 WHERE session_id=?`, string(b.sessionID))
		b.mutateMetadata(func(meta *ingest.UnifiedMetadata) {
			meta.SchemaVersion = 8
		})
	}
	switch testCase.StoredAdapter {
	case "behind":
		if testCase.FileAdapter == "current" {
			// The row is behind while the saved metadata already records this
			// build's adapter target. The decision must read the row's stamp and
			// never open the native source.
			b.setFileAdapterOnly(b.versions[b.harness].AdapterVersion)
		}
	case "absent":
		execSessionSQL(b.t, b.database, `UPDATE sessions SET adapter_version=NULL WHERE session_id=?`, string(b.sessionID))
		b.mutateMetadata(func(meta *ingest.UnifiedMetadata) {
			meta.AdapterVersion = nil
		})
	}
	if testCase.Pair == "missing" {
		if err := b.fs.Remove(b.metadataPath); err != nil {
			b.t.Fatal(err)
		}
		if err := b.fs.Remove(b.transcriptPath); err != nil {
			b.t.Fatal(err)
		}
	}
	if testCase.Refusal != "" {
		b.seedRefusal(testCase)
		if testCase.RefusalIndexer == "behind" {
			// The refusal was recorded by the previous producer; this build
			// has moved one revision past it.
			target := b.versions[b.harness]
			target.IndexerVersion++
			b.versions[b.harness] = target
		}
	}
	if testCase.Publication == "unbound" {
		b.unbindPublication(location)
	}
	if testCase.Source == "changed" {
		changed := b.sourceTranscript() + b.fixture.AppendRecord + "\n"
		if err := b.fs.WriteFile(b.sourcePath, []byte(changed), 0o600); err != nil {
			b.t.Fatal(err)
		}
	}
}

// mutateMetadata rewrites the stored metadata half of the pair through the
// production metadata encoder and reconciles the recorded artifact identity,
// so the pair stays a valid pair for the new bytes.
func (b *refreshLaneBench) mutateMetadata(mutate func(*ingest.UnifiedMetadata)) {
	b.t.Helper()
	data, err := b.fs.ReadFile(b.metadataPath)
	if err != nil {
		b.t.Fatal(err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		b.t.Fatal(err)
	}
	mutate(&meta)
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	encoded, err := json.Marshal(&meta)
	if err != nil {
		b.t.Fatal(err)
	}
	transcript, err := b.fs.ReadFile(b.transcriptPath)
	if err != nil {
		b.t.Fatal(err)
	}
	if err := b.fs.WriteFile(b.metadataPath, encoded, 0o600); err != nil {
		b.t.Fatal(err)
	}
	artifact, err := ingest.NewManagedArtifact(encoded, transcript)
	if err != nil {
		b.t.Fatal(err)
	}
	mirrored := b.database.MirrorArtifacts(b.t.Context(), []ingest.ArtifactMirrorRequest{{Artifact: artifact, TranscriptKept: true}})
	if len(mirrored) != 1 || mirrored[0].Err != nil || !mirrored[0].Mirrored {
		b.t.Fatalf("reconcile mutated pair: %+v", mirrored)
	}
	// A metadata-only pair change records a new publication capture. The
	// stored index still describes the same transcript, so re-state the
	// binding the way the ordinary index write would.
	state, err := b.database.ReadIndexState(b.t.Context(), b.sessionID)
	if err != nil || state == nil {
		b.t.Fatalf("read index state after metadata mutation: state=%+v err=%v", state, err)
	}
	if state.IndexedInputHash != nil && !state.PublicationBound {
		if err := b.database.BindPublicationIndex(b.t.Context(), ingest.PublicationIndexBind{
			SessionID: b.sessionID, ExpectedState: state, IndexedInputHash: *state.IndexedInputHash,
			CaptureRevision: state.PublicationCaptureRevision, IndexedAtMs: time.Now().UnixMilli(),
		}); err != nil {
			b.t.Fatalf("rebind metadata-only pair: %v", err)
		}
	}
}

// setFileAdapterOnly moves the saved metadata's adapter stamp without
// upserting the row, leaving the row's stamp behind while the pair identity
// stays current. This is the mixed state the row-stamp case pins: the row is
// the durable maintenance selection, the file is only evidence.
func (b *refreshLaneBench) setFileAdapterOnly(target int) {
	b.t.Helper()
	data, err := b.fs.ReadFile(b.metadataPath)
	if err != nil {
		b.t.Fatal(err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		b.t.Fatal(err)
	}
	meta.AdapterVersion = &target
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	encoded, err := json.Marshal(&meta)
	if err != nil {
		b.t.Fatal(err)
	}
	transcript, err := b.fs.ReadFile(b.transcriptPath)
	if err != nil {
		b.t.Fatal(err)
	}
	if err := b.fs.WriteFile(b.metadataPath, encoded, 0o600); err != nil {
		b.t.Fatal(err)
	}
	artifact, err := ingest.NewManagedArtifact(encoded, transcript)
	if err != nil {
		b.t.Fatal(err)
	}
	execSessionSQL(b.t, b.database, `UPDATE sessions SET artifact_hash=? WHERE session_id=?`, artifact.ArtifactHash, string(b.sessionID))
}

func (b *refreshLaneBench) seedRefusal(testCase refreshLaneCase) {
	b.t.Helper()
	state, err := b.database.ReadIndexState(b.t.Context(), b.sessionID)
	if err != nil || state == nil {
		b.t.Fatalf("read index state before refusal seed: state=%+v err=%v", state, err)
	}
	if state.IndexedInputHash == nil {
		b.t.Fatal("refusal seed has no stored input proof to preserve")
	}
	indexer := b.versions[b.harness].IndexerVersion
	code, err := ingest.NewContentCaptureFailureCode(testCase.Refusal)
	if err != nil {
		b.t.Fatal(err)
	}
	writes := b.database.IndexSessionEntryBatch(b.t.Context(), []ingest.SessionEntryWrite{{
		CaptureRevision:  state.PublicationCaptureRevision,
		Mode:             ingest.SessionEntryWriteReplaceAll,
		SessionID:        b.sessionID,
		Result:           indexformat.V1{},
		IndexVersion:     1,
		IndexerVersion:   indexer,
		IndexedAtMs:      time.Now().Add(-time.Hour).UnixMilli(),
		ExpectedState:    state,
		IndexedInputHash: state.IndexedInputHash,
		ContentCapture: ingest.SessionContentCaptureWrite{
			PublicationCaptureRevision: state.PublicationCaptureRevision,
			Status:                     ingest.ContentCaptureIncomplete,
			SourceAuthority:            ingest.ContentSourcePeasantSnapshot,
			TranscriptOrigin:           ingest.TranscriptOriginFile,
			CaptureFormat:              ingest.ContentCaptureFormatPreviewOnly,
			CapturedAtMs:               time.Now().Add(-time.Hour).UnixMilli(),
			FailureCode:                code,
			FailureMessage:             "synthetic stored refusal",
		},
	}})
	if len(writes) != 1 || writes[0].Err != nil {
		b.t.Fatalf("seed stored refusal: %+v", writes)
	}
	// The refusal's capture is incomplete, so the preview index write above
	// does not carry the publication binding. Re-state the binding the
	// baseline had, without touching the refusal it records.
	after, err := b.database.ReadIndexState(b.t.Context(), b.sessionID)
	if err != nil || after == nil || after.IndexedInputHash == nil {
		b.t.Fatalf("read index state after refusal seed: state=%+v err=%v", after, err)
	}
	if err := b.database.BindPublicationIndex(b.t.Context(), ingest.PublicationIndexBind{
		SessionID: b.sessionID, ExpectedState: after, IndexedInputHash: *after.IndexedInputHash,
		CaptureRevision: after.PublicationCaptureRevision, IndexedAtMs: time.Now().UnixMilli(),
	}); err != nil {
		b.t.Fatalf("restore publication binding after refusal seed: %v", err)
	}
}

// unbindPublication writes a newer publication capture for the same session,
// exactly as an ordinary metadata capture would, and leaves the stored index
// bound to the previous revision.
func (b *refreshLaneBench) unbindPublication(location ingest.SessionLocation) {
	b.t.Helper()
	data, err := b.fs.ReadFile(b.metadataPath)
	if err != nil {
		b.t.Fatal(err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		b.t.Fatal(err)
	}
	meta.CWD = filepath.Join(meta.CWD, "publication-refresh")
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	revisions, err := b.database.InsertSessionsWithRevisions(b.t.Context(), []ingest.StoreEntry{{
		Metadata: &meta, PublicationCapture: true, CWDProvenance: ingest.CWDSourceExact,
	}})
	if err != nil {
		b.t.Fatal(err)
	}
	if revisions[b.sessionID] <= location.CaptureRevision {
		b.t.Fatalf("publication capture revision = %d, want newer than %d", revisions[b.sessionID], location.CaptureRevision)
	}
}

func (b *refreshLaneBench) decision(result *ingest.PipelineResult) ingest.RefreshDecision {
	b.t.Helper()
	var found *ingest.SessionResult
	for index := range result.Sessions {
		if result.Sessions[index].SessionID == b.sessionID {
			found = &result.Sessions[index]
		}
	}
	if found == nil {
		b.t.Fatalf("run reported no result for %s; summary %+v", b.sessionID, result.Summary)
	}
	return found.Decision
}

func TestRefreshLaneDecisionsOnTheProductionPipeline(t *testing.T) {
	fixture := loadRefreshLaneFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			bench := newRefreshLaneBench(t, fixture, testCase)
			bench.applyMutation(testCase)
			wantReason, err := ingest.NewRefreshReason(testCase.WantReason)
			if err != nil {
				t.Fatal(err)
			}
			wantLane, err := ingest.NewRefreshLane(testCase.WantLane)
			if err != nil {
				t.Fatal(err)
			}
			// A settled row needs no native compare, so the case runs the
			// stored-only maintenance path. Every other case exercises the
			// discovery path the decision must survive.
			discover := wantReason != ingest.RefreshReasonNone
			var storedDecision ingest.RefreshDecision
			if !discover {
				storedDecision = ingest.StoredDecisionForTest(bench.pipeline(false, false), ingest.DiscoveredSession{SessionID: bench.sessionID, Harness: bench.harness}, bench.location())
			}
			bench.fs.ResetCounts()
			before := bench.lastRun()
			result := bench.run(testCase.Force, discover)
			runID := bench.lastRun()
			if runID == before {
				t.Fatalf("the case run stored no audit row")
			}

			decision := storedDecision
			if discover {
				decision = bench.decision(result)
			}
			if decision.Reason != wantReason || decision.Lane != wantLane {
				t.Fatalf("decision = %s/%s, want %s/%s; summary %+v; diagnostics %+v", decision.Reason, decision.Lane, wantReason, wantLane, result.Summary, result.Diagnostics)
			}

			if decision.IsRefresh() {
				rows := refreshLaneRows(t, bench.database, runID)
				reason, ok := rows[bench.sessionID]
				if !ok {
					t.Fatalf("no refresh outcome row for %s; outcomes %+v", bench.sessionID, rows)
				}
				if reason != decision.OutcomeReason() {
					t.Fatalf("refresh row reason = %s, want %s", reason.Code(), decision.OutcomeReason().Code())
				}
				censusReason := false
				for _, row := range result.Census.Rows {
					if row.Reason == decision.OutcomeReason() {
						censusReason = true
						if row.Lane != wantLane {
							t.Fatalf("census lane for %s = %s, want %s", row.Reason.Code(), row.Lane, wantLane)
						}
					}
				}
				if !censusReason {
					t.Fatalf("census has no row for %s: %+v", decision.OutcomeReason().Code(), result.Census.Rows)
				}
			} else {
				rows := refreshLaneRows(t, bench.database, runID)
				if _, ok := rows[bench.sessionID]; ok {
					t.Fatalf("a %s decision wrote a refresh outcome row: %+v", decision.Reason, rows)
				}
			}

			nativeBytes := bench.fs.BytesReadUnder(bench.sourceRoot, "")
			if testCase.WantNativeRead != (nativeBytes > 0) {
				t.Fatalf("native source bytes read = %d, want native read %t; summary %+v", nativeBytes, testCase.WantNativeRead, result.Summary)
			}
			wantWrites := make(map[string]bool, len(testCase.WantPairWrites))
			for _, name := range testCase.WantPairWrites {
				wantWrites[name] = true
			}
			if got := bench.fs.Count(testutil.FSOpRename, bench.metadataPath) > 0; got != wantWrites["metadata"] {
				t.Fatalf("metadata pair write = %t, want %t", got, wantWrites["metadata"])
			}
			if got := bench.fs.Count(testutil.FSOpRename, bench.transcriptPath) > 0; got != wantWrites["transcript"] {
				t.Fatalf("transcript pair write = %t, want %t", got, wantWrites["transcript"])
			}
			if got := result.Summary.Indexed == 1; got != testCase.WantIndexed {
				t.Fatalf("indexed = %t (%+v), want %t", got, result.Summary, testCase.WantIndexed)
			}
			if testCase.WantBound {
				state, err := bench.database.ReadIndexState(t.Context(), bench.sessionID)
				if err != nil || state == nil {
					t.Fatalf("read bound state: state=%+v err=%v", state, err)
				}
				if !state.PublicationBound {
					t.Fatalf("publication stayed unbound: %+v", state)
				}
			}

			// The next stored-only run decides none: no native read, no pair
			// write, no index write, no refresh row.
			bench.fs.ResetCounts()
			secondBefore := bench.lastRun()
			second := bench.runStoredOnly()
			if second.Summary.Updated != 0 || second.Summary.Indexed != 0 || second.Summary.New != 0 {
				t.Fatalf("the next stored-only run still did work: %+v", second.Summary)
			}
			if rows := refreshLaneRows(t, bench.database, bench.lastRun()); len(rows) != 0 {
				t.Fatalf("the next stored-only run wrote refresh rows: %+v", rows)
			}
			if secondBefore == 0 {
				t.Fatal("the second run stored no audit row")
			}
			if reads := bench.fs.BytesReadUnder(bench.sourceRoot, ""); reads != 0 {
				t.Fatalf("the next stored-only run read %d native source bytes", reads)
			}
			if bench.fs.Count(testutil.FSOpRename, bench.metadataPath) != 0 || bench.fs.Count(testutil.FSOpRename, bench.transcriptPath) != 0 {
				t.Fatal("the next stored-only run rewrote the stored pair")
			}
			nextPipeline := bench.pipeline(false, false)
			next := ingest.StoredDecisionForTest(nextPipeline, ingest.DiscoveredSession{SessionID: bench.sessionID, Harness: bench.harness}, bench.location())
			if next.Reason != ingest.RefreshReasonNone || next.Lane != ingest.RefreshLaneNone {
				t.Fatalf("the next stored decision = %s/%s, want none/none", next.Reason, next.Lane)
			}
		})
	}
}

func refreshLaneRows(t *testing.T, database *store.Store, runID int64) map[ingest.SessionID]ingest.OutcomeReason {
	t.Helper()
	outcomes, err := database.ListIngestRunOutcomes(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	rows := make(map[ingest.SessionID]ingest.OutcomeReason)
	for _, outcome := range outcomes {
		if outcome.Reason.Kind() != ingest.OutcomeKindRefresh {
			continue
		}
		rows[outcome.SessionID] = outcome.Reason
	}
	return rows
}

// refreshLaneStubAdapter is the native-only harness shape: it discovers one
// synthetic session and cannot extract metadata from a stored transcript.
type refreshLaneStubAdapter struct {
	harness    ingest.Harness
	fs         ingest.FileSystem
	sourcePath string
	sessionID  ingest.SessionID
	build      func() *ingest.UnifiedMetadata
}

func (a *refreshLaneStubAdapter) Harness() ingest.Harness { return a.harness }

func (a *refreshLaneStubAdapter) Discover(context.Context, ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	info, err := a.fs.Stat(a.sourcePath)
	if err != nil {
		return nil, err
	}
	return []ingest.DiscoveredSession{{
		SessionID: a.sessionID, Harness: a.harness,
		SourcePath: ingest.ResolvedPath(a.sourcePath), SourceFormat: ingest.SourceFormatJSONL,
		TranscriptOrigin: ingest.TranscriptOriginFile, ModTime: info.ModTime(),
	}}, nil
}

func (a *refreshLaneStubAdapter) ExtractMetadata(context.Context, ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	return a.build(), nil
}
