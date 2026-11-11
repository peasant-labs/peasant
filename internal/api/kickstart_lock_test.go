package api

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/schema"
)

// These are seam tests of one invariant, not scripts: the store lock is held for
// work on memory only. The slow part of a view is the review, which reads the
// configuration file; if it runs under the lock, a tab that reads or answers
// stops every scan tick and every answer from every other tab. The script
// fixtures run the whole server and cannot stop a request half way through its
// own view, so the review is held here through the one seam the store gives it.

// lockHarness is a session store whose review can be held, over a scan that
// never finishes so that nothing but the requests under test touches a session.
type lockHarness struct {
	st *kickstartSessions

	hold    atomic.Bool
	entered chan struct{}
	release chan struct{}
	// touched is signalled each time a change is accepted: the idle timer
	// restarts at that moment, under the lock.
	touched chan struct{}

	mu        sync.Mutex
	published []schema.LocalKickstartEvent
}

func newLockHarness(t *testing.T) *lockHarness {
	t.Helper()
	h := &lockHarness{
		entered: make(chan struct{}, 4),
		release: make(chan struct{}),
		touched: make(chan struct{}, 64),
	}
	h.st = newKickstartSessions(kickstartEnv{
		ctx:        t.Context(),
		configPath: filepath.Join(t.TempDir(), "config.yaml"),
		loadConfig: func(context.Context) (*config.Config, error) { return config.BaseConfig(), nil },
		scan: func(ctx context.Context, _ *config.Config, _ ingest.KnownSessionIndex, _ discovery.ScanProgress) (discovery.ProviderInventory, []onboarding.SessionListing, discovery.SubagentRelation) {
			<-ctx.Done()
			return nil, nil, nil
		},
		afterFunc: func(time.Duration, func()) func() bool {
			h.touched <- struct{}{}
			return func() bool { return true }
		},
		emit: func(event schema.LocalKickstartEvent) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.published = append(h.published, event)
		},
		review: func(ctx context.Context, draft *onboarding.Draft, input onboarding.ReviewInput) (onboarding.ReviewResult, error) {
			// The first review after the test arms the hold stops here.
			if h.hold.CompareAndSwap(true, false) {
				h.entered <- struct{}{}
				<-h.release
			}
			return draft.Review(ctx, input)
		},
	})
	return h
}

// arm makes the next review wait until release is closed.
func (h *lockHarness) arm() {
	h.release = make(chan struct{})
	h.hold.Store(true)
}

func (h *lockHarness) eventRevisions() []uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	revisions := make([]uint64, len(h.published))
	for i, event := range h.published {
		revisions[i] = event.Revision
	}
	return revisions
}

// requireStoreFree fails unless the store answers while a review is held.
func requireStoreFree(t *testing.T, h *lockHarness, want string, during string) {
	t.Helper()
	other := make(chan string, 1)
	go func() { other <- h.st.activeFullSessionID() }()
	if got := testwait.Receive(t, other, "the store lock while "+during); got != want {
		t.Errorf("while %s, the active full session is %q, want %q", during, got, want)
	}
}

// TestKickstartViewsOfReadsAreBuiltOutsideTheStoreLock holds the review of the
// view that GET and a new subscription build, and requires every other request
// to the store to go on meanwhile.
func TestKickstartViewsOfReadsAreBuiltOutsideTheStoreLock(t *testing.T) {
	t.Parallel()
	h := newLockHarness(t)
	opened, err := h.st.open(t.Context(), onboarding.ScopeFull)
	if err != nil {
		t.Fatalf("open a session: %v", err)
	}
	reads := map[string]func(){
		"GET a session": func() {
			if _, err := h.st.get(opened.ID); err != nil {
				t.Errorf("get the session: %v", err)
			}
		},
		"subscribe to the topic": func() {
			if events := h.st.snapshotEvents(""); len(events) != 1 {
				t.Errorf("the subscription was answered with %d events, want 1", len(events))
			}
		},
	}
	for name, read := range reads {
		h.arm()
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			read()
		}()
		testwait.Receive(t, h.entered, name+" to start its review")
		requireStoreFree(t, h, opened.ID, name+" reviews")
		close(h.release)
		testwait.Receive(t, finished, name+" to finish")
	}
}

// TestKickstartAcceptedChangesBuildTheirViewOutsideTheStoreLock holds the review
// of the view an accepted change carries, and requires the store to go on, a
// second change to be accepted meanwhile, and the events to leave in the order
// the changes were made.
func TestKickstartAcceptedChangesBuildTheirViewOutsideTheStoreLock(t *testing.T) {
	t.Parallel()
	h := newLockHarness(t)
	opened, err := h.st.open(t.Context(), onboarding.ScopeFull)
	if err != nil {
		t.Fatalf("open a session: %v", err)
	}
	for len(h.touched) > 0 {
		<-h.touched
	}

	h.arm()
	firstDone := make(chan error, 1)
	go func() {
		_, err := h.st.patch(opened.ID, schema.LocalKickstartAnswers{
			Revision: opened.Revision,
			Values:   map[string]schema.LocalSettingValue{onboarding.FieldLicense.String(): onboarding.StringValue("CC0-1.0")},
		})
		firstDone <- err
	}()
	// The first change is accepted under the lock and then waits in its review.
	testwait.Receive(t, h.touched, "the first change to be accepted")
	testwait.Receive(t, h.entered, "the first change to start its review")
	requireStoreFree(t, h, opened.ID, "an accepted change reviews")

	// A second change is accepted while the first is still reviewing, and its
	// event waits for the first's.
	secondDone := make(chan error, 1)
	go func() {
		_, err := h.st.patch(opened.ID, schema.LocalKickstartAnswers{
			Revision: opened.Revision + 1,
			Values:   map[string]schema.LocalSettingValue{onboarding.FieldPublication.String(): onboarding.StringValue("share-later")},
		})
		secondDone <- err
	}()
	testwait.Receive(t, h.touched, "the second change to be accepted while the first reviews")
	if published := h.eventRevisions(); len(published) != 0 {
		t.Fatalf("events %v were published while the first change was still reviewing", published)
	}

	close(h.release)
	for name, done := range map[string]chan error{"first": firstDone, "second": secondDone} {
		if err := testwait.Receive(t, done, "the "+name+" change to finish"); err != nil {
			t.Fatalf("the %s change was refused: %v", name, err)
		}
	}
	want := []uint64{opened.Revision + 1, opened.Revision + 2}
	got := h.eventRevisions()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("events were published at revisions %v, want %v: the order the changes were made", got, want)
	}
}

// TestKickstartRefusedChangeStillLeavesTheSessionAsItWas checks that taking the
// slow part of the view out of the lock did not move a refusal out with it: an
// answer to a hidden field is refused while the lock is held, and the session,
// its revision and its idle timer are as they were.
func TestKickstartRefusedChangeStillLeavesTheSessionAsItWas(t *testing.T) {
	t.Parallel()
	h := newLockHarness(t)
	opened, err := h.st.open(t.Context(), onboarding.ScopeFull)
	if err != nil {
		t.Fatalf("open a session: %v", err)
	}
	for len(h.touched) > 0 {
		<-h.touched
	}
	before := len(h.eventRevisions())

	// The sharing stop is shown only while this computer is signed in; it is not.
	_, err = h.st.patch(opened.ID, schema.LocalKickstartAnswers{
		Revision: opened.Revision,
		Values:   map[string]schema.LocalSettingValue{onboarding.FieldVisibility.String(): onboarding.StringValue("private")},
	})
	if err == nil {
		t.Fatal("an answer to a hidden field was accepted")
	}
	after, getErr := h.st.get(opened.ID)
	if getErr != nil {
		t.Fatalf("get the session: %v", getErr)
	}
	if after.Revision != opened.Revision {
		t.Errorf("the refused change left revision %d, want %d", after.Revision, opened.Revision)
	}
	if len(h.touched) != 0 {
		t.Error("the refused change restarted the idle timer")
	}
	if got := len(h.eventRevisions()); got != before {
		t.Errorf("the refused change published %d events", got-before)
	}
}

// TestKickstartConflictChoicesReadTheFileOutsideTheStoreLock holds the review a
// choice on a conflict row needs to learn the file value the user saw, and
// requires the store to go on meanwhile. The choice is then applied against a
// session that stood still, and the event it publishes is its own.
func TestKickstartConflictChoicesReadTheFileOutsideTheStoreLock(t *testing.T) {
	t.Parallel()
	h := newLockHarness(t)
	opened, err := h.st.open(t.Context(), onboarding.ScopeFull)
	if err != nil {
		t.Fatalf("open a session: %v", err)
	}
	h.arm()
	done := make(chan error, 1)
	go func() {
		_, err := h.st.patch(opened.ID, schema.LocalKickstartAnswers{
			Revision:    opened.Revision,
			Resolutions: map[string]schema.LocalKickstartResolution{onboarding.FieldLicense.String(): schema.LocalKickstartResolutionMine},
		})
		done <- err
	}()
	testwait.Receive(t, h.entered, "the choice to start reading the file")
	requireStoreFree(t, h, opened.ID, "a choice reads the file")
	close(h.release)
	if err := testwait.Receive(t, done, "the choice to be applied"); err != nil {
		t.Fatalf("the choice was refused: %v", err)
	}
}
