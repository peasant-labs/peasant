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
// Usage:
//
//	scripts/testgate plan    print the run plan; run nothing
//	scripts/testgate run     execute the plan and screen the result
//
// See TESTING.md, "Test gate", for the contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/testgate"
	"github.com/peasant-labs/peasant/internal/teststream"
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
	_ = fs.Parse(os.Args[2:])

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

	switch sub {
	case "plan":
		if err := runPlan(root, reg, *raceFlag); err != nil {
			fatal(2, "plan failed", err)
		}
	case "run":
		code := runGate(root, reg, *outDir, *parallel, *raceFlag)
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
  testgate plan [flags]   print the run plan; run nothing
  testgate run  [flags]   execute the plan and screen the result

flags:
  -registry PATH   registry fixture (default: <repo>/no-race-partition.yaml)
  -out DIR         stream output dir (default: .agents.local/testgate/<ts>)
  -p N             packages invoked concurrently (default: GOMAXPROCS)
  -race            run the race pass (default: $RACE != 0)

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

func buildPlan(root string, reg testgate.Registry) (*testgate.Plan, error) {
	modulePath, err := testgate.ModulePath(root)
	if err != nil {
		return nil, err
	}
	tests, listWall, err := testgate.ListTests(root)
	if err != nil {
		return nil, err
	}
	plan, err := testgate.BuildPlan(root, modulePath, tests, reg)
	if err != nil {
		return nil, err
	}
	plan.ListWall = listWall
	return plan, nil
}

func runPlan(root string, reg testgate.Registry, race bool) error {
	plan, err := buildPlan(root, reg)
	if err != nil {
		return err
	}
	printPlan(plan, race)
	if len(plan.MissingRegistered) > 0 {
		return fmt.Errorf("registered tests missing from go test -list: %s", strings.Join(plan.MissingRegistered, ", "))
	}
	return nil
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

func runGate(root string, reg testgate.Registry, outDir string, concurrency int, race bool) int {
	plan, err := buildPlan(root, reg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: plan failed: %v\n", err)
		return 2
	}
	fmt.Printf("testgate: plan %d packages, list-wall %s (recorded separately from the test wall)\n", len(plan.Packages), round(plan.ListWall))

	goBin, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testgate: go is not on PATH: %v\n", err)
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
	var invocationErrors []string

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
		invocationErrors = append(invocationErrors, resA.Errors...)
		invocationErrors = append(invocationErrors, resB.Errors...)
		printRecords("race", resA.Records[testgate.ModeRace])
		printRecords("no-race", resB.Records[testgate.ModeNoRace])
	} else {
		res, err := runner.Run(ctx, plan, testgate.ModeNoRace, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testgate: no-race pass failed to run: %v\n", err)
			return 2
		}
		streams[testgate.ModeNoRace] = res.Streams[testgate.ModeNoRace]
		walls[testgate.ModeNoRace] = res.Walls[testgate.ModeNoRace]
		invocationErrors = append(invocationErrors, res.Errors...)
		printRecords("no-race (single pass)", res.Records[testgate.ModeNoRace])
	}
	testWall := time.Since(testStart)

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
	printBudget(calL, testWall)

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

	fmt.Printf("\ntestgate: streams under %s\n", outDir)
	if len(failed) > 0 || testgate.Fails(findings) || len(invocationErrors) > 0 {
		fmt.Println("testgate: FAIL")
		return 1
	}
	fmt.Println("testgate: PASS")
	return 0
}

func printRecords(label string, records []testgate.Record) {
	sort.Slice(records, func(i, j int) bool { return records[i].Wall > records[j].Wall })
	fmt.Printf("\n=== per-invocation records (%s) ===\n", label)
	fmt.Printf("%-56s %-22s %9s %9s %9s\n", "UNIT", "CLASS", "WALL", "USER", "SYSTEM")
	for _, r := range records {
		fmt.Printf("%-56s %-22s %9s %9s %9s\n", r.Unit, r.Class, round(r.Wall), round(r.User), round(r.System))
	}
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

func printBudget(calL float64, testWall time.Duration) {
	raw := budgetFromEnv()
	if raw <= 0 {
		fmt.Println("budget:                 none committed (a later commit pins the reference value); raw walls only")
		return
	}
	if calL > 4 {
		fmt.Printf("budget:                 %ds reference; INCONCLUSIVE under load (L=%.3f), not failed\n", raw, calL)
		return
	}
	normalized := time.Duration(float64(testWall) / calL)
	fmt.Printf("budget:                 %ds reference; normalized test wall %s (%s / L)\n", raw, round(normalized), round(testWall))
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
