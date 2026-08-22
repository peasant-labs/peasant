package ingest

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

// TestDeferredOpenCodeSourceCloseReleasesConnectionUnderCancelledContext proves
// that a source kept open for the whole Discover is released even when the
// caller context is already cancelled. The deferred close detaches from the
// caller context, so Close cannot take its cancellation branch and leak the one
// connection in the long-lived web process. The private connection state is the
// observable outcome.
//
// A cancelled context makes the source Close select race between its
// cancellation branch and the ready idle permit, so one leak is probabilistic.
// The test opens several sources and requires every one closed. The detached
// close never signals cancellation, so all sources close. Passing the cancelled
// caller context through would leak a fraction of them, which fails this
// assertion with near certainty.
func TestDeferredOpenCodeSourceCloseReleasesConnectionUnderCancelledContext(t *testing.T) {
	const sources = 24

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for index := 0; index < sources; index++ {
		materialized := testfixture.MaterializeByName(t, "legacy-message-part")
		path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
		if err != nil {
			t.Fatalf("validate synthetic source path: %v", err)
		}
		opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
		if err != nil {
			t.Fatalf("open synthetic source: %v", err)
		}
		source := opened.(*zombiezenOpenCodeSQLiteSource)

		if err := closeDeferredOpenCodeSource(cancelled, source, DefaultOpenCodeSQLiteSourceOptions().queryTimeout); err != nil {
			t.Fatalf("deferred close under a cancelled context: %v", err)
		}

		source.stateMu.Lock()
		closed := source.connClosed
		source.stateMu.Unlock()
		if !closed {
			t.Fatalf("deferred close left source %d open under a cancelled context; it must detach and release the connection", index)
		}
	}
}
