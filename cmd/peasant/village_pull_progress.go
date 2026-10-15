package main

import (
	"context"
	"fmt"
	"io"
	"os"

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

// collectivePullProgress is the stderr bar for a bulk pull. It renders the
// pullprogress surface after each completed transcript while the run
// proceeds. It is silent when stderr is not a terminal and under --json;
// the per-transcript lines and the closing summary keep printing either
// way because they travel on stdout.
type collectivePullProgress struct {
	w       io.Writer
	enabled bool
	model   pullprogress.Model
}

func newCollectivePullProgress(w io.Writer, jsonOutput bool, th theme.Theme) *collectivePullProgress {
	model := pullprogress.New(th)
	if width := collectivePullTerminalWidth(w); width > 0 {
		model.SetSize(width, 0)
	}
	return &collectivePullProgress{
		w:       w,
		enabled: !jsonOutput && isCollectivePullTTY(w),
		model:   model,
	}
}

// Enabled reports whether the bar writes. Tests assert the gating without
// needing a terminal: a buffer writer and --json both stay disabled.
func (p *collectivePullProgress) Enabled() bool { return p.enabled }

// SetTotal publishes the listed cohort size and renders the initial bar.
func (p *collectivePullProgress) SetTotal(total int) {
	p.model.SetTotal(total)
	if p.enabled {
		p.render()
	}
}

// Advance records one more completed transcript and re-renders the bar.
func (p *collectivePullProgress) Advance(done int) {
	p.model.SetDone(done)
	if p.enabled {
		p.render()
	}
}

// Cancel retains the interrupted final state and renders it once, so a
// terminal keeps the reached counts beside the cancellation report.
func (p *collectivePullProgress) Cancel() {
	p.model.MarkCanceled()
	if p.enabled {
		p.render()
	}
}

func (p *collectivePullProgress) render() {
	fmt.Fprintln(p.w, p.model.View())
}

// collectivePullCancellationError reports an interrupted bulk pull. Done
// counts completed transcripts; total is the listed cohort size (0 when
// the run stopped before the listing finished). The fix names the resume:
// current transcripts are skipped, so a re-run repeats only what is
// missing.
func collectivePullCancellationError(done, total int, names []string, ctxErr error) error {
	return fmt.Errorf(
		"cancelled after %d of %d transcripts in %s\n"+
			"  what: the bulk pull stopped before every transcript was pulled\n"+
			"  why:  %v\n"+
			"  where: collective pull loop\n"+
			"  fix:  re-run the same command — current transcripts are skipped, so only what is missing repeats",
		done, total, collectiveLabel(names), ctxErr)
}
