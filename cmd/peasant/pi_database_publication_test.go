package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/pi_database_publication.yaml
var piDatabasePublicationYAML []byte

func TestPiDatabasePublicationThroughCLI(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		SessionID     string   `yaml:"sessionID"`
		Prefix        string   `yaml:"prefix"`
		Repetitions   int      `yaml:"repetitions"`
		Tail          string   `yaml:"tail"`
		Secret        string   `yaml:"secret"`
		PII           string   `yaml:"pii"`
		Source        string   `yaml:"source"`
		Cases         []struct {
			Name           string `yaml:"name"`
			IncompleteTail bool   `yaml:"incompleteTail"`
			CorruptChunk   bool   `yaml:"corruptChunk"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(piDatabasePublicationYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "Pi database CLI publication"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			text := strings.Repeat(fixture.Prefix, fixture.Repetitions) + fixture.Tail + " " + fixture.Secret + " " + fixture.PII
			encoded, err := json.Marshal(text)
			if err != nil {
				t.Fatal(err)
			}
			data := strings.ReplaceAll(fixture.Source, "TEXT", string(encoded))
			if c.IncompleteTail {
				data += `{"type":"message"`
			}
			source, retained := filepath.Join(root, "native.jsonl"), filepath.Join(root, "retained")
			if err := os.WriteFile(source, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			output, err := executeHarvestCmd(t, root, []string{"--source-provider", "pi", "--source-path", source, "--output", retained, "--json"})
			if err != nil {
				t.Fatalf("native harvest: %v %s", err, output)
			}
			db, err := store.Open(string(defaults.ResolveDBFilePathWith(root)))
			if err != nil {
				t.Fatal(err)
			}
			sid, err := ingest.NewSessionID(fixture.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			entries, capture, err := db.LoadFullSessionEntries(t.Context(), sid, 0)
			if err != nil || capture.Status != ingest.ContentCaptureComplete || len(entries) == 0 {
				t.Fatalf("native full capture: %+v %v", capture, err)
			}
			if c.CorruptChunk {
				conn, err := db.Pool().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.ExecuteTransient(conn, `UPDATE session_entry_full_content_chunks SET data=zeroblob(byte_length) WHERE session_id=? AND chunk_index=0`, &sqlitex.ExecOptions{Args: []any{fixture.SessionID}})
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(source); err != nil {
				t.Fatal(err)
			}
			// These directories contain only this test's synthetic native artifacts.
			if err := os.RemoveAll(retained); err != nil {
				t.Fatal(err)
			}
			captured := &capturedPublish{parts: map[string]string{}}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if !strings.Contains(r.URL.Path, "/transcripts/publish") {
					_ = json.NewEncoder(w).Encode(schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: schema.AllContentCapabilities})
					return
				}
				captured.record(t, r)
				receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), true)
				if err != nil {
					t.Error(err)
					http.Error(w, "fixture receipt failed", 500)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(receipt)
			}))
			defer peer.Close()
			writeTestCredentialsFor(t, root, peer.URL)
			cfg := config.BaseConfig()
			cfg.Output.BasePath = retained
			cfgPath := filepath.Join(root, "config.yaml")
			if err := config.SaveAtomic(cfgPath, cfg); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := executePushCmdSeparate(t, root, []string{"--non-interactive", "--json", "--config=" + cfgPath})
			parts := captured.snapshot()
			if c.CorruptChunk {
				if len(parts) != 0 || !strings.Contains(stdout+stderr, "harvest index --force") {
					t.Fatalf("corrupt capture escaped refusal: %v %s %s", err, stdout, stderr)
				}
				return
			}
			if err != nil || len(parts) != 2 {
				t.Fatalf("CLI publish: %v parts=%d %s %s", err, len(parts), stdout, stderr)
			}
			for name, part := range parts {
				if !strings.Contains(part, fixture.Tail) || !strings.Contains(part, "ANTHROPIC_KEY") || strings.Contains(part, fixture.Secret) || strings.Contains(part, fixture.PII) || strings.Contains(part, `"extra"`) || strings.Contains(part, "pi.carrier") {
					t.Fatalf("multipart %s lost full redacted tail or exposed private data", name)
				}
			}
			content, err := schema.DecodeTranscriptContentRaw([]byte(parts["transcript_file"]))
			if err != nil {
				t.Fatal(err)
			}
			if len(content.SessionDetail.NativeMetadata) != 2 {
				t.Fatal("native metadata lost")
			}
			tool := content.SessionDetail.Turns[1].ToolCalls[0]
			if tool.Namespace == nil || *tool.Namespace != "Synthetic.世界" || tool.Name != "custom_tool" || tool.Usage == nil || tool.Usage.Completeness != schema.UsageUnknown {
				t.Fatal("namespace/tool attribution lost")
			}
		})
	}
}
