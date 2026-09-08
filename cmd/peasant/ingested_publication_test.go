package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/ingested_publication.yaml
var ingestedPublicationYAML []byte

// This is the joined production path: the actual harvest command selects the
// real adapter, indexer and metrics engine. Only the external Village is fake.
func TestIngestedPublicationThroughCLIAndRegisteredShare(t *testing.T) {
	var cases []struct {
		Name    string `yaml:"name"`
		Legacy  bool   `yaml:"legacy"`
		Reindex bool   `yaml:"reindex"`
		CWD     string `yaml:"cwd"`
	}
	if err := yaml.Unmarshal(ingestedPublicationYAML, &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		seen[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("ingested publication", "case", strings.Fields("fresh-source-capture unchanged-legacy-source-recovery source-confirmed-cwd-absence source-reindex-publication"), seen); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv(defaults.EnvXDGConfigHome.String(), filepath.Join(root, "config"))
			t.Setenv(defaults.EnvXDGDataHome.String(), filepath.Join(root, "data"))
			t.Setenv(defaults.EnvXDGStateHome.String(), filepath.Join(root, "state"))
			dir := filepath.Join(root, "config")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			const id = "77777777-7777-4777-8777-777777777777"
			const model = "claude-sonnet-4-20250514"
			const secret = "sk-ant-api03-INTEGRATEDPUBLICATIONKEY000000x"
			sourceRoot := filepath.Join(root, "source")
			sourcePath := filepath.Join(sourceRoot, "project", id+".jsonl")
			if err := os.MkdirAll(filepath.Dir(sourcePath), 0700); err != nil {
				t.Fatal(err)
			}
			cwdField := ""
			if tc.CWD != "" {
				raw, _ := json.Marshal(tc.CWD)
				cwdField = `,"cwd":` + string(raw)
			}
			source := []byte(fmt.Sprintf("{\"type\":\"user\",\"sessionId\":%q%s,\"timestamp\":\"2024-01-02T03:12:00Z\",\"message\":{\"role\":\"user\",\"content\":%q}}\n{\"type\":\"assistant\",\"sessionId\":%q,\"timestamp\":\"2024-01-02T03:12:01Z\",\"message\":{\"role\":\"assistant\",\"model\":%q,\"content\":[{\"type\":\"text\",\"text\":\"synthetic response\"}]}}\n", id, cwdField, "synthetic request "+secret, id, model))
			if err := os.WriteFile(sourcePath, source, 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "output")
			harvest := func() {
				text, err := executeHarvestCmd(t, dir, []string{"--source-harness", "claude-code", "--source-path", sourceRoot, "--output", output, "--include-active"})
				if err != nil {
					t.Fatalf("canonical ingest: %v\n%s", err, text)
				}
			}
			harvest()
			dbPath := string(defaults.ResolveDBFilePathWith(dir))
			open := func() *store.Store {
				db, err := store.Open(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				return db
			}
			db := open()
			input, err := db.LoadPublicationInput(t.Context(), id)
			if err != nil || input.Readiness != ingest.PublicationReady || len(input.Entries) == 0 || input.Quality == nil {
				t.Fatalf("actual ingest did not certify publication: %+v %v", input, err)
			}
			originalHash := input.ReceiptProjectHash
			originalHost := input.Metadata.HostSlug
			metadataPath := ingest.SessionMetadataPath(output, originalHost.String(), id, "")
			if tc.Reindex {
				priorRevision := input.CaptureRevision
				db.Close()
				source = bytes.ReplaceAll(source, []byte("synthetic response"), []byte("fresh reindexed response"))
				if err := os.WriteFile(sourcePath, source, 0600); err != nil {
					t.Fatal(err)
				}
				reindexConfig := writeCfg(t, dir, "reindex.yaml", "version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths: ["+sourceRoot+"]\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\noutput:\n  basePath: "+output+"\n")
				text, err := executeHarvestCmd(t, dir, []string{"--config", reindexConfig, "index", "--force"})
				if err != nil {
					t.Fatalf("mounted reindex: %v\n%s", err, text)
				}
				db = open()
				input, err = db.LoadPublicationInput(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				entries, err := json.Marshal(input.Entries)
				if err != nil || input.CaptureRevision <= priorRevision || !bytes.Contains(entries, []byte("fresh reindexed response")) {
					t.Fatalf("reindex did not durably refresh the actual source: %+v, %v", input, err)
				}
			}
			if tc.Legacy {
				// Simulate a shipped legacy row, not a certified test capture. Normal
				// ingest must recover absent model/CWD and the entire missing snapshot.
				legacy := input.Metadata
				legacy.Model = ""
				legacy.CWD = ""
				if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: &legacy}}); err != nil {
					t.Fatal(err)
				}
				db.Close()
				conn, err := sqlite.OpenConn(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := sqlitex.ExecuteTransient(conn, "DELETE FROM session_publication_metadata WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{id}}); err != nil {
					t.Fatal(err)
				}
				conn.Close()
				db = open()
				held, err := db.LoadPublicationInput(t.Context(), id)
				if err != nil || held.Readiness != ingest.PublicationNeedsIngest {
					t.Fatalf("legacy unexpectedly publishable: %+v %v", held, err)
				}
				db.Close()
				if err := os.Remove(metadataPath); err != nil {
					t.Fatal(err)
				}
				harvest() // no --force: unchanged legacy source must recover normally
				db = open()
				input, err = db.LoadPublicationInput(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
			}
			if input.Readiness != ingest.PublicationReady || input.Metadata.Model != model || input.Metadata.CWD != tc.CWD || input.ReceiptProjectHash != originalHash || input.Metadata.HostSlug != originalHost {
				t.Fatalf("source facts or historical identity changed: %+v", input)
			}
			if input.Quality == nil || input.Quality.ComputeVersion == nil || *input.Quality.ComputeVersion == 0 {
				t.Fatal("actual metrics computation did not run")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = open() // publication sees only the durable reopened database
			defer db.Close()
			if err := os.Remove(metadataPath); err != nil {
				t.Fatal(err)
			}
			captured := &capturedPublish{parts: map[string]string{}}
			var publications atomic.Int32
			village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if !strings.Contains(r.URL.Path, "/transcripts/publish") {
					json.NewEncoder(w).Encode(schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: []schema.ContentCapability{schema.ContentCapabilityObservedModelV1}})
					return
				}
				captured.record(t, r)
				created := publications.Add(1) == 1
				receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), created)
				if err != nil {
					t.Error(err)
					http.Error(w, "invalid publish", 500)
					return
				}
				if created {
					w.WriteHeader(http.StatusCreated)
				}
				var terminal schema.AuthoritativePublishResponse
				if err := json.Unmarshal(receipt, &terminal); err != nil {
					t.Error(err)
					return
				}
				terminal.PublishedAt = time.Now().UnixMilli()
				terminal.UpdatedAt = terminal.PublishedAt
				json.NewEncoder(w).Encode(terminal)
			}))
			defer village.Close()
			writeTestCredentialsFor(t, dir, village.URL)
			cfgPath := writeCfg(t, dir, "publication.yaml", "version: 1\noutput:\n  basePath: "+output+"\npush:\n  method: all\n  visibility: private\n")
			out, stderr, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--non-interactive", "--quiet", "--config=" + cfgPath})
			if err != nil || !strings.Contains(out, "1 would push") || publications.Load() != 0 {
				t.Fatalf("dry-run: %v %s %s uploads=%d", err, out, stderr, publications.Load())
			}
			cfg := config.BaseConfig()
			cfg.Output.BasePath = output
			ctx, cancel := context.WithCancel(t.Context())
			server := api.NewServer(api.ServerConfig{Port: 0, Store: db, Config: cfg})
			if err := server.Listen(ctx); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			baseURL := "http://" + server.Addr().String()
			get := func(path string) []byte {
				resp, err := http.Get(baseURL + path)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				raw, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != 200 {
					t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
				}
				return raw
			}
			if !bytes.Contains(get(defaults.RouteSyncSessions.String()), []byte(`"syncStatus":"new"`)) {
				t.Fatal("ready capture held without sidecar")
			}
			if !bytes.Contains(get(defaults.RouteSyncRedactions.String()+"?session_id="+id), []byte(secret)) {
				t.Fatal("Share scan missed canonical indexed content")
			}
			out, stderr, err = executePushCmdSeparate(t, dir, []string{"--non-interactive", "--quiet", "--config=" + cfgPath})
			if err != nil || publications.Load() != 1 {
				t.Fatalf("CLI publish: %v %s %s uploads=%d", err, out, stderr, publications.Load())
			}
			first := captured.snapshot()
			firstReceipt, err := db.Publication(t.Context(), village.URL, "user-00001", originalHash, id)
			if err != nil || firstReceipt == nil || !firstReceipt.Receipt.Created {
				t.Fatalf("first receipt missing: %+v %v", firstReceipt, err)
			}
			for _, part := range []string{"metadata", "transcript_file"} {
				if strings.Contains(first[part], secret) || !strings.Contains(first[part], "ANTHROPIC_KEY") {
					t.Fatalf("%s did not redact actual source entry", part)
				}
			}
			statusBody := get(defaults.RouteSyncSessions.String())
			if !bytes.Contains(statusBody, []byte(`"syncStatus":"synced"`)) {
				t.Fatalf("publication receipt did not synchronize status: %s; CLI: %s %s", statusBody, out, stderr)
			}
			request := fmt.Sprintf(`{"sessionIds":[%q],"visibility":"private"}`, id)
			resp, err := http.Post(baseURL+defaults.RouteSyncPush.String(), "application/json", strings.NewReader(request))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || publications.Load() != 1 || !bytes.Contains(raw, []byte(`"skipped":1`)) {
				t.Fatalf("Share repeat: %d %s uploads=%d", resp.StatusCode, raw, publications.Load())
			}
			second := captured.snapshot()
			secondReceipt, err := db.Publication(t.Context(), village.URL, "user-00001", originalHash, id)
			if err != nil || !reflect.DeepEqual(secondReceipt, firstReceipt) {
				t.Fatalf("unchanged Share publication changed the authoritative receipt: first=%+v second=%+v err=%v", firstReceipt, secondReceipt, err)
			}
			if first["metadata"] != second["metadata"] || first["transcript_file"] != second["transcript_file"] {
				t.Fatal("same capture changed upstream identity/content across doors")
			}
			rows, err := db.AllPushableSessions(t.Context())
			if err != nil || len(rows) != 1 || rows[0].SessionID != id || rows[0].PushedAt == nil {
				t.Fatalf("publication duplicated/lost session: %+v %v", rows, err)
			}
			unchanged, err := os.ReadFile(sourcePath)
			if err != nil || !bytes.Equal(source, unchanged) {
				t.Fatal("original transcript source changed")
			}
			if _, err := os.Stat(metadataPath); !os.IsNotExist(err) {
				t.Fatal("publication regenerated metadata sidecar")
			}
		})
	}
}
