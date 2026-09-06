package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/tui/ingestprogress"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/ingest_progress.yaml
var ingestProgressYAML []byte

type harvestInterruptKind string

const (
	harvestInterruptKey  harvestInterruptKind = "key"
	harvestInterruptINT  harvestInterruptKind = "sigint"
	harvestInterruptTERM harvestInterruptKind = "sigterm"
)

type harvestInterruptCase struct {
	Name      string               `yaml:"name"`
	Terminal  bool                 `yaml:"terminal"`
	JSON      bool                 `yaml:"json"`
	Interrupt harvestInterruptKind `yaml:"interrupt"`
}

type inlineFixture struct {
	Name     string   `yaml:"name"`
	Cancel   bool     `yaml:"cancel"`
	Stop     bool     `yaml:"stop"`
	Theme    string   `yaml:"theme"`
	Width    int      `yaml:"width"`
	Height   int      `yaml:"height"`
	Contains []string `yaml:"contains"`
	Absent   []string `yaml:"absent"`
}

type ingestProgressFixtures struct {
	Lifecycle struct {
		Required []string `yaml:"required_cases"`
		Cases    []struct {
			Name     string `yaml:"name"`
			Key      bool   `yaml:"key"`
			External bool   `yaml:"external"`
		} `yaml:"cases"`
	} `yaml:"lifecycle"`
	Interrupt struct {
		Required []string               `yaml:"required_cases"`
		Cases    []harvestInterruptCase `yaml:"cases"`
	} `yaml:"interrupt"`
	Inline struct {
		Required []string        `yaml:"required_cases"`
		Cases    []inlineFixture `yaml:"cases"`
	} `yaml:"inline"`
}

func loadIngestProgressFixtures(t *testing.T) ingestProgressFixtures {
	t.Helper()
	var doc ingestProgressFixtures
	d := yaml.NewDecoder(bytes.NewReader(ingestProgressYAML))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("expected exactly one ingest progress fixture document: %v", err)
	}
	validateNamedFixtures(t, "interrupt", doc.Interrupt.Required, len(doc.Interrupt.Cases), func(i int) (string, bool) {
		c := doc.Interrupt.Cases[i]
		valid := false
		switch c.Interrupt {
		case harvestInterruptKey:
			valid = c.Terminal && !c.JSON
		case harvestInterruptINT, harvestInterruptTERM:
			valid = true
		}
		return c.Name, valid
	})
	validateNamedFixtures(t, "inline", doc.Inline.Required, len(doc.Inline.Cases), func(i int) (string, bool) {
		c := doc.Inline.Cases[i]
		return c.Name, c.Width > 0 && c.Height > 0 && len(c.Contains) > 0
	})
	validateNamedFixtures(t, "lifecycle", doc.Lifecycle.Required, len(doc.Lifecycle.Cases), func(i int) (string, bool) {
		c := doc.Lifecycle.Cases[i]
		return c.Name, !(c.Key && c.External)
	})
	return doc
}

// The operation deliberately withholds completion after acknowledging cancel.
// Raw terminal input and inherited cancellation must keep the same renderer alive.
func TestProgressRendererCancellationAcknowledgment(t *testing.T) {
	for _, c := range loadIngestProgressFixtures(t).Lifecycle.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := ingest.NewProgressState()
			state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageDiff, Total: 10})
			state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiff, Done: 4, Total: 10})
			out := newSignalWriter("ctrl+c to cancel")
			r := newProgressRenderer(out, state, nil, cancel)
			r.isTTY = true
			master, terminal := openTestTerminal(t)
			if err := unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer terminal.Close()
			r.input = terminal
			r.w = terminal
			copied := make(chan struct{})
			go func() { _, _ = io.Copy(out, master); close(copied) }()
			defer func() { _ = terminal.Close(); _ = master.Close(); <-copied }()
			go r.Run(ctx)
			done := make(chan struct{})
			go func() { r.Wait(); close(done) }()
			defer func() { r.Stop(ctx.Err() != nil); <-done }()
			waitFor := func(text string) {
				t.Helper()
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				tick := time.NewTicker(10 * time.Millisecond)
				defer tick.Stop()
				for !strings.Contains(out.String(), text) {
					select {
					case <-deadline.C:
						t.Fatalf("missing %q: %s", text, out.String())
					case <-tick.C:
					}
				}
			}
			waitFor("ctrl+c to cancel")
			if c.Key {
				if _, err := master.Write([]byte{3}); err != nil {
					t.Fatal(err)
				}
			}
			if c.External {
				cancel()
			}
			if c.Key || c.External {
				// The diff renderer reuses the initial 'c' from the old hint.
				waitFor("waiting for current work to stop")
				if ctx.Err() != context.Canceled {
					t.Fatal("key did not cancel operation")
				}
				select {
				case <-done:
					t.Fatal("renderer quit before operation acknowledged cancellation")
				default:
				}
			}
			r.Stop(ctx.Err() != nil)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("renderer did not stop after completion")
			}
			if err := r.Err(); err != nil {
				t.Fatal(err)
			}
			if c.Key || c.External {
				if !strings.Contains(out.String(), "harvest canceled") || !strings.Contains(out.String(), "4/10") {
					t.Fatalf("lost final snapshot: %s", out.String())
				}
			} else if strings.Contains(out.String(), "harvest canceled") {
				t.Fatal("success labeled canceled")
			}
		})
	}
}

func TestProgressModelSuccessClears(t *testing.T) {
	m := ingestprogress.NewModel(ingest.NewProgressState(), nil, theme.New(theme.ModeDark), time.Now(), nil)
	updated, cmd := m.Update(ingestprogress.StopMsg{})
	if cmd == nil || updated.View().Content != "" {
		t.Fatal("success must clear the live view")
	}
}

func TestProgressModelCancellationFreezesFinalSnapshot(t *testing.T) {
	started := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	state := ingest.NewProgressState()
	state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageDiff, Total: 10})
	m := ingestprogress.NewModel(state, nil, theme.New(theme.ModeDark), started, nil)
	updated, _ := m.Update(ingestprogress.CancelMsg{})
	state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiff, Done: 4, Total: 10})
	updated, cmd := updated.Update(ingestprogress.StopMsg{Canceled: true, At: started.Add(8 * time.Second)})
	if cmd == nil {
		t.Fatal("acknowledged cancellation did not quit")
	}
	view := updated.View().Content
	if !strings.Contains(view, "4/10") || !strings.Contains(view, "total elapsed: 8s") || !strings.Contains(view, "harvest canceled") {
		t.Fatalf("final snapshot omitted last operation progress or clock: %s", view)
	}
	state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiff, Done: 9, Total: 10})
	updated, _ = updated.Update(ingestprogress.TickMsg(started.Add(time.Minute)))
	if updated.View().Content != view {
		t.Fatal("completed cancellation snapshot changed after termination")
	}
}

func TestProgressRendererFailureCancelsOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	master, terminal := openTestTerminal(t)
	defer master.Close()
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	r := newProgressRenderer(&output, ingest.NewProgressState(), nil, cancel)
	r.isTTY = true
	r.input = terminal
	go r.Run(ctx)
	done := make(chan struct{})
	go func() { r.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.Stop(true)
		<-done
		t.Fatal("renderer setup failure did not terminate")
	}
	if r.Err() == nil || ctx.Err() != context.Canceled {
		t.Fatalf("renderer failure lost cause or left operation running: %v, %v", r.Err(), ctx.Err())
	}
}

func validateNamedFixtures(t *testing.T, family string, required []string, count int, fixture func(int) (string, bool)) {
	t.Helper()
	if len(required) == 0 {
		t.Fatalf("%s fixture requires a named coverage manifest", family)
	}
	seen := make(map[string]bool, count)
	for i := range count {
		name, valid := fixture(i)
		if name == "" || seen[name] || !valid {
			t.Fatalf("invalid or duplicate %s fixture %q", family, name)
		}
		seen[name] = true
	}
	manifest := make(map[string]bool, len(required))
	for _, name := range required {
		if name == "" || manifest[name] {
			t.Fatalf("invalid or duplicate required %s case %q", family, name)
		}
		manifest[name] = true
		if !seen[name] {
			t.Fatalf("missing required %s case %q", family, name)
		}
	}
}

// TestProgressRenderer_Run_NonTTY verifies that in non-TTY mode (the default
// in test environments, where the writer is a bytes.Buffer rather than *os.File),
// Run() exits cleanly when completion is acknowledged without emitting ANSI.
func TestProgressRenderer_Run_NonTTY(t *testing.T) {
	state := ingest.NewProgressState()
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, state, nil)

	ctx, cancel := context.WithCancel(context.Background())

	// Run() is launched in a goroutine (per the usage contract documented in
	// the progressRenderer struct comment). newProgressRenderer pre-increments
	// the WaitGroup so Wait() is safe immediately after go r.Run(ctx).
	go r.Run(ctx)

	// Acknowledge completion immediately, even if cancellation preceded startup.
	cancel()
	r.Stop(true)
	r.Wait()

	// In non-TTY mode no bytes are written to the buffer.
	if buf.Len() != 0 {
		t.Errorf("non-TTY renderer wrote %d bytes to buffer, want 0", buf.Len())
	}
}

// TestProgressRenderer_IsTTY_NonFileWriter verifies that non-*os.File writers
// are always detected as non-TTY, preventing ANSI escape sequences from
// being written to non-terminal destinations (e.g. log files, pipes).
func TestProgressRenderer_IsTTY_NonFileWriter(t *testing.T) {
	state := ingest.NewProgressState()
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, state, nil)
	if r.IsTTY() {
		t.Error("IsTTY() = true for bytes.Buffer writer, want false")
	}
}

func TestProgressModelRenderWritesOutput(t *testing.T) {
	state := ingest.NewProgressState()
	// Emit a start event so the stage appears in the snapshot.
	state.Update(ingest.ProgressEvent{
		Kind:  ingest.KindStart,
		Stage: ingest.StageDiscover,
		Total: 5,
	})
	state.Update(ingest.ProgressEvent{
		Kind:  ingest.KindAdvance,
		Stage: ingest.StageDiscover,
		Done:  3,
		Total: 5,
	})

	model := ingestprogress.NewModel(state, nil, theme.New(theme.ModeDark), time.Now(), nil)
	out := model.Render()
	if out == "" {
		t.Fatal("render() wrote nothing, want non-empty output")
	}
	// The output should contain the stage name.
	if !strings.Contains(strings.ToLower(out), strings.ToLower(ingest.StageDiscover.String())) {
		t.Errorf("render() output %q does not contain stage name %q", out, ingest.StageDiscover.String())
	}
}

func TestProgressRendererUsesGentleFrameRate(t *testing.T) {
	if progressRendererFPS != 24 {
		t.Fatalf("progressRendererFPS = %d, want 24", progressRendererFPS)
	}
}

func TestRenderProgressBarShowsNonZeroProgressBeforeFirstFullCell(t *testing.T) {
	line := kit.ProgressBar(ingest.StageExtract.String(), 126, 4953, false, false)
	if !strings.Contains(line, "█") {
		t.Fatalf("progress bar should show at least one filled cell for non-zero work; got %q", line)
	}
	if !strings.Contains(line, "126/4953") {
		t.Fatalf("progress bar count should remain precise; got %q", line)
	}
}

// TestProgressRenderer_Run_TTY_StartStop verifies that when isTTY is true the
// tick loop terminates cleanly after completion is acknowledged.
// The renderer retains a final snapshot for a canceled operation.
func TestProgressRenderer_Run_TTY_StartStop(t *testing.T) {
	state := ingest.NewProgressState()
	state.Update(ingest.ProgressEvent{
		Kind:  ingest.KindStart,
		Stage: ingest.StageExtract,
		Total: 10,
	})

	var buf bytes.Buffer
	r := newProgressRenderer(&buf, state, nil)
	// Force TTY mode to exercise the tick path.
	r.isTTY = true

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)

	// Allow a couple of ticks (100 ms each) so the ticker fires at least once.
	time.Sleep(250 * time.Millisecond)
	cancel()
	r.Stop(true)
	r.Wait()

	// At least one Bubble Tea render should have occurred before shutdown.
	if buf.Len() == 0 {
		t.Error("TTY renderer wrote 0 bytes after ticks + cancel, want some output")
	}
}

func TestProgressModelControlCCancelsPipeline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := ingestprogress.NewModel(ingest.NewProgressState(), nil, theme.New(theme.ModeDark), time.Now(), cancel)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil {
		t.Fatal("ctrl+c must not quit before operation completion")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("pipeline context error = %v, want context.Canceled", ctx.Err())
	}
	if !strings.Contains(updated.View().Content, "canceling harvest") {
		t.Fatal("ctrl+c must retain progress while cancellation unwinds")
	}
	updated, cmd = updated.Update(ingestprogress.StopMsg{})
	if cmd == nil || !strings.Contains(updated.View().Content, "harvest canceled") {
		t.Fatal("completion racing with a cancellation request must retain canceled progress")
	}
}

func TestInlineProgressLayout(t *testing.T) {
	for _, c := range loadIngestProgressFixtures(t).Inline.Cases {
		t.Run(c.Name, func(t *testing.T) {
			mode, err := theme.ModeFromConfig(c.Theme)
			if err != nil {
				t.Fatal(err)
			}
			state := ingest.NewProgressState()
			state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageIndex, Total: 10})
			state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageIndex, Done: 4, Total: 10})
			started := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
			model := ingestprogress.NewModel(state, animation.IngestAnimation(), theme.New(mode), started, nil)
			updated, _ := model.Update(tea.WindowSizeMsg{Width: c.Width, Height: c.Height})
			updated, _ = updated.Update(ingestprogress.TickMsg(started.Add(8 * time.Second)))
			if c.Cancel {
				updated, _ = updated.Update(ingestprogress.CancelMsg{})
			}
			if c.Stop {
				updated, _ = updated.Update(ingestprogress.StopMsg{Canceled: c.Cancel, At: started.Add(8 * time.Second)})
			}
			view := updated.View()
			if view.AltScreen {
				t.Fatal("harvest must stay inline in the user's terminal")
			}
			plain := ansi.Strip(view.Content)
			for _, want := range c.Contains {
				if !strings.Contains(plain, want) {
					t.Errorf("missing %q:\n%s", want, plain)
				}
			}
			for _, absent := range c.Absent {
				if strings.Contains(plain, absent) {
					t.Errorf("unexpected %q:\n%s", absent, plain)
				}
			}
			lines := strings.Split(plain, "\n")
			if len(lines) > c.Height {
				t.Errorf("rendered %d rows into %d", len(lines), c.Height)
			}
			if c.Height == 40 && len(lines) == c.Height {
				t.Error("inline progress padded to full screen height")
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > c.Width {
					t.Errorf("row exceeds terminal width: %q", line)
				}
			}
			if view.Content != updated.View().Content {
				t.Error("View mutated clock or rendering state")
			}
		})
	}
}

// The child executes the production root, not a replacement harvest handler.
// A distinct exit code proves Execute returned context.Canceled, rather than
// the OS killing the test process or an unrelated error stopping ingestion.
func TestHarvestInterruptProcess(t *testing.T) {
	if os.Getenv("PEASANT_INTERRUPT_CHILD") != "1" {
		return
	}
	root := buildRootCommand()
	root.SetIn(os.Stdin)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.SetArgs(strings.Split(os.Getenv("PEASANT_INTERRUPT_ARGS"), "\n"))
	err := root.Execute()
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "harvest returned context.Canceled")
		os.Exit(23)
	}
	fmt.Fprintf(os.Stderr, "unexpected harvest result: %v\n", err)
	os.Exit(24)
}

func TestHarvestInterruptMounted(t *testing.T) {
	for _, c := range loadIngestProgressFixtures(t).Interrupt.Cases {
		t.Run(c.Name, func(t *testing.T) {
			source := testfixture.MaterializeByName(t, "native-v2-only")
			project := testfixture.PrepareNativeCLI(t, source)
			dir := t.TempDir()
			fifo := filepath.Join(dir, "blocked-git")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(dir, "git-ready")
			// Only shell builtins: cancellation kills the actual CommandContext
			// dependency, with no sleep process retaining its output pipes.
			// Scope the block to this source's project: startup Git probes must
			// finish before harvest registers its signal handler and renderer.
			git := "#!/bin/sh\n[ \"$1\" = -C ] && [ \"$2\" = \"$PEASANT_INTERRUPT_PROJECT\" ] || exit 1\nprintf ready > \"$PEASANT_INTERRUPT_READY\"\nread value < \"$PEASANT_INTERRUPT_FIFO\"\n"
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(git), 0o700); err != nil {
				t.Fatal(err)
			}
			args := []string{"harvest", "--source-provider=opencode", "--source-path=" + filepath.Dir(source.Path), "--output=" + filepath.Join(dir, "output"), "--data-dir=" + dir, "--config-dir=" + dir}
			if c.JSON {
				args = append(args, "--json")
			}
			child := exec.Command(os.Args[0], "-test.run=^TestHarvestInterruptProcess$")
			child.Env = append(os.Environ(), "PEASANT_INTERRUPT_CHILD=1", "PEASANT_INTERRUPT_ARGS="+strings.Join(args, "\n"), "PEASANT_INTERRUPT_READY="+ready, "PEASANT_INTERRUPT_FIFO="+fifo, "PATH="+dir+":"+os.Getenv("PATH"), "HOME="+dir, defaults.EnvXDGConfigHome.String()+"="+dir, defaults.EnvXDGDataHome.String()+"="+dir, "TERM=xterm-256color")
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			child.Env = append(child.Env, "PEASANT_INTERRUPT_PROJECT="+project, defaults.EnvXDGStateHome.String()+"="+dir, "XDG_CACHE_HOME="+dir)
			stdout := newSignalWriter("")
			stderr := newSignalWriter("ctrl+c to cancel")
			child.Stdout = stdout
			child.Stderr = stderr
			var master, terminal *os.File
			var before *term.State
			if c.Terminal {
				master, terminal = openTestTerminal(t)
				if err := unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 120}); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = term.GetState(int(terminal.Fd()))
				if err != nil {
					t.Fatal(err)
				}
				child.Stdin, child.Stderr = terminal, terminal
				copied := make(chan struct{})
				go func() { _, _ = io.Copy(stderr, master); close(copied) }()
				t.Cleanup(func() { _ = terminal.Close(); _ = master.Close(); <-copied })
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- child.Wait() }()
			waited := false
			t.Cleanup(func() {
				_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
				if !waited {
					<-done
				}
			})
			deadline := time.NewTimer(20 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				_, err := os.Stat(ready)
				if err == nil && (!c.Terminal || c.JSON || strings.Contains(stderr.String(), "ctrl+c to cancel")) {
					break
				}
				select {
				case err := <-done:
					waited = true
					t.Fatalf("harvest exited before blocked dependency and renderer readiness: %v\n%s\n%s", err, stdout.String(), stderr.String())
				case <-deadline.C:
					t.Fatalf("harvest did not reach blocked git and renderer readiness\n%s\n%s", stdout.String(), stderr.String())
				case <-tick.C:
				}
			}
			if c.Terminal {
				active, err := term.GetState(int(terminal.Fd()))
				if err != nil {
					t.Fatal(err)
				}
				if c.JSON && !reflect.DeepEqual(before, active) {
					t.Fatal("JSON harvest changed terminal mode")
				}
				if !c.JSON && reflect.DeepEqual(before, active) {
					t.Fatal("progress never entered raw terminal mode")
				}
			}
			var err error
			switch c.Interrupt {
			case harvestInterruptKey:
				_, err = master.Write([]byte{3})
			case harvestInterruptINT:
				err = child.Process.Signal(syscall.SIGINT)
			case harvestInterruptTERM:
				err = child.Process.Signal(syscall.SIGTERM)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				waited = true
			case <-time.After(10 * time.Second):
				t.Fatalf("interrupt did not stop real harvest\n%s\n%s", stdout.String(), stderr.String())
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("expected normal cancellation exit 23, not success, signal death, or another error: %v\n%s\n%s", err, stdout.String(), stderr.String())
			}
			if c.Terminal {
				after, err := term.GetState(int(terminal.Fd()))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("harvest exited without restoring terminal state")
				}
			}
			if output := stdout.String(); strings.Contains(output, "peasant harvest:") || strings.Contains(output, `"summary"`) || strings.Contains(output, "duration:") {
				t.Fatalf("interrupted harvest emitted a success/JSON result: %s", output)
			}
			if (!c.Terminal || c.JSON) && strings.Contains(stderr.String(), "\x1b[") {
				t.Fatalf("non-terminal harvest animated progress: %q", stderr.String())
			}
			if c.Terminal && !c.JSON && !strings.Contains(stderr.String(), "harvest canceled") {
				t.Fatalf("terminal cancellation lost final progress: %s", stderr.String())
			}
		})
	}
}
