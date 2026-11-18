package api

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/peasant-labs/schema"
)

func TestPreviewIdleStartsAfterFrameQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		armed := make(chan struct{}, 1)
		st := newKickstartSessions(kickstartEnv{
			afterFunc: func(time.Duration, func()) func() bool {
				armed <- struct{}{}
				return func() bool { return true }
			},
		})
		registry := newPreviewRegistry(st)
		conn := newConn(nil, NewHub(&mockDataProvider{}))
		for range cap(conn.writeCh) {
			conn.writeCh <- outboundMessage{}
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		preview := &webPreview{conn: conn, ctx: ctx, cancel: cancel, busy: true}
		registry.captures[conn] = preview
		done := make(chan error, 1)
		go func() {
			done <- registry.send(preview, schema.LocalKickstartPreviewEvent{
				Kind:      schema.LocalKickstartPreviewSegmentEvent,
				Directory: &schema.LocalKickstartPreviewDirectory{},
			})
		}()
		synctest.Wait()
		select {
		case <-armed:
			t.Fatal("idle timer started before the frame was queued")
		default:
		}
		<-conn.writeCh
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		<-armed
	})
}
