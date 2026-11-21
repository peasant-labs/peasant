package push_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	storepkg "github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestPipeline_DryRunScansThroughTheBoundedWorkerPool proves a dry run honors
// --concurrency and still reports sessions in candidate order.
//
// The run's UpdateHold is the per-session hook: every session carries a receipt
// from this account under another project identity, so the scan asks it about
// each one. The first session does not return until every other session has
// finished. A serial scan can never get past it; a bounded scan runs the others
// in the second slot, so exactly two scans overlap and the first session
// finishes last.
func TestPipeline_DryRunScansThroughTheBoundedWorkerPool(t *testing.T) {
	t.Parallel()
	if push.DryRunCapabilityMutation {
		t.Skip("the mounted capability fixture owns the dry-run decision mutation")
	}
	const (
		concurrency = 2
		heldReason  = "held by the test hold"
		waitLimit   = 10 * time.Second
	)
	fs := testutil.NewMemFS()
	creds := baseCreds()
	otherProject := schema.ProjectHash(strings.Repeat("0", 64))
	ids := make([]string, 4)
	sessions := make([]ingest.PushSessionRow, len(ids))
	publications := make(map[string]storepkg.PublicationRecord, len(ids))
	for i := range ids {
		ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		seedMemFS(t, fs, testutil.TestHostSlug, ids[i], defaults.HarnessClaudeCode)
		sessions[i] = makeSession(ids[i], testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil)
		publications[ids[i]] = storepkg.PublicationRecord{VillageOrigin: creds.VillageURL, OwnerUserID: creds.UserID, SessionID: ids[i], ProjectHash: otherProject}
	}

	var (
		mu         sync.Mutex
		inFlight   int
		maxFlight  int
		finished   []string
		firstOn    = make(chan struct{})
		othersDone sync.WaitGroup
		timedOut   []string
	)
	othersDone.Add(len(ids) - 1)
	enter := func() {
		mu.Lock()
		defer mu.Unlock()
		inFlight++
		maxFlight = max(maxFlight, inFlight)
	}
	leave := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		inFlight--
		finished = append(finished, id)
	}
	wait := func(id string, done <-chan struct{}) {
		select {
		case <-done:
		case <-time.After(waitLimit):
			mu.Lock()
			timedOut = append(timedOut, id)
			mu.Unlock()
		}
	}
	allOthersDone := make(chan struct{})
	go func() { othersDone.Wait(); close(allOthersDone) }()

	hold := func(_ context.Context, sessionID string) string {
		enter()
		defer leave(sessionID)
		if sessionID == ids[0] {
			close(firstOn)
			wait(sessionID, allOthersDone)
			return ""
		}
		defer othersDone.Done()
		wait(sessionID, firstOn)
		if sessionID == ids[len(ids)-1] {
			return heldReason
		}
		return ""
	}

	store := &testutil.StubPushStore{Sessions: sessions, Publications: publications}
	pub := &testutil.StubPublisher{}
	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(),
		push.PipelineConfig{DryRun: true, Concurrency: concurrency, UpdateHold: hold}, &stderr)

	result, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(timedOut) != 0 {
		t.Fatalf("sessions %v waited %s for an overlapping scan; the dry run did not run sessions concurrently", timedOut, waitLimit)
	}
	if maxFlight != concurrency {
		t.Errorf("max in-flight scans = %d, want exactly %d (--concurrency)", maxFlight, concurrency)
	}
	if got := finished[len(finished)-1]; got != ids[0] {
		t.Fatalf("last scan to finish = %s, want the first candidate %s; finish order %v", got, ids[0], finished)
	}
	if len(pub.Calls) != 0 || len(pub.AuthoritativeCalls) != 0 || len(store.SavedPublicationIDs) != 0 || len(store.PushLogs) != 0 {
		t.Errorf("dry run made side effects: calls=%d authoritative=%d saved=%d logs=%d",
			len(pub.Calls), len(pub.AuthoritativeCalls), len(store.SavedPublicationIDs), len(store.PushLogs))
	}

	if len(result.Sessions) != len(ids) {
		t.Fatalf("result sessions = %d, want %d", len(result.Sessions), len(ids))
	}
	for i, sr := range result.Sessions {
		wantStatus, wantReason := push.PushStatusNew, ""
		if i == len(ids)-1 {
			wantStatus, wantReason = push.PushStatusHeld, heldReason
		}
		if sr.SessionID != ids[i] || sr.Status != wantStatus || sr.HeldReason != wantReason || sr.Error != nil {
			t.Errorf("result[%d] = {%s %v %q %v}, want {%s %v %q <nil>} in candidate order",
				i, sr.SessionID, sr.Status, sr.HeldReason, sr.Error, ids[i], wantStatus, wantReason)
		}
	}
	if result.New != len(ids)-1 || result.Held != 1 || result.Errors != 0 {
		t.Errorf("counts new=%d held=%d errors=%d, want %d/1/0", result.New, result.Held, result.Errors, len(ids)-1)
	}
}
