package main

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pi_common_modes.yaml
var piModesYAML []byte

func TestPiKickstartDefaultDiscoveryAndSourcePreview(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fixture := loadPiCommonModesFixture(t)
	directory := filepath.Join(home, ".pi", "agent", "sessions", "project")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "session.jsonl"), []byte(fixture.Source), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "corrupt.jsonl"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	inventory, sessions, _ := ftueDiscover(t.Context(), filepath.Join(home, "missing-config.yaml"), filepath.Join(home, "missing.db"), nil)
	pi := inventory[defaults.HarnessPi]
	if !pi.Enabled || pi.SessionCount != 1 || !strings.Contains(pi.Detail, "Pi source skipped") {
		t.Fatalf("Pi discovery inventory lacks source diagnostic/count: %+v", pi)
	}
	if len(sessions) != 1 || sessions[0].Harness != schema.HarnessPi.String() || sessions[0].ProjectName != "project" {
		t.Fatalf("Pi not listed with native project: %+v", sessions)
	}
	turns, err := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, sessions).Turns(sessions[0].SessionID)
	if err != nil || len(turns) != 3 {
		t.Fatalf("Pi source preview: %v, %d turns", err, len(turns))
	}
	if !strings.Contains(turns[2].Content, "searchable Pi context") {
		t.Fatal("display=false context missing from preview")
	}
}

type piCommonModesFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Source        string   `yaml:"source"`
	Cases         []struct {
		Name          string   `yaml:"name"`
		First         []string `yaml:"first"`
		Second        []string `yaml:"second"`
		Stored        bool     `yaml:"stored"`
		Managed       bool     `yaml:"managed"`
		RemoveSource  bool     `yaml:"removeSource"`
		HeaderOnly    bool     `yaml:"headerOnly"`
		Redact        bool     `yaml:"redact"`
		LongText      int      `yaml:"longText"`
		InvalidUpdate bool     `yaml:"invalidUpdate"`
	} `yaml:"cases"`
}

func loadPiCommonModesFixture(t *testing.T) piCommonModesFixture {
	t.Helper()
	var fixture piCommonModesFixture
	if err := testutil.DecodeNamedFixtureYAML(piModesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestPiHarvestCommonModes(t *testing.T) {
	fixture := loadPiCommonModesFixture(t)
	seen := make(map[string]bool)
	for _, tc := range fixture.Cases {
		if seen[tc.Name] || tc.Name == "" {
			t.Fatal("duplicate/empty fixture name")
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			sourceText := fixture.Source
			if tc.LongText > 0 {
				sourceText = strings.ReplaceAll(sourceText, "native pipeline body", strings.Repeat("native pipeline body ", tc.LongText))
			}
			root := t.TempDir()
			source := filepath.Join(root, "recording.jsonl")
			if err := os.WriteFile(source, []byte(sourceText), 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "managed")
			if tc.Redact {
				output = filepath.Join(defaults.ResolveDataDirPathWith(root).String(), "peasant-sync")
			}
			base := []string{"--source-harness", schema.HarnessPi.String(), "--source-path", source, "--output", output, "--include-active", "--json"}
			forecast := slices.Contains(tc.First, "--dry-run") || slices.Contains(tc.Second, "--dry-run")
			seededDigest := ""
			if forecast {
				// A forecast inspects an existing checkpointed database and
				// creates none, so the state it reads exists before the run and is
				// fingerprinted here to prove the run left it alone.
				seededDigest = databaseDigest(t, seedClosedStore(t, root))
			}
			result, err := executeHarvestCmd(t, root, append(append([]string{}, tc.First...), base...))
			if err != nil {
				t.Fatalf("harvest: %v\n%s", err, result)
			}
			original, err := os.ReadFile(source)
			if err != nil || string(original) != sourceText {
				t.Fatal("original changed")
			}
			if len(tc.Second) > 0 {
				if tc.Redact {
					result, _, err := executeRedactCmd(t, root, []string{"--session", "11111111-2222-4333-8444-555555555555"})
					if err != nil {
						t.Fatalf("local redact: %v\n%s", err, result)
					}
					original, err := os.ReadFile(source)
					if err != nil || string(original) != fixture.Source {
						t.Fatal("local redact changed original")
					}
				}
				if tc.RemoveSource {
					if err := os.Remove(source); err != nil {
						t.Fatal(err)
					}
				}
				if tc.HeaderOnly {
					if err := os.WriteFile(source, []byte(strings.Split(fixture.Source, "\n")[0]+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if tc.InvalidUpdate {
					if err := os.WriteFile(source, []byte("{}\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				args := append([]string{}, tc.Second...)
				if args[0] == "index" {
					args = append(args, "--output", output, "--json")
				} else {
					args = append(args, base...)
				}
				result, err = executeHarvestCmd(t, root, args)
				if err != nil {
					t.Fatalf("second harvest: %v\n%s", err, result)
				}
			}
			dbPath := defaults.ResolveDBFilePathWith(root).String()
			if !tc.Stored {
				if forecast {
					assertDatabaseUnchanged(t, dbPath, seededDigest)
				} else if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
					t.Fatal("a run that stores nothing created a database")
				}
				_, err := os.Stat(output)
				if tc.Managed && err != nil {
					t.Fatal("logs-only omitted managed transcript")
				}
				if !tc.Managed && !os.IsNotExist(err) {
					t.Fatal("dry-run wrote output")
				}
				return
			}
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			provider := api.NewStoreDataProvider(db, sessionvisibility.All())
			session, err := provider.SessionByID(context.Background(), "11111111-2222-4333-8444-555555555555")
			if err != nil {
				t.Fatal(err)
			}
			detail, err := transcript.SessionToDetailValidated(session)
			if err != nil {
				t.Fatal(err)
			}
			exported, err := export.ExportSession(context.Background(), db, &ingest.OSFileSystem{}, session.ID.String())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(exported.Turns, detail.Turns) || !reflect.DeepEqual(exported.NativeMetadata, detail.NativeMetadata) {
				t.Fatal("native viewer/export production projections diverged")
			}
			if tc.HeaderOnly {
				if len(detail.Turns) != 0 || len(detail.NativeMetadata) != 0 {
					t.Fatal("header replacement retained obsolete conversation")
				}
				return
			}
			if len(detail.Turns) != 3 || detail.Harness != schema.HarnessPi || len(detail.NativeMetadata) != 1 {
				t.Fatalf("incomplete native detail: %+v", detail)
			}
			if tc.Redact && strings.Contains(detail.Turns[0].Content, "synthetic@example.com") {
				t.Fatal("managed reindex resurrected pre-redaction content")
			}
			if tc.LongText > 0 && strings.Count(detail.Turns[0].Content, "native pipeline body") != tc.LongText {
				t.Fatal("source-backed full-content overlay truncated the native body")
			}
			if strings.Count(detail.Turns[1].Content, "native thinking once") != 1 || detail.Turns[1].Usage == nil || detail.Turns[1].Usage.Completeness != schema.UsageComplete {
				t.Fatal("thinking/usage lost after persistence")
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required fixture %q", name)
		}
	}
}
