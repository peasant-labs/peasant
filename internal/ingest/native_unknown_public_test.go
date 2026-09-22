package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/native_unknown_public.yaml
var nativeUnknownPublicYAML []byte

type nativeUnknownPublicCase struct {
	RetainedFallback bool           `yaml:"retained_fallback"`
	Unaccounted      bool           `yaml:"unaccounted"`
	Legacy           string         `yaml:"legacy"`
	Message          string         `yaml:"message"`
	CopyBoundary     *int64         `yaml:"copy_boundary"`
	Inherited        bool           `yaml:"inherited"`
	Forbidden        []string       `yaml:"forbidden"`
	Name             string         `yaml:"name"`
	Harness          ingest.Harness `yaml:"harness"`
	Native           bool           `yaml:"native"`
	OpaqueOnly       bool           `yaml:"opaque_only"`
	Framing          bool           `yaml:"framing"`
	Source           string         `yaml:"source"`
	Rows             []ocProvRow    `yaml:"rows"`
	Namespace        string         `yaml:"namespace"`
	RecordIndices    []int64        `yaml:"record_indices"`
	Positions        []int64        `yaml:"positions"`
	Pointers         []string       `yaml:"pointers"`
}

type nativeUnknownPublicDocument struct {
	Corrupt  map[ingest.Harness]string `yaml:"corrupt"`
	Required []string                  `yaml:"required_names"`
	Payload  string                    `yaml:"payload"`
	Expected string                    `yaml:"expected"`
	Cases    []nativeUnknownPublicCase `yaml:"cases"`
}

func loadNativeUnknownPublic(t *testing.T) nativeUnknownPublicDocument {
	t.Helper()
	var doc nativeUnknownPublicDocument
	decoder := yaml.NewDecoder(bytes.NewReader(nativeUnknownPublicYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("trailing fixture document", err)
	}
	names := map[string]bool{}
	var actualNames []string
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] || len(c.Positions) == 0 || len(c.Positions) != len(c.RecordIndices) || len(c.Positions) != len(c.Pointers) {
			t.Fatalf("invalid fixture %q", c.Name)
		}
		names[c.Name] = true
		actualNames = append(actualNames, c.Name)
	}
	if err := testutil.RequireFixtureNames("native unknown public", "case", doc.Required, names); err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: doc.Required}, actualNames, "native unknown public"); err != nil {
		t.Fatal(err)
	}
	return doc
}

type nativeUnknownFailingStore struct{ *unknownFailingStore }

func (s *nativeUnknownFailingStore) ActivateNativeGeneration(ctx context.Context, activation ingest.NativeGenerationActivation) error {
	if s.fail {
		// Exercise the real artifact transaction's missing-blob failure, not a
		// prepared success result or a fabricated accounting diagnostic.
		activation.Blobs = maps.Clone(activation.Blobs)
		delete(activation.Blobs, activation.Generation.Generation.Content[0].Ref)
	}
	return s.Store.ActivateNativeGeneration(ctx, activation)
}

var _ ingest.NativeGenerationActivator = (*nativeUnknownFailingStore)(nil)

func TestNativeUnknownSourceToPublication(t *testing.T) {
	doc := loadNativeUnknownPublic(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			fs := &ingest.OSFileSystem{}
			// Replace the secret marker first so padding substitution cannot alter it.
			payload := strings.ReplaceAll(strings.ReplaceAll(doc.Payload, "SECRET_BODY", strings.Repeat("A", 36)), "BODY", strings.Repeat("synthetic-", 1024))
			expected := strings.ReplaceAll(doc.Expected, "BODY", strings.Repeat("synthetic-", 1024))
			sid := schema.SessionID(testutil.TestSessionUUID)
			var path ingest.ResolvedPath
			var damageSource func()
			adapters := map[ingest.Harness]ingest.AdapterFactory{}
			if c.Harness == ingest.HarnessCodex {
				path = ingest.ResolvedPath(filepath.Join(dir, string(sid)+".jsonl"))
				source := strings.ReplaceAll(strings.ReplaceAll(c.Source, "SESSION_ID", string(sid)), "UNKNOWN", payload)
				if c.CopyBoundary != nil {
					fields := fmt.Sprintf(`"history_mode":"legacy","subagent_history_start_ordinal":%d`, *c.CopyBoundary)
					if c.Inherited {
						fields += `,"forked_from_id":"11111111-1111-4111-8111-111111111111"`
					}
					source = strings.Replace(source, `"history_mode":"legacy"`, fields, 1)
				}
				if err := os.WriteFile(path.String(), []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
				damageSource = func() {
					if err := os.WriteFile(path.String(), []byte(source+doc.Corrupt[c.Harness]+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				session := ingest.DiscoveredSession{SessionID: sid, Harness: c.Harness, SourcePath: path, SourceFormat: ingest.SourceFormatJSONL, CWD: "/workspace", ModTime: time.Now().Add(-time.Hour)}
				adapters[c.Harness] = func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
					return &fixedUnknownDiscovery{SourceAdapter: ingest.NewCodexAdapter(fs, git, s), session: session}
				}
			} else if c.Legacy != "" {
				sid = "ses_nativeUnknownPublic"
				path = setupLegacyUnknownPublic(t, c, dir, sid, payload)
				adapters[c.Harness] = func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
					return ingest.NewOpenCodeAdapter(fs, git, s)
				}
			} else {
				sid = "ses_nativeUnknownPublic"
				source := testfixture.MaterializeByName(t, "native-current-rows")
				rows := append([]ocProvRow(nil), c.Rows...)
				for i := range rows {
					rows[i].Data = strings.ReplaceAll(rows[i].Data, "UNKNOWN", payload)
				}
				seedOpenCodeProvenanceCase(t, source, ocProvCase{Scope: ocProvScope{SessionID: string(sid), ParentNullProven: true}, Rows: rows})
				damageSource = func() {
					connection, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
					if err != nil {
						t.Fatal(err)
					}
					defer connection.Close()
					if err := sqlitex.Execute(connection, "UPDATE session_message SET type='user',data=?,time_updated=time_updated+1000000,seq=seq+1000000 WHERE id=?", &sqlitex.ExecOptions{Args: []any{doc.Corrupt[c.Harness], rows[0].ID}}); err != nil {
						t.Fatal(err)
					}
					if err := sqlitex.Execute(connection, "UPDATE session SET time_updated=time_updated+1000000 WHERE id=?", &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
						t.Fatal(err)
					}
				}
				path = ingest.ResolvedPath(filepath.Dir(source.Path))
				adapters[c.Harness] = func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
					return ingest.NewOpenCodeAdapter(fs, git, s)
				}
			}
			dbPath := storetest.CopyGoldenDB(t)
			root := filepath.Join(dir, "artifacts")
			open := func() *store.Store {
				var options []store.OpenOption
				if c.Native {
					artifacts, err := store.NewOSGenerationArtifactStore(root)
					if err != nil {
						t.Fatal(err)
					}
					locks, err := store.NewFileSessionLocker(root)
					if err != nil {
						t.Fatal(err)
					}
					options = append(options, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locks))
				}
				db, err := store.Open(dbPath, options...)
				if err != nil {
					t.Fatal(err)
				}
				return db
			}
			db := open()
			defer func() { _ = db.Close() }()
			writer := &nativeUnknownFailingStore{unknownFailingStore: &unknownFailingStore{Store: db}}
			cfg := makePipelineConfig(filepath.Join(dir, "managed"))
			cfg.Force = true
			cfg.Sources = map[ingest.Harness]ingest.SourceConfig{c.Harness: {Enabled: true, Paths: []ingest.ResolvedPath{path}}}
			cfg.AllowedSessionIDs = map[ingest.SessionID]bool{sid: true}
			run := func() *ingest.PipelineResult {
				p, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithStore(writer), ingest.WithMetricsStore(writer), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
				if err != nil {
					t.Fatal(err)
				}
				result, err := p.Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			result := run()
			if result.Summary.Indexed != 1 {
				t.Fatalf("ordinary harvest did not index: %+v logs=%+v", result.Summary, result.IndexLog)
			}
			capture, found, err := db.GetSessionContentCapture(t.Context(), sid)
			if c.Unaccounted {
				if err != nil || !found || capture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted || capture.CaptureFormat != ingest.ContentCaptureFormatPreviewOnly {
					t.Fatalf("unaccounted omission falsely certified: %+v %v", capture, err)
				}
				if _, err := export.ExportSession(t.Context(), db, fs, string(sid)); err == nil {
					t.Fatal("unaccounted omission exported as full content")
				}
				return
			}
			if err != nil || !found || capture.Status != ingest.ContentCaptureIncomplete || capture.FailureCode != ingest.ContentCaptureUnknownDataRetained || capture.CaptureFormat != ingest.ContentCaptureFormatFull {
				t.Fatalf("unaccounted native status: %+v %v", capture, err)
			}
			counts := result.Summary.RetainedUnknownKinds
			if len(counts) != 1 || counts[0].Namespace != c.Namespace || counts[0].Occurrences != len(c.Positions) || counts[0].Sessions != 1 {
				t.Fatalf("successful occurrence/session accounting: %+v", counts)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = open()
			writer.Store = db
			detail, err := export.ExportSession(t.Context(), db, fs, string(sid))
			if err != nil {
				t.Fatal(err)
			}
			if c.Framing {
				expected = " " + expected + " "
			}
			checkNativeUnknownPublic(t, c, expected, detail)
			engine, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			publisher := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{MinPushContractVersion: "0.0.1", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: schema.AllContentCapabilities}}
			pushConfig := &config.Config{Output: config.OutputConfig{BasePath: cfg.OutputDir.String()}, Push: config.PushConfig{Method: config.PushMethodAll, Visibility: config.VisibilityPrivate}}
			credentials := &auth.Credentials{APIKey: "synthetic", KeyID: "synthetic", UserID: "synthetic", Username: "synthetic", VillageURL: "https://village.example.test"}
			publish := func() *push.PushResult {
				p, err := push.NewPipeline(db, publisher, credentials, pushConfig, nil, push.PipelineConfig{Force: true, Concurrency: 1, FilterSessionIDs: []string{string(sid)}}, engine, &bytes.Buffer{})
				if err != nil {
					t.Fatal(err)
				}
				result, err := p.Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			published := publish()
			if c.OpaqueOnly {
				if len(publisher.Calls) != 0 || published.Errors == 0 {
					t.Fatalf("publication invented a model: %+v", published)
				}
			} else {
				if len(publisher.Calls) != 1 || published.Errors != 0 {
					t.Fatalf("production push failed: %+v calls=%d", published, len(publisher.Calls))
				}
				var envelope schema.TranscriptContent
				if err := json.Unmarshal(publisher.Calls[0].TranscriptBody, &envelope); err != nil {
					t.Fatal(err)
				}
				checkNativeUnknownPublic(t, c, expected, envelope.SessionDetail)
				if len(publisher.AuthoritativeCalls) != 1 || publisher.AuthoritativeCalls[0].Diagnostics.Partial == nil || !*publisher.AuthoritativeCalls[0].Diagnostics.Partial {
					t.Fatal("authoritative partial mirror lost")
				}
				publisher.Calls = nil
				publisher.AuthoritativeCalls = nil
				publisher.SchemaVersionResp.ContentCapabilities = slices.DeleteFunc(append([]schema.ContentCapability(nil), schema.AllContentCapabilities...), func(capability schema.ContentCapability) bool {
					return capability == schema.ContentCapabilityRetainedUnknownV1
				})
				refused := publish()
				if len(publisher.Calls) != 0 || refused.Errors == 0 {
					t.Fatal("receiver without retention capability received a stripped upload")
				}
			}
			priorExport, err := export.ExportSession(t.Context(), db, fs, string(sid))
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(priorExport)
			cfg.Reindex = true
			writer.fail = true
			failed := run()
			if failed.Summary.Indexed != 0 || len(failed.Summary.RetainedUnknownKinds) != 0 {
				t.Fatalf("failed evidence write certified counts: %+v", failed.Summary)
			}
			after, err := export.ExportSession(t.Context(), db, fs, string(sid))
			if err != nil {
				t.Fatal(err)
			}
			afterBytes, _ := json.Marshal(after)
			if !bytes.Equal(before, afterBytes) {
				t.Fatal("failed write replaced prior export")
			}
			if damageSource != nil {
				writer.fail = false
				cfg.Reindex = false
				damageSource()
				damaged := run()
				if c.RetainedFallback {
					refused := false
					for _, diagnostic := range damaged.Diagnostics {
						if diagnostic.ErrorType == "adapter_refresh_unavailable" && strings.Contains(diagnostic.Message, "cannot unmarshal number") {
							refused = true
						}
					}
					if !refused || damaged.Summary.Indexed != 1 {
						t.Fatalf("native corruption was not refused before retained recovery: %+v", damaged.Diagnostics)
					}
				} else if damaged.Summary.Indexed != 0 || len(damaged.Summary.RetainedUnknownKinds) != 0 {
					t.Fatalf("corrupt known source certified: %+v logs=%+v diagnostics=%+v", damaged.Summary, damaged.IndexLog, damaged.Diagnostics)
				}
				preserved, err := export.ExportSession(t.Context(), db, fs, string(sid))
				if err != nil {
					t.Fatal(err)
				}
				preservedBytes, _ := json.Marshal(preserved)
				if !bytes.Equal(before, preservedBytes) {
					t.Fatal("corrupt known source replaced prior export")
				}
			}
		})
	}
}

func checkNativeUnknownPublic(t *testing.T, c nativeUnknownPublicCase, expected string, detail *schema.SessionDetailPayload) {
	t.Helper()
	if detail == nil || detail.Diagnostics == nil || !detail.Diagnostics.Partial || len(detail.RetainedUnknown) != len(c.Positions) {
		t.Fatalf("public opaque evidence missing: %+v", detail)
	}
	source := ""
	for i, record := range detail.RetainedUnknown {
		if record.Namespace != c.Namespace || record.Kind != "future" || record.RecordIndex != c.RecordIndices[i] || record.Position != c.Positions[i] || record.Pointer != c.Pointers[i] || record.Payload != expected {
			t.Fatalf("public evidence differs: namespace=%s record=%d position=%d pointer=%s payload=%.200s", record.Namespace, record.RecordIndex, record.Position, record.Pointer, record.Payload)
		}
		if source != "" && source != record.SourceRef {
			t.Fatal("one physical stream split per occurrence")
		}
		source = record.SourceRef
	}
	if c.OpaqueOnly && len(detail.Turns) != 0 {
		t.Fatal("opaque-only capture fabricated turns")
	}
	if c.CopyBoundary != nil && !c.Inherited && len(detail.EarlierHistory) == 0 {
		t.Fatal("uncertain earlier history disappeared")
	}
	closing := c.OpaqueOnly
	turns := append([]schema.TurnDetail(nil), detail.Turns...)
	for _, section := range detail.EarlierHistory {
		turns = append(turns, section.Turns...)
	}
	for _, turn := range turns {
		for _, call := range turn.ToolCalls {
			if strings.Contains(call.Result, "closing") {
				closing = true
			}
			if strings.Contains(call.Result, "synthetic-synthetic-") {
				t.Fatal("opaque tool output displayed")
			}
		}
		if strings.Contains(turn.Content, "closing") {
			closing = true
		}
		for _, forbidden := range c.Forbidden {
			if strings.Contains(turn.Content, forbidden) {
				t.Fatal("excluded history reappeared")
			}
		}
		if strings.Contains(turn.Content, "synthetic-synthetic-") {
			t.Fatal("opaque payload displayed")
		}
	}
	if !closing {
		t.Fatal("known closing sibling disappeared")
	}
}

func setupLegacyUnknownPublic(t *testing.T, c nativeUnknownPublicCase, dir string, sid schema.SessionID, payload string) ingest.ResolvedPath {
	t.Helper()
	const messageID = "msg_legacy_public"
	if c.Legacy == "file-tree" {
		write := func(relative, data string) {
			path := filepath.Join(dir, "provider", "storage", relative)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join("session", "project", string(sid)+".json"), fmt.Sprintf(`{"id":%q,"directory":"/workspace","projectID":"project","version":"1","time":{"created":1,"updated":4}}`, sid))
		write(filepath.Join("message", string(sid), messageID+".json"), c.Message)
		for _, row := range c.Rows {
			write(filepath.Join("part", messageID, row.ID+".json"), strings.ReplaceAll(row.Data, "UNKNOWN", payload))
		}
		return ingest.ResolvedPath(filepath.Join(dir, "provider"))
	}
	if c.Legacy != "sqlite" {
		t.Fatalf("unknown legacy source %q", c.Legacy)
	}
	source := testfixture.MaterializeByName(t, "legacy-message-part")
	connection, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	exec := func(query string, args ...any) {
		if err := sqlitex.Execute(connection, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	exec("DELETE FROM part")
	exec("DELETE FROM message")
	exec("DELETE FROM session")
	exec("INSERT OR REPLACE INTO session(id,parent_id,time_created,time_updated) VALUES(?,NULL,1,4)", string(sid))
	exec("INSERT INTO message(id,session_id,time_created,time_updated,data) VALUES(?,?,1,4,?)", messageID, string(sid), c.Message)
	for i, row := range c.Rows {
		exec("INSERT INTO part(id,message_id,session_id,time_created,time_updated,data) VALUES(?,?,?,?,?,?)", row.ID, messageID, string(sid), i+1, i+1, strings.ReplaceAll(row.Data, "UNKNOWN", payload))
	}
	if c.Unaccounted {
		exec("INSERT INTO part(id,message_id,session_id,time_created,time_updated,data) VALUES('prt_orphan_bad','msg_missing',?,5,5,'{')", string(sid))
	}
	return ingest.ResolvedPath(filepath.Dir(source.Path))
}
