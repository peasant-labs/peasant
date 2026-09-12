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
		RequiredNames         []string `yaml:"requiredNames"`
		ExpectedSummaryKeys   []string `yaml:"expectedSummaryKeys"`
		ExpectedHarvesterKeys []string `yaml:"expectedHarvesterKeys"`
		RequiredMutationNames []string `yaml:"requiredMutationNames"`
		Mutations             []struct {
			Name   string `yaml:"name"`
			Object string `yaml:"object"`
			From   string `yaml:"from"`
			To     string `yaml:"to"`
		} `yaml:"mutations"`
		SessionID  string `yaml:"sessionID"`
		Transcript string `yaml:"transcript"`
		Cases      []struct {
			Name    string `yaml:"name"`
			Command string `yaml:"command"`
			DryRun  bool   `yaml:"dryRun"`
			Indexed int    `yaml:"indexed"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(harvesterSummaryYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	mutationNames := make(map[string]bool)
	for _, mutation := range fixtures.Mutations {
		if mutation.Name == "" || mutationNames[mutation.Name] {
			t.Fatalf("invalid mutation fixture name %q", mutation.Name)
		}
		mutationNames[mutation.Name] = true
	}
	for _, name := range fixtures.RequiredMutationNames {
		if !mutationNames[name] {
			t.Fatalf("missing required mutation fixture %q", name)
		}
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
				// A forecast inspects an existing checkpointed database and
				// creates none, so the state it reads exists before the run.
				seedClosedStore(t, dir)
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
			if err := validateHarvesterSummaryJSON([]byte(out), fixtures.ExpectedSummaryKeys, fixtures.ExpectedHarvesterKeys); err != nil {
				t.Fatal(err)
			}
			for _, mutation := range fixtures.Mutations {
				t.Run(mutation.Name, func(t *testing.T) {
					var envelope map[string]json.RawMessage
					if err := json.Unmarshal([]byte(out), &envelope); err != nil {
						t.Fatal(err)
					}
					var summary map[string]json.RawMessage
					if err := json.Unmarshal(envelope["summary"], &summary); err != nil {
						t.Fatal(err)
					}
					switch mutation.Object {
					case "summary":
						renameHarvesterJSONKey(t, summary, mutation.From, mutation.To)
					case "harvester":
						var harvesters map[string]map[string]json.RawMessage
						if err := json.Unmarshal(summary["HarvesterVersions"], &harvesters); err != nil {
							t.Fatal(err)
						}
						// Mutate only one object: validating the first/last object alone
						// must not hide a bad member elsewhere in the map.
						harness := slices.Sorted(maps.Keys(harvesters))[len(harvesters)/2]
						renameHarvesterJSONKey(t, harvesters[harness], mutation.From, mutation.To)
						data, err := json.Marshal(harvesters)
						if err != nil {
							t.Fatal(err)
						}
						summary["HarvesterVersions"] = data
					default:
						t.Fatalf("unknown mutation object %q", mutation.Object)
					}
					data, err := json.Marshal(summary)
					if err != nil {
						t.Fatal(err)
					}
					envelope["summary"] = data
					mutated, err := json.Marshal(envelope)
					if err != nil {
						t.Fatal(err)
					}
					if err := validateHarvesterSummaryJSON(mutated, fixtures.ExpectedSummaryKeys, fixtures.ExpectedHarvesterKeys); err == nil || !strings.Contains(err.Error(), "keys") {
						t.Fatalf("raw JSON key mutation %q error = %v, want key membership rejection", mutation.Name, err)
					}
				})
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

// validateHarvesterSummaryJSON checks the public spelling independently from the
// production structs, whose JSON tags could otherwise change both sides of a test.
func validateHarvesterSummaryJSON(data []byte, summaryKeys, harvesterKeys []string) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	var summary map[string]json.RawMessage
	if err := json.Unmarshal(envelope["summary"], &summary); err != nil {
		return err
	}
	if err := validateHarvesterJSONKeys("summary", summary, summaryKeys); err != nil {
		return err
	}
	var harvesters map[string]map[string]json.RawMessage
	if err := json.Unmarshal(summary["HarvesterVersions"], &harvesters); err != nil {
		return err
	}
	if len(harvesters) == 0 {
		return fmt.Errorf("harvester keys cannot be checked against an empty target map")
	}
	for harness, versions := range harvesters {
		if err := validateHarvesterJSONKeys("harvester "+harness, versions, harvesterKeys); err != nil {
			return err
		}
	}
	return nil
}

func validateHarvesterJSONKeys(object string, value map[string]json.RawMessage, expected []string) error {
	want := slices.Sorted(slices.Values(expected))
	if len(want) == 0 || want[0] == "" || len(slices.Compact(slices.Clone(want))) != len(want) {
		return fmt.Errorf("%s expected keys fixture must contain distinct nonempty names", object)
	}
	if got := slices.Sorted(maps.Keys(value)); !slices.Equal(got, want) {
		return fmt.Errorf("%s keys = %v, want exact membership %v", object, got, want)
	}
	return nil
}

func renameHarvesterJSONKey(t *testing.T, object map[string]json.RawMessage, from, to string) {
	t.Helper()
	value, exists := object[from]
	if !exists {
		t.Fatalf("mutation source key %q is absent", from)
	}
	if _, exists := object[to]; exists || to == "" {
		t.Fatalf("mutation target key %q must be new and nonempty", to)
	}
	delete(object, from)
	object[to] = value
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
