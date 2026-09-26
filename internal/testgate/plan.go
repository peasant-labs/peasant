package testgate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// PackagePlan is one package's run plan: the tests that run in each pass.
type PackagePlan struct {
	ImportPath    string
	Dir           string // repo-relative
	Tests         []string
	RaceTests     []string
	NoRaceTests   []string
	NoRaceClasses map[string]Class // partition test name -> class
	Registered    bool
}

// Plan is the computed run plan. ListWall records the cost of the
// `go test -list` step that produced it.
type Plan struct {
	ModulePath string
	Packages   []PackagePlan
	ListWall   time.Duration
	// MissingRegistered names registered tests that the package no longer
	// lists (renamed or deleted).
	MissingRegistered []string
	// UnregisteredPackages names packages with no registry entry.
	UnregisteredPackages []string
}

// testNameRE matches a top-level test, benchmark, example, or fuzz name.
var testNameRE = regexp.MustCompile(`^(Test|Benchmark|Example|Fuzz)[A-Za-z0-9_]*$`)

// ModulePath reads the module path from the root go.mod.
func ModulePath(root string) (string, error) {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("open go.mod: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no module directive in %s", filepath.Join(root, "go.mod"))
}

// ListPackages runs `go list ./...` from root and returns the import paths.
func ListPackages(root string) ([]string, error) {
	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w", err)
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			pkgs = append(pkgs, line)
		}
	}
	return pkgs, nil
}

// ListTests runs `go test -list '.*' -json ./...` from root and returns the
// top-level test/benchmark/example/fuzz names per package, plus the wall cost.
//
// A package that fails to build contributes no names; the gate's screen treats
// a registered package with no events as a failure, so a build failure is not
// swallowed.
func ListTests(root string) (map[string][]string, time.Duration, error) {
	start := time.Now()
	cmd := exec.Command("go", "test", "-list", ".*", "-json", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	elapsed := time.Since(start)
	if err != nil {
		// `go test -list` exits non-zero when any package fails to build; the
		// stream still names the packages that did list, so parse what we have.
		if len(out) == 0 {
			return nil, elapsed, fmt.Errorf("go test -list: %w", err)
		}
	}
	byPkg := map[string]map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for sc.Scan() {
		var e struct {
			Action  string
			Package string
			Output  string
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Action != "output" || e.Package == "" {
			continue
		}
		for _, line := range strings.Split(e.Output, "\n") {
			name := strings.TrimSpace(line)
			if testNameRE.MatchString(name) {
				if byPkg[e.Package] == nil {
					byPkg[e.Package] = map[string]bool{}
				}
				byPkg[e.Package][name] = true
			}
		}
	}
	result := map[string][]string{}
	for pkg, set := range byPkg {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		sort.Strings(names)
		result[pkg] = names
	}
	return result, elapsed, nil
}

// partitionIndex maps a repo-relative package directory to its partition test
// names and each test's class.
type partitionIndex struct {
	tests map[string]map[string]bool
	class map[string]map[string]Class
}

func indexPartition(reg Registry) partitionIndex {
	idx := partitionIndex{tests: map[string]map[string]bool{}, class: map[string]map[string]Class{}}
	for _, e := range reg.Partition {
		if idx.tests[e.Package] == nil {
			idx.tests[e.Package] = map[string]bool{}
			idx.class[e.Package] = map[string]Class{}
		}
		idx.tests[e.Package][e.Test] = true
		idx.class[e.Package][e.Test] = e.Class
	}
	return idx
}

// BuildPlan computes the run plan from the listed tests and the registry.
//
// The race pass runs every listed test that is not a partition member; the
// no-race pass runs exactly the partition members. A registered test that is
// not listed is recorded in MissingRegistered rather than silently dropped.
func BuildPlan(root string, modulePath string, tests map[string][]string, reg Registry) (*Plan, error) {
	idx := indexPartition(reg)
	registeredDirs := map[string]bool{}
	for _, e := range reg.Partition {
		registeredDirs[e.Package] = true
	}
	for _, e := range reg.Protected {
		registeredDirs[e.Package] = true
	}

	plan := &Plan{ModulePath: modulePath}
	listed := map[string]bool{}
	for _, importPath := range sortedKeys(tests) {
		dir := packageDir(modulePath, importPath)
		names := tests[importPath]
		for _, n := range names {
			listed[dir+"|"+n] = true
		}
		p := PackagePlan{
			ImportPath: importPath,
			Dir:        dir,
			Tests:      names,
			Registered: registeredDirs[dir],
		}
		if !p.Registered {
			plan.UnregisteredPackages = append(plan.UnregisteredPackages, importPath)
		}
		part := idx.tests[dir]
		p.NoRaceClasses = idx.class[dir]
		for _, n := range names {
			if part[n] {
				p.NoRaceTests = append(p.NoRaceTests, n)
			} else {
				p.RaceTests = append(p.RaceTests, n)
			}
		}
		plan.Packages = append(plan.Packages, p)
	}

	for _, e := range append(append([]Entry{}, reg.Partition...), reg.Protected...) {
		if !listed[e.Package+"|"+e.Test] {
			plan.MissingRegistered = append(plan.MissingRegistered, e.Package+"/"+e.Test)
		}
	}
	sort.Strings(plan.MissingRegistered)
	return plan, nil
}

// packageDir converts an import path to its repo-relative directory.
func packageDir(modulePath, importPath string) string {
	if importPath == modulePath {
		return "."
	}
	return strings.TrimPrefix(strings.TrimPrefix(importPath, modulePath), "/")
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RunRegex builds an anchored alternation that matches exactly the named tests,
// or "" when names is empty.
func RunRegex(names []string) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, regexp.QuoteMeta(n))
	}
	return "^(" + strings.Join(parts, "|") + ")$"
}
