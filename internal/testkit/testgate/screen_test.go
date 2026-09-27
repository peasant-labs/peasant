package testgate

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/teststream"
)

func screenPlan() *Plan {
	return &Plan{Packages: []PackagePlan{
		{ImportPath: "example.com/m/registered", Dir: "registered", Tests: []string{"TestRegistered"}, RaceTests: []string{"TestRegistered"}, Registered: true, NoRaceClasses: map[string]Class{"TestRegistered": ClassSingleThreadedBytes}},
		{ImportPath: "example.com/m/unregistered", Dir: "unregistered", Tests: []string{"TestUnregistered"}, RaceTests: []string{"TestUnregistered"}},
	}}
}

func screenRegistry() Registry {
	return Registry{Version: 1, Partition: []Entry{
		{Package: "registered", Test: "TestRegistered", Class: ClassSingleThreadedBytes},
	}}
}

func ruleNames(findings []Finding) map[string]Severity {
	out := map[string]Severity{}
	for _, f := range findings {
		out[f.Rule] = f.Severity
	}
	return out
}

// A registered package that produced no test events must FAIL; an unregistered
// one that produced none must be reported only.
func TestScreen_RegisteredEmptyFailsUnregisteredReported(t *testing.T) {
	findings := Screen(ScreenInput{
		Plan:     screenPlan(),
		Registry: screenRegistry(),
		Race:     true,
		Streams: map[PassMode]map[string][]teststream.Record{
			ModeRace: {
				"example.com/m/registered":   {},
				"example.com/m/unregistered": {},
			},
			ModeNoRace: {},
		},
	})
	rules := ruleNames(findings)
	if rules["registered-liveness"] != SeverityFail {
		t.Fatalf("a registered package with no events must FAIL; findings: %v", findings)
	}
	if rules["unregistered-liveness"] != SeverityReport {
		t.Fatalf("an unregistered package with no events must be REPORTED; findings: %v", findings)
	}
	if !Fails(findings) {
		t.Fatal("the registered empty package must fail the screen")
	}
}

// A registered partition member that ran in the race pass must FAIL.
func TestScreen_PartitionMemberInRacePassFails(t *testing.T) {
	findings := Screen(ScreenInput{
		Plan:     screenPlan(),
		Registry: screenRegistry(),
		Race:     true,
		Streams: map[PassMode]map[string][]teststream.Record{
			ModeRace: {
				"example.com/m/registered": {{Package: "example.com/m/registered", Test: "TestRegistered"}},
			},
			ModeNoRace: {
				"example.com/m/registered": {{Package: "example.com/m/registered", Test: "TestRegistered"}},
			},
		},
	})
	rules := ruleNames(findings)
	if rules["partition-containment"] != SeverityFail {
		t.Fatalf("a partition member in the race pass must FAIL; findings: %v", findings)
	}
	if rules["exactly-once"] != SeverityFail {
		t.Fatalf("the same test in both passes is also a double-run; findings: %v", findings)
	}
}

// A test that ran in both passes must FAIL as a double-run.
func TestScreen_DoubleRunFails(t *testing.T) {
	findings := Screen(ScreenInput{
		Plan:     screenPlan(),
		Registry: screenRegistry(),
		Race:     true,
		Streams: map[PassMode]map[string][]teststream.Record{
			ModeRace: {
				"example.com/m/unregistered": {{Package: "example.com/m/unregistered", Test: "TestUnregistered"}},
			},
			ModeNoRace: {
				"example.com/m/unregistered": {{Package: "example.com/m/unregistered", Test: "TestUnregistered"}},
			},
		},
	})
	if ruleNames(findings)["exactly-once"] != SeverityFail {
		t.Fatalf("a test that ran twice must FAIL; findings: %v", findings)
	}
}

// A partition member that never ran is a dropped test, not a moved one.
func TestScreen_DroppedPartitionMemberFails(t *testing.T) {
	findings := Screen(ScreenInput{
		Plan:     screenPlan(),
		Registry: screenRegistry(),
		Race:     true,
		Streams: map[PassMode]map[string][]teststream.Record{
			ModeRace: {
				"example.com/m/unregistered": {{Package: "example.com/m/unregistered", Test: "TestUnregistered"}},
			},
			ModeNoRace: {},
		},
	})
	if ruleNames(findings)["registered-liveness"] != SeverityFail {
		t.Fatalf("a partition member that did not run must FAIL; findings: %v", findings)
	}
}

// A missing registered test is a registry defect.
func TestScreen_MissingRegisteredFails(t *testing.T) {
	plan := screenPlan()
	plan.MissingRegistered = []string{"registered/TestRegistered"}
	findings := Screen(ScreenInput{
		Plan:     plan,
		Registry: screenRegistry(),
		Race:     true,
		Streams: map[PassMode]map[string][]teststream.Record{
			ModeRace:   {"example.com/m/registered": {{Test: "TestRegistered"}}},
			ModeNoRace: {"example.com/m/registered": {{Test: "TestRegistered"}}},
		},
	})
	if ruleNames(findings)["registered-liveness"] != SeverityFail {
		t.Fatalf("a registered test missing from go test -list must FAIL; findings: %v", findings)
	}
}

// The screen records no baseline: the same streams produce the same verdict on
// every call.
func TestScreen_NoBaseline(t *testing.T) {
	in := ScreenInput{
		Plan:     screenPlan(),
		Registry: screenRegistry(),
		Race:     false,
		Streams: map[PassMode]map[string][]teststream.Record{
			ModeNoRace: {
				"example.com/m/registered":   {{Test: "TestRegistered"}},
				"example.com/m/unregistered": {{Test: "TestUnregistered"}},
			},
		},
	}
	first := Screen(in)
	second := Screen(in)
	if len(first) != len(second) {
		t.Fatalf("screen is not deterministic: %d vs %d findings", len(first), len(second))
	}
}
