package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/index_format_output.yaml
var indexFormatOutputYAML []byte

type indexOutputPayload string

const (
	indexOutputV1     indexOutputPayload = "v1"
	indexOutputV2     indexOutputPayload = "v2"
	indexOutputEmpty  indexOutputPayload = "empty"
	indexOutputError  indexOutputPayload = "parse-error"
	indexOutputAbsent indexOutputPayload = "absent"
	indexOutputNil    indexOutputPayload = "nil-v1"
)

// transcriptIdentity says whether the fixture's transcript text belongs to one
// named session or can be shared by every session in a case. A harness whose
// transcript carries a session header refuses a header id that is not the
// reading session's, so its text must name the peer it is written for; the
// placeholder SESSION_ID is replaced with that peer's id.
type transcriptIdentity string

const (
	// transcriptIdentityShared is the default: the text names no session, so
	// both peers of a mixed run can be seeded from the same string.
	transcriptIdentityShared transcriptIdentity = ""
	// transcriptIdentitySessionScoped: the text names its own session.
	transcriptIdentitySessionScoped transcriptIdentity = "session-scoped"
)

// expandSessionPlaceholder writes fixture text for one session. Shared text is
// used as it stands, so a stray placeholder in a shared case stays visible
// instead of being silently filled in.
func (identity transcriptIdentity) expandSessionPlaceholder(text string, sessionID schema.SessionID) string {
	if identity != transcriptIdentitySessionScoped {
		return text
	}
	return strings.ReplaceAll(text, "SESSION_ID", string(sessionID))
}

type indexFormatOutputCase struct {
	Name                 string              `yaml:"name"`
	DeclaredFormat       int                 `yaml:"declaredFormat"`
	StoredFormat         int                 `yaml:"storedFormat"`
	Versioned            bool                `yaml:"versioned"`
	LogsOnly             bool                `yaml:"logsOnly"`
	Payload              indexOutputPayload  `yaml:"payload"`
	WantIndexed          bool                `yaml:"wantIndexed"`
	WantLogError         string              `yaml:"wantLogError"`
	WantConstructorError string              `yaml:"wantConstructorError"`
	WantDiagnostic       bool                `yaml:"wantDiagnostic"`
	RealIndexer          bool                `yaml:"realIndexer"`
	Harness              ingest.Harness      `yaml:"harness"`
	Transcript           string              `yaml:"transcript"`
	EmptyTranscript      bool                `yaml:"emptyTranscript"`
	SourceRoot           ingest.ResolvedPath `yaml:"sourceRoot"`
	SourceFiles          map[string]string   `yaml:"sourceFiles"`
	SourceDirectories    []string            `yaml:"sourceDirectories"`
	HealthyTranscript    string              `yaml:"healthyTranscript"`
	HealthySourceFiles   map[string]string   `yaml:"healthySourceFiles"`
	TranscriptIdentity   transcriptIdentity  `yaml:"transcriptIdentity"`
}

func loadIndexFormatOutputFixtures(t *testing.T) []indexFormatOutputCase {
	t.Helper()
	var document struct {
		RequiredNames []string                `yaml:"requiredNames"`
		Cases         []indexFormatOutputCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(indexFormatOutputYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("index output fixture needs one document: %v", err)
	}
	names := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid index output case %q", row.Name)
		}
		switch row.Payload {
		case indexOutputV1, indexOutputV2, indexOutputEmpty, indexOutputError, indexOutputAbsent, indexOutputNil:
		default:
			t.Fatalf("unknown output payload %q", row.Payload)
		}
		switch row.TranscriptIdentity {
		case transcriptIdentityShared, transcriptIdentitySessionScoped:
		default:
			t.Fatalf("index output case %q declares the unknown transcript identity %q; a transcript either names its own session or names none", row.Name, row.TranscriptIdentity)
		}
		if row.TranscriptIdentity == transcriptIdentityShared && strings.Contains(row.Transcript+row.HealthyTranscript, "SESSION_ID") {
			t.Fatalf("index output case %q holds a SESSION_ID placeholder in transcript text it declares shared; declare transcriptIdentity: session-scoped so each peer is seeded with its own id", row.Name)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index output", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

type outputFixtureIndexer struct {
	payload indexOutputPayload
	parsed  bool
}

var _ ingest.TranscriptIndexer = (*outputFixtureIndexer)(nil)

func (*outputFixtureIndexer) SourceKind() ingest.TranscriptSourceKind {
	return ingest.TranscriptSourceFile
}
func (indexer *outputFixtureIndexer) IndexTranscript(_ context.Context, session ingest.DiscoveredSession) ([]schema.SessionEntry, error) {
	indexer.parsed = true
	if indexer.payload == indexOutputError {
		return nil, errors.New("synthetic parser failure")
	}
	if indexer.payload == indexOutputEmpty {
		return nil, nil
	}
	text := "current concrete result"
	return []schema.SessionEntry{{SessionID: session.SessionID, EntryIndex: 0, Harness: session.Harness, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text}}, nil
}
func (indexer *outputFixtureIndexer) IndexTranscriptBytes(ctx context.Context, session ingest.DiscoveredSession, _ []byte) ([]schema.SessionEntry, error) {
	return indexer.IndexTranscript(ctx, session)
}

type versionedOutputFixtureIndexer struct{ *outputFixtureIndexer }
type outputFixtureV2 struct{}

func (outputFixtureV2) IndexVersion() int { return 2 }

var _ indexformat.Result = outputFixtureV2{}
var _ ingest.VersionedTranscriptIndexer = (*versionedOutputFixtureIndexer)(nil)

func (indexer *versionedOutputFixtureIndexer) IndexTranscriptResult(ctx context.Context, session ingest.DiscoveredSession) (indexformat.Result, error) {
	if indexer.payload == indexOutputNil {
		indexer.parsed = true
		return (*indexformat.V1)(nil), nil
	}
	if indexer.payload == indexOutputAbsent {
		indexer.parsed = true
		return nil, nil
	}
	if indexer.payload == indexOutputV2 {
		indexer.parsed = true
		return outputFixtureV2{}, nil
	}
	entries, err := indexer.IndexTranscript(ctx, session)
	if err != nil {
		return nil, err
	}
	return indexformat.V1{Entries: entries}, nil
}
func (indexer *versionedOutputFixtureIndexer) IndexTranscriptBytesResult(ctx context.Context, session ingest.DiscoveredSession, _ []byte) (indexformat.Result, error) {
	return indexer.IndexTranscriptResult(ctx, session)
}

func TestPipelinePersistsDeclaredConcreteIndexOutput(t *testing.T) {
	for _, row := range loadIndexFormatOutputFixtures(t) {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			fs := testutil.NewMemFS()
			sid := schema.SessionID(testutil.TestSessionUUID)
			sourceBytes := make(map[string][]byte)
			for name, content := range row.SourceFiles {
				path := filepath.Join(row.SourceRoot.String(), strings.ReplaceAll(name, "SESSION_ID", string(sid)))
				data := []byte(content)
				if err := fs.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				sourceBytes[path] = data
			}
			for _, directory := range row.SourceDirectories {
				if err := fs.MkdirAll(filepath.Join(row.SourceRoot.String(), strings.ReplaceAll(directory, "SESSION_ID", string(sid))), 0700); err != nil {
					t.Fatal(err)
				}
			}
			harness := row.Harness
			if harness == "" {
				harness = ingest.HarnessClaudeCode
			}
			meta := makeReindexMeta(t, string(sid), "/missing/native.jsonl")
			meta.ModelHarness = harness
			if harness == ingest.HarnessOpenCode {
				meta.Source.Format = ingest.SourceFormatJSON
			}
			meta.Project.Hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			metadataPath, transcriptPath := setupPeasantSyncSession(t, fs, testOutputDir, testutil.TestHostSlug, string(sid), meta)
			if row.Transcript != "" || row.EmptyTranscript {
				if err := fs.WriteFile(transcriptPath, []byte(row.TranscriptIdentity.expandSessionPlaceholder(row.Transcript, sid)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			db, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}
			// These cases test index output, not first-time file reconciliation.
			// Establish the actual mirror before asserting immutable input bytes.
			storetest.MirrorRetainedPair(t, db, fs, testOutputDir, metadataPath, sid)
			beforeMetadata, err := fs.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeTranscript, err := fs.ReadFile(transcriptPath)
			if err != nil {
				t.Fatal(err)
			}
			old := "last-good stored result"
			oldEntry := schema.SessionEntry{SessionID: sid, EntryIndex: 0, Harness: harness, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &old}
			if harness == ingest.HarnessPi {
				// A Pi row carries typed evidence or the store refuses to decode
				// it, so the last-good seed must be a row Pi could have written.
				extra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraCarrier, Harness: schema.HarnessPi})
				if err != nil {
					t.Fatal(err)
				}
				oldEntry.Extra = extra
			}
			oldEntries := []schema.SessionEntry{oldEntry}
			seed := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Result: indexformat.V1{Entries: oldEntries}, IndexVersion: 1, IndexerVersion: 14, IndexedAtMs: 1700000000000}})
			if !seed[0].Written {
				t.Fatalf("seed failed: %v", seed[0].Err)
			}
			before, err := db.ListEntries(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if row.StoredFormat > 0 {
				conn, err := db.Pool().Take(ctx)
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_format_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{row.StoredFormat, string(sid)}})
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			}
			plainIndexer := &outputFixtureIndexer{payload: row.Payload}
			var indexer ingest.TranscriptIndexer = plainIndexer
			if row.Versioned {
				indexer = &versionedOutputFixtureIndexer{plainIndexer}
			}
			if row.RealIndexer {
				indexer = ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[harness]
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Reindex = !row.LogsOnly
			if row.SourceRoot != "" {
				cfg.Sources = map[ingest.Harness]ingest.SourceConfig{harness: {Enabled: true, Paths: []ingest.ResolvedPath{row.SourceRoot}}}
			}
			options := []ingest.PipelineOption{ingest.WithHarvesterVersions(map[ingest.Harness]ingest.HarvesterVersions{harness: {AdapterVersion: 1, IndexerVersion: 15, IndexVersion: row.DeclaredFormat}})}
			if !row.LogsOnly {
				options = append(options, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexLogger(db), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{harness: indexer}))
			}
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{harness: makeStubAdapter(nil, nil)}, cfg, options...)
			if row.WantConstructorError != "" {
				if err == nil || !strings.Contains(err.Error(), row.WantConstructorError) {
					t.Fatalf("constructor error=%v, want %q", err, row.WantConstructorError)
				}
				if plainIndexer.parsed {
					t.Fatal("constructor refusal called parser")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if row.LogsOnly {
				return
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			afterMetadata, err := fs.ReadFile(metadataPath)
			for path, before := range sourceBytes {
				after, readErr := fs.ReadFile(path)
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatalf("native fixture changed %s: %v", path, readErr)
				}
			}
			if err != nil || !bytes.Equal(beforeMetadata, afterMetadata) {
				t.Fatalf("indexing changed managed metadata: %v\nbefore: %s\nafter: %s", err, beforeMetadata, afterMetadata)
			}
			afterTranscript, err := fs.ReadFile(transcriptPath)
			if err != nil || !bytes.Equal(beforeTranscript, afterTranscript) {
				t.Fatalf("indexing changed transcript input: %v", err)
			}
			if !row.RealIndexer {
				refusedBeforeParse := row.StoredFormat > 0 && !db.SupportsIndexFormat(row.StoredFormat)
				if plainIndexer.parsed == refusedBeforeParse {
					t.Fatalf("intended parser behavior not reached: parsed=%t stored-format-refusal=%t log=%+v", plainIndexer.parsed, refusedBeforeParse, result.IndexLog)
				}
			}
			if (result.Summary.Indexed == 1) != row.WantIndexed {
				t.Fatalf("indexed=%d, want successful=%t; log=%+v", result.Summary.Indexed, row.WantIndexed, result.IndexLog)
			}
			var attempt *ingest.IndexLogEntry
			for index := range result.IndexLog {
				if result.IndexLog[index].SessionID == sid {
					if attempt != nil {
						t.Fatalf("session was retried within the same invocation: %+v", result.IndexLog)
					}
					attempt = &result.IndexLog[index]
				}
			}
			if attempt == nil {
				t.Fatalf("session has no recorded index outcome: %+v", result)
			}
			if row.WantDiagnostic {
				visible := false
				for _, diagnostic := range result.Diagnostics {
					visible = visible || strings.Contains(diagnostic.Message, "previous index and producer stamps were preserved") && diagnostic.Remediation != ""
				}
				if !visible || attempt.Outcome != ingest.IndexOutcomeSkipped || attempt.ErrorMessage != nil {
					t.Fatalf("ambiguous empty result was not a nonfatal warning: diagnostics=%+v log=%+v", result.Diagnostics, attempt)
				}
			}
			if row.WantLogError != "" && (attempt.ErrorMessage == nil || !strings.Contains(*attempt.ErrorMessage, row.WantLogError)) {
				logged := "<none>"
				if attempt.ErrorMessage != nil {
					logged = *attempt.ErrorMessage
				}
				t.Fatalf("index log for %s lacks %q; it recorded %q (outcome %s)", sid, row.WantLogError, logged, attempt.Outcome)
			}
			if row.WantLogError != "" {
				visible := false
				for _, diagnostic := range result.Diagnostics {
					visible = visible || strings.Contains(diagnostic.Message, row.WantLogError) && diagnostic.Remediation != ""
				}
				if !visible || result.Summary.Errors != 0 || attempt.Outcome != ingest.IndexOutcomeError {
					t.Fatalf("index refusal is not visible and nonfatal: diagnostics=%+v summary=%+v", result.Diagnostics, result.Summary)
				}
			}
			data, err := json.Marshal(attempt)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["IndexVersion"]) != "15" || fields["IndexFormatVersion"] != nil || fields["IndexerVersion"] != nil {
				t.Fatalf("legacy audit JSON changed: %s", data)
			}
			after, err := db.ListEntries(ctx, sid)
			if row.StoredFormat > 1 {
				var unsupported *store.UnsupportedIndexFormatError
				if !errors.As(err, &unsupported) {
					t.Fatalf("unknown format unexpectedly became readable: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if row.WantIndexed {
				if row.Payload == indexOutputEmpty && len(after) != 0 {
					t.Fatalf("empty success retained stale entries: %+v", after)
				}
			} else if row.StoredFormat <= 1 && !reflect.DeepEqual(before, after) {
				t.Fatal("failed parse replaced last-good entries")
			}
			conn, err := db.Pool().Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Pool().Put(conn)
			if err := sqlitex.ExecuteTransient(conn, `SELECT index_version, index_format_version, indexed_at FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
				wantProducer := 14
				wantFormat := 1
				if row.StoredFormat > 0 {
					wantFormat = row.StoredFormat
				}
				if row.WantIndexed {
					wantProducer = 15
				}
				if stmt.ColumnInt(0) != wantProducer || stmt.ColumnInt(1) != wantFormat {
					t.Errorf("incorrect actual producer/format=%d/%d", stmt.ColumnInt(0), stmt.ColumnInt(1))
				}
				if !row.WantIndexed && stmt.ColumnInt64(2) != 1700000000000 {
					t.Error("failed parser changed success timestamp")
				}
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
			if row.StoredFormat > 1 {
				if err := sqlitex.ExecuteTransient(conn, "SELECT content_preview FROM session_entries WHERE session_id = ? ORDER BY entry_index", &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
					if stmt.ColumnText(0) != old {
						t.Error("future-format refusal changed stored projection")
					}
					return nil
				}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := sqlitex.ExecuteTransient(conn, `SELECT index_version, index_format_version FROM index_log WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
				if stmt.ColumnInt(0) != 15 || stmt.ColumnType(1) == sqlite.TypeNull || stmt.ColumnInt(1) != row.DeclaredFormat {
					t.Error("attempt did not record declared parser/format")
				}
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
