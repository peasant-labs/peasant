package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/upgradeprogress"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/store_upgrade_progress.yaml
var storeUpgradeProgressYAML []byte

//go:embed testdata/store_upgrade_progress.manifest.yaml
var storeUpgradeProgressManifestYAML []byte

type upgradeProgressEventCase struct {
	Kind      string `yaml:"kind"`
	StepIndex int    `yaml:"stepIndex"`
	Total     int    `yaml:"total"`
	Title     string `yaml:"title"`
	Cost      string `yaml:"cost"`
	AtSeconds int64  `yaml:"atSeconds"`
	ElapsedMs int64  `yaml:"elapsedMs"`
	WALBytes  int64  `yaml:"walBytes"`
}

type upgradeProgressPlainCase struct {
	Name         string                     `yaml:"name"`
	Events       []upgradeProgressEventCase `yaml:"events"`
	WantStarts   int                        `yaml:"wantStarts"`
	WantProgress int                        `yaml:"wantProgress"`
	WantEnds     int                        `yaml:"wantEnds"`
	WantContains []string                   `yaml:"wantContains"`
}

type upgradeProgressOutcomeCase struct {
	Name  string `yaml:"name"`
	Error string `yaml:"error"`
	Want  string `yaml:"want"`
}

type upgradeProgressFixtures struct {
	PlainCases   []upgradeProgressPlainCase   `yaml:"plainCases"`
	OutcomeCases []upgradeProgressOutcomeCase `yaml:"outcomeCases"`
}

func loadUpgradeProgressFixtures(t *testing.T) upgradeProgressFixtures {
	t.Helper()
	var fixtures upgradeProgressFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(storeUpgradeProgressYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode testdata/store_upgrade_progress.yaml: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decoder = yaml.NewDecoder(bytes.NewReader(storeUpgradeProgressManifestYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode the upgrade progress manifest: %v", err)
	}
	present := map[string]bool{}
	for _, c := range fixtures.PlainCases {
		present[c.Name] = true
	}
	for _, c := range fixtures.OutcomeCases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("store upgrade progress", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestStoreUpgradePlainLines(t *testing.T) {
	fixtures := loadUpgradeProgressFixtures(t)
	for _, c := range fixtures.PlainCases {
		t.Run(c.Name, func(t *testing.T) {
			var out bytes.Buffer
			printer := newStoreUpgradeLinePrinter(&out)
			base := time.Unix(1700000000, 0)
			current := base
			printer.now = func() time.Time { return current }
			for _, event := range c.Events {
				current = base.Add(time.Duration(event.AtSeconds) * time.Second)
				kind, err := store.NewUpgradeEventKind(event.Kind)
				if err != nil {
					t.Fatal(err)
				}
				cost, err := store.NewUpgradeCost(event.Cost)
				if err != nil {
					t.Fatal(err)
				}
				printer.event(store.UpgradeEvent{
					Kind: kind, Index: event.StepIndex, Total: event.Total,
					Step: store.UpgradeStep{Title: event.Title, Cost: cost},
					Done: 0, Of: 0, Unit: store.UpgradeUnitNone,
					WALBytes: event.WALBytes, Elapsed: time.Duration(event.ElapsedMs) * time.Millisecond,
				})
			}
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			starts, progress, ends := 0, 0, 0
			for _, line := range lines {
				switch {
				case strings.HasSuffix(line, ": started"):
					starts++
				case strings.Contains(line, "elapsed, write-ahead log grew by"):
					progress++
				case strings.Contains(line, ": done in "):
					ends++
				}
			}
			if starts != c.WantStarts || progress != c.WantProgress || ends != c.WantEnds {
				t.Fatalf("lines = %d starts, %d progress, %d ends; want %d, %d, %d:\n%s",
					starts, progress, ends, c.WantStarts, c.WantProgress, c.WantEnds, out.String())
			}
			for _, marker := range c.WantContains {
				if !strings.Contains(out.String(), marker) {
					t.Fatalf("output does not contain %q:\n%s", marker, out.String())
				}
			}
			if strings.Contains(out.String(), "written to the log") {
				t.Fatalf("output claims bytes were written to the log; the number is net file growth:\n%s", out.String())
			}
		})
	}
}

func TestStoreUpgradeOutcome(t *testing.T) {
	fixtures := loadUpgradeProgressFixtures(t)
	for _, c := range fixtures.OutcomeCases {
		t.Run(c.Name, func(t *testing.T) {
			var err error
			switch c.Error {
			case "none":
			case "interrupted":
				err = &store.UpgradeInterruptedError{DBPath: "/synthetic/store.db", Cause: context.Canceled}
			case "low-disk":
				err = &store.UpgradeDiskError{DBPath: "/synthetic/store.db", FreeBytes: 1, RequiredBytes: 2}
			case "step":
				err = errors.New("synthetic step failure")
			default:
				t.Fatalf("unknown fixture error %q", c.Error)
			}
			want, parseErr := upgradeprogress.NewOutcome(c.Want)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if got := storeUpgradeOutcome(store.UpgradeReport{}, err); got != want {
				t.Fatalf("outcome = %q, want %q", got, want)
			}
		})
	}
}

func TestRunUpgradeWithProgressUsesPlainLinesOffTerminal(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "upgrade-test"}
	cmd.SetErr(&out)
	cmd.SetOut(&out)
	previous := upgradeStore
	upgradeStore = func(_ context.Context, _ string, options store.UpgradeOptions) (store.UpgradeReport, error) {
		if options.Progress == nil {
			t.Fatal("the production path installed no progress callback")
		}
		step := store.UpgradeStep{Version: 70, Kind: store.UpgradeStepSchema, Title: "add derived lookups", Cost: store.UpgradeCostQuick}
		options.Progress(store.UpgradeEvent{Kind: store.UpgradeEventStepStarted, Step: step, Index: 1, Total: 1})
		options.Progress(store.UpgradeEvent{Kind: store.UpgradeEventStepFinished, Step: step, Index: 1, Total: 1, Elapsed: time.Second, WALBytes: 2048})
		return store.UpgradeReport{From: 69, To: 70, Steps: []store.UpgradeStepResult{{Step: step, Duration: time.Second, WALBytes: 2048}}}, nil
	}
	t.Cleanup(func() { upgradeStore = previous })
	plan := store.UpgradePlan{DBPath: "/synthetic/store.db", From: 69, To: 70,
		Steps: []store.UpgradeStep{{Version: 70, Kind: store.UpgradeStepSchema, Title: "add derived lookups", Cost: store.UpgradeCostQuick}}}
	if _, err := runUpgradeWithProgressMode(cmd, plan, storeUpgradeProgressAuto); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, marker := range []string{
		"store upgrade 1/1: add derived lookups: started",
		"write-ahead log grew by 2.0 KiB",
		"store upgrade finished: schema 69 to 70, 1 step(s)",
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("plain output does not contain %q:\n%s", marker, text)
		}
	}
	if isTerminalWriter(&out) {
		t.Fatal("a bytes.Buffer was classified as an interactive terminal")
	}
}

func TestStoreUpgradeProgressModesAreClosed(t *testing.T) {
	for _, mode := range allStoreUpgradeProgressModes {
		parsed, err := newStoreUpgradeProgressMode(string(mode))
		if err != nil || parsed != mode {
			t.Fatalf("newStoreUpgradeProgressMode(%q) = %q, %v", mode, parsed, err)
		}
	}
	if _, err := newStoreUpgradeProgressMode("tui-always"); err == nil {
		t.Fatal("a progress mode outside the closed set was accepted")
	}
}
