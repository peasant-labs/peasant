package main

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/index_format_queries.yaml
var indexFormatQueriesYAML []byte

type indexFormatQuerySession struct {
	ID      schema.SessionID   `yaml:"id"`
	Project schema.ProjectHash `yaml:"project"`
	Name    string             `yaml:"name"`
	Entries int                `yaml:"entries"`
	Format  int                `yaml:"format"`
}
type indexFormatQueryCase struct {
	Name             string            `yaml:"name"`
	Args             []string          `yaml:"args"`
	AnnotationTarget schema.TargetKind `yaml:"annotationTarget"`
	Refuse           bool              `yaml:"refuse"`
	WantContains     string            `yaml:"wantContains"`
}
type indexFormatQueryDocument struct {
	RequiredNames []string                  `yaml:"requiredNames"`
	Sessions      []indexFormatQuerySession `yaml:"sessions"`
	Cases         []indexFormatQueryCase    `yaml:"cases"`
}

func loadIndexFormatQueryFixtures(t *testing.T) indexFormatQueryDocument {
	t.Helper()
	var document indexFormatQueryDocument
	decoder := yaml.NewDecoder(bytes.NewReader(indexFormatQueriesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("index query fixture requires one document: %v", err)
	}
	names := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] || len(row.Args) == 0 || (row.AnnotationTarget != "" && row.AnnotationTarget != schema.TargetSession && row.AnnotationTarget != schema.TargetEntry) {
			t.Fatalf("invalid query fixture %+v", row)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index format query", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	for _, session := range document.Sessions {
		if _, err := schema.NewSessionID(string(session.ID)); err != nil {
			t.Fatal(err)
		}
		if _, err := schema.NewProjectHash(string(session.Project)); err != nil {
			t.Fatal(err)
		}
		if session.Name == "" || session.Entries < 1 {
			t.Fatal("query fixture must carry populated named sessions")
		}
	}
	return document
}

func TestIndexFormatCommandsValidateScopedCandidatesBeforeProjection(t *testing.T) {
	document := loadIndexFormatQueryFixtures(t)
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := writeTestConfigFile(t, dir)
			dbPath := defaults.ResolveDBFilePathWith(dir).String()
			if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(dbPath, store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			for _, session := range document.Sessions {
				storetest.SeedSessionInProject(t, db, string(session.ID), session.Project)
				entries := make([]schema.SessionEntry, session.Entries)
				for i := range entries {
					text := session.Name
					entries[i] = schema.SessionEntry{SessionID: session.ID, Harness: schema.HarnessClaudeCode, EntryIndex: i, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text}
				}
				if err := db.IndexSessionEntries(t.Context(), session.ID, entries); err != nil {
					t.Fatal(err)
				}
				if row.AnnotationTarget != "" {
					annotator, err := db.GetAnnotatorIDByName(t.Context(), "human-web")
					if err != nil {
						t.Fatal(err)
					}
					typeID, err := db.GetAnnotationTypeID(t.Context(), "research.friction_episode")
					if err != nil {
						t.Fatal(err)
					}
					params := store.CreateAnnotationParams{AnnotatorID: annotator, AnnotationTypeID: typeID, Value: "bad_handoff"}
					if row.AnnotationTarget == schema.TargetEntry {
						params.EntryTarget = &store.EntryTarget{SessionID: string(session.ID), EntryIndex: 0, EndIndex: 1}
					} else {
						id := string(session.ID)
						params.SessionID = &id
					}
					if _, err := db.CreateAnnotation(t.Context(), params); err != nil {
						t.Fatal(err)
					}
				}
				conn, err := db.Pool().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.ExecuteTransient(conn, "UPDATE projects SET canonical_cwd = ? WHERE project_hash = ?", &sqlitex.ExecOptions{Args: []any{"/synthetic/" + session.Name, string(session.Project)}})
				if err == nil && session.Format > 0 {
					err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_format_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{session.Format, string(session.ID)}})
				}
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			}
			root := buildRootCommand()
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			args := []string{"--config", configPath, "--data-dir", dir, "--config-dir", dir, "--state-dir", dir}
			root.SetArgs(append(args, row.Args...))
			err = root.Execute()
			if row.Refuse {
				var unsupported *store.UnsupportedIndexFormatError
				// Cobra's unchanged error path may print usage. No partial result
				// may precede that usage, and no session data may be emitted.
				out := strings.TrimSpace(stdout.String())
				if !errors.As(err, &unsupported) || (out != "" && !strings.HasPrefix(out, "Usage:\n")) {
					t.Fatalf("command emitted incomplete projection: err=%v stdout=%s stderr=%s", err, &stdout, &stderr)
				}
				for _, session := range document.Sessions {
					if strings.Contains(out, string(session.ID)) {
						t.Fatalf("refusal emitted session data: %s", out)
					}
				}
			} else if err != nil || (row.WantContains != "" && !strings.Contains(stdout.String(), row.WantContains)) {
				t.Fatalf("healthy or empty selected scope failed: err=%v stdout=%s stderr=%s", err, &stdout, &stderr)
			}
		})
	}
}
