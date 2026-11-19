package e2e

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/workflows/release_publish_contract.yaml
var releasePublishContractBytes []byte

type releasePublishContractFixture struct {
	Workflow string `yaml:"workflow"`
	Publish  struct {
		Job               string                       `yaml:"job"`
		Steps             nonEmptyStrings              `yaml:"steps"`
		CreateStep        string                       `yaml:"create_step"`
		CreateRunContains nonEmptyStrings              `yaml:"create_run_contains"`
		AllowedActions    nonEmptyStrings              `yaml:"allowed_actions"`
		ForbiddenCommands nonEmptyStrings              `yaml:"forbidden_commands"`
		StepContracts     []releasePublishStepContract `yaml:"step_contracts"`
	} `yaml:"publish"`
	ViewChecks []releaseViewCheck `yaml:"view_checks"`
	Staging    struct {
		Job              string `yaml:"job"`
		NotesEnv         string `yaml:"notes_env"`
		GoReleaserConfig string `yaml:"goreleaser_config"`
		GoReleaserNames  struct {
			ExtensionFormats nonEmptyStrings `yaml:"extension_formats"`
			BinaryFormat     string          `yaml:"binary_format"`
			ExeGoos          string          `yaml:"exe_goos"`
			ExeSuffix        string          `yaml:"exe_suffix"`
			PackageGoos      string          `yaml:"package_goos"`
		} `yaml:"goreleaser_names"`
		Version      string                      `yaml:"version"`
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

// releasePublishStepContract is one publish step's gate and command. An
// unconditional step has no if:, and only a step with ContinueOnError may set
// continue-on-error.
type releasePublishStepContract struct {
	Name            string   `yaml:"name"`
	ID              string   `yaml:"id"`
	Unconditional   bool     `yaml:"unconditional"`
	ContinueOnError bool     `yaml:"continue_on_error"`
	IfContains      []string `yaml:"if_contains"`
	IfExcludes      []string `yaml:"if_excludes"`
	RunContains     []string `yaml:"run_contains"`
	RunExcludes     []string `yaml:"run_excludes"`
}

// releaseViewCheck runs one release-state step against stubbed gh output.
type releaseViewCheck struct {
	Step     string            `yaml:"step"`
	Args     string            `yaml:"args"`
	Required nonEmptyStrings   `yaml:"required"`
	Cases    []releaseViewCase `yaml:"cases"`
}

type releaseViewCase struct {
	Name           string `yaml:"name"`
	Response       string `yaml:"response"`
	APIExit        int    `yaml:"api_exit"`
	ExpectedExit   int    `yaml:"expected_exit"`
	ExpectedOutput string `yaml:"expected_output"`
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

	assertReleasePublishStepContracts(t, fixture.Workflow, steps, fixture.Publish.Steps, fixture.Publish.StepContracts)

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
	notesEnv := yamlMappingValue(yamlMappingValue(yamlMappingValue(yamlMappingValue(doc, "jobs"), staging.Job), "env"), staging.NotesEnv)
	if notesEnv == nil || notesEnv.Value != staging.Notes {
		t.Fatalf("%s: jobs.%s.env.%s = %v, want %q", fixture.Workflow, staging.Job, staging.NotesEnv, notesEnv, staging.Notes)
	}

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
				cmd.Env = append(os.Environ(), "RUNNER_TEMP="+runnerTemp, staging.NotesEnv+"="+staging.Notes)
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

func assertReleasePublishStepContracts(t *testing.T, workflow string, steps []*yaml.Node, names []string, contracts []releasePublishStepContract) {
	t.Helper()
	covered := make(map[string]bool)
	for _, contract := range contracts {
		if contract.Name == "" || covered[contract.Name] || !slices.Contains(names, contract.Name) {
			t.Fatalf("%s: step contract %q is empty, duplicated or names no publish step", workflow, contract.Name)
		}
		covered[contract.Name] = true
		step := workflowStepNode(t, steps, contract.Name)
		context := fmt.Sprintf("%s: publish step %q", workflow, contract.Name)

		if contract.ID != "" {
			if id := yamlMappingValue(step, "id"); id == nil || id.Value != contract.ID {
				t.Fatalf("%s: id = %v, want %q", context, id, contract.ID)
			}
		}
		continueOnError := yamlMappingValue(step, "continue-on-error")
		continues := false
		if continueOnError != nil {
			if err := continueOnError.Decode(&continues); err != nil || !continues {
				t.Fatalf("%s: continue-on-error = %q, want the boolean true or no key", context, continueOnError.Value)
			}
		}
		if contract.ContinueOnError != continues {
			t.Fatalf("%s: continue-on-error = %v, want set to true only when the contract says so (%v)", context, continueOnError, contract.ContinueOnError)
		}

		condition := yamlMappingValue(step, "if")
		switch {
		case contract.Unconditional:
			if condition != nil {
				t.Fatalf("%s: must run unconditionally, has if: %q", context, condition.Value)
			}
		case condition == nil:
			t.Fatalf("%s: must be gated, has no if:", context)
		default:
			// A gate is a conjunction of step outcomes: an || or a status
			// function would let the step run past a failed predecessor.
			for _, banned := range append([]string{"||", "()"}, contract.IfExcludes...) {
				if strings.Contains(condition.Value, banned) {
					t.Fatalf("%s: if: %q must not contain %q", context, condition.Value, banned)
				}
			}
			for _, required := range contract.IfContains {
				if !strings.Contains(condition.Value, required) {
					t.Fatalf("%s: if: %q must contain %q", context, condition.Value, required)
				}
			}
		}

		run := yamlMappingValue(step, "run")
		if run == nil {
			if len(contract.RunContains)+len(contract.RunExcludes) > 0 {
				t.Fatalf("%s: has no run, but its contract names run fragments", context)
			}
			continue
		}
		for _, required := range contract.RunContains {
			if !strings.Contains(run.Value, required) {
				t.Fatalf("%s: run must contain %q, got %q", context, required, run.Value)
			}
		}
		for _, banned := range contract.RunExcludes {
			if strings.Contains(run.Value, banned) {
				t.Fatalf("%s: run must not contain %q, got %q", context, banned, run.Value)
			}
		}
	}
	for _, name := range names {
		if !covered[name] {
			t.Errorf("%s: publish step %q has no step contract", workflow, name)
		}
	}
}

// TestReleasePublishViewChecks runs each release-state step's real script and
// jq predicate with a stubbed gh: the rerun completeness check, the refusal
// of an existing release or draft, the draft check before publishing and the
// published check after it.
func TestReleasePublishViewChecks(t *testing.T) {
	t.Parallel()
	fixture := loadReleasePublishContractFixture(t)
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), fixture.Workflow))
	steps := workflowJobSteps(t, doc, fixture.Publish.Job, fixture.Workflow)
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("release: locate bash for the release-state steps: %v", err)
	}
	// The pool runner image and GitHub-hosted runners both ship jq; a
	// missing jq fails rather than skipping the predicates.
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("release: the release-state checks need jq on PATH to apply the workflow's --jq filters: %v", err)
	}
	if len(fixture.ViewChecks) == 0 {
		t.Fatal("release: the publish fixture must list view checks")
	}
	for _, check := range fixture.ViewChecks {
		script := workflowStepRun(t, steps, check.Step)
		seen := make(map[string]bool)
		for _, c := range check.Cases {
			if c.Name == "" || seen[c.Name] {
				t.Fatalf("release: empty or duplicate case %q for %q", c.Name, check.Step)
			}
			seen[c.Name] = true
		}
		for _, name := range check.Required {
			if !seen[name] {
				t.Fatalf("release: required case %q for %q is missing", name, check.Step)
			}
		}
		t.Run(check.Step, func(t *testing.T) {
			t.Parallel()
			for _, c := range check.Cases {
				t.Run(c.Name, func(t *testing.T) {
					t.Parallel()
					runReleaseViewCase(t, bashPath, script, check.Args, c)
				})
			}
		})
	}
}

// releaseGHStub requires exactly TEST_ARGS before the final --jq filter, then
// applies that filter to TEST_RELEASE_JSON with jq, as gh does.
const releaseGHStub = `#!/bin/sh
filter=$(eval "printf '%s' \"\${$#}\"")
set -- $(printf '%s\n' "$@" | head -n $(($# - 1)))
[ "$*" = "$TEST_ARGS" ] || { printf 'stub gh got unexpected arguments: %s\n' "$*" >&2; exit 92; }
if [ "$TEST_API_EXIT" -ne 0 ]; then
  printf 'mock API unavailable\n' >&2
  exit "$TEST_API_EXIT"
fi
printf '%s\n' "$TEST_RELEASE_JSON" | jq -e -r "$filter" || exit 1
`

func runReleaseViewCase(t *testing.T, bashPath, script, args string, c releaseViewCase) {
	t.Helper()
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "gh"), releaseGHStub)
	cmd := exec.Command(bashPath, "--noprofile", "--norc", "-e", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TAG=v1.2.3",
		"GITHUB_REPOSITORY=peasant-labs/peasant",
		"TEST_ARGS="+args,
		"TEST_RELEASE_JSON="+c.Response,
		"TEST_API_EXIT="+strconv.Itoa(c.APIExit),
	)
	output, err := cmd.CombinedOutput()
	exit := 0
	if err != nil {
		failure, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("release: execute step: %v\n%s", err, output)
		}
		exit = failure.ExitCode()
	}
	if exit != c.ExpectedExit || !strings.Contains(string(output), c.ExpectedOutput) {
		t.Fatalf("release: step exit %d, want %d with %q:\n%s", exit, c.ExpectedExit, c.ExpectedOutput, output)
	}
}

// goReleaserNameConfig is the part of .goreleaser.yml that names the
// checksummed release files.
type goReleaserNameConfig struct {
	Builds []struct {
		Goos   []string `yaml:"goos"`
		Goarch []string `yaml:"goarch"`
		Ignore []struct {
			Goos   string `yaml:"goos"`
			Goarch string `yaml:"goarch"`
		} `yaml:"ignore"`
	} `yaml:"builds"`
	Archives []struct {
		NameTemplate    string   `yaml:"name_template"`
		Formats         []string `yaml:"formats"`
		FormatOverrides []struct {
			Goos    string   `yaml:"goos"`
			Formats []string `yaml:"formats"`
		} `yaml:"format_overrides"`
	} `yaml:"archives"`
	Checksum struct {
		NameTemplate string `yaml:"name_template"`
	} `yaml:"checksum"`
	Nfpms []struct {
		PackageName      string   `yaml:"package_name"`
		FileNameTemplate string   `yaml:"file_name_template"`
		Formats          []string `yaml:"formats"`
	} `yaml:"nfpms"`
}

// TestReleasePublishFixtureNamesFollowGoReleaser renders .goreleaser.yml's
// archive, package and checksum name templates for the fixture's version, so
// the fake dist the staging test builds cannot drift from what GoReleaser
// writes.
func TestReleasePublishFixtureNamesFollowGoReleaser(t *testing.T) {
	t.Parallel()
	fixture := loadReleasePublishContractFixture(t)
	staging := fixture.Staging
	data, err := os.ReadFile(filepath.Join(releaseWorkflowRepoRoot(t), staging.GoReleaserConfig))
	if err != nil {
		t.Fatal(err)
	}
	var config goReleaserNameConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatalf("release: parse %s: %v", staging.GoReleaserConfig, err)
	}
	if len(config.Builds) != 1 || len(config.Archives) != 1 || len(config.Nfpms) != 1 {
		t.Fatalf("release: %s must keep one build, one archive and one nfpm entry for this check; extend the test before adding more", staging.GoReleaserConfig)
	}
	if config.Checksum.NameTemplate != staging.Checksums {
		t.Fatalf("release: %s checksum name %q, fixture %q", staging.GoReleaserConfig, config.Checksum.NameTemplate, staging.Checksums)
	}
	render := func(template string, values map[string]string) string {
		t.Helper()
		out := template
		for key, value := range values {
			out = strings.ReplaceAll(out, "{{ ."+key+" }}", value)
		}
		if strings.Contains(out, "{{") {
			t.Fatalf("release: name template %q uses a field this check does not render", template)
		}
		return out
	}
	build, archive, nfpm := config.Builds[0], config.Archives[0], config.Nfpms[0]
	names := staging.GoReleaserNames
	var want []string
	for _, goos := range build.Goos {
		for _, goarch := range build.Goarch {
			ignored := false
			for _, ignore := range build.Ignore {
				ignored = ignored || (ignore.Goos == goos && ignore.Goarch == goarch)
			}
			if ignored {
				continue
			}
			values := map[string]string{"Version": staging.Version, "Os": goos, "Arch": goarch, "PackageName": nfpm.PackageName}
			formats := archive.Formats
			for _, override := range archive.FormatOverrides {
				if override.Goos == goos {
					formats = override.Formats
				}
			}
			name := render(archive.NameTemplate, values)
			for _, format := range formats {
				switch {
				case slices.Contains(names.ExtensionFormats, format):
					want = append(want, name+"."+format)
				case format == names.BinaryFormat && goos == names.ExeGoos:
					want = append(want, name+names.ExeSuffix)
				case format == names.BinaryFormat:
					want = append(want, name)
				default:
					t.Fatalf("release: archive format %q is not rendered by this check; add it to goreleaser_names", format)
				}
			}
			if goos == names.PackageGoos {
				for _, format := range nfpm.Formats {
					want = append(want, render(nfpm.FileNameTemplate, values)+"."+format)
				}
			}
		}
	}
	got := slices.Clone([]string(staging.Checksummed))
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("release: fixture checksummed names %q do not match %s rendered for %s: %q", got, staging.GoReleaserConfig, staging.Version, want)
	}
}
