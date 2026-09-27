package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/testgate"
	"github.com/peasant-labs/peasant/internal/testkit/teststream"
)

// profileOptions collects the `testgate profile` flags.
type profileOptions struct {
	root     string
	goBin    string
	outDir   string
	pkg      string
	batches  int
	parallel int
	prior    string
	cpuTop   int
	pretest  bool
	profiles bool
	trace    bool
	race     bool
}

// runProfile dispatches the two measurement modes of `profile`: the five
// pre-test steps (`-pretest`), and the batched LPT profile of one package.
func runProfile(opts profileOptions) int {
	if opts.pretest {
		return runPreTestProfile(opts)
	}
	if opts.pkg == "" {
		fmt.Fprintln(os.Stderr, "testgate: profile needs -pkg <dir> or -pretest")
		return 2
	}
	return runPackageProfile(opts)
}

// runPreTestProfile measures the five `make check` pre-test steps. It returns 1
// when any measured step failed, so it can substitute for the recipe lines it
// mirrors.
func runPreTestProfile(opts profileOptions) int {
	cmds, err := testgate.PreTestCommands()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: pre-test fixture: %v\n", err)
		return 2
	}
	logDir := filepath.Join(opts.outDir, "pretest")
	ctx := context.Background()
	steps, err := testgate.RunPreTestSteps(ctx, opts.root, logDir, cmds, os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: pre-test steps: %v\n", err)
		return 2
	}
	calL, calRaw := testgate.Calibrate()
	rows := testgate.PreTestRows(steps)

	doc := testgate.PreTestDocument{
		SchemaVersion: 1,
		Calibration: testgate.Calibration{
			L:            calL,
			ProbeMS:      calRaw.Milliseconds(),
			ReferenceMS:  testgate.CalibrationReferenceNS / int64(time.Millisecond),
			Inconclusive: calL > 4,
		},
		Steps: steps,
		Rows:  rows,
	}
	docPath := filepath.Join(opts.outDir, "pretest.json")
	if err := testgate.WritePreTestDocument(docPath, doc); err != nil {
		fmt.Fprintf(os.Stderr, "testgate: write %s: %v\n", docPath, err)
		return 2
	}

	fmt.Printf("\n=== pre-test steps (calibration L=%.3f) ===\n", calL)
	fmt.Printf("%-20s %9s %9s %9s %9s\n", "STEP", "WALL", "USER", "SYSTEM", "GAP")
	failed := false
	for _, s := range steps {
		gap := (s.Wall - s.User - s.System).Round(time.Millisecond)
		status := ""
		if s.Failed {
			failed = true
			status = fmt.Sprintf("  FAILED (exit %d)", s.ExitCode)
		}
		fmt.Printf("%-20s %9s %9s %9s %9s%s\n", s.Step, round(s.Wall), round(s.User), round(s.System), round(gap), status)
	}
	fmt.Printf("\ntestgate: pre-test doc %s\n", docPath)
	if failed {
		fmt.Println("testgate: FAIL (a pre-test step exited non-zero)")
		return 1
	}
	return 0
}

// runPackageProfile runs the batched LPT profile of one package.
func runPackageProfile(opts profileOptions) int {
	modulePath, err := testgate.ModulePath(opts.root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: %v\n", err)
		return 2
	}
	importPath := modulePath
	if dir := strings.TrimPrefix(opts.pkg, "./"); dir != "" && dir != "." {
		importPath = modulePath + "/" + dir
	}

	batchCount := opts.batches
	if batchCount < 1 {
		if opts.parallel == 0 {
			// An unpinned -parallel run is an isolated package measurement, so
			// it must not also be split into batches.
			batchCount = 1
		} else {
			batchCount = defaultBatchCount(opts.race)
		}
	}

	prior, err := loadPriorWeights(opts.prior)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: %v\n", err)
		return 2
	}

	cfg := testgate.BatchProfileConfig{
		Root:          opts.root,
		GoBin:         opts.goBin,
		Package:       opts.pkg,
		ImportPath:    importPath,
		OutDir:        opts.outDir,
		Race:          opts.race,
		Batches:       batchCount,
		Prior:         prior,
		CPUProfileTop: opts.cpuTop,
		Parallel:      opts.parallel,
		Profiles: testgate.ProfileFlags{
			Block: opts.profiles,
			Mutex: opts.profiles,
			CPU:   opts.profiles,
			Trace: opts.trace,
		},
		Env: os.Environ(),
	}
	if opts.trace && opts.profiles {
		fmt.Fprintln(os.Stderr, "testgate: note: -trace perturbs the block profile; read block.out from a -profiles run without -trace")
	}
	ctx := context.Background()
	res, err := testgate.RunBatchProfile(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: profile %s: %v\n", opts.pkg, err)
		return 2
	}

	docPath := filepath.Join(opts.outDir, "profile.json")
	if err := testgate.WriteProfileDocument(docPath, res); err != nil {
		fmt.Fprintf(os.Stderr, "testgate: write %s: %v\n", docPath, err)
		return 2
	}

	fmt.Printf("\n=== batched profile: %s ===\n", opts.pkg)
	fmt.Printf("batches=%d parallel=%d race=%v lpt=%v\n", res.RequestedBatches, res.Parallel, res.Race, res.LPT)
	fmt.Printf("%-6s %9s %9s %9s %6s\n", "BATCH", "WALL", "USER", "SYSTEM", "TESTS")
	for _, b := range res.Batches {
		fmt.Printf("%-6d %9s %9s %9s %6d\n", b.Index, ms(b.WallMS), ms(b.UserMS), ms(b.SystemMS), len(b.Tests))
	}
	for _, b := range res.Batches {
		if b.CPUProfile != "" || b.BlockProfile != "" || b.MutexProfile != "" || b.Trace != "" {
			fmt.Printf("  batch %d class B artifacts: cpu=%s block=%s mutex=%s trace=%s\n",
				b.Index, b.CPUProfile, b.BlockProfile, b.MutexProfile, b.Trace)
		}
	}
	fmt.Printf("\nslowest tests (top-level, queue-free):\n")
	for i, t := range res.Tests {
		if i >= 20 {
			break
		}
		fmt.Printf("  %9s  %s\n", ms(t.WallMS), t.Test)
	}
	fmt.Printf("\ntestgate: profile doc %s\n", docPath)
	if len(res.Errors) > 0 {
		fmt.Println("testgate: FAIL (profiler invocation errors)")
		for _, e := range res.Errors {
			fmt.Printf("  %s\n", e)
		}
		return 1
	}
	return 0
}

// defaultBatchCount keeps offered load well under the core count so the
// scheduler is not the bottleneck: half the cores for a no-race run, a quarter
// for a race run (the detector inflates per-test CPU several-fold).
func defaultBatchCount(race bool) int {
	cores := runtime.GOMAXPROCS(0)
	if race {
		if n := cores / 4; n > 1 {
			return n
		}
		return 1
	}
	if n := cores / 2; n > 1 {
		return n
	}
	return 1
}

// loadPriorWeights reads a prior `go test -json` stream and returns per-top-level
// test elapsed times for the LPT planner. Only relative magnitudes matter.
func loadPriorWeights(path string) (map[string]time.Duration, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open prior stream %s: %w", path, err)
	}
	defer f.Close()
	records, err := teststream.ParseStream(f)
	if err != nil {
		return nil, fmt.Errorf("parse prior stream %s: %w", path, err)
	}
	weights := map[string]time.Duration{}
	for _, rec := range teststream.TopLevel(records) {
		if rec.Elapsed > weights[rec.Test] {
			weights[rec.Test] = rec.Elapsed
		}
	}
	return weights, nil
}

func ms(v int64) string {
	return (time.Duration(v) * time.Millisecond).Round(time.Millisecond).String()
}
