// Command testshard partitions one package's top-level tests into cross-process
// `-run` shards and audits the partition.
//
// A package whose tests are serial in-process (for example because they call
// t.Setenv, which forbids t.Parallel) runs its tests one after another in a
// single test binary and cannot use more than one core. Sharding splits that
// package into several `go test -run <subset>` invocations that a runner can
// execute concurrently, each carrying the race detector.
//
// The subset for every shard is COMPUTED from the package's `go test -list`
// output and its measured per-test wall, never hand-written. The audit proves
// the shard sets are an exact partition: every top-level test appears in exactly
// one shard, and no test appears twice. Because each shard is its own process,
// a per-process test fixture (such as the refcounted golden database in
// internal/store/storetest) is recreated per shard and cannot collide across
// shards.
//
// Usage:
//
//	testshard plan -pkg ./internal/api [-n 4] [-costs prior.json] [-out plan.json]
//	testshard run  -pkg ./internal/api [-n 4] [-count 2] [-race] [-costs prior.json]
//
// `plan` prints the computed shards and the audit; `run` executes each shard
// with the race detector and `-count` repetitions and fails if any shard fails
// or the audit finds a collision.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// TestCost is one top-level test and its measured wall. An unmeasured test
// carries zero, which packs it as cheap; a caller that has measured the package
// supplies -costs so the partition balances real work.
type TestCost struct {
	Name   string `json:"name"`
	WallMS int64  `json:"wall_ms,omitempty"`
}

// Shard is one cross-process `-run` subset. Tests are disjoint from every
// sibling shard and their union is the package's top-level test list.
type Shard struct {
	Index    int        `json:"index"`
	Tests    []TestCost `json:"tests"`
	WallMS   int64      `json:"wall_ms"`
	RunRegex string     `json:"run_regex"`
	EnvBound int        `json:"env_bound"`
	Serial   int        `json:"serial"`
}

// Plan is the computed shard set for one package.
type Plan struct {
	Package  string  `json:"package"`
	Shards   int     `json:"shards"`
	Total    int     `json:"total_tests"`
	Covered  int     `json:"covered_tests"`
	ShardSet []Shard `json:"shard_set"`
}

// topLevelTest matches a Go test function name. It deliberately excludes
// subtests (which contain "/") so a shard's `-run` regex selects whole top-level
// tests.
var topLevelTest = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "plan", "run":
		code := run(os.Args[1], os.Args[2:])
		os.Exit(code)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `testshard — computed cross-process test shards with an exact-partition audit

usage:
  testshard plan [flags]   print the computed shards and the collision audit
  testshard run  [flags]   execute every shard with -race and audit the partition

flags:
  -pkg DIR       package directory to shard (required), e.g. ./internal/api
  -n N           number of shards (default: GOMAXPROCS)
  -costs FILE    a prior 'go test -json' stream used to balance the shards
  -out FILE      write the plan as JSON (plan only)
  -count N       test repetitions per shard (run only, default 1)
  -race          build shards with the race detector (run only, default true)
  -go BIN        go binary (default: go on PATH)
`)
}

func run(sub string, args []string) int {
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	pkg := fs.String("pkg", "", "package directory to shard")
	n := fs.Int("n", 0, "number of shards (default GOMAXPROCS)")
	costs := fs.String("costs", "", "prior go test -json stream for LPT weights")
	out := fs.String("out", "", "write plan JSON here")
	count := fs.Int("count", 1, "test repetitions per shard")
	race := fs.Bool("race", true, "build shards with the race detector")
	goBin := fs.String("go", "go", "go binary")
	_ = fs.Parse(args)

	if *pkg == "" {
		fmt.Fprintln(os.Stderr, "testshard: -pkg is required")
		return 2
	}
	if *n <= 0 {
		*n = gomaxprocs()
	}
	costMap, err := loadCosts(*costs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testshard: %v\n", err)
		return 2
	}
	names, err := listTests(*goBin, *pkg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testshard: %v\n", err)
		return 2
	}
	if len(names) == 0 {
		fmt.Fprintf(os.Stderr, "testshard: package %s lists no top-level tests\n", *pkg)
		return 2
	}
	envBound, serial, err := discover(*pkg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testshard: %v\n", err)
		return 2
	}
	if *n > len(names) {
		// More shards than tests would leave empty shards; that is a request
		// error, not a partition the audit should pass.
		fmt.Fprintf(os.Stderr, "testshard: %d shards requested but %s lists only %d tests\n", *n, *pkg, len(names))
		return 2
	}
	plan := buildPlan(*pkg, *n, names, costMap, envBound, serial)
	if err := auditPlan(plan); err != nil {
		fmt.Fprintf(os.Stderr, "testshard: collision audit failed: %v\n", err)
		return 1
	}
	printPlan(plan)
	if *out != "" {
		if err := writePlan(*out, plan); err != nil {
			fmt.Fprintf(os.Stderr, "testshard: %v\n", err)
			return 1
		}
	}
	if sub == "plan" {
		return 0
	}
	return runShards(*goBin, *pkg, plan, *count, *race)
}

// listTests runs `go test -list` and returns the package's top-level test names.
func listTests(goBin, pkg string) ([]string, error) {
	cmd := exec.Command(goBin, "test", "-list", ".*", pkg)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list tests in %s: %w", pkg, err)
	}
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !topLevelTest.MatchString(line) || seen[line] {
			continue
		}
		seen[line] = true
		names = append(names, line)
	}
	sort.Strings(names)
	return names, nil
}

// loadCosts reads a prior `go test -json` stream and returns each top-level
// test's elapsed wall in milliseconds, taking the maximum across repetitions.
func loadCosts(path string) (map[string]int64, error) {
	if path == "" {
		return map[string]int64{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read costs %s: %w", path, err)
	}
	costs := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event struct {
			Action  string  `json:"Action"`
			Test    string  `json:"Test"`
			Elapsed float64 `json:"Elapsed"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Action != "pass" && event.Action != "fail" && event.Action != "skip" {
			continue
		}
		if event.Test == "" || strings.Contains(event.Test, "/") || !topLevelTest.MatchString(event.Test) {
			continue
		}
		ms := int64(event.Elapsed * 1000)
		if ms > costs[event.Test] {
			costs[event.Test] = ms
		}
	}
	return costs, nil
}

// discover parses the package's test sources and reports which top-level tests
// are env-bound (they call t.Setenv/os.Setenv) and which are serial (they do not
// call t.Parallel, or an env mutation forbids it). The two sets are computed
// from the AST, never hand-listed.
func discover(pkgDir string) (envBound, serial map[string]bool, err error) {
	envBound = map[string]bool{}
	serial = map[string]bool{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read package dir %s: %w", pkgDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !topLevelTest.MatchString(fn.Name.Name) || fn.Body == nil {
				continue
			}
			hasParallel, hasEnv := false, false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if isCallTo(call, "t", "Parallel") {
					hasParallel = true
				}
				if isCallTo(call, "t", "Setenv") || isCallTo(call, "os", "Setenv") || isCallTo(call, "t", "Chdir") || isCallTo(call, "os", "Chdir") {
					hasEnv = true
				}
				return true
			})
			if hasEnv {
				envBound[fn.Name.Name] = true
			}
			if hasEnv || !hasParallel {
				serial[fn.Name.Name] = true
			}
		}
	}
	return envBound, serial, nil
}

// isCallTo reports whether call is pkg.name(...) with a plain identifier
// selector (t.Parallel, os.Setenv).
func isCallTo(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// buildPlan partitions names into n shards by longest-processing-time first:
// sort by cost descending, then place each test on the least-loaded shard. The
// result is deterministic for a given name/cost input.
func buildPlan(pkg string, n int, names []string, costs map[string]int64, envBound, serial map[string]bool) Plan {
	costsByShard := make([]int64, n)
	shards := make([]Shard, n)
	for i := range shards {
		shards[i].Index = i
	}
	ordered := append([]string(nil), names...)
	sort.Slice(ordered, func(i, j int) bool {
		if costs[ordered[i]] != costs[ordered[j]] {
			return costs[ordered[i]] > costs[ordered[j]]
		}
		return ordered[i] < ordered[j]
	})
	for _, name := range ordered {
		target := 0
		for i := 1; i < n; i++ {
			switch {
			case costsByShard[i] < costsByShard[target]:
				target = i
			case costsByShard[i] == costsByShard[target] && len(shards[i].Tests) < len(shards[target].Tests):
				// Break zero-cost ties by count so every shard receives work.
				target = i
			}
		}
		cost := costs[name]
		shards[target].Tests = append(shards[target].Tests, TestCost{Name: name, WallMS: cost})
		shards[target].WallMS += cost
		costsByShard[target] += cost
		if envBound[name] {
			shards[target].EnvBound++
		}
		if serial[name] {
			shards[target].Serial++
		}
	}
	for i := range shards {
		sort.Slice(shards[i].Tests, func(a, b int) bool { return shards[i].Tests[a].Name < shards[i].Tests[b].Name })
		shards[i].RunRegex = runRegex(shards[i].Tests)
	}
	return Plan{Package: pkg, Shards: n, Total: len(names), Covered: len(names), ShardSet: shards}
}

// runRegex builds an anchored alternation over the shard's test names.
func runRegex(tests []TestCost) string {
	names := make([]string, 0, len(tests))
	for _, test := range tests {
		names = append(names, regexp.QuoteMeta(test.Name))
	}
	return "^(" + strings.Join(names, "|") + ")$"
}

// auditPlan proves the shard sets are an exact partition of the package's tests:
// every test appears in exactly one shard, no shard shares a name, and every
// shard is non-empty.
func auditPlan(plan Plan) error {
	seen := map[string]int{}
	total := 0
	for _, shard := range plan.ShardSet {
		if len(shard.Tests) == 0 {
			return fmt.Errorf("shard %d is empty", shard.Index)
		}
		for _, test := range shard.Tests {
			seen[test.Name]++
			total++
		}
	}
	if total != plan.Total {
		return fmt.Errorf("shards cover %d tests, want %d", total, plan.Total)
	}
	for name, count := range seen {
		if count != 1 {
			return fmt.Errorf("test %s appears in %d shards, want exactly 1", name, count)
		}
	}
	if len(seen) != plan.Total {
		return fmt.Errorf("shards name %d distinct tests, want %d", len(seen), plan.Total)
	}
	return nil
}

// printPlan renders the plan and the audit summary.
func printPlan(plan Plan) {
	fmt.Printf("testshard: %s -> %d shards over %d top-level tests (audit: exact partition)\n", plan.Package, plan.Shards, plan.Total)
	for _, shard := range plan.ShardSet {
		fmt.Printf("  shard %d: %d tests, %d ms measured, %d env-bound, %d serial\n",
			shard.Index, len(shard.Tests), shard.WallMS, shard.EnvBound, shard.Serial)
		fmt.Printf("    -run '%s'\n", shard.RunRegex)
	}
}

func writePlan(path string, plan Plan) error {
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write plan %s: %w", path, err)
	}
	return nil
}

// runShards executes every shard with the race detector and the requested
// repetition count, sequentially, and reports the wall of each. A shard that
// fails its build or any test fails the whole run.
func runShards(goBin, pkg string, plan Plan, count int, race bool) int {
	code := 0
	for _, shard := range plan.ShardSet {
		args := []string{"test", "-count", fmt.Sprint(count), "-run", shard.RunRegex}
		if race {
			args = append(args, "-race")
		}
		args = append(args, pkg)
		start := time.Now()
		cmd := exec.CommandContext(context.Background(), goBin, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		wall := time.Since(start)
		if err != nil {
			fmt.Printf("testshard: shard %d FAILED in %s: %v\n", shard.Index, wall.Round(time.Millisecond), err)
			code = 1
			continue
		}
		fmt.Printf("testshard: shard %d passed in %s (%d tests, count=%d, race=%t)\n",
			shard.Index, wall.Round(time.Millisecond), len(shard.Tests), count, race)
	}
	if code == 0 {
		fmt.Printf("testshard: all %d shards green under -count=%d (race=%t)\n", plan.Shards, count, race)
	}
	return code
}

func gomaxprocs() int {
	return runtime.GOMAXPROCS(0)
}
