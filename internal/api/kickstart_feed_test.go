package api

import (
	"testing"

	"github.com/peasant-labs/schema"
)

// These are seam tests of one invariant, not scripts: the start of an import is
// published before the run is launched, so a run that ends at once can never have
// its end published ahead of its start. A page that heard the end first would
// show the start last and wait for an end that already came. The script fixtures
// cannot make a run end before launch returns.

func ingestStatesPublished(h *lockHarness) []schema.LocalKickstartIngestState {
	h.mu.Lock()
	defer h.mu.Unlock()
	var states []schema.LocalKickstartIngestState
	for _, event := range h.published {
		if event.Kind == schema.LocalKickstartEventIngest && event.Ingest != nil {
			states = append(states, event.Ingest.State)
		}
	}
	return states
}

func TestKickstartImportFeedPublishesTheStartBeforeALaunchedRunCanEnd(t *testing.T) {
	t.Parallel()
	h := newLockHarness(t)
	feed := newKickstartImportFeed()
	feed.attach(h.st)
	h.st.finishFor("saved-session")

	// A run that ends before launch returns, as an empty library does.
	feed.run(func(observe func(schema.LocalKickstartIngest)) {
		observe(schema.LocalKickstartIngest{State: schema.LocalKickstartIngestIdle})
	}, nil)

	got := ingestStatesPublished(h)
	want := []schema.LocalKickstartIngestState{schema.LocalKickstartIngestRunning, schema.LocalKickstartIngestIdle}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("the saved session heard %v, want %v: the start first, then the end", got, want)
	}
}

func TestKickstartImportFeedLaunchesARunNobodyWatches(t *testing.T) {
	t.Parallel()
	h := newLockHarness(t)
	launched := 0
	observed := true
	launch := func(observe func(schema.LocalKickstartIngest)) {
		launched++
		observed = observe != nil
	}

	// No saved session is kept, and a feed that was never attached to a store.
	feed := newKickstartImportFeed()
	feed.attach(h.st)
	feed.run(launch, nil)
	newKickstartImportFeed().run(launch, nil)
	(*kickstartImportFeed)(nil).run(launch, nil)

	if launched != 3 || observed {
		t.Errorf("%d runs were launched (want 3), and the last had an observer (%t, want none): a run nobody watches still runs", launched, observed)
	}
	if got := ingestStatesPublished(h); len(got) != 0 {
		t.Errorf("%v was published with no saved session kept", got)
	}
}
