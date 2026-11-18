package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/release-provenance.yaml
var releaseAdoptionFixture []byte

type provenanceConfig struct {
	AutomationAuthor string `yaml:"automation_author" json:"automation_author"`
	BotBranches      struct {
		Release string `yaml:"release" json:"release"`
	} `yaml:"bot_branches" json:"bot_branches"`
}

// TestReleaseCandidateProvenance executes the workflow's actual shell step.
// Git, yq, and the GitHub API are isolated dependencies; jq and the decision
// under test are real.
func TestReleaseCandidateProvenance(t *testing.T) {
	var fixture struct {
		Candidate string           `yaml:"provenance_candidate"`
		Config    provenanceConfig `yaml:"provenance_config"`
		Required  []string         `yaml:"provenance_required"`
		Cases     []struct {
			Name          string             `yaml:"name"`
			Candidate     string             `yaml:"candidate"`
			Config        *provenanceConfig  `yaml:"config"`
			Pages         [][]map[string]any `yaml:"pages"`
			RawOutput     string             `yaml:"raw_output"`
			APIExit       int                `yaml:"api_exit"`
			ExpectedExit  int                `yaml:"expected_exit"`
			ErrorContains string             `yaml:"error_contains"`
		} `yaml:"provenance_cases"`
	}
	if err := yaml.Unmarshal(releaseAdoptionFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release-batch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	job := provenanceMappingChild(provenanceMappingChild(doc.Content[0], "jobs"), "plan")
	steps := provenanceMappingChild(job, "steps")
	if steps == nil {
		t.Fatal("release plan has no steps")
	}
	var script string
	var tagCondition string
	for _, step := range steps.Content {
		if id := provenanceMappingChild(step, "id"); id != nil && id.Value == "provenance" {
			if provenanceMappingChild(step, "continue-on-error") != nil {
				t.Fatal("provenance must not continue on error")
			}
		}
		if name := provenanceMappingChild(step, "name"); name != nil && name.Value == "Make the annotated release tag" {
			if condition := provenanceMappingChild(step, "if"); condition != nil {
				tagCondition = condition.Value
			}
		}
	}
	if !strings.Contains(tagCondition, "steps.provenance.outcome == 'success'") {
		t.Fatal("tag creation must require provenance success")
	}
	for _, step := range steps.Content {
		if id := provenanceMappingChild(step, "id"); id != nil && id.Value == "provenance" {
			if run := provenanceMappingChild(step, "run"); run != nil {
				script = run.Value
			}
		}
	}
	if script == "" {
		t.Fatal("release candidate provenance step has no script")
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("empty or duplicate provenance case %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			if c.ExpectedExit != 0 && !strings.HasPrefix(c.ErrorContains, "::error::") {
				t.Fatal("failure case must name its workflow error")
			}
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			git := "#!/bin/sh\n[ \"$#\" -eq 2 ] && [ \"$1\" = show ] && [ \"$2\" = \"${PLAN_SOURCE}:.github/release-intent.yaml\" ] || exit 90\nprintf 'fixture config\\n'\n"
			yq := "#!/bin/sh\n[ \"$#\" -eq 2 ] && [ \"$1\" = '-o=json' ] && [ \"$2\" = '.' ] || exit 91\ncat >/dev/null\nprintf '%s\\n' \"$TEST_CONFIG_JSON\"\n"
			gh := "#!/bin/sh\n[ \"$#\" -eq 3 ] && [ \"$1\" = api ] && [ \"$2\" = --paginate ] && [ \"$3\" = \"repos/${GITHUB_REPOSITORY}/commits/${CANDIDATE}/pulls?per_page=100\" ] && [ \"$GH_TOKEN\" = readonly-test-token ] || exit 92\nprintf '%s' \"$TEST_API_OUTPUT\"\nexit \"$TEST_API_EXIT\"\n"
			for name, content := range map[string]string{"git": git, "yq": yq, "gh": gh} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			config := fixture.Config
			if c.Config != nil {
				config = *c.Config
			}
			configJSON, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			for _, page := range c.Pages {
				data, err := json.Marshal(page)
				if err != nil {
					t.Fatal(err)
				}
				output.Write(data)
				output.WriteByte('\n')
			}
			if c.RawOutput != "" {
				output.Reset()
				output.WriteString(c.RawOutput)
			}
			candidate := fixture.Candidate
			if c.Candidate != "" {
				candidate = c.Candidate
			}
			cmd := exec.Command("bash", "-e", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GH_TOKEN=readonly-test-token", "GITHUB_REPOSITORY=peasant-labs/peasant",
				"CANDIDATE="+candidate, "PLAN_SOURCE=plan-source", "RUNNER_TEMP="+dir,
				"TEST_CONFIG_JSON="+string(configJSON), "TEST_API_OUTPUT="+output.String(),
				"TEST_API_EXIT="+strconv.Itoa(c.APIExit))
			actual, err := cmd.CombinedOutput()
			exit := 0
			if err != nil {
				if failure, ok := err.(*exec.ExitError); ok {
					exit = failure.ExitCode()
				} else {
					t.Fatalf("execute provenance step: %v\n%s", err, actual)
				}
			}
			if exit != c.ExpectedExit || (c.ExpectedExit != 0 && (c.ErrorContains == "" || !strings.Contains(string(actual), c.ErrorContains))) {
				t.Fatalf("provenance step exit %d, want %d with %q:\n%s", exit, c.ExpectedExit, c.ErrorContains, actual)
			}
		})
	}
	for _, name := range fixture.Required {
		if !seen[name] {
			t.Errorf("required provenance case %q missing", name)
		}
	}
}

func provenanceMappingChild(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
