package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/upgrade"
)

type schemaPlanStep struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Cost    string `json:"cost"`
	Counts  string `json:"counts"`
}

type schemaPlanDisk struct {
	StoreBytes     int64 `json:"store_bytes"`
	FreeBytes      int64 `json:"free_bytes"`
	FreeBytesKnown bool  `json:"free_bytes_known"`
	RequiredBytes  int64 `json:"required_bytes"`
}

type schemaPlanJSON struct {
	DBPath string           `json:"db_path"`
	From   int              `json:"from"`
	To     int              `json:"to"`
	Steps  []schemaPlanStep `json:"steps"`
	Disk   schemaPlanDisk   `json:"disk"`
}

func schemaUpgradeJSON(plan store.UpgradePlan) schemaPlanJSON {
	steps := make([]schemaPlanStep, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		steps = append(steps, schemaPlanStep{
			Version: step.Version,
			Kind:    step.Kind.String(),
			Title:   step.Title,
			Cost:    step.Cost.String(),
			Counts:  step.Counts.String(),
		})
	}
	return schemaPlanJSON{
		DBPath: plan.DBPath, From: plan.From, To: plan.To, Steps: steps,
		Disk: schemaPlanDisk{
			StoreBytes: plan.StoreBytes, FreeBytes: plan.FreeBytes,
			FreeBytesKnown: plan.FreeBytesKnown, RequiredBytes: plan.RequiredFreeBytes,
		},
	}
}

// sessionWork is a census, not a list of session identities. An error means
// the evidence could not be read and the numeric fields are not a census.
type sessionWork struct {
	Available        bool                `json:"available"`
	Error            string              `json:"error,omitempty"`
	LowerBound       bool                `json:"lower_bound"`
	Approximate      bool                `json:"approximate"`
	EvidenceNotRead  []string            `json:"evidence_not_read"`
	UnappliedEffects []store.StampEffect `json:"unapplied_effects"`
	Sessions         int                 `json:"sessions"`
	Undecided        int                 `json:"undecided"`
	ByLane           map[string]int      `json:"by_lane"`
	ByReason         map[string]int      `json:"by_reason"`
	// FullStep counts the sessions each registered producer forced onto the
	// full refresh step because it declares no output-preserving change scope
	// for the stored lane its decision chose. Its key names the producer.
	FullStep map[string]int `json:"full_step"`
	Unknown  map[string]int `json:"unknown"`
}

func summarizeSessionWork(plan upgrade.RefreshPlan, err error) sessionWork {
	work := sessionWork{Available: err == nil, ByLane: map[string]int{}, ByReason: map[string]int{}, FullStep: map[string]int{}, Unknown: map[string]int{}}
	if err != nil {
		work.Error = err.Error()
		return work
	}
	work.LowerBound, work.Sessions = plan.LowerBound, plan.Estimate.Sessions
	work.Approximate, work.EvidenceNotRead, work.UnappliedEffects = plan.Approximate, plan.EvidenceNotRead, plan.UnappliedEffects
	work.Undecided = plan.Estimate.Undecided
	for lane, count := range plan.Estimate.ByLane {
		work.ByLane[string(lane)] = count
	}
	for reason, count := range plan.Estimate.ByReason {
		work.ByReason[string(reason)] = count
	}
	for producer, count := range plan.Estimate.FullStep {
		work.FullStep[producer.String()] += count
	}
	for _, unknown := range plan.Unknown {
		work.Unknown[unknown.Evidence]++
	}
	return work
}

func writeSessionWork(out io.Writer, work sessionWork) {
	if !work.Available {
		fmt.Fprintf(out, "session work: unavailable: %s; check the store and retry the dry run\n", work.Error)
		return
	}
	fmt.Fprintf(out, "session work: %d session(s)\n", work.Sessions)
	fmt.Fprintf(out, "  undecided: %d\n", work.Undecided)
	fmt.Fprintf(out, "  approximate: evidence not read: %s\n", strings.Join(work.EvidenceNotRead, ", "))
	for _, effect := range work.UnappliedEffects {
		fmt.Fprintf(out, "  pending migration effect not applied to census: %s\n", effect)
	}
	writeSessionWorkCounts(out, "lane", work.ByLane)
	writeSessionWorkCounts(out, "reason", work.ByReason)
	writeSessionWorkCounts(out, "full step, undeclared producer", work.FullStep)
	writeSessionWorkCounts(out, "unknown evidence", work.Unknown)
	if work.LowerBound {
		fmt.Fprintln(out, "  lower bound: pending schema changes may reveal more work; upgrade the schema and run the dry run again for a fuller census")
	}
}

func writeSessionWorkCounts(out io.Writer, label string, counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Fprintf(out, "  %s: none\n", label)
		return
	}
	for _, key := range keys {
		fmt.Fprintf(out, "  %s %s: %d\n", label, key, counts[key])
	}
}
