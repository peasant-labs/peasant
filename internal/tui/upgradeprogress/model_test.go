package upgradeprogress

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/states.yaml
var statesYAML []byte

//go:embed testdata/states.manifest.yaml
var statesManifestYAML []byte

type planStepFixture struct {
	Version int    `yaml:"version"`
	Kind    string `yaml:"kind"`
	Title   string `yaml:"title"`
	Cost    string `yaml:"cost"`
	Counts  string `yaml:"counts"`
}

type planFixture struct {
	From       int               `yaml:"from"`
	To         int               `yaml:"to"`
	StoreBytes int64             `yaml:"storeBytes"`
	Steps      []planStepFixture `yaml:"steps"`
}

type eventFixture struct {
	Kind      string `yaml:"kind"`
	Step      int    `yaml:"step"`
	ElapsedMs int64  `yaml:"elapsedMs"`
	Done      int64  `yaml:"done"`
	Of        int64  `yaml:"of"`
	WALBytes  int64  `yaml:"walBytes"`
}

type finishedStepFixture struct {
	Step       int   `yaml:"step"`
	DurationMs int64 `yaml:"durationMs"`
}

type stateFixture struct {
	Key          string                `yaml:"key"`
	Plan         planFixture           `yaml:"plan"`
	Events       []eventFixture        `yaml:"events"`
	Finished     []finishedStepFixture `yaml:"finished"`
	Outcome      string                `yaml:"outcome"`
	Error        string                `yaml:"error"`
	WantContains []string              `yaml:"wantContains"`
	WantAbsent   []string              `yaml:"wantAbsent"`
}

type statesFixture struct {
	States []stateFixture `yaml:"states"`
}

func loadStates(t *testing.T) statesFixture {
	t.Helper()
	var fixtures statesFixture
	decoder := yaml.NewDecoder(bytes.NewReader(statesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode testdata/states.yaml: %v", err)
	}
	var manifest struct {
		RequiredStates []string `yaml:"requiredStates"`
	}
	decoder = yaml.NewDecoder(bytes.NewReader(statesManifestYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode the states manifest: %v", err)
	}
	present := make(map[string]bool, len(fixtures.States))
	for _, state := range fixtures.States {
		if present[state.Key] {
			t.Fatalf("states.yaml repeats state %q", state.Key)
		}
		present[state.Key] = true
		if !nonEmpty(state.WantContains) {
			t.Fatalf("state %q declares no wantContains marker", state.Key)
		}
	}
	if err := testutil.RequireFixtureNames("upgradeprogress states", "state", manifest.RequiredStates, present); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func nonEmpty(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return len(values) > 0
}

func (f planFixture) storePlan(t *testing.T) store.UpgradePlan {
	t.Helper()
	plan := store.UpgradePlan{From: f.From, To: f.To, StoreBytes: f.StoreBytes}
	for _, step := range f.Steps {
		kind, err := store.NewUpgradeStepKind(step.Kind)
		if err != nil {
			t.Fatal(err)
		}
		cost, err := store.NewUpgradeCost(step.Cost)
		if err != nil {
			t.Fatal(err)
		}
		counts, err := store.NewUpgradeUnit(step.Counts)
		if err != nil {
			t.Fatal(err)
		}
		plan.Steps = append(plan.Steps, store.UpgradeStep{
			Version: step.Version, Kind: kind, Title: step.Title, Cost: cost, Counts: counts,
		})
	}
	return plan
}

func (f stateFixture) model(t *testing.T, th theme.Theme) Model {
	t.Helper()
	plan := f.Plan.storePlan(t)
	model := New(Options{Theme: th, Plan: plan})
	for _, event := range f.Events {
		if event.Step < 1 || event.Step > len(plan.Steps) {
			t.Fatalf("state %q names event step %d outside the %d-step plan", f.Key, event.Step, len(plan.Steps))
		}
		kind, err := store.NewUpgradeEventKind(event.Kind)
		if err != nil {
			t.Fatalf("state %q: %v", f.Key, err)
		}
		step := plan.Steps[event.Step-1]
		model, _ = model.Update(EventMsg{Event: store.UpgradeEvent{
			Kind: kind, Step: step, Index: event.Step, Total: len(plan.Steps),
			Done: event.Done, Of: event.Of, Unit: step.Counts,
			WALBytes: event.WALBytes, Elapsed: time.Duration(event.ElapsedMs) * time.Millisecond,
		}})
	}
	if f.Outcome != "" {
		outcome, err := NewOutcome(f.Outcome)
		if err != nil {
			t.Fatalf("state %q: %v", f.Key, err)
		}
		report := store.UpgradeReport{From: plan.From, To: plan.To}
		for _, finished := range f.Finished {
			if finished.Step < 1 || finished.Step > len(plan.Steps) {
				t.Fatalf("state %q names finished step %d outside the plan", f.Key, finished.Step)
			}
			report.Steps = append(report.Steps, store.UpgradeStepResult{
				Step: plan.Steps[finished.Step-1], Duration: time.Duration(finished.DurationMs) * time.Millisecond,
			})
		}
		var finalErr error
		if f.Error != "" {
			finalErr = errors.New(f.Error)
		}
		model, _ = model.Update(FinalMsg{Report: report, Outcome: outcome, Err: finalErr})
	}
	return model
}

func TestStates(t *testing.T) {
	fixtures := loadStates(t)
	for _, state := range fixtures.States {
		t.Run(state.Key, func(t *testing.T) {
			for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
				for _, size := range []struct{ width, height int }{{80, 24}, {120, 40}} {
					t.Run(fmt.Sprintf("%s-%dx%d", mode, size.width, size.height), func(t *testing.T) {
						model := state.model(t, theme.New(mode))
						model.SetSize(size.width, size.height)
						view := model.View()
						assertViewMarkers(t, state.Key, view, state.WantContains, state.WantAbsent)
						assertViewFits(t, state.Key, view, size.width, size.height)
						again := model.View()
						if view != again {
							t.Fatalf("state %q rendered differently on a second pass", state.Key)
						}
					})
				}
			}
		})
	}
}

func assertViewMarkers(t *testing.T, key, view string, wantContains, wantAbsent []string) {
	t.Helper()
	plain := ansi.Strip(view)
	for _, marker := range wantContains {
		if !strings.Contains(plain, marker) {
			t.Errorf("state %q: rendered view does not contain %q:\n%s", key, marker, plain)
		}
	}
	for _, marker := range wantAbsent {
		if strings.Contains(plain, marker) {
			t.Errorf("state %q: rendered view unexpectedly contains %q:\n%s", key, marker, plain)
		}
	}
}

func assertViewFits(t *testing.T, key, view string, width, height int) {
	t.Helper()
	if got := lipgloss.Height(view); got > height {
		t.Errorf("state %q: view is %d rows, want at most %d", key, got, height)
	}
	for i, line := range strings.Split(view, "\n") {
		if got := ansi.StringWidth(line); got > width {
			t.Errorf("state %q: row %d is %d cells wide, want at most %d:\n%s", key, i+1, got, width, ansi.Strip(line))
		}
	}
}

func TestOutcomeIsAClosedSet(t *testing.T) {
	expected := []Outcome{OutcomeDone, OutcomeStoppedByUser, OutcomeRefusedLowDisk, OutcomeRefusedConsent, OutcomeFailedStep}
	if !slices.Equal(AllOutcomes, expected) {
		t.Fatalf("AllOutcomes = %v, want %v", AllOutcomes, expected)
	}
	for _, outcome := range AllOutcomes {
		parsed, err := NewOutcome(outcome.String())
		if err != nil || parsed != outcome {
			t.Fatalf("NewOutcome(%q) = %v, %v; want %v", outcome, parsed, err, outcome)
		}
	}
	if _, err := NewOutcome("hijacked"); err == nil {
		t.Fatal("NewOutcome accepted a value outside the closed set")
	}
}

// TestLongPlanKeepsTheActiveStep proves an old store's step list windows
// around the running step instead of clipping it away.
func TestLongPlanKeepsTheActiveStep(t *testing.T) {
	plan := store.UpgradePlan{From: 1, To: 60}
	for version := 2; version <= 61; version++ {
		plan.Steps = append(plan.Steps, store.UpgradeStep{
			Version: version, Kind: store.UpgradeStepSchema,
			Title: fmt.Sprintf("step title %d", version), Cost: store.UpgradeCostQuick,
		})
	}
	model := New(Options{Theme: theme.New(theme.ModeDark), Plan: plan})
	model.SetSize(80, 24)
	for index := 1; index <= 30; index++ {
		model, _ = model.Update(EventMsg{Event: store.UpgradeEvent{
			Kind: store.UpgradeEventStepFinished, Step: plan.Steps[index-1], Index: index, Total: len(plan.Steps),
			Elapsed: time.Second,
		}})
	}
	model, _ = model.Update(EventMsg{Event: store.UpgradeEvent{
		Kind: store.UpgradeEventStepStarted, Step: plan.Steps[30], Index: 31, Total: len(plan.Steps),
	}})
	view := model.View()
	plain := ansi.Strip(view)
	if !strings.Contains(plain, "step 31 of 60") {
		t.Fatalf("the running step is not visible:\n%s", plain)
	}
	if !strings.Contains(plain, "earlier step(s)") {
		t.Fatalf("the hidden step rows are not named:\n%s", plain)
	}
	assertViewFits(t, "long-plan", view, 80, 24)
}
