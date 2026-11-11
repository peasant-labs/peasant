package api

import (
	"context"
	"log/slog"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// The kickstart topic carries the live setup sessions to every tab. One event
// carries one session id; a tab applies the events whose id is its session's and
// ignores the rest, so a wizard and a selection page share the topic without
// sharing state.
//
// Every accepted change is published once, with the session's new revision:
//
//	session    the full view after the session opened a scan result, so a tab can
//	           replace what it renders
//	answers    the full view after an answer, a resolution, or the connect offer
//	conflicts  the full view after the conflict rows changed
//	scan       the discovery job alone; the forest is loaded from the tree route
//	ingest     the import alone, published after the session is gone
//	closed     the end of the session, with its reason
//
// Subscribing yields one session event per live session id, built when the
// subscription is answered.

// snapshotEvents implements kickstartSnapshotSource. It builds the session event
// a subscription is answered with, one per live session. An empty sessionID names
// every live session.
func (st *kickstartSessions) snapshotEvents(sessionID string) []schema.LocalKickstartEvent {
	st.mu.Lock()
	var frozen []*kickstartSession
	for _, scope := range onboarding.AllModelScopes {
		sess, ok := st.byScope[scope]
		if !ok || sess.closed {
			continue
		}
		if sessionID != "" && sess.id != sessionID {
			continue
		}
		frozen = append(frozen, sess.frozen())
	}
	st.mu.Unlock()

	// The views read the configuration file and run the redactor, so they are
	// built after the store lock is released.
	var events []schema.LocalKickstartEvent
	for _, sess := range frozen {
		view, err := st.viewOf(sess)
		if err != nil {
			slog.Error("kickstart: a session snapshot could not be built for a subscription", "session", sess.id, "error", err)
			continue
		}
		events = append(events, schema.LocalKickstartEvent{Kind: schema.LocalKickstartEventSession, SessionID: sess.id, Revision: sess.revision, View: &view})
	}
	return events
}

var _ kickstartSnapshotSource = (*kickstartSessions)(nil)

// hubEmitter returns the publisher that sends events to every tab subscribed to
// the kickstart topic. It gives up once ctx is done, so a job that outlives the
// hub at shutdown never blocks on a channel nobody drains.
func hubEmitter(ctx context.Context, hub *Hub) func(schema.LocalKickstartEvent) {
	return func(event schema.LocalKickstartEvent) {
		hub.broadcastContext(ctx, TopicKickstart, ServerMessage{Type: MsgKickstart, Data: &event})
	}
}
