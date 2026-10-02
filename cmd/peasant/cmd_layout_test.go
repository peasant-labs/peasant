package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
)

const (
	layoutTestTool harnesslayout.Tool = "cmd-layout-test-tool"
	layoutFailTool harnesslayout.Tool = "cmd-layout-fail-tool"
)

var layoutTestArtifact = harnesslayout.Artifact{
	Name:    "transcript",
	Pattern: "*.jsonl",
	Format:  harnesslayout.FormatJSONL,
	Role:    harnesslayout.RoleTranscript,
}

type layoutTestProbe struct{}

var _ harnesslayout.Probe = layoutTestProbe{}

func (layoutTestProbe) Discover(_ context.Context, src harnesslayout.Source) ([]harnesslayout.SessionRef, error) {
	matches, err := fs.Glob(src.FS, layoutTestArtifact.Pattern)
	refs := make([]harnesslayout.SessionRef, 0, len(matches))
	for _, match := range matches {
		refs = append(refs, harnesslayout.SessionRef{ID: strings.TrimSuffix(match, ".jsonl"), Paths: []string{match}})
	}
	return refs, err
}

func (layoutTestProbe) Capture(_ context.Context, src harnesslayout.Source, ref harnesslayout.SessionRef) (harnesslayout.Capture, error) {
	file, err := src.FS.Open(ref.Paths[0])
	if err != nil {
		return harnesslayout.Capture{}, err
	}
	defer file.Close()
	rec := harnesslayout.NewShapeRecorder(layoutTestArtifact, ref.Paths[0])
	meta := harnesslayout.Metadata{SessionID: ref.ID}
	err = harnesslayout.ShapeJSONL(file, rec, harnesslayout.KindField("type"), func(record any) {
		meta.Title = harnesslayout.StringField(record, "title")
	})
	return harnesslayout.Capture{Metadata: meta, Shapes: []harnesslayout.ArtifactShape{rec.Shape()}}, err
}

type layoutFailProbe struct{ layoutTestProbe }

func (layoutFailProbe) Capture(context.Context, harnesslayout.Source, harnesslayout.SessionRef) (harnesslayout.Capture, error) {
	return harnesslayout.Capture{}, &fs.PathError{Op: "open", Path: "secret-session", Err: fs.ErrPermission}
}

func init() {
	harnesslayout.Register(harnesslayout.Layout{
		Tool:        layoutTestTool,
		DisplayName: "Layout Test Tool",
		Sessions:    "One JSONL file per session.",
		Roots:       []harnesslayout.Root{{Path: "{home}/.layout-test-tool"}},
		Artifacts:   []harnesslayout.Artifact{layoutTestArtifact},
		Sources:     []string{"synthetic test layout"},
		Probe:       layoutTestProbe{},
	})
	harnesslayout.Register(harnesslayout.Layout{
		Tool:        layoutFailTool,
		DisplayName: "Layout Fail Tool",
		Sessions:    "One JSONL file per session.",
		Roots:       []harnesslayout.Root{{Path: "{home}/.layout-fail-tool"}},
		Artifacts:   []harnesslayout.Artifact{layoutTestArtifact},
		Sources:     []string{"synthetic test layout"},
		Probe:       layoutFailProbe{},
	})
}

func runLayoutCommand(t *testing.T, home string, args ...string) (string, string, error) {
	t.Helper()
	env := func() (harnesslayout.Env, error) {
		return harnesslayout.Env{GOOS: harnesslayout.OSLinux, Home: home, ConfigDir: filepath.Join(home, ".config")}, nil
	}
	cmd := buildLayoutCommand(env, harnesslayout.OSSource)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestLayoutCaptureReadsDefaultRootAndHidesValues(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, ".layout-test-tool")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"type":"user","title":"Private title","text":"private words"}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "s1.jsonl"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runLayoutCommand(t, home, "capture", string(layoutTestTool))
	if err != nil {
		t.Fatal(err)
	}
	var report harnesslayout.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout)
	}
	if len(report.Captures) != 1 || report.Captures[0].Shapes[0].Kinds["user"] != 1 || !report.Roots[0].Present {
		t.Fatalf("report = %+v", report)
	}
	for _, private := range []string{"Private title", "private words", home, "s1"} {
		if strings.Contains(stdout, private) {
			t.Fatalf("shape-only output contains %q:\n%s", private, stdout)
		}
	}

	stdout, _, err = runLayoutCommand(t, home, "capture", string(layoutTestTool), "--include-metadata")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Private title") || strings.Contains(stdout, "private words") {
		t.Fatalf("--include-metadata output must keep metadata and drop text:\n%s", stdout)
	}
}

func TestLayoutCaptureReportsEmptyExplicitPath(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent")
	stdout, stderr, err := runLayoutCommand(t, t.TempDir(), "capture", string(layoutTestTool), "--path", missing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"present": false`) || !strings.Contains(stderr, "no Layout Test Tool sessions found") {
		t.Fatalf("stdout=%s stderr=%s", stdout, stderr)
	}
}

func TestLayoutCaptureStderrDistinguishesEmptyFromFailed(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	empty := filepath.Join(home, ".layout-test-tool")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runLayoutCommand(t, home, "capture", string(layoutTestTool))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"present": true`) || !strings.Contains(stderr, "no Layout Test Tool sessions found") || strings.Contains(stderr, "every capture failed") {
		t.Fatalf("empty store stdout=%s stderr=%s", stdout, stderr)
	}

	failed := filepath.Join(home, ".layout-fail-tool")
	if err := os.MkdirAll(failed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failed, "s1.jsonl"), []byte("{\"type\":\"user\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runLayoutCommand(t, home, "capture", string(layoutFailTool))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "found 1 Layout Fail Tool session in:") || !strings.Contains(stderr, "every capture failed") || strings.Contains(stderr, "no Layout Fail Tool sessions found") {
		t.Fatalf("all-failed stderr=%s\nstdout=%s", stderr, stdout)
	}
	if strings.Contains(stdout, "s1") || !strings.Contains(stdout, `"errorClass": "permission"`) {
		t.Fatalf("shape-only failure report = %s", stdout)
	}
}

func TestLayoutCaptureRejectsNegativeLimit(t *testing.T) {
	t.Parallel()
	_, stderr, err := runLayoutCommand(t, t.TempDir(), "capture", string(layoutTestTool), "--limit", "-1")
	if err == nil || !strings.Contains(err.Error(), "zero or greater") {
		t.Fatalf("error = %v, stderr = %s", err, stderr)
	}
	stdout, _, err := runLayoutCommand(t, t.TempDir(), "capture", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Maximum successful captures") {
		t.Fatalf("help = %s", stdout)
	}
}

func TestLayoutListMarksPermissionErrors(t *testing.T) {
	t.Parallel()
	env := func() (harnesslayout.Env, error) {
		return harnesslayout.Env{GOOS: harnesslayout.OSLinux, Home: "/home", ConfigDir: "/home/.config"}, nil
	}
	open := func(string) harnesslayout.Source {
		return harnesslayout.Source{FS: statErrFS{err: fs.ErrPermission}}
	}
	cmd := buildLayoutCommand(env, open)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"list"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "[absent]") || !strings.Contains(stdout.String(), "[permission]") {
		t.Fatalf("list = %s", stdout.String())
	}
}

type statErrFS struct{ err error }

func (s statErrFS) Open(string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: ".", Err: s.err}
}

func (s statErrFS) Stat(string) (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "stat", Path: ".", Err: s.err}
}

func TestLayoutListShowsResolvedRoots(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	stdout, _, err := runLayoutCommand(t, home, "list")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".layout-test-tool") + " [absent]"
	if !strings.Contains(stdout, "Layout Test Tool ("+string(layoutTestTool)+")") || !strings.Contains(stdout, want) {
		t.Fatalf("list output missing %q:\n%s", want, stdout)
	}
}
