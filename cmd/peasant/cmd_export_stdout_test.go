package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/export_stdout.yaml
var exportStdoutYAML []byte

type exportStdoutCase struct {
	Name       string   `yaml:"name"`
	Session    string   `yaml:"session"`
	Args       []string `yaml:"args"`
	Outcome    string   `yaml:"outcome"`
	WantStderr string   `yaml:"wantStderr"`
	WantError  string   `yaml:"wantError"`
}

// TestExportSessionsStdout drives the real command with --stdout over one
// stored session. A successful run writes exactly the transcript a file export
// holds and nothing else; a refused or misused run writes nothing to stdout and
// reports on stderr or as a usage error; no run creates a file.
func TestExportSessionsStdout(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string           `yaml:"requiredNames"`
		Cases         []exportStdoutCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(exportStdoutYAML, &fixture); err != nil {
		t.Fatalf("export_stdout.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	for _, required := range fixture.RequiredNames {
		if !slices.Contains(names, required) {
			t.Fatalf("export_stdout.yaml lost required case %q", required)
		}
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			id := "bbbbbbbb-bbbb-4bbb-8bbb-000000000880"
			if c.Session != "missing" {
				seedUploadableSession(t, dir, id)
				seedEntryCarrying(t, dir, id, "the stored body")
				seedRefusedCapture(t, dir, id, c.Session)
			}
			list := filepath.Join(dir, "ids.txt")
			if err := os.WriteFile(list, []byte(id+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			outDir := filepath.Join(dir, "never-created")
			expand := strings.NewReplacer("{session}", id, "{list}", list, "{outdir}", outDir)
			args := []string{"sessions"}
			for _, arg := range c.Args {
				args = append(args, expand.Replace(arg))
			}
			before := jsonFilesUnder(t, dir)

			stdout, stderr, err := executeWithDataDirStreams(t, BuildExportCommand(), dir, args)

			if _, statErr := os.Stat(outDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("--stdout created %s (stat err %v)", outDir, statErr)
			}
			if after := jsonFilesUnder(t, dir); !slices.Equal(before, after) {
				t.Fatalf("--stdout wrote files: before %v, after %v", before, after)
			}
			switch c.Outcome {
			case "exported":
				if err != nil {
					t.Fatalf("export failed: %v\nstderr:\n%s", err, stderr)
				}
				if stderr != "" {
					t.Fatalf("a successful --stdout export wrote stderr:\n%s", stderr)
				}
				assertStdoutIsTheFileTranscript(t, dir, id, c.Args, stdout)
			case "refused":
				if err == nil || !strings.Contains(err.Error(), "all 1 session(s) failed to export") {
					t.Fatalf("export error = %v, want the refusal", err)
				}
				var usage *clicontract.UsageError
				if errors.As(err, &usage) {
					t.Fatalf("a refused session was reported as a usage error: %v", err)
				}
				if stdout != "" {
					t.Fatalf("a refused --stdout export wrote stdout:\n%s", stdout)
				}
				if want := expand.Replace(c.WantStderr); !strings.Contains(stderr, want) {
					t.Fatalf("stderr lacks %q:\n%s", want, stderr)
				}
			case "usage":
				var usage *clicontract.UsageError
				if !errors.As(err, &usage) || !strings.Contains(err.Error(), c.WantError) {
					t.Fatalf("export error = %v, want a usage error carrying %q", err, c.WantError)
				}
				// Cobra prints its usage block to an out writer set on the root,
				// as this test does; production sets none, so the block reaches
				// stderr. Either way no transcript is begun.
				if strings.Contains(stdout, `"id"`) {
					t.Fatalf("a refused --stdout invocation wrote a transcript:\n%s", stdout)
				}
			default:
				t.Fatalf("export_stdout.yaml names unknown outcome %q", c.Outcome)
			}
		})
	}
}

// assertStdoutIsTheFileTranscript proves the stdout document is the file
// export's document, byte for byte, followed by one newline, and that it
// carries the session's identity and content.
func assertStdoutIsTheFileTranscript(t *testing.T, dir, id string, stdoutArgs []string, stdout string) {
	t.Helper()
	outDir := filepath.Join(dir, "file-export")
	fileArgs := []string{"sessions", "--output-dir", outDir}
	for _, arg := range stdoutArgs {
		switch arg {
		case "--stdout":
		case "{session}":
			fileArgs = append(fileArgs, id)
		default:
			fileArgs = append(fileArgs, arg)
		}
	}
	if _, _, err := executeWithDataDirStreams(t, BuildExportCommand(), dir, fileArgs); err != nil {
		t.Fatalf("file export: %v", err)
	}
	file, err := os.ReadFile(filepath.Join(outDir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(file)+"\n" {
		t.Fatalf("stdout is not the file transcript and one newline\nstdout:\n%s\nfile:\n%s", stdout, file)
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	var payload schema.SessionDetailPayload
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("stdout is not a transcript document: %v", err)
	}
	if decoder.More() {
		t.Fatal("stdout holds more than one JSON document")
	}
	if payload.ID != id {
		t.Fatalf("transcript id = %q, want %q", payload.ID, id)
	}
	if !strings.Contains(stdout, "the stored body") {
		t.Fatalf("transcript lost the stored content:\n%s", stdout)
	}
}

func jsonFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".json") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

type failingStdout struct{ wrote bytes.Buffer }

func (w *failingStdout) Write(p []byte) (int, error) {
	half := len(p) / 2
	w.wrote.Write(p[:half])
	return half, errors.New("broken pipe")
}

// TestExportSessionsStdoutWriteFailure pins the one late failure: stdout
// itself refusing the document. The run reports write_failed on stderr and
// exits 1, so a caller that reads stdout only on exit 0 never takes the
// partial bytes for a transcript.
func TestExportSessionsStdoutWriteFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := "bbbbbbbb-bbbb-4bbb-8bbb-000000000881"
	seedUploadableSession(t, dir, id)
	seedEntryCarrying(t, dir, id, "the stored body")

	root := newTestRoot()
	root.AddCommand(BuildExportCommand())
	stdout := &failingStdout{}
	var stderr bytes.Buffer
	root.SetOut(stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "export", "sessions", "--session", id, "--stdout"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "all 1 session(s) failed to export") {
		t.Fatalf("export error = %v, want the write failure to fail the run", err)
	}
	if !strings.Contains(stderr.String(), "write stdout: broken pipe") {
		t.Fatalf("stderr does not name the stdout failure:\n%s", stderr.String())
	}
	if json.Valid(stdout.wrote.Bytes()) {
		t.Fatal("the failing writer received a whole document; the test no longer exercises a partial write")
	}
}
