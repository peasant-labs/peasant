// Command testgate runs the two-pass test gate and applies the exactly-once
// screen.
//
// It computes a run plan from `go test -list`, partitions the suite into a
// race pass and a no-race pass (no-race-partition.yaml), executes both, merges
// their streams, and verifies that every test ran exactly once. A package the
// registry names that produced no test events fails; an unregistered one that
// produced none is only reported. RACE=0 collapses to a single no-race pass but
// still plans and screens.
//
// -pkgs narrows the run to a comma-separated set of package patterns (default
// ./...). A narrower set is a subset run: it owes events only for the packages
// it planned, and the whole-suite budget is not applicable to it.
//
// Usage:
//
//	cmd/testgate plan    print the run plan; run nothing
//	cmd/testgate run     execute the plan and screen the result
//
// See TESTING.md, "Test gate", for the contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/testgate"
	"github.com/peasant-labs/peasant/internal/testkit/teststream"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	registry := fs.String("registry", "", "path to no-race-partition.yaml (default: repo root)")
	outDir := fs.String("out", "", "directory for streams and profiles (default: $TESTGATE_OUT or .agents.local/testgate/<ts>)")
	parallel := fs.Int("p", runtime.GOMAXPROCS(0), "packages to invoke concurrently (default: GOMAXPROCS)")
	raceFlag := fs.Bool("race", os.Getenv("RACE") != "0", "run the race pass (default: $RACE != 0)")
	pkgsFlag := fs.String("pkgs", "./...", "run/plan: comma-separated repo-relative package patterns (default: ./...)")
	pkgFlag := fs.String("pkg", "", "profile: repo-relative package to profile (e.g. ./internal/ingest)")
	batchesFlag := fs.Int("n", 0, "profile: concurrent batches (default: half the cores, a quarter under -race)")
	parallelFlag := fs.Int("parallel", 1, "profile: per-batch -parallel; 0 leaves it unpinned (isolated run)")
	priorFlag := fs.String("prior", "", "profile: prior go test -json stream used for LPT batch weights")
	cpuTopFlag := fs.Int("cpuprofile-top", 0, "profile: re-profile the N slowest tests with -cpuprofile")
	pretestFlag := fs.Bool("pretest", false, "profile: measure the five pre-test steps instead of a package")
	profilesFlag := fs.Bool("profiles", false, "profile: write Class B block/mutex/cpu profiles per batch")
	traceFlag := fs.Bool("trace", false, "profile: also write a runtime trace per batch (perturbs block.out)")
	timingTop := fs.Int("top", 25, "timing: rows to print per section")
	timingFamilyRe := fs.String("family-re", "", "timing: regexp with one capture group; overrides default family grouping")
	timingNoFamilies := fs.Bool("no-families", false, "timing: skip the per-family section")
	timingWarnPct := fs.Float64("warn-pct", 5, "timing: flag tests whose share of elapsed time exceeds this percentage")
	_ = fs.Parse(os.Args[2:])

	// timing is a stream-only frontend for an arbitrary `go test -json` input: it
	// needs neither the repository root nor the registry, so dispatch it before
	// that wiring and keep `... | testgate timing` usable from anywhere in the
	// module.
	if sub == "timing" {
		os.Exit(runTiming(fs.Args(), *timingTop, *timingFamilyRe, *timingNoFamilies, *timingWarnPct))
	}

	root, err := findRepoRoot()
	if err != nil {
		fatal(2, "cannot locate the repository root", err)
	}
	if *registry == "" {
		*registry = filepath.Join(root, "no-race-partition.yaml")
	}
	if *outDir == "" {
		*outDir = defaultOutDir(root)
	}

	reg, err := testgate.LoadRegistry(*registry)
	if err != nil {
		fatal(2, "cannot load the registry", err)
	}
	if err := testgate.ValidateRegistry(root, reg); err != nil {
		fatal(2, "registry is invalid", err)
	}

	patterns := parsePatterns(*pkgsFlag)
	subset := !isFullSuite(patterns)

	switch sub {
	case "plan":
		if err := runPlan(root, reg, *raceFlag, patterns, subset); err != nil {
			fatal(2, "plan failed", err)
		}
	case "run":
		code := runGate(root, reg, *outDir, *parallel, *raceFlag, patterns, subset)
		os.Exit(code)
	case "profile":
		goBin, err := exec.LookPath("go")
		if err != nil {
			fmt.Fprintf(os.Stderr, "testgate: go is not on PATH: %v\n", err)
			os.Exit(2)
		}
		code := runProfile(profileOptions{
			root:     root,
			goBin:    goBin,
			outDir:   *outDir,
			pkg:      *pkgFlag,
			batches:  *batchesFlag,
			parallel: *parallelFlag,
			prior:    *priorFlag,
			cpuTop:   *cpuTopFlag,
			pretest:  *pretestFlag,
			profiles: *profilesFlag,
			trace:    *traceFlag,
			race:     *raceFlag,
		})
		os.Exit(code)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `testgate — two-pass test gate with an exactly-once screen

usage:
  testgate plan [flags]     print the run plan; run nothing
  testgate run  [flags]     execute the plan and screen the result
  testgate profile [flags]  measure: batched LPT per-test profile, or -pretest
  testgate timing [flags] [file]
                            summarize a go test -json stream (stdin when no file)

flags:
  -registry PATH   registry fixture (default: <repo>/no-race-partition.yaml)
  -out DIR         stream output dir (default: .agents.local/testgate/<ts>)
  -p N             packages invoked concurrently (default: GOMAXPROCS)
  -race            run the race pass (default: $RACE != 0)
  -pkgs PATTERNS   run/plan: comma-separated repo-relative package patterns
                   (default: ./...); a narrower set is a SUBSET run, whose
                   result is not a full-suite result and which reports the
                   whole-suite budget as not applicable
  -pkg DIR         profile: repo-relative package to profile
  -n N             profile: concurrent batches
  -parallel N      profile: per-batch -parallel; 0 is unpinned (isolated run)
  -prior FILE      profile: prior go test -json stream for LPT weights
  -profiles        profile: write Class B block/mutex/cpu profiles
  -trace           profile: also write a runtime trace (perturbs block.out)
  -pretest         profile: measure the five pre-test steps
  -top N           timing: rows to print per section (default 25)
  -family-re RE    timing: regexp with one capture group; overrides default family grouping
  -no-families     timing: skip the per-family section
  -warn-pct PCT    timing: flag tests whose share of elapsed time exceeds this percentage

exit codes:
  0  the gate passed
  1  a test failed, the screen failed, or an invocation errored
  2  usage, registry, or plan error
`)
}

func fatal(code int, msg string, err error) {
	fmt.Fprintf(os.Stderr, "testgate: %s\n  %v\n", msg, err)
	os.Exit(code)
}

// runTiming summarizes a `go test -json` stream into the ranked per-test and
// per-family report.
//
// It is the CLI half of the shared renderer in internal/testkit/teststream: the same
// library that renders the gate's own merged passes renders an ad-hoc stream
// here, so a measurement taken by hand and one taken by the gate are the same
// report. A file argument is read in place of stdin. A failing test makes the
// exit non-zero so the mode can gate directly.
func runTiming(args []string, top int, familyRe string, noFamilies bool, warnPct float64) int {
	var re *regexp.Regexp
	if familyRe != "" {
		var err error
		re, err = regexp.Compile(familyRe)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testgate timing: bad -family-re: %v\n", err)
			return 2
		}
	}

	var in io.Reader = os.Stdin
	if len(args) > 0 {
		f, err := os.Open(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "testgate timing: %v\n", err)
			return 1
		}
		defer f.Close()
		in = f
	}

	records, err := teststream.ParseStream(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate timing: %v\n", err)
		return 1
	}
	if len(records) == 0 {
		fmt.Fprintln(os.Stderr, "testgate timing: no test events on input (did you pipe `go test -json`?)")
		return 1
	}
	if err := teststream.Report(os.Stdout, records, teststream.ReportOptions{
		Top:        top,
		FamilyRe:   re,
		NoFamilies: noFamilies,
		WarnPct:    warnPct,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "testgate timing: %v\n", err)
		return 1
	}

	for _, r := range teststream.Failing(records) {
		fmt.Fprintf(os.Stderr, "testgate timing: FAILING TEST: %s (%s)\n", r.Test, r.Package)
		return 1
	}
	return 0
}

// findRepoRoot walks up from the working directory to the go.mod root.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

func defaultOutDir(root string) string {
	if v := os.Getenv("TESTGATE_OUT"); v != "" {
		return v
	}
	return filepath.Join(root, ".agents.local", "testgate", time.Now().UTC().Format("20060102T150405Z"))
}

// parsePatterns splits the comma-separated -pkgs value and drops blanks. An
// empty result means the whole module.
func parsePatterns(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{testgate.DefaultPackagePattern}
	}
	return out
}

// isFullSuite reports whether patterns name the whole module. Only the exact
// whole-module pattern is a full-suite run; every narrower set is a subset run.
func isFullSuite(patterns []string) bool {
	return len(patterns) == 1 && patterns[0] == testgate.DefaultPackagePattern
}

// scopeRegistryForRun restricts the registry to the packages a subset run
// actually requested. A full-suite run keeps the whole registry. It returns the
// scoped registry, the module path, and the total repo package count (for the
// subset header; 0 when the run is full).
func scopeRegistryForRun(root string, reg testgate.Registry, patterns []string, subset bool) (testgate.Registry, string, int, error) {
	modulePath, err := testgate.ModulePath(root)
	if err != nil {
		return testgate.Registry{}, "", 0, err
	}
	if !subset {
		return reg, modulePath, 0, nil
	}
	inScope, err := testgate.ListPackages(root, patterns)
	if err != nil {
		return testgate.Registry{}, "", 0, err
	}
	dirs := map[string]bool{}
	for _, ip := range inScope {
		dirs[testgate.PackageDir(modulePath, ip)] = true
	}
	all, err := testgate.ListPackages(root, []string{testgate.DefaultPackagePattern})
	if err != nil {
		return testgate.Registry{}, "", 0, err
	}
	return testgate.ScopeRegistry(reg, dirs), modulePath, len(all), nil
}

// planForRun builds the run plan for the requested package patterns. A subset
// run scopes the registry to the requested packages first, so a registered
// package outside the subset is not held to the liveness rule. It returns the
// plan and the total repo package count (0 when the run is full).
func planForRun(root string, reg testgate.Registry, patterns []string, subset bool) (*testgate.Plan, int, error) {
	scoped, modulePath, total, err := scopeRegistryForRun(root, reg, patterns, subset)
	if err != nil {
		return nil, 0, err
	}
	tests, listWall, err := testgate.ListTests(root, patterns)
	if err != nil {
		return nil, 0, err
	}
	plan, err := testgate.BuildPlan(root, modulePath, tests, scoped)
	if err != nil {
		return nil, 0, err
	}
	plan.ListWall = listWall
	return plan, total, nil
}

func runPlan(root string, reg testgate.Registry, race bool, patterns []string, subset bool) error {
	plan, total, err := planForRun(root, reg, patterns, subset)
	if err != nil {
		return err
	}
	if subset {
		printSubsetHeader(patterns, len(plan.Packages), total)
	}
	printPlan(plan, race)
	if len(plan.MissingRegistered) > 0 {
		return fmt.Errorf("registered tests missing from go test -list: %s", strings.Join(plan.MissingRegistered, ", "))
	}
	return nil
}

// printSubsetHeader names the subset before any result, so a reader cannot
// mistake a subset run for a full-suite gate result.
func printSubsetHeader(patterns []string, planned, total int) {
	fmt.Printf("testgate: SUBSET RUN — patterns: %s\n", strings.Join(patterns, ", "))
	fmt.Printf("testgate: subset packages: %d of %d repo packages; NOT a full-suite gate result\n", planned, total)
}

func printPlan(plan *testgate.Plan, race bool) {
	fmt.Printf("testgate plan: %d packages listed, list-wall %s\n", len(plan.Packages), round(plan.ListWall))
	var racePkgs, noRacePkgs, raceTests, noRaceTests int
	for _, p := range plan.Packages {
		if len(p.RaceTests) > 0 {
			racePkgs++
			raceTests += len(p.RaceTests)
		}
		if len(p.NoRaceTests) > 0 {
			noRacePkgs++
			noRaceTests += len(p.NoRaceTests)
		}
	}
	if race {
		fmt.Printf("  race pass:    %d packages, %d tests\n", racePkgs, raceTests)
		fmt.Printf("  no-race pass: %d packages, %d tests\n", noRacePkgs, noRaceTests)
	} else {
		fmt.Printf("  single no-race pass (RACE=0): %d packages\n", len(plan.Packages))
	}
	fmt.Println("  registered packages:")
	for _, p := range plan.Packages {
		if !p.Registered {
			continue
		}
		fmt.Printf("    %-52s race=%d no-race=%d\n", p.Dir, len(p.RaceTests), len(p.NoRaceTests))
		for _, name := range p.NoRaceTests {
			fmt.Printf("      partition %s (%s)\n", name, p.NoRaceClasses[name])
		}
	}
	if len(plan.MissingRegistered) > 0 {
		fmt.Println("  MISSING registered tests (not listed):")
		for _, m := range plan.MissingRegistered {
			fmt.Printf("    %s\n", m)
		}
	}
}

func runGate(root string, reg testgate.Registry, outDir string, concurrency int, race bool, patterns []string, subset bool) int {
	plan, totalPackages, err := planForRun(root, reg, patterns, subset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: plan failed: %v\n", err)
		return 2
	}
	if subset {
		printSubsetHeader(patterns, len(plan.Packages), totalPackages)
	}
	fmt.Printf("testgate: plan %d packages, list-wall %s (recorded separately from the test wall)\n", len(plan.Packages), round(plan.ListWall))

	goBin, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: go is not on PATH: %v\n", err)
		return 2
	}

	budgetSeconds, budgetBasis, budgetPresent, err := resolveBudget(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: %v\n", err)
		return 2
	}

	checkStart, haveCheckStart := checkStartNS()
	testStart := time.Now()
	if haveCheckStart {
		fmt.Printf("testgate: pre-test wall %s (from CHECK_START_NS)\n", round(testStart.Sub(checkStart)))
	} else {
		fmt.Println("testgate: pre-test wall n/a (CHECK_START_NS unset)")
	}
	calL, calRaw := testgate.Calibrate()
	fmt.Printf("testgate: calibration L=%.3f (probe %s, reference %s)\n", calL, round(calRaw), round(time.Duration(float64(testgate.CalibrationReferenceNS))))
	if calL > 4 {
		fmt.Printf("testgate: INCONCLUSIVE — L=%.3f > 4: this box is too loaded to quote a budget against\n", calL)
	}

	runner := &testgate.Runner{
		Root:        root,
		OutDir:      outDir,
		GoBin:       goBin,
		Concurrency: concurrency,
		SerialPassB: true,
	}
	ctx := context.Background()
	streams := map[testgate.PassMode]map[string][]teststream.Record{}
	walls := map[testgate.PassMode]time.Duration{}
	passCPU := map[testgate.PassMode][2]time.Duration{}
	var raceRecords, noRaceRecords []testgate.Record
	invocationErrors := []string{}

	if race {
		resA, err := runner.Run(ctx, plan, testgate.ModeRace, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testgate: race pass failed to run: %v\n", err)
			return 2
		}
		resB, err := runner.Run(ctx, plan, testgate.ModeNoRace, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testgate: no-race pass failed to run: %v\n", err)
			return 2
		}
		streams[testgate.ModeRace] = resA.Streams[testgate.ModeRace]
		streams[testgate.ModeNoRace] = resB.Streams[testgate.ModeNoRace]
		walls[testgate.ModeRace] = resA.Walls[testgate.ModeRace]
		walls[testgate.ModeNoRace] = resB.Walls[testgate.ModeNoRace]
		passCPU[testgate.ModeRace] = [2]time.Duration{resA.User, resA.System}
		passCPU[testgate.ModeNoRace] = [2]time.Duration{resB.User, resB.System}
		invocationErrors = append(invocationErrors, resA.Errors...)
		invocationErrors = append(invocationErrors, resB.Errors...)
		raceRecords = resA.Records[testgate.ModeRace]
		noRaceRecords = resB.Records[testgate.ModeNoRace]
		printRecords("race", raceRecords)
		printRecords("no-race", noRaceRecords)
	} else {
		res, err := runner.Run(ctx, plan, testgate.ModeNoRace, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testgate: no-race pass failed to run: %v\n", err)
			return 2
		}
		streams[testgate.ModeNoRace] = res.Streams[testgate.ModeNoRace]
		walls[testgate.ModeNoRace] = res.Walls[testgate.ModeNoRace]
		passCPU[testgate.ModeNoRace] = [2]time.Duration{res.User, res.System}
		invocationErrors = append(invocationErrors, res.Errors...)
		noRaceRecords = res.Records[testgate.ModeNoRace]
		printRecords("no-race (single pass)", noRaceRecords)
	}
	testWall := time.Since(testStart)

	// The per-class decision table: registry classes and the race pass from the
	// test records, plus measured pre-test steps when a pre-test doc is present
	// in the same output directory. AttributionPasses chooses serialized
	// per-invocation CPU for the RACE=1 partition pass and pass-level CPU for
	// every concurrent pass, including the RACE=0 single pass.
	passes := testgate.AttributionPasses(
		race,
		walls[testgate.ModeRace], walls[testgate.ModeNoRace],
		passCPU[testgate.ModeRace][0], passCPU[testgate.ModeRace][1],
		passCPU[testgate.ModeNoRace][0], passCPU[testgate.ModeNoRace][1],
		len(raceRecords), len(noRaceRecords),
	)
	classTable := testgate.BuildClassTable(passes, append(append([]testgate.Record{}, raceRecords...), noRaceRecords...))

	var preTestRecords []testgate.ReportRecord
	preTestDoc, havePreTest, preTestErr := testgate.ReadPreTestDocument(filepath.Join(outDir, "pretest.json"))
	if preTestErr != nil {
		fmt.Fprintf(os.Stderr, "testgate: %v\n", preTestErr)
	}
	if havePreTest {
		classTable = append(classTable, preTestDoc.Rows...)
		preTestRecords = testgate.StepRecords(preTestDoc.Steps)
	}
	printClassTable(classTable)
	printCapacityCheck(race, passCPU)

	combined := &testgate.RunResult{Streams: streams}
	failed := combined.FailedTests()

	findings := testgate.Screen(testgate.ScreenInput{Plan: plan, Registry: reg, Race: race, Streams: streams})

	fmt.Println()
	fmt.Println("=== summary ===")
	if race {
		fmt.Printf("pass A (race) wall:     %s\n", round(walls[testgate.ModeRace]))
		fmt.Printf("pass B (no-race) wall:  %s\n", round(walls[testgate.ModeNoRace]))
	} else {
		fmt.Printf("single no-race wall:    %s\n", round(walls[testgate.ModeNoRace]))
	}
	fmt.Printf("combined test wall:     %s\n", round(testWall))
	if haveCheckStart {
		fmt.Printf("pre-test wall:          %s\n", round(testStart.Sub(checkStart)))
	} else {
		fmt.Println("pre-test wall:          n/a (CHECK_START_NS unset)")
	}
	fmt.Printf("calibration L:          %.3f\n", calL)
	printControls(concurrency, race)
	budgetText, budgetFail := budgetVerdict(subset, len(plan.Packages), totalPackages, calL, testWall, budgetSeconds, budgetBasis, budgetPresent)
	fmt.Println(budgetText)

	fmt.Println()
	fmt.Println("=== screen ===")
	if len(findings) == 0 {
		fmt.Println("all four rules passed: every test ran exactly once across the passes")
	} else {
		for _, f := range findings {
			fmt.Print(f.Render())
		}
	}
	if len(invocationErrors) > 0 {
		fmt.Println()
		fmt.Println("=== invocation errors ===")
		for _, e := range invocationErrors {
			fmt.Printf("  %s\n", e)
		}
	}

	if len(failed) > 0 {
		fmt.Println()
		fmt.Println("=== failing tests ===")
		for _, f := range failed {
			fmt.Printf("  %s (%s)\n", f.Test, f.Package)
		}
	}

	report := testgate.Report{
		SchemaVersion:    1,
		Module:           plan.ModulePath,
		Race:             race,
		Concurrency:      concurrency,
		GOMAXPROCS:       runtime.GOMAXPROCS(0),
		ListWallMS:       plan.ListWall.Milliseconds(),
		CombinedWallMS:   testWall.Milliseconds(),
		Calibration:      testgate.Calibration{L: calL, ProbeMS: calRaw.Milliseconds(), ReferenceMS: testgate.CalibrationReferenceNS / int64(time.Millisecond), Inconclusive: calL > 4},
		Records:          append(append([]testgate.ReportRecord{}, reportRecords(raceRecords)...), reportRecords(noRaceRecords)...),
		Findings:         reportFindings(findings),
		FailedTests:      reportTests(failed),
		InvocationErrors: invocationErrors,
		ClassTable:       classTable,
		PreTestSteps:     preTestRecords,
	}
	if haveCheckStart {
		report.PreTestWallMS = testStart.Sub(checkStart).Milliseconds()
	}
	if race {
		report.PassA = passReport("race", plan, testgate.ModeRace, walls[testgate.ModeRace], passCPU[testgate.ModeRace], race)
		report.PassB = passReport("no-race", plan, testgate.ModeNoRace, walls[testgate.ModeNoRace], passCPU[testgate.ModeNoRace], race)
	} else {
		report.PassB = passReport("no-race (single pass)", plan, testgate.ModeNoRace, walls[testgate.ModeNoRace], passCPU[testgate.ModeNoRace], race)
	}
	reportPath := filepath.Join(outDir, "report.json")
	if err := testgate.WriteReport(reportPath, report); err != nil {
		fmt.Fprintf(os.Stderr, "testgate: write report: %v\n", err)
		return 2
	}

	fmt.Printf("\ntestgate: streams under %s\n", outDir)
	fmt.Printf("testgate: report %s\n", reportPath)
	if len(failed) > 0 || testgate.Fails(findings) || len(invocationErrors) > 0 || budgetFail {
		fmt.Println("testgate: FAIL")
		return 1
	}
	fmt.Println("testgate: PASS")
	return 0
}

func reportRecords(records []testgate.Record) []testgate.ReportRecord {
	out := make([]testgate.ReportRecord, 0, len(records))
	for _, r := range records {
		out = append(out, testgate.ReportRecord{
			Unit:     r.Unit,
			Class:    string(r.Class),
			Pass:     r.Pass.String(),
			WallMS:   r.Wall.Milliseconds(),
			UserMS:   r.User.Milliseconds(),
			SystemMS: r.System.Milliseconds(),
		})
	}
	return out
}

func reportFindings(findings []testgate.Finding) []testgate.ReportFinding {
	out := make([]testgate.ReportFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, testgate.ReportFinding{
			Rule: f.Rule, Severity: f.Severity.String(), What: f.What, Why: f.Why,
			Where: f.Where, When: f.When, Means: f.Means, Fix: f.Fix,
		})
	}
	return out
}

func reportTests(records []teststream.Record) []testgate.ReportTest {
	out := make([]testgate.ReportTest, 0, len(records))
	for _, r := range records {
		out = append(out, testgate.ReportTest{Package: r.Package, Test: r.Test})
	}
	return out
}

// passReport counts the packages and tests a pass ran, and carries the pass's
// whole-pass child CPU and the derived wall − CPU gap.
func passReport(name string, plan *testgate.Plan, mode testgate.PassMode, wall time.Duration, cpu [2]time.Duration, race bool) *testgate.PassReport {
	pkgs, tests := 0, 0
	for _, p := range plan.Packages {
		var n int
		switch {
		case mode == testgate.ModeRace:
			n = len(p.RaceTests)
		case race:
			n = len(p.NoRaceTests)
		default:
			n = len(p.Tests)
		}
		if n > 0 {
			pkgs++
			tests += n
		}
	}
	wallMS := wall.Milliseconds()
	userMS := cpu[0].Milliseconds()
	sysMS := cpu[1].Milliseconds()
	return &testgate.PassReport{
		Name: name, WallMS: wallMS, Packages: pkgs, Tests: tests,
		UserMS: userMS, SystemMS: sysMS, GapMS: wallMS - userMS - sysMS,
	}
}

func printRecords(label string, records []testgate.Record) {
	sort.Slice(records, func(i, j int) bool { return records[i].Wall > records[j].Wall })
	fmt.Printf("\n=== per-invocation records (%s) ===\n", label)
	fmt.Printf("%-56s %-22s %9s %9s %9s\n", "UNIT", "CLASS", "WALL", "USER", "SYSTEM")
	for _, r := range records {
		fmt.Printf("%-56s %-22s %9s %9s %9s\n", r.Unit, r.Class, round(r.Wall), round(r.User), round(r.System))
	}
}

// printClassTable prints the per-class wall/user/system decision table and the
// derived wall − CPU gap: the work more cores cannot compress.
func printClassTable(rows []testgate.ClassRow) {
	fmt.Println("\n=== per-class decision table (wall − CPU = work cores cannot compress) ===")
	fmt.Printf("%-24s %5s %9s %9s %9s %9s  %s\n", "CLASS", "UNITS", "WALL", "USER", "SYSTEM", "GAP", "BASIS")
	for _, r := range rows {
		fmt.Printf("%-24s %5d %9s %9s %9s %9s  %s\n", r.Class, r.Units, ms(r.WallMS), ms(r.UserMS), ms(r.SystemMS), ms(r.GapMS), r.Basis)
	}
}

// printCapacityCheck states the CPU-numerator bookkeeping. The bar is held at
// 120 s (a user ruling, not this measurement's decision); this tool measures the
// CPU input and asserts no impossibility claim in either direction.
func printCapacityCheck(race bool, passCPU map[testgate.PassMode][2]time.Duration) {
	var total time.Duration
	for _, mode := range []testgate.PassMode{testgate.ModeRace, testgate.ModeNoRace} {
		c := passCPU[mode]
		total += c[0] + c[1]
	}
	fmt.Println("\n=== capacity check (CPU seconds; the bar is HELD at 120s — this measurement does not decide it) ===")
	fmt.Printf("measured CPU today (user+system, all passes): %s\n", round(total))
	fmt.Println("Y  post-optimization CPU numerator: PENDING (the parallelism/faking work has not run)")
	fmt.Println("check CPU_seconds(Y)/120: PENDING — no impossibility claim is asserted in either direction")
}

func printControls(concurrency int, race bool) {
	fmt.Printf("effective -p:           %d (package invocations concurrent)\n", concurrency)
	fmt.Printf("effective -parallel:    %d per package (default: GOMAXPROCS; not pinned)\n", runtime.GOMAXPROCS(0))
	fmt.Printf("-count:                 1 (result cache bypassed)\n")
	cache, _ := exec.Command("go", "env", "GOCACHE").Output()
	fmt.Printf("cache:                  GOCACHE=%s (results not cached; -count=1)\n", strings.TrimSpace(string(cache)))
	if race {
		fmt.Printf("race:                   on (pass A)\n")
	} else {
		fmt.Printf("race:                   off (RACE=0; single pass, plan + screen still run)\n")
	}
}

// budgetVerdict renders the budget line and returns whether the gate should
// fail on a budget miss. A miss fails closed, except that L > 4 is INCONCLUSIVE
// (loud, exit 0) because a loaded box cannot be quoted against a reference
// budget. A subset run has no whole-suite budget: its line says so loudly and
// never fails, because a subset wall is not comparable to the full-suite bar.
func budgetVerdict(subset bool, planned, total int, calL float64, testWall time.Duration, seconds int, basis string, present bool) (string, bool) {
	if subset {
		return fmt.Sprintf("budget:                 not applicable (subset run: %d of %d packages)", planned, total), false
	}
	if !present {
		return "budget:                 none committed (a later commit pins the reference value); raw walls only", false
	}
	tag := ""
	if basis != "" {
		tag = " (" + basis + ")"
	}
	if calL > 4 {
		return fmt.Sprintf("budget:                 %ds reference%s; INCONCLUSIVE under load (L=%.3f), not failed", seconds, tag, calL), false
	}
	normalized := time.Duration(float64(testWall) / calL)
	verdict := "PASS"
	fail := false
	if normalized > time.Duration(seconds)*time.Second {
		verdict = "FAIL"
		fail = true
	}
	return fmt.Sprintf("budget:                 %ds reference%s; normalized test wall %s (%s / L) -> %s", seconds, tag, round(normalized), round(testWall), verdict), fail
}

// printBudget prints the full-run budget line and returns whether the gate
// should fail. It is the subset-free view of budgetVerdict.
func printBudget(calL float64, testWall time.Duration, seconds int, basis string, present bool) bool {
	line, fail := budgetVerdict(false, 0, 0, calL, testWall, seconds, basis, present)
	fmt.Println(line)
	return fail
}

// resolveBudget reads budget.yaml, then TEST_BUDGET, then reports no budget.
// A malformed budget fixture is an error so a bad value cannot silently disable
// the check.
func resolveBudget(root string) (int, string, bool, error) {
	b, found, err := testgate.LoadBudget(filepath.Join(root, "budget.yaml"))
	if err != nil {
		return 0, "", false, err
	}
	if found {
		return b.Seconds, b.Basis, true, nil
	}
	if n := budgetFromEnv(); n > 0 {
		return n, "TEST_BUDGET", true, nil
	}
	return 0, "", false, nil
}

func budgetFromEnv() int {
	v := os.Getenv("TEST_BUDGET")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func checkStartNS() (time.Time, bool) {
	v := os.Getenv("CHECK_START_NS")
	if v == "" {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

func round(d time.Duration) string {
	if d < time.Millisecond {
		return d.Round(time.Microsecond).String()
	}
	return d.Round(time.Millisecond).String()
}
