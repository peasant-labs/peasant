package api

import (
	"bytes"
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

//go:embed testdata/active_publication.yaml
var activePublicationYAML []byte

func TestActiveSnapshotSharePublicationConverges(t *testing.T) {
	var fixture struct {
		Name    string `yaml:"name"`
		Initial string `yaml:"initial"`
		Append  string `yaml:"append"`
	}
	if err := yaml.Unmarshal(activePublicationYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Name != "active upstream snapshot explicit publication convergence" || fixture.Initial == "" || fixture.Append == "" {
		t.Fatal("required active publication fixture is missing")
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
			t.Fatalf("git: %v: %s", err, out)
		}
	}
	git("init", "--quiet", "--initial-branch=main")
	git("-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.com", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "synthetic initial history")
	git("remote", "add", "origin", "https://github.com/example/fork.git")
	const upstream = "https://github.com/example/canonical.git"
	git("remote", "add", "canonical", upstream)
	git("config", "branch.main.remote", "canonical")
	git("config", "branch.main.merge", "refs/heads/main")
	const selectedID = testutil.TestSessionUUID
	const sentinelID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	source := filepath.Join(home, "source", "project")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeSource := func(id, content string) {
		t.Helper()
		data := strings.NewReplacer("SESSION", id, "CWD", string(cwd)).Replace(content)
		if err := os.WriteFile(filepath.Join(source, id+".jsonl"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSource(selectedID, fixture.Initial)
	writeSource(sentinelID, fixture.Initial)
	var calls atomic.Int32
	captured := &syncCapturedPublish{parts: map[string]string{}}
	village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/transcripts/publish") {
			_ = json.NewEncoder(w).Encode(schema.SchemaVersionResponse{ContentCapabilities: []schema.ContentCapability{schema.ContentCapabilityObservedModelV1}})
			return
		}
		captured.record(r)
		n := calls.Add(1)
		receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), n == 1)
		if err != nil {
			t.Errorf("receipt: %v", err)
			http.Error(w, err.Error(), 500)
			return
		}
		if n == 1 {
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = w.Write(receipt)
	}))
	defer village.Close()
	writeSyncDoorCredentials(t, village.URL)
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
		if handler.ingestResult == nil {
			t.Fatal("missing ingest result")
		}
	}
	ingestNow()
	rows, err := db.AllPushableSessions(t.Context())
	if err != nil || len(rows) != 2 {
		t.Fatalf("stored active rows=%+v err=%v", rows, err)
	}
	var selected ingest.PushSessionRow
	for _, row := range rows {
		if row.SessionID == selectedID {
			selected = row
		}
	}
	if selected.GitRemote != upstream {
		t.Fatalf("stored upstream=%q want %q", selected.GitRemote, upstream)
	}
	sentinel, _ := ingest.NewSessionID(sentinelID)
	beforeSentinel, err := db.ListEntries(t.Context(), sentinel)
	if err != nil || len(beforeSentinel) == 0 {
		t.Fatalf("sentinel entries=%+v err=%v", beforeSentinel, err)
	}
	if calls.Load() != 0 {
		t.Fatal("ingest published without explicit action")
	}
	publish := func() pushResponse {
		t.Helper()
		restoreSource := prepareSourceFreePublication(t, db, cfg.Output.BasePath, filepath.Join(source, selectedID+".jsonl"), selectedID, repo)
		defer restoreSource()
		body, _ := json.Marshal(pushRequest{SessionIDs: []string{selectedID}, Visibility: "private"})
		response := httptest.NewRecorder()
		handler.handleSyncPush(response, httptest.NewRequest("POST", "/api/v1/sync/push", bytes.NewReader(body)))
		var result pushResponse
		if response.Code != http.StatusOK {
			t.Fatalf("share status=%d body=%s", response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Errors != 0 || len(result.Sessions) != 1 || result.Sessions[0].SessionID != selectedID {
			t.Fatalf("selected share result=%+v", result)
		}
		return result
	}
	if first := publish(); first.New != 1 || calls.Load() != 1 {
		t.Fatalf("first publication=%+v calls=%d", first, calls.Load())
	}
	project, err := schema.NewProjectHash(selected.ProjectHash)
	if err != nil {
		t.Fatal(err)
	}
	readReceipt := func() *store.PublicationRecord {
		t.Helper()
		record, err := db.Publication(t.Context(), village.URL, "user-1", project, selectedID)
		if err != nil || record == nil {
			t.Fatalf("retained receipt=%+v err=%v", record, err)
		}
		return record
	}
	firstReceipt := readReceipt()
	writeSource(selectedID, fixture.Initial+fixture.Append)
	ingestNow()
	if calls.Load() != 1 || !reflect.DeepEqual(firstReceipt, readReceipt()) {
		t.Fatal("ordinary ingest changed the publication receipt or sent a request")
	}
	if update := publish(); update.Updated != 1 || calls.Load() != 2 {
		t.Fatalf("repeat publication=%+v calls=%d", update, calls.Load())
	}
	updatedReceipt := readReceipt()
	if updatedReceipt.Receipt.TranscriptID != firstReceipt.Receipt.TranscriptID || updatedReceipt.Receipt.ContentHash == firstReceipt.Receipt.ContentHash {
		t.Fatal("repeat publication must update the same remote transcript with changed content")
	}
	parts := captured.snapshot()
	if !strings.Contains(parts["metadata"], selectedID) || strings.Contains(parts["metadata"], sentinelID) {
		t.Fatalf("wrong selected identity: %s", parts["metadata"])
	}
	foundLater := false
	for _, part := range parts {
		foundLater = foundLater || strings.Contains(part, "synthetic later answer")
	}
	if !foundLater {
		t.Fatal("updated stored content did not reach Village")
	}
	beforeNoop, err := db.AllPushableSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ingestNow()
	afterNoop, err := db.AllPushableSessions(t.Context())
	if err != nil || !reflect.DeepEqual(beforeNoop, afterNoop) {
		t.Fatalf("unchanged ingest mutated stored metadata: before=%+v after=%+v err=%v", beforeNoop, afterNoop, err)
	}
	if unchanged := publish(); unchanged.Skipped != 1 || calls.Load() != 2 {
		t.Fatalf("unchanged publication=%+v calls=%d", unchanged, calls.Load())
	}
	if !reflect.DeepEqual(updatedReceipt, readReceipt()) {
		t.Fatal("unchanged run mutated receipt")
	}
	afterSentinel, err := db.ListEntries(t.Context(), sentinel)
	if err != nil || !reflect.DeepEqual(beforeSentinel, afterSentinel) {
		t.Fatal("unselected stored snapshot changed")
	}
	rows, err = db.AllPushableSessions(t.Context())
	if err != nil || len(rows) != 2 {
		t.Fatalf("stored sessions changed: %+v %v", rows, err)
	}
	for _, row := range rows {
		if row.SessionID == sentinelID && row.PushedAt != nil {
			t.Fatal("unselected sentinel was published")
		}
	}
}

// Exercise the mounted publisher from a captured active/re-attributed session,
// with neither original transcript nor generated metadata available to it.
func prepareSourceFreePublication(t *testing.T, db *store.Store, output, source, rawID, cwd string) func() {
	t.Helper()
	id, err := ingest.NewSessionID(rawID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := db.LoadPublicationInput(t.Context(), id)
	if err != nil || bundle.Readiness != ingest.PublicationReady || bundle.Metadata.CWD != cwd || bundle.Metadata.Project.Hash != bundle.ReceiptProjectHash || len(bundle.Entries) == 0 {
		t.Fatalf("publication capture is not coherent: %+v, %v", bundle, err)
	}
	host, parent, err := db.LookupSessionLocation(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	metadata := ingest.SessionMetadataPath(output, host, rawID, parent)
	if err := os.Remove(metadata); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Rename(source, source+".held"); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Rename(source+".held", source); err != nil {
			t.Fatal(err)
		}
	}
}
