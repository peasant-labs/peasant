package ingest_test

// This file covers the two upkeep guards that a parity table cannot see: the
// evidence cache must be read by path only (no whole-table load) on a scoped
// run, and the same scoped run must stay independent of how many stored
// sessions the database holds.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

type scopeUpkeepFixture struct {
	RequiredNames []string             `yaml:"requiredNames"`
	Refusals      []scopeUpkeepRefusal `yaml:"refusals"`
	Cases         []scopeUpkeepCase    `yaml:"cases"`
}

type scopeUpkeepRefusal struct {
	Name             string   `yaml:"name"`
	Guard            string   `yaml:"guard"`
	Expected         []string `yaml:"expected"`
	StalePaths       []string `yaml:"stalePaths"`
	ContextPaths     []string `yaml:"contextPaths"`
	WantErrorContain string   `yaml:"wantErrorContain"`
}

type scopeUpkeepCase struct {
	Name string `yaml:"name"`
	// Guard is read_recorder. A decoy guard is refused by the loader; the
	// decoy rows are not implemented in this fixture.
	Guard string `yaml:"guard"`
	// StaleOrigin plants an origin version the resolver judges stale on the
	// selected stored-but-unreadable root.
	StaleOrigin bool `yaml:"staleOrigin"`
	// DeleteSource removes the selected transcript before the scoped run.
	DeleteSource bool `yaml:"deleteSource"`
	// StalePaths is the path set the planted stale origin names; it must not
	// overlap ContextPaths, or the stale-origin read could pass for the wrong
	// reason.
	StalePaths   []string `yaml:"stalePaths"`
	ContextPaths []string `yaml:"contextPaths"`
	// WantRequested is the exact sorted union the evidence recorder must see.
	WantRequested []string `yaml:"wantRequested"`
	// WantWholeLoads is the number of whole-table LoadClaudeEvidence calls the
	// scoped run may make (zero, always, for a read_recorder case).
	WantWholeLoads int `yaml:"wantWholeLoads"`
}

func loadScopeUpkeepFixture(t *testing.T) scopeUpkeepFixture {
	t.Helper()
	var fixture scopeUpkeepFixture
	if err := yaml.Unmarshal(sessionScopeUpkeepYAML, &fixture); err != nil {
		t.Fatalf("decode the session-scope upkeep fixture: %v", err)
	}
	present := make(map[string]bool)
	for _, testCase := range fixture.Cases {
		present[testCase.Name] = true
		if testCase.Guard != "read_recorder" {
			t.Fatalf("upkeep case %q names guard %q; only read_recorder cases exist here, so an unimplemented decoy guard is refused rather than silently ignored", testCase.Name, testCase.Guard)
		}
		if err := validateReadRecorderCase(testCase.Name, testCase.WantRequested, testCase.StalePaths, testCase.ContextPaths); err != nil {
			t.Fatal(err)
		}
	}
	for _, refusal := range fixture.Refusals {
		present[refusal.Name] = true
	}
	if err := testutil.RequireFixtureNames("session-scope upkeep fixture", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestSessionScopeReadRecorder pins the evidence read contract on B only:
// zero whole-table loads, and a requested path set inside the cohort/context
// union. The full run on A legitimately loads the whole table, so A is never
// recorded here.
func TestSessionScopeReadRecorder(t *testing.T) {
	t.Parallel()
	fixture := loadScopeUpkeepFixture(t)
	const selected = "11111111-1111-4111-8111-111111111111"
	const source = "project-a/11111111-1111-4111-8111-111111111111.jsonl"

	for _, testCase := range fixture.Cases {
		testCase := testCase
		if testCase.Guard != "read_recorder" {
			continue
		}
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			files := []scopeFile{{
				Path: source,
				Lines: []string{
					`{"sessionId":"` + selected + `","type":"user","cwd":"/work/project","message":{"role":"user","content":"drive"}}`,
				},
			}}
			seedPath := newScopeSeedPath(t)
			seed := openScopeWorld(t, "/claude", files, nil, scopeMutation{}, seedPath, false)
			runScopePhase(t, ctx, seed, nil)
			runScopePhase(t, ctx, seed, nil)
			if testCase.StaleOrigin {
				plantStaleScopeOrigin(t, ctx, seed, selected)
			}
			seed.close(t)

			mutation := scopeMutation{}
			if testCase.DeleteSource {
				mutation = scopeMutation{Action: "delete", Path: source}
			}
			bPath := filepath.Join(t.TempDir(), "b.db")
			storetest.ForkSeeded(t, seedPath, bPath)
			world := openScopeWorld(t, "/claude", files, nil, mutation, bPath, true)
			defer world.close(t)

			scope, err := ingest.NewSessionScope([]ingest.SessionID{selected}, ingest.RelatedInclude)
			if err != nil {
				t.Fatalf("NewSessionScope: %v", err)
			}
			result := runScopePhase(t, ctx, world, &scope)
			if result.ScopeReport == nil {
				t.Fatal("scoped run returned no ScopeReport")
			}

			wholeLoad, requested := world.pipe.evidenceSnapshot()
			if wholeLoad != testCase.WantWholeLoads {
				t.Fatalf("whole-table LoadClaudeEvidence calls = %d, want %d", wholeLoad, testCase.WantWholeLoads)
			}
			if len(testCase.WantRequested) != 0 {
				sorted := append([]string(nil), requested...)
				sort.Strings(sorted)
				want := make([]string, 0, len(testCase.WantRequested))
				for _, path := range testCase.WantRequested {
					want = append(want, filepath.Join("/claude", path))
				}
				sort.Strings(want)
				if strings.Join(sorted, "\x00") != strings.Join(want, "\x00") {
					t.Fatalf("evidence recorder requested %v, want exactly %v", sorted, want)
				}
			}
			// Every requested path is a source this run may read: the selected
			// stored path or a context path. A path outside that union is the
			// widening the guard exists to catch.
			allowed := map[string]bool{"/claude/" + source: true}
			for _, path := range requested {
				if !allowed[path] {
					t.Fatalf("evidence recorder requested %q, which is outside the cohort/context union", path)
				}
			}
		})
	}
}

// plantStaleScopeOrigin writes an origin verdict older than the current rule
// version, so the scoped run's origin resolver treats the row as stale and must
// read its evidence by path.
func plantStaleScopeOrigin(t *testing.T, ctx context.Context, world *scopeWorld, rawID string) {
	t.Helper()
	id, err := ingest.NewSessionID(rawID)
	if err != nil {
		t.Fatalf("stale-origin session id: %v", err)
	}
	if err := world.raw.UpdateOriginState(ctx, id, "unknown", 0); err != nil {
		t.Fatalf("plant the stale origin: %v", err)
	}
}

// TestSessionScopeSizeIndependence runs the same scoped harvest against a
// 3-session and a 200-session store and requires the op multiset, the WAL
// commit-frame count, the evidence golden and the report to match. A guard that
// grows with the stored population fails here even though both runs are
// internally consistent.
func TestSessionScopeSizeIndependence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const selected = "11111111-1111-4111-8111-111111111111"
	const source = "project-a/11111111-1111-4111-8111-111111111111.jsonl"
	files := []scopeFile{{
		Path: source,
		Lines: []string{
			`{"sessionId":"` + selected + `","type":"user","cwd":"/work/project","message":{"role":"user","content":"drive"}}`,
		},
	}}
	const small, large = 3, 200

	type observation struct {
		ops      map[string]int
		wal      int
		evidence []string
		report   string
	}
	run := func(background int) observation {
		seedPath := newScopeSeedPath(t)
		seed := openScopeWorld(t, "/claude", files, nil, scopeMutation{}, seedPath, false)
		seedScopeStore(t, ctx, seed.raw, background)
		runScopePhase(t, ctx, seed, nil)
		runScopePhase(t, ctx, seed, nil)
		seed.close(t)

		bPath := filepath.Join(t.TempDir(), "b.db")
		storetest.ForkSeeded(t, seedPath, bPath)
		world := openScopeWorld(t, "/claude", files, nil, scopeMutation{}, bPath, true)
		defer world.close(t)

		beforeWAL := countScopeWAL(t, world.path)
		scope, err := ingest.NewSessionScope([]ingest.SessionID{selected}, ingest.RelatedInclude)
		if err != nil {
			t.Fatalf("NewSessionScope: %v", err)
		}
		result := runScopePhase(t, ctx, world, &scope)
		afterWAL := countScopeWAL(t, world.path)

		// The source-root op multiset, not the whole recorder: output-file
		// writes carry temp names that are not a property of the source work.
		ops := make(map[string]int)
		for op, paths := range world.fs.Counts() {
			for path, count := range paths {
				if path == "/claude" || strings.HasPrefix(path, "/claude/") {
					key := string(op) + "\x00" + strings.TrimPrefix(path, "/claude/")
					ops[key] += count
				}
			}
		}
		_, requested := world.pipe.evidenceSnapshot()
		sort.Strings(requested)
		processed := idsToStrings(result.ScopeReport.Processed())
		upkeep := idsToStrings(result.ScopeReport.Upkeep())
		return observation{
			ops:      ops,
			wal:      afterWAL - beforeWAL,
			evidence: requested,
			report:   fmt.Sprintf("processed=%v upkeep=%v", processed, upkeep),
		}
	}

	smallRun := run(small)
	largeRun := run(large)
	if smallRun.wal != largeRun.wal {
		t.Fatalf("WAL commit frames: %d-session store = %d, %d-session store = %d; the scoped harvest must commit the same transactions either way", small, smallRun.wal, large, largeRun.wal)
	}
	if smallRun.wal == 0 {
		t.Fatal("the scoped harvest committed no transaction; the WAL contract cannot be vacuous")
	}
	if len(smallRun.ops) != len(largeRun.ops) {
		t.Fatalf("source-root operations differ by size: %d-session store has %v, %d-session store has %v", small, sortedOpKeys(smallRun.ops), large, sortedOpKeys(largeRun.ops))
	}
	for key, count := range smallRun.ops {
		if largeRun.ops[key] != count {
			t.Fatalf("source-root operation %q ran %d times on the %d-session store and %d times on the %d-session store", key, count, small, largeRun.ops[key], large)
		}
	}
	if strings.Join(smallRun.evidence, "\x00") != strings.Join(largeRun.evidence, "\x00") {
		t.Fatalf("evidence golden differs by size: %v vs %v", smallRun.evidence, largeRun.evidence)
	}
	if smallRun.report != largeRun.report {
		t.Fatalf("ScopeReport differs by size: %q vs %q", smallRun.report, largeRun.report)
	}
}

func sortedOpKeys(ops map[string]int) []string {
	out := make([]string, 0, len(ops))
	for key := range ops {
		out = append(out, strings.ReplaceAll(key, "\x00", " "))
	}
	sort.Strings(out)
	return out
}

func countScopeWAL(t *testing.T, dbPath string) int {
	t.Helper()
	frames, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatalf("count WAL commit frames of %s: %v", dbPath, err)
	}
	return frames
}

// validateReadRecorderCase is the loader refusal the fixture contract names: a
// read_recorder case whose expected evidence set is empty proves nothing, and a
// stale path that is also a context path would make the stale-origin read fail
// for the wrong reason. Both are refused by name, not by a count.
func validateReadRecorderCase(name string, expected, stalePaths, contextPaths []string) error {
	if len(expected) == 0 {
		return fmt.Errorf("read_recorder case %q names an empty expected evidence set; a recorder that may request nothing cannot prove the by-path read happened; fix: name the path the pass must request", name)
	}
	context := make(map[string]bool, len(contextPaths))
	for _, path := range contextPaths {
		context[path] = true
	}
	for _, path := range stalePaths {
		if context[path] {
			return fmt.Errorf("read_recorder case %q plants a stale origin at %q, which is also a context path; a readable related member would put that path in the cohort-build request set and make the stale-origin assertion green for the wrong reason; fix: choose a stored-but-unreadable root with no readable related member", name, path)
		}
	}
	return nil
}

// TestSessionScopeUpkeepLoaderRefusals pins the two loader refusals the
// fixture names.
func TestSessionScopeUpkeepLoaderRefusals(t *testing.T) {
	t.Parallel()
	fixture := loadScopeUpkeepFixture(t)
	if len(fixture.Refusals) == 0 {
		t.Fatal("the upkeep fixture declares no loader refusals")
	}
	for _, refusal := range fixture.Refusals {
		refusal := refusal
		t.Run(refusal.Name, func(t *testing.T) {
			if refusal.Guard != "read_recorder" {
				t.Fatalf("refusal %q names guard %q; only read_recorder refusal cases exist", refusal.Name, refusal.Guard)
			}
			err := validateReadRecorderCase(refusal.Name, refusal.Expected, refusal.StalePaths, refusal.ContextPaths)
			if err == nil {
				t.Fatalf("the loader accepted %q, want a refusal naming %q", refusal.Name, refusal.WantErrorContain)
			}
			if !strings.Contains(err.Error(), refusal.WantErrorContain) {
				t.Fatalf("loader refusal = %v, want it to contain %q", err, refusal.WantErrorContain)
			}
		})
	}
}
