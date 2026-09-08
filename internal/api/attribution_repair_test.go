package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/attribution_provider.yaml
var attributionProviderYAML []byte

// This is ordinary source discovery and real SQLite persistence, not a forced
// reindex or a resolver-only assertion. The child is absent from discovery.
func TestOrdinaryAttributionRepairPreservesStoredSession(t *testing.T) {
	var fixture struct {
		Name    string `yaml:"name"`
		Initial string `yaml:"initial"`
	}
	if err := yaml.Unmarshal(activePublicationYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Name != "active upstream snapshot explicit publication convergence" || fixture.Initial == "" {
		t.Fatal("required source fixture missing")
	}
	home := t.TempDir()
	t.Setenv(defaults.EnvXDGConfigHome.String(), filepath.Join(home, "config"))
	t.Setenv(defaults.EnvXDGDataHome.String(), filepath.Join(home, "data"))
	t.Setenv(defaults.EnvXDGStateHome.String(), filepath.Join(home, "state"))
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "--quiet", "--initial-branch=main")
	git("-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.com", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "initial")
	const archive = "https://github.com/example/archive.git"
	const upstream = "https://github.com/example/canonical.git"
	git("remote", "add", "origin", archive)
	git("remote", "add", "canonical", upstream)
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(home, "source", "project")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	data := strings.NewReplacer("SESSION", sid.String(), "CWD", string(cwd)).Replace(fixture.Initial)
	if err := os.WriteFile(filepath.Join(source, sid.String()+".jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(string(defaults.ResolveDBFilePath())), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(string(defaults.ResolveDBFilePath()))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := config.BaseConfig()
	cfg.Output.BasePath = filepath.Join(home, "output")
	cfg.Sources = config.SourcesConfig{}
	cfg.Sources.ClaudeCode.Enabled = true
	cfg.Sources.ClaudeCode.Paths = []string{filepath.Dir(source)}
	handler := &syncHandler{store: db, config: cfg}
	ingestNow := func() {
		t.Helper()
		handler.runIngestPipeline(ingest.NewProgressState())
		if handler.ingestError != nil {
			t.Fatal(handler.ingestError)
		}
		if handler.ingestResult == nil || handler.ingestResult.Summary.Errors != 0 {
			t.Fatalf("ingest failed: %+v", handler.ingestResult)
		}
	}
	ingestNow()
	rows, err := db.AllPushableSessions(t.Context())
	if err != nil || len(rows) != 1 || rows[0].GitRemote != archive {
		t.Fatalf("initial rows=%+v err=%v", rows, err)
	}
	oldHost, _, err := db.LookupSessionLocation(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	oldDir := ingest.SessionDir(cfg.Output.BasePath, oldHost, sid.String(), "")
	metaPath := filepath.Join(oldDir, sid.String()+defaults.MetadataSuffix)
	rawMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		t.Fatal(err)
	}
	entries, err := db.ListEntries(t.Context(), sid)
	if err != nil || len(entries) == 0 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	metrics, err := db.GetMetrics(t.Context(), sid)
	if err != nil || metrics == nil {
		t.Fatalf("metrics=%+v err=%v", metrics, err)
	}
	annotator, err := db.GetAnnotatorIDByName(t.Context(), "outcome-classifier")
	if err != nil {
		t.Fatal(err)
	}
	annotationType, err := db.GetAnnotationTypeID(t.Context(), testutil.TestTypeIDSessionOutcome)
	if err != nil {
		t.Fatal(err)
	}
	id := sid.String()
	if _, err := db.CreateAnnotation(t.Context(), store.CreateAnnotationParams{SessionID: &id, AnnotatorID: annotator, AnnotationTypeID: annotationType, Value: "resolved"}); err != nil {
		t.Fatal(err)
	}
	annotations, err := db.GetAnnotationsForSession(t.Context(), id)
	if err != nil || len(annotations) == 0 {
		t.Fatalf("annotations=%+v err=%v", annotations, err)
	}
	if err := db.UpsertSessionCommits(t.Context(), sid, []ingest.CommitInfo{{Hash: strings.Repeat("a", 40), Message: "retained history"}}); err != nil {
		t.Fatal(err)
	}
	commits, err := db.ListCurrentSessionCommitAssociations(t.Context(), sid)
	if err != nil || len(commits) == 0 {
		t.Fatalf("commits=%+v err=%v", commits, err)
	}
	associationID := commits[0].ID
	if _, err := db.CreateAnnotation(t.Context(), store.CreateAnnotationParams{AssociationID: &associationID, AnnotatorID: annotator, AnnotationTypeID: annotationType, Value: "resolved"}); err != nil {
		t.Fatal(err)
	}
	associationAnnotations, err := db.GetAssociationAnnotationsForSession(t.Context(), id)
	if err != nil || len(associationAnnotations) == 0 {
		t.Fatalf("durable association annotation=%+v err=%v", associationAnnotations, err)
	}
	childID, err := ingest.NewSessionID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	childMeta := meta
	childMeta.SessionID, childMeta.ParentUUID = childID, &sid
	childDir := ingest.SessionDir(cfg.Output.BasePath, oldHost, childID.String(), sid.String())
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatal(err)
	}
	childRaw, err := json.Marshal(childMeta)
	if err != nil {
		t.Fatal(err)
	}
	childMetaPath := filepath.Join(childDir, childID.String()+defaults.MetadataSuffix)
	childTranscript := filepath.Join(childDir, childID.String()+"--transcript.jsonl")
	if err := os.WriteFile(childMetaPath, childRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childTranscript, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: &childMeta}}); err != nil {
		t.Fatal(err)
	}
	// Seed a real authoritative receipt through the normal explicit publish path.
	var calls atomic.Int32
	captured := &syncCapturedPublish{parts: map[string]string{}}
	village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/transcripts/publish") {
			_ = json.NewEncoder(w).Encode(schema.SchemaVersionResponse{ContentCapabilities: []schema.ContentCapability{schema.ContentCapabilityObservedModelV1}})
			return
		}
		n := calls.Add(1)
		captured.record(r)
		receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), n == 1)
		if err != nil {
			t.Errorf("receipt: %v", err)
			http.Error(w, "receipt failed", 500)
			return
		}
		if n == 1 {
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = w.Write(receipt)
	}))
	defer village.Close()
	writeSyncDoorCredentials(t, village.URL)
	publish := func() pushResponse {
		t.Helper()
		response := httptest.NewRecorder()
		handler.handleSyncPush(response, httptest.NewRequest("POST", "/api/v1/sync/push", strings.NewReader(`{"sessionIds":["`+id+`"],"visibility":"private"}`)))
		var result pushResponse
		if response.Code != http.StatusOK {
			t.Fatalf("publish: %d %s", response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Errors != 0 || len(result.Sessions) != 1 || result.Sessions[0].SessionID != id {
			t.Fatalf("selected publication=%+v", result)
		}
		return result
	}
	if first := publish(); first.New != 1 || calls.Load() != 1 {
		t.Fatalf("initial publication=%+v calls=%d", first, calls.Load())
	}
	receipt, err := db.Publication(t.Context(), village.URL, "user-1", meta.Project.Hash, id)
	if err != nil || receipt == nil {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	git("config", "branch.main.remote", "canonical")
	git("config", "branch.main.merge", "refs/heads/main")
	newDir := ingest.SessionDir(cfg.Output.BasePath, "github.com--example--canonical", id, "")
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(newDir, "unrelated.txt")
	if err := os.WriteFile(sentinel, []byte("retain destination member"), 0o600); err != nil {
		t.Fatal(err)
	}
	ingestNow()
	rows, err = db.AllPushableSessions(t.Context())
	if len(rows) == 2 && rows[0].SessionID != id {
		rows[0], rows[1] = rows[1], rows[0]
	}
	if err != nil || len(rows) != 2 || rows[0].SessionID != id || rows[0].GitRemote != upstream || rows[0].ProjectHash == meta.Project.Hash.String() {
		t.Fatalf("repaired rows=%+v err=%v", rows, err)
	}
	newHost, _, err := db.LookupSessionLocation(t.Context(), sid)
	if err != nil || newHost == oldHost {
		t.Fatalf("host=%s err=%v", newHost, err)
	}
	repairedDir := ingest.SessionDir(cfg.Output.BasePath, newHost, id, "")
	if repairedDir != newDir {
		t.Fatalf("repaired path=%s want %s", repairedDir, newDir)
	}
	if _, err := os.Stat(filepath.Join(repairedDir, id+defaults.MetadataSuffix)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repairedDir, id+"--transcript.jsonl")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "retain destination member" {
		t.Fatalf("destination member=%q err=%v", got, err)
	}
	childHost, childParent, err := db.LookupSessionLocation(t.Context(), childID)
	if err != nil || childHost != oldHost || childParent != id {
		t.Fatalf("child location=%s/%s err=%v", childHost, childParent, err)
	}
	if got, err := os.ReadFile(childMetaPath); err != nil || string(got) != string(childRaw) {
		t.Fatalf("child metadata lost: %v", err)
	}
	if got, err := os.ReadFile(childTranscript); err != nil || string(got) != data {
		t.Fatalf("child transcript lost: %v", err)
	}
	if got, err := db.ListEntries(t.Context(), sid); err != nil || !reflect.DeepEqual(got, entries) {
		t.Fatalf("entries changed: %v", err)
	}
	if got, err := db.GetAnnotationsForSession(t.Context(), id); err != nil || !reflect.DeepEqual(got, annotations) {
		t.Fatalf("annotations changed: %v", err)
	}
	if got, err := db.GetMetrics(t.Context(), sid); err != nil || got == nil {
		t.Fatalf("metrics lost: %v", err)
	} else {
		// Recompute audit time is not a session statistic.
		got.ComputedAt, metrics.ComputedAt = nil, nil
		if !reflect.DeepEqual(got, metrics) {
			t.Fatalf("metrics changed: got=%+v want=%+v", got, metrics)
		}
	}
	// Current temporal detection may change; the durable association and its
	// annotation must still resolve through the production ledger-backed reader.
	if got, err := db.GetAssociationAnnotationsForSession(t.Context(), id); err != nil || !reflect.DeepEqual(got, associationAnnotations) {
		t.Fatalf("durable association changed: got=%+v want=%+v err=%v", got, associationAnnotations, err)
	}
	if got, err := db.Publication(t.Context(), village.URL, "user-1", meta.Project.Hash, id); err != nil || !reflect.DeepEqual(got, receipt) {
		t.Fatalf("receipt changed: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("ordinary repair published without explicit action")
	}
	project, err := schema.NewProjectHash(rows[0].ProjectHash)
	if err != nil {
		t.Fatal(err)
	}
	if update := publish(); update.Updated != 1 || calls.Load() != 2 {
		t.Fatalf("repair publication=%+v calls=%d", update, calls.Load())
	}
	updatedReceipt, err := db.Publication(t.Context(), village.URL, "user-1", project, id)
	if err != nil || updatedReceipt == nil || updatedReceipt.Receipt.TranscriptID != receipt.Receipt.TranscriptID {
		t.Fatalf("relocated receipt=%+v err=%v", updatedReceipt, err)
	}
	if oldReceipt, err := db.Publication(t.Context(), village.URL, "user-1", meta.Project.Hash, id); err != nil || oldReceipt != nil {
		t.Fatalf("old project receipt remains=%+v err=%v", oldReceipt, err)
	}
	if !strings.Contains(captured.snapshot()["metadata"], project.String()) {
		t.Fatal("repaired project identity did not reach publisher")
	}
	before, err := db.SessionByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ingestNow()
	if got, err := db.SessionByID(t.Context(), id); err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("unchanged rerun changed stored session: %v", err)
	}
	if unchanged := publish(); unchanged.Skipped != 1 || calls.Load() != 2 {
		t.Fatalf("unchanged publication=%+v calls=%d", unchanged, calls.Load())
	}
	if got, err := db.Publication(t.Context(), village.URL, "user-1", project, id); err != nil || !reflect.DeepEqual(got, updatedReceipt) {
		t.Fatalf("unchanged publication mutated receipt: %v", err)
	}
	// Losing every usable remote must also converge to the adapter's path identity.
	git("remote", "remove", "origin")
	git("remote", "remove", "canonical")
	ingestNow()
	rows, err = db.AllPushableSessions(t.Context())
	if len(rows) == 2 && rows[0].SessionID != id {
		rows[0], rows[1] = rows[1], rows[0]
	}
	if err != nil || len(rows) != 2 || rows[0].SessionID != id || rows[0].GitRemote != "" {
		t.Fatalf("path fallback rows=%+v err=%v", rows, err)
	}
	pathHost, _, err := db.LookupSessionLocation(t.Context(), sid)
	if err != nil || pathHost == newHost {
		t.Fatalf("path fallback host=%s err=%v", pathHost, err)
	}
	before, err = db.SessionByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ingestNow()
	if got, err := db.SessionByID(t.Context(), id); err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("path identity did not converge: %v", err)
	}
	// Provider-supplied remote remains authoritative despite a different checkout
	// upstream, including the ordinary unchanged-source decision.
	if err := yaml.Unmarshal(attributionProviderYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Name != "explicit provider identity converges independently of checkout" || fixture.Initial == "" {
		t.Fatal("required provider fixture missing")
	}
	git("remote", "add", "canonical", upstream)
	git("config", "branch.main.remote", "canonical")
	git("config", "branch.main.merge", "refs/heads/main")
	providerRoot := filepath.Join(home, defaults.HarnessCodex.String())
	providerDir := filepath.Join(providerRoot, "2026", "09", "07")
	if err := os.MkdirAll(providerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const providerID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if err := os.WriteFile(filepath.Join(providerDir, "rollout-2026-09-07T10-00-00-"+providerID+".jsonl"), []byte(strings.ReplaceAll(fixture.Initial, "CWD", string(cwd))), 0o600); err != nil {
		t.Fatal(err)
	}
	providerSource, err := ingest.NewResolvedPath(providerRoot)
	if err != nil {
		t.Fatal(err)
	}
	output, err := ingest.NewResolvedPath(cfg.Output.BasePath)
	if err != nil {
		t.Fatal(err)
	}
	ingestProvider := func() {
		t.Helper()
		fs := &ingest.OSFileSystem{}
		pipeline, err := ingest.NewPipeline(fs, &ingest.ExecGitResolver{}, ingest.DefaultAdapterRegistry, ingest.PipelineConfig{
			OutputDir: output,
			Sources:   map[defaults.Harness]ingest.SourceConfig{defaults.HarnessCodex: {Enabled: true, Paths: []ingest.ResolvedPath{providerSource}}},
		}, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(map[defaults.Harness]ingest.TranscriptIndexer{defaults.HarnessCodex: ingest.NewCodexIndexer(fs)}))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(t.Context())
		if err != nil || result.Summary.Errors != 0 {
			t.Fatalf("provider ingest=%+v err=%v", result, err)
		}
	}
	ingestProvider()
	providerBefore, err := db.SessionByID(t.Context(), providerID)
	if err != nil || providerBefore == nil {
		t.Fatalf("provider missing: %v", err)
	}
	providerSID, err := ingest.NewSessionID(providerID)
	if err != nil {
		t.Fatal(err)
	}
	providerHost, _, err := db.LookupSessionLocation(t.Context(), providerSID)
	if err != nil || providerHost != "github.com--example--provider" {
		t.Fatalf("provider identity=%s err=%v", providerHost, err)
	}
	ingestProvider()
	if got, err := db.SessionByID(t.Context(), providerID); err != nil || !reflect.DeepEqual(got, providerBefore) {
		t.Fatalf("explicit provider remote did not converge: %v", err)
	}
}
