package store

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The turn count recount cases of TestStoreUpgrade. Each seeds the sessions
// of turn_count_recount.yaml and reads the stored counts from the raw file.

// buildRecountOwedStore seeds the recount fixture's sessions on a schema-76
// store and runs the frozen chain to version with the raw migration engine,
// so migration 77 writes its marker and nothing runs the recount: a store at
// 77 is one a killed 0.17.0 open left behind; a store at the head schema is
// one whose recount was skipped past.
func buildRecountOwedStore(t *testing.T, fixture turnCountRecountFixture, version int) string {
	t.Helper()
	path := seedTurnCountRecountStore(t, fixture, 76)
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	// WAL mode, like every store peasant writes, so a refused Open has no
	// journal mode to switch and can leave the file byte for byte.
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA journal_mode = WAL", nil); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(version)); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, pending, got := readTurnCountRecountState(t, path); !pending || got != int64(version) {
		t.Fatalf("built store is at schema %d with the marker pending=%t, want schema %d with the marker", got, pending, version)
	}
	return path
}

// wantRecountStep is the plan step of the turn count recount.
func wantRecountStep() UpgradeStep {
	entry := upgradeRecountCatalog[pendingRecountTurnCounts]
	return UpgradeStep{Version: 77, Kind: UpgradeStepRecount, Title: entry.title, Cost: entry.cost, Counts: entry.counts, recount: pendingRecountTurnCounts}
}

// schemaStep is the plan step of one migration, as the catalog gives it.
func schemaStep(version int) UpgradeStep {
	entry := upgradeStepCatalog[version]
	return UpgradeStep{Version: version, Kind: UpgradeStepSchema, Title: entry.title, Cost: entry.cost, Counts: entry.counts}
}

// requireRecounted fails unless every session holds the fixture's recounted
// counts and the marker is gone.
func requireRecounted(t *testing.T, fixture turnCountRecountFixture, path, after string) {
	t.Helper()
	counts, pending, _ := readTurnCountRecountState(t, path)
	if pending {
		t.Errorf("the recount marker is still set after %s", after)
	}
	for _, session := range fixture.Sessions {
		want := storedTurnCounts{session.WantTurnCount, session.WantConversationTurnCount}
		if got := counts[session.SessionID]; got != want {
			t.Errorf("%s: counts after %s = %+v, want %+v", session.Name, after, got, want)
		}
	}
}

func runUpgradeRecountPlanCase(t *testing.T, _ storeUpgradeCase) {
	fixture := loadTurnCountRecountFixture(t)
	head := CurrentSchemaVersion()
	planOf := func(t *testing.T, path string) UpgradePlan {
		t.Helper()
		before := fileHash(t, path)
		plan := mustPlan(t, path)
		assertHashUnchanged(t, path, before, "PlanUpgrade")
		return plan
	}
	requireSteps := func(t *testing.T, what string, plan UpgradePlan, want ...UpgradeStep) {
		t.Helper()
		if !slices.Equal(plan.Steps, want) {
			t.Fatalf("%s: plan steps = %+v, want %+v", what, plan.Steps, want)
		}
	}

	t.Run("crossing 77 with sessions", func(t *testing.T) {
		path := seedTurnCountRecountStore(t, fixture, 76)
		plan := planOf(t, path)
		requireSteps(t, "a schema-76 store with sessions", plan, schemaStep(77), wantRecountStep(), schemaStep(78), schemaStep(79))
		if !plan.HasLongStep() {
			t.Error("the recount is a long step; the plan must say so, so every entry point applies its long-step policy")
		}
	})
	t.Run("crossing 77 without sessions", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.db")
		buildBehindStore(t, path, 76, nil)
		requireSteps(t, "a schema-76 store with no sessions", planOf(t, path), schemaStep(77), schemaStep(78), schemaStep(79))
		// The plan's prediction agrees with the shipped migration: it
		// writes no marker either.
		if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, pending, _ := readTurnCountRecountState(t, path); pending {
			t.Fatal("migration 77 marked a recount on a store with no sessions, which the plan did not predict")
		}
	})
	t.Run("schema 77 with its marker", func(t *testing.T) {
		path := buildRecountOwedStore(t, fixture, 77)
		requireSteps(t, "a schema-77 store a killed 0.17.0 open left", planOf(t, path), wantRecountStep(), schemaStep(78), schemaStep(79))
	})
	t.Run("head schema with its marker", func(t *testing.T) {
		path := buildRecountOwedStore(t, fixture, head)
		plan := planOf(t, path)
		if plan.From != head || plan.To != head {
			t.Fatalf("plan header = %d -> %d, want %d -> %d", plan.From, plan.To, head, head)
		}
		requireSteps(t, "a head store with its marker", plan, wantRecountStep())
		if plan.RequiredFreeBytes <= 0 {
			t.Error("the recount writes to the store; its plan must ask for free disk")
		}
	})
}

func runUpgradeRecountOwedAtHeadCase(t *testing.T, _ storeUpgradeCase) {
	fixture := loadTurnCountRecountFixture(t)
	head := CurrentSchemaVersion()
	path := buildRecountOwedStore(t, fixture, head)

	s, err := Open(path, WithPoolSize(1))
	if err == nil {
		_ = s.Close()
		t.Fatal("Open accepted a store at the head schema that still owes its recount")
	}
	var behind *SchemaBehindError
	if !errors.As(err, &behind) || !IsSchemaBehind(err) {
		t.Fatalf("Open error = %v, want a *SchemaBehindError", err)
	}
	if behind.From != head || behind.To != head || !slices.Equal(behind.Owed, []string{wantRecountStep().Title}) {
		t.Errorf("SchemaBehindError = %+v, want %d -> %d owing %q", behind, head, head, wantRecountStep().Title)
	}
	for _, part := range []string{"did not finish", wantRecountStep().Title, "peasant migrate --schema-only", "nothing was changed"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("the refusal misses %q: %v", part, err)
		}
	}

	var finished []UpgradeStep
	report, err := Upgrade(t.Context(), path, UpgradeOptions{Progress: func(event UpgradeEvent) {
		if event.Kind == UpgradeEventStepFinished {
			finished = append(finished, event.Step)
		}
	}})
	if err != nil {
		t.Fatalf("upgrade the store that owes its recount: %v", err)
	}
	onlyRecount := []UpgradeStep{wantRecountStep()}
	var reported []UpgradeStep
	for _, result := range report.Steps {
		reported = append(reported, result.Step)
	}
	if report.From != head || report.To != head {
		t.Fatalf("upgrade report = %d -> %d, want %d -> %d", report.From, report.To, head, head)
	}
	if !slices.Equal(reported, onlyRecount) || !slices.Equal(finished, onlyRecount) {
		t.Fatalf("upgrade report = %+v with finished steps %+v, want the one recount step at schema %d", report, finished, head)
	}
	requireRecounted(t, fixture, path, "the upgrade")
	if plan := mustPlan(t, path); len(plan.Steps) != 0 {
		t.Fatalf("plan after the recount = %+v, want no steps", plan.Steps)
	}
	opened, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatalf("Open after the recount: %v", err)
	}
	_ = opened.Close()
}

func runUpgradeRecountOpenNeverRunsCase(t *testing.T, _ storeUpgradeCase) {
	fixture := loadTurnCountRecountFixture(t)
	for _, version := range []int{77, CurrentSchemaVersion()} {
		path := buildRecountOwedStore(t, fixture, version)
		before := fileHash(t, path)
		for attempt := 1; attempt <= 2; attempt++ {
			s, err := Open(path, WithPoolSize(1))
			if err == nil {
				_ = s.Close()
				t.Fatalf("schema %d: Open accepted a store that owes its recount", version)
			}
			if !IsSchemaBehind(err) {
				t.Fatalf("schema %d: Open error = %v, want a *SchemaBehindError", version, err)
			}
		}
		assertHashUnchanged(t, path, before, "the refused Open")
		assertNoSidecars(t, path, "the refused Open")
		counts, pending, got := readTurnCountRecountState(t, path)
		if !pending || got != int64(version) {
			t.Errorf("schema %d: after two refused opens the store is at %d with the marker pending=%t; Open must leave both", version, got, pending)
		}
		requireUnrecountedState(t, fixture, counts, "a refused Open")
	}
}

func runUpgradeRecountCancelledCase(t *testing.T, c storeUpgradeCase) {
	fixture := loadTurnCountRecountFixture(t)
	path := seedTurnCountRecountStore(t, fixture, c.From)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handled := 0
	report, err := Upgrade(ctx, path, UpgradeOptions{afterRecountSession: func(done int, _ ingest.SessionID) error {
		handled = done
		if done == c.StopAfterSessions {
			cancel()
		}
		return nil
	}})
	var interrupted *UpgradeInterruptedError
	if !errors.As(err, &interrupted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Upgrade error = %v, want an *UpgradeInterruptedError wrapping context.Canceled", err)
	}
	if handled != c.StopAfterSessions {
		t.Errorf("the recount handled %d sessions after the cancel at %d; it must stop at the next session", handled, c.StopAfterSessions)
	}
	if interrupted.Step != wantRecountStep() || interrupted.At != c.WantVersion || interrupted.Finished != 1 {
		t.Errorf("interruption = step %+v at schema %d after %d finished, want the recount at %d after 1", interrupted.Step, interrupted.At, interrupted.Finished, c.WantVersion)
	}
	if len(report.Steps) != 1 || report.Steps[0].Step != schemaStep(77) {
		t.Errorf("report lists %+v, want only step 77 finished", report.Steps)
	}
	counts, pending, version := readTurnCountRecountState(t, path)
	if version != int64(c.WantVersion) || !pending {
		t.Fatalf("after the cancelled recount the store is at %d with the marker pending=%t, want %d with the marker kept", version, pending, c.WantVersion)
	}
	requireUnrecountedState(t, fixture, counts, "the cancelled recount")

	if plan := mustPlan(t, path); !slices.Equal(plan.Steps, []UpgradeStep{wantRecountStep(), schemaStep(78), schemaStep(79)}) {
		t.Fatalf("plan after the cancel = %+v, want the recount, then 78 and 79", plan.Steps)
	}
	if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
		t.Fatalf("resume after the cancelled recount: %v", err)
	}
	requireRecounted(t, fixture, path, "the resumed upgrade")
}

func runUpgradeRecountInOrderCase(t *testing.T, c storeUpgradeCase) {
	fixture := loadTurnCountRecountFixture(t)
	path := seedTurnCountRecountStore(t, fixture, c.From)
	want := []UpgradeStep{schemaStep(77), wantRecountStep(), schemaStep(78), schemaStep(79)}
	if c.From != 76 || CurrentSchemaVersion() != 79 {
		t.Fatalf("this case pins the upgrade of a 0.16.2 store (schema 76) by a build at schema 79; the build is at %d; add the new steps to its want list", CurrentSchemaVersion())
	}

	type seen struct {
		kind  UpgradeEventKind
		index int
		step  UpgradeStep
	}
	var events []seen
	var recountProgress []UpgradeEvent
	report, err := Upgrade(t.Context(), path, UpgradeOptions{Progress: func(event UpgradeEvent) {
		if event.Total != len(want) {
			t.Errorf("event %s carries Total %d, want %d", event.Kind, event.Total, len(want))
		}
		if event.Kind == UpgradeEventStepProgress {
			if event.Step.Kind == UpgradeStepRecount && event.Of > 0 {
				recountProgress = append(recountProgress, event)
			}
			return
		}
		events = append(events, seen{event.Kind, event.Index, event.Step})
	}})
	if err != nil {
		t.Fatalf("upgrade the 0.16.2 store: %v", err)
	}
	var wantEvents []seen
	for i, step := range want {
		wantEvents = append(wantEvents, seen{UpgradeEventStepStarted, i + 1, step}, seen{UpgradeEventStepFinished, i + 1, step})
	}
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("step events = %+v, want %+v", events, wantEvents)
	}
	if report.From != 76 || report.To != 79 || len(report.Steps) != len(want) {
		t.Fatalf("report = %d -> %d with %d steps, want 76 -> 79 with %d", report.From, report.To, len(report.Steps), len(want))
	}
	for i, result := range report.Steps {
		if result.Step != want[i] {
			t.Errorf("report step %d = %+v, want %+v", i+1, result.Step, want[i])
		}
	}
	sessions := int64(len(fixture.Sessions))
	if len(recountProgress) == 0 {
		t.Fatal("the recount sent no counted progress; it counts sessions like step 74")
	}
	last := recountProgress[len(recountProgress)-1]
	if last != (UpgradeEvent{Kind: last.Kind, Step: last.Step, Index: last.Index, Total: last.Total, Done: sessions, Of: sessions, Unit: UpgradeUnitSessions, WALBytes: last.WALBytes, Elapsed: last.Elapsed}) {
		t.Errorf("last recount progress = %+v, want %d/%d sessions", last, sessions, sessions)
	}
	if got := mustVersionAt(t, path); got != 79 {
		t.Fatalf("store is at schema %d after the upgrade, want 79", got)
	}
	requireRecounted(t, fixture, path, "the upgrade from 0.16.2")
}

func runUpgradeRecountDoneInOrderCase(t *testing.T, c storeUpgradeCase) {
	fixture := loadTurnCountRecountFixture(t)
	if CurrentSchemaVersion() != 79 {
		t.Fatalf("this case pins the upgrade of a 0.17.0 store (schema 77) by a build at schema 79; the build is at %d; add the new steps to its want list", CurrentSchemaVersion())
	}
	// A 0.17.0 open that finished its recount: schema 77, the marker
	// deleted, and the counts it wrote (the sentinel stands for them).
	path := buildRecountOwedStore(t, fixture, c.From)
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM pending_recounts`, nil); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	sentinel := storedTurnCounts{fixture.InterruptedRecount.SentinelTurnCount, fixture.InterruptedRecount.SentinelConversationTurnCount}
	writeTurnCountSentinel(t, path, fixture.Sessions[0].SessionID, sentinel)
	countsBefore, _, _ := readTurnCountRecountState(t, path)

	want := []UpgradeStep{schemaStep(78), schemaStep(79)}
	if plan := mustPlan(t, path); plan.From != c.From || !slices.Equal(plan.Steps, want) {
		t.Fatalf("plan = %d -> %d with %+v, want %d -> 79 with %+v", plan.From, plan.To, plan.Steps, c.From, want)
	}
	var started []UpgradeStep
	report, err := Upgrade(t.Context(), path, UpgradeOptions{
		Progress: func(event UpgradeEvent) {
			if event.Kind == UpgradeEventStepStarted {
				started = append(started, event.Step)
			}
		},
		afterRecountSession: func(int, ingest.SessionID) error {
			t.Error("the upgrade of a store whose recount finished recounted again")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("upgrade the 0.17.0 store: %v", err)
	}
	if !slices.Equal(started, want) || len(report.Steps) != len(want) {
		t.Fatalf("started steps %+v and %d finished, want %+v", started, len(report.Steps), want)
	}
	counts, pending, version := readTurnCountRecountState(t, path)
	if version != 79 || pending {
		t.Fatalf("store is at schema %d with the marker pending=%t, want 79 with no marker", version, pending)
	}
	if !maps.Equal(counts, countsBefore) {
		t.Errorf("stored counts changed across the upgrade: %+v, want %+v", counts, countsBefore)
	}
}
