package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/harvest_index_selection.yaml
var harvestIndexSelectionYAML []byte

type harvestIndexSessionFixture struct {
	Name         string           `yaml:"name"`
	ID           ingest.SessionID `yaml:"id"`
	Harness      ingest.Harness   `yaml:"harness"`
	Age          string           `yaml:"age"`
	VersionDelta int              `yaml:"version_delta"`
	Transcript   string           `yaml:"transcript"`
}

type harvestIndexSelectionFixture struct {
	Name            string   `yaml:"name"`
	Args            []string `yaml:"args"`
	Indexed         []string `yaml:"indexed"`
	Refused         []string `yaml:"refused"`
	SelectedSession string   `yaml:"selected_session"`
	ErrorContains   string   `yaml:"error_contains"`
}

func LoadHarvestIndexSelectionFixtures(t testing.TB) ([]harvestIndexSessionFixture, []harvestIndexSelectionFixture) {
	t.Helper()
	var document struct {
		RequiredNames        []string                       `yaml:"required_names"`
		RequiredSessionNames []string                       `yaml:"required_session_names"`
		Sessions             []harvestIndexSessionFixture   `yaml:"sessions"`
		Cases                []harvestIndexSelectionFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(harvestIndexSelectionYAML, &document); err != nil {
		t.Fatal(err)
	}
	sessionNames := make(map[string]bool)
	ids := make(map[ingest.SessionID]bool)
	for _, session := range document.Sessions {
		if session.Name == "" || sessionNames[session.Name] || ids[session.ID] {
			t.Fatalf("empty or duplicate index selection session fixture %q", session.Name)
		}
		if _, err := ingest.NewSessionID(string(session.ID)); err != nil {
			t.Fatal(err)
		}
		if _, ok := ingest.HarvesterVersionRegistry[session.Harness]; !ok {
			t.Fatalf("fixture %s has no harvester registration", session.Name)
		}
		sessionNames[session.Name], ids[session.ID] = true, true
	}
	if err := testutil.RequireFixtureNames("harvest index selection", "session", document.RequiredSessionNames, sessionNames); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate index selection fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
		for _, name := range append(slices.Clone(fixture.Indexed), fixture.Refused...) {
			if !sessionNames[name] {
				t.Fatalf("fixture %s references unknown session %q", fixture.Name, name)
			}
		}
	}
	if err := testutil.RequireFixtureNames("harvest index selection", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document.Sessions, document.Cases
}

func TestHarvestIndexSelectionMounted(t *testing.T) {
	sessions, cases := LoadHarvestIndexSelectionFixtures(t)
	for _, fixture := range cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			output := filepath.Join(dir, "managed")
			dbPath := defaults.ResolveDBFilePathWith(dir).String()
			if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			before := make(map[ingest.SessionID]harvestIndexSnapshot)
			files := make(map[string][]byte)
			for _, session := range sessions {
				seedHarvestIndexSession(t, db, output, session, files)
				before[session.ID] = readHarvestIndexSnapshot(t, db, session.ID)
			}
			configPath := writeTestConfigFile(t, dir)
			if fixture.SelectedSession != "" {
				cfg, err := loadConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Selection.Mode = config.SelectionModeSelected
				cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
					string(defaults.HarnessCodex): {Sessions: []string{fixture.SelectedSession}},
				}
				data, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(configPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			root := buildRootCommand()
			var buf, diagnostics bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&diagnostics)
			args := []string{"--config", configPath, "--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "harvest", "index", "--output", output, "--json"}
			root.SetArgs(append(args, fixture.Args...))
			err = root.Execute()
			if fixture.ErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), fixture.ErrorContains) {
					t.Fatalf("command error = %v, want %q; output: %s", err, fixture.ErrorContains, &buf)
				}
			} else {
				if err != nil {
					t.Fatalf("harvest index: %v; output: %s", err, &buf)
				}
				var result struct {
					Summary  ingest.PipelineSummary `json:"summary"`
					Sessions []struct {
						SessionID ingest.SessionID `json:"sessionId"`
					} `json:"sessions"`
				}
				if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
					t.Fatalf("decode result: %v; output: %s", err, &buf)
				}
				if result.Summary.Indexed != len(fixture.Indexed) {
					t.Errorf("indexed = %d, want %d; output: %s", result.Summary.Indexed, len(fixture.Indexed), &buf)
				}
				for _, session := range sessions {
					if slices.Contains(fixture.Refused, session.Name) && (!strings.Contains(diagnostics.String(), string(session.ID)) || !strings.Contains(diagnostics.String(), "preserved")) {
						t.Fatalf("refused session %s lacks visible stderr warning: %s", session.Name, &diagnostics)
					}
				}
				var got, want []ingest.SessionID
				for _, selected := range result.Sessions {
					got = append(got, selected.SessionID)
				}
				for _, session := range sessions {
					if slices.Contains(fixture.Indexed, session.Name) || slices.Contains(fixture.Refused, session.Name) {
						want = append(want, session.ID)
					}
				}
				slices.Sort(got)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Errorf("target sessions = %v, want %v", got, want)
				}
			}
			for _, session := range sessions {
				after := readHarvestIndexSnapshot(t, db, session.ID)
				if slices.Contains(fixture.Indexed, session.Name) {
					if after.IndexerVersion != ingest.HarvesterVersionRegistry[session.Harness].IndexerVersion || reflect.DeepEqual(after.Entries, before[session.ID].Entries) {
						t.Errorf("selected session %s not refreshed: %+v", session.Name, after)
					}
				} else if !reflect.DeepEqual(after, before[session.ID]) {
					t.Errorf("unselected/refused session %s changed: before=%+v after=%+v", session.Name, before[session.ID], after)
				}
			}
			assertHarvestIndexLogScope(t, db, sessions, fixture)
			for path, original := range files {
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, original) {
					t.Errorf("retained artifact changed at %s: %v", path, err)
				}
			}
		})
	}
}

func assertHarvestIndexLogScope(t testing.TB, db *store.Store, sessions []harvestIndexSessionFixture, fixture harvestIndexSelectionFixture) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var got, want []string
	err = sqlitex.ExecuteTransient(conn, "SELECT DISTINCT session_id FROM index_log ORDER BY session_id", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			got = append(got, stmt.ColumnText(0))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if slices.Contains(fixture.Indexed, session.Name) || slices.Contains(fixture.Refused, session.Name) {
			want = append(want, string(session.ID))
		}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("index log session scope = %v, want %v", got, want)
	}
}

type harvestIndexSnapshot struct {
	IndexerVersion int
	IndexedAt      int64
	Entries        []schema.SessionEntry
	Session        *store.SessionRow
}

func readHarvestIndexSnapshot(t testing.TB, db *store.Store, id ingest.SessionID) harvestIndexSnapshot {
	t.Helper()
	var snapshot harvestIndexSnapshot
	var err error
	snapshot.Entries, err = db.ListEntries(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Session, err = db.SessionByID(t.Context(), string(id))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	err = sqlitex.ExecuteTransient(conn, "SELECT index_version, indexed_at FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			snapshot.IndexerVersion, snapshot.IndexedAt = stmt.ColumnInt(0), stmt.ColumnInt64(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func seedHarvestIndexSession(t testing.TB, db *store.Store, output string, fixture harvestIndexSessionFixture, files map[string][]byte) {
	t.Helper()
	age, err := time.ParseDuration(fixture.Age)
	if err != nil {
		t.Fatal(err)
	}
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID, meta.ModelHarness = fixture.ID, fixture.Harness
	meta.HostSlug = ingest.HostSlug(testutil.TestHostSlug)
	meta.Timestamp.Start = time.Now().Add(-age).UnixMilli()
	meta.Timestamp.End = meta.Timestamp.Start + 60000
	ingested := meta.Timestamp.End
	meta.Timestamp.Ingested = &ingested
	meta.Project.Hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	meta.Project.Name = "fixture-project"
	meta.Source = ingest.SourceInfo{Format: ingest.SourceFormatJSONL}
	directory := filepath.Join(output, string(meta.HostSlug), string(fixture.ID))
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(directory, string(fixture.ID)+defaults.MetadataSuffix)
	transcriptPath := filepath.Join(directory, string(fixture.ID)+"--transcript.jsonl")
	files[metaPath], files[transcriptPath] = metaData, []byte(fixture.Transcript)
	if err := os.WriteFile(metaPath, metaData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(fixture.Transcript), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: &meta, Session: ingest.DiscoveredSession{SessionID: fixture.ID, Harness: fixture.Harness}}}); err != nil {
		t.Fatal(err)
	}
	content := "last-good " + fixture.Name
	entries := []schema.SessionEntry{{SessionID: fixture.ID, Harness: fixture.Harness, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &content}}
	results := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{SessionID: fixture.ID, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, IndexerVersion: ingest.HarvesterVersionRegistry[fixture.Harness].IndexerVersion + fixture.VersionDelta, IndexedAtMs: 1700000001000}})
	if len(results) != 1 || !results[0].Written {
		t.Fatalf("seed index: %+v", results)
	}
}
