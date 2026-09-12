package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/tui/harvestprogress"
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
	Name        string               `yaml:"name"`
	Terminal    bool                 `yaml:"terminal"`
	JSON        bool                 `yaml:"json"`
	Interrupt   harvestInterruptKind `yaml:"interrupt"`
	DiffSession string               `yaml:"diff_session"`
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

type harvestCancelBoundary string

const (
	harvestCancelNone        harvestCancelBoundary = "none"
	harvestCancelStartup     harvestCancelBoundary = "startup"
	harvestCancelPipeline    harvestCancelBoundary = "pipeline"
	harvestCancelBeforeFinal harvestCancelBoundary = "before-final"
	harvestCancelWait        harvestCancelBoundary = "wait"
)

type ingestProgressFixtures struct {
	Estimate struct {
		Required []string `yaml:"required_cases"`
		Cases    []struct {
			Name          string `yaml:"name"`
			Key           bool   `yaml:"key"`
			External      bool   `yaml:"external"`
			Unavailable   bool   `yaml:"unavailable"`
			Expired       bool   `yaml:"expired"`
			DelayedCancel bool   `yaml:"delayed_cancel"`
			StopFallback  bool   `yaml:"stop_fallback"`
			Want          string `yaml:"want"`
		} `yaml:"cases"`
	} `yaml:"estimate"`
	Lifecycle struct {
		Required []string `yaml:"required_cases"`
		Cases    []struct {
			Name                string `yaml:"name"`
			Key                 bool   `yaml:"key"`
			External            bool   `yaml:"external"`
			DelayedNotification bool   `yaml:"delayed_notification"`
		} `yaml:"cases"`
	} `yaml:"lifecycle"`
	Execution struct {
		Required []string `yaml:"required_cases"`
		Cases    []struct {
			Name          string                `yaml:"name"`
			CancelAt      harvestCancelBoundary `yaml:"cancel_at"`
			RunError      bool                  `yaml:"run_error"`
			UIError       bool                  `yaml:"ui_error"`
			NilResult     bool                  `yaml:"nil_result"`
			SessionErrors int                   `yaml:"session_errors"`
			JSON          bool                  `yaml:"json"`
			Want          string                `yaml:"want"`
			WantError     string                `yaml:"want_error"`
			Contains      []string              `yaml:"contains"`
		} `yaml:"cases"`
	} `yaml:"execution"`
	CommandStartup struct {
		Required []string `yaml:"required_cases"`
		Cases    []struct {
			Name string `yaml:"name"`
			JSON bool   `yaml:"json"`
		} `yaml:"cases"`
	} `yaml:"command_startup"`
	FinalOverride struct {
		Required []string `yaml:"required_cases"`
		Cases    []struct {
			Name    string `yaml:"name"`
			Outcome string `yaml:"outcome"`
		} `yaml:"cases"`
	} `yaml:"final_override"`
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
		if c.DiffSession != "" {
			valid = valid && c.Interrupt == harvestInterruptKey
		}
		return c.Name, valid
	})
	validateNamedFixtures(t, "inline", doc.Inline.Required, len(doc.Inline.Cases), func(i int) (string, bool) {
		c := doc.Inline.Cases[i]
		return c.Name, c.Width > 0 && c.Height > 0 && len(c.Contains) > 0
	})
	validateNamedFixtures(t, "lifecycle", doc.Lifecycle.Required, len(doc.Lifecycle.Cases), func(i int) (string, bool) {
		c := doc.Lifecycle.Cases[i]
		return c.Name, !(c.Key && c.External) && (!c.DelayedNotification || c.External)
	})
	validateNamedFixtures(t, "execution", doc.Execution.Required, len(doc.Execution.Cases), func(i int) (string, bool) {
		c := doc.Execution.Cases[i]
		valid := false
		switch c.CancelAt {
		case harvestCancelNone, harvestCancelStartup, harvestCancelPipeline, harvestCancelBeforeFinal, harvestCancelWait:
			valid = true
		}
		return c.Name, valid && (c.Want == "succeeded" || c.Want == "failed" || c.Want == "canceled")
	})
	validateNamedFixtures(t, "command_startup", doc.CommandStartup.Required, len(doc.CommandStartup.Cases), func(i int) (string, bool) {
		return doc.CommandStartup.Cases[i].Name, true
	})
	validateNamedFixtures(t, "final_override", doc.FinalOverride.Required, len(doc.FinalOverride.Cases), func(i int) (string, bool) {
		c := doc.FinalOverride.Cases[i]
		return c.Name, c.Outcome == "succeeded" || c.Outcome == "failed"
	})
	return doc
}

func finalOutcome(canceled bool) ingestprogress.FinalOutcome {
	if canceled {
		return ingestprogress.FinalCanceled
	}
	return ingestprogress.FinalSucceeded
}

func finalMessage(at time.Time, state *ingest.ProgressState, err error) ingestprogress.FinalMsg {
	return ingestprogress.FinalMsg{At: at, Snapshot: state.Snapshot(), Outcome: finalOutcome(err != nil)}
}

func TestProgressModelCancellationRetainsEstimate(t *testing.T) {
	doc := loadIngestProgressFixtures(t)
	validateNamedFixtures(t, "estimate", doc.Estimate.Required, len(doc.Estimate.Cases), func(i int) (string, bool) {
		c := doc.Estimate.Cases[i]
		return c.Name, !(c.Key && c.External) && (!c.Expired || c.Unavailable) && c.Want != ""
	})
	for _, c := range doc.Estimate.Cases {
		t.Run(c.Name, func(t *testing.T) {
			started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			state := ingest.NewProgressState()
			state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageDiscover, Total: 4})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			renderer := newProgressProgram(io.Discard, state, nil, cancel)
			var model tea.Model = renderer.newModel(ctx, started)
			if !c.Unavailable || c.Expired {
				state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiscover, Done: 1, Total: 4})
			}
			model, _ = model.Update(harvestprogress.TickMsg(started.Add(2 * time.Second)))
			initialClock := "total elapsed: 2s"
			if c.Expired {
				if !strings.Contains(ansi.Strip(model.View().Content), "estimate: 6s") {
					t.Fatal("fixture did not establish a live estimate before the stall")
				}
				model, _ = model.Update(harvestprogress.TickMsg(started.Add(8 * time.Second)))
				initialClock = "total elapsed: 8s"
			}
			assertView := func(markers ...string) {
				t.Helper()
				view := ansi.Strip(model.View().Content)
				for _, marker := range append(markers, c.Want) {
					if !strings.Contains(view, marker) {
						t.Fatalf("missing %q in\n%s", marker, view)
					}
				}
				if !c.Unavailable && strings.Contains(view, "estimate unavailable") {
					t.Fatalf("lost estimate:\n%s", view)
				}
			}
			assertView(initialClock)
			if c.DelayedCancel || c.StopFallback {
				cancel()
			}
			// The operation can publish its failure before the renderer receives
			// any cancellation message. Capture the displayed estimate, not this snapshot.
			state.Update(ingest.ProgressEvent{Kind: ingest.KindEnd, Stage: ingest.StageDiscover, Done: 2, Total: 4, Err: context.Canceled})
			if c.DelayedCancel {
				// The tick is already queued while the context watcher has not
				// delivered its CancelMsg. Use the renderer's real model wiring.
				model, _ = model.Update(harvestprogress.TickMsg(started.Add(9 * time.Second)))
				assertView("canceling harvest", "✗ discover", "2/4", "total elapsed: 9s")
				initialClock = "total elapsed: 9s"
			}
			if c.Key {
				model, _ = model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			} else if c.External {
				model, _ = model.Update(harvestprogress.CancelMsg{})
			}
			if c.Key || c.External {
				assertView("canceling harvest", initialClock)
				model, _ = model.Update(harvestprogress.TickMsg(started.Add(9 * time.Second)))
				assertView("canceling harvest", "✗ discover", "2/4", "total elapsed: 9s")
				// Repeated notification must not capture the now failed/expired ETA.
				model, _ = model.Update(harvestprogress.CancelMsg{})
				model, _ = model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
				assertView("canceling harvest", "2/4")
			}
			model, _ = model.Update(ingestprogress.FinalMsg{At: started.Add(12 * time.Second), Snapshot: state.Snapshot(), Outcome: ingestprogress.FinalCanceled})
			assertView("harvest canceled", "✗ discover", "2/4", "total elapsed: 12s")
			wantStageClock := "12s"
			if c.Key || c.External {
				wantStageClock = "9s"
			}
			for _, line := range strings.Split(ansi.Strip(model.View().Content), "\n") {
				if strings.Contains(line, "✗ discover") {
					fields := strings.Fields(line)
					if fields[len(fields)-1] != wantStageClock {
						t.Fatalf("ended stage clock = %q, want %s", line, wantStageClock)
					}
				}
			}
			final := model.View().Content
			model, _ = model.Update(harvestprogress.TickMsg(started.Add(time.Hour)))
			if model.View().Content != final {
				t.Fatal("final snapshot changed after stop")
			}
		})
	}
}

// The operation deliberately withholds completion after acknowledging cancel.
// Raw terminal input and inherited cancellation must keep the same renderer alive.
type delayedNotificationContext struct {
	context.Context
	release <-chan struct{}
}

// Delay only the watcher's subscription; Err still reads the real operation.
func (c delayedNotificationContext) Done() <-chan struct{} {
	<-c.release
	return c.Context.Done()
}

var _ context.Context = delayedNotificationContext{}

// Pause the PTY consumer after cancellation acknowledgment, so program shutdown
// cannot accidentally stand in for capture completion just because reads are fast.
type pausedProgressCapture struct {
	*signalWriter
	resume <-chan struct{}
}

func (w *pausedProgressCapture) Write(p []byte) (int, error) {
	n, err := w.signalWriter.Write(p)
	if strings.Contains(w.String(), "waiting for current work to stop") {
		<-w.resume
	}
	return n, err
}

var _ io.Writer = (*pausedProgressCapture)(nil)

func TestProgressRendererCancellationAcknowledgment(t *testing.T) {
	for _, c := range loadIngestProgressFixtures(t).Lifecycle.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := ingest.NewProgressState()
			state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageDiff, Total: 10})
			state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiff, Done: 4, Total: 10})
			out := newSignalWriter("ctrl+c to cancel")
			r := newProgressProgram(out, state, nil, cancel)
			r.isTTY = true
			master, terminal := openTestTerminal(t)
			if err := unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
				t.Fatal(err)
			}
			before, err := term.GetState(int(terminal.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer terminal.Close()
			r.input = terminal
			r.w = terminal
			resumeCapture := make(chan struct{})
			releaseCapture := sync.OnceFunc(func() { close(resumeCapture) })
			capture := &pausedProgressCapture{signalWriter: out, resume: resumeCapture}
			copied := make(chan struct{})
			var copyErr error
			go func() { _, copyErr = io.Copy(capture, master); close(copied) }()
			defer func() { releaseCapture(); _ = terminal.Close(); _ = master.Close(); <-copied }()
			notification := make(chan struct{})
			releaseNotification := sync.OnceFunc(func() { close(notification) })
			var operation context.Context = ctx
			if c.DelayedNotification {
				operation = delayedNotificationContext{Context: ctx, release: notification}
			}
			go r.Run(operation)
			done := make(chan struct{})
			go func() { r.Wait(); close(done) }()
			defer func() { releaseNotification(); r.Finish(finalMessage(time.Now(), state, ctx.Err())); <-done }()
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
				if c.DelayedNotification {
					state.Update(ingest.ProgressEvent{Kind: ingest.KindEnd, Stage: ingest.StageDiff, Done: 4, Total: 10, Err: context.Canceled})
				}
			}
			if c.Key || c.External {
				// The diff renderer reuses the initial 'c' from the old hint.
				waitFor("waiting for current work to stop")
				// The mounted renderer must recognize cancellation from ticks
				// before its separate notification watcher is allowed to proceed.
				releaseNotification()
				if ctx.Err() != context.Canceled {
					t.Fatal("key did not cancel operation")
				}
				select {
				case <-done:
					t.Fatal("renderer quit before operation acknowledged cancellation")
				default:
				}
			}
			r.Finish(finalMessage(time.Now(), state, ctx.Err()))
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("renderer did not stop after completion")
			}
			if err := r.Err(); err != nil {
				t.Fatal(err)
			}
			after, err := term.GetState(int(terminal.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("progress program exited without restoring terminal state")
			}
			// Wait joins the program, not our independent PTY consumer. Close only
			// the slave after terminal restoration, then drain through EOF/EIO before
			// inspecting output. Closing the master here could discard queued bytes.
			if err := terminal.Close(); err != nil {
				t.Fatal(err)
			}
			releaseCapture()
			select {
			case <-copied:
			case <-time.After(5 * time.Second):
				t.Fatal("PTY capture did not drain after progress program shutdown and slave close")
			}
			// Linux reports EIO on a PTY master once its last slave closes.
			if copyErr != nil && !errors.Is(copyErr, syscall.EIO) {
				t.Fatalf("read progress program output through PTY shutdown: %v", copyErr)
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

type controlledHarvestPipeline struct {
	cancel     context.CancelFunc
	err        error
	runs       int
	ready      <-chan struct{}
	result     *ingest.PipelineResult
	returnedAt time.Time
}

func (p *controlledHarvestPipeline) Run(context.Context) (*ingest.PipelineResult, error) {
	p.runs++
	<-p.ready
	if p.cancel != nil {
		p.cancel()
	}
	p.returnedAt = time.Now()
	return p.result, p.err
}

type harvestModelUpdate struct {
	msg      tea.Msg
	response chan harvestModelResponse
}

type harvestModelResponse struct {
	view string
	cmd  tea.Cmd
}

type controlledHarvestProgram struct {
	finishEntered chan ingestprogress.FinalMsg
	releaseFinish chan struct{}
	waitEntered   chan struct{}
	releaseWait   chan struct{}
	ready         chan struct{}
	exited        chan struct{}
	updates       chan harvestModelUpdate
	done          chan struct{}
	renderer      *progressProgram
	err           error
	runs          int
}

func newControlledHarvestProgram(state *ingest.ProgressState, cancel context.CancelFunc) *controlledHarvestProgram {
	return &controlledHarvestProgram{
		finishEntered: make(chan ingestprogress.FinalMsg, 1), releaseFinish: make(chan struct{}),
		waitEntered: make(chan struct{}), releaseWait: make(chan struct{}),
		ready: make(chan struct{}), exited: make(chan struct{}),
		updates: make(chan harvestModelUpdate), done: make(chan struct{}),
		renderer: newProgressProgram(io.Discard, state, nil, cancel),
	}
}

// Substitute only the runtime scheduling: mount the production model factory
// with the same source/context as the runner, and serialize all root updates.
func (p *controlledHarvestProgram) Run(ctx context.Context) {
	p.runs++
	var model tea.Model = p.renderer.newModel(ctx, time.Now().Add(-time.Second))
	close(p.ready)
	defer close(p.exited)
	for {
		select {
		case update := <-p.updates:
			var cmd tea.Cmd
			model, cmd = model.Update(update.msg)
			update.response <- harvestModelResponse{view: ansi.Strip(model.View().Content), cmd: cmd}
		case <-p.done:
			return
		}
	}
}

func (p *controlledHarvestProgram) update(msg tea.Msg) harvestModelResponse {
	response := make(chan harvestModelResponse, 1)
	p.updates <- harvestModelUpdate{msg: msg, response: response}
	return <-response
}

func (p *controlledHarvestProgram) Finish(msg ingestprogress.FinalMsg) {
	p.finishEntered <- msg
	<-p.releaseFinish
	p.update(msg)
}
func (p *controlledHarvestProgram) Wait() {
	close(p.waitEntered)
	<-p.releaseWait
	close(p.done)
	<-p.exited
}
func (p *controlledHarvestProgram) Err() error { return p.err }

var _ harvestPipeline = (*controlledHarvestPipeline)(nil)
var _ harvestProgressProgram = (*controlledHarvestProgram)(nil)

func TestExecuteHarvestCommitsOutcomeBeforeFinalDelivery(t *testing.T) {
	for _, c := range loadIngestProgressFixtures(t).Execution.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := ingest.NewProgressState()
			state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageDiff, Total: 10})
			state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiff, Done: 4, Total: 10})
			committedSnapshot := state.Snapshot()
			payload := &ingest.PipelineResult{Duration: 2300 * time.Millisecond}
			payload.Summary.New = 7
			payload.Summary.Errors = c.SessionErrors
			program := newControlledHarvestProgram(state, cancel)
			pipeline := &controlledHarvestPipeline{ready: program.ready, result: payload}
			if c.NilResult {
				pipeline.result = nil
			}
			if c.RunError {
				pipeline.err = errors.New("pipeline failed")
			}
			if c.UIError {
				program.err = errors.New("terminal write failed")
			}
			if c.CancelAt == harvestCancelPipeline {
				pipeline.cancel = cancel
			}
			if c.CancelAt == harvestCancelStartup {
				cancel()
			}
			result := make(chan harvestExecution, 1)
			startedAt := time.Now()
			// The caller now starts the renderer; executeHarvest no longer does.
			if c.CancelAt != harvestCancelStartup {
				go program.Run(ctx)
			}
			go func() { result <- executeHarvest(ctx, pipeline, state, program) }()
			var final ingestprogress.FinalMsg
			var commitObservedAt time.Time
			if c.CancelAt != harvestCancelStartup {
				releaseFinish := sync.OnceFunc(func() { close(program.releaseFinish) })
				releaseWait := sync.OnceFunc(func() { close(program.releaseWait) })
				t.Cleanup(func() { releaseFinish(); releaseWait() })
				final = awaitHarvestEvent(t, program.finishEntered)
				commitObservedAt = time.Now()
				if final.At.Before(pipeline.returnedAt) || final.At.After(commitObservedAt) || !reflect.DeepEqual(final.Snapshot, committedSnapshot) {
					t.Fatalf("final did not capture completion time/progress: %+v", final)
				}
				// Change the shared source after the commit, while final delivery
				// is held. Neither outcome snapshot may follow these later counts.
				state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiff, Done: 9, Total: 10})
				if c.CancelAt == harvestCancelBeforeFinal {
					program.update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
					pending := program.update(harvestprogress.TickMsg(final.At.Add(time.Second)))
					if ctx.Err() != context.Canceled || !strings.Contains(pending.view, "canceling harvest") || !strings.Contains(pending.view, "9/10") {
						t.Fatalf("late key/tick did not reach mounted root/shared source: %s", pending.view)
					}
				}
				releaseFinish()
				awaitHarvestEvent(t, program.waitEntered)
				if c.CancelAt == harvestCancelWait {
					// This order is deliberately after final delivery, while Wait
					// still holds the command. External cancellation reaches ctx too.
					cancel()
					late := program.update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
					if late.cmd != nil || late.view != "" {
						t.Fatalf("final root accepted late key: %+v", late)
					}
				}
				frozen := program.update(harvestprogress.TickMsg(final.At.Add(time.Hour)))
				if frozen.cmd != nil {
					t.Fatal("final root scheduled another tick")
				}
				if c.Want == "canceled" {
					if !strings.Contains(frozen.view, "harvest canceled") || !strings.Contains(frozen.view, "4/10") || strings.Contains(frozen.view, "9/10") {
						t.Fatalf("canceled root lost committed snapshot: %s", frozen.view)
					}
				} else if frozen.view != "" {
					t.Fatalf("committed success/failure did not clear root: %s", frozen.view)
				}
				releaseWait()
			}
			execution := awaitHarvestEvent(t, result)
			wantKind := harvestCompletionSucceeded
			wantFinal := ingestprogress.FinalSucceeded
			switch c.Want {
			case "canceled":
				wantKind, wantFinal = harvestCompletionCanceled, ingestprogress.FinalCanceled
			case "failed":
				wantKind, wantFinal = harvestCompletionFailed, ingestprogress.FinalFailed
			}
			if execution.kind != wantKind {
				t.Fatalf("execution kind = %v, want %v", execution.kind, wantKind)
			}
			if !reflect.DeepEqual(execution.snapshot, committedSnapshot) || execution.at.Before(startedAt) {
				t.Fatalf("execution lost committed snapshot/time: %+v", execution)
			}
			if c.CancelAt == harvestCancelStartup {
				if pipeline.runs != 0 || program.runs != 0 || execution.result != nil || execution.runErr != nil || execution.uiErr != nil {
					t.Fatalf("startup cancellation launched work: pipeline=%d program=%d execution=%+v", pipeline.runs, program.runs, execution)
				}
			} else if final.Outcome != wantFinal {
				t.Fatalf("execution/final = %v/%v, want %v/%v", execution.kind, final.Outcome, wantKind, wantFinal)
			} else if pipeline.runs != 1 || program.runs != 1 || execution.result != pipeline.result || execution.runErr != pipeline.err || execution.uiErr != program.err || !execution.at.Equal(final.At) || execution.at.After(commitObservedAt) {
				t.Fatalf("execution lost committed payload/errors/time: %+v", execution)
			}
			var wantContextError error
			if c.Want == "canceled" {
				wantContextError = context.Canceled
			}
			if execution.ctxErr != wantContextError {
				t.Fatalf("sampled context error = %v, want %v", execution.ctxErr, wantContextError)
			}
			root := buildRootCommand()
			cmd, _, err := root.Find([]string{"harvest"})
			if err != nil {
				t.Fatal(err)
			}
			cmd.SetContext(ctx)
			var output bytes.Buffer
			cmd.SetOut(&output)
			err = outputHarvest(cmd, execution, harvestOutputOptions{flags: &harvestFlags{jsonOutput: c.JSON}})
			var wantError error
			switch c.WantError {
			case "":
			case "context":
				wantError = context.Canceled
			case "pipeline":
				wantError = pipeline.err
			case "ui":
				wantError = program.err
			case "sessions":
				if err == nil || err.Error() != fmt.Sprintf("%d session(s) failed", c.SessionErrors) {
					t.Fatalf("session failure = %v", err)
				}
				wantError = err
			default:
				t.Fatalf("unsupported output error %q", c.WantError)
			}
			if !errors.Is(err, wantError) {
				t.Fatalf("output error = %v, want %v", err, wantError)
			}
			if cmd.SilenceUsage != (c.WantError == "context" || c.WantError == "ui") {
				t.Fatalf("unexpected usage suppression for %v", err)
			}
			if len(c.Contains) == 0 && output.Len() != 0 {
				t.Fatalf("unexpected summary/JSON: %s", output.String())
			}
			for _, marker := range c.Contains {
				if !strings.Contains(output.String(), marker) {
					t.Fatalf("missing %q in %s", marker, output.String())
				}
			}
		})
	}
}

func awaitHarvestEvent[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("harvest lifecycle did not reach expected dependency boundary")
		var zero T
		return zero
	}
}

func TestHarvestCommandCanceledBeforeSetup(t *testing.T) {
	for _, c := range loadIngestProgressFixtures(t).CommandStartup.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			root := buildRootCommand()
			args := []string{"--config-dir", filepath.Join(dir, "config"), "--data-dir", filepath.Join(dir, "data"), "--state-dir", filepath.Join(dir, "state"), "harvest", "--output", filepath.Join(dir, "output")}
			if c.JSON {
				args = append(args, "--json")
			}
			root.SetArgs(args)
			var output, diagnostic bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&diagnostic)
			originalHandler := slog.Default().Handler()
			err := root.ExecuteContext(ctx)
			if !errors.Is(err, context.Canceled) || output.Len() != 0 {
				t.Fatalf("pre-canceled command = %v, output %q", err, output.String())
			}
			if strings.Contains(diagnostic.String(), "local import progress") || strings.Contains(diagnostic.String(), "ctrl+c to cancel") {
				t.Fatalf("pre-canceled command mounted progress: %s", diagnostic.String())
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("pre-canceled command created setup files: %v", entries)
			}
			if slog.Default().Handler() != originalHandler {
				t.Fatal("pre-canceled command changed logger")
			}
		})
	}
}

func TestProgressModelSuccessClears(t *testing.T) {
	m := harvestprogress.New(harvestprogress.Options{Progress: ingest.NewProgressState(), Theme: theme.New(theme.ModeDark), StartedAt: time.Now()})
	updated, cmd := m.Update(ingestprogress.FinalMsg{At: time.Now(), Snapshot: ingest.NewProgressState().Snapshot(), Outcome: ingestprogress.FinalSucceeded})
	if cmd == nil || updated.View().Content != "" {
		t.Fatal("success must clear the live view")
	}
}

func TestProgressModelAuthoritativeFinalOverridesPendingCancellation(t *testing.T) {
	for _, c := range loadIngestProgressFixtures(t).FinalOverride.Cases {
		t.Run(c.Name, func(t *testing.T) {
			outcome := ingestprogress.FinalSucceeded
			if c.Outcome == "failed" {
				outcome = ingestprogress.FinalFailed
			}
			started := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
			state := ingest.NewProgressState()
			state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageDiff, Total: 10})
			model := harvestprogress.New(harvestprogress.Options{Progress: state, Theme: theme.New(theme.ModeDark), StartedAt: started})
			updated, _ := model.Update(harvestprogress.CancelMsg{})
			finalSnapshot := state.Snapshot()
			updated, cmd := updated.Update(ingestprogress.FinalMsg{At: started.Add(3 * time.Second), Snapshot: finalSnapshot, Outcome: outcome})
			if cmd == nil || updated.View().Content != "" {
				t.Fatalf("outcome %v did not clear pending cancellation", outcome)
			}
			late, lateCmd := updated.Update(harvestprogress.CancelMsg{})
			if lateCmd != nil || late.View().Content != "" {
				t.Fatalf("outcome %v changed after final", outcome)
			}
		})
	}
}

func TestProgressModelCancellationFreezesFinalSnapshot(t *testing.T) {
	started := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	state := ingest.NewProgressState()
	state.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageDiff, Total: 10})
	m := harvestprogress.New(harvestprogress.Options{Progress: state, Theme: theme.New(theme.ModeDark), StartedAt: started})
	updated, _ := m.Update(harvestprogress.CancelMsg{})
	state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiff, Done: 4, Total: 10})
	updated, cmd := updated.Update(ingestprogress.FinalMsg{At: started.Add(8 * time.Second), Snapshot: state.Snapshot(), Outcome: ingestprogress.FinalCanceled})
	if cmd == nil {
		t.Fatal("acknowledged cancellation did not quit")
	}
	view := updated.View().Content
	if !strings.Contains(view, "4/10") || !strings.Contains(view, "total elapsed: 8s") || !strings.Contains(view, "harvest canceled") {
		t.Fatalf("final snapshot omitted last operation progress or clock: %s", view)
	}
	state.Update(ingest.ProgressEvent{Kind: ingest.KindAdvance, Stage: ingest.StageDiff, Done: 9, Total: 10})
	updated, _ = updated.Update(harvestprogress.TickMsg(started.Add(time.Minute)))
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
	state := ingest.NewProgressState()
	r := newProgressProgram(&output, state, nil, cancel)
	r.isTTY = true
	r.input = terminal
	go r.Run(ctx)
	done := make(chan struct{})
	go func() { r.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.Finish(finalMessage(time.Now(), state, context.Canceled))
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
	r := newProgressProgram(&buf, state, nil)

	ctx, cancel := context.WithCancel(context.Background())

	// Run() is launched in a goroutine (per the usage contract documented in
	// the progressProgram struct comment). newProgressProgram pre-increments
	// the WaitGroup so Wait() is safe immediately after go r.Run(ctx).
	go r.Run(ctx)

	// Acknowledge completion immediately, even if cancellation preceded startup.
	cancel()
	r.Finish(finalMessage(time.Now(), state, context.Canceled))
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
	r := newProgressProgram(&buf, state, nil)
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

	model := harvestprogress.New(harvestprogress.Options{Progress: state, Theme: theme.New(theme.ModeDark), StartedAt: time.Now()})
	out := model.View().Content
	if out == "" {
		t.Fatal("render() wrote nothing, want non-empty output")
	}
	// The output should contain the stage name.
	if !strings.Contains(strings.ToLower(out), strings.ToLower(ingest.StageDiscover.String())) {
		t.Errorf("render() output %q does not contain stage name %q", out, ingest.StageDiscover.String())
	}
}

func TestProgressRendererUsesGentleFrameRate(t *testing.T) {
	if progressProgramFPS != 24 {
		t.Fatalf("progressProgramFPS = %d, want 24", progressProgramFPS)
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
	r := newProgressProgram(&buf, state, nil)
	// Force TTY mode to exercise the tick path.
	r.isTTY = true

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)

	// Allow a couple of ticks (100 ms each) so the ticker fires at least once.
	time.Sleep(250 * time.Millisecond)
	cancel()
	r.Finish(finalMessage(time.Now(), state, context.Canceled))
	r.Wait()

	// At least one Bubble Tea render should have occurred before shutdown.
	if buf.Len() == 0 {
		t.Error("TTY renderer wrote 0 bytes after ticks + cancel, want some output")
	}
}

func TestProgressModelControlCCancelsPipeline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := harvestprogress.New(harvestprogress.Options{Progress: ingest.NewProgressState(), Theme: theme.New(theme.ModeDark), StartedAt: time.Now(), Cancel: cancel})

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
	updated, cmd = updated.Update(ingestprogress.FinalMsg{At: time.Now(), Snapshot: ingest.NewProgressState().Snapshot(), Outcome: ingestprogress.FinalCanceled})
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
			model := harvestprogress.New(harvestprogress.Options{Progress: state, Animation: animation.IngestAnimation(), Theme: theme.New(mode), StartedAt: started})
			updated, _ := model.Update(tea.WindowSizeMsg{Width: c.Width, Height: c.Height})
			updated, _ = updated.Update(harvestprogress.TickMsg(started.Add(8 * time.Second)))
			if c.Cancel {
				updated, _ = updated.Update(harvestprogress.CancelMsg{})
			}
			if c.Stop {
				updated, _ = updated.Update(ingestprogress.FinalMsg{At: started.Add(8 * time.Second), Snapshot: state.Snapshot(), Outcome: finalOutcome(c.Cancel)})
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
			var metadataWriter *os.File
			if c.DiffSession != "" {
				fifo = filepath.Join(dir, "output", "synthetic-host", "parent", "subagents", c.DiffSession, c.DiffSession+defaults.MetadataSuffix)
				if err := os.MkdirAll(filepath.Dir(fifo), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(fifo, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if metadataWriter != nil {
						_ = metadataWriter.Close()
					}
				})
			}
			// Only shell builtins: cancellation kills the actual CommandContext
			// dependency, with no sleep process retaining its output pipes.
			// Scope the block to this source's project: startup Git probes must
			// finish before harvest registers its signal handler and renderer.
			git := "#!/bin/sh\n[ \"$1\" = -C ] && [ \"$2\" = \"$PEASANT_INTERRUPT_PROJECT\" ] || exit 1\nprintf ready > \"$PEASANT_INTERRUPT_READY\"\nread value < \"$PEASANT_INTERRUPT_FIFO\"\n"
			if c.DiffSession != "" {
				git = "#!/bin/sh\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(git), 0o700); err != nil {
				t.Fatal(err)
			}
			args := []string{"harvest", "--source-harness=opencode", "--source-path=" + filepath.Dir(source.Path), "--output=" + filepath.Join(dir, "output"), "--data-dir=" + dir, "--config-dir=" + dir}
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
				if c.DiffSession != "" {
					// A nonblocking writer opens only once the real DIFF ReadFile
					// is waiting at the synthetic metadata FIFO. No timing-sized tree.
					if metadataWriter == nil {
						fd, openErr := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
						if openErr == nil {
							metadataWriter = os.NewFile(uintptr(fd), fifo)
						}
						if openErr != nil && !errors.Is(openErr, unix.ENXIO) {
							t.Fatal(openErr)
						}
					}
					if metadataWriter != nil && strings.Contains(ansi.Strip(stderr.String()), "0/1") {
						err = nil
					}
				}
				if err == nil && (!c.Terminal || c.JSON || strings.Contains(stderr.String(), "ctrl+c to cancel")) {
					break
				}
				select {
				case err := <-done:
					waited = true
					t.Fatalf("harvest exited before blocked dependency and renderer readiness: %v\n%s\n%s", err, stdout.String(), stderr.String())
				case <-deadline.C:
					t.Fatalf("harvest did not reach blocked dependency and renderer readiness\n%s\n%s", stdout.String(), stderr.String())
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
			if c.DiffSession != "" {
				// Cancellation cannot interrupt an in-flight OS read. Retain the
				// renderer until the dependency returns, then require bounded exit
				// without another key. Backend tests cover cancellation between Stats.
				ack := time.NewTimer(5 * time.Second)
				defer ack.Stop()
				for !strings.Contains(stderr.String(), "waiting for current work to stop") {
					select {
					case <-ack.C:
						t.Fatalf("DIFF did not acknowledge first key: %s", stderr.String())
					case err := <-done:
						waited = true
						t.Fatalf("harvest unmounted before DIFF dependency returned: %v", err)
					case <-tick.C:
					}
				}
				if err := metadataWriter.Close(); err != nil {
					t.Fatal(err)
				}
				metadataWriter = nil
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
			if c.DiffSession != "" {
				// This checks the PTY transcript, not an emulated final screen;
				// the frozen model and mounted screenshot tests check final layout.
				plain := ansi.Strip(stderr.String())
				if !strings.Contains(plain, "diff") || !strings.Contains(plain, "0/1") {
					t.Fatalf("canceled DIFF lost its incomplete snapshot: %s", plain)
				}
			}
		})
	}
}
