package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
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

	// configPath is the configuration file the sessions read and review.
	configPath string
	// reviewsUnderLock counts reviews that began while the store lock was held.
	reviewsUnderLock atomic.Int32
	// signInReads counts reads of the sign-in file, and signInReadsUnderLock the
	// reads that found the store lock held.
	signInReads          atomic.Int32
	signInReadsUnderLock atomic.Int32

	mu        sync.Mutex
	published []schema.LocalKickstartEvent
}

func newLockHarness(t *testing.T) *lockHarness {
	t.Helper()
	h := &lockHarness{
		entered:    make(chan struct{}, 4),
		release:    make(chan struct{}),
		touched:    make(chan struct{}, 64),
		configPath: filepath.Join(t.TempDir(), "config.yaml"),
	}
	h.st = newKickstartSessions(kickstartEnv{
		ctx:        t.Context(),
		configPath: h.configPath,
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
		village: func() kickstartFacts {
			h.signInReads.Add(1)
			if h.heldNow() {
				h.signInReadsUnderLock.Add(1)
			}
			return kickstartFacts{}
		},
		review: func(ctx context.Context, draft *onboarding.Draft, input onboarding.ReviewInput) (onboarding.ReviewResult, error) {
			if h.heldNow() {
				h.reviewsUnderLock.Add(1)
			}
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

// heldNow reports whether the store lock is held by someone at this moment. A
// test that reads it has no other request running, so a held lock is the
// request that is asking.
func (h *lockHarness) heldNow() bool {
	if h.st.mu.TryLock() {
		h.st.mu.Unlock()
		return false
	}
	return true
}

// writeFile replaces the configuration file the sessions review, the way another
// program would.
func (h *lockHarness) writeFile(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(h.configPath, []byte(text), 0o600); err != nil {
		t.Fatalf("write the configuration file: %v", err)
	}
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
	var refusal *kickstartError
	if !errors.As(err, &refusal) || refusal.code != schema.LocalKickstartErrorFieldHidden {
		t.Fatalf("an answer to a hidden field was answered %v, want a %s refusal", err, schema.LocalKickstartErrorFieldHidden)
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

// kickstartFileWithLicense is a configuration file another program saved with
// the license the setup session's own answer disagrees with.
const kickstartFileWithLicense = `version: 1
push:
  license: CC-BY-4.0
`

// TestKickstartConflictChoicesReadTheFileOutsideTheStoreLock holds the review a
// choice on a conflict row needs to learn the file value the user saw, and
// requires the store to go on meanwhile. The choice is then applied against a
// session that stood still: it is recorded with the file value that review saw,
// no review ran under the lock, and the event it publishes is its own.
func TestKickstartConflictChoicesReadTheFileOutsideTheStoreLock(t *testing.T) {
	t.Parallel()
	h := newLockHarness(t)
	opened, err := h.st.open(t.Context(), onboarding.ScopeFull)
	if err != nil {
		t.Fatalf("open a session: %v", err)
	}
	// The session answers the license, and then another program saves a
	// different one: the file's value is a row the choice settles.
	answered, err := h.st.patch(opened.ID, schema.LocalKickstartAnswers{
		Revision: opened.Revision,
		Values:   map[string]schema.LocalSettingValue{onboarding.FieldLicense.String(): onboarding.StringValue("CC0-1.0")},
	})
	if err != nil {
		t.Fatalf("answer the license: %v", err)
	}
	h.writeFile(t, kickstartFileWithLicense)
	published := len(h.eventRevisions())

	h.arm()
	type outcome struct {
		result schema.LocalKickstartPatchResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := h.st.patch(opened.ID, schema.LocalKickstartAnswers{
			Revision:    answered.Revision,
			Resolutions: map[string]schema.LocalKickstartResolution{onboarding.FieldLicense.String(): schema.LocalKickstartResolutionMine},
		})
		done <- outcome{result, err}
	}()
	testwait.Receive(t, h.entered, "the choice to start reading the file")
	requireStoreFree(t, h, opened.ID, "a choice reads the file")
	close(h.release)
	got := testwait.Receive(t, done, "the choice to be applied")
	if got.err != nil {
		t.Fatalf("the choice was refused: %v", got.err)
	}

	if want := answered.Revision + 1; got.result.Revision != want || got.result.Stale {
		t.Errorf("the choice was answered at revision %d (stale %t), want revision %d, not stale", got.result.Revision, got.result.Stale, want)
	}
	events := h.eventRevisions()
	if len(events) != published+1 || events[len(events)-1] != answered.Revision+1 {
		t.Errorf("the events after the choice are %v, want exactly one more, at revision %d", events[min(published, len(events)):], answered.Revision+1)
	}
	h.st.mu.Lock()
	recorded := h.st.byID[opened.ID].resolutions[onboarding.FieldLicense]
	h.st.mu.Unlock()
	if recorded.Choice != schema.LocalKickstartResolutionMine || string(recorded.Seen) != `"CC-BY-4.0"` {
		t.Errorf("the choice was recorded as %q seeing %s, want mine seeing \"CC-BY-4.0\", the value the review read", recorded.Choice, recorded.Seen)
	}
	if n := h.reviewsUnderLock.Load(); n != 0 {
		t.Errorf("%d reviews ran with the store lock held, want none: the session stood still", n)
	}
}

// TestKickstartSignInFileIsReadOnceOutsideTheStoreLock counts the reads of the
// sign-in file each request makes: one, before the store lock is taken, however
// many views and rebuilds the request goes on to make under it.
func TestKickstartSignInFileIsReadOnceOutsideTheStoreLock(t *testing.T) {
	t.Parallel()
	h := newLockHarness(t)
	once := func(name string, do func()) {
		t.Helper()
		before := h.signInReads.Load()
		do()
		if got := h.signInReads.Load() - before; got != 1 {
			t.Errorf("%s read the sign-in file %d times, want once", name, got)
		}
	}

	var opened schema.LocalKickstartSession
	once("opening a session", func() {
		var err error
		if opened, err = h.st.open(t.Context(), onboarding.ScopeFull); err != nil {
			t.Fatalf("open a session: %v", err)
		}
	})
	revision := opened.Revision
	once("an accepted change", func() {
		result, err := h.st.patch(opened.ID, schema.LocalKickstartAnswers{
			Revision: revision,
			Values:   map[string]schema.LocalSettingValue{onboarding.FieldLicense.String(): onboarding.StringValue("CC0-1.0")},
		})
		if err != nil {
			t.Fatalf("answer the license: %v", err)
		}
		revision = result.Revision
	})
	h.writeFile(t, kickstartFileWithLicense)
	once("a choice on a conflict row, which also reviews the file", func() {
		result, err := h.st.patch(opened.ID, schema.LocalKickstartAnswers{
			Revision:    revision,
			Resolutions: map[string]schema.LocalKickstartResolution{onboarding.FieldLicense.String(): schema.LocalKickstartResolutionMine},
		})
		if err != nil {
			t.Fatalf("settle the license: %v", err)
		}
		revision = result.Revision
	})
	once("reading a session", func() {
		if _, err := h.st.get(opened.ID); err != nil {
			t.Fatalf("get the session: %v", err)
		}
	})
	once("publishing the rows of a refused save", func() {
		rows := []schema.LocalKickstartConflict{{
			Field: onboarding.FieldLicense.String(), Stop: "license", Label: "license",
			Mine: onboarding.StringValue("CC0-1.0"), Theirs: onboarding.StringValue("CC-BY-4.0"),
		}}
		if _, err := h.st.publishConflicts(opened.ID, rows); err != nil {
			t.Fatalf("publish the rows: %v", err)
		}
	})
	once("freezing a session for a save", func() {
		if _, err := h.st.beginCommit(opened.ID); err != nil {
			t.Fatalf("begin the save: %v", err)
		}
		h.st.abortCommit(opened.ID)
	})
	once("a finished scan", func() {
		h.st.mu.Lock()
		sess, gen := h.st.byID[opened.ID], h.st.byID[opened.ID].scanGen
		h.st.mu.Unlock()
		h.st.finishScan(sess, gen, kickstartScanResult{})
	})

	if n := h.signInReadsUnderLock.Load(); n != 0 {
		t.Errorf("%d reads of the sign-in file happened with the store lock held, want none", n)
	}
}

// TestConfigLockWaitIsOneValue holds the two writers that take config.yaml's
// lock to one wait: the setup save and the settings update default to
// defaults.ConfigLockWait, and a wait a test sets is the one used.
func TestConfigLockWaitIsOneValue(t *testing.T) {
	t.Parallel()
	save := kickstartEnv{}.withDefaults().lockWait
	update := (&settingsHandler{}).lockWaitOrDefault()
	if save != defaults.ConfigLockWait || update != defaults.ConfigLockWait {
		t.Errorf("the setup save waits %s and the settings update waits %s for the configuration lock, both want defaults.ConfigLockWait (%s)", save, update, defaults.ConfigLockWait)
	}
	if got := (&settingsHandler{lockWait: time.Second}).lockWaitOrDefault(); got != time.Second {
		t.Errorf("a settings update with its own wait waits %s, want 1s", got)
	}
	if got := (kickstartEnv{lockWait: time.Second}).withDefaults().lockWait; got != time.Second {
		t.Errorf("a setup save with its own wait waits %s, want 1s", got)
	}
}
