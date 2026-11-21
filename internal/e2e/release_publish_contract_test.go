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
		Job               string                   `yaml:"job"`
		ConcurrencyGroup  string                   `yaml:"concurrency_group"`
		VerifyJob         string                   `yaml:"verify_job"`
		Needs             nonEmptyStrings          `yaml:"needs"`
		RunsOn            string                   `yaml:"runs_on"`
		Permissions       []workflowEnvExpectation `yaml:"permissions"`
		JobEnv            []workflowEnvExpectation `yaml:"job_env"`
		InfraRef          string                   `yaml:"infra_ref"`
		Steps             nonEmptyStrings          `yaml:"steps"`
		AllowedActions    nonEmptyStrings          `yaml:"allowed_actions"`
		ToolCheckoutStep  string                   `yaml:"tool_checkout_step"`
		ToolCheckoutWith  []workflowEnvExpectation `yaml:"tool_checkout_with"`
		SetupGoStep       string                   `yaml:"setup_go_step"`
		SetupGoWith       []workflowEnvExpectation `yaml:"setup_go_with"`
		BuildStep         string                   `yaml:"build_step"`
		BuildEnv          []workflowEnvExpectation `yaml:"build_env"`
		BuildRun          string                   `yaml:"build_run"`
		DownloadStep      string                   `yaml:"download_step"`
		DownloadWith      []workflowEnvExpectation `yaml:"download_with"`
		PublishStep       string                   `yaml:"publish_step"`
		PublishEnv        []workflowEnvExpectation `yaml:"publish_env"`
		PublishRun        string                   `yaml:"publish_run"`
		ForbiddenCommands nonEmptyStrings          `yaml:"forbidden_commands"`
	} `yaml:"publish"`
	Staging struct {
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
	if !is40HexSHA(fixture.Publish.InfraRef) {
		t.Fatalf("release: publish fixture infra_ref %q must be a 40-hex commit SHA", fixture.Publish.InfraRef)
	}
	return fixture
}

func is40HexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// TestReleasePublishUsesFiveStepPublishRelease pins the publish job to the
// exact five-step release-intent publish-release contract: tool checkout from
// the pinned infra commit, setup-go with cache disabled, build with GOWORK=off
// before downloading release-dist, and publish-release with no conditionals,
// continue-on-error, or retired gh release glue.
func TestReleasePublishUsesFiveStepPublishRelease(t *testing.T) {
	t.Parallel()
	fixture := loadReleasePublishContractFixture(t)
	pub := fixture.Publish
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), fixture.Workflow))

	concurrency := yamlMappingValue(doc, "concurrency")
	assertYAMLMappingValue(t, concurrency, "group", pub.ConcurrencyGroup, fixture.Workflow+" concurrency")
	assertYAMLMappingValue(t, concurrency, "cancel-in-progress", "false", fixture.Workflow+" concurrency")

	jobs := yamlMappingValue(doc, "jobs")
	job := yamlMappingValue(jobs, pub.Job)
	if job == nil {
		t.Fatalf("%s: missing job %q", fixture.Workflow, pub.Job)
	}
	if yamlMappingValue(job, "if") != nil {
		t.Fatalf("%s: jobs.%s must not define an if condition", fixture.Workflow, pub.Job)
	}
	if yamlMappingValue(job, "continue-on-error") != nil {
		t.Fatalf("%s: jobs.%s must not define continue-on-error", fixture.Workflow, pub.Job)
	}
	assertYAMLMappingValue(t, job, "runs-on", pub.RunsOn, fixture.Workflow+" jobs."+pub.Job)
	gotNeeds := workflowNeedsValues(yamlMappingValue(job, "needs"))
	if !sameStringSet(gotNeeds, pub.Needs) {
		t.Fatalf("%s: jobs.%s needs = %v, want exactly %v", fixture.Workflow, pub.Job, gotNeeds, pub.Needs)
	}
	assertYAMLMappingExact(t, yamlMappingValue(job, "permissions"), pub.Permissions, fixture.Workflow+" jobs."+pub.Job+" permissions")
	assertYAMLMappingExact(t, yamlMappingValue(job, "env"), pub.JobEnv, fixture.Workflow+" jobs."+pub.Job+" env")

	steps := workflowJobSteps(t, doc, pub.Job, fixture.Workflow)
	var names []string
	for _, step := range steps {
		name := yamlMappingValue(step, "name")
		if name == nil {
			t.Fatalf("%s: every %s step must be named", fixture.Workflow, pub.Job)
		}
		if yamlMappingValue(step, "if") != nil {
			t.Fatalf("%s: %s step %q must not define an if condition", fixture.Workflow, pub.Job, name.Value)
		}
		if yamlMappingValue(step, "continue-on-error") != nil {
			t.Fatalf("%s: %s step %q must not define continue-on-error", fixture.Workflow, pub.Job, name.Value)
		}
		names = append(names, name.Value)
		if uses := yamlMappingValue(step, "uses"); uses != nil {
			action, ref, ok := strings.Cut(uses.Value, "@")
			if !ok || !slices.Contains(pub.AllowedActions, action) {
				t.Fatalf("%s: %s step %q uses %q; only %v may run in the publish job", fixture.Workflow, pub.Job, name.Value, action, pub.AllowedActions)
			}
			if !is40HexSHA(ref) {
				t.Fatalf("%s: %s step %q uses %q, must pin a 40-hex commit SHA", fixture.Workflow, pub.Job, name.Value, uses.Value)
			}
		}
	}
	if !slices.Equal(names, pub.Steps) {
		t.Fatalf("%s: %s steps = %q, want exactly %q", fixture.Workflow, pub.Job, names, pub.Steps)
	}

	toolCheckout := workflowStepNode(t, steps, pub.ToolCheckoutStep)
	assertYAMLMappingExact(t, yamlMappingValue(toolCheckout, "with"), pub.ToolCheckoutWith, fixture.Workflow+" "+pub.ToolCheckoutStep+" with")

	verifySteps := workflowJobSteps(t, doc, pub.VerifyJob, fixture.Workflow)
	verifyCheckout := workflowStepNode(t, verifySteps, pub.ToolCheckoutStep)
	assertYAMLMappingValue(t, yamlMappingValue(verifyCheckout, "with"), "ref", pub.InfraRef, fixture.Workflow+" "+pub.VerifyJob+" "+pub.ToolCheckoutStep+" with")

	setupGo := workflowStepNode(t, steps, pub.SetupGoStep)
	assertYAMLMappingExact(t, yamlMappingValue(setupGo, "with"), pub.SetupGoWith, fixture.Workflow+" "+pub.SetupGoStep+" with")

	buildStep := workflowStepNode(t, steps, pub.BuildStep)
	assertYAMLMappingExact(t, yamlMappingValue(buildStep, "env"), pub.BuildEnv, fixture.Workflow+" "+pub.BuildStep+" env")
	if got := strings.TrimSpace(workflowStepRun(t, steps, pub.BuildStep)); got != pub.BuildRun {
		t.Fatalf("%s: %q run = %q, want %q", fixture.Workflow, pub.BuildStep, got, pub.BuildRun)
	}

	downloadStep := workflowStepNode(t, steps, pub.DownloadStep)
	assertYAMLMappingExact(t, yamlMappingValue(downloadStep, "with"), pub.DownloadWith, fixture.Workflow+" "+pub.DownloadStep+" with")

	publishStep := workflowStepNode(t, steps, pub.PublishStep)
	assertYAMLMappingExact(t, yamlMappingValue(publishStep, "env"), pub.PublishEnv, fixture.Workflow+" "+pub.PublishStep+" env")
	if got := strings.TrimSpace(workflowStepRun(t, steps, pub.PublishStep)); got != pub.PublishRun {
		t.Fatalf("%s: %q run = %q, want %q", fixture.Workflow, pub.PublishStep, got, pub.PublishRun)
	}

	for _, jobName := range releaseWorkflowJobNames(t, doc) {
		for _, step := range workflowJobSteps(t, doc, jobName, fixture.Workflow) {
			run := yamlMappingValue(step, "run")
			if run == nil {
				continue
			}
			for _, forbidden := range pub.ForbiddenCommands {
				if strings.Contains(run.Value, forbidden) {
					t.Fatalf("%s: job %s runs %q; release publication uses release-intent publish-release", fixture.Workflow, jobName, forbidden)
				}
			}
		}
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
