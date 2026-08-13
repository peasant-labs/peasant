package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

//go:embed testdata/kickstart_post_save.yaml
var kickstartPostSaveData []byte

type kickstartPostSaveCase struct {
	Name                     string                `yaml:"name"`
	Mode                     config.SelectionMode  `yaml:"mode"`
	Listings                 []ftue.SessionListing `yaml:"listings"`
	SelectedWorkingDirs      []string              `yaml:"selectedWorkingDirs"`
	ExpectSelectedSessionIDs []string              `yaml:"expectSelectedSessionIds"`
	ExpectImportAll          bool                  `yaml:"expectImportAll"`
}

type kickstartPostSaveDocument struct {
	ExpectedCaseCount int                     `yaml:"expectedCaseCount"`
	Cases             []kickstartPostSaveCase `yaml:"cases"`
}

func loadKickstartPostSaveDocument(t *testing.T) kickstartPostSaveDocument {
	t.Helper()
	var document kickstartPostSaveDocument
	decoder := yaml.NewDecoder(bytes.NewReader(kickstartPostSaveData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode testdata/kickstart_post_save.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("kickstart_post_save.yaml must hold exactly one document: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || len(document.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", document.ExpectedCaseCount, len(document.Cases))
	}
	for _, testCase := range document.Cases {
		if testCase.Name == "" || !testCase.Mode.IsValid() || len(testCase.Listings) < 2 {
			t.Fatalf("invalid post-save fixture case %q: mode=%q listings=%d", testCase.Name, testCase.Mode, len(testCase.Listings))
		}
	}
	return document
}

func TestKickstartPostSaveIngestUsesResolvedCloneCohort(t *testing.T) {
	document := loadKickstartPostSaveDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			root := t.TempDir()
			listings := append([]ftue.SessionListing(nil), testCase.Listings...)
			for index := range listings {
				listings[index].WorkingDir = filepath.Join(root, listings[index].WorkingDir)
				if err := os.MkdirAll(listings[index].WorkingDir, 0o755); err != nil {
					t.Fatalf("create discovered clone %q: %v", listings[index].WorkingDir, err)
				}
			}

			selection := config.SelectionConfig{Mode: testCase.Mode}
			if testCase.Mode == config.SelectionModeSelected {
				clonePaths := make([]string, len(testCase.SelectedWorkingDirs))
				for index, relative := range testCase.SelectedWorkingDirs {
					clonePaths[index] = filepath.Join(root, relative)
				}
				selection.Harnesses = map[string]config.SelectionHarnessConfig{
					listings[0].Harness: {Projects: []config.ProjectSelection{{
						GitRemote:  listings[0].GitRemote,
						ClonePaths: clonePaths,
					}}},
				}
			}
			configured := config.BaseConfig()
			configured.Selection = selection
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := config.SaveAtomic(configPath, configured); err != nil {
				t.Fatalf("save mounted kickstart selection: %v", err)
			}

			var captured ftue.WizardAnswers
			runner := func(_ context.Context, answers ftue.WizardAnswers) (*ftue.IngestResult, error) {
				captured = answers
				return &ftue.IngestResult{}, nil
			}
			callback := kickstartIngestFuncWithRunner(configPath, listings, ingest.NewPhysicalPathResolver(), runner)
			if _, err := callback(t.Context()); err != nil {
				t.Fatalf("run mounted post-save ingest callback: %v", err)
			}

			gotIDs := make([]string, len(captured.SelectedSessions))
			for index, listing := range captured.SelectedSessions {
				gotIDs[index] = listing.SessionID
			}
			sort.Strings(gotIDs)
			wantIDs := append([]string(nil), testCase.ExpectSelectedSessionIDs...)
			sort.Strings(wantIDs)
			if len(gotIDs) == 0 {
				gotIDs = nil
			}
			if len(wantIDs) == 0 {
				wantIDs = nil
			}
			if !reflect.DeepEqual(gotIDs, wantIDs) {
				t.Fatalf("post-save selected sessions = %v, want %v", gotIDs, wantIDs)
			}
			if len(captured.ProviderSelections) != 1 || captured.ProviderSelections[0].Harness != listings[0].Harness || captured.ProviderSelections[0].ImportAll != testCase.ExpectImportAll {
				t.Fatalf("post-save provider selections = %#v, want harness %q importAll=%t", captured.ProviderSelections, listings[0].Harness, testCase.ExpectImportAll)
			}
		})
	}
}
