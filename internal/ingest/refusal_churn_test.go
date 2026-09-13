package ingest_test

import (
	"context"
	_ "embed"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/refusal_churn.yaml
var refusalChurnYAML []byte

type refusalChurnFixture struct {
	SessionID           string   `yaml:"session_id"`
	Required            []string `yaml:"required_names"`
	Transcript          string   `yaml:"transcript"`
	UnrepresentedRecord string   `yaml:"unrepresented_record"`
	Cases               []struct {
		Name              string `yaml:"name"`
		Stored            string `yaml:"stored"`
		Mutate            string `yaml:"mutate"`
		SecondSourceReads int    `yaml:"second_source_reads"`
		SecondUpdated     int    `yaml:"second_updated"`
		SecondUnchanged   int    `yaml:"second_unchanged"`
	} `yaml:"cases"`
}

func loadRefusalChurnFixture(t *testing.T) refusalChurnFixture {
	t.Helper()
	var fixture refusalChurnFixture
	if err := yaml.Unmarshal(refusalChurnYAML, &fixture); err != nil {
		t.Fatalf("decode refusal churn fixture: %v", err)
	}
	names := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || names[testCase.Name] {
			t.Fatalf("empty or duplicate refusal churn fixture %q", testCase.Name)
		}
		names[testCase.Name] = true
	}
	for _, name := range fixture.Required {
		if !names[name] {
			t.Fatalf("missing refusal churn fixture %s", name)
		}
	}
	if len(fixture.Required) == 0 {
		t.Fatal("refusal churn fixture declares no required cases")
	}
	return fixture
}

// TestSettledRefusalDoesNotChurn proves the harvest after a settled refusal
// does no native read and no index write, while the states that should still
// work keep working: a legacy preview, changed bytes, a newer indexer, and a
// refusal whose input proof is gone. Each case runs two harvests over one file
// system and one database and states what the second owes, so a run that
// stopped churning by refusing to do real work is visible as a missing update
// in the same fixture.
func TestSettledRefusalDoesNotChurn(t *testing.T) {
	fixture := loadRefusalChurnFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			runSettledRefusalChurnCase(t, fixture, testCase.Name, testCase.Stored, testCase.Mutate, testCase.SecondSourceReads, testCase.SecondUpdated, testCase.SecondUnchanged)
		})
	}
}

func runSettledRefusalChurnCase(t *testing.T, fixture refusalChurnFixture, name, stored, mutate string, wantReads, wantUpdated, wantUnchanged int) {
	t.Helper()
	ctx := context.Background()
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	database, err := store.Open(filepath.Join(t.TempDir(), "churn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	indexStore := &countingIndexStore{Store: database}

	sid, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(testSourceDir, fixture.SessionID+".jsonl")
	source := fixture.Transcript
	if stored == "refusal" {
		source += fixture.UnrepresentedRecord + "\n"
	}
	if err := fs.WriteFile(sourcePath, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}

	meta := makeMinimalMeta(t, sid.String())
	meta.Source.FilePath = sourcePath
	meta.Source.Format = ingest.SourceFormatJSONL
	session := makeDiscoveredSession(t, sid.String(), sourcePath, time.Unix(1_700_000_000, 0))
	metaMap := map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta}
	sessions := []ingest.DiscoveredSession{session}

	run := func(versions map[ingest.Harness]ingest.HarvesterVersions) *ingest.PipelineResult {
		t.Helper()
		cfg := makePipelineConfig(testOutputDir)
		adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(sessions, metaMap)}
		pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
			ingest.WithStore(indexStore), ingest.WithMetricsStore(indexStore), ingest.WithIndexLogger(database),
			ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
			ingest.WithHarvesterVersions(versions))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			t.Fatalf("harvest: %v", err)
		}
		return result
	}

	versions := cloneHarvesterVersions()
	first := run(versions)
	if first.Summary.New != 1 {
		t.Fatalf("%s: first harvest did not ingest the session: %+v", name, first.Summary)
	}
	capture, found, err := database.GetSessionContentCapture(ctx, sid)
	if err != nil || !found {
		t.Fatalf("first harvest stored no capture: found=%t err=%v", found, err)
	}
	switch stored {
	case "refusal":
		if capture.FailureCode != ingest.ContentCaptureStrictRefused {
			t.Fatalf("first harvest did not record the strict refusal: %+v; diagnostics=%+v", capture, first.Diagnostics)
		}
	case "legacy_preview":
		// The migrated shape: a complete capture rewritten to the preview-only
		// state the content-capture migration wrote for sessions that predate
		// it. Nothing refused it, so it is pending work, not a steady state.
		execSessionSQL(t, database, `UPDATE session_content_captures SET status='incomplete', capture_format='preview_only', failure_code='legacy_preview_only', failure_message='Full content has not been captured' WHERE session_id=?`, sid.String())
		execSessionSQL(t, database, `UPDATE sessions SET indexed_input_hash=NULL WHERE session_id=?`, sid.String())
	default:
		t.Fatalf("unknown stored state %q", stored)
	}

	switch mutate {
	case "none":
	case "append_record":
		changed := source + fixture.UnrepresentedRecord + "\n"
		if err := fs.WriteFile(sourcePath, []byte(changed), 0600); err != nil {
			t.Fatal(err)
		}
		sessions[0].ModTime = time.Now().Add(time.Hour)
	case "bump_indexer":
		bumped := versions[ingest.HarnessClaudeCode]
		bumped.IndexerVersion++
		versions[ingest.HarnessClaudeCode] = bumped
	case "clear_input_proof":
		execSessionSQL(t, database, `UPDATE sessions SET indexed_input_hash=NULL WHERE session_id=?`, sid.String())
	default:
		t.Fatalf("unknown mutation %q", mutate)
	}

	fs.ResetCounts()
	indexStore.indexedSessions.Store(0)
	second := run(versions)
	reads := fs.ReadCount(sourcePath)
	if reads != wantReads {
		t.Fatalf("%s: the second harvest read the native source %d times, want %d; the settled state must owe no read and every other state must still read it", name, reads, wantReads)
	}
	if second.Summary.Updated != wantUpdated || second.Summary.Unchanged != wantUnchanged {
		t.Fatalf("%s: the second harvest reported updated=%d unchanged=%d, want updated=%d unchanged=%d; summary=%+v", name, second.Summary.Updated, second.Summary.Unchanged, wantUpdated, wantUnchanged, second.Summary)
	}
	indexed := indexStore.indexedSessions.Load()
	if (indexed > 0) != (wantUpdated > 0) {
		t.Fatalf("%s: the second harvest indexed %d session(s), want %s; a settled state must write no index and every other state must re-index", name, indexed, map[bool]string{true: "at least one", false: "none"}[wantUpdated > 0])
	}
}

// countingIndexStore counts the sessions the pipeline asks the real store to
// index, so the churn fixture can assert "no index write" as observable
// behavior rather than inferring it from the summary.
type countingIndexStore struct {
	*store.Store
	indexedSessions atomic.Int64
}

func (s *countingIndexStore) IndexSessionEntryBatch(ctx context.Context, writes []ingest.SessionEntryWrite) []ingest.SessionEntryWriteResult {
	s.indexedSessions.Add(int64(len(writes)))
	return s.Store.IndexSessionEntryBatch(ctx, writes)
}

func cloneHarvesterVersions() map[ingest.Harness]ingest.HarvesterVersions {
	versions := make(map[ingest.Harness]ingest.HarvesterVersions, len(ingest.HarvesterVersionRegistry))
	for harness, target := range ingest.HarvesterVersionRegistry {
		versions[harness] = target
	}
	return versions
}

func execSessionSQL(t *testing.T, database *store.Store, statement string, args ...any) {
	t.Helper()
	conn, err := database.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, statement, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}
