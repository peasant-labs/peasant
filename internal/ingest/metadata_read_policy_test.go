package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/metadata_read_policy.yaml
var metadataReadPolicyYAML []byte

type metadataReadPolicyFixtures struct {
	RequiredNames []string         `yaml:"requiredNames"`
	SessionID     ingest.SessionID `yaml:"sessionID"`
	Transcript    string           `yaml:"transcript"`
	Cases         []struct {
		Name                  string              `yaml:"name"`
		SchemaVersion         int                 `yaml:"schemaVersion"`
		StoredSchemaVersion   *int                `yaml:"storedSchemaVersion"`
		StoredAdapterVersion  *int                `yaml:"storedAdapterVersion"`
		WarmStoredSchema      *int                `yaml:"warmStoredSchema"`
		StoredAbsent          bool                `yaml:"storedAbsent"`
		LookupFailures        int                 `yaml:"lookupFailures"`
		RetryLookup           bool                `yaml:"retryLookup"`
		WantDiagnostic        string              `yaml:"wantDiagnostic"`
		OutsideDiscovery      bool                `yaml:"outsideDiscovery"`
		RetryCompatible       bool                `yaml:"retryCompatible"`
		AdapterVersion        *int                `yaml:"adapterVersion"`
		RawAdapterVersion     string              `yaml:"rawAdapterVersion"`
		FaultOperation        metadataPolicyFault `yaml:"faultOperation"`
		TransientIO           bool                `yaml:"transientIO"`
		MetadataAbsent        bool                `yaml:"metadataAbsent"`
		RawMetadata           string              `yaml:"rawMetadata"`
		Nested                bool                `yaml:"nested"`
		Database              bool                `yaml:"database"`
		SourceChanged         bool                `yaml:"sourceChanged"`
		Reindex               bool                `yaml:"reindex"`
		Force                 bool                `yaml:"force"`
		Stale                 bool                `yaml:"stale"`
		WantExtract           int                 `yaml:"wantExtract"`
		WantIndexed           int                 `yaml:"wantIndexed"`
		ExpectedMetadataReads *int                `yaml:"expectedMetadataReads"`
	} `yaml:"cases"`
}

func loadMetadataReadPolicyFixtures(t *testing.T) metadataReadPolicyFixtures {
	t.Helper()
	var fixtures metadataReadPolicyFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(metadataReadPolicyYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("missing required fixture %q", name)
		}
	}
	return fixtures
}

type metadataPolicyFS struct {
	*testutil.MemFS
	nativePath     string
	nativeRead     atomic.Int64
	nativeStat     atomic.Int64
	faultOperation metadataPolicyFault
	faultPath      string
	faults         atomic.Int64
	transientIO    bool
	metadataPath   string
	metadataReads  atomic.Int64
}

type metadataPolicyFault string

const (
	metadataPolicyReadFault           metadataPolicyFault = "metadata-read"
	metadataPolicyStatFault           metadataPolicyFault = "metadata-stat"
	metadataPolicyOutputListFault     metadataPolicyFault = "output-list"
	metadataPolicyHostListFault       metadataPolicyFault = "host-list"
	metadataPolicyTranscriptStatFault metadataPolicyFault = "transcript-stat"
)

func (f *metadataPolicyFS) fault(path string) error {
	f.faults.Add(1)
	if f.transientIO {
		return &fs.PathError{Op: string(f.faultOperation), Path: path, Err: syscall.EIO}
	}
	return &fs.PathError{Op: string(f.faultOperation), Path: path, Err: fs.ErrPermission}
}

var _ ingest.FileSystem = (*metadataPolicyFS)(nil)

func (f *metadataPolicyFS) ReadFile(path string) ([]byte, error) {
	if path == f.metadataPath {
		f.metadataReads.Add(1)
	}
	if f.faultOperation == metadataPolicyReadFault && path == f.faultPath {
		return nil, f.fault(path)
	}
	if path == f.nativePath {
		f.nativeRead.Add(1)
	}
	return f.MemFS.ReadFile(path)
}

func (f *metadataPolicyFS) Stat(path string) (fs.FileInfo, error) {
	if (f.faultOperation == metadataPolicyStatFault || f.faultOperation == metadataPolicyTranscriptStatFault) && path == f.faultPath {
		return nil, f.fault(path)
	}
	if path == f.nativePath {
		f.nativeStat.Add(1)
	}
	return f.MemFS.Stat(path)
}

func (f *metadataPolicyFS) ReadDir(path string) ([]fs.DirEntry, error) {
	if (f.faultOperation == metadataPolicyOutputListFault || f.faultOperation == metadataPolicyHostListFault) && path == f.faultPath {
		return nil, f.fault(path)
	}
	return f.MemFS.ReadDir(path)
}

type metadataPolicyAdapter struct {
	*testutil.StubAdapter
	extracts atomic.Int64
}

var _ ingest.SourceAdapter = (*metadataPolicyAdapter)(nil)

func (a *metadataPolicyAdapter) ExtractMetadata(ctx context.Context, session ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	a.extracts.Add(1)
	return a.StubAdapter.ExtractMetadata(ctx, session)
}

// metadataPolicyStore faults only the lookup dependency. All writes and later
// state assertions use the real SQLite store.
type metadataPolicyStore struct {
	*store.Store
	remainingFailures atomic.Int64
	faults            atomic.Int64
}

var _ ingest.SessionStore = (*metadataPolicyStore)(nil)

func (s *metadataPolicyStore) BulkLookupSessionLocations(ctx context.Context, ids []ingest.SessionID) (map[ingest.SessionID]ingest.SessionLocation, error) {
	for {
		remaining := s.remainingFailures.Load()
		if remaining == 0 {
			return s.Store.BulkLookupSessionLocations(ctx, ids)
		}
		if remaining < 0 || s.remainingFailures.CompareAndSwap(remaining, remaining-1) {
			s.faults.Add(1)
			return nil, errors.New("synthetic stored-location lookup I/O failure")
		}
	}
}

type metadataPolicyIndexState struct {
	IndexerVersion int
	IndexedAt      int64
	EntriesHash    string
	OriginVersion  int
}

func readMetadataPolicyIndexState(t *testing.T, database *store.Store, sid ingest.SessionID) metadataPolicyIndexState {
	t.Helper()
	conn, err := database.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Pool().Put(conn)
	var state metadataPolicyIndexState
	err = sqlitex.ExecuteTransient(conn, "SELECT index_version, indexed_at, session_entries_hash, origin_version FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			state = metadataPolicyIndexState{IndexerVersion: stmt.ColumnInt(0), IndexedAt: stmt.ColumnInt64(1), EntriesHash: stmt.ColumnText(2), OriginVersion: stmt.ColumnInt(3)}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPipelineMetadataReadPolicy(t *testing.T) {
	t.Parallel()
	fixtures := loadMetadataReadPolicyFixtures(t)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			sid := fixtures.SessionID
			nativePath := "/synthetic/native/" + string(sid) + ".jsonl"
			filesystem := &metadataPolicyFS{MemFS: testutil.NewMemFS(), nativePath: nativePath}
			if err := filesystem.WriteFile(nativePath, []byte(fixtures.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			meta := makeReindexMeta(t, string(sid), nativePath)
			meta.SchemaVersion = fixture.SchemaVersion
			meta.Project.Hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			ingested := time.Now().Add(-2 * time.Hour).UnixMilli()
			meta.Timestamp.Ingested = &ingested
			hostDir := testutil.TestHostSlug
			if fixture.Nested {
				parentID := ingest.SessionID(testSessionID2)
				meta.ParentUUID = &parentID
				hostDir = filepath.Join(hostDir, string(parentID), "subagents")
			}
			metaPath, transcriptPath := setupPeasantSyncSession(t, filesystem.MemFS, testOutputDir, hostDir, string(sid), meta)
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &raw); err != nil {
				t.Fatal(err)
			}
			if fixture.AdapterVersion != nil {
				raw["adapterVersion"], err = json.Marshal(*fixture.AdapterVersion)
				if err != nil {
					t.Fatal(err)
				}
			}
			if fixture.RawAdapterVersion != "" {
				raw["adapterVersion"] = json.RawMessage(fixture.RawAdapterVersion)
			}
			if fixture.SchemaVersion > 10 {
				raw["futureSentinel"] = json.RawMessage(`{"must":"survive"}`)
			}
			beforeMetadata, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.RawMetadata != "" {
				beforeMetadata = []byte(fixture.RawMetadata)
			}
			if err := filesystem.WriteFile(metaPath, beforeMetadata, 0600); err != nil {
				t.Fatal(err)
			}
			if err := filesystem.WriteFile(transcriptPath, []byte(fixtures.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			modTime := time.UnixMilli(ingested).Add(-time.Hour)
			if fixture.SourceChanged {
				modTime = time.UnixMilli(ingested).Add(time.Hour)
			}
			session := makeDiscoveredSession(t, string(sid), nativePath, modTime)
			session.ParentUUID = meta.ParentUUID
			freshMeta := makeReindexMeta(t, string(sid), nativePath)
			freshMeta.ParentUUID = meta.ParentUUID
			freshMeta.Project.Hash = meta.Project.Hash
			adapter := &metadataPolicyAdapter{StubAdapter: &testutil.StubAdapter{
				ProviderValue: ingest.HarnessClaudeCode,
				Sessions:      []ingest.DiscoveredSession{session},
				Metadata:      map[ingest.SessionID]*ingest.UnifiedMetadata{sid: freshMeta},
			}}
			if fixture.OutsideDiscovery {
				adapter.Sessions = nil
			}
			options := []ingest.PipelineOption{}
			var database *store.Store
			var lookupStore *metadataPolicyStore
			var beforeEntries []schema.SessionEntry
			var beforeLocations map[ingest.SessionID]ingest.SessionLocation
			var beforeIndexState metadataPolicyIndexState
			var beforeMetrics *ingest.SessionMetrics
			if fixture.Database {
				database, err = store.Open(filepath.Join(t.TempDir(), "peasant.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = database.Close() })
				// Deliberately keep DB metadata at v9 for a future file.
				// This reproduces stale mirror state and exercises file refusal before
				// the DB's native source-info fallback can parse or stamp the session.
				seed := *meta
				seed.AdapterVersion = fixture.StoredAdapterVersion
				if seed.SchemaVersion > 10 {
					seed.SchemaVersion = 9
				}
				if fixture.StoredSchemaVersion != nil {
					seed.SchemaVersion = *fixture.StoredSchemaVersion
				}
				if fixture.WarmStoredSchema != nil {
					seed.SchemaVersion = *fixture.WarmStoredSchema
				}
				if !fixture.StoredAbsent {
					if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: &seed, Session: session}}); err != nil {
						t.Fatal(err)
					}
					producer := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion
					if fixture.Stale {
						producer--
					}
					preview := "last-good indexed content"
					writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Result: indexformat.V1{Entries: []schema.SessionEntry{{SessionID: sid, Harness: ingest.HarnessClaudeCode, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview}}}, IndexVersion: 1, IndexerVersion: producer, IndexedAtMs: ingested}})
					if len(writes) != 1 || !writes[0].Written {
						t.Fatalf("seed index: %+v", writes)
					}
				}
				lookupStore = &metadataPolicyStore{Store: database}
				options = append(options, ingest.WithStore(lookupStore), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database), ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Reindex, cfg.Force = fixture.Reindex, fixture.Force
			filesystem.metadataPath = metaPath
			filesystem.faultOperation = fixture.FaultOperation
			filesystem.transientIO = fixture.TransientIO
			switch fixture.FaultOperation {
			case "":
			case metadataPolicyReadFault, metadataPolicyStatFault:
				filesystem.faultPath = metaPath
			case metadataPolicyOutputListFault:
				filesystem.faultPath = testOutputDir
			case metadataPolicyHostListFault:
				filesystem.faultPath = filepath.Join(testOutputDir, testutil.TestHostSlug)
			case metadataPolicyTranscriptStatFault:
				filesystem.faultPath = transcriptPath
			default:
				t.Fatalf("unknown metadata I/O fault %q", fixture.FaultOperation)
			}
			if fixture.MetadataAbsent {
				if err := filesystem.MemFS.Remove(metaPath); err != nil {
					t.Fatal(err)
				}
			}
			pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return adapter }}, cfg, options...)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.WarmStoredSchema != nil {
				if fixture.Force || fixture.StoredSchemaVersion == nil || database == nil {
					t.Fatal("warm-cache fixture requires an unforced stored session with a final schema")
				}
				adapter.Sessions[0].ModTime = time.UnixMilli(ingested).Add(-time.Hour)
				if _, err := pipeline.Run(ctx); err != nil {
					t.Fatal(err)
				}
				if adapter.extracts.Load() != 0 {
					t.Fatal("cache warmup unexpectedly extracted native data")
				}
				adapter.Sessions[0] = session
				conn, err := database.Pool().Take(ctx)
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET schema_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{*fixture.StoredSchemaVersion, string(sid)}})
				database.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			}
			if database != nil {
				beforeEntries, err = database.ListEntries(ctx, sid)
				if err != nil {
					t.Fatal(err)
				}
				beforeLocations, err = database.BulkLookupSessionLocations(ctx, []ingest.SessionID{sid})
				if err != nil {
					t.Fatal(err)
				}
				beforeIndexState = readMetadataPolicyIndexState(t, database, sid)
				beforeMetrics, err = database.GetMetrics(ctx, sid)
				if err != nil {
					t.Fatal(err)
				}
				lookupStore.remainingFailures.Store(int64(fixture.LookupFailures))
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := adapter.extracts.Load(); got != int64(fixture.WantExtract) {
				t.Errorf("adapter extraction calls = %d, want %d", got, fixture.WantExtract)
			}
			if result.Summary.Indexed != fixture.WantIndexed {
				t.Errorf("indexed sessions = %d, want %d", result.Summary.Indexed, fixture.WantIndexed)
			}
			if fixture.LookupFailures != 0 && lookupStore.faults.Load() == 0 {
				t.Fatal("configured stored-location lookup failure was not reached")
			}
			if fixture.LookupFailures != 0 && result.Summary.Errors != 0 {
				t.Errorf("lookup refusal became a fatal session error: %+v", result.Summary)
			}
			if fixture.WantDiagnostic != "" {
				found := false
				for _, diagnostic := range result.Diagnostics {
					if strings.Contains(diagnostic.Message, fixture.WantDiagnostic) {
						found = true
					}
				}
				if !found {
					t.Errorf("missing diagnostic %q: %+v", fixture.WantDiagnostic, result.Diagnostics)
				}
			} else if fixture.LookupFailures != 0 && len(result.Diagnostics) != 0 {
				t.Errorf("compatible lookup recovery reported refusal: %+v", result.Diagnostics)
			}
			if fixture.ExpectedMetadataReads != nil && filesystem.metadataReads.Load() != int64(*fixture.ExpectedMetadataReads) {
				t.Errorf("metadata reads = %d, want %d", filesystem.metadataReads.Load(), *fixture.ExpectedMetadataReads)
			}
			if fixture.FaultOperation != "" && filesystem.faults.Load() == 0 {
				t.Fatalf("configured metadata I/O fault %q was not reached", fixture.FaultOperation)
			}
			afterMetadata, err := filesystem.MemFS.ReadFile(metaPath)
			if fixture.MetadataAbsent && fixture.WantExtract == 0 && !errors.Is(err, fs.ErrNotExist) {
				t.Error("refused session created previously missing metadata")
			}
			if err != nil && !(fixture.MetadataAbsent && fixture.WantExtract == 0 && errors.Is(err, fs.ErrNotExist)) {
				t.Fatal(err)
			}
			if fixture.WantExtract == 0 {
				if !fixture.MetadataAbsent && !bytes.Equal(afterMetadata, beforeMetadata) {
					t.Error("read-only metadata adoption changed bytes or producer evidence")
				}
				if filesystem.nativeRead.Load() != 0 || filesystem.nativeStat.Load() != 0 {
					t.Errorf("retained/no-work path accessed native input: read=%d stat=%d", filesystem.nativeRead.Load(), filesystem.nativeStat.Load())
				}
			}
			afterTranscript, err := filesystem.MemFS.ReadFile(transcriptPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterTranscript, []byte(fixtures.Transcript)) {
				t.Error("managed transcript changed")
			}
			afterNative, err := filesystem.MemFS.ReadFile(nativePath)
			if err != nil || !bytes.Equal(afterNative, []byte(fixtures.Transcript)) {
				t.Fatalf("native source changed: %v", err)
			}
			if database != nil && fixture.WantIndexed == 0 && fixture.WantExtract == 0 {
				afterEntries, err := database.ListEntries(ctx, sid)
				if err != nil || !reflect.DeepEqual(afterEntries, beforeEntries) {
					t.Fatalf("last-good index changed: %v", err)
				}
				afterLocations, err := database.BulkLookupSessionLocations(ctx, []ingest.SessionID{sid})
				if err != nil || !reflect.DeepEqual(afterLocations, beforeLocations) {
					t.Fatalf("session metadata stamps changed: %v", err)
				}
				afterMetrics, err := database.GetMetrics(ctx, sid)
				if err != nil || !reflect.DeepEqual(afterMetrics, beforeMetrics) {
					t.Fatalf("last-good metrics changed: %v", err)
				}
				afterState := readMetadataPolicyIndexState(t, database, sid)
				if afterState.OriginVersion != beforeIndexState.OriginVersion {
					t.Logf("separate pre-diff origin maintenance advanced its watermark from %d to %d; it is not adapter/indexer provenance", beforeIndexState.OriginVersion, afterState.OriginVersion)
					afterState.OriginVersion = beforeIndexState.OriginVersion
				}
				if afterState != beforeIndexState {
					t.Fatalf("actual index producer/state changed: got %+v, want %+v", afterState, beforeIndexState)
				}
			}
			if fixture.RetryLookup {
				if len(result.Diagnostics) == 0 || fixture.WantExtract != 0 || lookupStore == nil {
					t.Fatal("lookup retry fixture must first refuse a stored-lookup failure")
				}
				firstDiagnostics := append([]ingest.DiagnosticEntry(nil), result.Diagnostics...)
				lookupStore.remainingFailures.Store(0)
				retried, err := pipeline.Run(ctx)
				if err != nil || retried.Summary.Indexed != 1 || len(retried.Diagnostics) != 0 || adapter.extracts.Load() != 1 {
					t.Fatalf("lookup recovery did not refresh cleanly: result=%+v extracts=%d err=%v", retried, adapter.extracts.Load(), err)
				}
				if !reflect.DeepEqual(firstDiagnostics, result.Diagnostics) {
					t.Fatal("lookup recovery mutated prior diagnostic snapshot")
				}
			}
			if fixture.RetryCompatible {
				if len(result.Diagnostics) == 0 {
					t.Fatal("first refused run lost its diagnostics")
				}
				firstDiagnostics := append([]ingest.DiagnosticEntry(nil), result.Diagnostics...)
				meta.SchemaVersion = 9
				compatible, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err := filesystem.MemFS.WriteFile(metaPath, compatible, 0600); err != nil {
					t.Fatal(err)
				}
				retried, err := pipeline.Run(ctx)
				if err != nil || len(retried.Diagnostics) != 0 {
					t.Fatalf("compatible repeat replayed old warnings: %+v %v", retried, err)
				}
				if !reflect.DeepEqual(firstDiagnostics, result.Diagnostics) {
					t.Fatal("later run mutated prior diagnostic snapshot")
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &envelope); err != nil {
					t.Fatal(err)
				}
				if _, exists := envelope["Diagnostics"]; exists {
					t.Fatal("internal diagnostic field entered raw result JSON")
				}
			}
		})
	}
}
