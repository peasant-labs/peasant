package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"strings"
	"sync"
	"syscall"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/defaults"
)

// Conn wraps a WebSocket connection with channel subscriptions and a write buffer.
type Conn struct {
	ws        *websocket.Conn
	hub       *Hub
	channels  map[ChannelTopic]bool
	writeCh   chan ServerMessage
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex // guards channels map
}

func newConn(ws *websocket.Conn, hub *Hub) *Conn {
	return &Conn{
		ws:       ws,
		hub:      hub,
		channels: make(map[ChannelTopic]bool),
		writeCh:  make(chan ServerMessage, defaults.ServerWriteChSize),
		done:     make(chan struct{}),
	}
}

// close signals teardown to Send and the write pump. The write channel is
// deliberately NEVER closed: snapshot goroutines run off the hub loop and
// may Send concurrently with unregister — a send on a closed channel
// panics the whole process, while an orphaned buffered channel is simply
// garbage-collected with the Conn.
func (c *Conn) close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// Send enqueues a message for delivery. Non-blocking; drops if buffer full
// and sends an error notification to the client.
func (c *Conn) Send(msg ServerMessage) {
	select {
	case <-c.done:
		// Connection torn down (possibly mid-snapshot): the message is
		// undeliverable — but leave a trace for "why did the client never
		// get its snapshot" investigations.
		slog.Debug("ws: dropping send to closed connection", "type", msg.Type)
		return
	default:
	}
	select {
	case c.writeCh <- msg:
	default:
		log.Printf("websocket: dropping message to slow client")
		// Best-effort error notification — don't block if this also fails
		select {
		case c.writeCh <- ServerMessage{
			Type:    MsgError,
			Message: "messages dropped: client too slow, re-subscribe for fresh snapshot",
		}:
		default:
		}
	}
}

// IsSubscribed returns whether the connection is subscribed to the topic.
func (c *Conn) IsSubscribed(topic ChannelTopic) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.channels[topic]
}

// Subscribe adds topics to the subscription set.
func (c *Conn) Subscribe(topics []ChannelTopic) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range topics {
		c.channels[t] = true
	}
}

// Unsubscribe removes topics from the subscription set.
func (c *Conn) Unsubscribe(topics []ChannelTopic) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range topics {
		delete(c.channels, t)
	}
}

// ReadPump reads messages from the WebSocket and dispatches to the hub.
func (c *Conn) ReadPump(ctx context.Context) {
	defer func() {
		// Non-blocking unregister avoids a goroutine leak if the hub has already exited.
		select {
		case c.hub.unregisterCh <- c:
		default:
		}
		c.ws.Close(websocket.StatusNormalClosure, "")
	}()

	c.ws.SetReadLimit(defaults.ServerReadLimit)

	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			if ctx.Err() == nil && !isClientDisconnect(err) {
				log.Printf("websocket: read error: %v", err)
			}
			return
		}

		var msg ClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.Send(ServerMessage{
				Type:    MsgError,
				Message: "invalid message format",
			})
			continue
		}

		switch msg.Type {
		case MsgSubscribe:
			c.hub.subscribeCh <- subscribeMsg{conn: c, subs: msg.Channels}
		case MsgUnsubscribe:
			topics := make([]ChannelTopic, 0, len(msg.Channels))
			for _, sub := range msg.Channels {
				topics = append(topics, sub.Topic)
			}
			c.hub.unsubscribeCh <- unsubscribeMsg{conn: c, topics: topics}
		default:
			c.Send(ServerMessage{
				Type:    MsgError,
				Message: "unsupported message type: " + string(msg.Type),
			})
		}
	}
}

// isClientDisconnect returns true for errors that indicate a normal client
// disconnect (tab close, page navigation, hot reload) — not worth logging.
func isClientDisconnect(err error) bool {
	// WebSocket close frames with expected status codes.
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.StatusNormalClosure,
			websocket.StatusGoingAway,
			websocket.StatusNoStatusRcvd:
			return true
		}
	}
	// TCP-level reset: browser tore down connection before close handshake.
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	// Catch wrapped "connection reset by peer" that doesn't unwrap to ECONNRESET.
	if strings.Contains(err.Error(), "connection reset by peer") {
		return true
	}
	return false
}

// WritePump drains the write channel and sends messages to the WebSocket.
func (c *Conn) WritePump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case msg, ok := <-c.writeCh:
			if !ok {
				return
			}
			data, err := json.Marshal(msg)
			if err != nil {
				log.Printf("websocket: marshal error: %v", err)
				continue
			}

			writeCtx, cancel := context.WithTimeout(ctx, defaults.ServerWriteTimeout)
			err = c.ws.Write(writeCtx, websocket.MessageText, data)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
