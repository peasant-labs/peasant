package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/peasant/internal/tui/upgradeprogress"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/store_upgrade_progress.yaml
var storeUpgradeProgressYAML []byte

// storeUpgradeProgressFixturePath is the embedded upgrade-progress fixture.
// The embed directive must spell the path; every other use, including errors, uses this name.
const storeUpgradeProgressFixturePath = "testdata/store_upgrade_progress.yaml"

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

// upgradeProgressFixtureError names the synthetic error supplied to the
// outcome mapping. It is not a store-upgrade action or a push-wizard state.
type upgradeProgressFixtureError string

const (
	upgradeProgressFixtureNoError     upgradeProgressFixtureError = "none"
	upgradeProgressFixtureInterrupted upgradeProgressFixtureError = "interrupted"
	upgradeProgressFixtureLowDisk     upgradeProgressFixtureError = "low-disk"
	upgradeProgressFixtureConsent     upgradeProgressFixtureError = "consent"
	upgradeProgressFixtureStepError   upgradeProgressFixtureError = "step"
)

type upgradeProgressOutcomeCase struct {
	Name  string                      `yaml:"name"`
	Error upgradeProgressFixtureError `yaml:"error"`
	Want  string                      `yaml:"want"`
}

type upgradeProgressThemeCase struct {
	Name   string `yaml:"name"`
	Config string `yaml:"config"`
	Absent bool   `yaml:"absent"`
	Want   string `yaml:"want"`
}

// upgradeProgressMountedScenario selects the synthetic mounted upgrade outcome.
// The screenshot harness owns a different closed set with some shared wire values.
type upgradeProgressMountedScenario string

const (
	upgradeProgressMountedDone        upgradeProgressMountedScenario = "done"
	upgradeProgressMountedStopped     upgradeProgressMountedScenario = "stopped-before-start"
	upgradeProgressMountedPostFailure upgradeProgressMountedScenario = "failed-after-commit"
)

type upgradeProgressMountedCase struct {
	Name         string                         `yaml:"name"`
	Scenario     upgradeProgressMountedScenario `yaml:"scenario"`
	Notice       string                         `yaml:"notice"`
	WantContains []string                       `yaml:"wantContains"`
	WantAbsent   []string                       `yaml:"wantAbsent"`
}

type upgradeProgressFixtures struct {
	PlainCases   []upgradeProgressPlainCase   `yaml:"plainCases"`
	OutcomeCases []upgradeProgressOutcomeCase `yaml:"outcomeCases"`
	ThemeCases   []upgradeProgressThemeCase   `yaml:"themeCases"`
	MountedCases []upgradeProgressMountedCase `yaml:"mountedCases"`
}

func loadUpgradeProgressFixtures(t *testing.T) upgradeProgressFixtures {
	t.Helper()
	var fixtures upgradeProgressFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(storeUpgradeProgressYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode %s: %v", storeUpgradeProgressFixturePath, err)
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
	for _, c := range fixtures.ThemeCases {
		present[c.Name] = true
	}
	for _, c := range fixtures.MountedCases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("store upgrade progress", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestRunUpgradeWithProgressMountedRendering exercises the terminal path. The
// stub pauses after the initial frame, then (when needed) until its notice
// appears, so the notice really arrives while the live block owns the PTY.
// This mounted test stubs upgradeStore and proves rendering only. The real
// Upgrade progress emission is tested in internal/store/upgrade_test.go.
func TestRunUpgradeWithProgressMountedRendering(t *testing.T) {
	previousUpgrade, previousConsent := upgradeStore, needsMigrationConsent
	t.Cleanup(func() {
		upgradeStore, needsMigrationConsent = previousUpgrade, previousConsent
	})
	needsMigrationConsent = func(string) (bool, error) { return false, nil }
	for _, c := range loadUpgradeProgressFixtures(t).MountedCases {
		t.Run(c.Name, func(t *testing.T) {
			master, terminal := openTestTerminal(t)
			setTestTerminalSize(t, terminal, 24, 80)
			header := newSignalWriter("upgrading the local store")
			notice := newSignalWriter(c.Notice)
			copied := make(chan error, 1)
			go func() {
				_, err := io.Copy(io.MultiWriter(header, notice), master)
				copied <- err
			}()

			ctx, cancel := context.WithTimeout(testwait.Context(t), 8*time.Second)
			defer cancel()
			start, resume := make(chan struct{}), make(chan struct{})
			step := store.UpgradeStep{Version: 70, Kind: store.UpgradeStepSchema, Title: "add derived lookups", Cost: store.UpgradeCostQuick}
			plan := store.UpgradePlan{DBPath: "/synthetic/store.db", From: 69, To: 70, Steps: []store.UpgradeStep{step}}
			report := store.UpgradeReport{From: 69, To: 70}
			upgradeStore = func(ctx context.Context, _ string, options store.UpgradeOptions) (store.UpgradeReport, error) {
				select {
				case <-start:
				case <-ctx.Done():
					return report, ctx.Err()
				}
				if c.Scenario == upgradeProgressMountedStopped {
					return report, fmt.Errorf("waiting for the upgrade lock: %w", context.Canceled)
				}
				options.Progress(store.UpgradeEvent{Kind: store.UpgradeEventStepStarted, Step: step, Index: 1, Total: 1})
				if c.Notice != "" {
					_, _ = fmt.Fprintln(options.Notices, c.Notice)
					select {
					case <-resume:
					case <-ctx.Done():
						return report, ctx.Err()
					}
				}
				options.Progress(store.UpgradeEvent{Kind: store.UpgradeEventStepFinished, Step: step, Index: 1, Total: 1, Elapsed: time.Second})
				report.Steps = []store.UpgradeStepResult{{Step: step, Duration: time.Second}}
				if c.Scenario == upgradeProgressMountedPostFailure {
					return report, errors.New("could not close the database")
				}
				return report, nil
			}
			cmd := &cobra.Command{Use: "upgrade-test"}
			cmd.SetContext(ctx)
			cmd.SetErr(terminal)
			cmd.Flags().String("config", filepath.Join(t.TempDir(), "absent.yaml"), "")
			type result struct {
				report store.UpgradeReport
				err    error
			}
			done := make(chan result, 1)
			go func() {
				report, err := runUpgradeWithProgressMode(cmd, plan, storeUpgradeProgressAuto)
				done <- result{report, err}
			}()

			select {
			case <-header.seen:
			case <-ctx.Done():
				t.Fatalf("the mounted upgrade did not render its initial frame: %v", ctx.Err())
			}
			beforeNotice := len(header.String())
			close(start)
			if c.Notice != "" {
				select {
				case <-notice.seen:
				case <-ctx.Done():
					t.Fatalf("the mounted upgrade did not display its notice: %v", ctx.Err())
				}
				// Bubble Tea inserts two terminal lines for the newline-terminated
				// notice before writing it. A direct write to the PTY does not.
				if delta := header.String()[beforeNotice:]; !strings.Contains(delta, ansi.InsertLine(2)) {
					t.Fatalf("notice was not inserted above the live block: %q", delta)
				}
			}
			close(resume)
			var got result
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatalf("the mounted upgrade did not return after its final report: %v", ctx.Err())
			}
			if (got.err == nil) != (c.Scenario == upgradeProgressMountedDone) {
				t.Fatalf("upgrade error = %v for scenario %q", got.err, c.Scenario)
			}
			if c.Scenario != upgradeProgressMountedStopped && len(got.report.Steps) != 1 {
				t.Fatalf("upgrade returned %d committed steps, want one", len(got.report.Steps))
			}
			if err := terminal.Close(); err != nil {
				t.Fatal(err)
			}
			if err := testwait.Receive(t, copied, "the mounted upgrade PTY drained after shutdown"); err != nil && !errors.Is(err, syscall.EIO) {
				t.Fatalf("read mounted upgrade output: %v", err)
			}
			text := ansi.Strip(header.String())
			for _, marker := range c.WantContains {
				if !strings.Contains(text, marker) {
					t.Errorf("mounted output does not contain %q:\n%s", marker, text)
				}
			}
			for _, marker := range c.WantAbsent {
				if strings.Contains(text, marker) {
					t.Errorf("mounted output unexpectedly contains %q:\n%s", marker, text)
				}
			}
		})
	}
}

// TestRunUpgradeWithProgressRendererExitDoesNotBlockNotices kills the mounted
// renderer after the upgrade starts, then has the real notice writer and
// progress callback report more work after the program has exited.
func TestRunUpgradeWithProgressRendererExitDoesNotBlockNotices(t *testing.T) {
	previousUpgrade, previousConsent := upgradeStore, needsMigrationConsent
	t.Cleanup(func() {
		upgradeStore, needsMigrationConsent = previousUpgrade, previousConsent
	})
	needsMigrationConsent = func(string) (bool, error) { return false, nil }
	master, terminal := openTestTerminal(t)
	setTestTerminalSize(t, terminal, 24, 80)
	output := newSignalWriter("upgrading the local store")
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(output, master)
		copied <- err
	}()
	ctx, cancel := context.WithTimeout(testwait.Context(t), 8*time.Second)
	defer cancel()
	step := store.UpgradeStep{Version: 70, Kind: store.UpgradeStepSchema, Title: "add derived lookups", Cost: store.UpgradeCostQuick}
	plan := store.UpgradePlan{DBPath: "/synthetic/store.db", From: 69, To: 70, Steps: []store.UpgradeStep{step}}
	rendererExited := make(chan struct{})
	upgradeStore = func(_ context.Context, _ string, options store.UpgradeOptions) (store.UpgradeReport, error) {
		writer, ok := options.Notices.(upgradeProgramNoticeWriter)
		if !ok {
			return store.UpgradeReport{}, errors.New("the terminal upgrade installed no program notice writer")
		}
		options.Progress(store.UpgradeEvent{Kind: store.UpgradeEventStepStarted, Step: step, Index: 1, Total: 1})
		writer.program.Kill()
		writer.program.Wait()
		close(rendererExited)
		_, _ = fmt.Fprintln(options.Notices, "synthetic notice after renderer exit")
		options.Progress(store.UpgradeEvent{Kind: store.UpgradeEventStepFinished, Step: step, Index: 1, Total: 1, Elapsed: time.Second})
		return store.UpgradeReport{From: 69, To: 70, Steps: []store.UpgradeStepResult{{Step: step, Duration: time.Second}}}, nil
	}
	cmd := &cobra.Command{Use: "upgrade-test"}
	cmd.SetContext(ctx)
	cmd.SetErr(terminal)
	cmd.Flags().String("config", filepath.Join(t.TempDir(), "absent.yaml"), "")
	type result struct {
		report store.UpgradeReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := runUpgradeWithProgressMode(cmd, plan, storeUpgradeProgressAuto)
		done <- result{report, err}
	}()
	select {
	case <-rendererExited:
	case <-ctx.Done():
		t.Fatalf("the renderer did not exit during the upgrade: %v", ctx.Err())
	}
	select {
	case got := <-done:
		if got.err != nil || len(got.report.Steps) != 1 {
			t.Fatalf("upgrade after renderer exit = %+v, %v; want one committed step and no upgrade error", got.report, got.err)
		}
	case <-ctx.Done():
		t.Fatalf("the upgrade blocked after the renderer exited: %v", ctx.Err())
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testwait.Receive(t, copied, "the stopped renderer's PTY drained"); err != nil && !errors.Is(err, syscall.EIO) {
		t.Fatalf("read stopped renderer output: %v", err)
	}
	if text := ansi.Strip(output.String()); !strings.Contains(text, "store upgrade progress renderer failed") {
		t.Fatalf("the renderer failure was not reported:\n%s", text)
	}
}

func TestRunUpgradeWithProgressConsentPendingUsesPlainLines(t *testing.T) {
	previousUpgrade, previousConsent := upgradeStore, needsMigrationConsent
	t.Cleanup(func() {
		upgradeStore, needsMigrationConsent = previousUpgrade, previousConsent
	})
	needsMigrationConsent = func(string) (bool, error) { return true, nil }
	upgradeStore = func(_ context.Context, _ string, options store.UpgradeOptions) (store.UpgradeReport, error) {
		_, _ = fmt.Fprintln(options.Notices, "synthetic consent notice")
		return store.UpgradeReport{}, store.ErrMigrationRefused
	}
	master, terminal := openTestTerminal(t)
	output := newSignalWriter("synthetic consent notice")
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(output, master)
		copied <- err
	}()
	ctx, cancel := context.WithTimeout(testwait.Context(t), 8*time.Second)
	defer cancel()
	cmd := &cobra.Command{Use: "upgrade-test"}
	cmd.SetErr(terminal)
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := runUpgradeWithProgressMode(cmd, store.UpgradePlan{DBPath: "/synthetic/store.db"}, storeUpgradeProgressAuto)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrMigrationRefused) {
			t.Fatalf("consent refusal = %v, want ErrMigrationRefused", err)
		}
	case <-ctx.Done():
		t.Fatalf("the upgrade did not return after consent was refused: %v", ctx.Err())
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testwait.Receive(t, copied, "the plain consent path drained its PTY"); err != nil && !errors.Is(err, syscall.EIO) {
		t.Fatalf("read consent notice: %v", err)
	}
	text := output.String()
	if !strings.Contains(text, "synthetic consent notice") || strings.Contains(text, "upgrading the local store") || strings.Contains(text, "\x1b[") {
		t.Fatalf("pending consent must retain plain terminal output without mounting progress:\n%q", text)
	}
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
			case upgradeProgressFixtureNoError:
			case upgradeProgressFixtureInterrupted:
				err = &store.UpgradeInterruptedError{DBPath: "/synthetic/store.db", Cause: context.Canceled}
			case upgradeProgressFixtureLowDisk:
				err = &store.UpgradeDiskError{DBPath: "/synthetic/store.db", FreeBytes: 1, RequiredBytes: 2}
			case upgradeProgressFixtureConsent:
				err = errors.Join(errors.New("synthetic refusal"), store.ErrMigrationRefused)
			case upgradeProgressFixtureStepError:
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

func TestShouldMountUpgradeProgress(t *testing.T) {
	previous := needsMigrationConsent
	t.Cleanup(func() { needsMigrationConsent = previous })

	needsMigrationConsent = func(string) (bool, error) { return false, nil }
	if !shouldMountUpgradeProgress(storeUpgradeProgressAuto, true, "/synthetic/store.db") {
		t.Fatal("a terminal upgrade with no pending consent must mount the inline program")
	}
	if shouldMountUpgradeProgress(storeUpgradeProgressPlain, true, "/synthetic/store.db") {
		t.Fatal("a plain progress mode must never mount the inline program")
	}
	if shouldMountUpgradeProgress(storeUpgradeProgressAuto, false, "/synthetic/store.db") {
		t.Fatal("a non-terminal upgrade must not mount the inline program")
	}

	needsMigrationConsent = func(string) (bool, error) { return true, nil }
	if shouldMountUpgradeProgress(storeUpgradeProgressAuto, true, "/synthetic/store.db") {
		t.Fatal("a pending one-time migration consent must keep the inline program unmounted")
	}

	needsMigrationConsent = func(string) (bool, error) {
		return false, errors.New("synthetic consent probe failure")
	}
	if shouldMountUpgradeProgress(storeUpgradeProgressAuto, true, "/synthetic/store.db") {
		t.Fatal("an unreadable consent gate must fall back to plain lines")
	}
}

// TestStoreUpgradeThemeFromConfig is the production-wiring test for the
// inline upgrade child's palette: the same resolveConfigPath/loadConfig/
// themeModeFor path the rest of the CLI uses picks the theme before the store
// opens, a configured light theme reaches the production model, and an absent
// or malformed config falls back to dark.
func TestStoreUpgradeThemeFromConfig(t *testing.T) {
	fixtures := loadUpgradeProgressFixtures(t)
	plan := store.UpgradePlan{From: 69, To: 70, StoreBytes: 1 << 20, Steps: []store.UpgradeStep{{
		Version: 70, Kind: store.UpgradeStepSchema, Title: "add derived lookups", Cost: store.UpgradeCostQuick,
	}}}
	var lightView, darkView string
	for _, c := range fixtures.ThemeCases {
		t.Run(c.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if !c.Absent {
				if err := os.WriteFile(path, []byte(c.Config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := &cobra.Command{Use: "upgrade-theme-test"}
			cmd.Flags().String("config", "", "")
			if err := cmd.Flags().Set("config", path); err != nil {
				t.Fatal(err)
			}
			got := storeUpgradeTheme(cmd)
			want, err := theme.ModeFromConfig(c.Want)
			if err != nil {
				t.Fatal(err)
			}
			if got.Mode != want {
				t.Fatalf("resolved theme = %q, want %q", got.Mode, want)
			}
			production := newUpgradeProgressModel(plan, got)
			production.SetSize(80, 24)
			explicit := upgradeprogress.New(upgradeprogress.Options{Theme: theme.New(want), Plan: plan})
			explicit.SetSize(80, 24)
			if view := production.View(); view != explicit.View() {
				t.Fatalf("the production model did not render in the resolved %q palette", want)
			}
			switch want {
			case theme.ModeLight:
				lightView = production.View()
			case theme.ModeDark:
				darkView = production.View()
			}
		})
	}
	// The comparison above is only evidence when the two palettes actually
	// differ for this surface.
	if lightView == "" || darkView == "" {
		t.Fatal("the theme cases did not render both palettes")
	}
	if lightView == darkView {
		t.Fatal("the light and dark palettes render identically for the upgrade surface; the wiring test would be vacuous")
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
