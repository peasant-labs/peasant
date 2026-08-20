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

const expectedCurrentSQLiteCLIWiringCases = 1

type currentSQLiteCLIWiringCase struct {
	Name             string `yaml:"name"`
	SourceFixture    string `yaml:"source_fixture"`
	ExpectedSessions int    `yaml:"expected_sessions"`
}

type currentSQLiteCLIWiringDocument struct {
	DeclaredCases int                          `yaml:"declared_cases"`
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
	if document.DeclaredCases != expectedCurrentSQLiteCLIWiringCases || len(document.Cases) != expectedCurrentSQLiteCLIWiringCases {
		t.Fatalf("current SQLite CLI wiring fixture row guard: declared=%d actual=%d expected=%d", document.DeclaredCases, len(document.Cases), expectedCurrentSQLiteCLIWiringCases)
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
			if !strings.Contains(output, wantDiscovery) || !strings.Contains(output, "NEW        opencode") {
				t.Fatalf("normal OpenCode CLI did not retain current SQLite discovery through its adapter factory; want %q and ingested OpenCode session in output:\n%s", wantDiscovery, output)
			}
		})
	}
}
