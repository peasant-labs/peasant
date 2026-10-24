package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/spf13/cobra"
)

// TestCollectivePullProgressMarkCancelingKeepsBothPanels proves the interrupt
// paints its own panel and that the final panel lands after it without erasing
// it. The ending state has to stay readable: a user who looks back after a
// cancelled run reads what was reached from these panels.
func TestCollectivePullProgressMarkCancelingKeepsBothPanels(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	clock := &progressTestClock{now: time.Now()}
	progress := newCollectivePullProgressAt(&stderr, false, theme.New(theme.ModeDark), clock.Now)
	progress.enabled = true
	progress.SetTotal(4)
	progress.Advance(1)

	clock.Advance(2 * time.Second)
	progress.MarkCanceling()
	if got := stderr.String(); !strings.Contains(got, "canceling pull...") {
		t.Fatalf("the interrupt must paint the canceling footer; got %q", got)
	}

	clock.Advance(3 * time.Second)
	progress.Cancel()
	final := stderr.String()
	if !strings.Contains(final, "pull canceled") {
		t.Errorf("the final state must paint the canceled footer; got %q", final)
	}
	if !strings.Contains(final, "canceling pull...") {
		t.Errorf("the canceling panel must remain on the terminal, not be erased; got %q", final)
	}
	if strings.Index(final, "canceling pull...") > strings.Index(final, "pull canceled") {
		t.Errorf("the canceled footer must land after the canceling one; got %q", final)
	}
	if !strings.Contains(final, "1/4") || !strings.Contains(final, "elapsed: 5s") {
		t.Errorf("the final panel must keep the reached count and the elapsed run time; got %q", final)
	}
}

// TestCollectivePullProgressLateWatcherDoesNotRepaint proves the guard between
// the interrupt watcher and the run's own unwind: once the cancellation is
// reported, a watcher that wakes late cannot paint the intermediate state over
// the final one.
func TestCollectivePullProgressLateWatcherDoesNotRepaint(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	clock := &progressTestClock{now: time.Now()}
	progress := newCollectivePullProgressAt(&stderr, false, theme.New(theme.ModeDark), clock.Now)
	progress.enabled = true
	progress.SetTotal(4)
	progress.Advance(1)
	progress.Cancel()

	reported := stderr.String()
	progress.MarkCanceling()
	if got := stderr.String(); got != reported {
		t.Errorf("a watcher waking after the final state must not repaint; before=%q after=%q", reported, got)
	}
}

// cancelWhileInFlightPuller cancels the run during a pull and holds that pull
// open until the test releases it. The held window is the one the interrupt
// watcher paints in, so a cancellation footer observed during it cannot have
// come from the run's own unwind.
type cancelWhileInFlightPuller struct {
	inner   collectivePuller
	cancel  context.CancelFunc
	release <-chan struct{}
}

func (p *cancelWhileInFlightPuller) PullTranscript(ctx context.Context, ref pull.TranscriptRef, opts pull.PullOptions) (*pull.PullResult, error) {
	p.cancel()
	<-p.release
	return p.inner.PullTranscript(ctx, ref, opts)
}

// testVillagePullCollectiveCancelPaintsWhileInFlight proves the cancellation
// state is a production state rather than a fixture-only one: on a terminal,
// the interrupt paints "canceling pull..." while the transcript in flight is
// still running, and the final "pull canceled" panel lands after it and erases
// neither.
func testVillagePullCollectiveCancelPaintsWhileInFlight(t *testing.T) {
	t.Helper()
	master, terminal := openTestTerminal(t)
	setTestTerminalSize(t, terminal, 24, 80)
	out := newSignalWriter("canceling pull...")
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(out, master); close(copied) }()
	t.Cleanup(func() { _ = terminal.Close(); _ = master.Close(); <-copied })

	dir := t.TempDir()
	server := newPullCollectiveDouble(t)
	ids := pullCollectiveTestIDs(3)
	seedPullTranscripts(t, server, ids, pullCollectiveAID)

	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	client := village.NewVillageClient(server.URL(), "test-api-key", nil)
	pullsRoot := string(defaults.ResolveVillagePullsDirPathWith(dir))
	realPipeline := pull.NewPipeline(client, pull.NewFileSystem(pullsRoot), db,
		pull.SystemClock{},
		pull.Credentials{UserID: testutil.TestOwnerUserID, VillageURL: server.URL()},
		pullsRoot)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	stub := &cancelWhileInFlightPuller{inner: realPipeline, cancel: cancel, release: release}

	cmd := &cobra.Command{Use: "pull"}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(terminal)
	cmd.SetContext(ctx)

	runErr := make(chan error, 1)
	go func() {
		runErr <- runVillageTranscriptsPullCollectives(cmd, db, client, stub,
			[]string{pullCollectiveAName}, collectivePullFlags{})
	}()

	// The watcher must paint during the held pull, before the test releases it.
	testwait.Receive(t, out.seen, "the interrupt painted the canceling footer while a transcript was in flight")
	close(release)

	if err := <-runErr; err == nil {
		t.Fatal("a cancelled bulk pull must exit non-zero")
	}

	// The copy from the pseudo-terminal drains asynchronously, so the final panel
	// can arrive after the command returns. Wait for it instead of reading the
	// buffer the instant the run finishes, which races the copy.
	testwait.Until(t, "the terminal kept the canceled panel", func() bool {
		return strings.Contains(out.String(), "pull canceled")
	})

	rendered := out.String()
	if !strings.Contains(rendered, "canceling pull...") {
		t.Errorf("the terminal must keep the canceling panel; got %q", rendered)
	}
	if !strings.Contains(rendered, "pull canceled") {
		t.Errorf("the terminal must keep the canceled panel; got %q", rendered)
	}
	if strings.Index(rendered, "canceling pull...") > strings.Index(rendered, "pull canceled") {
		t.Errorf("the canceled panel must land after the canceling one; got %q", rendered)
	}
}
