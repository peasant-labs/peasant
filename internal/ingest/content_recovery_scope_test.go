package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/content_recovery_scope.yaml
var contentRecoveryScopeFixtureData []byte

// recoveryScopeOutcome is the observable result of one scoped recovery run.
type recoveryScopeOutcome string

const (
	recoveryScopeRecovered recoveryScopeOutcome = "recovered"
	recoveryScopeUntouched recoveryScopeOutcome = "untouched"
	recoveryScopeRefused   recoveryScopeOutcome = "refused"
)

func newRecoveryScopeOutcome(raw string) (recoveryScopeOutcome, error) {
	switch outcome := recoveryScopeOutcome(raw); outcome {
	case recoveryScopeRecovered, recoveryScopeUntouched, recoveryScopeRefused:
		return outcome, nil
	}
	return "", fmt.Errorf("content recovery scope fixture: unknown expected outcome %q; use recovered, untouched or refused", raw)
}

type contentRecoveryScopeFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name             string `yaml:"name"`
		Harness          string `yaml:"harness"`
		SinceAfterStart  bool   `yaml:"since_after_start"`
		OtherSessionOnly bool   `yaml:"other_session_only"`
		FutureProducer   bool   `yaml:"future_producer"`
		FutureSchema     bool   `yaml:"future_stored_schema"`
		PreExistingReads int    `yaml:"pre_existing_retained_reads"`
		NoIndexLog       bool   `yaml:"no_index_log"`
		Expect           string `yaml:"expect"`
	} `yaml:"cases"`
}

func loadContentRecoveryScopeFixtures(t *testing.T) contentRecoveryScopeFixtures {
	t.Helper()
	var fixtures contentRecoveryScopeFixtures
	if err := yaml.Unmarshal(contentRecoveryScopeFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate content recovery scope fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing content recovery scope fixture %s", name)
		}
	}
	return fixtures
}

func TestContentRecoveryScope(t *testing.T) {
	fixtures := loadContentRecoveryScopeFixtures(t)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			expect, err := newRecoveryScopeOutcome(fixture.Expect)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			database, err := store.Open(filepath.Join(t.TempDir(), "scope.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			id, err := ingest.NewSessionID("ses_scopetarget")
			if err != nil {
				t.Fatal(err)
			}
			meta := makeMinimalMeta(t, id.String())
			meta.Project.Hash = testutil.TestProjectHash
			meta.Source.FilePath = "/synthetic/missing.jsonl"
			stored := *meta
			if fixture.FutureSchema {
				// The stored row claims a schema this build does not know; the
				// retained file stays at the current schema.
				stored.SchemaVersion = ingest.CurrentSchemaVersion + 1
			}
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: &stored}}); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(testOutputDir, testutil.TestHostSlug)
			path := filepath.Join(dir, id.String(), id.String()+"--transcript.jsonl")
			data := []byte(`{"type":"assistant","message":{"role":"assistant","content":"scoped recovery target"}}`)
			session := ingest.DiscoveredSession{SessionID: id, Harness: ingest.HarnessClaudeCode, SourcePath: ingest.ResolvedPath(path), SourceFormat: ingest.SourceFormatJSONL}
			indexer := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[ingest.HarnessClaudeCode]
			entries, err := indexer.IndexTranscriptBytes(ctx, session, data)
			if err != nil {
				t.Fatal(err)
			}
			producer := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion
			if fixture.FutureProducer {
				producer++
			}
			// A stored projection without a content capture row is a recovery
			// target; the stored producer stamp decides whether this build may touch it.
			results := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, IndexerVersion: producer, IndexedAtMs: 1700000001000}})
			if len(results) != 1 || results[0].Err != nil || !results[0].Written {
				t.Fatalf("seed projection: %+v", results)
			}
			metadata, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(filepath.Join(dir, id.String(), id.String()+"--metadata.json"), metadata, 0600); err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := database.ListEntries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Reindex = true
			if fixture.Harness != "" {
				var harness schema.Harness
				if err := harness.UnmarshalText([]byte(fixture.Harness)); err != nil || !harness.IsKnown() {
					t.Fatalf("fixture harness %q is not a known harness: %v", fixture.Harness, err)
				}
				cfg.Harness = &harness
			}
			if fixture.SinceAfterStart {
				since := time.UnixMilli(meta.Timestamp.Start).Add(time.Hour)
				cfg.Since = &since
			}
			if fixture.OtherSessionOnly {
				other, err := ingest.NewSessionID("ses_scopeother")
				if err != nil {
					t.Fatal(err)
				}
				cfg.AllowedSessionIDs = map[ingest.SessionID]bool{other: true}
			}
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(nil, nil)}
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})), ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
			if err != nil {
				t.Fatal(err)
			}
			fs.ResetCounts()
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatalf("reindex: %v", err)
			}
			if fs.ReadCount(meta.Source.FilePath) != 0 {
				t.Fatalf("retained recovery read the native source %d times", fs.ReadCount(meta.Source.FilePath))
			}
			logged, recoveryLogged := 0, 0
			for _, entry := range result.IndexLog {
				if entry.SessionID == id {
					logged++
					if entry.Reason != nil && *entry.Reason == "content recovered from retained input" {
						recoveryLogged++
					}
				}
			}
			diagnostics := make(map[string]ingest.DiagnosticEntry)
			for _, diagnostic := range result.Diagnostics {
				diagnostics[diagnostic.ErrorType] = diagnostic
			}
			capture, found, err := database.GetSessionContentCapture(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			after, err := database.ListEntries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			switch expect {
			case recoveryScopeRecovered:
				if !found || capture.Status != ingest.ContentCaptureComplete {
					t.Fatalf("in-scope session was not recovered: found=%t capture=%+v diagnostics=%+v", found, capture, result.Diagnostics)
				}
				if _, refused := diagnostics["content_recovery_refused"]; refused {
					t.Fatalf("in-scope current session was refused: %+v", diagnostics["content_recovery_refused"])
				}
				// Recovery repairs content only. The same run still evaluates the
				// session's indexer work and verifies its input at the current producer.
				state, err := database.ReadIndexState(ctx, id)
				if err != nil || state == nil || state.IndexedInputHash == nil || state.IndexerVersion != producer {
					t.Fatalf("recovered session was excluded from independent index work: %+v %v", state, err)
				}
				if fs.ReadCount(path) == 0 {
					t.Fatal("recovery did not read the retained transcript")
				}
				// The repair is reported in the run's index log, counted once, and
				// persisted as exactly one recovery row.
				reported := false
				for _, entry := range result.IndexLog {
					if entry.SessionID == id && entry.Outcome == ingest.IndexOutcomeReindexed && entry.Reason != nil && *entry.Reason == "content recovered from retained input" {
						reported = true
					}
				}
				if !reported || result.Summary.Indexed != 1 {
					t.Fatalf("recovery is not reported and counted once: indexed=%d log=%+v", result.Summary.Indexed, result.IndexLog)
				}
				if rows := countRecoveryLogRows(t, database, id); rows != 1 {
					t.Fatalf("recovery persisted %d index_log rows, want exactly 1", rows)
				}
			case recoveryScopeUntouched:
				if found && capture.Status == ingest.ContentCaptureComplete {
					t.Fatalf("out-of-scope session was recovered: %+v", capture)
				}
				if fs.ReadCount(path) != 0 {
					t.Fatalf("out-of-scope session's retained transcript was read %d times", fs.ReadCount(path))
				}
				for errorType := range diagnostics {
					if errorType == "content_recovery_refused" || errorType == "content_recovery_unavailable" {
						t.Fatalf("out-of-scope session produced a recovery diagnostic: %+v", diagnostics[errorType])
					}
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("out-of-scope session entries changed: before=%+v after=%+v", before, after)
				}
			case recoveryScopeRefused:
				refused, ok := diagnostics["content_recovery_refused"]
				if !ok {
					t.Fatalf("future producer was not refused: %+v", result.Diagnostics)
				}
				if !containsAll(refused.Message, id.String(), "preserved") {
					t.Fatalf("refusal is not visible and actionable: %+v", refused)
				}
				if found && capture.Status == ingest.ContentCaptureComplete {
					t.Fatalf("refused session was written: %+v", capture)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("refused session entries changed: before=%+v after=%+v", before, after)
				}
				if reads := fs.ReadCount(path); reads != fixture.PreExistingReads {
					t.Fatalf("refused session's retained transcript was read %d times, want %d (recovery itself reads nothing; any pre-existing read outside recovery is pinned by the fixture)", reads, fixture.PreExistingReads)
				}
				if rows := countRecoveryLogRows(t, database, id); recoveryLogged != 0 || rows != 0 {
					t.Fatalf("refused session grew a recovery index-log entry: run=%d persisted=%d", recoveryLogged, rows)
				}
				if fixture.NoIndexLog && logged != 0 {
					t.Fatalf("stored-schema refusal grew %d index-log entries: %+v", logged, result.IndexLog)
				}
			}
		})
	}
}

// countRecoveryLogRows counts persisted index_log rows carrying the recovery
// reason for one session: the audit table must hold exactly one per repair.
func countRecoveryLogRows(t *testing.T, database *store.Store, id ingest.SessionID) int {
	t.Helper()
	conn, err := database.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Pool().Put(conn)
	rows := 0
	if err := sqlitex.ExecuteTransient(conn, "SELECT count(*) FROM index_log WHERE session_id = ? AND reason = ?", &sqlitex.ExecOptions{Args: []any{id.String(), "content recovered from retained input"}, ResultFunc: func(stmt *sqlite.Stmt) error { rows = stmt.ColumnInt(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func containsAll(text string, needles ...string) bool {
	for _, needle := range needles {
		if needle == "" || !strings.Contains(text, needle) {
			return false
		}
	}
	return true
}
