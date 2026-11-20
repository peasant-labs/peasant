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
	"strconv"

	"github.com/peasant-labs/peasant/internal/upgrade"
)

// The new-session smoke shape and sample count.
const (
	smokeSessions = 2
	smokeEntries  = 10
	smokeSamples  = 5
)

// sampleKind names what one child process measures. Every timed pipeline
// run happens in a child, so no sample reuses a heap an earlier run left
// behind: a later run in the same process would first zero the freed staging
// arena, which inflates and scatters its time.
type sampleKind string

const (
	// sampleKindImport times the new-session import into an empty store.
	sampleKindImport sampleKind = "new-session-import"
	// sampleKindRun times one arm of a corpus case on a copy of its
	// prepared base.
	sampleKindRun sampleKind = "case-arm"
)

var allSampleKinds = []sampleKind{sampleKindImport, sampleKindRun}

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

// harnessSampleCommand re-runs the harness binary in its import sample mode.
func harnessSampleCommand(self string) sampleCommand {
	return func(dir string) *exec.Cmd {
		return exec.Command(self, sampleFlags{Kind: string(sampleKindImport), Dir: dir}.args()...)
	}
}

// runCommand builds the child process that runs one arm sample.
type runCommand func(f sampleFlags) *exec.Cmd

// harnessRunCommand re-runs the harness binary in its run sample mode.
func harnessRunCommand(self string) runCommand {
	return func(f sampleFlags) *exec.Cmd { return exec.Command(self, f.args()...) }
}

// runArmSampleChild validates a run sample's flags, runs it, and prints the
// sample stamped with this process id.
func runArmSampleChild(ctx context.Context, f sampleFlags, out io.Writer) error {
	id, err := upgrade.NewMeasurementCase(f.Case)
	if err != nil {
		return err
	}
	spec, ok := caseSpecs()[id]
	if !ok {
		return fmt.Errorf("-sample-case %s has no arms to run; a run sample takes a corpus case", id)
	}
	a, err := newArm(f.Arm)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(f.Base) {
		return fmt.Errorf("-sample-base %q must be the absolute prepared base directory", f.Base)
	}
	if f.Round < 1 {
		return fmt.Errorf("-sample-round %d must be positive", f.Round)
	}
	s, err := runRound(ctx, spec, f.Base, f.Dir, f.Round, a)
	if err != nil {
		return fmt.Errorf("%s round %d %s arm in %s: %w", id, f.Round, a, f.Dir, err)
	}
	s.PID = os.Getpid()
	return json.NewEncoder(out).Encode(s)
}

// collectRunSample runs the arm sample f names in its own child process and
// checks the child ran that sample in another process. The case's count
// check stays with the caller.
func collectRunSample(f sampleFlags, command runCommand) (sample, error) {
	cmd := command(f)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return sample{}, fmt.Errorf("run sample in %s: %w: %s", f.Dir, err, stderr.Bytes())
	}
	var s sample
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return sample{}, fmt.Errorf("run sample in %s printed %q, want one JSON sample: %w", f.Dir, out, err)
	}
	fromChild := s.PID > 0 && s.PID != os.Getpid()
	if !fromChild || s.Round != f.Round || string(s.Arm) != f.Arm {
		return sample{}, fmt.Errorf("run sample in %s reported pid %d round %d arm %q, want round %d arm %q from a child process", f.Dir, s.PID, s.Round, s.Arm, f.Round, f.Arm)
	}
	return s, nil
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

// sampleFlags is one child invocation as its flags name it, unvalidated.
// Base, Case, Arm and Round are read only by a run sample.
type sampleFlags struct {
	Kind, Dir, Base, Case, Arm string
	Round                      int
}

// args renders f as the harness flags that select it.
func (f sampleFlags) args() []string {
	args := []string{"-sample", f.Kind, "-sample-dir", f.Dir}
	if sampleKind(f.Kind) == sampleKindRun {
		args = append(args, "-sample-base", f.Base, "-sample-case", f.Case, "-sample-arm", f.Arm, "-sample-round", strconv.Itoa(f.Round))
	}
	return args
}

// runSampleChild is the child side: it runs the one sample f names and
// writes its result to out as JSON. An import sample imports a sessions x
// entries corpus into f.Dir; a run sample copies f.Base into f.Dir outside
// the timed region and runs f.Arm of f.Case once.
func runSampleChild(ctx context.Context, f sampleFlags, sessions, entries int, out io.Writer) error {
	kind, err := newSampleKind(f.Kind)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(f.Dir) {
		return fmt.Errorf("-sample-dir %q must be an absolute directory", f.Dir)
	}
	if kind == sampleKindRun {
		return runArmSampleChild(ctx, f, out)
	}
	dir := f.Dir
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
