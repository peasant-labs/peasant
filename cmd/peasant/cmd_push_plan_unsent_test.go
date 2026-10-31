package main

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/push_plan_unsent.yaml
var pushPlanUnsentYAML []byte

//go:embed testdata/push_plan_unsent_manifest.yaml
var pushPlanUnsentManifestYAML []byte

type planUnsentCase struct {
	Name         string    `yaml:"name"`
	Pinned       *[]string `yaml:"pinned"`
	Held         string    `yaml:"held"`
	Selected     *bool     `yaml:"selected"`
	InRepository *bool     `yaml:"inRepository"`
	Want         struct {
		Verdict        string `yaml:"verdict"`
		Reason         string `yaml:"reason"`
		DetailContains string `yaml:"detailContains"`
		DetailOmits    string `yaml:"detailOmits"`
	} `yaml:"want"`
}

func loadPlanUnsentCases(t *testing.T) []planUnsentCase {
	t.Helper()
	var fixture struct {
		Cases []planUnsentCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(pushPlanUnsentYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode push_plan_unsent.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushPlanUnsentManifestYAML, "push plan unsent")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push plan unsent"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// TestExplainOneNamesTheFilterTheRunApplied pins the verdict of a session the
// run did not report: the filter the run applied, in its narrowing order.
func TestExplainOneNamesTheFilterTheRunApplied(t *testing.T) {
	t.Parallel()
	const id = "0a0a0a0a-0000-4000-8000-00000000000a"
	const projectHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const otherHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, tc := range loadPlanUnsentCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			in := planInputs{known: map[string]bool{id: true}, autoPublish: autoPublishPlan{held: map[string]string{}}}
			if tc.Pinned != nil {
				in.runCfg.PinnedSessionIDs = map[string]bool{}
				for _, pinned := range *tc.Pinned {
					in.runCfg.PinnedSessionIDs[pinned] = true
				}
			}
			if tc.Held != "" {
				in.autoPublish.held[id] = tc.Held
			}
			if tc.Selected != nil {
				decision := ingest.BranchMatchNo
				if *tc.Selected {
					decision = ingest.BranchMatchYes
				}
				in.runCfg.Selection = push.NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{ingest.SessionID(id): decision})
			}
			if tc.InRepository != nil {
				admitted := otherHash
				if *tc.InRepository {
					admitted = projectHash
				}
				in.runCfg.Repository = push.NewRepositoryScope("/repo", ingest.ProjectHash(admitted), push.IdentityFromRemote, nil, nil)
			}
			rows := map[string]ingest.PushSessionRow{id: {SessionID: id, ProjectHash: projectHash, ModelHarness: string(schema.HarnessClaudeCode)}}
			entry := explainOne(id, in, rows, map[string]push.SessionPushResult{}, nil)
			if string(entry.Verdict) != tc.Want.Verdict || string(entry.Reason) != tc.Want.Reason {
				t.Fatalf("verdict, reason = %s, %q; want %s, %q (detail %q)", entry.Verdict, entry.Reason, tc.Want.Verdict, tc.Want.Reason, entry.Detail)
			}
			if !strings.Contains(entry.Detail, tc.Want.DetailContains) {
				t.Errorf("detail = %q, want it to contain %q", entry.Detail, tc.Want.DetailContains)
			}
			if tc.Want.DetailOmits != "" && strings.Contains(entry.Detail, tc.Want.DetailOmits) {
				t.Errorf("detail = %q, want it not to contain %q", entry.Detail, tc.Want.DetailOmits)
			}
		})
	}
}
