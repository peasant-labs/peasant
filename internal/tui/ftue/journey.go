package ftue

import (
	"context"
	"fmt"
	"slices"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/schema"
)

// PageID identifies a mounted journey page independently from its position.
type PageID int

const (
	PageAuthentication PageID = iota
	PageDiscovery
	PageProjects
	PageScope
	PageProjectPolicy
	PageRedaction
	PageLicense
	PageRetention
	PageDestination
	PageConsent
	PageExecution
	PageRetry
	PageCompletion
)

// Destination is the consented boundary for effects outside the local machine.
type Destination string

const (
	DestinationLocal   Destination = "local"
	DestinationVillage Destination = "village"
)

func (d Destination) IsValid() bool { return d == DestinationLocal || d == DestinationVillage }

// AuthenticationChoice records the explicit login-or-skip decision without
// treating the presence of credentials as consent to publish.
type AuthenticationChoice string

const (
	AuthenticationSkipped       AuthenticationChoice = "skipped"
	AuthenticationAuthenticated AuthenticationChoice = "authenticated"
)

func (a AuthenticationChoice) IsValid() bool {
	return a == AuthenticationSkipped || a == AuthenticationAuthenticated
}

// ExecutionStage names an ordered effect without owning its implementation.
type ExecutionStage string

const (
	StageConfig      ExecutionStage = "config"
	StageIngest      ExecutionStage = "ingest"
	StagePublication ExecutionStage = "publication"
	StageReceipt     ExecutionStage = "receipt"
	StageHooks       ExecutionStage = "hooks"
)

// ExecutionStatus is the durable outcome known for one stage.
type ExecutionStatus string

const (
	StatusPending   ExecutionStatus = "pending"
	StatusRunning   ExecutionStatus = "running"
	StatusPersisted ExecutionStatus = "persisted"
	StatusSkipped   ExecutionStatus = "skipped"
	StatusFailed    ExecutionStatus = "failed"
	StatusCancelled ExecutionStatus = "cancelled"
)

func (s ExecutionStatus) IsTerminal() bool {
	return s == StatusPersisted || s == StatusSkipped || s == StatusFailed || s == StatusCancelled
}

func executionStageRank(stage ExecutionStage) int {
	switch stage {
	case StageConfig:
		return 1
	case StageIngest:
		return 2
	case StagePublication:
		return 3
	case StageReceipt:
		return 4
	case StageHooks:
		return 5
	default:
		return 0
	}
}

// RetryTarget identifies the exact failed work a subsequent run may repeat.
type RetryTarget struct {
	Stage      ExecutionStage   `yaml:"stage"`
	SessionIDs []string         `yaml:"sessionIds"`
	Repository string           `yaml:"repository"`
	Events     []githooks.Event `yaml:"events"`
}

func (r RetryTarget) Validate() error {
	if executionStageRank(r.Stage) == 0 {
		return fmt.Errorf("retry target names unsupported stage %q; choose config, ingest, publication, receipt, or hooks", r.Stage)
	}
	switch r.Stage {
	case StageConfig:
		if len(r.SessionIDs) != 0 || r.Repository != "" || len(r.Events) != 0 {
			return fmt.Errorf("config retry must not name sessions, a repository, or hook events; reload and retry only the exact config save")
		}
	case StageIngest, StagePublication, StageReceipt:
		if len(r.SessionIDs) == 0 {
			return fmt.Errorf("%s retry must name at least one session ID so persisted work is not replayed", r.Stage)
		}
		if r.Repository != "" || len(r.Events) != 0 {
			return fmt.Errorf("%s retry accepts session IDs only; repository and hook event fields belong to hooks retry", r.Stage)
		}
	case StageHooks:
		if len(r.SessionIDs) != 0 || r.Repository == "" || len(r.Events) == 0 {
			return fmt.Errorf("hooks retry must name one repository and at least one event, and must not name session IDs")
		}
		for _, event := range r.Events {
			if err := event.Validate(); err != nil {
				return fmt.Errorf("hooks retry for repository %q names an unsupported event: %w", r.Repository, err)
			}
		}
	}
	return nil
}

// HookConsent binds hook mutation to one repository and explicit event set.
type HookConsent struct {
	Repository string
	Events     []githooks.Event
}

// PersistedEffect records what survived the run. It deliberately has no
// aggregate transaction flag: earlier effects remain true after later failure.
type PersistedEffect struct {
	Stage           ExecutionStage                       `yaml:"stage"`
	Status          ExecutionStatus                      `yaml:"status"`
	SessionID       string                               `yaml:"sessionId"`
	Repository      string                               `yaml:"repository"`
	RetryRepository string                               `yaml:"-"`
	HookEvent       githooks.Event                       `yaml:"hookEvent,omitempty"`
	URL             string                               `yaml:"url"`
	Visibility      schema.Visibility                    `yaml:"visibility"`
	VillageOrigin   string                               `yaml:"villageOrigin"`
	OwnerUserID     string                               `yaml:"ownerUserId"`
	ProjectHash     schema.ProjectHash                   `yaml:"projectHash"`
	Receipt         *schema.AuthoritativePublishResponse `yaml:"receipt,omitempty"`
	Detail          string                               `yaml:"detail"`
}

// JourneyRequest is the complete consent snapshot handed to the production
// orchestrator. FTUE does not implement any authority represented here.
type JourneyRequest struct {
	Answers       WizardAnswers
	PriorEffects  []PersistedEffect
	RetryTargets  []RetryTarget
	SessionFilter []string
	HookFilter    *RetryTarget
}

// JourneyResult is an exact, partial-safe execution receipt.
type JourneyResult struct {
	Effects []PersistedEffect
	Retry   []RetryTarget
}

func (r JourneyResult) Validate() error {
	seen := make(map[ExecutionStage]bool, len(r.Effects))
	lastRank := 0
	for _, effect := range r.Effects {
		rank := executionStageRank(effect.Stage)
		if rank == 0 || !effect.Status.IsTerminal() {
			return fmt.Errorf("journey result contains a non-terminal or unidentified effect at stage %q", effect.Stage)
		}
		if rank < lastRank {
			return fmt.Errorf("journey result reports stage %q after a later persisted-effect boundary", effect.Stage)
		}
		lastRank = rank
		if effect.Stage == StageConfig && seen[effect.Stage] {
			return fmt.Errorf("journey result contains duplicate singleton stage %q", effect.Stage)
		}
		seen[effect.Stage] = true
		if effect.RetryRepository != "" && effect.Stage != StageHooks {
			return fmt.Errorf("journey result contains hook retry identity on non-hook stage %q", effect.Stage)
		}
		if effect.URL != "" && effect.Status != StatusPersisted {
			return fmt.Errorf("journey result reports URL %q for non-persisted stage %q", effect.URL, effect.Stage)
		}
		if effect.Visibility != "" && effect.URL == "" {
			return fmt.Errorf("journey result reports visibility %q without an authoritative URL", effect.Visibility)
		}
		if effect.Receipt != nil {
			if effect.Stage != StageReceipt || effect.Status != StatusPersisted {
				return fmt.Errorf("journey result attaches an authoritative receipt outside a persisted receipt stage")
			}
			if err := effect.Receipt.Validate(); err != nil {
				return fmt.Errorf("journey result contains an invalid authoritative receipt for session %q: %w", effect.SessionID, err)
			}
			if effect.URL != effect.Receipt.TranscriptURL || effect.Visibility != effect.Receipt.Visibility {
				return fmt.Errorf("journey result summary does not match the authoritative receipt for session %q", effect.SessionID)
			}
			if effect.VillageOrigin == "" || effect.OwnerUserID == "" || effect.ProjectHash == "" || effect.SessionID == "" {
				return fmt.Errorf("journey result receipt identity is incomplete for session %q; Village origin, owner, project, and session are required", effect.SessionID)
			}
		} else if effect.Stage == StageReceipt && effect.Status == StatusPersisted {
			return fmt.Errorf("journey result persisted receipt stage for session %q without the complete authoritative receipt", effect.SessionID)
		}
	}
	for _, retry := range r.Retry {
		if err := retry.Validate(); err != nil {
			return fmt.Errorf("journey result contains an invalid retry target for stage %q: %w", retry.Stage, err)
		}
	}
	return nil
}

// JourneyRunner is the single orchestration seam. Its implementation composes
// config, ingest, publication, receipt and hook authorities under ctx.
type JourneyRunner interface {
	Run(context.Context, JourneyRequest) (JourneyResult, error)
}

type JourneyRunnerFunc func(context.Context, JourneyRequest) (JourneyResult, error)

func (f JourneyRunnerFunc) Run(ctx context.Context, request JourneyRequest) (JourneyResult, error) {
	return f(ctx, request)
}

var _ JourneyRunner = JourneyRunnerFunc(nil)

// StageOperation delegates one effect to its existing production authority.
type StageOperation func(context.Context, JourneyRequest) ([]PersistedEffect, []RetryTarget, error)

// OrderedJourneyRunner executes the consented effects in their safety order.
// A retry resumes at its named stage; successfully persisted earlier stages are not repeated.
type OrderedJourneyRunner struct {
	Operations map[ExecutionStage]StageOperation
}

var journeyStageOrder = [...]ExecutionStage{StageConfig, StageIngest, StagePublication, StageReceipt, StageHooks}

func (r OrderedJourneyRunner) Run(ctx context.Context, request JourneyRequest) (JourneyResult, error) {
	result := JourneyResult{Effects: append([]PersistedEffect(nil), request.PriorEffects...)}
	for _, target := range request.RetryTargets {
		if err := target.Validate(); err != nil {
			return result, fmt.Errorf("cannot start targeted journey retry: %w; no effect was attempted", err)
		}
	}
	startRank := 1
	if len(request.RetryTargets) > 0 {
		startRank = 6
		for _, target := range request.RetryTargets {
			startRank = min(startRank, executionStageRank(target.Stage))
		}
	}
	for _, stage := range journeyStageOrder {
		if executionStageRank(stage) < startRank {
			continue
		}
		if err := ctx.Err(); err != nil {
			result.Effects = mergeJourneyEffects(result.Effects, []PersistedEffect{{Stage: stage, Status: StatusCancelled, Detail: err.Error()}})
			retry, retryErr := cancellationRetryTargets(stage, request, result.Effects, exactRetryTargets(request.RetryTargets, stage))
			if retryErr != nil {
				return result, retryErr
			}
			result.Retry = append(result.Retry, retry...)
			if validateErr := result.Validate(); validateErr != nil {
				return result, fmt.Errorf("validate journey result after cancellation at stage %q: %w; completion state is unsafe to display", stage, validateErr)
			}
			return result, nil
		}
		op := r.Operations[stage]
		if op == nil {
			result.Effects = mergeJourneyEffects(result.Effects, []PersistedEffect{{Stage: stage, Status: StatusSkipped}})
			continue
		}
		stageRequests := requestsForStage(request, stage, retrySessionIDsThrough(request.RetryTargets, stage))
		for _, stageRequest := range stageRequests {
			effects, retries, err := op(ctx, stageRequest)
			result.Effects = mergeJourneyEffects(result.Effects, effects)
			if err != nil {
				if cancelErr := ctx.Err(); cancelErr != nil {
					if !slices.ContainsFunc(effects, func(effect PersistedEffect) bool { return effect.Stage == stage && effect.Status == StatusCancelled }) {
						detail := fmt.Sprintf("%s was cancelled while %s was running; persisted effects above remain applied; retry the exact target to resume downstream work", cancelErr, stage)
						result.Effects = mergeJourneyEffects(result.Effects, []PersistedEffect{{Stage: stage, Status: StatusCancelled, Detail: detail}})
					}
					retry, retryErr := cancellationRetryTargets(stage, stageRequest, result.Effects, retries)
					if retryErr != nil {
						return result, retryErr
					}
					result.Retry = append(result.Retry, retry...)
					if validateErr := result.Validate(); validateErr != nil {
						return result, fmt.Errorf("validate journey result after cancellation at stage %q: %w; completion state is unsafe to display", stage, validateErr)
					}
					return result, nil
				}
				result.Retry = append(result.Retry, retries...)
				if !slices.ContainsFunc(effects, func(effect PersistedEffect) bool { return effect.Stage == stage && effect.Status == StatusFailed }) {
					result.Effects = mergeJourneyEffects(result.Effects, []PersistedEffect{{Stage: stage, Status: StatusFailed, Detail: err.Error()}})
				}
				if validateErr := result.Validate(); validateErr != nil {
					return result, fmt.Errorf("validate journey result after failure at stage %q: %w; original stage error: %v; completion state is unsafe to display", stage, validateErr, err)
				}
				return result, err
			}
			result.Retry = append(result.Retry, retries...)
		}
	}
	return result, result.Validate()
}

func exactRetryTargets(targets []RetryTarget, stage ExecutionStage) []RetryTarget {
	exact := make([]RetryTarget, 0, len(targets))
	for _, target := range targets {
		if target.Stage == stage {
			exact = append(exact, target)
		}
	}
	return exact
}

func cancellationRetryTargets(stage ExecutionStage, request JourneyRequest, effects []PersistedEffect, operationTargets []RetryTarget) ([]RetryTarget, error) {
	if len(operationTargets) == 0 {
		return cancellationRetry(stage, request, effects)
	}
	remaining := make([]RetryTarget, 0, len(operationTargets)+1)
	for _, target := range operationTargets {
		if err := target.Validate(); err != nil {
			return nil, fmt.Errorf("cannot safely resume cancelled %q stage: operation returned an invalid retry target: %w; persisted and cancelled effects remain recorded, but automatic retry is disabled", stage, err)
		}
		if target.Stage == StageHooks {
			target = subtractDurableHookEvents(target, effects)
			if len(target.Events) == 0 {
				continue
			}
		} else if target.Stage == stage {
			target.SessionIDs = subtractDurableSessionIDs(target, effects)
			if stage != StageConfig && len(target.SessionIDs) == 0 {
				continue
			}
		}
		remaining = append(remaining, target)
	}
	durable := durableSessionIDs(stage, requestedSessionIDs(request), effects)
	if len(durable) > 0 {
		if downstream, ok := downstreamSessionStage(stage); ok {
			remaining = append(remaining, RetryTarget{Stage: downstream, SessionIDs: durable})
		}
	}
	if len(remaining) == 0 {
		return downstreamCancellationRetry(stage, request)
	}
	return remaining, nil
}

func cancellationRetry(stage ExecutionStage, request JourneyRequest, effects []PersistedEffect) ([]RetryTarget, error) {
	switch stage {
	case StageConfig:
		if !stageHasDurableEffect(effects, stage) {
			return []RetryTarget{{Stage: stage}}, nil
		}
		return downstreamCancellationRetry(stage, request)
	case StageIngest, StagePublication, StageReceipt:
		ids := requestedSessionIDs(request)
		durable := durableSessionIDs(stage, ids, effects)
		ids = subtractDurableSessionIDs(RetryTarget{Stage: stage, SessionIDs: ids}, effects)
		targets := make([]RetryTarget, 0, 2)
		if len(ids) > 0 {
			targets = append(targets, RetryTarget{Stage: stage, SessionIDs: ids})
		}
		if downstream, ok := downstreamSessionStage(stage); ok && len(durable) > 0 {
			targets = append(targets, RetryTarget{Stage: downstream, SessionIDs: durable})
		}
		if len(targets) > 0 {
			return targets, nil
		}
		return downstreamCancellationRetry(stage, request)
	case StageHooks:
		if request.HookFilter != nil {
			remaining := subtractDurableHookEvents(*request.HookFilter, effects)
			if len(remaining.Events) > 0 {
				return []RetryTarget{remaining}, nil
			}
			return nil, fmt.Errorf("cannot safely resume after completed cancelled %q stage: the final stage has no downstream retry target; persisted and cancelled effects remain recorded, but automatic retry is disabled", stage)
		}
	}
	return nil, fmt.Errorf("cannot safely resume cancelled %q stage: no exact retry identity or valid downstream target is available; persisted and cancelled effects remain recorded, but automatic retry is disabled", stage)
}

func subtractDurableSessionIDs(target RetryTarget, effects []PersistedEffect) []string {
	return slices.DeleteFunc(append([]string(nil), target.SessionIDs...), func(id string) bool {
		if id == "" {
			return true
		}
		return slices.ContainsFunc(effects, func(effect PersistedEffect) bool {
			return effect.Stage == target.Stage && effect.SessionID == id && effectIsDurable(effect)
		})
	})
}

func requestedSessionIDs(request JourneyRequest) []string {
	ids := append([]string(nil), request.SessionFilter...)
	if len(ids) == 0 {
		for _, session := range request.Answers.EffectiveSelectedSessions() {
			ids = append(ids, session.SessionID)
			ids = append(ids, session.SubagentIDs...)
		}
	}
	return slices.DeleteFunc(ids, func(id string) bool { return id == "" })
}

func durableSessionIDs(stage ExecutionStage, requested []string, effects []PersistedEffect) []string {
	durable := make([]string, 0, len(requested))
	for _, id := range requested {
		if slices.Contains(durable, id) {
			continue
		}
		if slices.ContainsFunc(effects, func(effect PersistedEffect) bool {
			return effect.Stage == stage && effect.SessionID == id && effectIsDurable(effect)
		}) {
			durable = append(durable, id)
		}
	}
	return durable
}

func downstreamSessionStage(stage ExecutionStage) (ExecutionStage, bool) {
	switch stage {
	case StageIngest:
		return StagePublication, true
	case StagePublication:
		return StageReceipt, true
	default:
		return "", false
	}
}

func effectIsDurable(effect PersistedEffect) bool {
	return effect.Status == StatusPersisted || effect.Status == StatusSkipped
}

func stageHasDurableEffect(effects []PersistedEffect, stage ExecutionStage) bool {
	return slices.ContainsFunc(effects, func(effect PersistedEffect) bool {
		return effect.Stage == stage && effectIsDurable(effect)
	})
}

func subtractDurableHookEvents(target RetryTarget, effects []PersistedEffect) RetryTarget {
	remaining := target
	remaining.Events = slices.DeleteFunc(append([]githooks.Event(nil), target.Events...), func(event githooks.Event) bool {
		return slices.ContainsFunc(effects, func(effect PersistedEffect) bool {
			return effect.Stage == StageHooks && effect.RetryRepository == target.Repository && effect.HookEvent == event && effectIsDurable(effect)
		})
	})
	return remaining
}

func downstreamCancellationRetry(stage ExecutionStage, request JourneyRequest) ([]RetryTarget, error) {
	ids := append([]string(nil), request.SessionFilter...)
	if len(ids) == 0 {
		for _, session := range request.Answers.EffectiveSelectedSessions() {
			ids = append(ids, session.SessionID)
			ids = append(ids, session.SubagentIDs...)
		}
	}
	ids = slices.DeleteFunc(ids, func(id string) bool { return id == "" })
	switch stage {
	case StageConfig:
		if len(ids) > 0 {
			return []RetryTarget{{Stage: StageIngest, SessionIDs: ids}}, nil
		}
	case StageIngest:
		if len(ids) > 0 {
			return []RetryTarget{{Stage: StagePublication, SessionIDs: ids}}, nil
		}
	case StagePublication:
		if len(ids) > 0 {
			return []RetryTarget{{Stage: StageReceipt, SessionIDs: ids}}, nil
		}
	case StageReceipt:
		targets := make([]RetryTarget, 0, len(request.Answers.HookConsents))
		for _, consent := range request.Answers.HookConsents {
			target := RetryTarget{Stage: StageHooks, Repository: consent.Repository, Events: append([]githooks.Event(nil), consent.Events...)}
			if err := target.Validate(); err != nil {
				return nil, fmt.Errorf("cannot safely resume after completed cancelled %q stage: downstream hook evidence is invalid: %w; automatic retry is disabled", stage, err)
			}
			targets = append(targets, target)
		}
		if len(targets) > 0 {
			return targets, nil
		}
	}
	return nil, fmt.Errorf("cannot safely resume after completed cancelled %q stage: no valid downstream retry target can be identified; persisted and cancelled effects remain recorded, but automatic retry is disabled", stage)
}

func retrySessionIDsThrough(targets []RetryTarget, stage ExecutionStage) []string {
	seen := map[string]bool{}
	var ids []string
	for _, target := range targets {
		if executionStageRank(target.Stage) > executionStageRank(stage) {
			continue
		}
		for _, id := range target.SessionIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func requestsForStage(base JourneyRequest, stage ExecutionStage, downstream []string) []JourneyRequest {
	var exact []RetryTarget
	for _, target := range base.RetryTargets {
		if target.Stage == stage {
			exact = append(exact, target)
		}
	}
	if len(exact) == 0 {
		base.RetryTargets = nil
		base.SessionFilter = append([]string(nil), downstream...)
		base.HookFilter = nil
		return []JourneyRequest{base}
	}
	if stage != StageHooks {
		ids := append([]string(nil), downstream...)
		for _, target := range exact {
			for _, id := range target.SessionIDs {
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
		base.RetryTargets = nil
		base.SessionFilter = ids
		base.HookFilter = nil
		return []JourneyRequest{base}
	}
	requests := make([]JourneyRequest, 0, len(exact))
	for i := range exact {
		r := base
		r.RetryTargets = nil
		r.SessionFilter = append([]string(nil), exact[i].SessionIDs...)
		if stage == StageHooks {
			target := exact[i]
			r.HookFilter = &target
		} else {
			r.HookFilter = nil
		}
		requests = append(requests, r)
	}
	return requests
}

func mergeJourneyEffects(prior, current []PersistedEffect) []PersistedEffect {
	resolvedStages := map[ExecutionStage]bool{}
	for _, effect := range current {
		if effect.Status == StatusPersisted || effect.Status == StatusSkipped {
			resolvedStages[effect.Stage] = true
		}
	}
	merged := make([]PersistedEffect, 0, len(prior)+len(current))
	for _, effect := range prior {
		if resolvedStages[effect.Stage] && effect.SessionID == "" && effect.Repository == "" && (effect.Status == StatusFailed || effect.Status == StatusCancelled) {
			continue
		}
		merged = append(merged, effect)
	}
	index := map[string]int{}
	for i, effect := range merged {
		index[effectIdentity(effect)] = i
	}
	for _, effect := range current {
		key := effectIdentity(effect)
		if i, ok := index[key]; ok {
			merged[i] = effect
			continue
		}
		index[key] = len(merged)
		merged = append(merged, effect)
	}
	slices.SortStableFunc(merged, func(a, b PersistedEffect) int { return executionStageRank(a.Stage) - executionStageRank(b.Stage) })
	return merged
}

func effectIdentity(effect PersistedEffect) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s", effect.Stage, effect.SessionID, effect.Repository, effect.HookEvent)
}

var _ JourneyRunner = OrderedJourneyRunner{}
