package testgate

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/teststream"
)

// TestRunner_TwoPassRecordsAndScreen exercises the real runner path on two
// cheap packages: it invokes `go test` for a race pass and a no-race partition
// pass, parses the streams, builds per-invocation records, and screens the
// merge. It proves the invocation/record/screen wiring without running the
// whole suite.
func TestRunner_TwoPassRecordsAndScreen(t *testing.T) {
	root := testRepoRoot(t)
	plan := &Plan{Packages: []PackagePlan{
		{
			ImportPath: "github.com/peasant-labs/peasant/scripts/testgate",
			Dir:        "scripts/testgate",
			Tests:      []string{"TestSummarizer_ExcludesSubtestsFromTotal", "TestSummarizer_RejectsEmptyInput"},
			RaceTests:  []string{"TestSummarizer_ExcludesSubtestsFromTotal", "TestSummarizer_RejectsEmptyInput"},
		},
		{
			ImportPath:    "github.com/peasant-labs/peasant/internal/moduleboundary",
			Dir:           "internal/moduleboundary",
			Tests:         []string{"TestRedactionBoundaryFixtureStrictDecoding"},
			NoRaceTests:   []string{"TestRedactionBoundaryFixtureStrictDecoding"},
			NoRaceClasses: map[string]Class{"TestRedactionBoundaryFixtureStrictDecoding": ClassSingleThreadedBytes},
			Registered:    true,
		},
	}}
	reg := Registry{Version: 1, Partition: []Entry{{Package: "internal/moduleboundary", Test: "TestRedactionBoundaryFixtureStrictDecoding", Class: ClassSingleThreadedBytes}}}
	runner := &Runner{Root: root, OutDir: t.TempDir(), GoBin: "go", Concurrency: 2, SerialPassB: true}
	ctx := context.Background()
	resA, err := runner.Run(ctx, plan, ModeRace, true)
	if err != nil {
		t.Fatalf("race pass: %v", err)
	}
	resB, err := runner.Run(ctx, plan, ModeNoRace, true)
	if err != nil {
		t.Fatalf("no-race pass: %v", err)
	}
	streams := map[PassMode]map[string][]teststream.Record{
		ModeRace:   resA.Streams[ModeRace],
		ModeNoRace: resB.Streams[ModeNoRace],
	}
	findings := Screen(ScreenInput{Plan: plan, Registry: reg, Race: true, Streams: streams})
	if Fails(findings) {
		for _, f := range findings {
			t.Log(f.Render())
		}
		t.Fatal("synthetic two-pass run must pass the screen")
	}
	if len(resA.Records[ModeRace]) != 1 || len(resB.Records[ModeNoRace]) != 1 {
		t.Fatalf("expected one record per pass, got race=%d no-race=%d", len(resA.Records[ModeRace]), len(resB.Records[ModeNoRace]))
	}
	t.Logf("race record: %+v", resA.Records[ModeRace][0])
	t.Logf("no-race record: %+v", resB.Records[ModeNoRace][0])
}
