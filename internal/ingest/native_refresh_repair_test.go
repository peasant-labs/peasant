package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/native_refresh_repair.yaml
var nativeRefreshRepairYAML []byte

//go:embed testdata/native_refresh_repair.manifest.yaml
var nativeRefreshRepairManifestYAML []byte

// nativeRefreshRepairCase is one named native-repair maintenance oracle. The
// scenario selects how the real Pipeline is arranged; every case drives real
// temporary managed files and a real SQLite store with managed-generation
// support, and never a prepared activation result.
type nativeRefreshRepairCase struct {
	Name                 string         `yaml:"name"`
	Harness              ingest.Harness `yaml:"harness"`
	Scenario             string         `yaml:"scenario"`
	SessionID            string         `yaml:"sessionId"`
	StoredIndexerVersion int            `yaml:"storedIndexerVersion"`
	StoredIndexFormat    int            `yaml:"storedIndexFormat"`
	Source               string         `yaml:"source"`
	UnrelatedSessionID   string         `yaml:"unrelatedSessionId"`
	UnrelatedTranscript  string         `yaml:"unrelatedTranscript"`
	NativeSource         string         `yaml:"nativeSource"`
}

type nativeRefreshRepairFixture struct {
	RequiredNames []string                  `yaml:"requiredNames"`
	Cases         []nativeRefreshRepairCase `yaml:"cases"`
}

func loadNativeRefreshRepairFixture(t *testing.T) nativeRefreshRepairFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(nativeRefreshRepairYAML))
	decoder.KnownFields(true)
	var fixture nativeRefreshRepairFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode native_refresh_repair.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("native_refresh_repair.yaml must contain exactly one document: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	seen := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.Name) == "" || seen[c.Name] {
			t.Fatalf("native_refresh_repair case name %q is empty or repeated", c.Name)
		}
		seen[c.Name] = true
		names = append(names, c.Name)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(nativeRefreshRepairManifestYAML, "native refresh repair")
	if err != nil {
		t.Fatalf("decode native_refresh_repair manifest: %v", err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "native refresh repair"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// nativeRepairStore opens a real store that can persist and read managed
// generations, which is the capability the native targets are activated on.
func nativeRepairStore(t *testing.T, dbPath, root string) *store.Store {
	t.Helper()
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// nativeRepairMeta builds the managed metadata a completed harvest leaves for a
// native session. Its recorded source locator points at the native path the
// adapter recorded.
func nativeRepairMeta(t *testing.T, sessionID string, harness ingest.Harness, nativePath string) *ingest.UnifiedMetadata {
	t.Helper()
	meta := makeMinimalMeta(t, sessionID)
	meta.ModelHarness = harness
	meta.Source = ingest.SourceInfo{FilePath: nativePath, Format: ingest.SourceFormatJSONL}
	return meta
}

// runNativeRepairPipeline runs the real Pipeline with the managed-generation
// targets injected explicitly, exactly as production composes them once a
// capable store is configured.
func runNativeRepairPipeline(t *testing.T, db *store.Store, fs ingest.FileSystem, outputDir string) *ingest.PipelineResult {
	t.Helper()
	cfg := makePipelineConfig(outputDir)
	versions := ingest.NativeGenerationTargets(ingest.HarvesterVersionRegistry)
	pipeline, err := ingest.NewPipeline(
		fs,
		testutil.DefaultGitResolver(),
		ingest.DefaultAdapterRegistry,
		cfg,
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithIndexLogger(db),
		ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithHarvesterVersions(versions),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("run native repair pipeline: %v", err)
	}
	return result
}

func indexedOutcomeFor(result *ingest.PipelineResult, sid ingest.SessionID) ingest.IndexOutcome {
	for _, entry := range result.IndexLog {
		if entry.SessionID == sid {
			return entry.Outcome
		}
	}
	return ""
}

func readIndexVersion(t *testing.T, db *store.Store, sid ingest.SessionID) int {
	t.Helper()
	state, err := db.ReadIndexState(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.IndexVersion == nil {
		return 0
	}
	return *state.IndexVersion
}

func readEntryCount(t *testing.T, db *store.Store, sid ingest.SessionID) int {
	t.Helper()
	entries, err := db.ListEntries(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// removeManagedPair deletes a session's saved transcript and metadata, leaving
// the database rows behind. The native source is then unavailable, which is the
// failed-capture condition the repair path must refuse without a success stamp.
func removeManagedPair(t *testing.T, fs ingest.FileSystem, outputDir string, sid ingest.SessionID) {
	t.Helper()
	metadataPath := ingest.SessionMetadataPath(outputDir, testutil.TestHostSlug, string(sid), "")
	transcriptPath := filepath.Join(filepath.Dir(metadataPath), string(sid)+"--transcript.jsonl")
	for _, path := range []string{metadataPath, transcriptPath} {
		if err := fs.Remove(path); err != nil {
			t.Fatalf("remove managed pair member %q: %v", path, err)
		}
	}
}

// runOpenCodeNativeActivation drives the real OpenCode provenance candidate
// through the production managed-generation activation, reopens the store and
// proves that the persisted prior evidence reuses the captured identities on a
// refresh. The native SQLite source is byte- and row-immutable across the reads.
func runOpenCodeNativeActivation(t *testing.T, tc nativeRefreshRepairCase) {
	t.Helper()
	source := testfixture.MaterializeByName(t, tc.NativeSource)
	before := testfixture.SnapshotSource(t, source)
	sid, err := ingest.NewSessionID(tc.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "native.db")
	root := filepath.Join(dir, "artifacts")
	db := nativeRepairStore(t, dbPath, root)
	defer func() { _ = db.Close() }()
	seedOpenCodeRepairSession(t, db, tc.SessionID, sid)

	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	metadata := schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: sid, ModelHarness: ingest.HarnessOpenCode}
	first := buildOpenCodeRepairCandidate(t, adapter, source.Path, tc.SessionID, sid, metadata, "gen-open-repair-1", ingest.NewProjectionPriorState())
	if err := db.ActivateNativeGeneration(t.Context(), ingest.NativeGenerationActivation{
		Generation:     first.Result,
		Blobs:          first.Blobs,
		PriorEvidence:  first.PriorEvidence,
		IndexerVersion: ingest.NativeGenerationRepairTargets[ingest.HarnessOpenCode].IndexerVersion,
		IndexedAtMs:    1,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceProviderSource,
			TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 1,
		},
	}); err != nil {
		t.Fatalf("activate OpenCode native generation: %v", err)
	}
	prior, err := db.ReadNativeGenerationPrior(t.Context(), sid)
	if err != nil {
		t.Fatalf("read prior after activation: %v", err)
	}
	if prior == nil || len(prior.Aliases.Entries) == 0 || len(prior.PriorEvidence) == 0 {
		t.Fatalf("prior after activation = %+v, want persisted aliases and evidence", prior)
	}
	testfixture.AssertUnchanged(t, source, before)

	firstRefs := openCodeRepairMainRefs(first.Result)
	if len(firstRefs) == 0 {
		t.Fatal("first OpenCode candidate carries no main refs")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := nativeRepairStore(t, dbPath, root)
	defer func() { _ = reopened.Close() }()
	reloaded, err := reopened.ReadNativeGenerationPrior(t.Context(), sid)
	if err != nil {
		t.Fatalf("read prior after reopen: %v", err)
	}
	priorState := ingest.OpenCodeProvenancePrior{Aliases: reloaded.Aliases, HasCompleteGeneration: reloaded.HasCompleteGeneration}
	if len(reloaded.PriorEvidence) > 0 {
		decoded, decodeErr := ingest.DecodeOpenCodeProvenancePrior(reloaded.PriorEvidence)
		if decodeErr != nil {
			t.Fatalf("decode persisted prior: %v", decodeErr)
		}
		priorState = decoded
		priorState.Aliases = reloaded.Aliases
		priorState.HasCompleteGeneration = reloaded.HasCompleteGeneration
	}
	second := buildOpenCodeRepairCandidate(t, adapter, source.Path, tc.SessionID, sid, metadata, "gen-open-repair-2", priorState.Aliases)
	secondRefs := openCodeRepairMainRefs(second.Result)
	if strings.Join(toStringRefs(secondRefs), ",") != strings.Join(toStringRefs(firstRefs), ",") {
		t.Fatalf("refresh after reopen rekeyed refs: %v -> %v", firstRefs, secondRefs)
	}
	testfixture.AssertUnchanged(t, source, before)
}

func buildOpenCodeRepairCandidate(t *testing.T, adapter *ingest.OpenCodeAdapter, nativePath, nativeSessionID string, sid ingest.SessionID, metadata schema.UnifiedMetadata, generationID string, prior ingest.ProjectionPriorState) ingest.NativeGenerationCandidate {
	t.Helper()
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(ingest.OpenCodeProvenanceIndexerConfig{
		Enabled: true,
		Snapshot: func(ctx context.Context, _ ingest.DiscoveredSession) (ingest.OpenCodeHistorySnapshot, error) {
			return adapter.SnapshotOpenCodeProvenance(ctx, nativePath, nativeSessionID, ingest.OpenCodeSnapshotOptions{})
		},
		Metadata:     func(ingest.DiscoveredSession) (schema.UnifiedMetadata, error) { return metadata, nil },
		GenerationID: func(ingest.DiscoveredSession) string { return generationID },
		Prior: func(context.Context, ingest.DiscoveredSession) (ingest.OpenCodeProvenancePrior, error) {
			return ingest.OpenCodeProvenancePrior{Aliases: prior, HasCompleteGeneration: true}, nil
		},
	}))
	candidate, err := indexer.BuildNativeGeneration(t.Context(), ingest.DiscoveredSession{
		SessionID: sid, Harness: ingest.HarnessOpenCode, TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
	})
	if err != nil {
		t.Fatalf("build OpenCode candidate %s: %v", generationID, err)
	}
	return candidate
}

func openCodeRepairMainRefs(result indexformat.V2) []schema.SourceEntryRef {
	refs := make([]schema.SourceEntryRef, 0, len(result.Generation.Main.Entries))
	for i := range result.Generation.Main.Entries {
		refs = append(refs, result.Generation.Main.Entries[i].SourceEntryRef)
	}
	return refs
}

func toStringRefs(refs []schema.SourceEntryRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, string(ref))
	}
	return out
}

func seedOpenCodeRepairSession(t *testing.T, db *store.Store, rawSessionID string, sid ingest.SessionID) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, "INSERT OR IGNORE INTO host_slugs(opaque_id, host_slug) VALUES('host-repair','host-repair')", nil); err != nil {
		t.Fatalf("seed OpenCode repair host: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, "INSERT OR IGNORE INTO projects(project_hash, canonical_cwd, canonical_remote) VALUES('proj-repair','/tmp/repair','github.com/repair/repair')", nil); err != nil {
		t.Fatalf("seed OpenCode repair project: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version)
VALUES(?1,'opencode','model-repair','host-repair','proj-repair',1,2,3,'/tmp/repair/source.json','json',?2)`, &sqlitex.ExecOptions{Args: []any{rawSessionID, ingest.CurrentSchemaVersion}}); err != nil {
		t.Fatalf("seed OpenCode repair session: %v", err)
	}
}

// rawIndexFormat reads the stored representation directly, without the format
// compatibility gate, so a future representation can be proven unchanged.
func rawIndexFormat(t *testing.T, db *store.Store, sid ingest.SessionID) int {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	format := 0
	if err := sqlitex.ExecuteTransient(conn, "SELECT index_format_version FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				format = stmt.ColumnInt(0)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return format
}

// rawEntryCount counts the stored canonical entries without decoding them.
func rawEntryCount(t *testing.T, db *store.Store, sid ingest.SessionID) int {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	count := 0
	if err := sqlitex.ExecuteTransient(conn, "SELECT COUNT(*) FROM session_entries WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func setSessionAdapterVersion(t *testing.T, db *store.Store, sid ingest.SessionID, version int) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET adapter_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{version, string(sid)}}); err != nil {
		t.Fatal(err)
	}
}

func setStoredIndexFormat(t *testing.T, db *store.Store, sid ingest.SessionID, indexerVersion, format int) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_version = ?, index_format_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{indexerVersion, format, string(sid)}}); err != nil {
		t.Fatal(err)
	}
}

func setBadDerivedTitle(t *testing.T, db *store.Store, sid ingest.SessionID, title string, turnCount int) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, "UPDATE session_metrics SET title = ?, turn_count = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{title, turnCount, string(sid)}}); err != nil {
		t.Fatal(err)
	}
}

func readMetricsTitle(t *testing.T, db *store.Store, sid ingest.SessionID) string {
	t.Helper()
	metrics, err := db.GetMetrics(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if metrics == nil || metrics.TitleGenerated == nil {
		return ""
	}
	return *metrics.TitleGenerated
}

// TestNativeRefreshRepair drives the real Pipeline through the named native
// repair oracle: a stale format-1 Codex projection is repaired into a managed
// generation with its metadata, a second run is unchanged, a failed native
// capture retains the last good projection and stamps no success, a future
// representation is refused, and an unrelated harness is untouched.
func TestNativeRefreshRepair(t *testing.T) {
	fixture := loadNativeRefreshRepairFixture(t)
	for _, tc := range fixture.Cases {
		if tc.Scenario != "maintenance_repair" && tc.Scenario != "failed_capture" && tc.Scenario != "future_state" && tc.Scenario != "unrelated_harness" && tc.Scenario != "opencode_native_activation" {
			t.Fatalf("native_refresh_repair case %q declares unknown scenario %q", tc.Name, tc.Scenario)
		}
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			if tc.Scenario == "opencode_native_activation" {
				runOpenCodeNativeActivation(t, tc)
				return
			}
			sid, err := ingest.NewSessionID(tc.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			root := filepath.Join(dir, "artifacts")
			dbPath := filepath.Join(dir, "native.db")
			db := nativeRepairStore(t, dbPath, root)
			defer func() { _ = db.Close() }()
			fs := testutil.NewMemFS()
			const outputDir = "/output"

			meta := nativeRepairMeta(t, tc.SessionID, tc.Harness, "/native/"+tc.SessionID+".jsonl")
			storetest.SeedManagedInput(t, db, fs, outputDir, *meta, []byte(tc.Source))
			if tc.StoredIndexerVersion > 0 {
				setStoredIndexFormat(t, db, sid, tc.StoredIndexerVersion, 1)
			}
			// Keep the session out of the adapter-refresh inventory so the stale
			// index inventory is the one that recomposes it from its stored pair.
			setSessionAdapterVersion(t, db, sid, ingest.NativeGenerationRepairTargets[tc.Harness].AdapterVersion)
			if tc.Scenario == "maintenance_repair" {
				setBadDerivedTitle(t, db, sid, "stale derived title", 99)
			}
			if tc.Scenario == "failed_capture" {
				removeManagedPair(t, fs, outputDir, sid)
			}

			entriesBefore := readEntryCount(t, db, sid)
			if tc.Scenario == "future_state" && tc.StoredIndexFormat > 0 {
				setStoredIndexFormat(t, db, sid, tc.StoredIndexerVersion, tc.StoredIndexFormat)
			}

			var unrelated ingest.SessionID
			var unrelatedBefore int
			if tc.Scenario == "unrelated_harness" {
				unrelated, err = ingest.NewSessionID(tc.UnrelatedSessionID)
				if err != nil {
					t.Fatal(err)
				}
				unrelatedMeta := makeMinimalMeta(t, tc.UnrelatedSessionID)
				storetest.SeedManagedInput(t, db, fs, outputDir, *unrelatedMeta, []byte(tc.UnrelatedTranscript))
				unrelatedBefore = readEntryCount(t, db, unrelated)
			}

			result := runNativeRepairPipeline(t, db, fs, outputDir)

			switch tc.Scenario {
			case "maintenance_repair":
				if got := indexedOutcomeFor(result, sid); got != ingest.IndexOutcomeIndexed && got != ingest.IndexOutcomeReindexed {
					t.Fatalf("repair outcome = %q, want indexed; log=%+v", got, result.IndexLog)
				}
				if got := readIndexVersion(t, db, sid); got != 2 {
					t.Fatalf("stored index format = %d, want 2", got)
				}
				var generationID string
				var mainEntries int
				err := db.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
					generationID = snapshot.GenerationID
					mainEntries = len(snapshot.Main.Entries)
					if snapshot.Completeness != indexformat.GenerationCompletenessComplete {
						t.Errorf("snapshot completeness = %q, want complete", snapshot.Completeness)
					}
					if snapshot.IndexVersion != 2 {
						t.Errorf("snapshot index version = %d, want 2", snapshot.IndexVersion)
					}
					return nil
				})
				if err != nil {
					t.Fatalf("read repaired snapshot: %v", err)
				}
				if generationID == "" || mainEntries == 0 {
					t.Fatalf("repaired generation id %q main entries %d, want an installed generation with entries", generationID, mainEntries)
				}
				if got := readMetricsTitle(t, db, sid); got != "repair the stale projection" {
					t.Fatalf("repaired title = %q, want %q", got, "repair the stale projection")
				}
				if got := readEntryCount(t, db, sid); got == 0 {
					t.Fatal("repaired session has no canonical entries")
				}

				// Reopen the store and run again: the repaired session is at
				// target, so the second run leaves identity and content intact.
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				reopened := nativeRepairStore(t, dbPath, root)
				defer func() { _ = reopened.Close() }()
				second := runNativeRepairPipeline(t, reopened, fs, outputDir)
				if second.Summary.Indexed != 0 {
					t.Fatalf("second run indexed %d sessions, want 0", second.Summary.Indexed)
				}
				if got := readIndexVersion(t, reopened, sid); got != 2 {
					t.Fatalf("second run changed stored format to %d, want 2", got)
				}
				var secondGenerationID string
				err = reopened.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
					secondGenerationID = snapshot.GenerationID
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if secondGenerationID != generationID {
					t.Fatalf("second run replaced generation %q with %q", generationID, secondGenerationID)
				}
				// The repair's activation must preserve the recorded
				// publication-capture agreement: pointing the session at its
				// generation updates watched session facts, and a cleared
				// provenance kind would re-ingest this session on every
				// discovery run and leave it permanently unpublishable. The
				// kind this session had before the repair is exactly the kind
				// it must still carry afterwards; whether the activation
				// should also BIND a session that had no captured metadata
				// yet is a separate design decision, tracked on the slice.
				bound, err := reopened.ReadIndexState(t.Context(), sid)
				if err != nil {
					t.Fatal(err)
				}
				if bound == nil {
					t.Fatal("repaired session has no readable index state")
				}
				metadataPath := ingest.SessionMetadataPath(outputDir, testutil.TestHostSlug, string(sid), "")
				pair, pairErr := ingest.ReadManagedPair(fs, outputDir, metadataPath, ingest.SessionID(sid))
				if pairErr != nil {
					t.Fatalf("read repaired managed pair: %v", pairErr)
				}
				if pair.ArtifactHash == "" || bound.ArtifactHash == nil || pair.ArtifactHash != *bound.ArtifactHash {
					t.Fatalf("repaired pair identity %q disagrees with the stored artifact hash %v", pair.ArtifactHash, bound.ArtifactHash)
				}

			case "failed_capture":
				if got := indexedOutcomeFor(result, sid); got == ingest.IndexOutcomeIndexed || got == ingest.IndexOutcomeReindexed {
					t.Fatalf("failed native capture reported %q, want no success", got)
				}
				if got := readIndexVersion(t, db, sid); got != 1 {
					t.Fatalf("failed capture changed stored format to %d, want 1", got)
				}
				var generationID string
				err := db.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
					generationID = snapshot.GenerationID
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if generationID != "" {
					t.Fatalf("failed capture installed generation %q, want none", generationID)
				}
				if got := readEntryCount(t, db, sid); got != entriesBefore {
					t.Fatalf("failed capture changed entries: %d -> %d", entriesBefore, got)
				}

			case "future_state":
				if got := rawIndexFormat(t, db, sid); got != tc.StoredIndexFormat {
					t.Fatalf("future representation changed to %d, want %d", got, tc.StoredIndexFormat)
				}
				if got := rawEntryCount(t, db, sid); got != entriesBefore {
					t.Fatalf("future representation entries changed: %d -> %d", entriesBefore, got)
				}

			case "unrelated_harness":
				if got := readEntryCount(t, db, unrelated); got != unrelatedBefore {
					t.Fatalf("unrelated harness entries changed: %d -> %d", unrelatedBefore, got)
				}
				if got := readIndexVersion(t, db, sid); got != 2 {
					t.Fatalf("codex session was not repaired: format %d, want 2", got)
				}
			}
		})
	}
}
