//go:build skipbudget

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
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

// sampleFlags is one child invocation as its flags name it, unvalidated.
// Base, Case, Arm and Round are read only by a case-arm sample.
type sampleFlags struct {
	Kind, Dir, Base, Case, Arm string
	Round                      int
}

// bindSampleFlags registers the child flags on fs and returns the value they
// parse into. The harness binds them on its command line; args renders the
// same flags, so the parent and the child agree on one flag set.
func bindSampleFlags(fs *flag.FlagSet) *sampleFlags {
	f := &sampleFlags{}
	fs.StringVar(&f.Kind, "sample", "", "internal: run one sample of this kind in this process ("+string(sampleKindImport)+", "+string(sampleKindRun)+") and print it as JSON")
	fs.StringVar(&f.Dir, "sample-dir", "", "internal: the directory the -sample run writes into")
	fs.StringVar(&f.Base, "sample-base", "", "internal: the prepared base a case-arm sample copies")
	fs.StringVar(&f.Case, "sample-case", "", "internal: the case a case-arm sample measures")
	fs.StringVar(&f.Arm, "sample-arm", "", "internal: the arm a case-arm sample runs: skip or baseline")
	fs.IntVar(&f.Round, "sample-round", 0, "internal: the round a case-arm sample belongs to")
	return f
}

// args renders f as the harness flags that select it.
func (f sampleFlags) args() []string {
	args := []string{"-sample", f.Kind, "-sample-dir", f.Dir}
	if sampleKind(f.Kind) == sampleKindRun {
		args = append(args, "-sample-base", f.Base, "-sample-case", f.Case, "-sample-arm", f.Arm, "-sample-round", strconv.Itoa(f.Round))
	}
	return args
}

// sampleRequest is a validated child invocation. spec, arm and round are set
// only for a case-arm sample.
type sampleRequest struct {
	kind  sampleKind
	dir   string
	base  string
	spec  caseSpec
	arm   arm
	round int
}

// validate checks f at the child's input boundary.
func (f sampleFlags) validate() (sampleRequest, error) {
	kind, err := newSampleKind(f.Kind)
	if err != nil {
		return sampleRequest{}, err
	}
	if !filepath.IsAbs(f.Dir) {
		return sampleRequest{}, fmt.Errorf("-sample-dir %q must be an absolute directory", f.Dir)
	}
	req := sampleRequest{kind: kind, dir: f.Dir}
	if kind != sampleKindRun {
		return req, nil
	}
	id, err := upgrade.NewMeasurementCase(f.Case)
	if err != nil {
		return sampleRequest{}, err
	}
	spec, ok := caseSpecs()[id]
	if !ok {
		return sampleRequest{}, fmt.Errorf("-sample-case %s has no arms to run; a case-arm sample takes a corpus case", id)
	}
	if req.arm, err = newArm(f.Arm); err != nil {
		return sampleRequest{}, err
	}
	if !filepath.IsAbs(f.Base) {
		return sampleRequest{}, fmt.Errorf("-sample-base %q must be the absolute prepared base directory", f.Base)
	}
	if f.Round < 1 {
		return sampleRequest{}, fmt.Errorf("-sample-round %d must be positive", f.Round)
	}
	req.spec, req.base, req.round = spec, f.Base, f.Round
	return req, nil
}

// sampleCommand builds the child process that runs the sample f names.
type sampleCommand func(f sampleFlags) *exec.Cmd

// harnessSampleCommand re-runs the harness binary in its sample mode.
func harnessSampleCommand(self string) sampleCommand {
	return func(f sampleFlags) *exec.Cmd { return exec.Command(self, f.args()...) }
}

// collectChild runs the child for f and decodes the one JSON value it must
// print into target. Unknown fields, a second value, or any other trailing
// output fail it.
func collectChild(f sampleFlags, command sampleCommand, target any) error {
	cmd := command(f)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s sample in %s: %w: %s", f.Kind, f.Dir, err, stderr.Bytes())
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%s sample in %s printed %q, want one JSON sample: %w", f.Kind, f.Dir, out, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s sample in %s printed %q, want one JSON sample and nothing after it", f.Kind, f.Dir, out)
	}
	return nil
}

// runSampleChild is the child side: it runs the one sample f names and
// writes its result to out as JSON. An import sample imports a sessions x
// entries corpus into f.Dir; a case-arm sample copies f.Base into f.Dir
// outside the timed region and runs f.Arm of f.Case once.
func runSampleChild(ctx context.Context, f sampleFlags, sessions, entries int, out io.Writer) error {
	req, err := f.validate()
	if err != nil {
		return err
	}
	if req.kind == sampleKindRun {
		s, err := runRound(ctx, req.spec, req.base, req.dir, req.round, req.arm)
		if err != nil {
			return fmt.Errorf("%s round %d %s arm in %s: %w", req.spec.id, req.round, req.arm, req.dir, err)
		}
		s.PID = os.Getpid()
		return json.NewEncoder(out).Encode(s)
	}
	shape, err := prepareBase(ctx, req.dir, sessions, entries, mutateNone)
	if err != nil {
		return fmt.Errorf("import sample in %s: %w", req.dir, err)
	}
	return json.NewEncoder(out).Encode(sampleResult{
		PID: os.Getpid(), Dir: req.dir, Sessions: shape.Sessions, Entries: shape.Entries,
		ImportedEntries: shape.ImportedEntries, ActiveGenerations: shape.ActiveGenerations, ImportMs: shape.PrepareMs,
	})
}

// collectRunSample runs the case-arm sample f names in its own child process
// and checks the child ran that sample, in another process. The case's count
// check stays with the caller.
func collectRunSample(f sampleFlags, command sampleCommand) (sample, error) {
	var s sample
	if err := collectChild(f, command, &s); err != nil {
		return sample{}, err
	}
	if s.PID <= 0 || s.PID == os.Getpid() {
		return sample{}, fmt.Errorf("case-arm sample in %s reported pid %d, want a child process other than the runner's pid %d", f.Dir, s.PID, os.Getpid())
	}
	if s.Round != f.Round {
		return sample{}, fmt.Errorf("case-arm sample in %s reported round %d, want round %d", f.Dir, s.Round, f.Round)
	}
	if string(s.Arm) != f.Arm {
		return sample{}, fmt.Errorf("case-arm sample in %s reported arm %q, want arm %q", f.Dir, s.Arm, f.Arm)
	}
	return s, nil
}

// runSmokeCase cleans the new-session case directory and runs its samples.
func runSmokeCase(opts harnessOptions, command sampleCommand) error {
	dir, err := cleanCaseDir(opts.root, upgrade.MeasurementCaseNewSession)
	if err != nil {
		return err
	}
	_, err = runSmoke(dir, opts, smokeSamples, command)
	return err
}

// collectImportSamples runs count import samples, one child process and one
// directory each, and checks every child imported the smoke shape.
func collectImportSamples(dir string, count int, command sampleCommand) ([]sampleResult, error) {
	results := make([]sampleResult, 0, count)
	for i := range count {
		sampleDir := filepath.Join(dir, fmt.Sprintf("sample-%d", i+1))
		var r sampleResult
		if err := collectChild(sampleFlags{Kind: string(sampleKindImport), Dir: sampleDir}, command, &r); err != nil {
			return nil, fmt.Errorf("sample %d: %w", i+1, err)
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
