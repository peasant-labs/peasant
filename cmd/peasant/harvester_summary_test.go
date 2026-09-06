package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harvester_summary.yaml
var harvesterSummaryYAML []byte

func TestHarvestHarvesterSummary(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		SessionID     string   `yaml:"sessionID"`
		Transcript    string   `yaml:"transcript"`
		Cases         []struct {
			Name    string `yaml:"name"`
			Command string `yaml:"command"`
			DryRun  bool   `yaml:"dryRun"`
			Indexed int    `yaml:"indexed"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(harvesterSummaryYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			dir, source, output := t.TempDir(), t.TempDir(), t.TempDir()
			project := filepath.Join(source, "-fixture-project")
			if err := os.MkdirAll(project, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(project, fixtures.SessionID+".jsonl")
			if err := os.WriteFile(path, []byte(fixtures.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{fixture.Command, "--source-harness=claude-code", "--source-path=" + source, "--output=" + output, "--include-active", "--json"}
			if fixture.DryRun {
				args = append(args, "--dry-run")
			}
			out, err := executeHarvestCmd(t, dir, args)
			if err != nil {
				t.Fatalf("harvest: %v\n%s", err, out)
			}
			var decoded struct {
				Summary  ingest.PipelineSummary `json:"summary"`
				Sessions []struct {
					SessionID string `json:"sessionId"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal([]byte(out), &decoded); err != nil {
				t.Fatalf("decode harvest: %v\n%s", err, out)
			}
			if !maps.Equal(decoded.Summary.HarvesterVersions, ingest.HarvesterVersionRegistry) {
				t.Fatalf("target map = %+v", decoded.Summary.HarvesterVersions)
			}
			if len(decoded.Sessions) != 1 || string(decoded.Sessions[0].SessionID) != fixtures.SessionID || decoded.Summary.Indexed != fixture.Indexed {
				t.Fatalf("populated command result = %+v", decoded)
			}
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal([]byte(out), &envelope); err != nil {
				t.Fatal(err)
			}
			var summary map[string]json.RawMessage
			if err := json.Unmarshal(envelope["summary"], &summary); err != nil {
				t.Fatal(err)
			}
			if _, exists := summary["IndexVersion"]; exists {
				t.Fatal("legacy scalar summary.IndexVersion remains")
			}
			if _, exists := summary["MetadataVersion"]; !exists {
				t.Fatal("unrelated MetadataVersion key removed")
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, []byte(fixtures.Transcript)) {
				t.Fatalf("native fixture modified: %v", err)
			}
		})
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("missing required fixture %q", name)
		}
	}
}

func TestPrintSummaryHarvesterTargets(t *testing.T) {
	t.Parallel()
	result := &ingest.PipelineResult{Summary: ingest.PipelineSummary{HarvesterVersions: maps.Clone(ingest.HarvesterVersionRegistry)}}
	var out bytes.Buffer
	printSummary(&out, result, false, false, "/fixture/output", "", nil, 0)
	text := out.String()
	if !strings.Contains(text, "harvester targets:\n") {
		t.Fatalf("targets not labeled: %s", text)
	}
	previous := -1
	for _, harness := range slices.Sorted(maps.Keys(ingest.HarvesterVersionRegistry)) {
		versions := result.Summary.HarvesterVersions[harness]
		line := fmt.Sprintf("  %s: adapter_version=%d indexer_version=%d index_version=%d\n", harness, versions.AdapterVersion, versions.IndexerVersion, versions.IndexVersion)
		position := strings.Index(text, line)
		if position <= previous {
			t.Fatalf("missing or unordered target %q: %s", line, text)
		}
		previous = position
	}
}
