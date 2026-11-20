//go:build skipbudget

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/peasant-labs/peasant/internal/upgrade"
)

// The new-session smoke shape and sample count.
const (
	smokeSessions = 2
	smokeEntries  = 10
	smokeSamples  = 5
)

// sampleKind names what one child process measures. The closed set grows as
// cases move into their own processes.
type sampleKind string

const sampleKindImport sampleKind = "import"

var allSampleKinds = []sampleKind{sampleKindImport}

func newSampleKind(raw string) (sampleKind, error) {
	kind := sampleKind(raw)
	if !slices.Contains(allSampleKinds, kind) {
		return "", fmt.Errorf("-sample %q is not one of %v", raw, allSampleKinds)
	}
	return kind, nil
}

// sampleResult is what one child process prints: the shape it imported and
// the time the pipeline run took. ImportMs times the pipeline run only, so it
// excludes the process start and the corpus write.
type sampleResult struct {
	PID               int    `json:"pid"`
	Dir               string `json:"dir"`
	Sessions          int    `json:"sessions"`
	Entries           int    `json:"entriesPerSession"`
	ImportedEntries   int    `json:"importedEntries"`
	ActiveGenerations int64  `json:"activeGenerations"`
	ImportMs          int64  `json:"importMs"`
}

// sampleCommand builds the child process that runs one sample into dir.
type sampleCommand func(dir string) *exec.Cmd

// harnessSampleCommand re-runs the harness binary in its sample mode.
func harnessSampleCommand(self string) sampleCommand {
	return func(dir string) *exec.Cmd {
		return exec.Command(self, "-sample", string(sampleKindImport), "-sample-dir", dir)
	}
}

// runSmokeCase cleans the new-session case directory and runs its samples.
func runSmokeCase(opts harnessOptions) error {
	dir, err := cleanCaseDir(opts.root, upgrade.MeasurementCaseNewSession)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the harness binary to run each sample: %w", err)
	}
	_, err = runSmoke(dir, opts, smokeSamples, harnessSampleCommand(self))
	return err
}

// runSampleChild is the child side: it runs one sample of kind into dir and
// writes its result to out as JSON.
func runSampleChild(ctx context.Context, rawKind, dir string, sessions, entries int, out io.Writer) error {
	if _, err := newSampleKind(rawKind); err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("-sample-dir %q must be an absolute directory", dir)
	}
	shape, err := prepareBase(ctx, dir, sessions, entries, mutateNone)
	if err != nil {
		return fmt.Errorf("import sample in %s: %w", dir, err)
	}
	return json.NewEncoder(out).Encode(sampleResult{
		PID: os.Getpid(), Dir: dir, Sessions: shape.Sessions, Entries: shape.Entries,
		ImportedEntries: shape.ImportedEntries, ActiveGenerations: shape.ActiveGenerations, ImportMs: shape.PrepareMs,
	})
}

// collectImportSamples runs count import samples, one child process and one
// directory each, and checks every child imported the smoke shape.
func collectImportSamples(dir string, count int, command sampleCommand) ([]sampleResult, error) {
	results := make([]sampleResult, 0, count)
	for i := range count {
		sampleDir := filepath.Join(dir, fmt.Sprintf("sample-%d", i+1))
		cmd := command(sampleDir)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("import sample %d in %s: %w: %s", i+1, sampleDir, err, stderr.Bytes())
		}
		var r sampleResult
		decoder := json.NewDecoder(bytes.NewReader(out))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&r); err != nil {
			return nil, fmt.Errorf("sample %d printed %q, want one JSON sample result: %w", i+1, out, err)
		}
		// An assistant record can index as more than one entry, so every
		// source record indexed sets a floor, not an exact count.
		want := sampleResult{PID: r.PID, Dir: sampleDir, Sessions: smokeSessions, Entries: smokeEntries,
			ImportedEntries: r.ImportedEntries, ActiveGenerations: smokeSessions, ImportMs: r.ImportMs}
		if r != want || r.PID <= 0 || r.ImportedEntries < smokeSessions*smokeEntries {
			return nil, fmt.Errorf("sample %d reported %+v, want the %d x %d smoke shape in %s, every record indexed and one active generation per session", i+1, r, smokeSessions, smokeEntries, sampleDir)
		}
		results = append(results, r)
	}
	return results, nil
}
