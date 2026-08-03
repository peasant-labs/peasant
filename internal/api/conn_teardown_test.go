package api

import (
	"context"
	"testing"
	"time"
)

// slowQualityProvider blocks QualitySessions until released, simulating a
// cold-store snapshot build. Every other DataProvider method panics if
// reached (embedded nil interface) — with no subscribers on the broadcast
// topics, the tick must not call them.
type slowQualityProvider struct {
	DataProvider
	entered chan struct{}
	release chan struct{}
}

func (p *slowQualityProvider) QualitySessions(context.Context, QualityFilter) ([]QualitySession, error) {
	close(p.entered)
	<-p.release
	return nil, nil
}

// A snapshot goroutine may still be sending when the hub unregisters its
// connection (the client closed the tab during a slow cold snapshot — the
// exact reported live behavior). The write channel must never be closed: a
// send on a closed channel panics the whole server process. This test is
// deterministic on the regression: with close(writeCh) teardown, the
// explicit Send below panics every run.
func TestSnapshotSendAfterUnregister_NoPanic(t *testing.T) {
	t.Parallel()
	provider := &slowQualityProvider{entered: make(chan struct{}), release: make(chan struct{})}
	h := NewHub(provider)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	conn := newConn(nil, h)
	h.registerCh <- conn
	h.subscribeCh <- subscribeMsg{conn: conn, subs: []ChannelSubscription{{Topic: TopicQuality}}}

	// The detached snapshot goroutine is now inside the slow provider.
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot never reached the provider")
	}

	// Client tears down mid-snapshot; wait for the hub to process it.
	h.unregisterCh <- conn
	select {
	case <-conn.done:
	case <-time.After(5 * time.Second):
		t.Fatal("unregister did not signal the conn's done channel")
	}

	// The regression, deterministically: Send on a torn-down conn — the same
	// call the snapshot goroutine makes when the provider returns. Under the
	// old close(writeCh) teardown this panics unconditionally.
	conn.Send(ServerMessage{Type: MsgError, Message: "post-teardown send"})

	// And the real goroutine's own send: release the provider so the
	// in-flight snapshot completes and Sends into the dead conn. An
	// unrecovered panic on that goroutine would kill the test process.
	close(provider.release)
}
