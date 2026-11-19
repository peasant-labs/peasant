package e2e

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/workflows/release_publish_contract.yaml
var releasePublishContractBytes []byte

type releasePublishContractFixture struct {
	Workflow string `yaml:"workflow"`
	Publish  struct {
		Job               string          `yaml:"job"`
		Steps             nonEmptyStrings `yaml:"steps"`
		CreateStep        string          `yaml:"create_step"`
		CreateRunContains nonEmptyStrings `yaml:"create_run_contains"`
		AllowedActions    nonEmptyStrings `yaml:"allowed_actions"`
		ForbiddenCommands nonEmptyStrings `yaml:"forbidden_commands"`
	} `yaml:"publish"`
	Staging struct {
		Job          string                      `yaml:"job"`
		StageStep    string                      `yaml:"stage_step"`
		VerifyStep   string                      `yaml:"verify_step"`
		Notes        string                      `yaml:"notes"`
		Checksums    string                      `yaml:"checksums"`
		Checksummed  nonEmptyStrings             `yaml:"checksummed"`
		Metadata     nonEmptyStrings             `yaml:"metadata"`
		BuildOutputs nonEmptyStrings             `yaml:"build_outputs"`
		Cases        []releaseStagingFixtureCase `yaml:"cases"`
	} `yaml:"staging"`
}

type releaseStagingFixtureCase struct {
	Name        string   `yaml:"name"`
	AddTopLevel []string `yaml:"add_top_level"`
	Remove      []string `yaml:"remove"`
	Corrupt     []string `yaml:"corrupt"`
	WantOK      bool     `yaml:"want_ok"`
}

func loadReleasePublishContractFixture(t *testing.T) releasePublishContractFixture {
	t.Helper()
	var fixture releasePublishContractFixture
	decoder := yaml.NewDecoder(bytes.NewReader(releasePublishContractBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("release: parse publish contract fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("release: publish contract fixture must have exact EOF: %v", err)
	}
	return fixture
}

// TestReleasePublishCreatesDraftAndAssetsInOneCommand pins the publish job to
// one gh release create that makes the draft and uploads its assets, followed
// by a gated publish. A second creator or an upload action finds the release
// by tag and can fill a release that is never published.
func TestReleasePublishCreatesDraftAndAssetsInOneCommand(t *testing.T) {
	t.Parallel()
	fixture := loadReleasePublishContractFixture(t)
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), fixture.Workflow))
	steps := workflowJobSteps(t, doc, fixture.Publish.Job, fixture.Workflow)

	var names []string
	for _, step := range steps {
		name := yamlMappingValue(step, "name")
		if name == nil {
			t.Fatalf("%s: every %s step must be named", fixture.Workflow, fixture.Publish.Job)
		}
		names = append(names, name.Value)
		if uses := yamlMappingValue(step, "uses"); uses != nil {
			action, _, _ := strings.Cut(uses.Value, "@")
			if !slices.Contains(fixture.Publish.AllowedActions, action) {
				t.Fatalf("%s: %s step %q uses %q; only %v may run in the publish job", fixture.Workflow, fixture.Publish.Job, name.Value, action, fixture.Publish.AllowedActions)
			}
		}
	}
	if !slices.Equal(names, fixture.Publish.Steps) {
		t.Fatalf("%s: %s steps = %q, want exactly %q", fixture.Workflow, fixture.Publish.Job, names, fixture.Publish.Steps)
	}

	create := workflowStepRun(t, steps, fixture.Publish.CreateStep)
	for _, required := range fixture.Publish.CreateRunContains {
		if !strings.Contains(create, required) {
			t.Fatalf("%s: %q must pass %s, got %q", fixture.Workflow, fixture.Publish.CreateStep, required, create)
		}
	}

	creators := 0
	for _, jobName := range releaseWorkflowJobNames(t, doc) {
		for _, step := range workflowJobSteps(t, doc, jobName, fixture.Workflow) {
			run := yamlMappingValue(step, "run")
			if run == nil {
				continue
			}
			creators += strings.Count(run.Value, "gh release create")
			for _, forbidden := range fixture.Publish.ForbiddenCommands {
				if strings.Contains(run.Value, forbidden) {
					t.Fatalf("%s: job %s runs %q; assets reach the release only through the one create", fixture.Workflow, jobName, forbidden)
				}
			}
		}
	}
	if creators != 1 {
		t.Fatalf("%s: found %d gh release create commands, want exactly one", fixture.Workflow, creators)
	}
}

// TestReleaseBuildStagesFlatReleaseAssets runs the build job's staging steps
// against a fake GoReleaser dist: the publisher's one asset glob must match
// only files, exactly the checksummed set plus checksums.txt and the
// GoReleaser metadata, with the notes kept out of it.
func TestReleaseBuildStagesFlatReleaseAssets(t *testing.T) {
	t.Parallel()
	fixture := loadReleasePublishContractFixture(t)
	staging := fixture.Staging
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), fixture.Workflow))
	steps := workflowJobSteps(t, doc, staging.Job, fixture.Workflow)
	stage := workflowStepRun(t, steps, staging.StageStep)
	verify := workflowStepRun(t, steps, staging.VerifyStep)
	assertStepBefore(t, steps, "Verify release checksums", staging.StageStep)
	assertStepBefore(t, steps, staging.StageStep, staging.VerifyStep)
	assertStepBefore(t, steps, staging.VerifyStep, "Hand release artifacts to the publisher")

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("release: locate bash for the staging steps: %v", err)
	}
	if len(staging.Cases) == 0 {
		t.Fatal("release: the staging fixture must list cases")
	}
	for _, tc := range staging.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			workspace := t.TempDir()
			runnerTemp := t.TempDir()
			writeFakeGoReleaserDist(t, filepath.Join(workspace, "dist"), staging.Checksums, staging.Notes, staging.Checksummed, staging.Metadata, staging.BuildOutputs, tc)

			var output bytes.Buffer
			ok := true
			for _, script := range []string{stage, verify} {
				cmd := exec.Command(bashPath, "--noprofile", "--norc", "-e", "-c", script)
				cmd.Dir = workspace
				cmd.Env = append(os.Environ(), "RUNNER_TEMP="+runnerTemp)
				cmd.Stdout, cmd.Stderr = &output, &output
				if err := cmd.Run(); err != nil {
					ok = false
					break
				}
			}
			if ok != tc.WantOK {
				t.Fatalf("release: staging succeeded=%v, want %v\n%s", ok, tc.WantOK, output.String())
			}
			if !tc.WantOK {
				return
			}
			assertDirEntries(t, filepath.Join(workspace, "dist"), []string{"assets", staging.Notes})
			want := append(append([]string{staging.Checksums}, staging.Checksummed...), staging.Metadata...)
			assertDirEntries(t, filepath.Join(workspace, "dist", "assets"), want)
		})
	}
}

func writeFakeGoReleaserDist(t *testing.T, dist, checksums, notes string, checksummed, metadata, buildOutputs []string, tc releaseStagingFixtureCase) {
	t.Helper()
	write := func(rel, contents string) {
		t.Helper()
		path := filepath.Join(dist, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var sums strings.Builder
	for _, name := range checksummed {
		contents := "asset " + name + "\n"
		sum := sha256.Sum256([]byte(contents))
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
		write(name, contents)
	}
	write(checksums, sums.String())
	for _, name := range metadata {
		write(name, "metadata "+name+"\n")
	}
	for _, rel := range buildOutputs {
		write(rel, "build output\n")
	}
	write(notes, "release notes\n")
	for _, name := range tc.AddTopLevel {
		write(name, "unexpected\n")
	}
	for _, name := range tc.Remove {
		if err := os.Remove(filepath.Join(dist, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range tc.Corrupt {
		write(name, "changed after checksums\n")
	}
}

func assertDirEntries(t *testing.T, dir string, want []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	want = slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("release: %s holds %q, want exactly %q", dir, got, want)
	}
}

func releaseWorkflowJobNames(t *testing.T, doc *yaml.Node) []string {
	t.Helper()
	jobs := yamlMappingValue(doc, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		t.Fatal("release: workflow must define jobs")
	}
	var names []string
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		if steps := yamlMappingValue(jobs.Content[i+1], "steps"); steps != nil {
			names = append(names, jobs.Content[i].Value)
		}
	}
	return names
}
