package main

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/kickstart_journey_diagnostics.yaml
var kickstartJourneyDiagnosticsYAML []byte

func TestKickstartJourneyPreservesWarningEvidenceAcrossRetries(t *testing.T) {
	var fixture struct {
		RequiredNames []string               `yaml:"requiredNames"`
		PriorWarning  ingest.DiagnosticEntry `yaml:"priorWarning"`
		Cases         []struct {
			Name              string                   `yaml:"name"`
			RetryStage        ftue.ExecutionStage      `yaml:"retryStage"`
			PriorWarnings     []ingest.DiagnosticEntry `yaml:"priorWarnings"`
			CurrentWarnings   []ingest.DiagnosticEntry `yaml:"currentWarnings"`
			WantWarnings      []ingest.DiagnosticEntry `yaml:"wantWarnings"`
			WantIngestCalls   int                      `yaml:"wantIngestCalls"`
			WantAggregateOnly bool                     `yaml:"wantAggregateOnly"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(kickstartJourneyDiagnosticsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("journey diagnostic fixture must contain exactly one document")
	}
	required := []string{"first-attempt-refusal", "retry-other-stage", "partial-ingest-retry", "repeated-warning-deduplicated"}
	if !reflect.DeepEqual(fixture.RequiredNames, required) {
		t.Fatal("required journey diagnostic manifest changed")
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatal("invalid journey diagnostic name")
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			calls := 0
			runner := buildKickstartJourneyRunnerWithDeps(&cobra.Command{Use: "kickstart"}, "", config.BaseConfig(), nil, false, func(context.Context, ftue.WizardAnswers) (*ftue.IngestResult, error) {
				calls++
				return &ftue.IngestResult{Unchanged: 1, Diagnostics: row.CurrentWarnings}, nil
			}, kickstartJourneyDeps{})
			request := ftue.JourneyRequest{
				Answers:          ftue.WizardAnswers{FinalConsent: true, WantImport: true, Destination: ftue.DestinationLocal, SelectedSessions: []ftue.SessionListing{{SessionID: "retried-session", Harness: "claude-code"}}},
				PriorEffects:     []ftue.PersistedEffect{{Stage: ftue.StageConfig, Status: ftue.StatusPersisted}},
				PriorDiagnostics: row.PriorWarnings,
				RetryTargets:     []ftue.RetryTarget{{Stage: row.RetryStage, SessionIDs: []string{"retried-session"}}},
			}
			result, err := runner.Run(t.Context(), request)
			if err != nil {
				t.Fatalf("nonfatal warning blocked journey: %v", err)
			}
			if calls != row.WantIngestCalls || !reflect.DeepEqual(result.Diagnostics, row.WantWarnings) {
				t.Fatalf("calls=%d diagnostics=%+v want=%+v", calls, result.Diagnostics, row.WantWarnings)
			}
			if len(result.Retry) != 0 {
				t.Fatalf("warnings became fatal retry targets: %+v", result.Retry)
			}
			if row.WantAggregateOnly {
				for _, effect := range result.Effects {
					if effect.Stage == ftue.StageIngest && (effect.SessionID != "" || effect.Status != ftue.StatusSkipped) {
						t.Fatalf("refusal claimed requested session persisted: %+v", effect)
					}
				}
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing journey diagnostic fixture %q", name)
		}
	}
}
