package ftue_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/journey_contract.yaml
var journeyContractYAML []byte

//go:embed testdata/journey_retry.yaml
var journeyRetryYAML []byte

type journeyRetryDocument struct {
	DeclaredRows int                   `yaml:"declaredRows"`
	RequiredArms []string              `yaml:"requiredArms"`
	Cases        []journeyRetryFixture `yaml:"cases"`
}

type journeyRetryFixture struct {
	Name                 string                `yaml:"name"`
	Arm                  string                `yaml:"arm"`
	RetryStage           ftue.ExecutionStage   `yaml:"retryStage"`
	SessionIDs           []string              `yaml:"sessionIds"`
	Repository           string                `yaml:"repository"`
	Events               []githooks.Event      `yaml:"events"`
	AdditionalRetryStage ftue.ExecutionStage   `yaml:"additionalRetryStage"`
	AdditionalSessionIDs []string              `yaml:"additionalSessionIds"`
	ExpectedStages       []ftue.ExecutionStage `yaml:"expectedStages"`
}

func loadJourneyRetryFixtures(raw []byte) ([]journeyRetryFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var document journeyRetryDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode journey retry fixture: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("journey retry fixture must contain exactly one YAML document")
	}
	if document.DeclaredRows != len(document.Cases) || document.DeclaredRows < 4 {
		return nil, fmt.Errorf("journey retry fixture row count is not guarded")
	}
	seen := map[string]bool{}
	arms := map[string]bool{}
	for _, row := range document.Cases {
		target := ftue.RetryTarget{Stage: row.RetryStage, SessionIDs: row.SessionIDs, Repository: row.Repository, Events: row.Events}
		if row.Name == "" || row.Arm == "" || seen[row.Name] || len(row.ExpectedStages) == 0 || target.Validate() != nil {
			return nil, fmt.Errorf("journey retry fixture contains invalid row %q", row.Name)
		}
		seen[row.Name] = true
		arms[row.Arm] = true
		if row.AdditionalRetryStage != "" {
			if err := (ftue.RetryTarget{Stage: row.AdditionalRetryStage, SessionIDs: row.AdditionalSessionIDs}).Validate(); err != nil {
				return nil, fmt.Errorf("journey retry fixture has invalid additional target in %q: %w", row.Name, err)
			}
		}
	}
	for _, arm := range document.RequiredArms {
		if !arms[arm] {
			return nil, fmt.Errorf("journey retry fixture does not exercise required arm %q", arm)
		}
	}
	return document.Cases, nil
}

type journeyFixtureDocument struct {
	DeclaredRows int              `yaml:"declaredRows"`
	RequiredArms []string         `yaml:"requiredArms"`
	Cases        []journeyFixture `yaml:"cases"`
}

type journeyFixture struct {
	ID                  string                 `yaml:"id"`
	Arm                 string                 `yaml:"arm"`
	Destination         ftue.Destination       `yaml:"destination"`
	RequestedVisibility schema.Visibility      `yaml:"requestedVisibility"`
	Effects             []ftue.PersistedEffect `yaml:"effects"`
	Retry               []ftue.RetryTarget     `yaml:"retry"`
	Valid               bool                   `yaml:"valid"`
	OperationError      string                 `yaml:"operationError"`
}

func loadJourneyFixtures(raw []byte) ([]journeyFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var document journeyFixtureDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode journey contract fixture: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("journey contract fixture must contain exactly one YAML document")
	}
	if document.DeclaredRows != len(document.Cases) || document.DeclaredRows < 7 {
		return nil, fmt.Errorf("journey contract row count = declared %d actual %d; need at least 7", document.DeclaredRows, len(document.Cases))
	}
	ids := make(map[string]bool, len(document.Cases))
	arms := make(map[string]bool, len(document.Cases))
	for _, fixture := range document.Cases {
		if fixture.ID == "" || ids[fixture.ID] || fixture.Arm == "" || !fixture.Destination.IsValid() || len(fixture.Effects) == 0 {
			return nil, fmt.Errorf("journey contract fixture has blank, duplicate, or vacuous row %q", fixture.ID)
		}
		ids[fixture.ID] = true
		arms[fixture.Arm] = true
	}
	for _, arm := range document.RequiredArms {
		if !arms[arm] {
			return nil, fmt.Errorf("journey contract fixture does not exercise required arm %q", arm)
		}
	}
	return document.Cases, nil
}

func TestJourneyResultContract(t *testing.T) {
	fixtures, err := loadJourneyFixtures(journeyContractYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.ID, func(t *testing.T) {
			answers := ftue.WizardAnswers{Destination: fixture.Destination, RequestedVisibility: fixture.RequestedVisibility}
			runner := ftue.JourneyRunnerFunc(func(ctx context.Context, request ftue.JourneyRequest) (ftue.JourneyResult, error) {
				if ctx == nil || request.Answers.Destination != fixture.Destination {
					t.Fatal("runner did not receive the mounted consent snapshot and context")
				}
				return ftue.JourneyResult{Effects: fixture.Effects, Retry: fixture.Retry}, nil
			})
			result, runErr := runner.Run(t.Context(), ftue.JourneyRequest{Answers: answers})
			if runErr != nil {
				t.Fatal(runErr)
			}
			if gotValid := result.Validate() == nil; gotValid != fixture.Valid {
				t.Fatalf("Validate() success=%v want %v: %+v", gotValid, fixture.Valid, result)
			}
		})
	}
}

func TestJourneyFixtureStrictnessAndMutation(t *testing.T) {
	if _, err := loadJourneyFixtures(append(journeyContractYAML, []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("loader accepted a second YAML document")
	}
	if _, err := loadJourneyFixtures(bytes.Replace(journeyContractYAML, []byte("declaredRows:"), []byte("unknown: true\ndeclaredRows:"), 1)); err == nil {
		t.Fatal("loader accepted an unknown field")
	}
	mutated := bytes.Replace(journeyContractYAML, []byte("arm: cancelled"), []byte("arm: local"), 1)
	if _, err := loadJourneyFixtures(mutated); err == nil {
		t.Fatal("loader accepted a corpus missing the cancellation arm")
	}
	if _, err := loadJourneyRetryFixtures(append(journeyRetryYAML, []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("retry loader accepted another YAML document")
	}
	mutatedRetry := bytes.Replace(journeyRetryYAML, []byte("arm: exact-hook"), []byte("arm: resume"), 1)
	if _, err := loadJourneyRetryFixtures(mutatedRetry); err == nil {
		t.Fatal("retry loader accepted a corpus missing the exact-hook arm")
	}
}

func TestPersistedReceiptRequiresCompleteAuthoritativeState(t *testing.T) {
	fixtures, err := loadJourneyFixtures(journeyContractYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		if fixture.Arm != "private" {
			continue
		}
		mutated := fixture.Effects[0]
		mutated.Receipt = nil
		if err := (ftue.JourneyResult{Effects: []ftue.PersistedEffect{mutated}}).Validate(); err == nil {
			t.Fatal("persisted receipt validation accepted missing authoritative state")
		}
		mutated = fixture.Effects[0]
		mutated.OwnerUserID = ""
		if err := (ftue.JourneyResult{Effects: []ftue.PersistedEffect{mutated}}).Validate(); err == nil {
			t.Fatal("persisted receipt validation accepted incomplete publication identity")
		}
	}
}

func TestJourneyRunnerUsesOneCancellationBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runner := ftue.JourneyRunnerFunc(func(runCtx context.Context, _ ftue.JourneyRequest) (ftue.JourneyResult, error) {
		return ftue.JourneyResult{}, runCtx.Err()
	})
	if _, err := runner.Run(ctx, ftue.JourneyRequest{}); err != context.Canceled {
		t.Fatalf("runner cancellation error=%v want context.Canceled", err)
	}
}

func TestOrderedJourneyRunnerRejectsFailedStageClaims(t *testing.T) {
	fixtures, err := loadJourneyFixtures(journeyContractYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		if fixture.OperationError == "" {
			continue
		}
		t.Run(fixture.ID, func(t *testing.T) {
			runner := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
				ftue.StagePublication: func(context.Context, ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
					return fixture.Effects, fixture.Retry, errors.New(fixture.OperationError)
				},
			}}
			_, runErr := runner.Run(t.Context(), ftue.JourneyRequest{RetryTargets: []ftue.RetryTarget{{Stage: ftue.StagePublication, SessionIDs: []string{"session-failed-claim"}}}})
			if runErr == nil || !strings.Contains(runErr.Error(), "validate journey result after failure at stage \"publication\"") {
				t.Fatalf("runner error=%v; want actionable failed-stage validation rejection", runErr)
			}
		})
	}
}

func TestOrderedJourneyRunnerResumesAtFailedWork(t *testing.T) {
	fixtures, err := loadJourneyRetryFixtures(journeyRetryYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			called := []ftue.ExecutionStage{}
			operations := map[ftue.ExecutionStage]ftue.StageOperation{}
			for _, stage := range []ftue.ExecutionStage{ftue.StageConfig, ftue.StageIngest, ftue.StagePublication, ftue.StageReceipt, ftue.StageHooks} {
				stage := stage
				operations[stage] = func(_ context.Context, request ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
					called = append(called, stage)
					if stage == fixture.RetryStage && stage != ftue.StageHooks && fmt.Sprint(request.SessionFilter) != fmt.Sprint(fixture.SessionIDs) {
						t.Fatal("operation lost the exact session retry filter")
					}
					if stage == fixture.AdditionalRetryStage && fmt.Sprint(request.SessionFilter) != fmt.Sprint(fixture.AdditionalSessionIDs) {
						want := append(append([]string(nil), fixture.SessionIDs...), fixture.AdditionalSessionIDs...)
						if fmt.Sprint(request.SessionFilter) != fmt.Sprint(want) {
							t.Fatal("operation lost the union of earlier and exact retry filters")
						}
					}
					if stage == ftue.StageHooks && fixture.RetryStage == ftue.StageHooks && request.HookFilter == nil {
						t.Fatal("operation lost the exact hook retry filter")
					}
					return []ftue.PersistedEffect{{Stage: stage, Status: ftue.StatusSkipped}}, nil, nil
				}
			}
			targets := []ftue.RetryTarget{{Stage: fixture.RetryStage, SessionIDs: fixture.SessionIDs, Repository: fixture.Repository, Events: fixture.Events}}
			if fixture.AdditionalRetryStage != "" {
				targets = append(targets, ftue.RetryTarget{Stage: fixture.AdditionalRetryStage, SessionIDs: fixture.AdditionalSessionIDs})
			}
			prior := []ftue.PersistedEffect{{Stage: ftue.StageConfig, Status: ftue.StatusPersisted}}
			result, err := (ftue.OrderedJourneyRunner{Operations: operations}).Run(t.Context(), ftue.JourneyRequest{RetryTargets: targets, PriorEffects: prior})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Effects) == 0 || result.Effects[0].Stage != ftue.StageConfig {
				t.Fatal("retry discarded the prior persisted checkpoint")
			}
			if fmt.Sprint(called) != fmt.Sprint(fixture.ExpectedStages) {
				t.Fatalf("called stages=%v want %v", called, fixture.ExpectedStages)
			}
		})
	}
}

func TestOrderedJourneyRunnerClassifiesOperationCancellation(t *testing.T) {
	prior := []ftue.PersistedEffect{{Stage: ftue.StageConfig, Status: ftue.StatusPersisted}}
	ctx, cancel := context.WithCancel(t.Context())
	runner := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
		ftue.StageIngest: func(context.Context, ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			cancel()
			return []ftue.PersistedEffect{{Stage: ftue.StageIngest, Status: ftue.StatusPersisted, SessionID: "session-complete"}}, nil, context.Canceled
		},
	}}

	requestedSessions := []string{"session-complete", "session-pending"}
	result, err := runner.Run(ctx, ftue.JourneyRequest{PriorEffects: prior, RetryTargets: []ftue.RetryTarget{{Stage: ftue.StageIngest, SessionIDs: requestedSessions}}})
	if err != nil {
		t.Fatalf("operation cancellation returned an execution failure: %v", err)
	}
	if len(result.Effects) != 3 || result.Effects[0] != prior[0] || result.Effects[1].Status != ftue.StatusPersisted || result.Effects[2].Status != ftue.StatusCancelled {
		t.Fatalf("cancellation did not preserve prior and operation effects: %+v", result.Effects)
	}
	if len(result.Retry) != 2 || result.Retry[0].Stage != ftue.StageIngest || fmt.Sprint(result.Retry[0].SessionIDs) != fmt.Sprint([]string{"session-pending"}) || result.Retry[1].Stage != ftue.StagePublication || fmt.Sprint(result.Retry[1].SessionIDs) != fmt.Sprint([]string{"session-complete"}) {
		t.Fatalf("cancellation replayed a session with durable same-stage evidence: %+v", result.Retry)
	}
}

func TestOrderedJourneyRunnerPreservesTargetWithoutDurableIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	runner := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
		ftue.StageIngest: func(context.Context, ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			cancel()
			return []ftue.PersistedEffect{{Stage: ftue.StageIngest, Status: ftue.StatusSkipped}}, nil, context.Canceled
		},
	}}
	want := []string{"session-one", "session-two"}
	result, err := runner.Run(ctx, ftue.JourneyRequest{RetryTargets: []ftue.RetryTarget{{Stage: ftue.StageIngest, SessionIDs: want}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Retry) != 1 || fmt.Sprint(result.Retry[0].SessionIDs) != fmt.Sprint(want) {
		t.Fatalf("identity-free durable evidence narrowed the requested target: %+v", result.Retry)
	}
}

func TestOrderedJourneyRunnerAdvancesFullyCompletedCancelledStage(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	runner := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
		ftue.StagePublication: func(context.Context, ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			cancel()
			return []ftue.PersistedEffect{{Stage: ftue.StagePublication, Status: ftue.StatusSkipped, SessionID: "session-complete"}}, nil, context.Canceled
		},
	}}

	result, err := runner.Run(ctx, ftue.JourneyRequest{RetryTargets: []ftue.RetryTarget{{Stage: ftue.StagePublication, SessionIDs: []string{"session-complete"}}}})
	if err != nil {
		t.Fatalf("completed interrupted stage could not resume downstream: %v", err)
	}
	if len(result.Retry) != 1 || result.Retry[0].Stage != ftue.StageReceipt || fmt.Sprint(result.Retry[0].SessionIDs) != fmt.Sprint([]string{"session-complete"}) {
		t.Fatalf("completed interrupted stage was replayed instead of advancing: %+v", result.Retry)
	}
	if len(result.Effects) != 2 || result.Effects[0].Status != ftue.StatusSkipped || result.Effects[1].Status != ftue.StatusCancelled {
		t.Fatalf("completed interrupted stage lost durable or cancelled evidence: %+v", result.Effects)
	}
}

func TestOrderedJourneyRunnerSubtractsDurableHookEvidenceOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	target := ftue.RetryTarget{Stage: ftue.StageHooks, Repository: "/tmp/repository", Events: []githooks.Event{githooks.EventPostCommit, githooks.EventPrePush}}
	runner := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
		ftue.StageHooks: func(context.Context, ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			cancel()
			return []ftue.PersistedEffect{{Stage: ftue.StageHooks, Status: ftue.StatusPersisted, Repository: "/canonical/repository", RetryRepository: target.Repository, HookEvent: githooks.EventPostCommit}}, nil, context.Canceled
		},
	}}

	result, err := runner.Run(ctx, ftue.JourneyRequest{RetryTargets: []ftue.RetryTarget{target}})
	if err != nil {
		t.Fatalf("partially completed hook stage could not retain its exact retry: %v", err)
	}
	if len(result.Retry) != 1 || result.Retry[0].Repository != target.Repository || fmt.Sprint(result.Retry[0].Events) != fmt.Sprint([]githooks.Event{githooks.EventPrePush}) {
		t.Fatalf("hook retry did not subtract durable repository/event evidence: %+v", result.Retry)
	}
	if len(result.Effects) != 2 || result.Effects[0].HookEvent != githooks.EventPostCommit || result.Effects[1].Status != ftue.StatusCancelled {
		t.Fatalf("hook cancellation lost durable or cancelled evidence: %+v", result.Effects)
	}
}

func TestOrderedJourneyRunnerResumesPartialIngestAcrossBothFlows(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	first := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
		ftue.StageIngest: func(context.Context, ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			cancel()
			return []ftue.PersistedEffect{{Stage: ftue.StageIngest, Status: ftue.StatusPersisted, SessionID: "session-a"}},
				[]ftue.RetryTarget{{Stage: ftue.StageIngest, SessionIDs: []string{"session-b"}}}, context.Canceled
		},
	}}
	result, err := first.Run(ctx, ftue.JourneyRequest{RetryTargets: []ftue.RetryTarget{{Stage: ftue.StageIngest, SessionIDs: []string{"session-a", "session-b"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Retry) != 2 || result.Retry[0].Stage != ftue.StageIngest || fmt.Sprint(result.Retry[0].SessionIDs) != "[session-b]" || result.Retry[1].Stage != ftue.StagePublication || fmt.Sprint(result.Retry[1].SessionIDs) != "[session-a]" {
		t.Fatalf("partial ingest did not partition same-stage and downstream retries: %+v", result.Retry)
	}
	assertJourneyResumeUnion(t, result, ftue.StageIngest, []ftue.ExecutionStage{ftue.StageIngest, ftue.StagePublication, ftue.StageReceipt, ftue.StageHooks})
}

func TestOrderedJourneyRunnerResumesPartialPublicationAcrossBothFlows(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	first := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
		ftue.StagePublication: func(context.Context, ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			cancel()
			return []ftue.PersistedEffect{{Stage: ftue.StagePublication, Status: ftue.StatusPersisted, SessionID: "session-a"}},
				[]ftue.RetryTarget{{Stage: ftue.StagePublication, SessionIDs: []string{"session-b"}}}, context.Canceled
		},
	}}
	result, err := first.Run(ctx, ftue.JourneyRequest{RetryTargets: []ftue.RetryTarget{{Stage: ftue.StagePublication, SessionIDs: []string{"session-a", "session-b"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Retry) != 2 || result.Retry[0].Stage != ftue.StagePublication || fmt.Sprint(result.Retry[0].SessionIDs) != "[session-b]" || result.Retry[1].Stage != ftue.StageReceipt || fmt.Sprint(result.Retry[1].SessionIDs) != "[session-a]" {
		t.Fatalf("partial publication did not partition same-stage and downstream retries: %+v", result.Retry)
	}
	assertJourneyResumeUnion(t, result, ftue.StagePublication, []ftue.ExecutionStage{ftue.StagePublication, ftue.StageReceipt, ftue.StageHooks})
}

func TestOrderedJourneyRunnerResumesHookByConsentIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	target := ftue.RetryTarget{Stage: ftue.StageHooks, Repository: "relative/repository", Events: []githooks.Event{githooks.EventPostCommit, githooks.EventPrePush}}
	first := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
		ftue.StageHooks: func(context.Context, ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			cancel()
			return []ftue.PersistedEffect{{Stage: ftue.StageHooks, Status: ftue.StatusPersisted, Repository: "/canonical/repository", RetryRepository: target.Repository, HookEvent: githooks.EventPostCommit}}, []ftue.RetryTarget{target}, context.Canceled
		},
	}}
	result, err := first.Run(ctx, ftue.JourneyRequest{RetryTargets: []ftue.RetryTarget{target}})
	if err != nil {
		t.Fatal(err)
	}
	called := []githooks.Event{}
	resume := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
		ftue.StageHooks: func(_ context.Context, request ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			called = append(called, request.HookFilter.Events...)
			return []ftue.PersistedEffect{{Stage: ftue.StageHooks, Status: ftue.StatusPersisted, Repository: "/canonical/repository", RetryRepository: request.HookFilter.Repository, HookEvent: request.HookFilter.Events[0]}}, nil, nil
		},
	}}
	if _, err := resume.Run(t.Context(), ftue.JourneyRequest{PriorEffects: result.Effects, RetryTargets: result.Retry}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(called) != fmt.Sprint([]githooks.Event{githooks.EventPrePush}) || result.Effects[0].Repository == result.Effects[0].RetryRepository {
		t.Fatalf("hook resume lost canonical evidence or stable retry identity: effects=%+v called=%v", result.Effects, called)
	}
}

func TestOrderedJourneyRunnerPreservesRetryOnOperationFailure(t *testing.T) {
	target := ftue.RetryTarget{Stage: ftue.StagePublication, SessionIDs: []string{"session-pending"}}
	runner := ftue.OrderedJourneyRunner{Operations: map[ftue.ExecutionStage]ftue.StageOperation{
		ftue.StagePublication: func(context.Context, ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			return nil, []ftue.RetryTarget{target}, errors.New("village rejected publication")
		},
	}}
	result, err := runner.Run(t.Context(), ftue.JourneyRequest{RetryTargets: []ftue.RetryTarget{target}})
	if err == nil || !strings.Contains(err.Error(), "village rejected publication") {
		t.Fatalf("operation error=%v; want original actionable failure", err)
	}
	if len(result.Retry) != 1 || fmt.Sprint(result.Retry[0]) != fmt.Sprint(target) {
		t.Fatalf("operation failure lost exact retry target: %+v", result.Retry)
	}
	if validateErr := result.Validate(); validateErr != nil {
		t.Fatalf("failure result is unsafe to display: %v", validateErr)
	}
}

func assertJourneyResumeUnion(t *testing.T, cancelled ftue.JourneyResult, firstStage ftue.ExecutionStage, wantStages []ftue.ExecutionStage) {
	t.Helper()
	calls := []ftue.ExecutionStage{}
	seen := map[ftue.ExecutionStage][]string{}
	operations := map[ftue.ExecutionStage]ftue.StageOperation{}
	for _, stage := range wantStages {
		stage := stage
		operations[stage] = func(_ context.Context, request ftue.JourneyRequest) ([]ftue.PersistedEffect, []ftue.RetryTarget, error) {
			calls = append(calls, stage)
			seen[stage] = append([]string(nil), request.SessionFilter...)
			effects := make([]ftue.PersistedEffect, 0, len(request.SessionFilter))
			for _, id := range request.SessionFilter {
				effects = append(effects, ftue.PersistedEffect{Stage: stage, Status: ftue.StatusSkipped, SessionID: id})
			}
			if len(effects) == 0 {
				effects = append(effects, ftue.PersistedEffect{Stage: stage, Status: ftue.StatusSkipped})
			}
			return effects, nil, nil
		}
	}
	resumed, err := (ftue.OrderedJourneyRunner{Operations: operations}).Run(t.Context(), ftue.JourneyRequest{PriorEffects: cancelled.Effects, RetryTargets: cancelled.Retry})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(calls) != fmt.Sprint(wantStages) {
		t.Fatalf("resume stages=%v want %v", calls, wantStages)
	}
	wantPublication := []string{"session-a", "session-b"}
	if firstStage == ftue.StagePublication {
		wantPublication = []string{"session-b"}
	}
	if !sameUniqueStrings(seen[firstStage], []string{"session-b"}) || !sameUniqueStrings(seen[ftue.StagePublication], wantPublication) || !sameUniqueStrings(seen[ftue.StageReceipt], []string{"session-a", "session-b"}) {
		t.Fatalf("resume did not carry the exact A+B union once through publication and receipt: %+v", seen)
	}
	for _, stage := range []ftue.ExecutionStage{ftue.StagePublication, ftue.StageReceipt} {
		ids := []string{}
		for _, effect := range resumed.Effects {
			if effect.Stage == stage && effect.SessionID != "" {
				ids = append(ids, effect.SessionID)
			}
		}
		if !sameUniqueStrings(ids, []string{"session-a", "session-b"}) {
			t.Fatalf("resumed %s effects=%v, want session-a and session-b exactly once", stage, ids)
		}
	}
}

func sameUniqueStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, value := range want {
		if !slices.Contains(got, value) {
			return false
		}
	}
	return true
}
