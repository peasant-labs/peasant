package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/documented_selection_examples.yaml
var documentedSelectionExamplesYAML []byte

type documentedExampleKind string

const (
	documentedExampleConfig   documentedExampleKind = "config"
	documentedExampleCommands documentedExampleKind = "commands"
)

type documentedSelectionExamples struct {
	DeclaredCases int                              `yaml:"declared_cases"`
	Cases         []documentedSelectionExampleCase `yaml:"cases"`
}

type documentedSelectionExampleCase struct {
	Name              string                  `yaml:"name"`
	Document          string                  `yaml:"document"`
	Marker            string                  `yaml:"marker"`
	Kind              documentedExampleKind   `yaml:"kind"`
	ExpectedSelection *config.SelectionConfig `yaml:"expected_selection,omitempty"`
	Commands          [][]string              `yaml:"commands,omitempty"`
}

func loadDocumentedSelectionExamples(t *testing.T) documentedSelectionExamples {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(documentedSelectionExamplesYAML))
	decoder.KnownFields(true)
	var document documentedSelectionExamples
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode documented selection examples with strict fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("documented selection examples must contain exactly one YAML document: %v", err)
	}

	const expectedCases = 3
	if document.DeclaredCases != expectedCases || len(document.Cases) != expectedCases {
		t.Fatalf("documented selection example row guard failed: declared=%d actual=%d expected=%d", document.DeclaredCases, len(document.Cases), expectedCases)
	}
	seenNames := make(map[string]struct{}, expectedCases)
	seenMarkers := make(map[string]struct{}, expectedCases)
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || strings.TrimSpace(testCase.Document) == "" || strings.TrimSpace(testCase.Marker) == "" {
			t.Fatalf("documented selection example row %d needs a name, document, and marker", index)
		}
		if filepath.IsAbs(testCase.Document) || !filepath.IsLocal(testCase.Document) {
			t.Fatalf("documented selection example %q has non-local document path %q", testCase.Name, testCase.Document)
		}
		if _, duplicate := seenNames[testCase.Name]; duplicate {
			t.Fatalf("documented selection examples repeat name %q", testCase.Name)
		}
		seenNames[testCase.Name] = struct{}{}
		markerKey := testCase.Document + "\x00" + testCase.Marker
		if _, duplicate := seenMarkers[markerKey]; duplicate {
			t.Fatalf("documented selection examples repeat marker %q in %q", testCase.Marker, testCase.Document)
		}
		seenMarkers[markerKey] = struct{}{}

		switch testCase.Kind {
		case documentedExampleConfig:
			if testCase.ExpectedSelection == nil || len(testCase.Commands) != 0 {
				t.Fatalf("documented config example %q needs expected_selection and no commands", testCase.Name)
			}
		case documentedExampleCommands:
			if testCase.ExpectedSelection != nil || len(testCase.Commands) == 0 {
				t.Fatalf("documented command example %q needs commands and no expected_selection", testCase.Name)
			}
		default:
			t.Fatalf("documented selection example %q has unknown kind %q", testCase.Name, testCase.Kind)
		}
	}
	return document
}

func TestDocumentedSelectionExamplesParseThroughProductionBoundaries(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate documented selection sources from the package working directory: %v", err)
	}
	repositoryRoot := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))

	for _, testCase := range loadDocumentedSelectionExamples(t).Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			documentPath := filepath.Join(repositoryRoot, filepath.FromSlash(testCase.Document))
			document, err := os.ReadFile(documentPath)
			if err != nil {
				t.Fatalf("read documented example source %q: %v", testCase.Document, err)
			}
			language := "bash"
			if testCase.Kind == documentedExampleConfig {
				language = "yaml"
			}
			block, err := extractVerifiedExample(string(document), testCase.Marker, language)
			if err != nil {
				t.Fatalf("extract documented example %q from %q: %v", testCase.Marker, testCase.Document, err)
			}

			switch testCase.Kind {
			case documentedExampleConfig:
				parsed, err := config.Parse([]byte(block))
				if err != nil {
					t.Fatalf("parse documented configuration through config.Parse: %v", err)
				}
				if !reflect.DeepEqual(parsed.Selection, *testCase.ExpectedSelection) {
					t.Fatalf("documented selection = %#v, want %#v", parsed.Selection, *testCase.ExpectedSelection)
				}
			case documentedExampleCommands:
				assertDocumentedCommands(t, block, testCase.Commands)
			}
		})
	}
}

func extractVerifiedExample(document, marker, language string) (string, error) {
	markerText := "<!-- verified-example: " + marker + " -->"
	if count := strings.Count(document, markerText); count != 1 {
		return "", fmt.Errorf("marker %q occurs %d times, want exactly once", markerText, count)
	}
	afterMarker := strings.SplitN(document, markerText, 2)[1]
	afterMarker = strings.TrimLeft(afterMarker, " \t\r\n")
	opener := "```" + language + "\n"
	if !strings.HasPrefix(afterMarker, opener) {
		return "", fmt.Errorf("marker %q must be followed by a %s code fence", markerText, language)
	}
	body := strings.TrimPrefix(afterMarker, opener)
	closing := strings.Index(body, "\n```")
	if closing < 0 {
		return "", fmt.Errorf("marker %q has no closing code fence", markerText)
	}
	return body[:closing] + "\n", nil
}

func assertDocumentedCommands(t *testing.T, block string, expected [][]string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(block), "\n")
	wantLines := make([]string, len(expected))
	for index, argv := range expected {
		wantLines[index] = strings.Join(argv, " ")
	}
	if !slices.Equal(lines, wantLines) {
		t.Fatalf("documented commands = %q, want %q", lines, wantLines)
	}
	for _, argv := range expected {
		parseDocumentedCommand(t, argv)
	}
}

func parseDocumentedCommand(t *testing.T, argv []string) {
	t.Helper()
	if len(argv) < 2 || argv[0] != "peasant" {
		t.Fatalf("documented command argv = %q, want peasant plus a subcommand", argv)
	}
	root := &cobra.Command{Use: "peasant", SilenceErrors: true, SilenceUsage: true}
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	for _, build := range commands {
		root.AddCommand(build())
	}
	args := append([]string(nil), argv[1:]...)
	root.SetArgs(append(args, "--help"))
	executed, err := root.ExecuteC()
	if err != nil {
		t.Fatalf("parse documented command %q through Cobra production command: %v", strings.Join(argv, " "), err)
	}
	if executed.Name() != argv[1] {
		t.Fatalf("documented command %q resolved to %q, want %q", strings.Join(argv, " "), executed.Name(), argv[1])
	}
}
