package upgrade

import (
	"context"
	"fmt"
	"strconv"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// RefreshPlan is a read-only estimate based on the evidence available in the store.
type RefreshPlan struct {
	Schema   store.UpgradePlan
	Sessions []SessionRefresh
	Unknown  []RefreshUnknown
	Estimate RefreshEstimate
	// LowerBound means pending schema work can reveal additional work.
	LowerBound bool
	// Approximate remains true even on a current schema: not all evidence is read.
	Approximate      bool
	EvidenceNotRead  []string
	UnappliedEffects []store.StampEffect
}

// SessionRefresh records the decision made for one stored session.
type SessionRefresh struct {
	SessionID string
	Harness   string
	Decision  ingest.RefreshDecision
	Decided   bool
	// Effects lists pending migrations that change this session's evidence.
	// A decision with a higher-priority reason can otherwise mask an index reset.
	Effects []store.StampEffect
	// FullStep names the registered producer whose declaration does not cover
	// the stored lane the decision chose. It is nil when the decision's lane
	// stands. A non-nil FullStep plans the full refresh step instead.
	FullStep *UndeclaredProducer
}

// PlannedLane is the lane the plan charges this session: the decision's lane,
// or the native lane when an undeclared producer forces the full step.
func (s SessionRefresh) PlannedLane() ingest.RefreshLane {
	if s.FullStep != nil {
		return ingest.RefreshLaneNative
	}
	return s.Decision.Lane
}

// UndeclaredProducer names a producer that is in this build's registry but
// declares no output-preserving change scope for the stored lane a session's
// decision chose. The planner cannot charge that lane's skip, so it plans the
// full refresh step and names the producer.
//
// It is distinct from an unknown harness, where nothing in the registry can
// produce the session's targets: that session stays undecided and is listed in
// RefreshPlan.Unknown.
type UndeclaredProducer struct {
	Kind ProducerKind
	Name string
	// Skipped is the stored lane the decision chose and the plan does not charge.
	Skipped ingest.RefreshLane
	// Required is the change scope the producer would have to declare for the
	// stored lane to preserve the output it skips.
	Required ChangeScope
}

// String names the producer as kind/name, as ProducerDeclaration does.
func (u UndeclaredProducer) String() string {
	return ProducerDeclaration{Kind: u.Kind, Name: u.Name}.String()
}

// storedLaneScope is the output-preserving declaration one stored lane needs:
// the producer kind that does the lane's work and the change scope under which
// that work leaves the rest of the stored pair unchanged. The native and none
// lanes skip nothing and need no declaration.
type storedLaneScope struct {
	kind  ProducerKind
	scope ChangeScope
}

// storedLaneScopeFor returns the declaration a lane needs, or false for a lane
// that skips nothing.
func storedLaneScopeFor(lane ingest.RefreshLane) (storedLaneScope, bool) {
	switch lane {
	case ingest.RefreshLaneStoredMetadata:
		return storedLaneScope{ProducerKindAdapter, ChangeScopeMetadataOnly}, true
	case ingest.RefreshLaneStoredIndex:
		return storedLaneScope{ProducerKindIndexer, ChangeScopeIndexOnly}, true
	default:
		return storedLaneScope{}, false
	}
}

// RefreshUnknown identifies unavailable evidence for a session.
type RefreshUnknown struct{ SessionID, Evidence string }

// RefreshEstimate counts decisions by their canonical lane and reason; it is
// not a time prediction, since source contents and future discoveries are unknown.
type RefreshEstimate struct {
	Sessions  int
	Undecided int
	// ByLane counts the planned lane, so a session an undeclared producer
	// forces onto the full step counts as native.
	ByLane   map[ingest.RefreshLane]int
	ByReason map[ingest.RefreshReason]int
	// FullStep counts the sessions each undeclared producer forced onto the
	// full refresh step.
	FullStep map[UndeclaredProducer]int
}

// PlanRefresh reads the store without applying schema changes or writing files.
// It plans against the producers this build declares.
func PlanRefresh(ctx context.Context, path string) (RefreshPlan, error) {
	declarations, err := ProducerDeclarations()
	if err != nil {
		return RefreshPlan{}, fmt.Errorf("plan refresh producers: %w", err)
	}
	return PlanRefreshFor(ctx, path, declarations)
}

// PlanRefreshFor is PlanRefresh against a supplied declaration table. A
// measurement build passes a table in which one registered producer declares
// no change scope, to observe the full step that producer forces.
func PlanRefreshFor(ctx context.Context, path string, declarations []ProducerDeclaration) (RefreshPlan, error) {
	if err := ctx.Err(); err != nil {
		return RefreshPlan{}, err
	}
	schema, err := store.PlanUpgrade(path)
	if err != nil {
		return RefreshPlan{}, fmt.Errorf("plan refresh schema: %w", err)
	}
	rows, version, err := store.ReadRefreshEvidence(ctx, path)
	if err != nil {
		return RefreshPlan{}, fmt.Errorf("plan refresh evidence: %w", err)
	}
	if version != schema.From && !(version == 0 && len(rows) == 0 && len(schema.Steps) == 0) {
		return RefreshPlan{}, fmt.Errorf("plan refresh %s: schema changed from %d to %d while reading; no changes were made; retry after other migrations finish", path, schema.From, version)
	}
	type targets struct {
		adapter, indexer int
		canExtract       bool
	}
	producers := make(map[string]targets)
	scopes := make(map[string]map[ProducerKind]ChangeScope)
	for _, d := range declarations {
		if d.Kind != ProducerKindAdapter && d.Kind != ProducerKindIndexer {
			continue
		}
		if scopes[d.Name] == nil {
			scopes[d.Name] = make(map[ProducerKind]ChangeScope)
		}
		scopes[d.Name][d.Kind] = d.ChangeScope
		version, err := strconv.Atoi(string(d.Current()))
		if err != nil {
			return RefreshPlan{}, fmt.Errorf("plan refresh producer %s: invalid target %q: %w", d.String(), d.Current(), err)
		}
		target := producers[d.Name]
		if d.Kind == ProducerKindAdapter {
			target.adapter, target.canExtract = version, d.Input == ProducerInputStoredTranscript
		} else {
			target.indexer = version
		}
		producers[d.Name] = target
	}
	plan := RefreshPlan{Schema: schema, LowerBound: schema.From < schema.To, Approximate: true,
		EvidenceNotRead: []string{"capture_recovery", "publication_capture", "content_pending", "publication_bound", "source_compared", "latest_attempt", "source_state", "stored_pair", "permanent_refusal"},
		Estimate:        RefreshEstimate{ByLane: map[ingest.RefreshLane]int{}, ByReason: map[ingest.RefreshReason]int{}, FullStep: map[UndeclaredProducer]int{}}}
	for _, step := range schema.Steps {
		if step.StampEffect == store.StampEffectSearchInvalidated || step.StampEffect == store.StampEffectRecountOwed {
			plan.UnappliedEffects = append(plan.UnappliedEffects, step.StampEffect)
		}
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return RefreshPlan{}, err
		}
		if row.AdapterVersion == nil {
			plan.Unknown = append(plan.Unknown, RefreshUnknown{row.SessionID, "adapter_version"})
		}
		if row.IndexedInputHash == nil {
			plan.Unknown = append(plan.Unknown, RefreshUnknown{row.SessionID, "indexed_input_hash"})
		}
		var effects []store.StampEffect
		// Historical identifiers are intentionally rejected by NewHarness.
		// Preserve them (and unknown values) until the rename step is applied.
		historicalHarness := defaults.Harness(row.Harness)
		for _, step := range schema.Steps {
			switch step.StampEffect {
			case store.StampEffectHarnessRename:
				switch historicalHarness {
				case defaults.LegacyHarnessClaude:
					historicalHarness = defaults.HarnessClaudeCode
					row.Harness = string(historicalHarness)
					effects = append(effects, step.StampEffect)
				case defaults.LegacyHarnessGemini:
					historicalHarness = defaults.HarnessGeminiCLI
					row.Harness = string(historicalHarness)
					effects = append(effects, step.StampEffect)
				}
			case store.StampEffectIndexInvalidated:
				row.IndexerVersion = 0
				row.IndexedInputHash = nil
				effects = append(effects, step.StampEffect)
			}
		}
		target, found := producers[row.Harness]
		plan.Estimate.Sessions++
		if !found {
			plan.Unknown = append(plan.Unknown, RefreshUnknown{row.SessionID, "harness"})
			plan.Estimate.Undecided++
			plan.Sessions = append(plan.Sessions, SessionRefresh{SessionID: row.SessionID, Harness: row.Harness, Effects: effects})
			continue
		}

		version := 1
		if row.AdapterVersion != nil {
			version = *row.AdapterVersion
		}
		decision := ingest.DecideRefresh(ingest.RefreshEvidence{StoredRow: true, MetadataSchemaOld: ingest.MetadataNeedsNativeRefresh(row.MetadataVersion), AdapterVersion: version, AdapterTarget: target.adapter, AdapterStampPresent: row.AdapterVersion != nil, AdapterCanExtract: target.canExtract, IndexerVersion: row.IndexerVersion, IndexerTarget: target.indexer, IndexProofPresent: row.IndexedInputHash != nil})
		session := SessionRefresh{SessionID: row.SessionID, Harness: row.Harness, Decision: decision, Decided: true, Effects: effects}
		if need, stored := storedLaneScopeFor(decision.Lane); stored && scopes[row.Harness][need.kind] != need.scope {
			// The harness is registered, so this is not an unknown harness:
			// its producer for this lane is present but does not declare
			// the scope that lets the lane skip work. Plan the full step.
			session.FullStep = &UndeclaredProducer{Kind: need.kind, Name: row.Harness, Skipped: decision.Lane, Required: need.scope}
			plan.Estimate.FullStep[*session.FullStep]++
		}
		plan.Sessions = append(plan.Sessions, session)
		plan.Estimate.ByLane[session.PlannedLane()]++
		plan.Estimate.ByReason[decision.Reason]++
	}
	return plan, nil
}
