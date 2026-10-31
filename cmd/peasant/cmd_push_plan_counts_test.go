package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/push_plan_counts.yaml
var pushPlanCountsYAML []byte

//go:embed testdata/push_plan_counts_manifest.yaml
var pushPlanCountsManifestYAML []byte

type planCountsEntry struct {
	Verdict   string            `yaml:"verdict"`
	Subagents []planCountsEntry `yaml:"subagents"`
}

type planCountsCase struct {
	Name     string            `yaml:"name"`
	NoPlan   bool              `yaml:"noPlan"`
	Plan     []planCountsEntry `yaml:"plan"`
	Pipeline map[string]int    `yaml:"pipeline"`
	Want     map[string]int    `yaml:"want"`
}

func loadPlanCountsCases(t *testing.T) []planCountsCase {
	t.Helper()
	var fixture struct {
		Cases []planCountsCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(pushPlanCountsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode push_plan_counts.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushPlanCountsManifestYAML, "push plan counts")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push plan counts"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// buildPlanEntries turns fixture verdicts into plan entries, numbering the
// sessions so each entry has its own id.
func buildPlanEntries(t *testing.T, in []planCountsEntry, next *int) ([]planEntry, int) {
	t.Helper()
	out := make([]planEntry, 0, len(in))
	total := 0
	for _, e := range in {
		verdict, err := NewPlanVerdict(e.Verdict)
		if err != nil {
			t.Fatal(err)
		}
		*next++
		entry := planEntry{SessionID: fmt.Sprintf("s%d", *next), Verdict: verdict}
		subs, subTotal := buildPlanEntries(t, e.Subagents, next)
		if len(subs) > 0 {
			entry.Subagents = subs
		}
		out = append(out, entry)
		total += 1 + subTotal
	}
	return out, total
}

// TestPushPlanCounts proves the counts a push that names its sessions prints
// are the tally of its plan, never the pipeline's own counters, and that every
// plan entry is counted exactly once.
func TestPushPlanCounts(t *testing.T) {
	t.Parallel()
	planOnly := []string{"not_ready", "excluded", "unknown_session"}
	for _, tc := range loadPlanCountsCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			result := &push.PushResult{
				New: tc.Pipeline["new"], Updated: tc.Pipeline["updated"], Skipped: tc.Pipeline["skipped"],
				Errors: tc.Pipeline["errors"], Held: tc.Pipeline["held"],
			}
			var plan *jsonPushPlan
			entries := 0
			if !tc.NoPlan {
				next := 0
				var built []planEntry
				built, entries = buildPlanEntries(t, tc.Plan, &next)
				plan = &jsonPushPlan{Collectives: []planCollective{}, Plan: built}
			}

			var buf bytes.Buffer
			if err := printPushJSONWithPlan(&buf, result, nil, nil, plan); err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
				t.Fatalf("stdout is not one JSON document: %v\n%s", err, buf.String())
			}
			sum := 0
			for key, want := range tc.Want {
				got, ok := doc[key].(float64)
				if !ok {
					t.Errorf("key %q missing from the document", key)
					continue
				}
				if int(got) != want {
					t.Errorf("%s = %d, want %d", key, int(got), want)
				}
				sum += want
			}
			for _, key := range planOnly {
				if _, present := doc[key]; present == tc.NoPlan {
					t.Errorf("key %q present = %v, want %v", key, present, !tc.NoPlan)
				}
			}
			if !tc.NoPlan && sum != entries {
				t.Errorf("counts add up to %d, want one per plan entry (%d)", sum, entries)
			}

			// The text summary prints the same counts.
			var text bytes.Buffer
			printPushSummary(&text, result, runCounts(result, plan), false, false)
			if !strings.Contains(text.String(), fmt.Sprintf("%d new, %d updated, %d error(s)", tc.Want["new"], tc.Want["updated"], tc.Want["errors"])) {
				t.Errorf("text summary does not restate the counts: %q", text.String())
			}
			if !tc.NoPlan && !strings.Contains(text.String(), fmt.Sprintf("%d not ready, %d excluded, %d unknown", tc.Want["not_ready"], tc.Want["excluded"], tc.Want["unknown_session"])) {
				t.Errorf("text summary does not restate the plan-only counts: %q", text.String())
			}
		})
	}
}
