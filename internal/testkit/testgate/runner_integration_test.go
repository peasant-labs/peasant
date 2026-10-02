package testgate

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/teststream"
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
			ImportPath: "github.com/peasant-labs/peasant/cmd/testgate",
			Dir:        "cmd/testgate",
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

// TestRunner_RACE0OverlapsAndAttributesPassLevel runs the gate command on two
// cheap packages. The child tests rendezvous, so a serialized RACE=0 pass
// fails, and the printed class table must call that concurrent CPU pass-level.
func TestRunner_RACE0OverlapsAndAttributesPassLevel(t *testing.T) {
	var overlap []string
	var wantBasis string
	for _, tc := range loadAttributionModes(t).Cases {
		if len(tc.OverlapPackages) > 0 {
			overlap = tc.OverlapPackages
			wantBasis = tc.WantNoRaceBasis
		}
	}
	if len(overlap) != 2 || wantBasis == "" {
		t.Fatal("attribution fixture has no two-package RACE=0 overlap case")
	}
	root := testRepoRoot(t)
	dir := t.TempDir()
	t.Setenv("PEASANT_TESTGATE_OVERLAP_DIR", dir)
	cmd := exec.Command("go", "run", "./cmd/testgate", "run",
		"-race=false",
		"-p=2",
		"-pkgs", strings.Join(overlap, ","),
		"-out", filepath.Join(t.TempDir(), "out"),
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("RACE=0 overlap run: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "race:                   off (RACE=0") || !strings.Contains(text, wantBasis) {
		t.Fatalf("RACE=0 output missing %q:\n%s", wantBasis, text)
	}
	if strings.Contains(text, BasisSerialized) {
		t.Fatalf("RACE=0 labeled overlapping CPU as %s:\n%s", BasisSerialized, text)
	}
}
