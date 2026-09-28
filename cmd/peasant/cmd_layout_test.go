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

const layoutTestTool harnesslayout.Tool = "cmd-layout-test-tool"

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
