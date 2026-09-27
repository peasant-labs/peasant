package testgate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/teststream"
)

// Severity separates a screen finding that fails the gate from one that is
// only reported.
type Severity int

const (
	// SeverityReport is informational; the gate does not fail.
	SeverityReport Severity = iota
	// SeverityFail fails the gate.
	SeverityFail
)

func (s Severity) String() string {
	if s == SeverityFail {
		return "FAIL"
	}
	return "REPORT"
}

// Finding is one screen result with actionable detail.
type Finding struct {
	Rule     string
	Severity Severity
	What     string
	Why      string
	Where    string
	When     string
	Means    string
	Fix      string
}

// Render renders the finding in the what/why/where/when/means/fix shape.
func (f Finding) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s: %s\n", f.Severity, f.Rule, f.What)
	fmt.Fprintf(&b, "  why:   %s\n", f.Why)
	fmt.Fprintf(&b, "  where: %s\n", f.Where)
	fmt.Fprintf(&b, "  when:  %s\n", f.When)
	fmt.Fprintf(&b, "  means: %s\n", f.Means)
	fmt.Fprintf(&b, "  fix:   %s\n", f.Fix)
	return b.String()
}

// ScreenInput is the merged evidence the screen validates.
type ScreenInput struct {
	Plan     *Plan
	Registry Registry
	// Race reports whether the race pass ran (RACE=1). RACE=0 is a single
	// no-race pass.
	Race bool
	// Streams is pass -> package import path -> records.
	Streams map[PassMode]map[string][]teststream.Record
}

// Screen applies the four exactly-once rules over the merged passes.
//
//  1. exactly-once: a test that ran in more than one pass is a double-run.
//  2. partition containment: a partition member must not run in the race pass.
//  3. registered liveness: a registered package with no events, or a registered
//     member with no run, fails.
//  4. unregistered liveness: an unregistered planned package with no events is
//     reported, not failed.
//
// It records no baseline and compares nothing across time.
func Screen(in ScreenInput) []Finding {
	var findings []Finding

	passes := []PassMode{ModeNoRace}
	if in.Race {
		passes = []PassMode{ModeRace, ModeNoRace}
	}

	// Registry packages are repo-relative directories; streams are keyed by
	// import path. Normalize every registry lookup through the plan.
	importOf := map[string]string{}
	for _, p := range in.Plan.Packages {
		importOf[p.Dir] = p.ImportPath
	}
	pkgKey := func(dir string) string {
		if ip, ok := importOf[dir]; ok {
			return ip
		}
		return dir
	}

	// ranCount[pkg][test] counts non-skip terminal events across all passes.
	ranCount := map[string]map[string]int{}
	// passRan[pass][pkg][test] records a real run in that pass.
	passRan := map[PassMode]map[string]map[string]bool{}
	// eventCount[pkg] counts every terminal event (including skips).
	eventCount := map[string]int{}
	for _, pass := range passes {
		passRan[pass] = map[string]map[string]bool{}
		for pkg, records := range in.Streams[pass] {
			if eventCount[pkg] == 0 {
				eventCount[pkg] = 0
			}
			if passRan[pass][pkg] == nil {
				passRan[pass][pkg] = map[string]bool{}
			}
			for _, rec := range records {
				eventCount[pkg]++
				if rec.Skipped {
					continue
				}
				if ranCount[pkg] == nil {
					ranCount[pkg] = map[string]int{}
				}
				ranCount[pkg][rec.Test]++
				passRan[pass][pkg][rec.Test] = true
			}
		}
	}

	var partitionEntries, protectedEntries []Entry
	partitionEntries = append(partitionEntries, in.Registry.Partition...)
	protectedEntries = append(protectedEntries, in.Registry.Protected...)

	// Rule 1 — exactly-once.
	for _, pkg := range sortedKeys(ranCount) {
		for _, test := range sortedKeys(ranCount[pkg]) {
			if ranCount[pkg][test] > 1 {
				findings = append(findings, Finding{
					Rule:     "exactly-once",
					Severity: SeverityFail,
					What:     fmt.Sprintf("%s/%s ran %d times across the gate's passes", pkg, test, ranCount[pkg][test]),
					Why:      "each test must run exactly once across the race and no-race passes",
					Where:    fmt.Sprintf("package %s, test %s", pkg, test),
					When:     "merging the passes",
					Means:    "a partition or a -run overlap is scheduling the same test in both passes",
					Fix:      fmt.Sprintf("remove %s/%s from no-race-partition.yaml or narrow the -run selection so it runs in exactly one pass", pkg, test),
				})
			}
		}
	}

	// Rule 2 — partition containment (RACE=1 only).
	if in.Race {
		for _, e := range partitionEntries {
			if passRan[ModeRace][pkgKey(e.Package)][e.Test] {
				findings = append(findings, Finding{
					Rule:     "partition-containment",
					Severity: SeverityFail,
					What:     fmt.Sprintf("partition member %s/%s ran in the race pass", e.Package, e.Test),
					Why:      "a no-race partition member must be excluded from the race pass; running it there defeats the partition",
					Where:    fmt.Sprintf("partition entry %s/%s (class %s)", e.Package, e.Test, e.Class),
					When:     "screening the race pass",
					Means:    "the race pass's -run selection is not excluding the partition member",
					Fix:      "re-run `cmd/testgate plan` and confirm the package's race set excludes the partition member",
				})
			}
		}
	}

	// Rule 3 — registered liveness.
	for _, dir := range registeredPackages(in.Registry) {
		if eventCount[pkgKey(dir)] == 0 {
			findings = append(findings, Finding{
				Rule:     "registered-liveness",
				Severity: SeverityFail,
				What:     fmt.Sprintf("registered package %s produced no test events", dir),
				Why:      "a registered package must run; a missing package means it was misregistered or it crashed before emitting events",
				Where:    fmt.Sprintf("registry package %s", dir),
				When:     "screening the merged passes",
				Means:    "either the package path no longer resolves (misregistration) or the package failed to build or crashed",
				Fix:      fmt.Sprintf("confirm %s builds (`go test -list ./%s`) and that its registry package path is correct; inspect the pass stderr", dir, dir),
			})
		}
	}
	for _, e := range partitionEntries {
		if ranCount[pkgKey(e.Package)][e.Test] == 0 {
			findings = append(findings, Finding{
				Rule:     "registered-liveness",
				Severity: SeverityFail,
				What:     fmt.Sprintf("partition member %s/%s did not run", e.Package, e.Test),
				Why:      "a partition member is moved out of the race pass, not dropped; it must run in the no-race pass",
				Where:    fmt.Sprintf("partition entry %s/%s", e.Package, e.Test),
				When:     "screening the no-race pass",
				Means:    "the no-race pass did not execute the registered test",
				Fix:      fmt.Sprintf("run `cmd/testgate plan` and confirm %s lists %s, then check the no-race pass stderr", e.Package, e.Test),
			})
		}
	}
	for _, e := range protectedEntries {
		ranInPass := false
		for _, pass := range passes {
			if passRan[pass][pkgKey(e.Package)][e.Test] {
				ranInPass = true
			}
		}
		if !ranInPass {
			findings = append(findings, Finding{
				Rule:     "registered-liveness",
				Severity: SeverityFail,
				What:     fmt.Sprintf("protected test %s/%s did not run", e.Package, e.Test),
				Why:      "a protected test is pinned into the gate's pass and must keep running there",
				Where:    fmt.Sprintf("protected entry %s/%s", e.Package, e.Test),
				When:     "screening the merged passes",
				Means:    "the test was renamed, filtered out, or its package crashed",
				Fix:      fmt.Sprintf("confirm %s is listed in %s and re-run the gate", e.Test, e.Package),
			})
		}
	}
	for _, missing := range in.Plan.MissingRegistered {
		findings = append(findings, Finding{
			Rule:     "registered-liveness",
			Severity: SeverityFail,
			What:     fmt.Sprintf("registered test %s is not listed by go test -list", missing),
			Why:      "the registry names a test the package no longer declares",
			Where:    "no-race-partition.yaml",
			When:     "computing the run plan",
			Means:    "the test was renamed or deleted without updating the registry",
			Fix:      "update or remove the registry entry so it names a listed test",
		})
	}

	// Rule 4 — unregistered packages with no events are reported only.
	for _, p := range in.Plan.Packages {
		if p.Registered || len(p.Tests) == 0 {
			continue
		}
		if eventCount[p.ImportPath] == 0 {
			findings = append(findings, Finding{
				Rule:     "unregistered-liveness",
				Severity: SeverityReport,
				What:     fmt.Sprintf("unregistered package %s produced no test events", p.ImportPath),
				Why:      "the package was planned to run tests but emitted none; it is not registered, so this is reported, not failed",
				Where:    fmt.Sprintf("package %s", p.ImportPath),
				When:     "screening the merged passes",
				Means:    "a build or crash removed the events, or the -run filter excluded every test",
				Fix:      "investigate the pass stderr; register the package if it should be part of the partition",
			})
		}
	}

	return findings
}

// Fail reports whether any finding fails the gate.
func Fails(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityFail {
			return true
		}
	}
	return false
}

func registeredPackages(reg Registry) []string {
	set := map[string]bool{}
	for _, e := range reg.Partition {
		set[e.Package] = true
	}
	for _, e := range reg.Protected {
		set[e.Package] = true
	}
	out := make([]string, 0, len(set))
	for pkg := range set {
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}
