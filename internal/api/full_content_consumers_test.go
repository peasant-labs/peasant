package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/full_content_consumers.yaml
var fullConsumerYAML []byte

type fullConsumerFixture struct {
	Name, Damage, Prefix, Tail, Secret, PII string
	Harness, Source                         string
	NativeSource, SessionID                 string
	ToolFile                                string
	Backfill                                bool
	StructuredOutput                        bool
	Roles                                   []schema.Role
	Repetitions                             int
}

func loadFullConsumerFixtures(t *testing.T) []fullConsumerFixture {
	t.Helper()
	var fixture struct {
		RequiredNames []string              `yaml:"requiredNames"`
		Cases         []fullConsumerFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(fullConsumerYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] || c.Repetitions <= 0 || c.Tail == "" || c.Secret == "" {
			t.Fatal("invalid full consumer fixture")
		}
		seen[c.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
	return fixture.Cases
}

// Filesystem reads are forbidden, not merely missing. Both conversation content
// and publication metadata must survive in the database independently.
type deniedTranscriptFS struct {
	*testutil.MemFS
	reads int
}

var _ ingest.FileSystem = (*deniedTranscriptFS)(nil)

func (f *deniedTranscriptFS) ReadFile(string) ([]byte, error) {
	f.reads++
	return nil, fs.ErrPermission
}

func TestFullContentConsumersDatabaseAuthority(t *testing.T) {
	for _, fixture := range loadFullConsumerFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv(defaults.EnvXDGConfigHome.String(), filepath.Join(home, "config"))
			t.Setenv(defaults.EnvXDGDataHome.String(), filepath.Join(home, "data"))
			t.Setenv(defaults.EnvXDGStateHome.String(), filepath.Join(home, "state"))
			id := "eeee5555-eeee-4eee-8eee-eeeeeeeeeeee"
			if fixture.SessionID != "" {
				id = fixture.SessionID
			}
			sid := ingest.SessionID(id)
			basePath := filepath.Join(home, "retained")
			text := strings.Repeat(fixture.Prefix, fixture.Repetitions) + fixture.Tail + " key " + fixture.Secret + " email " + fixture.PII
			var db *store.Store
			if fixture.Source != "" {
				db = ingestConsumerSession(t, fixture, id, basePath, text)
			} else {
				db = seedSyncDoorSession(t, id, basePath)
			}
			entries := []schema.SessionEntry{
				{SessionID: sid, EntryIndex: 0, Role: schema.RoleUser, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, ContentPreview: &text},
				{SessionID: sid, EntryIndex: 1, Role: schema.RoleAssistant, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, ContentPreview: &text},
			}
			if fixture.Source == "" {
				input, err := db.LoadPublicationInput(t.Context(), sid)
				if err != nil {
					t.Fatal(err)
				}
				testutil.SeedReadyPublication(t, db, &input.Metadata, entries)
			}
			switch fixture.Damage {
			case "none", "source-omitted":
			case "incomplete":
				if err := db.IndexSessionEntries(t.Context(), sid, entries); err != nil {
					t.Fatal(err)
				}
			case "chunk", "projection":
				conn, err := db.Pool().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				query := `UPDATE session_entry_full_content_chunks SET data=zeroblob(byte_length) WHERE session_id=? AND entry_index=1 AND chunk_index=0`
				if fixture.Damage == "projection" {
					query = `UPDATE session_entries SET tool_output='damaged' WHERE session_id=? AND entry_index=1`
				}
				err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: []any{id}})
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("unknown fixture damage")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(string(defaults.ResolveDBFilePath()))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if fixture.StructuredOutput {
				page, err := db.ReadSessionEntries(t.Context(), sid, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, entry := range page.Entries {
					if entry.ToolOutput == nil {
						continue
					}
					var output map[string]string
					if err := json.Unmarshal([]byte(*entry.ToolOutput), &output); err != nil {
						t.Fatal(err)
					}
					found = output["text"] == "visible" && output["details"] == text
				}
				if !found {
					t.Fatal("structured tool output siblings missing after reopen")
				}
			}
			if fixture.ToolFile != "" {
				assertFullToolSemantics(t, db, sid, fixture.ToolFile, text)
			}
			denied := &deniedTranscriptFS{MemFS: testutil.NewMemFS()}
			provider := NewStoreDataProviderWithFS(db, sessionvisibility.All(), denied)
			session, detailErr := provider.SessionByID(t.Context(), id)
			detail, exportErr := export.ExportSession(t.Context(), db, denied, id)
			cfg := config.BaseConfig()
			cfg.Output.BasePath = basePath
			handler := &syncHandler{store: db, config: cfg}
			scan := httptest.NewRecorder()
			handler.handleSyncRedactions(scan, httptest.NewRequest("GET", "/api/v1/sync/redactions?session_id="+id+"&level=standard", nil))
			captured := &syncCapturedPublish{parts: map[string]string{}}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if !strings.Contains(r.URL.Path, "/transcripts/publish") {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				captured.record(r)
				receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), true)
				if err != nil {
					t.Error(err)
					http.Error(w, "invalid fixture publication", 500)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(receipt)
			}))
			defer remote.Close()
			writeSyncDoorCredentials(t, remote.URL)
			body, _ := json.Marshal(pushRequest{SessionIDs: []string{id}, Visibility: "private"})
			response := httptest.NewRecorder()
			handler.handleSyncPush(response, httptest.NewRequest("POST", "/api/v1/sync/push", bytes.NewReader(body)))
			parts := captured.snapshot()
			if fixture.Damage != "none" {
				if detailErr == nil || exportErr == nil || scan.Code == http.StatusOK || len(parts) != 0 {
					t.Fatalf("damaged capture escaped: detail=%v export=%v scan=%d uploads=%d", detailErr, exportErr, scan.Code, len(parts))
				}
				if !strings.Contains(response.Body.String(), "harvest index --force") {
					t.Fatalf("push failure lacks remediation: %s", response.Body.String())
				}
			} else {
				if detailErr != nil || exportErr != nil {
					t.Fatalf("detail=%v export=%v", detailErr, exportErr)
				}
				if fixture.Source == "" && (len(session.Turns) != 2 || len(detail.Turns) != 2) {
					t.Fatal("full conversation structure lost")
				}
				viewerJSON, _ := json.Marshal(session)
				exportJSON, _ := json.Marshal(detail)
				encodedText, _ := json.Marshal(text)
				if !fixture.StructuredOutput && (!bytes.Contains(viewerJSON, encodedText) || !bytes.Contains(exportJSON, encodedText)) {
					t.Fatal("full content differs after reopen")
				}
				if fixture.StructuredOutput && (!bytes.Contains(viewerJSON, []byte(fixture.Tail)) || !bytes.Contains(exportJSON, []byte(fixture.Tail))) {
					t.Fatal("structured output lost from viewer/export")
				}
				for _, role := range fixture.Roles {
					viewerFull, exportFull := false, false
					for _, turn := range session.Turns {
						if turn.Role == role && turn.Content == text {
							viewerFull = true
						}
					}
					for _, turn := range detail.Turns {
						if turn.Role == role && turn.Content == text {
							exportFull = true
						}
					}
					if !viewerFull || !exportFull {
						t.Fatalf("full %s turn missing from viewer/export", role)
					}
				}
				if scan.Code != http.StatusOK || !strings.Contains(scan.Body.String(), "anthropic") {
					t.Fatalf("late secret absent from share scan: %d %s", scan.Code, scan.Body.String())
				}
				if len(parts) != 2 {
					t.Fatalf("expected actual transcript and metadata multipart parts: %s", response.Body.String())
				}
				for name, part := range parts {
					if fixture.StructuredOutput && (!strings.Contains(part, "details") || !strings.Contains(part, "visible")) {
						t.Fatalf("multipart %s lost structured output siblings", name)
					}
					if !strings.Contains(part, fixture.Tail) || strings.Contains(part, fixture.Secret) || !strings.Contains(part, "ANTHROPIC_KEY") {
						t.Fatalf("multipart %s lost tail or failed late redaction", name)
					}
					if fixture.PII == "" || strings.Contains(part, fixture.PII) {
						t.Fatalf("multipart %s failed late PII redaction", name)
					}
				}
			}
			if denied.reads != 0 {
				t.Fatalf("authoritative consumers attempted %d denied transcript reads", denied.reads)
			}
		})
	}
}
