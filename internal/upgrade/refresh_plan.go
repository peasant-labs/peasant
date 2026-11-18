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
}

// RefreshUnknown identifies unavailable evidence for a session.
type RefreshUnknown struct{ SessionID, Evidence string }

// RefreshEstimate counts decisions by their canonical lane and reason; it is
// not a time prediction, since source contents and future discoveries are unknown.
type RefreshEstimate struct {
	Sessions  int
	Undecided int
	ByLane    map[ingest.RefreshLane]int
	ByReason  map[ingest.RefreshReason]int
}

// PlanRefresh reads the store without applying schema changes or writing files.
func PlanRefresh(ctx context.Context, path string) (RefreshPlan, error) {
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
	declarations, err := ProducerDeclarations()
	if err != nil {
		return RefreshPlan{}, fmt.Errorf("plan refresh producers: %w", err)
	}
	type targets struct {
		adapter, indexer int
		canExtract       bool
	}
	producers := make(map[string]targets)
	for _, d := range declarations {
		if d.Kind != ProducerKindAdapter && d.Kind != ProducerKindIndexer {
			continue
		}
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
		Estimate:        RefreshEstimate{ByLane: map[ingest.RefreshLane]int{}, ByReason: map[ingest.RefreshReason]int{}}}
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
		plan.Sessions = append(plan.Sessions, SessionRefresh{SessionID: row.SessionID, Harness: row.Harness, Decision: decision, Decided: true, Effects: effects})
		plan.Estimate.ByLane[decision.Lane]++
		plan.Estimate.ByReason[decision.Reason]++
	}
	return plan, nil
}
