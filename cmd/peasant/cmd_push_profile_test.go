package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_cli/cases.yaml
var pushProfileCasesYAML []byte

//go:embed testdata/profile_cli/manifest.yaml
var pushProfileManifestYAML []byte

type pushProfileCase struct {
	Name                 string       `yaml:"name"`
	Description          string       `yaml:"description"`
	Mode                 string       `yaml:"mode"`
	Trace                bool         `yaml:"trace"`
	SeedSessions         bool         `yaml:"seedSessions"`
	Disabled             bool         `yaml:"disabled"`
	Timing               bool         `yaml:"timing"`
	ExistingFiles        bool         `yaml:"existingFiles"`
	ConfigOverride       string       `yaml:"configOverride"`
	PublishStatus        int          `yaml:"publishStatus"`
	ExpectError          bool         `yaml:"expectError"`
	Outcome              perf.Outcome `yaml:"outcome"`
	OutputFailure        bool         `yaml:"outputFailure"`
	TraceFailure         bool         `yaml:"traceFailure"`
	ExpectStdoutContains []string     `yaml:"expectStdoutContains"`
	ForbidStdoutContains []string     `yaml:"forbidStdoutContains"`
	ExpectStderrContains []string     `yaml:"expectStderrContains"`
	ForbidStderrContains []string     `yaml:"forbidStderrContains"`
	Stdout               string       `yaml:"stdout"`
	Stderr               string       `yaml:"stderr"`
}

type pushProfileInvalidCase struct {
	Name                string   `yaml:"name"`
	Description         string   `yaml:"description"`
	Shape               string   `yaml:"shape"`
	ExpectErrorContains []string `yaml:"expectErrorContains"`
}

type pushProfileFixtures struct {
	Config          string                   `yaml:"config"`
	SessionID       string                   `yaml:"sessionID"`
	ForbiddenInputs []string                 `yaml:"forbiddenInputs"`
	Cases           []pushProfileCase        `yaml:"cases"`
	InvalidCases    []pushProfileInvalidCase `yaml:"invalidCases"`
}

func loadPushProfileFixtures(t *testing.T) pushProfileFixtures {
	t.Helper()
	var fixtures pushProfileFixtures
	d := yaml.NewDecoder(bytes.NewReader(pushProfileCasesYAML))
	d.KnownFields(true)
	if err := d.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing YAML: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushProfileManifestYAML, "push CLI")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixtures.Cases {
		names = append(names, c.Name)
	}
	for _, c := range fixtures.InvalidCases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push CLI"); err != nil {
		t.Fatal(err)
	}
	shared, err := testutil.LoadProfileCLIFixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range shared.Names() {
		found := false
		for _, name := range names {
			found = found || name == required
		}
		if !found {
			t.Fatalf("missing shared CLI case %s", required)
		}
	}
	return fixtures
}

func TestPushCmd_Profile(t *testing.T) {
	fixtures := loadPushProfileFixtures(t)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			outputPath := filepath.Join(dir, "profile.json")
			tracePath := filepath.Join(dir, "PRIVATE_HISTORY_SENTINEL.jsonl")
			writeTestCredentials(t, dir)
			cfg := writeCfg(t, dir, "config.yaml", fixtures.Config)
			if c.ConfigOverride != "" {
				cfg = writeCfg(t, dir, "override.yaml", c.ConfigOverride)
			}
			if c.SeedSessions {
				cfg = seedUploadableSession(t, dir, fixtures.SessionID)
				seedEntryCarrying(t, dir, fixtures.SessionID, strings.Join(fixtures.ForbiddenInputs, " "))
			}
			captured := &capturedPublish{parts: map[string]string{}}
			if c.PublishStatus != 0 {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if !strings.Contains(r.URL.Path, "/transcripts/publish") {
						_, _ = w.Write([]byte(`{}`))
						return
					}
					captured.record(t, r)
					if c.OutputFailure {
						if err := os.Mkdir(outputPath, 0o700); err != nil {
							t.Error(err)
						}
					}
					if c.TraceFailure {
						if err := os.Mkdir(tracePath, 0o700); err != nil {
							t.Error(err)
						}
					}
					if c.PublishStatus != http.StatusCreated {
						w.WriteHeader(c.PublishStatus)
						_, _ = io.WriteString(w, strings.Join(fixtures.ForbiddenInputs, " "))
						return
					}
					receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), true)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(c.PublishStatus)
					_, _ = w.Write(receipt)
				}))
				t.Cleanup(server.Close)
				writeTestCredentialsFor(t, dir, server.URL)
			}
			args := []string{"--non-interactive", "--config", cfg, "--state-dir", dir}
			switch c.Mode {
			case "default":
				args = append(args, "--dry-run")
			case "quiet":
				args = append(args, "--dry-run", "--quiet")
			case "verbose":
				args = append(args, "--dry-run", "--verbose")
			case defaults.JSONFlagName:
				args = append(args, "--json")
			default:
				t.Fatalf("unknown mode %s", c.Mode)
			}
			if c.ExistingFiles {
				if err := os.WriteFile(outputPath, []byte("old profile"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tracePath, []byte("old trace"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if !c.Disabled {
				args = append(args, "--profile-output", outputPath)
			}
			if c.Trace {
				args = append(args, "--profile-trace", tracePath)
			}
			if c.Timing {
				args = append(args, "--timing")
			}
			out, stderr, err := executePushCmdSeparate(t, dir, args)
			assertNoProfileTemps(t, dir)
			if (err != nil) != c.ExpectError {
				t.Fatalf("push: %v\nstdout: %s\nstderr: %s", err, out, stderr)
			}
			if c.PublishStatus != 0 && len(captured.snapshot()) == 0 {
				t.Fatal("fake Village received no publish")
			}
			assertProfileText(t, out, c.ExpectStdoutContains, c.ForbidStdoutContains)
			assertProfileText(t, stderr, c.ExpectStderrContains, c.ForbidStderrContains)
			if c.Mode == defaults.JSONFlagName && !json.Valid([]byte(out)) {
				t.Errorf("stdout is not JSON: %s", out)
			}
			if c.Disabled {
				if out != c.Stdout || stderr != c.Stderr {
					t.Errorf("disabled output changed\nstdout: %q\nstderr: %q", out, stderr)
				}
				if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
					t.Errorf("disabled profile exists: %v", err)
				}
				if _, err := os.Stat(filepath.Join(string(defaults.ResolveStateDirPathWith(dir)), "logs")); !os.IsNotExist(err) {
					t.Errorf("disabled logs exist: %v", err)
				}
				return
			}
			if c.OutputFailure {
				if strings.Contains(stderr, "profile written to") {
					t.Error("claimed a profile that was not written")
				}
				return
			}
			data := readPrivateProfileFile(t, outputPath)
			forbidden := append(append([]string{}, fixtures.ForbiddenInputs...), dir)
			assertProfileText(t, string(data), nil, forbidden)
			var doc perf.ProfileDocument
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.FormatVersion != perf.JSONFormatVersion || doc.Producer.Command != "village push" {
				t.Fatalf("wrong profile identity: %+v", doc.Producer)
			}
			foundRun := false
			for _, span := range doc.Spans {
				if span.Stage == perf.StagePushRun {
					foundRun = true
					wantOutcome := c.Outcome
					if wantOutcome == "" {
						wantOutcome = perf.OutcomeOK
					}
					if err := wantOutcome.Validate(); err != nil {
						t.Fatal(err)
					}
					if span.Outcome != wantOutcome {
						t.Errorf("run outcome: %s", span.Outcome)
					}
				}
			}
			if !foundRun {
				t.Error("missing push.run span")
			}
			if c.Outcome == perf.OutcomeFailed && len(doc.Errors) == 0 {
				t.Error("failed run has no safe diagnostic")
			}
			if c.Trace && !c.TraceFailure {
				trace := readPrivateProfileFile(t, tracePath)
				assertProfileText(t, string(trace), nil, forbidden)
				decoder := json.NewDecoder(bytes.NewReader(trace))
				var events []perf.Event
				for {
					var event perf.Event
					err := decoder.Decode(&event)
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					events = append(events, event)
				}
				if len(events) == 0 || doc.TraceFile == "" {
					t.Error("missing trace events or reference")
				}
			} else if doc.TraceFile != "" {
				t.Errorf("unexpected trace reference %q", doc.TraceFile)
			}
			if c.TraceFailure && strings.Contains(stderr, "profile trace written to") {
				t.Error("claimed a trace that was not written")
			}
		})
	}
}

func assertProfileText(t *testing.T, text string, required, forbidden []string) {
	t.Helper()
	for _, s := range required {
		if !strings.Contains(text, s) {
			t.Errorf("missing %q in %q", s, text)
		}
	}
	for _, s := range forbidden {
		if strings.Contains(text, s) {
			t.Errorf("unexpected %q in output", s)
		}
	}
}

func readPrivateProfileFile(t *testing.T, path string) []byte {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("profile mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPushCmd_ProfileInvalidPaths(t *testing.T) {
	for _, c := range loadPushProfileFixtures(t).InvalidCases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "profile.json")
			args := []string{"--profile-output", path}
			switch c.Shape {
			case "missing-parent":
				args[1] = filepath.Join(dir, "missing", "profile.json")
			case "trace-without-output":
				args = []string{"--profile-trace", path}
			case "path-is-directory":
				args[1] = dir
			case "same-path":
				args = append(args, "--profile-trace", path)
			case "empty-value":
				args[1] = ""
			case "empty-trace":
				args = []string{"--profile-trace", ""}
			case "relative-alias":
				rel, err := filepath.Rel(mustWorkingDirectory(t), path)
				if err != nil {
					t.Fatal(err)
				}
				args = append(args, "--profile-trace", rel)
			case "parent-alias":
				alias := filepath.Join(dir, "alias")
				if err := os.Symlink(dir, alias); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--profile-trace", filepath.Join(alias, "profile.json"))
			case "unwritable-parent":
				parent := filepath.Join(dir, "read-only")
				if err := os.Mkdir(parent, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
				args[1] = filepath.Join(parent, "profile.json")
			case "no-credentials":
				args = append(args, "--profile-trace", filepath.Join(dir, "trace.jsonl"))
			case "symlink", "hardlink":
				if err := os.WriteFile(path, []byte("keep existing content"), 0o600); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(dir, "alias.jsonl")
				link := os.Symlink
				if c.Shape == "hardlink" {
					link = os.Link
				}
				if err := link(path, alias); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--profile-trace", alias)
			default:
				t.Fatalf("unknown invalid shape %s", c.Shape)
			}
			out, stderr, err := executePushCmdSeparate(t, dir, args)
			assertNoProfileTemps(t, dir)
			if err == nil {
				t.Fatal("invalid profile path accepted")
			}
			assertProfileText(t, err.Error(), c.ExpectErrorContains, nil)
			if out != "" || strings.Contains(stderr, "profile written") {
				t.Errorf("push ran before validation: %s %s", out, stderr)
			}
			if _, err := os.Stat(string(defaults.ResolveDBFilePathWith(dir))); !os.IsNotExist(err) {
				t.Errorf("store opened before validation: %v", err)
			}
			if c.Shape == "symlink" || c.Shape == "hardlink" {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "keep existing content" {
					t.Fatalf("existing content changed: %q %v", data, err)
				}
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("profile file created before preflight completed: %v", err)
			}
		})
	}
}

func assertNoProfileTemps(t *testing.T, dir string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, ".profile-*.tmp"))
	if err != nil || len(paths) != 0 {
		t.Errorf("profile temporary files remain: %v (%v)", paths, err)
	}
}

func mustWorkingDirectory(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
