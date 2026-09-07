package ingest_test

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_discovery.yaml
var piDiscoveryYAML []byte

func TestPiDiscoveryLocationsAndCandidates(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name   string `yaml:"name"`
			Mode   string `yaml:"mode"`
			Reject string `yaml:"reject"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(strings.NewReader(string(piDiscoveryYAML)))
	d.KnownFields(true)
	if err := d.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, tc := range fixture.Cases {
		if seen[tc.Name] || tc.Name == "" {
			t.Fatal("duplicate or empty fixture name")
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			if err := os.Mkdir(project, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(project, "a.jsonl")
			body := `{"type":"session","version":3,"id":"` + testutil.TestSessionUUID + `","cwd":"/synthetic/project"}` + "\n"
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			selected := root
			switch tc.Mode {
			case "file-limit":
				if err := os.Truncate(path, (64<<20)+1); err != nil {
					t.Fatal(err)
				}
			case "line-limit":
				if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+strings.Repeat(" ", 8<<20)), 0600); err != nil {
					t.Fatal(err)
				}
			case "line-count-limit":
				if err := os.WriteFile(path, []byte(body+strings.Repeat("\n", 200000)), 0600); err != nil {
					t.Fatal(err)
				}
			case "root":
			case "project":
				selected = project
			case "file":
				selected = path
			case "missing":
				selected = filepath.Join(root, "missing")
			case "symlink":
				selected = t.TempDir()
				if err := os.Symlink(project, filepath.Join(selected, "linked-project")); err != nil {
					t.Fatal(err)
				}
			case "duplicate-invalid", "duplicate-size", "duplicate-path":
				second := filepath.Join(project, "b.jsonl")
				secondBody := body
				if tc.Mode == "duplicate-invalid" {
					secondBody += "{}\n"
				}
				if tc.Mode == "duplicate-size" {
					secondBody += "\n"
					path = second
				}
				if err := os.WriteFile(second, []byte(secondBody), 0600); err != nil {
					t.Fatal(err)
				}
				secondTime := stamp
				if tc.Mode == "duplicate-invalid" {
					secondTime = stamp.Add(time.Hour)
				}
				if err := os.Chtimes(second, secondTime, secondTime); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown discovery fixture mode %q", tc.Mode)
			}
			resolved, err := ingest.NewResolvedPath(selected)
			if err != nil {
				t.Fatal(err)
			}
			adapter := ingest.NewPiAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
			sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{resolved}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.Mode == "missing" {
				if len(sessions) != 0 {
					t.Fatal("missing root invented a session")
				}
				return
			}
			if tc.Reject != "" {
				if len(sessions) != 0 || len(adapter.DiscoveryDiagnostics()) != 1 || !strings.Contains(adapter.DiscoveryDiagnostics()[0].Detail, tc.Reject) {
					t.Fatalf("source limit not enforced: %+v", adapter.DiscoveryDiagnostics())
				}
				return
			}
			if len(sessions) != 1 || sessions[0].SourcePath.String() != path {
				t.Fatalf("wrong canonical winner: %+v", sessions)
			}
			if tc.Mode == "duplicate-invalid" && len(adapter.DiscoveryDiagnostics()) != 1 {
				t.Fatal("invalid newer candidate was not diagnosed")
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("required discovery case missing: %q", name)
		}
	}
}
