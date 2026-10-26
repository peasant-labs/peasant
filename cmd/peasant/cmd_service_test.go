package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

type serviceUnitCase struct {
	Name            string       `yaml:"name"`
	Args            []string     `yaml:"args"`
	RedactArgs      []string     `yaml:"redact_args"`
	ExecStartRedact string       `yaml:"exec_start_redact"`
	Interval        string       `yaml:"interval"`
	OnCalendar      string       `yaml:"on_calendar"`
	RandomizedDelay string       `yaml:"randomized_delay"`
	Environment     []serviceEnv `yaml:"environment"`
	EnvironmentOut  []string     `yaml:"environment_lines"`
	ExecStart       string       `yaml:"exec_start"`
	Timer           []string     `yaml:"timer"`
}

func loadServiceUnitCases(t *testing.T) []serviceUnitCase {
	t.Helper()
	data, err := os.ReadFile("testdata/service_units.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []serviceUnitCase `yaml:"cases"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("no cases in service_units.yaml")
	}
	return f.Cases
}

func mustDuration(t *testing.T, s string) time.Duration {
	t.Helper()
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func unitSection(text, section string) []string {
	var lines []string
	in := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "[") {
			in = line == "["+section+"]"
			continue
		}
		if in && line != "" && !strings.HasPrefix(line, "Unit=") {
			lines = append(lines, line)
		}
	}
	return lines
}

func unitDirectiveValues(text, directive string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, directive); ok {
			lines = append(lines, v)
		}
	}
	return lines
}

func execStartsOf(text string) []string { return unitDirectiveValues(text, "ExecStart=") }

func execStartOf(text string) string {
	if lines := execStartsOf(text); len(lines) > 0 {
		return lines[0]
	}
	return ""
}

// splitSystemdExec is an independent reader of the ExecStart quoting
// systemd applies: double-quoted words, \\ and \" escapes, %% and $$.
func splitSystemdExec(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	for line = strings.TrimSpace(line); line != ""; line = strings.TrimSpace(line) {
		if line[0] != '"' {
			t.Fatalf("unquoted word in %q", line)
		}
		var b strings.Builder
		i := 1
		for ; i < len(line) && line[i] != '"'; i++ {
			switch {
			case line[i] == '\\':
				i++
				b.WriteByte(line[i])
			case isSystemdDoubledEscape(line, i):
				b.WriteByte(line[i])
				i++
			case line[i] == '%' || line[i] == '$':
				t.Fatalf("unescaped %c in %q", line[i], line)
			default:
				b.WriteByte(line[i])
			}
		}
		words = append(words, b.String())
		line = line[i+1:]
	}
	return words
}

// isSystemdDoubledEscape reports a %% or $$ pair starting at i.
func isSystemdDoubledEscape(line string, i int) bool {
	special := line[i] == '%' || line[i] == '$'
	return special && i+1 < len(line) && line[i+1] == line[i]
}

func TestServiceUnitsRenderFixture(t *testing.T) {
	t.Parallel()
	for _, tc := range loadServiceUnitCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			schedule := serviceSchedule{interval: mustDuration(t, tc.Interval), calendar: tc.OnCalendar, randomizedDelay: mustDuration(t, tc.RandomizedDelay)}
			units := renderServiceUnits(serviceInvocation{harvest: tc.Args, redact: tc.RedactArgs, env: tc.Environment}, schedule)
			var envLines []string
			for _, line := range unitSection(units.Service, "Service") {
				if strings.HasPrefix(line, "Environment=") {
					envLines = append(envLines, line)
				}
			}
			if strings.Join(envLines, "\n") != strings.Join(tc.EnvironmentOut, "\n") {
				t.Errorf("Environment\n got: %q\nwant: %q", envLines, tc.EnvironmentOut)
			}
			for name, text := range map[string]string{"service": units.Service, "timer": units.Timer} {
				if !strings.HasPrefix(text, serviceUnitMarker+"\n") {
					t.Errorf("%s unit does not start with the marker:\n%s", name, text)
				}
			}
			if got := execStartOf(units.Service); got != tc.ExecStart {
				t.Errorf("ExecStart\n got: %s\nwant: %s", got, tc.ExecStart)
			}
			if starts := execStartsOf(units.Service); len(starts) != 1 {
				t.Errorf("ExecStart lines = %q, want harvest only", starts)
			}
			stopPost := unitDirectiveValues(units.Service, "ExecStopPost=")
			if tc.RedactArgs == nil && len(stopPost) != 0 {
				t.Errorf("ExecStopPost = %q, want none", stopPost)
			}
			if tc.RedactArgs != nil && (len(stopPost) != 1 || stopPost[0] != tc.ExecStartRedact) {
				t.Errorf("redact ExecStopPost = %q, want %q", stopPost, tc.ExecStartRedact)
			}
			if got := splitSystemdExec(t, execStartOf(units.Service)); strings.Join(got, "\x00") != strings.Join(tc.Args, "\x00") {
				t.Errorf("ExecStart does not round-trip: got %q want %q", got, tc.Args)
			}
			if got := unitSection(units.Timer, "Timer"); strings.Join(got, "\n") != strings.Join(tc.Timer, "\n") {
				t.Errorf("[Timer]\n got: %q\nwant: %q", got, tc.Timer)
			}
			if !strings.Contains(units.Service, "Type=oneshot\n") || !strings.Contains(units.Timer, "Unit=peasant-harvest.service\n") || !strings.Contains(units.Timer, "WantedBy=timers.target\n") {
				t.Errorf("unit pair is missing a required directive:\n%s\n%s", units.Service, units.Timer)
			}
		})
	}
}

// fakeSystemctl records `systemctl --user` calls and answers them.
type fakeSystemctl struct {
	mu    sync.Mutex
	calls []string
	// fail makes every call return this error with this output.
	failOut string
	fail    error
}

func (f *fakeSystemctl) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.fail != nil {
		return f.failOut, f.fail
	}
	switch {
	case len(args) > 0 && args[0] == "is-enabled":
		return "enabled\n", nil
	case len(args) > 0 && args[0] == "is-active":
		return "active\n", nil
	}
	return "", nil
}

func (f *fakeSystemctl) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type serviceHarness struct {
	dir     string
	unitDir string
	exe     string
	ctl     *fakeSystemctl
	deps    serviceDeps
}

func newServiceHarness(t *testing.T, dirName string) *serviceHarness {
	t.Helper()
	dir := filepath.Join(t.TempDir(), dirName)
	h := &serviceHarness{dir: dir, unitDir: filepath.Join(dir, "systemd", "user"), exe: filepath.Join(t.TempDir(), "bin dir", "peasant"), ctl: &fakeSystemctl{}}
	h.deps = serviceDeps{
		goos:       "linux",
		executable: func() (string, error) { return h.exe, nil },
		unitDir:    func() (string, error) { return h.unitDir, nil },
		systemctl:  h.ctl.run,
	}
	return h
}

func (h *serviceHarness) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return executeWithDataDir(t, buildServiceCommand(h.deps), h.dir, args)
}

func (h *serviceHarness) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.unitDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestServiceInstallDryRunMatchesWrittenUnits(t *testing.T) {
	t.Parallel()
	h := newServiceHarness(t, `dir with spaces 'q' "dq" 100% $HOME`)
	dry, err := h.run(t, "install", "--dry-run", "--interval", "15m")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, dry)
	}
	if calls := h.ctl.Calls(); len(calls) != 0 {
		t.Fatalf("dry run called systemctl: %q", calls)
	}
	if _, err := os.Stat(h.unitDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created the unit dir: %v", err)
	}
	out, err := h.run(t, "install", "--interval", "15m")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, name := range []string{serviceUnitName, timerUnitName} {
		written := h.read(t, name)
		block := "# " + filepath.Join(h.unitDir, name) + "\n" + written + "\n"
		if !strings.Contains(dry, block) {
			t.Errorf("dry-run output is not byte-identical to the written %s:\n--- written\n%s\n--- dry run\n%s", name, written, dry)
		}
		info, err := os.Stat(filepath.Join(h.unitDir, name))
		if err != nil || info.Mode().Perm() != serviceUnitPerm {
			t.Errorf("%s mode = %v, %v; want %v", name, info.Mode().Perm(), err, serviceUnitPerm)
		}
	}
	want := []string{"show-environment", "daemon-reload", "enable peasant-harvest.timer", "restart peasant-harvest.timer"}
	if got := h.ctl.Calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("systemctl calls = %q, want %q", got, want)
	}
	global := []string{"--data-dir", h.dir, "--config-dir", h.dir, "--state-dir", h.dir}
	unit := h.read(t, serviceUnitName)
	steps := map[string][]string{
		"ExecStart=":    append([]string{h.exe, "harvest"}, global...),
		"ExecStopPost=": append(append([]string{h.exe, "redact"}, global...), "--all"),
	}
	for directive, want := range steps {
		lines := unitDirectiveValues(unit, directive)
		if len(lines) != 1 {
			t.Fatalf("%s lines = %q, want exactly one", directive, lines)
		}
		if got := splitSystemdExec(t, lines[0]); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s args = %q, want %q", directive, got, want)
		}
	}
	for _, env := range []defaults.EnvVar{defaults.EnvXDGDataHome, defaults.EnvXDGConfigHome, defaults.EnvXDGStateHome} {
		name := env.String()
		want := "Environment=" + systemdQuote(name+"="+h.dir, false) + "\n"
		if !strings.Contains(h.read(t, serviceUnitName), want) {
			t.Errorf("service unit lacks %q", want)
		}
	}
	entries, _ := os.ReadDir(h.unitDir)
	if len(entries) != 2 {
		t.Errorf("unit dir holds %d entries, want the 2 units (no temp files left)", len(entries))
	}
}

// serviceSeededToken is a fake GitHub token the redact rules catch.
const serviceSeededToken = "ghp_0123456789abcdef0123456789abcdef0123"

// writeServiceClaudeSession writes a minimal Claude Code transcript whose
// user turn carries serviceSeededToken.
func writeServiceClaudeSession(t *testing.T, dir, project string) string {
	t.Helper()
	sid := "11111111-1111-4111-8111-111111111111"
	projectDir := filepath.Join(dir, "-svc-"+project)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := `"cwd":"/work/` + project + `","sessionId":"` + sid + `","isSidechain":false,"userType":"external","version":"2.1.0","gitBranch":"main"`
	lines := []string{
		`{` + base + `,"type":"user","uuid":"aaaaaaaa-0000-4000-8000-000000000001","timestamp":"2026-10-25T10:00:00.000Z","message":{"role":"user","content":"deploy with ` + serviceSeededToken + `"}}`,
		`{` + base + `,"type":"assistant","uuid":"aaaaaaaa-0000-4000-8000-000000000002","parentUuid":"aaaaaaaa-0000-4000-8000-000000000001","timestamp":"2026-10-25T10:00:01.000Z","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	}
	if err := os.WriteFile(filepath.Join(projectDir, sid+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sid
}

func TestServiceInstallNoRedactRendersHarvestOnly(t *testing.T) {
	t.Parallel()
	h := newServiceHarness(t, "home")
	if out, err := h.run(t, "install", "--no-redact"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	unit := h.read(t, serviceUnitName)
	starts := execStartsOf(unit)
	if len(starts) != 1 || splitSystemdExec(t, starts[0])[1] != "harvest" {
		t.Errorf("--no-redact ExecStart lines = %q, want harvest only", starts)
	}
	if post := unitDirectiveValues(unit, "ExecStopPost="); len(post) != 0 {
		t.Errorf("--no-redact ExecStopPost = %q, want none", post)
	}
}

func TestServiceReinstallConverges(t *testing.T) {
	t.Parallel()
	h := newServiceHarness(t, "home")
	if out, err := h.run(t, "install", "--interval", "15m"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if out, err := h.run(t, "install", "--on-calendar", "hourly", "--randomized-delay", "0s"); err != nil {
		t.Fatalf("reinstall: %v\n%s", err, out)
	}
	timer := unitSection(h.read(t, timerUnitName), "Timer")
	if strings.Join(timer, "\n") != "OnCalendar=hourly\nPersistent=true" {
		t.Errorf("reinstalled [Timer] = %q", timer)
	}
	first := h.read(t, serviceUnitName) + h.read(t, timerUnitName)
	if out, err := h.run(t, "install", "--on-calendar", "hourly", "--randomized-delay", "0s"); err != nil {
		t.Fatalf("third install: %v\n%s", err, out)
	}
	if again := h.read(t, serviceUnitName) + h.read(t, timerUnitName); again != first {
		t.Errorf("identical reinstall changed the units")
	}
}

func TestServiceUninstallRemovesOnlyItsOwnFiles(t *testing.T) {
	t.Parallel()
	h := newServiceHarness(t, "home")
	if out, err := h.run(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	other := filepath.Join(h.unitDir, "other.service")
	if err := os.WriteFile(other, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A hand-written timer under our name must survive and block nothing else.
	handTimer := "[Timer]\nOnCalendar=daily\n"
	if err := os.WriteFile(filepath.Join(h.unitDir, timerUnitName), []byte(handTimer), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := h.run(t, "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(h.unitDir, serviceUnitName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("generated service not removed: %v", err)
	}
	if h.read(t, timerUnitName) != handTimer {
		t.Error("uninstall changed a timer peasant did not write")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("uninstall touched an unrelated unit: %v", err)
	}
	if strings.Contains(strings.Join(h.ctl.Calls(), "|"), "disable") {
		t.Errorf("uninstall disabled a timer it did not write: %q", h.ctl.Calls())
	}
	again, err := h.run(t, "uninstall")
	if err != nil || !strings.Contains(again, "nothing to uninstall") {
		t.Errorf("second uninstall = %v\n%s", err, again)
	}
}

func TestServiceInstallRefusesForeignUnit(t *testing.T) {
	t.Parallel()
	h := newServiceHarness(t, "home")
	if err := os.MkdirAll(h.unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hand := "[Service]\nExecStart=/bin/true\n"
	if err := os.WriteFile(filepath.Join(h.unitDir, serviceUnitName), []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := h.run(t, "install")
	if err == nil || !strings.Contains(err.Error(), "not written by peasant") {
		t.Fatalf("install over a hand-written unit = %v", err)
	}
	if h.read(t, serviceUnitName) != hand {
		t.Error("install overwrote a hand-written unit")
	}
	if _, err := os.Stat(filepath.Join(h.unitDir, timerUnitName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("install wrote the timer after refusing the service")
	}
}

func TestServiceStatus(t *testing.T) {
	t.Parallel()
	h := newServiceHarness(t, "home")
	out, err := h.run(t, "status")
	if err != nil || !strings.Contains(out, serviceUnitName+": not installed") {
		t.Fatalf("status before install = %v\n%s", err, out)
	}
	if out, err := h.run(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	out, err = h.run(t, "status")
	if err != nil {
		t.Fatalf("status after install: %v\n%s", err, out)
	}
	for _, want := range []string{timerUnitName + ": installed", "is-enabled: enabled", "is-active: active"} {
		if !strings.Contains(out, want) {
			t.Errorf("status after install lacks %q:\n%s", want, out)
		}
	}
}

func TestServiceRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(h *serviceHarness)
		args    []string
		wantErr string
	}{
		{"interval below minimum", nil, []string{"install", "--interval", "30s"}, "--interval must be at least 1min"},
		{"interval not a duration", nil, []string{"install", "--interval", "often"}, "invalid argument"},
		{"negative delay", nil, []string{"install", "--randomized-delay", "-1m"}, "must not be negative"},
		{"interval and calendar", nil, []string{"install", "--interval", "1h", "--on-calendar", "daily"}, "mutually exclusive"},
		{"multi-line calendar", nil, []string{"install", "--on-calendar", "daily\nExecStart=/bin/sh"}, "one line"},
		{"source path without harness", nil, []string{"install", "--source-path", "/x"}, "--source-path requires --source-harness"},
		{"unknown harness", nil, []string{"install", "--source-harness", "nope", "--source-path", "/x"}, "nope"},
		{"not linux install", func(h *serviceHarness) { h.deps.goos = "darwin" }, []string{"install", "--dry-run"}, "Linux only (this is darwin)"},
		{"not linux status", func(h *serviceHarness) { h.deps.goos = "windows" }, []string{"status"}, "Linux only"},
		{"no systemctl", func(h *serviceHarness) { h.ctl.fail = errSystemctlMissing }, []string{"install"}, "systemctl not found on PATH"},
		{"dollar in binary path", func(h *serviceHarness) { h.exe = filepath.Join(h.dir, "b$X", "peasant") }, []string{"install", "--dry-run"}, "systemd cannot run an executable"},
		{"double quote in binary path", func(h *serviceHarness) { h.exe = filepath.Join(h.dir, `q"q`, "peasant") }, []string{"install", "--dry-run"}, "systemd cannot run an executable"},
		{"single quote in binary path", func(h *serviceHarness) { h.exe = filepath.Join(h.dir, "q'q", "peasant") }, []string{"install", "--dry-run"}, "systemd cannot run an executable"},
		{"backslash in binary path", func(h *serviceHarness) { h.exe = filepath.Join(h.dir, `b\b`, "peasant") }, []string{"install", "--dry-run"}, "systemd cannot run an executable"},
		{"newline in binary path", func(h *serviceHarness) { h.exe = filepath.Join(h.dir, "nl\nExecStartPre=/bin/true", "peasant") }, []string{"install", "--dry-run"}, "control character"},
		{"newline in source path", nil, []string{"install", "--dry-run", "--source-harness", string(defaults.HarnessClaudeCode), "--source-path", "/x\nExecStartPre=/bin/true"}, "control character"},
		{"tab in source path", nil, []string{"install", "--dry-run", "--source-harness", string(defaults.HarnessClaudeCode), "--source-path", "/x\ty"}, "control character"},
		{"no user manager", func(h *serviceHarness) {
			h.ctl.failOut = "Failed to connect to user scope bus via local transport"
			h.ctl.fail = errors.New("exit status 1")
		}, []string{"install"}, "loginctl enable-linger"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newServiceHarness(t, "home")
			if tc.setup != nil {
				tc.setup(h)
			}
			out, err := h.run(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q\n%s", err, tc.wantErr, out)
			}
			if _, statErr := os.Stat(h.unitDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused command created %s", h.unitDir)
			}
		})
	}
}

// TestServiceUnitsVerifyWithSystemd runs systemd's own parser over the
// rendered pair when systemd-analyze is installed (it is not on CI runners
// without systemd, where this skips). The directory arguments carry a space,
// %, $, a double quote and a backslash; the binary sits in a path with a
// space, the one special character systemd accepts in an executable.
func TestServiceUnitsVerifyWithSystemd(t *testing.T) {
	t.Parallel()
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not installed")
	}
	h := newServiceHarness(t, `home 100% $HOME "q" \b`)
	h.exe = filepath.Join(t.TempDir(), "bin dir", "peasant")
	if err := os.MkdirAll(filepath.Dir(h.exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := h.run(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	cmd := exec.Command(analyze, "--user", "verify", "--man=no", filepath.Join(h.unitDir, serviceUnitName), filepath.Join(h.unitDir, timerUnitName))
	// A private runtime dir: --user verify never reaches a real user manager.
	cmd.Env = append(os.Environ(), "SYSTEMD_LOG_LEVEL=warning", "XDG_RUNTIME_DIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || len(bytes.TrimSpace(out)) > 0 {
		t.Fatalf("systemd-analyze verify: %v\n%s", err, out)
	}
}

// TestServiceInstallRefusesControlCharacterInDataDir: a newline in an
// inherited or passed directory would start a new unit directive.
func TestServiceInstallRefusesControlCharacterInDataDir(t *testing.T) {
	t.Parallel()
	h := newServiceHarness(t, "nl\nExecStartPre=/bin/touch pwned")
	out, err := h.run(t, "install", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("install --dry-run with a newline in the data dir = %v\n%s", err, out)
	}
	if strings.Contains(out, "\nExecStartPre=") {
		t.Errorf("a unit with an injected directive was printed:\n%s", out)
	}
}

// TestServiceExecutablePrefersStablePathEntry: a package-manager install
// resolves os.Executable to a versioned path; the unit must name the PATH
// entry that links to it, which survives an upgrade.
func TestServiceExecutablePrefersStablePathEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	versioned := filepath.Join(root, "store", "abc123-peasant-0.12.0", "bin", "peasant")
	if err := os.MkdirAll(filepath.Dir(versioned), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versioned, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "profile", "bin", "peasant")
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versioned, profile); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other", "peasant")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		onPath string
		want   string
	}{
		{"PATH links to this binary", profile, profile},
		{"PATH names a different peasant", other, versioned},
		{"no peasant on PATH", "", versioned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newServiceHarness(t, "home")
			h.exe = versioned
			h.deps.lookPath = func(string) (string, error) {
				if tc.onPath == "" {
					return "", exec.ErrNotFound
				}
				return tc.onPath, nil
			}
			if out, err := h.run(t, "install"); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			if got := splitSystemdExec(t, execStartOf(h.read(t, serviceUnitName)))[0]; got != tc.want {
				t.Errorf("unit binary = %q, want %q", got, tc.want)
			}
		})
	}
}
