package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// syncDoorSecret is planted in an indexed entry, the field the outward redaction
// exists to protect.
const syncDoorSecret = "sk-ant-api03-SYNCDOORKEY00000000000x"

// TestHandleSyncPush_TheShareDoorGivesThePipelineARedactor pins the second
// production push door — the one the /share wizard drives.
//
// There are two doors into push.NewPipeline and neither had its redactor
// argument pinned. Measured on this tree before this test existed: replacing the
// redactor here with a nil ingest.TextRedactor left all 37 packages green, and
// every push through the web wizard would have published as recorded.
//
// It drives the real handler against a village that captures the multipart
// request, and sweeps EVERY part rather than the one called "metadata", because
// the transcript text leaves in more than one of them.
func TestHandleSyncPush_TheShareDoorGivesThePipelineARedactor(t *testing.T) {
	captured := &syncCapturedPublish{parts: map[string]string{}}
	village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/transcripts/publish") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		captured.record(r)
		receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), true)
		if err != nil {
			t.Errorf("build authoritative receipt: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(receipt)
	}))
	t.Cleanup(village.Close)

	home := t.TempDir()
	t.Setenv(defaults.EnvXDGConfigHome.String(), filepath.Join(home, "config"))
	t.Setenv(defaults.EnvXDGDataHome.String(), filepath.Join(home, "data"))
	t.Setenv(defaults.EnvXDGStateHome.String(), filepath.Join(home, "state"))
	writeSyncDoorCredentials(t, village.URL)

	const sessionID = "eeee5555-eeee-4eee-8eee-eeeeeeeeeeee"
	basePath := filepath.Join(home, "peasant-sync")
	db := seedSyncDoorSession(t, sessionID, basePath)
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.BaseConfig()
	cfg.Output.BasePath = basePath
	handler := &syncHandler{store: db, config: cfg}

	body, err := json.Marshal(pushRequest{SessionIDs: []string{sessionID}, Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/api/v1/sync/push", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.handleSyncPush(response, request)

	parts := captured.snapshot()
	if len(parts) == 0 {
		t.Fatalf("the village received no publish, so this test cannot say anything about what the door sends. "+
			"status=%d body=%s", response.Code, response.Body.String())
	}
	sawContent := false
	for name, part := range parts {
		// Non-vacuity keyed on the REDACTED FORM OF THE PLANT. The previous marker
		// ("sessionDetail") is in every publish, so this passed over a body with
		// no entries in it - which pipeline.go produces whenever ListEntries
		// errors. The placeholder appears only if the planted content reached the
		// wire and was redacted.
		if strings.Contains(part, "ANTHROPIC_KEY") {
			sawContent = true
		}
		if strings.Contains(part, syncDoorSecret) {
			t.Errorf("the %q part of the real publish carries the planted secret VERBATIM. The /share door built its "+
				"pipeline without a redactor, so a wizard push publishes as recorded while the wizard's own copy says "+
				"content is redacted.\n%s:\n%s", name, name, part)
		}
	}
	if !sawContent {
		t.Errorf("no captured part carries a redacted placeholder, so the planted content never reached the wire and the " +
			"sweep above ran over a body that could not leak")
	}
}

type syncCapturedPublish struct {
	mu    sync.Mutex
	parts map[string]string
}

func (c *syncCapturedPublish) record(r *http.Request) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		data, _ := io.ReadAll(r.Body)
		c.parts["body"] = string(data)
		return
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, partErr := reader.NextPart()
		if partErr != nil {
			return
		}
		data, _ := io.ReadAll(part)
		name := part.FormName()
		if name == "" {
			name = part.FileName()
		}
		c.parts[name] = string(data)
	}
}

func (c *syncCapturedPublish) snapshot() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.parts))
	for k, v := range c.parts {
		out[k] = v
	}
	return out
}

func writeSyncDoorCredentials(t *testing.T, villageURL string) {
	t.Helper()
	dir := string(defaults.ResolveConfigDirPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	creds := map[string]any{
		"village_url": villageURL,
		"api_key":     "test-key",
		"username":    "tester",
		"user_id":     "user-1",
		"key_id":      "key-1",
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, string(defaults.CredentialsFile)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedSyncDoorSession(t *testing.T, sessionID, basePath string) *store.Store {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePath())
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const startMs int64 = 1700000000000
	ingested := startMs + 120000
	remote, branch := "git@github.com:user/repo.git", "main"
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = schema.SessionID(sessionID)
	meta.HostSlug = schema.HostSlug("github.com-user-repo")
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = schema.ModelID("claude-opus-4-6")
	meta.Timestamp = ingest.TimestampInfo{Start: startMs, End: startMs + 60000, Ingested: &ingested}
	meta.Source = ingest.SourceInfo{FilePath: "/test/path/" + sessionID + ".jsonl", Format: ingest.SourceFormatJSONL}
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "myapp", FilePath: "/home/test/myapp"}
	meta.Stats = ingest.StatsInfo{TurnCount: 5, ToolCallCount: 3, DurationMs: 60000, TokensIn: 100, TokensOut: 50}
	meta.Git = ingest.GitContext{Remote: &remote, Branch: &branch}
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: &meta}}); err != nil {
		t.Fatal(err)
	}
	preview := "here is the key " + syncDoorSecret + " thanks"
	if err := db.IndexSessionEntries(t.Context(), ingest.SessionID(sessionID), []schema.SessionEntry{{
		SessionID:      schema.SessionID(sessionID),
		EntryIndex:     1,
		Depth:          0,
		Role:           schema.RoleUser,
		Harness:        schema.Harness(defaults.HarnessClaudeCode),
		EntryType:      schema.EntryTypeText,
		ContentPreview: &preview,
	}}); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(basePath, "github.com-user-repo", sessionID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, sessionID+"--metadata.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return db
}
