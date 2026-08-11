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
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

const (
	expectedSelectionRunnerSourceSessions = 3
	expectedSelectionRunnerCases          = 4
	expectedLegacyBroadeningMutations     = 2
)

type selectionRunnerSourceSession struct {
	SessionID   string `yaml:"sessionId"`
	ProjectDir  string `yaml:"projectDir"`
	ProjectName string `yaml:"projectName"`
	WorkingDir  string `yaml:"workingDir"`
	Branch      string `yaml:"branch"`
	Title       string `yaml:"title"`
}

func (s selectionRunnerSourceSession) listing() ftue.SessionListing {
	return ftue.SessionListing{
		Harness:     defaults.HarnessClaudeCode.String(),
		ProjectName: s.ProjectName,
		Branch:      s.Branch,
		Title:       s.Title,
		SessionID:   s.SessionID,
		WorkingDir:  s.WorkingDir,
	}
}

type selectionRunnerCase struct {
	Name                     string                 `yaml:"name"`
	LegacyBroadeningMutation bool                   `yaml:"legacyBroadeningMutation"`
	Selection                config.SelectionConfig `yaml:"selection"`
	WantStoredSessionIDs     []string               `yaml:"wantStoredSessionIds"`
}

type selectionRunnerDocument struct {
	ExpectedSourceSessionCount            int                            `yaml:"expectedSourceSessionCount"`
	ExpectedCaseCount                     int                            `yaml:"expectedCaseCount"`
	ExpectedLegacyBroadeningMutationCount int                            `yaml:"expectedLegacyBroadeningMutationCount"`
	SourceSessions                        []selectionRunnerSourceSession `yaml:"sourceSessions"`
	Cases                                 []selectionRunnerCase          `yaml:"cases"`
}

//go:embed testdata/kickstart_selection_runner.yaml
var selectionRunnerFixtureData []byte

func decodeSelectionRunnerFixture(data []byte) (selectionRunnerDocument, error) {
	var document selectionRunnerDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/kickstart_selection_runner.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("kickstart_selection_runner.yaml must hold exactly one document: %w", err)
	}
	if document.ExpectedSourceSessionCount != expectedSelectionRunnerSourceSessions ||
		len(document.SourceSessions) != expectedSelectionRunnerSourceSessions {
		return document, fmt.Errorf("selection runner source sessions: declared=%d actual=%d required=%d",
			document.ExpectedSourceSessionCount, len(document.SourceSessions), expectedSelectionRunnerSourceSessions)
	}
	if document.ExpectedCaseCount != expectedSelectionRunnerCases || len(document.Cases) != expectedSelectionRunnerCases {
		return document, fmt.Errorf("selection runner cases: declared=%d actual=%d required=%d",
			document.ExpectedCaseCount, len(document.Cases), expectedSelectionRunnerCases)
	}

	sourceIDs := make(map[string]bool, len(document.SourceSessions))
	for _, session := range document.SourceSessions {
		if strings.TrimSpace(session.ProjectDir) == "" || strings.TrimSpace(session.ProjectName) == "" ||
			strings.TrimSpace(session.WorkingDir) == "" || strings.TrimSpace(session.Branch) == "" ||
			strings.TrimSpace(session.Title) == "" || sourceIDs[session.SessionID] {
			return document, fmt.Errorf("selection runner source session is incomplete or duplicated: %#v", session)
		}
		if _, err := ingest.NewSessionID(session.SessionID); err != nil {
			return document, fmt.Errorf("selection runner source session %q is invalid: %w", session.SessionID, err)
		}
		sourceIDs[session.SessionID] = true
	}

	caseNames := make(map[string]bool, len(document.Cases))
	mutations := 0
	allModeControls := 0
	selectedMatches := 0
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || caseNames[row.Name] || !row.Selection.Mode.IsValid() {
			return document, fmt.Errorf("selection runner case is invalid or duplicated: %#v", row)
		}
		caseNames[row.Name] = true
		if row.LegacyBroadeningMutation {
			mutations++
			if row.Selection.Mode != config.SelectionModeSelected || len(row.WantStoredSessionIDs) != 0 {
				return document, fmt.Errorf("selection runner broadening mutation %q must be selected mode with an empty store", row.Name)
			}
		}
		if row.Selection.Mode == config.SelectionModeAll {
			allModeControls++
		}
		if row.Selection.Mode == config.SelectionModeSelected && len(row.WantStoredSessionIDs) > 0 {
			selectedMatches++
		}
		seenWanted := map[string]bool{}
		for _, id := range row.WantStoredSessionIDs {
			if !sourceIDs[id] || seenWanted[id] {
				return document, fmt.Errorf("selection runner case %q wants absent or duplicate source session %q", row.Name, id)
			}
			seenWanted[id] = true
		}
	}
	if document.ExpectedLegacyBroadeningMutationCount != expectedLegacyBroadeningMutations || mutations != expectedLegacyBroadeningMutations {
		return document, fmt.Errorf("selection runner broadening mutations: declared=%d actual=%d required=%d",
			document.ExpectedLegacyBroadeningMutationCount, mutations, expectedLegacyBroadeningMutations)
	}
	if allModeControls != 1 || selectedMatches != 1 {
		return document, fmt.Errorf("selection runner controls all/matched=%d/%d, want 1/1", allModeControls, selectedMatches)
	}
	return document, nil
}

func loadSelectionRunnerFixture(t *testing.T) selectionRunnerDocument {
	t.Helper()
	document, err := decodeSelectionRunnerFixture(selectionRunnerFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func writeSelectionRunnerSources(t *testing.T, root string, sessions []selectionRunnerSourceSession) {
	t.Helper()
	for index, session := range sessions {
		path := filepath.Join(root, session.ProjectDir, session.SessionID+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create selection runner source directory: %v", err)
		}
		user := fmt.Sprintf(
			`{"sessionId":%q,"version":"2.1.14","cwd":%q,"gitBranch":%q,"type":"user","message":{"role":"user","content":"fixture prompt"},"timestamp":"2026-08-09T12:%02d:00Z","uuid":"fixture-user-%d"}`,
			session.SessionID, session.WorkingDir, session.Branch, index, index)
		assistant := fmt.Sprintf(
			`{"sessionId":%q,"version":"2.1.14","type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"fixture reply"}]},"timestamp":"2026-08-09T12:%02d:01Z","uuid":"fixture-assistant-%d"}`,
			session.SessionID, index, index)
		if err := os.WriteFile(path, []byte(user+"\n"+assistant+"\n"), 0o600); err != nil {
			t.Fatalf("write selection runner source %q: %v", session.SessionID, err)
		}
		settled := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(path, settled, settled); err != nil {
			t.Fatalf("age selection runner source %q beyond the active-session window: %v", session.SessionID, err)
		}
	}
}

func selectionRunnerListings(sessions []selectionRunnerSourceSession) []ftue.SessionListing {
	listings := make([]ftue.SessionListing, 0, len(sessions))
	for _, session := range sessions {
		listings = append(listings, session.listing())
	}
	return listings
}

func TestKickstartLocalIngestPreservesCommittedSelectionAtRunnerBoundary(t *testing.T) {
	document := loadSelectionRunnerFixture(t)
	listings := selectionRunnerListings(document.SourceSessions)
	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			sourceRoot := filepath.Join(root, "claude-projects")
			writeSelectionRunnerSources(t, sourceRoot, document.SourceSessions)

			cfg := config.BaseConfig()
			cfg.Sources = config.SourcesConfig{
				ClaudeCode: config.SourceProviderConfig{Enabled: true, Paths: []string{sourceRoot}},
			}
			cfg.Output.BasePath = filepath.Join(root, "transcripts")
			cfg.Selection = row.Selection
			configPath := filepath.Join(root, "config.yaml")
			if err := config.SaveAtomic(configPath, cfg); err != nil {
				t.Fatalf("save selection runner config: %v", err)
			}

			cmd := &cobra.Command{Use: "selection-runner-fixture"}
			cmd.Flags().String("data-dir", root, "")
			run, _ := kickstartLocalIngest(cmd, configPath, listings)
			result, err := run(context.Background())
			if err != nil {
				t.Fatalf("run production kickstart local ingest: %v", err)
			}
			if result == nil {
				t.Fatal("production kickstart local ingest returned no result")
			}

			db, err := store.Open(defaults.ResolveDBFilePathWith(root).String())
			if err != nil {
				t.Fatalf("open selection runner store: %v", err)
			}
			stored, err := db.AllSessionIDs(context.Background())
			if closeErr := db.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
			if err != nil {
				t.Fatalf("read selection runner store: %v", err)
			}
			want := append([]string(nil), row.WantStoredSessionIDs...)
			sort.Strings(stored)
			sort.Strings(want)
			if !reflect.DeepEqual(stored, want) {
				t.Fatalf("selection runner stored sessions=%v, want exactly admitted %v", stored, want)
			}
		})
	}
}

func TestKickstartSelectedEmptyMutationStaysAllocated(t *testing.T) {
	document := loadSelectionRunnerFixture(t)
	listings := selectionRunnerListings(document.SourceSessions)
	mutations := 0
	for _, row := range document.Cases {
		if !row.LegacyBroadeningMutation {
			continue
		}
		mutations++
		cfg := config.BaseConfig()
		cfg.Selection = row.Selection
		answers := deriveKickstartAnswers(cfg, listings)
		if answers.SelectionMode != config.SelectionModeSelected {
			t.Fatalf("selection runner mutation %q lost committed mode across adapter: %q", row.Name, answers.SelectionMode)
		}
		allowed := kickstartAllowedSessionIDs(answers)
		if allowed == nil || len(allowed) != 0 {
			t.Fatalf("selection runner mutation %q allowed IDs=%v, want allocated empty set", row.Name, allowed)
		}
	}
	if mutations != expectedLegacyBroadeningMutations {
		t.Fatalf("selection runner mutation checks=%d, want %d", mutations, expectedLegacyBroadeningMutations)
	}
}

func mutateSelectionRunnerCount(t *testing.T, field string, expected int) []byte {
	t.Helper()
	declared := []byte(fmt.Sprintf("%s: %d", field, expected))
	changed := []byte(fmt.Sprintf("%s: %d", field, expected+1))
	mutated := bytes.Replace(selectionRunnerFixtureData, declared, changed, 1)
	if bytes.Equal(mutated, selectionRunnerFixtureData) {
		t.Fatalf("selection runner %s mutation did not alter the fixture", field)
	}
	return mutated
}

func TestSelectionRunnerFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), selectionRunnerFixtureData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeSelectionRunnerFixture(mutated); err == nil {
		t.Fatal("selection runner fixture accepted an unknown field")
	}
}

func TestSelectionRunnerFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), selectionRunnerFixtureData...), []byte("\n---\n{}\n")...)
	if _, err := decodeSelectionRunnerFixture(mutated); err == nil {
		t.Fatal("selection runner fixture accepted a trailing document")
	}
}

func TestSelectionRunnerFixturePinsCounts(t *testing.T) {
	assertSelectionRunnerCountMutationRejected(t, "expectedSourceSessionCount", expectedSelectionRunnerSourceSessions)
	assertSelectionRunnerCountMutationRejected(t, "expectedCaseCount", expectedSelectionRunnerCases)
	assertSelectionRunnerCountMutationRejected(t, "expectedLegacyBroadeningMutationCount", expectedLegacyBroadeningMutations)
}

func assertSelectionRunnerCountMutationRejected(t *testing.T, field string, expected int) {
	t.Helper()
	mutated := mutateSelectionRunnerCount(t, field, expected)
	if _, err := decodeSelectionRunnerFixture(mutated); err == nil {
		t.Fatalf("selection runner fixture accepted changed %s", field)
	}
}
