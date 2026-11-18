package upgrade

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
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
}

// SessionRefresh records the decision made for one stored session.
type SessionRefresh struct {
	SessionID string
	Harness   string
	Decision  ingest.RefreshDecision
	// Effects lists pending migrations that change this session's evidence.
	// A decision with a higher-priority reason can otherwise mask an index reset.
	Effects []store.StampEffect
}

// RefreshUnknown identifies unavailable evidence for a session.
type RefreshUnknown struct{ SessionID, Evidence string }

// RefreshEstimate counts decisions by their canonical lane and reason; it is
// not a time prediction, since source contents and future discoveries are unknown.
type RefreshEstimate struct {
	Sessions int
	ByLane   map[ingest.RefreshLane]int
	ByReason map[ingest.RefreshReason]int
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
	plan := RefreshPlan{Schema: schema, LowerBound: schema.From < schema.To, Estimate: RefreshEstimate{ByLane: map[ingest.RefreshLane]int{}, ByReason: map[ingest.RefreshReason]int{}}}
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
		for _, step := range schema.Steps {
			switch step.StampEffect {
			case store.StampEffectHarnessRename:
				switch row.Harness {
				case "claude":
					row.Harness = string(ingest.HarnessClaudeCode)
					effects = append(effects, step.StampEffect)
				case "gemini":
					row.Harness = "gemini-cli"
					effects = append(effects, step.StampEffect)
				}
			case store.StampEffectIndexInvalidated:
				row.IndexerVersion = 0
				row.IndexedInputHash = nil
				effects = append(effects, step.StampEffect)
			}
		}
		var harness ingest.Harness
		var target ingest.HarvesterVersions
		found := false
		for h, versions := range ingest.HarvesterVersionRegistry {
			if string(h) == row.Harness {
				harness, target, found = h, versions, true
				break
			}
		}
		if !found {
			plan.Unknown = append(plan.Unknown, RefreshUnknown{row.SessionID, "harness"})
		}

		version := 1
		if row.AdapterVersion != nil {
			version = *row.AdapterVersion
		}
		canExtract := false
		if factory, ok := ingest.DefaultAdapterRegistry[harness]; ok {
			_, canExtract = factory(nil, nil, salt.Salt{}).(ingest.TranscriptMetadataExtractor)
		}
		decision := ingest.DecideRefresh(ingest.RefreshEvidence{StoredRow: true, MetadataSchemaOld: row.MetadataVersion <= int(ingest.CurrentSchemaVersion) && !ingest.MetadataSchemaVersionIsCurrent(row.MetadataVersion), AdapterVersion: version, AdapterTarget: target.AdapterVersion, AdapterStampPresent: row.AdapterVersion != nil, AdapterCanExtract: canExtract, IndexerVersion: row.IndexerVersion, IndexerTarget: target.IndexerVersion, IndexProofPresent: row.IndexedInputHash != nil})
		plan.Sessions = append(plan.Sessions, SessionRefresh{SessionID: row.SessionID, Harness: row.Harness, Decision: decision, Effects: effects})
		plan.Estimate.Sessions++
		plan.Estimate.ByLane[decision.Lane]++
		plan.Estimate.ByReason[decision.Reason]++
	}
	return plan, nil
}
