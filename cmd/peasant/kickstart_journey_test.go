package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type receiptRetryStore struct{ closed *int }

func (s *receiptRetryStore) AllPushableSessions(context.Context) ([]ingest.PushSessionRow, error) {
	return nil, nil
}
func (s *receiptRetryStore) Publication(context.Context, string, string, schema.ProjectHash, string) (*store.PublicationRecord, error) {
	return nil, nil
}
func (s *receiptRetryStore) Close() error { *s.closed++; return nil }

//go:embed testdata/kickstart_journey.yaml
var kickstartJourneyYAML []byte

//go:embed testdata/kickstart_publication_failures.yaml
var kickstartPublicationFailuresYAML []byte

type kickstartJourneyDocument struct {
	DeclaredRows int                       `yaml:"declaredRows"`
	Cases        []kickstartJourneyFixture `yaml:"cases"`
}

type kickstartJourneyFixture struct {
	Name             string                `yaml:"name"`
	CancelBeforeRun  bool                  `yaml:"cancelBeforeRun"`
	SessionID        string                `yaml:"sessionId"`
	SiblingSessionID string                `yaml:"siblingSessionId"`
	RetryStage       ftue.ExecutionStage   `yaml:"retryStage"`
	ExpectedStages   []ftue.ExecutionStage `yaml:"expectedStages"`
}

type kickstartPublicationFailureDocument struct {
	DeclaredRows int                                  `yaml:"declaredRows"`
	Cases        []kickstartPublicationFailureFixture `yaml:"cases"`
}

type kickstartPublicationFailureFixture struct {
	Name                        string                              `yaml:"name"`
	SelectedSessionIDs          []string                            `yaml:"selectedSessionIds"`
	PersistedSessionIDs         []string                            `yaml:"persistedSessionIds"`
	FailedSessions              []kickstartPublicationFailedSession `yaml:"failedSessions"`
	RunError                    string                              `yaml:"runError"`
	ExpectedPersistedSessionIDs []string                            `yaml:"expectedPersistedSessionIds"`
	ExpectedRetrySessionIDs     []string                            `yaml:"expectedRetrySessionIds"`
	ExpectedErrorContains       []string                            `yaml:"expectedErrorContains"`
	ExpectedErrorExcludes       []string                            `yaml:"expectedErrorExcludes"`
}

type kickstartPublicationFailedSession struct {
	SessionID string `yaml:"sessionId"`
	Error     string `yaml:"error"`
}

func loadKickstartJourneyFixtures(raw []byte) ([]kickstartJourneyFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var document kickstartJourneyDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode kickstart journey fixture: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("kickstart journey fixture must contain exactly one YAML document")
	}
	if document.DeclaredRows != len(document.Cases) || document.DeclaredRows < 2 {
		return nil, fmt.Errorf("kickstart journey fixture row count is not guarded")
	}
	seen := map[string]bool{}
	for _, row := range document.Cases {
		if row.Name == "" || seen[row.Name] || row.SessionID == "" || len(row.ExpectedStages) == 0 {
			return nil, fmt.Errorf("kickstart journey fixture contains a blank, duplicate, or vacuous row %q", row.Name)
		}
		seen[row.Name] = true
	}
	return document.Cases, nil
}

func loadKickstartPublicationFailureFixtures(raw []byte) ([]kickstartPublicationFailureFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var document kickstartPublicationFailureDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode kickstart publication failure fixture: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("kickstart publication failure fixture must contain exactly one YAML document")
	}
	if document.DeclaredRows != len(document.Cases) || document.DeclaredRows != 4 {
		return nil, fmt.Errorf("kickstart publication failure fixture must define exactly four guarded rows")
	}
	seen := map[string]bool{}
	for _, row := range document.Cases {
		if row.Name == "" || seen[row.Name] || len(row.SelectedSessionIDs) == 0 || len(row.ExpectedRetrySessionIDs) == 0 || len(row.ExpectedErrorContains) == 0 || len(row.ExpectedErrorExcludes) == 0 || (row.RunError == "" && len(row.FailedSessions) == 0) {
			return nil, fmt.Errorf("kickstart publication failure fixture contains a blank, duplicate, or vacuous row %q", row.Name)
		}
		seen[row.Name] = true
	}
	return document.Cases, nil
}

func TestMountedKickstartJourneyProductionPath(t *testing.T) {
	fixtures, err := loadKickstartJourneyFixtures(kickstartJourneyYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			cmd := buildTestRootCmd()
			cmd.SetContext(t.Context())
			cmd.Flags().String("config-dir", dir, "")
			cmd.Flags().String("data-dir", filepath.Join(dir, "data"), "")
			ingested := 0
			var ingestedIDs []string
			runner := buildKickstartJourneyRunner(cmd, configPath, config.BaseConfig(), nil, false, func(_ context.Context, got ftue.WizardAnswers) (*ftue.IngestResult, error) {
				ingested++
				for _, session := range got.EffectiveSelectedSessions() {
					ingestedIDs = append(ingestedIDs, session.SessionID)
				}
				return &ftue.IngestResult{New: 1}, nil
			})
			ctx := t.Context()
			if fixture.CancelBeforeRun {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			answers := ftue.WizardAnswers{FinalConsent: true, WantImport: true, Destination: ftue.DestinationLocal, RedactionLevel: redact.Standard.String(), ProviderSelections: []ftue.ProviderSelection{{Harness: defaults.HarnessClaudeCode.String()}}, SelectedSessions: []ftue.SessionListing{{Harness: defaults.HarnessClaudeCode.String(), SessionID: fixture.SessionID}, {Harness: defaults.HarnessClaudeCode.String(), SessionID: fixture.SiblingSessionID}}}
			request := ftue.JourneyRequest{Answers: answers}
			if fixture.RetryStage != "" {
				request.RetryTargets = []ftue.RetryTarget{{Stage: fixture.RetryStage, SessionIDs: []string{fixture.SessionID}}}
			}
			result, runErr := runner.Run(ctx, request)
			if runErr != nil {
				t.Fatalf("mounted journey: %v", runErr)
			}
			if len(result.Effects) != len(fixture.ExpectedStages) {
				t.Fatalf("effects=%+v want stages=%v", result.Effects, fixture.ExpectedStages)
			}
			for i, stage := range fixture.ExpectedStages {
				if result.Effects[i].Stage != stage {
					t.Fatalf("effect[%d].stage=%s want %s", i, result.Effects[i].Stage, stage)
				}
			}
			if fixture.CancelBeforeRun {
				if ingested != 0 {
					t.Fatal("cancelled journey ingested a session")
				}
				return
			}
			if ingested != 1 {
				t.Fatalf("ingest calls=%d want 1", ingested)
			}
			if fixture.RetryStage == ftue.StageIngest && (len(ingestedIDs) != 1 || ingestedIDs[0] != fixture.SessionID) {
				t.Fatalf("targeted ingest received IDs %v", ingestedIDs)
			}
			if fixture.RetryStage == "" {
				if _, err := os.Stat(configPath); err != nil {
					t.Fatalf("saved config: %v", err)
				}
			}
		})
	}
}

func TestKickstartJourneyFixtureStrictness(t *testing.T) {
	if _, err := loadKickstartJourneyFixtures(append(kickstartJourneyYAML, []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("accepted second YAML document")
	}
	if _, err := loadKickstartJourneyFixtures(bytes.Replace(kickstartJourneyYAML, []byte("declaredRows:"), []byte("unknown: true\ndeclaredRows:"), 1)); err == nil {
		t.Fatal("accepted unknown YAML field")
	}
}

func TestKickstartPublicationFailureReportsActualCause(t *testing.T) {
	fixtures, err := loadKickstartPublicationFailureFixtures(kickstartPublicationFailuresYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			result := &push.PushResult{}
			for _, sessionID := range fixture.PersistedSessionIDs {
				result.Sessions = append(result.Sessions, push.SessionPushResult{SessionID: sessionID, Status: push.PushStatusNew})
			}
			for _, failed := range fixture.FailedSessions {
				var sessionErr error
				if failed.Error != "" {
					sessionErr = errors.New(failed.Error)
				}
				result.Sessions = append(result.Sessions, push.SessionPushResult{SessionID: failed.SessionID, Status: push.PushStatusError, Error: sessionErr})
			}
			var runErr error
			if fixture.RunError != "" {
				runErr = errors.New(fixture.RunError)
			}
			effects, retries, gotErr := classifyKickstartPublication(fixture.SelectedSessionIDs, result, runErr)
			if gotErr == nil {
				t.Fatal("publication failure unexpectedly returned nil")
			}
			gotPersisted := make([]string, 0, len(effects))
			for _, effect := range effects {
				gotPersisted = append(gotPersisted, effect.SessionID)
			}
			if fmt.Sprint(gotPersisted) != fmt.Sprint(fixture.ExpectedPersistedSessionIDs) {
				t.Errorf("persisted session IDs = %v, want %v", gotPersisted, fixture.ExpectedPersistedSessionIDs)
			}
			if len(retries) != 1 || fmt.Sprint(retries[0].SessionIDs) != fmt.Sprint(fixture.ExpectedRetrySessionIDs) {
				t.Errorf("publication retries = %+v, want session IDs %v", retries, fixture.ExpectedRetrySessionIDs)
			}
			for _, want := range fixture.ExpectedErrorContains {
				if !strings.Contains(gotErr.Error(), want) {
					t.Errorf("error %q does not contain %q", gotErr, want)
				}
			}
			for _, forbidden := range fixture.ExpectedErrorExcludes {
				if strings.Contains(gotErr.Error(), forbidden) {
					t.Errorf("error %q contains forbidden text %q", gotErr, forbidden)
				}
			}
		})
	}
}

func TestKickstartPublicationFailureFixtureStrictness(t *testing.T) {
	if _, err := loadKickstartPublicationFailureFixtures(append(kickstartPublicationFailuresYAML, []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("accepted second YAML document")
	}
	if _, err := loadKickstartPublicationFailureFixtures(bytes.Replace(kickstartPublicationFailuresYAML, []byte("declaredRows:"), []byte("unknown: true\ndeclaredRows:"), 1)); err == nil {
		t.Fatal("accepted unknown YAML field")
	}
}

func TestBlockedHookDiagnosticNeverFormatsNilError(t *testing.T) {
	err := hookInstallFailure("/tmp/disposable-repository", true, nil)
	if strings.Contains(err.Error(), "<nil>") || !strings.Contains(err.Error(), "occupied by content Peasant does not own") || !strings.Contains(err.Error(), "manual integration instructions") {
		t.Fatalf("blocked-hook diagnostic is not actionable: %v", err)
	}
}

func TestReceiptRetryReopensAndClosesStorePerRun(t *testing.T) {
	opened, closed := 0, 0
	cmd := buildTestRootCmd()
	runner := buildKickstartJourneyRunnerWithDeps(cmd, "/tmp/config.yaml", config.BaseConfig(), nil, false, nil, kickstartJourneyDeps{
		loadCredentials: func(string) (*auth.Credentials, error) {
			return &auth.Credentials{APIKey: "test-key", KeyID: "test-key-id", UserID: "test-owner", Username: "test-user", VillageURL: "https://village.test"}, nil
		},
		openReceiptStore: func(string) (kickstartReceiptStore, error) { opened++; return &receiptRetryStore{closed: &closed}, nil },
	})
	request := ftue.JourneyRequest{Answers: ftue.WizardAnswers{Destination: ftue.DestinationVillage}, RetryTargets: []ftue.RetryTarget{{Stage: ftue.StageReceipt, SessionIDs: []string{"session-retry"}}}}
	for range 2 {
		if _, err := runner.Run(t.Context(), request); err == nil {
			t.Fatal("receipt retry unexpectedly succeeded without a persisted identity")
		}
	}
	if opened != 2 || closed != 2 {
		t.Fatalf("receipt retry store lifecycle opened=%d closed=%d, want 2/2", opened, closed)
	}
}
