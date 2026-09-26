package testgate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/teststream"
)

// PassMode selects the two-pass split.
type PassMode int

const (
	// ModeRace is the race-instrumented pass; it runs every test that is not a
	// partition member.
	ModeRace PassMode = iota
	// ModeNoRace is the single no-race pass. Under RACE=1 it runs exactly the
	// partition members; under RACE=0 it is the only pass and runs every test.
	ModeNoRace
)

func (m PassMode) String() string {
	if m == ModeRace {
		return "race"
	}
	return "no-race"
}

// Record is one recordable unit: a go test invocation, with its timing
// boundary. This is the shape the CPU-time accumulator wraps.
type Record struct {
	Unit   string
	Class  Class
	Pass   PassMode
	Wall   time.Duration
	User   time.Duration
	System time.Duration
}

// Invocation is one `go test` invocation for one package (or one partition test).
type Invocation struct {
	ImportPath string
	Dir        string
	Test       string // set for a per-test no-race invocation
	Pass       PassMode
	Class      Class
	Args       []string
	OutputDir  string
	StreamPath string
	ErrPath    string
}

// Unit is the recordable unit name: the package, or package/test for a
// per-test invocation.
func (i Invocation) Unit() string {
	if i.Test != "" {
		return i.ImportPath + "/" + i.Test
	}
	return i.ImportPath
}

// Runner executes the plan's invocations.
type Runner struct {
	Root        string
	OutDir      string
	GoBin       string
	Concurrency int
	Env         []string
	// SerialPassB forces the no-race pass to run one invocation at a time so
	// getrusage(RUSAGE_CHILDREN) attributes per unit without cross-talk.
	SerialPassB bool
}

// RunResult is the merged outcome of both passes.
type RunResult struct {
	Records map[PassMode][]Record
	Streams map[PassMode]map[string][]teststream.Record // pass -> package -> records
	Walls   map[PassMode]time.Duration
	Errors  []string
}

// FailedTests returns every failed test across both passes.
func (r *RunResult) FailedTests() []teststream.Record {
	var out []teststream.Record
	for _, mode := range []PassMode{ModeRace, ModeNoRace} {
		for _, pkg := range sortedKeys(r.Streams[mode]) {
			for _, rec := range r.Streams[mode][pkg] {
				if rec.Failed {
					out = append(out, rec)
				}
			}
		}
	}
	return out
}

// BuildInvocations builds the invocation list for one pass.
//
// race=true selects the race pass. The race pass and the RACE=0 single pass
// invoke one test binary per package; the RACE=1 no-race pass invokes one
// test binary per partition test, so each record carries that test's class.
func BuildInvocations(root, outDir string, plan *Plan, mode PassMode, race bool) []Invocation {
	var out []Invocation
	for _, p := range plan.Packages {
		if mode == ModeNoRace && race {
			// Partition pass: one invocation per registered test.
			for _, name := range p.NoRaceTests {
				unitDir := filepath.Join(outDir, mode.String(), sanitize(p.Dir), name)
				args := []string{"test", "-json", "-fullpath", "-count=1", "-timeout=0", "-outputdir", unitDir, "-run", "^" + regexp.QuoteMeta(name) + "$", "./" + p.Dir}
				out = append(out, Invocation{
					ImportPath: p.ImportPath,
					Dir:        p.Dir,
					Test:       name,
					Pass:       mode,
					Class:      p.NoRaceClasses[name],
					Args:       args,
					OutputDir:  unitDir,
					StreamPath: filepath.Join(outDir, mode.String(), sanitize(p.Dir)+"_"+name+".json"),
					ErrPath:    filepath.Join(outDir, mode.String(), sanitize(p.Dir)+"_"+name+".err"),
				})
			}
			continue
		}

		var names []string
		if mode == ModeRace {
			names = p.RaceTests
		} else {
			// RACE=0: the single no-race pass runs every test in one pass.
			names = p.Tests
		}
		if len(names) == 0 {
			continue
		}
		unitDir := filepath.Join(outDir, mode.String(), sanitize(p.Dir))
		args := []string{"test", "-json", "-fullpath", "-count=1", "-timeout=0", "-outputdir", unitDir}
		if mode == ModeRace && race {
			args = append(args, "-race")
		}
		if len(names) < len(p.Tests) {
			args = append(args, "-run", RunRegex(names))
		}
		args = append(args, "./"+p.Dir)
		out = append(out, Invocation{
			ImportPath: p.ImportPath,
			Dir:        p.Dir,
			Pass:       mode,
			Class:      ClassRace,
			Args:       args,
			OutputDir:  unitDir,
			StreamPath: filepath.Join(outDir, mode.String(), sanitize(p.Dir)+".json"),
			ErrPath:    filepath.Join(outDir, mode.String(), sanitize(p.Dir)+".err"),
		})
	}
	return out
}

// Run executes the plan's invocations for the given pass and returns its
// records, streams, wall, and any invocation errors.
//
// race controls the detector; mode selects which test set each package runs.
// RACE=0 is mode ModeNoRace with race=false over every test.
func (r *Runner) Run(ctx context.Context, plan *Plan, mode PassMode, race bool) (*RunResult, error) {
	invocations := BuildInvocations(r.Root, r.OutDir, plan, mode, race)
	result := &RunResult{
		Records: map[PassMode][]Record{},
		Streams: map[PassMode]map[string][]teststream.Record{},
		Walls:   map[PassMode]time.Duration{},
	}
	streams := map[string][]teststream.Record{}

	concurrency := r.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if mode == ModeNoRace && r.SerialPassB {
		concurrency = 1
	}

	start := time.Now()
	var mu sync.Mutex
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, inv := range invocations {
		wg.Add(1)
		go func(inv Invocation) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rec, records, err := r.runOne(ctx, inv)
			mu.Lock()
			defer mu.Unlock()
			result.Records[mode] = append(result.Records[mode], rec)
			streams[inv.ImportPath] = append(streams[inv.ImportPath], records...)
			if err != nil {
				result.Errors = append(result.Errors, err.Error())
			}
		}(inv)
	}
	wg.Wait()
	result.Walls[mode] = time.Since(start)
	result.Streams[mode] = streams
	return result, nil
}

func (r *Runner) runOne(ctx context.Context, inv Invocation) (Record, []teststream.Record, error) {
	if err := os.MkdirAll(inv.OutputDir, 0o755); err != nil {
		return Record{Unit: inv.Unit(), Class: inv.Class, Pass: inv.Pass}, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(inv.StreamPath), 0o755); err != nil {
		return Record{Unit: inv.Unit(), Class: inv.Class, Pass: inv.Pass}, nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.GoBin, inv.Args...)
	cmd.Dir = r.Root
	cmd.Env = r.Env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	beforeUser, beforeSys, _ := readChildUsage()
	start := time.Now()
	runErr := cmd.Run()
	wall := time.Since(start)
	afterUser, afterSys, _ := readChildUsage()

	rec := Record{
		Unit:   inv.Unit(),
		Class:  inv.Class,
		Pass:   inv.Pass,
		Wall:   wall,
		User:   afterUser - beforeUser,
		System: afterSys - beforeSys,
	}

	if err := os.WriteFile(inv.StreamPath, stdout.Bytes(), 0o644); err != nil {
		return rec, nil, fmt.Errorf("write stream %s: %w", inv.StreamPath, err)
	}
	if err := os.WriteFile(inv.ErrPath, stderr.Bytes(), 0o644); err != nil {
		return rec, nil, fmt.Errorf("write stderr %s: %w", inv.ErrPath, err)
	}

	records, err := teststream.ParseStream(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		return rec, nil, fmt.Errorf("parse stream for %s (%s): %w", inv.ImportPath, inv.Pass, err)
	}
	if runErr != nil && len(records) == 0 {
		// A build failure or crash emits no test events. The screen reports a
		// registered package with no events as a failure; surface the cause too.
		return rec, records, fmt.Errorf("go test %s (%s) failed with no test events: %v\n%s", inv.ImportPath, inv.Pass, runErr, tail(stderr.String(), 40))
	}
	return rec, records, nil
}

func sanitize(dir string) string {
	s := strings.ReplaceAll(dir, "/", "_")
	if s == "" || s == "." {
		return "root"
	}
	return s
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
