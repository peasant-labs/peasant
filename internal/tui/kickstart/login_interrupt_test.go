package kickstart_test

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

// blockingLogin returns a login runner that reports the context it was handed on
// started, then blocks until that context is canceled. It models the real
// failure: an interactive login the runner cannot complete, which must stay
// interruptible from the spinner.
func blockingLogin(started chan<- context.Context) func(context.Context, func(string)) (string, error) {
	return func(ctx context.Context, _ func(string)) (string, error) {
		started <- ctx
		<-ctx.Done()
		return "", ctx.Err()
	}
}

// runLoginChildren runs each child of the accept batch in its own goroutine so
// the blocking login runner does not stall the test. Results are discarded; the
// test observes the login through the context it captured.
func runLoginChildren(cmd tea.Cmd) {
	for _, child := range unwrapBatch(cmd) {
		if child == nil {
			continue
		}
		go child()
	}
}

func acceptConnectPrompt(t *testing.T, p kickstart.Program) (kickstart.Program, tea.Cmd) {
	t.Helper()
	p, _ = p.Update(press(tea.KeyLeft)) // move focus from the default "no" to "yes"
	p, cmd := p.Update(press(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("accepting the connect prompt produced no login command")
	}
	return p, cmd
}

func awaitContext(t *testing.T, ch <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-ch:
		return ctx
	case <-time.After(2 * time.Second):
		t.Fatal("login runner never started")
		return nil
	}
}

func requireCanceled(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not cancel the in-flight login context", what)
	}
}

// TestProgram_LoginSpinnerCtrlCCancelsAndQuits proves that while "connecting to
// village" is shown, ctrl+c cancels the in-flight login and quits kickstart. The
// pre-fix flow dropped every key here, trapping the user on the spinner.
func TestProgram_LoginSpinnerCtrlCCancelsAndQuits(t *testing.T) {
	t.Parallel()
	started := make(chan context.Context, 1)
	p, _ := newTestProgram(t, kickstart.ProgramDeps{Login: blockingLogin(started)})

	p, cmd := acceptConnectPrompt(t, p)
	runLoginChildren(cmd)
	loginCtx := awaitContext(t, started)

	if !strings.Contains(stripRender(p.View()), "connecting to village") {
		t.Fatal("accepting the connect prompt did not show the connecting spinner")
	}
	if !strings.Contains(stripRender(p.View()), "ctrl+c to quit") {
		t.Fatal("the connecting spinner did not advertise how to escape it")
	}

	p, quit := p.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	requireCanceled(t, loginCtx, "ctrl+c")
	if quit == nil || quit() != (tea.QuitMsg{}) {
		t.Fatal("ctrl+c on the connecting spinner did not quit kickstart")
	}
}

// TestProgram_LoginSpinnerEscCancelsAndReturns proves esc cancels the in-flight
// login and returns to the connect prompt, and that the canceled attempt's late
// result cannot then clobber the resumed prompt.
func TestProgram_LoginSpinnerEscCancelsAndReturns(t *testing.T) {
	t.Parallel()
	started := make(chan context.Context, 1)
	p, _ := newTestProgram(t, kickstart.ProgramDeps{Login: blockingLogin(started)})

	p, cmd := acceptConnectPrompt(t, p)
	runLoginChildren(cmd)
	loginCtx := awaitContext(t, started)

	p, _ = p.Update(press(tea.KeyEscape))
	requireCanceled(t, loginCtx, "esc")

	if p.Phase() != kickstart.PhaseOAuth {
		t.Fatalf("esc phase = %s, want the connect prompt (oauth)", p.Phase())
	}
	if p.Connected() {
		t.Fatal("esc must not mark the program connected")
	}
	if strings.Contains(stripRender(p.View()), "connecting to village") {
		t.Fatal("esc did not leave the connecting spinner")
	}

	// The canceled attempt's runner now returns; its stale result must be ignored,
	// leaving the user on a usable prompt rather than a false connected state.
	p, _ = p.Update(kickstart.LoginResultForTest("octocat", nil, 1))
	if p.Connected() {
		t.Fatal("a stale result from the canceled login marked the program connected")
	}
	if p.Phase() != kickstart.PhaseOAuth {
		t.Fatalf("after a stale result phase = %s, want the connect prompt (oauth)", p.Phase())
	}
}
