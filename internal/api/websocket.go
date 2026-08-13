package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

type subscribeMsg struct {
	conn *Conn
	subs []ChannelSubscription
}

type unsubscribeMsg struct {
	conn   *Conn
	topics []ChannelTopic
}

// Hub manages WebSocket connections, channel subscriptions, and broadcasts.
// A single hub goroutine owns the clients map; all mutations go through channels.
type Hub struct {
	provider      DataProvider
	clients       map[*Conn]bool
	registerCh    chan *Conn
	unregisterCh  chan *Conn
	subscribeCh   chan subscribeMsg
	unsubscribeCh chan unsubscribeMsg
	broadcastCh   chan broadcastMsg
}

type broadcastMsg struct {
	topic   ChannelTopic
	message ServerMessage
}

// NewHub creates a Hub backed by the given data provider.
func NewHub(provider DataProvider) *Hub {
	return &Hub{
		provider:      provider,
		clients:       make(map[*Conn]bool),
		registerCh:    make(chan *Conn, 16),
		unregisterCh:  make(chan *Conn, 16),
		subscribeCh:   make(chan subscribeMsg, 16),
		unsubscribeCh: make(chan unsubscribeMsg, 16),
		broadcastCh:   make(chan broadcastMsg, 16),
	}
}

// Run starts the hub event loop. Blocks until ctx is cancelled.
// The hub goroutine is the sole owner of the clients map — no mutex needed.
func (h *Hub) Run(ctx context.Context) {
	ticker := time.NewTicker(defaults.ServerBroadcastTick)
	defer ticker.Stop()
	defer func() {
		// Drain any pending unregister messages after shutdown
		for {
			select {
			case <-h.unregisterCh:
			default:
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			for c := range h.clients {
				c.close()
				delete(h.clients, c)
			}
			return

		case conn := <-h.registerCh:
			h.clients[conn] = true

		case conn := <-h.unregisterCh:
			if _, ok := h.clients[conn]; ok {
				conn.close()
				delete(h.clients, conn)
			}

		case msg := <-h.subscribeCh:
			validSubs := make([]ChannelSubscription, 0, len(msg.subs))
			for _, sub := range msg.subs {
				if ValidateSubscription(sub) {
					validSubs = append(validSubs, sub)
				} else {
					msg.conn.Send(topicError(sub, "invalid subscription", fmt.Errorf("topic %q is unknown or is missing its required identity fields", sub.Topic)))
				}
			}
			topics := make([]ChannelTopic, 0, len(validSubs))
			for _, sub := range validSubs {
				topics = append(topics, sub.Topic)
			}
			slog.Info("ws: subscribe", "topics", topics, "count", len(topics))
			msg.conn.Subscribe(topics)
			// Snapshot-on-subscribe: push current data for each subscription.
			// Off the hub loop: a cold snapshot build (the quality payload
			// reads the whole store) must never block the tick or every
			// OTHER client's snapshot behind one goroutine. Providers are
			// read-only and Conn.Send/IsSubscribed are concurrency-safe.
			go h.sendSnapshots(ctx, msg.conn, validSubs)

		case msg := <-h.unsubscribeCh:
			msg.conn.Unsubscribe(msg.topics)

		case msg := <-h.broadcastCh:
			for c := range h.clients {
				if c.IsSubscribed(msg.topic) {
					c.Send(msg.message)
				}
			}

		case <-ticker.C:
			h.broadcastAll(ctx)
		}
	}
}

// broadcastAll pushes current data for lightweight channels to subscribed clients.
// Expensive channels (quality, annotations, session_detail) are event-driven only —
// they send a snapshot on subscribe and update via targeted broadcast on mutation.
func (h *Hub) broadcastAll(ctx context.Context) {
	start := time.Now()
	topics := []ChannelTopic{TopicDashboard, TopicSessions, TopicTrends}
	msgs := make(map[ChannelTopic]ServerMessage)
	errorsByTopic := make(map[ChannelTopic]ServerMessage)

	// Only build payloads someone is subscribed to. The sessions payload
	// alone costs over a second on a large store — paying that every tick
	// for zero subscribers starved the hub loop (and the SQLite reader)
	// while a viewer sat on an entirely different channel.
	wanted := make(map[ChannelTopic]bool, len(topics))
	for c := range h.clients {
		for _, topic := range topics {
			if !wanted[topic] && c.IsSubscribed(topic) {
				wanted[topic] = true
			}
		}
	}

	for _, topic := range topics {
		if !wanted[topic] {
			continue
		}
		switch topic {
		case TopicDashboard:
			if payload, err := h.provider.DashboardMetrics(ctx); err == nil {
				msgs[topic] = ServerMessage{Type: MsgDashboard, Data: payload}
			} else {
				errorsByTopic[topic] = topicError(ChannelSubscription{Topic: topic}, "failed to refresh dashboard data", err)
			}
		case TopicSessions:
			if summaries, err := h.sessionSummaries(ctx); err == nil {
				msgs[topic] = ServerMessage{Type: MsgSessions, Data: &SessionsPayload{Sessions: summaries}}
			} else {
				errorsByTopic[topic] = topicError(ChannelSubscription{Topic: topic}, "failed to refresh selected sessions", err)
			}
		case TopicTrends:
			if payload, err := h.provider.TrendsData(ctx); err == nil {
				msgs[topic] = ServerMessage{Type: MsgTrends, Data: payload}
			} else {
				errorsByTopic[topic] = topicError(ChannelSubscription{Topic: topic}, "failed to refresh trends data", err)
			}
		}
	}

	for c := range h.clients {
		for topic, msg := range msgs {
			if c.IsSubscribed(topic) {
				c.Send(msg)
			}
		}
		for topic, msg := range errorsByTopic {
			if c.IsSubscribed(topic) {
				c.Send(msg)
			}
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		slog.Warn("ws: broadcastAll slow", "elapsed", elapsed, "clients", len(h.clients))
	}
}

// Broadcast sends a message to all connections subscribed to the topic.
//
// For TopicAnnotations: Conn.channels stores only ChannelTopic (not the full
// ChannelSubscription with Axis+ID), so broadcasts reach ALL annotation
// subscribers regardless of which session they subscribed to. The payload
// includes Axis and ID so clients MUST filter by payload.ID matching their
// subscribed session. This is acceptable for MVP traffic volumes.
// TODO(annotation/broadcast-filter): store full ChannelSubscription in Conn.channels for server-side filtering.
func (h *Hub) Broadcast(topic ChannelTopic, msg ServerMessage) {
	h.broadcastCh <- broadcastMsg{topic: topic, message: msg}
}

// RefreshQuality fetches current quality data and broadcasts to subscribers.
// Called after annotation mutations that affect quality metrics.
func (h *Hub) RefreshQuality(ctx context.Context) {
	sessions, err := h.provider.QualitySessions(ctx, QualityFilter{})
	if err != nil {
		slog.Warn("ws: RefreshQuality failed", "error", err)
		return
	}
	h.Broadcast(TopicQuality, ServerMessage{
		Type: MsgQuality,
		Data: &QualityPayload{Sessions: sessions},
	})
}

// HandleUpgrade upgrades an HTTP request to a WebSocket connection.
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	origins := make([]string, len(defaults.WSAllowedOrigins))
	for i, o := range defaults.WSAllowedOrigins {
		origins[i] = string(o)
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: origins, // local-only; accept all origins
	})
	if err != nil {
		slog.Warn("ws: upgrade failed", "error", err)
		return
	}

	conn := newConn(ws, h)
	h.registerCh <- conn
	slog.Info("ws: client connected", "remote", r.RemoteAddr)

	// Send connected message
	conn.Send(ServerMessage{
		Type:    MsgConnected,
		Version: defaults.Version.String(),
	})

	ctx := r.Context()
	go conn.WritePump(ctx)
	conn.ReadPump(ctx) // blocks until disconnect
	slog.Info("ws: client disconnected", "remote", r.RemoteAddr)
}

// sendSnapshots pushes current data for each subscription.
// Each ChannelSubscription carries the topic and any topic-specific fields (ID, Axis).
func (h *Hub) sendSnapshots(ctx context.Context, conn *Conn, subs []ChannelSubscription) {
	start := time.Now()
	for _, sub := range subs {
		var msg ServerMessage
		var err error
		subStart := time.Now()

		switch sub.Topic {
		case TopicDashboard:
			var payload *DashboardPayload
			payload, err = h.provider.DashboardMetrics(ctx)
			if err == nil {
				msg = ServerMessage{Type: MsgDashboard, Data: payload}
			}
		case TopicSessions:
			var sessions []SessionSummary
			sessions, err = h.sessionSummaries(ctx)
			if err == nil {
				msg = ServerMessage{Type: MsgSessions, Data: &SessionsPayload{Sessions: sessions}}
			}
		case TopicTrends:
			var payload *TrendsPayload
			payload, err = h.provider.TrendsData(ctx)
			if err == nil {
				msg = ServerMessage{Type: MsgTrends, Data: payload}
			}
		case TopicQuality:
			var sessions []QualitySession
			sessions, err = h.provider.QualitySessions(ctx, QualityFilter{})
			if err == nil {
				msg = ServerMessage{Type: MsgQuality, Data: &QualityPayload{Sessions: sessions}}
			}
		case TopicSessionDetail:
			session, findErr := h.provider.SessionByID(ctx, sub.ID)
			if findErr != nil {
				conn.Send(topicError(sub, "failed to load session detail", findErr))
				continue
			}
			detail, validationErr := transcript.SessionToDetailValidated(session)
			if validationErr != nil {
				conn.Send(topicError(sub, "failed to emit session detail because observed model evidence is invalid; repair and re-index the source session before retrying", validationErr))
				continue
			}
			refs, childErr := h.provider.ChildSessionsForParent(ctx, sub.ID)
			if childErr != nil {
				slog.Error("failed to load child sessions", "parent_id", sub.ID, "err", childErr)
			} else if len(refs) > 0 {
				detail.ChildSessions = refs
			}
			msg = ServerMessage{Type: MsgSessionDetail, Data: detail}
		case TopicProjectFamiliarity:
			projectHash, hashErr := schema.NewProjectHash(sub.ID)
			if hashErr != nil {
				conn.Send(topicError(sub, "failed to load project familiarity because the subscription project identity is malformed; subscribe with the 64-character lowercase hexadecimal project hash", hashErr))
				continue
			}
			payload, famErr := h.provider.ProjectFamiliarity(ctx, projectHash)
			if famErr != nil {
				conn.Send(topicError(sub, "failed to load project familiarity", famErr))
				continue
			}
			msg = ServerMessage{Type: MsgProjectFamiliarity, Data: payload}

		case TopicAnnotations:
			switch sub.Axis {
			case AxisSession:
				anns, annErr := h.provider.AnnotationsForSession(ctx, sub.ID)
				if annErr != nil {
					conn.Send(topicError(sub, "failed to load annotations", annErr))
					continue
				}
				msg = ServerMessage{
					Type: MsgAnnotations,
					Data: &AnnotationsPayload{
						Axis:        sub.Axis,
						ID:          sub.ID,
						Annotations: anns,
					},
				}
			default:
				conn.Send(topicError(sub, "failed to load annotations", fmt.Errorf("unsupported annotation axis %q", sub.Axis)))
				continue
			}
		}

		if err != nil {
			slog.Warn("ws: snapshot failed", "topic", sub.Topic, "elapsed", time.Since(subStart), "error", err)
			conn.Send(topicError(sub, "failed to load "+string(sub.Topic), err))
			continue
		}
		slog.Debug("ws: snapshot sent", "topic", sub.Topic, "elapsed", time.Since(subStart))
		conn.Send(msg)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		slog.Warn("ws: sendSnapshots slow", "elapsed", elapsed, "subs", len(subs))
	}
}

func topicError(sub ChannelSubscription, operation string, err error) ServerMessage {
	message := operation + ": " + err.Error() + "; the previous data for only this topic was cleared to avoid showing stale results; retry after resolving the reported cause"
	var data any
	if sessionvisibility.IsError(err) {
		data = struct {
			Code string `json:"code"`
		}{Code: "selection_visibility"}
		message += "; run `peasant kickstart` to repair the persisted selection, then retry"
	}
	return ServerMessage{
		Type:    MsgError,
		Topic:   sub.Topic,
		ID:      sub.ID,
		Axis:    sub.Axis,
		Message: message,
		Data:    data,
	}
}

// sessionSummaries converts full sessions to summary form.
func (h *Hub) sessionSummaries(ctx context.Context) ([]SessionSummary, error) {
	return h.provider.SessionSummaries(ctx)
}
