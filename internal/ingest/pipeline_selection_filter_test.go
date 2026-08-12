package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pipeline_selection_filter.yaml
var pipelineSelectionFilterYAML []byte

const pipelineSelectionFilterCaseCount = 3

var requiredPipelineSelectionFilterCases = []string{
	"real_run_exact_child_deny_overrides_selected_parent",
	"dry_run_uses_exact_child_deny",
	"dry_run_preserves_rejected_parent_inheritance",
}

type pipelineSelectionFilterDocument struct {
	DeclaredCases int                           `yaml:"declared_cases"`
	Cases         []pipelineSelectionFilterCase `yaml:"cases"`
}

type pipelineSelectionFilterCase struct {
	Name               string                           `yaml:"name"`
	DryRun             bool                             `yaml:"dry_run"`
	ParentSessionID    string                           `yaml:"parent_session_id"`
	ChildSessionID     string                           `yaml:"child_session_id"`
	SelectedRoots      []string                         `yaml:"selected_roots"`
	ExcludedSessions   []string                         `yaml:"excluded_sessions"`
	ExpectedStatuses   map[string]fixturePipelineStatus `yaml:"expected_statuses"`
	ExpectedWrittenIDs []string                         `yaml:"expected_written"`
}

type fixturePipelineStatus string

const (
	fixturePipelineNew       fixturePipelineStatus = "new"
	fixturePipelineUnchanged fixturePipelineStatus = "unchanged"
)

func decodePipelineSelectionFilterFixture(source []byte) (pipelineSelectionFilterDocument, error) {
	var document pipelineSelectionFilterDocument
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode pipeline selection-filter fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, fmt.Errorf("pipeline selection-filter fixture must contain exactly one YAML document: %v", err)
	}
	if document.DeclaredCases != pipelineSelectionFilterCaseCount || len(document.Cases) != pipelineSelectionFilterCaseCount {
		return document, fmt.Errorf("pipeline selection-filter fixture case count mismatch: declared=%d actual=%d required=%d", document.DeclaredCases, len(document.Cases), pipelineSelectionFilterCaseCount)
	}
	seen := make(map[string]bool, len(document.Cases))
	for index, testCase := range document.Cases {
		if testCase.Name == "" || seen[testCase.Name] {
			return document, fmt.Errorf("pipeline selection-filter fixture cases[%d] has an empty or duplicate name %q", index, testCase.Name)
		}
		seen[testCase.Name] = true
		if _, err := ingest.NewSessionID(testCase.ParentSessionID); err != nil {
			return document, fmt.Errorf("pipeline selection-filter fixture case %q has invalid parent_session_id: %w", testCase.Name, err)
		}
		if _, err := ingest.NewSessionID(testCase.ChildSessionID); err != nil {
			return document, fmt.Errorf("pipeline selection-filter fixture case %q has invalid child_session_id: %w", testCase.Name, err)
		}
		if len(testCase.ExpectedStatuses) != 2 {
			return document, fmt.Errorf("pipeline selection-filter fixture case %q must declare exactly two expected statuses", testCase.Name)
		}
		for _, sessionID := range []string{testCase.ParentSessionID, testCase.ChildSessionID} {
			status, ok := testCase.ExpectedStatuses[sessionID]
			if !ok || (status != fixturePipelineNew && status != fixturePipelineUnchanged) {
				return document, fmt.Errorf("pipeline selection-filter fixture case %q has missing or unknown status %q for session %s", testCase.Name, status, sessionID)
			}
		}
		if testCase.DryRun && len(testCase.ExpectedWrittenIDs) != 0 {
			return document, fmt.Errorf("pipeline selection-filter fixture case %q is dry-run but expects files to be written", testCase.Name)
		}
	}
	for _, required := range requiredPipelineSelectionFilterCases {
		if !seen[required] {
			return document, fmt.Errorf("pipeline selection-filter fixture is missing required case %q", required)
		}
	}
	return document, nil
}

func loadPipelineSelectionFilterFixture(t *testing.T) pipelineSelectionFilterDocument {
	t.Helper()
	document, err := decodePipelineSelectionFilterFixture(pipelineSelectionFilterYAML)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestPipelineSelectionFilterFixtureRejectsSemanticMutation(t *testing.T) {
	mutated := bytes.Replace(
		pipelineSelectionFilterYAML,
		[]byte("name: dry_run_uses_exact_child_deny"),
		[]byte("name: renamed_dry_run_case"),
		1,
	)
	if _, err := decodePipelineSelectionFilterFixture(mutated); err == nil {
		t.Fatal("a count-preserving pipeline selection-filter case rename unexpectedly validated")
	}
}

func TestPipeline_SessionFilterExactChildDenialAndDryRunUseSharedPass(t *testing.T) {
	for _, testCase := range loadPipelineSelectionFilterFixture(t).Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			parentSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testCase.ParentSessionID)
			childSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testCase.ChildSessionID)
			setupSourceFile(t, mfs, parentSource)
			setupSourceFile(t, mfs, childSource)
			parent := makeDiscoveredSession(t, testCase.ParentSessionID, parentSource, time.Now().Add(-time.Hour))
			child := makeDiscoveredSession(t, testCase.ChildSessionID, childSource, time.Now().Add(-time.Hour))
			child.ParentUUID = &parent.SessionID
			sessions := []ingest.DiscoveredSession{parent, child}
			metadata := map[ingest.SessionID]*ingest.UnifiedMetadata{
				parent.SessionID: makeMinimalMeta(t, testCase.ParentSessionID),
				child.SessionID:  makeMinimalMeta(t, testCase.ChildSessionID),
			}

			selectedRoots := make(map[ingest.SessionID]bool, len(testCase.SelectedRoots))
			for _, raw := range testCase.SelectedRoots {
				selectedRoots[ingest.SessionID(raw)] = true
			}
			excluded := make(map[ingest.SessionID]bool, len(testCase.ExcludedSessions))
			for _, raw := range testCase.ExcludedSessions {
				excluded[ingest.SessionID(raw)] = true
			}
			prepared := false
			cfg := makePipelineConfig(testOutputDir, func(cfg *ingest.PipelineConfig) {
				cfg.DryRun = testCase.DryRun
				cfg.PrepareSessionFilter = func(_ context.Context, cohort []ingest.DiscoveredSession) error {
					if len(cohort) != len(sessions) {
						return fmt.Errorf("prepared cohort has %d sessions, want %d", len(cohort), len(sessions))
					}
					prepared = true
					return nil
				}
				cfg.SessionFilter = func(session ingest.DiscoveredSession) bool {
					if !prepared {
						t.Fatalf("positive session filter ran before cohort preparation")
					}
					return selectedRoots[session.SessionID]
				}
				cfg.SessionExclusionFilter = func(session ingest.DiscoveredSession) bool {
					if !prepared {
						t.Fatalf("exact session filter ran before cohort preparation")
					}
					return excluded[session.SessionID]
				}
			})
			pipeline, err := ingest.NewPipeline(
				mfs,
				testutil.DefaultGitResolver(),
				map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(sessions, metadata)},
				cfg,
			)
			if err != nil {
				t.Fatalf("NewPipeline: %v", err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			gotStatuses := make(map[string]fixturePipelineStatus, len(result.Sessions))
			for _, session := range result.Sessions {
				switch session.Status {
				case ingest.DiffNew:
					gotStatuses[session.SessionID.String()] = fixturePipelineNew
				case ingest.DiffUnchanged:
					gotStatuses[session.SessionID.String()] = fixturePipelineUnchanged
				default:
					t.Fatalf("session %s returned unexpected status %v", session.SessionID, session.Status)
				}
			}
			if len(gotStatuses) != len(testCase.ExpectedStatuses) {
				t.Fatalf("pipeline returned %d session statuses, want %d", len(gotStatuses), len(testCase.ExpectedStatuses))
			}
			for sessionID, want := range testCase.ExpectedStatuses {
				if got := gotStatuses[sessionID]; got != want {
					t.Errorf("session %s status = %q, want %q", sessionID, got, want)
				}
			}

			for _, session := range sessions {
				parentID := ""
				if session.ParentUUID != nil {
					parentID = session.ParentUUID.String()
				}
				metadataPath := ingest.SessionMetadataPath(testOutputDir, testutil.TestHostSlug, session.SessionID.String(), parentID)
				_, statErr := mfs.Stat(metadataPath)
				wantWritten := slices.Contains(testCase.ExpectedWrittenIDs, session.SessionID.String())
				if wantWritten && statErr != nil {
					t.Errorf("session %s metadata was not written: %v", session.SessionID, statErr)
				}
				if !wantWritten && statErr == nil {
					t.Errorf("session %s metadata was written despite the fixture boundary", session.SessionID)
				}
			}
		})
	}
}
