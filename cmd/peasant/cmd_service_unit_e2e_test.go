package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// unitRun is what running the service unit produced.
type unitRun struct {
	harvestExit int
	redactRan   bool
	redactExit  int
	output      string
}

// failed reports whether systemd would leave the unit failed. A oneshot
// fails when ExecStart= fails or when an ExecStopPost= without a "-" prefix
// fails (systemd v257 src/core/service.c: service_sigchld_event in
// SERVICE_STOP_POST passes the control process result to
// service_enter_signal, which records it when the result is still success).
func (r unitRun) failed() bool { return r.result() != "success" }

// result is the unit's Result= property. systemd keeps the first failure:
// service_enter_signal and service_enter_dead only overwrite a success, so
// when harvest and redact both fail the unit records harvest's failure and
// redact's exit status reaches only the journal.
func (r unitRun) result() string {
	if r.firstFailure() == "" {
		return "success"
	}
	return "exit-code"
}

// firstFailure names the step whose failure the unit result records: the
// first one that failed, or "" when none did.
func (r unitRun) firstFailure() string {
	switch {
	case r.harvestExit != 0:
		return "harvest"
	case r.redactRan && r.redactExit != 0:
		return "redact"
	}
	return ""
}

// ExecMainStatus is always ExecStart='s exit code, even when only
// ExecStopPost= failed.
func (r unitRun) execMainStatus() int { return r.harvestExit }

// systemdEnvValue unquotes one Environment= value: C escapes \\ and \"
// inside double quotes and %% for a literal %.
func systemdEnvValue(t *testing.T, quoted string) string {
	t.Helper()
	if len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
		t.Fatalf("Environment= value %q is not double-quoted", quoted)
	}
	var b strings.Builder
	body := quoted[1 : len(quoted)-1]
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '\\':
			i++
			b.WriteByte(body[i])
		case body[i] == '%':
			i++
			b.WriteByte('%')
		default:
			b.WriteByte(body[i])
		}
	}
	return b.String()
}

// runServiceUnit runs peasant-harvest.service the way systemd runs a
// oneshot: ExecStart=, then every ExecStopPost= whether ExecStart= failed or
// not, each in a subprocess whose environment is only HOME, PATH and the
// unit's Environment= lines (the user manager's environment, not the test's).
// The unit names the real peasant binary, so this is the production path.
func runServiceUnit(t *testing.T, unitText, home string) unitRun {
	t.Helper()
	return runServiceUnitWith(t, unitText, home, nil)
}

// runServiceUnitWith is runServiceUnit with afterHarvest called between
// ExecStart= and ExecStopPost=, so a test can make only redact fail.
func runServiceUnitWith(t *testing.T, unitText, home string, afterHarvest func()) unitRun {
	t.Helper()
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	for _, value := range unitDirectiveValues(unitText, "Environment=") {
		env = append(env, systemdEnvValue(t, value))
	}
	var out bytes.Buffer
	exec1 := func(line string) int {
		words := splitSystemdExec(t, line)
		cmd := exec.Command(words[0], words[1:]...)
		cmd.Env = env
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		if err != nil {
			t.Fatalf("start %q: %v", words[0], err)
		}
		return 0
	}
	starts := execStartsOf(unitText)
	if len(starts) != 1 {
		t.Fatalf("unit has %d ExecStart= lines, want 1", len(starts))
	}
	run := unitRun{harvestExit: exec1(starts[0])}
	if afterHarvest != nil {
		afterHarvest()
	}
	for _, line := range unitDirectiveValues(unitText, "ExecStopPost=") {
		run.redactRan = true
		run.redactExit = exec1(line)
	}
	run.output = out.String()
	return run
}

// installServiceForE2E installs the unit for the real peasant binary into a
// fresh HOME with a Claude Code source holding one seeded-token session.
func installServiceForE2E(t *testing.T, configYAML string) (*serviceHarness, string, string) {
	t.Helper()
	return installServiceForE2EIn(t, newServiceHarness(t, "home dir"), configYAML)
}

// transcriptCopies lists the peasant-sync transcript copies under root.
func transcriptCopies(t *testing.T, root string) []string {
	t.Helper()
	var copies []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, "--transcript.jsonl") {
			copies = append(copies, path)
		}
		return err
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return copies
}

func assertCopiesRedacted(t *testing.T, root string) {
	t.Helper()
	copies := transcriptCopies(t, root)
	if len(copies) == 0 {
		t.Fatalf("no transcript copy under %s", root)
	}
	for _, path := range copies {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(serviceSeededToken)) {
			t.Errorf("%s still holds the seeded token", path)
		}
	}
}

// TestServiceUnitRedactsWhenHarvestFails: one corrupt session makes harvest
// exit 1. The copies harvest did write must still be redacted, and the unit
// must still report failure.
func TestServiceUnitRedactsWhenHarvestFails(t *testing.T) {
	t.Parallel()
	h, home, src := installServiceForE2E(t, "")
	bad := filepath.Join(src, "-svc-demo", "22222222-2222-4222-8222-222222222222.jsonl")
	if err := os.WriteFile(bad, []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := runServiceUnit(t, h.read(t, serviceUnitName), home)
	if run.harvestExit == 0 {
		t.Fatalf("harvest succeeded with a corrupt session; the test needs it to fail:\n%s", run.output)
	}
	if !run.redactRan || run.redactExit != 0 || !strings.Contains(run.output, "redacted 1 session(s)") {
		t.Fatalf("redact after a failed harvest: ran=%v exit=%d\n%s", run.redactRan, run.redactExit, run.output)
	}
	if !run.failed() {
		t.Error("the unit reported success after harvest failed")
	}
	assertCopiesRedacted(t, filepath.Join(home, "peasant", "peasant-sync"))
}

// lockCopyDirs makes every session directory holding a peasant-sync copy
// read-only, so redact's temp-file write fails, and restores it on cleanup.
func lockCopyDirs(t *testing.T, root string) {
	t.Helper()
	copies := transcriptCopies(t, root)
	if len(copies) == 0 {
		t.Fatal("harvest wrote no copies to lock")
	}
	for _, c := range copies {
		dir := filepath.Dir(c)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}
}

// TestServiceUnitFailsWhenRedactFails: harvest succeeds, then redact cannot
// write the copy. systemd marks the unit failed (Result=exit-code) while
// ExecMainStatus stays harvest's 0.
func TestServiceUnitFailsWhenRedactFails(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory this test relies on")
	}
	h, home, _ := installServiceForE2E(t, "")
	sync := filepath.Join(home, "peasant", "peasant-sync")
	run := runServiceUnitWith(t, h.read(t, serviceUnitName), home, func() { lockCopyDirs(t, sync) })
	if run.harvestExit != 0 {
		t.Fatalf("harvest failed; the test needs it to succeed:\n%s", run.output)
	}
	if !run.redactRan || run.redactExit == 0 {
		t.Fatalf("redact into a read-only directory: ran=%v exit=%d\n%s", run.redactRan, run.redactExit, run.output)
	}
	if !run.failed() || run.result() != "exit-code" || run.firstFailure() != "redact" {
		t.Errorf("unit after a failed redact: failed=%v Result=%s first=%q, want failed, exit-code, redact",
			run.failed(), run.result(), run.firstFailure())
	}
	if run.execMainStatus() != 0 {
		t.Errorf("ExecMainStatus = %d, want harvest's 0", run.execMainStatus())
	}
}

// TestServiceUnitBothFailRecordsHarvest: harvest fails on a corrupt session
// and redact fails too. The unit result records harvest's failure only.
func TestServiceUnitBothFailRecordsHarvest(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory this test relies on")
	}
	h, home, src := installServiceForE2E(t, "")
	bad := filepath.Join(src, "-svc-demo", "22222222-2222-4222-8222-222222222222.jsonl")
	if err := os.WriteFile(bad, []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sync := filepath.Join(home, "peasant", "peasant-sync")
	run := runServiceUnitWith(t, h.read(t, serviceUnitName), home, func() { lockCopyDirs(t, sync) })
	if run.harvestExit == 0 || !run.redactRan || run.redactExit == 0 {
		t.Fatalf("want both steps to fail: harvest %d, redact ran=%v exit %d\n%s",
			run.harvestExit, run.redactRan, run.redactExit, run.output)
	}
	if run.result() != "exit-code" || run.firstFailure() != "harvest" || run.execMainStatus() != run.harvestExit {
		t.Errorf("Result=%s first=%q ExecMainStatus=%d, want exit-code, harvest, %d",
			run.result(), run.firstFailure(), run.execMainStatus(), run.harvestExit)
	}
}

// TestServiceUnitHarvestAndRedactAgreeThroughUnitEnv: no --output anywhere.
// Harvest and redact must find the same copies through the unit's own
// Environment= and directory flags, with the default and a custom basePath.
func TestServiceUnitHarvestAndRedactAgreeThroughUnitEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		basePath func(home string) string
	}{
		{"default peasant-sync under the data dir", nil},
		{"custom output.basePath", func(home string) string { return filepath.Join(home, "custom out", "sync") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newServiceHarness(t, "home dir")
			home := h.dir
			sync := filepath.Join(home, "peasant", "peasant-sync")
			config := ""
			if tc.basePath != nil {
				sync = tc.basePath(home)
				config = "output:\n  basePath: " + sync + "\n"
			}
			h, home, _ = installServiceForE2EIn(t, h, config)
			run := runServiceUnit(t, h.read(t, serviceUnitName), home)
			if run.failed() || run.redactExit != 0 {
				t.Fatalf("unit run failed (harvest %d, redact %d):\n%s", run.harvestExit, run.redactExit, run.output)
			}
			if !strings.Contains(run.output, "redacted 1 session(s)") {
				t.Fatalf("redact did not find the harvested copy:\n%s", run.output)
			}
			assertCopiesRedacted(t, sync)
		})
	}
}

// installServiceForE2EIn is installServiceForE2E on an existing harness.
func installServiceForE2EIn(t *testing.T, h *serviceHarness, configYAML string) (*serviceHarness, string, string) {
	t.Helper()
	h.exe = buildPeasantMatrixBinary(t)
	home := h.dir
	src := filepath.Join(home, ".claude", "projects")
	writeServiceClaudeSession(t, src, "demo")
	if configYAML != "" {
		cfgDir := filepath.Join(home, "peasant")
		if err := os.MkdirAll(cfgDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := h.run(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	return h, home, src
}
