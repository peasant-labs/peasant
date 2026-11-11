package api

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
)

// TestKickstartViewsOfReadsAreBuiltOutsideTheStoreLock holds the view that GET
// and a new subscription build while it reads the configuration file and the
// sign-in, and requires every other request to the store to go on meanwhile. The
// view is the slow part of a read; if it is built under the store's lock, a tab
// that reconnects stops every scan tick and every answer from every other tab.
//
// This is a seam test of one invariant, not a script: the script fixtures run
// the whole server and cannot stop a request half way through its own view.
func TestKickstartViewsOfReadsAreBuiltOutsideTheStoreLock(t *testing.T) {
	t.Parallel()
	var hold atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	st := newKickstartSessions(kickstartEnv{
		ctx:        t.Context(),
		configPath: filepath.Join(t.TempDir(), "config.yaml"),
		loadConfig: func(context.Context) (*config.Config, error) { return config.BaseConfig(), nil },
		// The scan never finishes, so nothing but the views touches the session.
		scan: func(ctx context.Context, _ *config.Config, _ ingest.KnownSessionIndex, _ discovery.ScanProgress) (discovery.ProviderInventory, []onboarding.SessionListing, discovery.SubagentRelation) {
			<-ctx.Done()
			return nil, nil, nil
		},
		afterFunc: func(time.Duration, func()) func() bool { return func() bool { return true } },
		village: func() kickstartFacts {
			// The first view built after the test arms the hold stops here.
			if hold.CompareAndSwap(true, false) {
				entered <- struct{}{}
				<-release
			}
			return kickstartFacts{}
		},
	})
	opened, err := st.open(t.Context(), onboarding.ScopeFull)
	if err != nil {
		t.Fatalf("open a session: %v", err)
	}

	reads := map[string]func(){
		"GET a session": func() {
			if _, err := st.get(opened.ID); err != nil {
				t.Errorf("get the session: %v", err)
			}
		},
		"subscribe to the topic": func() {
			if events := st.snapshotEvents(""); len(events) != 1 {
				t.Errorf("the subscription was answered with %d events, want 1", len(events))
			}
		},
	}
	for name, read := range reads {
		hold.Store(true)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			read()
		}()
		testwait.Receive(t, entered, name+" to start building its view")

		// The view is half built. Another tab asks the store for something.
		other := make(chan string, 1)
		go func() { other <- st.activeFullSessionID() }()
		if got := testwait.Receive(t, other, "the store lock while "+name+" builds its view"); got != opened.ID {
			t.Errorf("while %s builds its view, the active full session is %q, want %q", name, got, opened.ID)
		}

		close(release)
		testwait.Receive(t, finished, name+" to finish")
		release = make(chan struct{})
	}
}
