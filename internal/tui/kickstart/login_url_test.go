package kickstart_test

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

// urlReportingLogin returns a login runner that reports url via onURL (exactly
// as the real internal/auth runner does, before it blocks on the OAuth
// callback), then reports the context it was handed on started and blocks
// until that context is canceled. It models the real seam end-to-end without
// touching the network or the browser.
func urlReportingLogin(started chan<- context.Context, url string) func(context.Context, func(string)) (string, error) {
	return func(ctx context.Context, onURL func(string)) (string, error) {
		if onURL != nil {
			onURL(url)
		}
		started <- ctx
		<-ctx.Done()
		return "", ctx.Err()
	}
}

// TestProgram_ConnectingSpinnerShowsReportedLoginURL proves the "connecting to
// village" spinner renders the exact login URL the injected runner reports via
// onURL — the user's manual fallback when no browser appears (or the wrong
// profile opens) while kickstart's alt-screen swallows stderr.
func TestProgram_ConnectingSpinnerShowsReportedLoginURL(t *testing.T) {
	t.Parallel()
	const wantURL = "https://village.example.test/api/v1/auth/cli/login?port=54321&state=deadbeef"
	started := make(chan context.Context, 1)
	p, _ := newTestProgram(t, kickstart.ProgramDeps{Login: urlReportingLogin(started, wantURL)})

	p, cmd := acceptConnectPrompt(t, p)
	children := unwrapBatch(cmd)
	if len(children) < 2 {
		t.Fatalf("connect-accept batch has %d children, want at least the login runner and its URL delivery", len(children))
	}

	// The login runner itself blocks until canceled below; run it in its own
	// goroutine (mirrors login_interrupt_test.go's blockingLogin usage) so the
	// test can synchronously await its URL delivery command next.
	go children[0]()
	loginCtx := awaitContext(t, started)

	// waitForLoginURL resolves as soon as the runner's onURL call lands on its
	// channel — synchronous from the test's perspective since onURL already
	// happened before started was fed above.
	urlMsg := children[1]()
	if urlMsg == nil {
		t.Fatal("login-URL delivery command produced no message even though the runner reported a URL")
	}
	p, _ = p.Update(urlMsg)

	view := stripRender(p.View())
	if !strings.Contains(view, wantURL) {
		t.Fatalf("connecting spinner view does not contain the reported login URL %q:\n%s", wantURL, view)
	}
	if !strings.Contains(view, "connecting to village") {
		t.Fatalf("connecting spinner view lost its label once a URL was shown:\n%s", view)
	}
	if !strings.Contains(view, "ctrl+c to quit") {
		t.Fatalf("connecting spinner with a reported URL lost its cancel/quit hint:\n%s", view)
	}

	// Clean up: cancel the in-flight attempt exactly like the interruptible-spinner
	// tests do, so the blocking runner goroutine above does not leak past the test.
	p, quit := p.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	requireCanceled(t, loginCtx, "test cleanup ctrl+c")
	if quit == nil || quit() != (tea.QuitMsg{}) {
		t.Fatal("cleanup ctrl+c did not quit kickstart")
	}
}

// TestProgram_StaleLoginURLIsIgnored proves a URL report tagged with a
// superseded epoch (e.g. from a canceled attempt whose goroutine reports late)
// never appears under a later prompt, mirroring staleLogin's guarantee for
// loginDoneMsg.
func TestProgram_StaleLoginURLIsIgnored(t *testing.T) {
	t.Parallel()
	started := make(chan context.Context, 1)
	p, _ := newTestProgram(t, kickstart.ProgramDeps{
		Login: urlReportingLogin(started, "https://village.example.test/current"),
	})

	p, cmd := acceptConnectPrompt(t, p)
	runLoginChildren(cmd)
	_ = awaitContext(t, started)

	// A stale report (epoch 0, before the first real attempt's epoch of 1) must
	// be dropped rather than shown.
	p, _ = p.Update(kickstart.LoginURLForTest("https://village.example.test/stale", 0))

	view := stripRender(p.View())
	if strings.Contains(view, "stale") {
		t.Fatalf("a stale-epoch login URL was rendered:\n%s", view)
	}
}
