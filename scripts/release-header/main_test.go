package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/release_header.yaml
var fixtureData []byte

//go:embed testdata/release_header.manifest.yaml
var manifestData []byte

type headerCase struct {
	Name          string `yaml:"name"`
	Mounted       bool   `yaml:"mounted"`
	Tag           string `yaml:"tag"`
	Changelog     string `yaml:"changelog"`
	Header        string `yaml:"header"`
	Commits       string `yaml:"commits"`
	Body          string `yaml:"body"`
	ErrorContains string `yaml:"error_contains"`
}

func loadCases(t *testing.T) []headerCase {
	t.Helper()
	var fixture struct {
		Cases []headerCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(fixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("parse release header fixtures: %v", err)
	}
	var manifest struct {
		RequiredNames        []string `yaml:"required_names"`
		RequiredMountedNames []string `yaml:"required_mounted_names"`
		RequiredBodyNames    []string `yaml:"required_body_names"`
	}
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("parse release header manifest: %v", err)
	}
	if len(manifest.RequiredNames) == 0 || len(manifest.RequiredMountedNames) == 0 || len(manifest.RequiredBodyNames) == 0 {
		t.Fatal("release header manifest must require section, mounted workflow and body cases")
	}
	present := make(map[string]bool, len(fixture.Cases))
	mounted := make(map[string]bool, len(fixture.Cases))
	bodies := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if c.Name == "" || present[c.Name] {
			t.Fatalf("blank or repeated release header case: %+v", c)
		}
		if c.Tag == "" || c.Changelog == "" {
			t.Fatalf("release header case missing input: %+v", c)
		}
		if (c.Header == "") == (c.ErrorContains == "") {
			t.Fatalf("invalid or repeated release header case: %+v", c)
		}
		present[c.Name] = true
		mounted[c.Name] = c.Mounted
		bodies[c.Name] = c.Mounted && c.Body != "" && c.Commits != ""
	}
	seen := make(map[string]bool, len(manifest.RequiredNames))
	for _, name := range manifest.RequiredNames {
		if name == "" || seen[name] || !present[name] {
			t.Fatalf("missing, blank or repeated required release header case %q", name)
		}
		seen[name] = true
	}
	for _, name := range manifest.RequiredMountedNames {
		if !present[name] || !mounted[name] {
			t.Fatalf("required release header case %q must exercise the mounted workflow", name)
		}
	}
	for _, name := range manifest.RequiredBodyNames {
		if !present[name] || !bodies[name] {
			t.Fatalf("required release header case %q must assert a mounted body and commit list", name)
		}
	}
	return fixture.Cases
}

func TestReleaseHeaderSections(t *testing.T) {
	for _, c := range loadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			got, err := section([]byte(c.Changelog), c.Tag)
			if c.ErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), c.ErrorContains) || len(got) != 0 {
					t.Fatalf("section error = %v, output = %q; want error containing %q and no output", err, got, c.ErrorContains)
				}
				return
			}
			if err != nil || string(got) != c.Header {
				t.Fatalf("section = %q, error = %v; want %q", got, err, c.Header)
			}
		})
	}
}

func TestMissingChangelogFailsWithoutOutput(t *testing.T) {
	path := filepath.Join(testDir(t), "absent-CHANGELOG.md")
	var output, diagnostics bytes.Buffer
	err := run([]string{"-changelog", path, "-tag", "v2.4.0"}, &output, &diagnostics)
	if err == nil || !strings.Contains(err.Error(), path) || output.Len() != 0 {
		t.Fatalf("missing changelog: error = %v, output = %q; want path in error and no output", err, output.String())
	}
}

type workflow struct {
	Jobs map[string]struct {
		Env   map[string]string `yaml:"env"`
		Steps []workflowStep    `yaml:"steps"`
	} `yaml:"jobs"`
}

type workflowStep struct {
	Name            string            `yaml:"name"`
	Run             string            `yaml:"run"`
	If              string            `yaml:"if"`
	ContinueOnError string            `yaml:"continue-on-error"`
	Env             map[string]string `yaml:"env"`
}

func readWorkflow(t *testing.T, path string) workflow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var result workflow
	if err := yaml.Unmarshal(data, &result); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return result
}

func findStep(t *testing.T, workflow workflow, job, name string) (workflowStep, int) {
	t.Helper()
	steps := workflow.Jobs[job].Steps
	for i, step := range steps {
		if step.Name == name {
			if step.If != "" || step.ContinueOnError != "" {
				t.Fatalf("%s: %s must run unconditionally and fail closed", job, name)
			}
			return step, i
		}
	}
	t.Fatalf("%s: missing step %q", job, name)
	return workflowStep{}, -1
}

// releaseHeaderEnv is the GoReleaser template environment. The field name is
// the template key {{ .Env.PEASANT_RELEASE_HEADER }}; the tag is that same key.
type releaseHeaderEnv struct {
	PEASANT_RELEASE_HEADER string `json:"PEASANT_RELEASE_HEADER"`
}

// releaseHeaderContext is the data the release header template reads.
type releaseHeaderContext struct {
	Env releaseHeaderEnv `json:"Env"`
}

// TestMountedReleaseHeader runs the actual workflow shell steps in isolated
// synthetic projects. The only stub is GoReleaser, which records its arguments
// and environment instead of publishing. No Git repository or service is used.
func TestMountedReleaseHeader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the release workflow runs its bash steps on Linux")
	}
	root := filepath.Join("..", "..")
	release := readWorkflow(t, filepath.Join(root, ".github", "workflows", "release.yml"))
	publish, _ := findStep(t, release, "build", "Run goreleaser")
	if publish.Env["TAG"] != "${{ github.ref_name }}" {
		t.Fatal("release build must use the triggering tag")
	}
	// The runner gives every step its job's env, so the step runs with the
	// build job's literal values; an expression there cannot be resolved here.
	var jobEnv []string
	for key, value := range release.Jobs["build"].Env {
		if strings.Contains(value, "${{") {
			t.Fatalf("build job env %s = %q is an expression this mounted test cannot resolve", key, value)
		}
		jobEnv = append(jobEnv, key+"="+value)
	}

	configBytes, err := os.ReadFile(filepath.Join(root, ".goreleaser.yml"))
	if err != nil {
		t.Fatalf("read GoReleaser config: %v", err)
	}
	var config struct {
		Release struct {
			Header string `yaml:"header"`
			Mode   string `yaml:"mode"`
		} `yaml:"release"`
		Changelog struct {
			Disable bool `yaml:"disable"`
		} `yaml:"changelog"`
	}
	if err := yaml.Unmarshal(configBytes, &config); err != nil {
		t.Fatalf("parse GoReleaser config: %v", err)
	}
	if config.Release.Mode != "keep-existing" || config.Changelog.Disable {
		t.Fatal("release must keep existing release notes on rerun and keep the generated commit list")
	}
	headerTemplate, err := template.New("header").Option("missingkey=error").Parse(config.Release.Header)
	if err != nil {
		t.Fatalf("parse GoReleaser header template: %v", err)
	}
	program, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read production release-header command: %v", err)
	}
	for _, c := range loadCases(t) {
		if !c.Mounted {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			dir := testDir(t)
			writeTestFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/synthetic\n\ngo 1.26.0\n", 0o600)
			writeTestFile(t, filepath.Join(dir, "scripts", "release-header", "main.go"), string(program), 0o600)
			writeTestFile(t, filepath.Join(dir, "CHANGELOG.md"), c.Changelog, 0o600)
			runner := filepath.Join(dir, "runner")
			capture := filepath.Join(dir, "published-header")
			argsFile := filepath.Join(dir, "publisher-args")
			stub := "#!/bin/sh\nmkdir -p dist\nprintf '%s' \"$PEASANT_RELEASE_HEADER\" > \"$CAPTURE\"\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\n"
			writeTestFile(t, filepath.Join(runner, "goreleaser-bin", "goreleaser"), stub, 0o700)
			for _, step := range []workflowStep{publish} {
				cmd := exec.Command("bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", step.Run)
				cmd.Dir = dir
				cmd.Env = append(append(os.Environ(), jobEnv...), "TAG="+c.Tag, "VERSION="+c.Tag, "RUNNER_TEMP="+runner, "CAPTURE="+capture, "ARGS_FILE="+argsFile, "GOWORK=off")
				output, runErr := cmd.CombinedOutput()
				if c.ErrorContains != "" {
					if runErr == nil || !strings.Contains(string(output), c.ErrorContains) {
						t.Fatalf("%s: got %v, %q; want failure containing %q", step.Name, runErr, output, c.ErrorContains)
					}
					if _, statErr := os.Stat(capture); !os.IsNotExist(statErr) {
						t.Fatalf("%s: publisher ran after header failure: %v", step.Name, statErr)
					}
					continue
				}
				if runErr != nil {
					t.Fatalf("%s: %v\n%s", step.Name, runErr, output)
				}
			}
			header, err := os.ReadFile(capture)
			if c.ErrorContains != "" {
				return
			}
			if err != nil || string(header) != c.Header {
				t.Fatalf("publisher header = %q, error = %v; want %q", header, err, c.Header)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil || string(args) != "release\n--clean\n--skip=publish\n" {
				t.Fatalf("publisher arguments = %q, error = %v", args, err)
			}
			notes, err := os.ReadFile(filepath.Join(dir, "dist", "release-notes.md"))
			if err != nil || strings.TrimSuffix(string(notes), "\n") != c.Header {
				t.Fatalf("release notes = %q, error = %v; want %q", notes, err, c.Header)
			}
			if c.Body != "" {
				var rendered bytes.Buffer
				context := releaseHeaderContext{Env: releaseHeaderEnv{PEASANT_RELEASE_HEADER: string(header)}}
				if err := headerTemplate.Execute(&rendered, context); err != nil {
					t.Fatalf("render GoReleaser header: %v", err)
				}
				// GoReleaser v2.17.1 inserts one newline between its rendered
				// header and its independently generated release notes.
				body := rendered.String() + "\n" + c.Commits
				if body != c.Body {
					t.Fatalf("release body = %q; want %q", body, c.Body)
				}
			}
		})
	}
}

func testDir(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("RELEASE_HEADER_TEST_TMP_ROOT"); root != "" {
		dir, err := os.MkdirTemp(root, "release-header-")
		if err != nil {
			t.Fatalf("create retained synthetic test directory: %v", err)
		}
		return dir
	}
	return t.TempDir()
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(fmt.Errorf("write synthetic fixture %s: %w", path, err))
	}
}
