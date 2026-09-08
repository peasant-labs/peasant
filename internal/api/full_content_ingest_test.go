package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Only discovery and metadata extraction are controlled; materialization reads
// synthetic provider bytes and the production registry parses every entry.
type consumerSource struct{ *testutil.StubAdapter }

type consumerEmptyEnvironment struct{}

var _ ingest.OpenCodeEnvironmentLookup = consumerEmptyEnvironment{}

func (consumerEmptyEnvironment) LookupEnv(string) (string, bool) { return "", false }

var _ ingest.TranscriptMaterializer = (*consumerSource)(nil)
var _ ingest.SourceAdapter = (*consumerSource)(nil)

func (a *consumerSource) MaterializeTranscript(ctx context.Context, s ingest.DiscoveredSession) (ingest.MaterializedTranscript, error) {
	meta, err := a.ExtractMetadata(ctx, s)
	if err != nil {
		return ingest.MaterializedTranscript{}, err
	}
	data, err := os.ReadFile(s.SourcePath.String())
	fingerprint := sha256.Sum256(data)
	return ingest.MaterializedTranscript{Metadata: meta, Data: data, SourceFingerprint: fingerprint[:], EventSeq: s.EventSeq}, err
}

func ingestConsumerSession(t *testing.T, fixture fullConsumerFixture, id, basePath, text string) *store.Store {
	t.Helper()
	t.Setenv(ingest.EnvArenaSizeBytes, "1048576")
	harness := ingest.Harness(fixture.Harness)
	sid, err := ingest.NewSessionID(id)
	if err != nil {
		t.Fatal(err)
	}
	providerDir := t.TempDir()
	path := filepath.Join(providerDir, "source.jsonl")
	encoded, _ := json.Marshal(text)
	data := strings.NewReplacer("TEXT", string(encoded), "SESSION_ID", id).Replace(fixture.Source)
	if fixture.IncompleteTail {
		data += `{"type":"message"`
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	sourcePath, err := ingest.NewResolvedPath(path)
	if err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{SessionID: sid, Harness: harness, SourcePath: sourcePath, SourceFormat: ingest.SourceFormatJSONL, ModTime: time.Now().Add(-time.Hour)}
	if harness == ingest.HarnessOpenCode {
		session.TranscriptOrigin = ingest.TranscriptOriginOpenCodeLegacySQLite
		session.SourceFormat = ingest.SourceFormatJSON
	}
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID, meta.ModelHarness = sid, harness
	meta.Model = schema.ModelID("claude-opus-4-6")
	meta.HostSlug = schema.HostSlug("github.com-user-repo")
	meta.Source = ingest.SourceInfo{FilePath: path, Format: session.SourceFormat}
	ingested := int64(1700000120000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested}
	remote, branch := "git@github.com:user/repo.git", "main"
	meta.Git = ingest.GitContext{Remote: &remote, Branch: &branch}
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "myapp", FilePath: "/home/test/myapp"}
	dbPath := string(defaults.ResolveDBFilePath())
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	output, err := ingest.NewResolvedPath(basePath)
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[ingest.Harness]ingest.AdapterFactory{harness: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
		return &consumerSource{&testutil.StubAdapter{ProviderValue: harness, Sessions: []ingest.DiscoveredSession{session}, Metadata: map[ingest.SessionID]*ingest.UnifiedMetadata{sid: &meta}}}
	}}
	osfs := &ingest.OSFileSystem{}
	if harness == ingest.HarnessPi {
		adapters[harness] = ingest.DefaultAdapterRegistry[harness]
	}
	if fixture.NativeSource != "" {
		native := testfixture.MaterializeByName(t, fixture.NativeSource)
		sourcePath, err = ingest.NewResolvedPath(filepath.Dir(native.Path))
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(osfs, testutil.NoGitResolver(), salt.Salt{}, "latest", consumerEmptyEnvironment{}, osfs, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
		if err != nil {
			t.Fatal(err)
		}
		adapters[harness] = func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return adapter }
	}
	pipeline, err := ingest.NewPipeline(osfs, testutil.DefaultGitResolver(), adapters, ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{harness: {Enabled: true, Paths: []ingest.ResolvedPath{sourcePath}}}, OutputDir: output,
	}, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithAnalyzer(metrics.NewEngine(db)), ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)), ingest.WithIndexers(ingest.NewIndexerRegistry(osfs, ingest.IndexerRegistryOptions{})))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	capture, found, err := db.GetSessionContentCapture(t.Context(), sid)
	if fixture.Damage == "source-omitted" {
		if err != nil || found && capture.Status == ingest.ContentCaptureComplete {
			t.Fatalf("native omission certified: %+v %v", capture, err)
		}
		// Retained metadata and the managed artifact must both keep the omission
		// ineligible even when force reindex tries to recover content later.
		reindex, err := ingest.NewPipeline(osfs, testutil.DefaultGitResolver(), adapters, ingest.PipelineConfig{OutputDir: output, Reindex: true, Force: true}, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(ingest.NewIndexerRegistry(osfs, ingest.IndexerRegistryOptions{})))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = reindex.Run(t.Context())
		capture, found, err = db.GetSessionContentCapture(t.Context(), sid)
		if err != nil || found && capture.Status == ingest.ContentCaptureComplete {
			t.Fatalf("retained omission certified: %+v %v", capture, err)
		}
		return db
	}
	if err != nil || !found || capture.Status != ingest.ContentCaptureComplete {
		t.Fatalf("normal ingest did not capture full content: %+v %v", capture, err)
	}
	if harness == ingest.HarnessPi {
		input, err := db.LoadPublicationInput(t.Context(), sid)
		if err != nil {
			t.Fatal(err)
		}
		foundTail := false
		for _, warning := range input.Metadata.Diagnostics.Warnings {
			if warning.ErrorType == "incomplete_tail" {
				foundTail = true
				if warning.Location == "" || warning.Remediation == "" {
					t.Fatal("prefix diagnostic is not actionable")
				}
			}
		}
		if foundTail != fixture.IncompleteTail {
			t.Fatal("native accepted-prefix diagnostic changed")
		}
		locations, err := db.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{sid})
		if err != nil {
			t.Fatal(err)
		}
		accepted := strings.TrimSuffix(data, `{"type":"message"`)
		hash := sha256.Sum256([]byte(accepted))
		if !bytes.Equal(hash[:], locations[sid].SourceFingerprint) {
			t.Fatal("full native capture fingerprint differs from consumed prefix")
		}
		repeat, err := pipeline.Run(t.Context())
		if err != nil || repeat.Summary.Unchanged != 1 || repeat.Summary.Indexed != 0 {
			t.Fatalf("stable full native capture reindexed: %+v %v", repeat, err)
		}
	}
	if err := os.RemoveAll(providerDir); err != nil {
		t.Fatal(err)
	}
	if fixture.Backfill {
		var annotationID string
		var associations []ingest.CurrentCommitAssociation
		if harness == ingest.HarnessPi {
			annotator, err := db.GetAnnotatorIDByName(t.Context(), "human-web")
			if err != nil {
				t.Fatal(err)
			}
			typeID, err := db.GetAnnotationTypeID(t.Context(), "quality.frustration_signal")
			if err != nil {
				t.Fatal(err)
			}
			annotationID, err = db.CreateEntryAnnotation(t.Context(), ingest.EntryAnnotationParams{SessionID: sid.String(), EntryIndex: 0, EndIndex: 0, AnnotatorID: annotator, AnnotationTypeID: typeID, Value: fixture.Annotation})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertSessionCommits(t.Context(), sid, []ingest.CommitInfo{{Hash: fixture.Commit, Message: fixture.Annotation}}); err != nil {
				t.Fatal(err)
			}
			associations, err = db.ListCurrentSessionCommitAssociations(t.Context(), sid)
			if err != nil || len(associations) != 1 {
				t.Fatalf("seed native association: %v", err)
			}
		}
		// Reproduce a preview-only pre-upgrade database. The only recovery
		// source now available is the artifact created by normal ingest above.
		conn, err := db.Pool().Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Preserve the valid metadata/index proof, as the V52 upgrade does.
		// A manual legacy index write would intentionally invalidate that proof.
		err = sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_full_content WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid)}})
		if err == nil {
			err = sqlitex.ExecuteTransient(conn, `DELETE FROM session_content_captures WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid)}})
		}
		if err == nil && fixture.Mismatch {
			err = sqlitex.ExecuteTransient(conn, `UPDATE session_entries SET tool_output='legacy output' WHERE session_id=? AND tool_output IS NOT NULL`, &sqlitex.ExecOptions{Args: []any{string(sid)}})
		}
		db.Pool().Put(conn)
		if err != nil {
			t.Fatal(err)
		}
		reindex, err := ingest.NewPipeline(osfs, testutil.DefaultGitResolver(), adapters, ingest.PipelineConfig{
			OutputDir: output, Reindex: true, Force: true,
		}, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithAnalyzer(metrics.NewEngine(db)), ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)), ingest.WithIndexers(ingest.NewIndexerRegistry(osfs, ingest.IndexerRegistryOptions{})))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reindex.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		capture, found, err = db.GetSessionContentCapture(t.Context(), sid)
		if err != nil || !found || capture.Status != ingest.ContentCaptureComplete || capture.SourceAuthority != ingest.ContentSourcePeasantSnapshot {
			t.Fatalf("retained artifact did not backfill content: %+v %v", capture, err)
		}
		if harness == ingest.HarnessPi {
			annotations, err := db.GetAnnotationsForEntry(t.Context(), sid.String(), 0)
			if err != nil {
				t.Fatal(err)
			}
			kept := false
			for _, annotation := range annotations {
				if annotation.ID == annotationID && annotation.Value == fixture.Annotation {
					kept = true
				}
			}
			if !kept {
				t.Fatal("native retained recovery lost human annotation anchor")
			}
			after, err := db.ListCurrentSessionCommitAssociations(t.Context(), sid)
			if err != nil || !reflect.DeepEqual(associations, after) {
				t.Fatal("native retained recovery changed commit associations")
			}
		}
	}
	// Remove every retained artifact, including generated publication metadata.
	// All consumers must use the reopened database without a filesystem fallback.
	removed := 0
	err = filepath.WalkDir(basePath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			removed++
			return os.Remove(path)
		}
		return nil
	})
	if err != nil || removed == 0 {
		t.Fatalf("remove retained transcripts: removed=%d error=%v", removed, err)
	}
	return db
}

func assertFullToolSemantics(t *testing.T, db *store.Store, sid ingest.SessionID, file, text string) {
	t.Helper()
	entries, err := db.ListEntries(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	inputFound, outputFound := false, false
	for _, entry := range entries {
		if entry.ToolInput != nil {
			var input struct {
				Content  string `json:"content"`
				FilePath string `json:"file_path"`
			}
			if err := json.Unmarshal([]byte(*entry.ToolInput), &input); err != nil {
				t.Fatal(err)
			}
			inputFound = input.Content == text && input.FilePath == file && strings.Index(*entry.ToolInput, file) > defaults.ContentPreviewLimit
		}
		if entry.ToolOutput != nil && *entry.ToolOutput == text {
			outputFound = true
		}
	}
	if !inputFound || !outputFound {
		t.Fatal("long tool input/output lost after ingest and reopen")
	}
	m, err := db.GetMetrics(t.Context(), sid)
	if err != nil || m == nil || m.FilesTouched == nil || *m.FilesTouched != 1 {
		t.Fatalf("long tool JSON did not drive files-touched metric: %+v %v", m, err)
	}
	annotations, err := db.ListSystemAnnotations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scopeFound := false
	for _, annotation := range annotations {
		if annotation.TypeID == "metadata.session_scope" && annotation.Value == filepath.Dir(file) && annotation.SessionID != nil && *annotation.SessionID == sid.String() {
			scopeFound = true
		}
	}
	if !scopeFound {
		t.Fatal("long tool JSON did not produce stored scope annotation")
	}
	svc := codemap.NewService(db, func(string) gitops.Repository { return testutil.NoGitRepository() }, nil, sessionvisibility.All())
	tasks, err := svc.ProjectTasks(t.Context(), testutil.TestProjectHash, "internal/widget.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks.Tasks) != 1 || len(tasks.Tasks[0].EditedFiles) != 1 || tasks.Tasks[0].EditedFiles[0] != "internal/widget.go" {
		t.Fatalf("long tool JSON lost codemap evidence: %+v", tasks)
	}
}
