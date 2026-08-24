package main

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
)

type currentSQLiteCLIWiringCase struct {
	Name             string `yaml:"name"`
	SourceFixture    string `yaml:"source_fixture"`
	ExpectedSessions int    `yaml:"expected_sessions"`
}

type currentSQLiteCLIWiringDocument struct {
	RequiredCases []string                     `yaml:"required_cases"`
	Cases         []currentSQLiteCLIWiringCase `yaml:"cases"`
}

//go:embed testdata/opencode_current_cli_wiring.yaml
var currentSQLiteCLIWiringYAML []byte

func loadCurrentSQLiteCLIWiringDocument(t testing.TB) currentSQLiteCLIWiringDocument {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(currentSQLiteCLIWiringYAML))
	decoder.KnownFields(true)
	var document currentSQLiteCLIWiringDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode current SQLite CLI wiring fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("current SQLite CLI wiring fixture must contain exactly one document: %v", err)
	}
	presentWiring := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		presentWiring[testCase.Name] = struct{}{}
	}
	if len(document.RequiredCases) == 0 {
		t.Fatal("current SQLite CLI wiring fixture declares no required cases")
	}
	for _, name := range document.RequiredCases {
		if _, ok := presentWiring[name]; !ok {
			t.Fatalf("current SQLite CLI wiring fixture is missing required case %q", name)
		}
	}
	for _, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || strings.TrimSpace(testCase.SourceFixture) == "" || testCase.ExpectedSessions < 1 {
			t.Fatalf("current SQLite CLI wiring fixture has an invalid case: %+v", testCase)
		}
	}
	return document
}

func TestCurrentOpenCodeSQLiteNormalCLIAdapterWiring(t *testing.T) {
	for _, testCase := range loadCurrentSQLiteCLIWiringDocument(t).Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
			commandRoot := t.TempDir()
			outputRoot := filepath.Join(commandRoot, "managed")
			output, err := executeHarvestCmd(t, commandRoot, []string{"--source-provider=" + defaults.HarnessOpenCode.String(), "--source-path=" + filepath.Dir(materialized.Path), "--output=" + outputRoot})
			if err != nil {
				t.Fatalf("normal OpenCode CLI harvest silently disabled current SQLite discovery: %v\n%s", err, output)
			}
			wantDiscovery := "peasant harvest: " + strconv.Itoa(testCase.ExpectedSessions) + " sessions"
			// The freshly materialized database's file mtime is within the
			// staleness threshold while its row times and session clock are
			// older, so a session still being written classifies active on its
			// file mtime, not on the older changed clock.
			if !strings.Contains(output, wantDiscovery) || !strings.Contains(output, "ACTIVE     opencode") {
				t.Fatalf("normal OpenCode CLI did not classify a still-active current SQLite session on its file mtime through its adapter factory; want %q and an ACTIVE OpenCode session in output:\n%s", wantDiscovery, output)
			}
		})
	}
}
