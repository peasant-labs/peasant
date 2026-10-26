package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/tui/pullprogress"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// collectivePuller is the narrow pull operation the bulk collective driver
// consumes. *pull.Pipeline satisfies it in production; tests wrap it to
// steer cancellation deterministically without timing.
type collectivePuller interface {
	PullTranscript(ctx context.Context, ref pull.TranscriptRef, opts pull.PullOptions) (*pull.PullResult, error)
}

var _ collectivePuller = (*pull.Pipeline)(nil)

// isCollectivePullTTY reports whether w is an interactive terminal. It
// follows the established stderr progress pattern: TTY detection runs on
// the writer itself, so pipes and CI buffers never receive redraws.
func isCollectivePullTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// collectivePullTerminalWidth returns the terminal width of w, or 0 when w
// is not a terminal or its size cannot be read. A 0 width makes the
// progress panel shrink to its widest line instead of stretching.
func collectivePullTerminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 {
		return 0
	}
	return width
}

// pullProgressTheme resolves the palette mode for the bulk-pull bar from
// the loaded config, defaulting to dark when no config names a theme. The
// bar renders in both themes; this only picks the production default.
func pullProgressTheme(cmd *cobra.Command) theme.Theme {
	if cfg, err := loadConfig(resolveConfigPath(cmd)); err == nil && cfg != nil {
		return theme.New(themeModeFor(cfg))
	}
	return theme.New(theme.ModeDark)
}

// collectivePullProgressFloor is the minimum gap between two stderr bar
// writes while transcripts keep landing. It follows the migrate progress
// tracker's floor: a bulk pull of hundreds of transcripts must not fill
// stderr with near-identical panels.
const collectivePullProgressFloor = 5 * time.Second

// collectivePullProgress is the stderr bar for a bulk pull. It renders the
// pullprogress surface while the run proceeds, throttled to
// collectivePullProgressFloor: the initial bar, a periodic refresh carrying
// the elapsed run time, and the final bar always render, while the advances
// between two refreshes only move the counts. Throttling instead of an
// in-place redraw is deliberate. The per-transcript lines travel on stdout,
// so both streams share the terminal: a cursor-up erase of the stderr panel
// would clobber stdout lines printed since the last render, and a Bubble Tea
// renderer (the ingest approach) would fight the same interleaving. It is
// silent when stderr is not a terminal and under --json; the per-transcript
// lines and the closing summary keep printing either way because they travel
// on stdout.
//
// Every write APPENDS. Nothing clears, erases, or repositions the panel, so the
// bar and its final footer are still on the terminal after the command exits:
// they are the ending state, and a user looking back after a cancelled run must
// be able to read what was reached.
type collectivePullProgress struct {
	// mu guards the surface against the interrupt watcher, which paints the
	// canceling state from its own goroutine while the loop is blocked in a pull.
	mu         sync.Mutex
	w          io.Writer
	enabled    bool
	model      pullprogress.Model
	started    time.Time
	lastRender time.Time
	// final is set once the cancellation is reported, so a watcher that wakes
	// late cannot repaint the intermediate state over the final one.
	final bool
	now   func() time.Time
}

func newCollectivePullProgress(w io.Writer, jsonOutput bool, th theme.Theme) *collectivePullProgress {
	return newCollectivePullProgressAt(w, jsonOutput, th, time.Now)
}

// newCollectivePullProgressAt is the clock-injectable constructor. Tests
// steer the throttle deterministically through now; production passes
// time.Now.
func newCollectivePullProgressAt(w io.Writer, jsonOutput bool, th theme.Theme, now func() time.Time) *collectivePullProgress {
	if now == nil {
		now = time.Now
	}
	model := pullprogress.New(th)
	if width := collectivePullTerminalWidth(w); width > 0 {
		model.SetSize(width, 0)
	}
	started := now()
	return &collectivePullProgress{
		w:       w,
		enabled: !jsonOutput && isCollectivePullTTY(w),
		model:   model,
		started: started,
		now:     now,
	}
}

// Enabled reports whether the bar writes. Tests assert the gating without
// needing a terminal: a buffer writer and --json both stay disabled.
func (p *collectivePullProgress) Enabled() bool { return p.enabled }

// SetTotal publishes the listed cohort size and renders the initial bar.
func (p *collectivePullProgress) SetTotal(total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.model.SetTotal(total)
	p.model.SetElapsed(p.sinceStart())
	if p.enabled {
		p.renderLocked()
	}
}

// Advance records one more completed transcript and refreshes the bar when
// the floor interval has passed. The final advance always renders, so a
// fast run still leaves its completed bar on stderr.
func (p *collectivePullProgress) Advance(done int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.model.SetDone(done)
	p.model.SetElapsed(p.sinceStart())
	if !p.enabled {
		return
	}
	if total := p.model.Total(); total > 0 && done >= total {
		p.renderLocked()
		return
	}
	if p.now().Sub(p.lastRender) >= collectivePullProgressFloor {
		p.renderLocked()
	}
}

// MarkCanceling paints the intermediate state as soon as an interrupt arrives,
// so the user learns the signal was received while the transcript in flight
// finishes. Without it the bar would sit on its last refresh for the whole
// unwind, which reads as an unresponsive command. It is safe to call from the
// watcher goroutine, and it yields to the final state once that is reported.
func (p *collectivePullProgress) MarkCanceling() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.final {
		return
	}
	p.model.SetElapsed(p.sinceStart())
	p.model.MarkCanceling()
	if p.enabled {
		p.renderLocked()
	}
}

// Cancel retains the interrupted final state and renders it once, so a
// terminal keeps the reached counts beside the cancellation report. A
// cancellation is a final state, never a throttled refresh.
func (p *collectivePullProgress) Cancel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.final {
		return
	}
	p.final = true
	p.model.SetElapsed(p.sinceStart())
	p.model.MarkCanceled()
	if p.enabled {
		p.renderLocked()
	}
}

// sinceStart reports how long the run has been going. A clock stepped
// backwards still renders a zero elapsed, never a negative one.
func (p *collectivePullProgress) sinceStart() time.Duration {
	elapsed := p.now().Sub(p.started)
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

// renderLocked writes one panel. The caller holds p.mu.
func (p *collectivePullProgress) renderLocked() {
	fmt.Fprintln(p.w, p.model.View())
	p.lastRender = p.now()
}

// collectivePullCancellationError reports an interrupted bulk pull. Done
// counts completed transcripts; total is the listed cohort size (0 when
// the run stopped before the listing finished). The fix names the resume:
// current transcripts are skipped, so a re-run repeats only what is
// missing.
func collectivePullCancellationError(done, total int, names []string, owner string, ctxErr error) error {
	return fmt.Errorf(
		"cancelled after %d of %d transcripts %s\n"+
			"  what: the bulk pull stopped before every transcript was pulled\n"+
			"  why:  %v\n"+
			"  where: collective pull loop\n"+
			"  fix:  re-run the same command — current transcripts are skipped, so only what is missing repeats",
		done, total, selectionLabel(names, owner), ctxErr)
}
