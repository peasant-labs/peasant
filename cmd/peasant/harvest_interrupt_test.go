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

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harvest_interrupt.yaml
var harvestInterruptYAML []byte

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

func loadHarvestInterruptCases(t *testing.T) []harvestInterruptCase {
	t.Helper()
	var doc struct {
		Required []string               `yaml:"required_cases"`
		Cases    []harvestInterruptCase `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(harvestInterruptYAML))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("expected one interrupt fixture document: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("missing or duplicate interrupt case name: %q", c.Name)
		}
		seen[c.Name] = true
		switch c.Interrupt {
		case harvestInterruptKey:
			if !c.Terminal || c.JSON {
				t.Fatal("raw Ctrl+C requires animated terminal mode")
			}
		case harvestInterruptINT, harvestInterruptTERM:
		default:
			t.Fatalf("unknown interrupt %q", c.Interrupt)
		}
	}
	if len(doc.Required) == 0 {
		t.Fatal("interrupt fixture requires a named coverage manifest")
	}
	for _, name := range doc.Required {
		if !seen[name] {
			t.Fatalf("missing required interrupt case %q", name)
		}
	}
	return doc.Cases
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
	for _, c := range loadHarvestInterruptCases(t) {
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
		})
	}
}
