'use client';

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from 'react';
import type { ChannelName, ClientMessage, ServerMessage, SubscriptionMessage } from '@/types/messages';
import { subscriptionKey, subscribe as mkSub, ChannelTopic, MessageType } from '@/types/messages';
import { parseServerMessageRaw } from '@peasant-labs/schema';
import { parseKickstartEvent, type KickstartEvent } from '@/lib/kickstart/model';
import { DiscoveryErrorCode } from '@/lib/api/errors';
import {
  codeFromFault,
  FaultCause,
  FaultOperation,
  FaultStore,
  faultOperationForMessageType,
  faultOperationForTopic,
  frameByteLength,
  MAX_FRAME_BYTES,
  peekMessageType,
  schemaPathDetail,
  WebFault,
} from '@/lib/faults';

/** Derive WebSocket URL from the current page origin so any --port value works. */
function defaultWsUrl(): string {
  if (typeof window === 'undefined') return 'ws://localhost:8690/api/v1/ws';
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/api/v1/ws`;
}

const INITIAL_BACKOFF_MS = 1000;
const MAX_BACKOFF_MS = 30000;
const BACKOFF_MULTIPLIER = 2;

// ---------------------------------------------------------------------------
// Channel data store (ref-based, outside React state)
// ---------------------------------------------------------------------------

type ChannelListener = () => void;

/**
 * ChannelStore holds per-channel data outside React state. Components
 * subscribe to specific channels via useSyncExternalStore, so a `sessions`
 * update does NOT re-render a component that only cares about `dashboard`.
 *
 * Keyed by subscriptionKey() strings (e.g. "dashboard", "session_detail:abc123").
 * A retained fault is keyed by operation plus that same request key, so a
 * success for one operation or one session never clears another.
 */
class ChannelStore {
  private data = new Map<string, unknown>();
  private readonly faults = new FaultStore();
  private listeners = new Map<string, Set<ChannelListener>>();

  get(key: string): unknown {
    return this.data.get(key);
  }

  getFault(operation: FaultOperation, key: string): WebFault | null {
    return this.faults.get(operation, key);
  }

  set(operation: FaultOperation, key: string, value: unknown): void {
    const prev = JSON.stringify(this.data.get(key));
    const next = JSON.stringify(value);
    const hadFault = this.faults.clear(operation, key);
    if (prev === next && !hadFault) return; // no-op if unchanged
    this.data.set(key, value);
    // Notify only listeners for this specific channel.
    this.notify(key);
  }

  /** Clear one operation's fault and publish the change to that channel only. */
  clearFault(operation: FaultOperation, key: string): void {
    if (this.faults.clear(operation, key)) this.notify(key);
  }

  /**
   * Store one operation-scoped failure and publish it to that channel only. The
   * channel's last snapshot is RETAINED: a failed refresh is a note above the
   * rows the reader already has, not a reason to blank them.
   */
  fail(operation: FaultOperation, key: string, fault: WebFault): void {
    if (!this.faults.set(operation, key, fault)) return;
    this.notify(key);
  }

  private notify(key: string): void {
    const subs = this.listeners.get(key);
    if (subs) {
      for (const fn of subs) fn();
    }
  }

  subscribe(key: string, listener: ChannelListener): () => void {
    let subs = this.listeners.get(key);
    if (!subs) {
      subs = new Set();
      this.listeners.set(key, subs);
    }
    subs.add(listener);
    return () => {
      subs!.delete(listener);
      if (subs!.size === 0) this.listeners.delete(key);
    };
  }
}

// ---------------------------------------------------------------------------
// Context (connection state only — no channel data)
// ---------------------------------------------------------------------------

interface WebSocketContextValue {
  connected: boolean;
  /** The socket's fault: a frame failure, a socket error, or a non-topic refusal. */
  connectionFault: WebFault | null;
  store: ChannelStore;
  /** Subscribe to WS channels on the server. */
  wsSubscribe: (channels: SubscriptionMessage[]) => void;
  /** Unsubscribe from WS channels on the server. */
  wsUnsubscribe: (channels: SubscriptionMessage[]) => void;
  /**
   * Ask the server for a fresh snapshot of channels this page already holds.
   * An event-driven channel (a session's detail) is sent once on subscribe, so
   * after a change the page made itself (a re-index) it reads the new state
   * by subscribing again; the server keeps one subscription per channel.
   */
  wsRefresh: (channels: SubscriptionMessage[]) => void;
  /** Send an arbitrary client message. */
  sendMessage: (msg: ClientMessage) => void;
  /**
   * Listen for the live setup events. They are discrete events, not a snapshot
   * of a channel: the channel store keeps only the last value and drops a
   * repeat, which would lose a scan tick or a second tab's answer. Returns the
   * function that stops listening.
   */
  onKickstartEvent: (listener: (event: KickstartEvent) => void) => () => void;
  /**
   * Connect again now instead of waiting out the reconnect backoff. A no-op
   * while a socket is open or still connecting.
   */
  reconnect: () => void;
}

const WebSocketContext = createContext<WebSocketContextValue | null>(null);

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

export function WebSocketProvider({ children }: { children: ReactNode }) {
  const [connected, setConnected] = useState(false);
  const [connectionFault, setConnectionFault] = useState<WebFault | null>(null);

  // Stable refs — never trigger React re-renders.
  const storeRef = useRef(new ChannelStore());
  const wsRef = useRef<WebSocket | null>(null);
  const backoffRef = useRef(INITIAL_BACKOFF_MS);
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const mountedRef = useRef(true);
  // Keyed by subscriptionKey(); stores the full subscription + ref-count for reconnect.
  const subsRef = useRef<Map<string, { sub: SubscriptionMessage; count: number }>>(new Map());
  const kickstartListenersRef = useRef(new Set<(event: KickstartEvent) => void>());

  const sendRaw = useCallback((msg: ClientMessage) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(msg));
    }
  }, []);

  const connect = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.onopen = null;
      wsRef.current.onclose = null;
      wsRef.current.onmessage = null;
      wsRef.current.onerror = null;
      if (
        wsRef.current.readyState === WebSocket.OPEN ||
        wsRef.current.readyState === WebSocket.CONNECTING
      ) {
        wsRef.current.close();
      }
      wsRef.current = null;
    }

    const wsUrl =
      typeof window !== 'undefined'
        ? (process.env.NEXT_PUBLIC_WS_URL ?? defaultWsUrl())
        : 'ws://localhost:8690/api/v1/ws';

    const ws = new WebSocket(wsUrl);
    wsRef.current = ws;

    ws.onopen = () => {
      if (!mountedRef.current) return;
      setConnected(true);
      // A reconnect is the connection fault's only healthy transition.
      setConnectionFault(null);
      backoffRef.current = INITIAL_BACKOFF_MS;

      const activeSubs = Array.from(subsRef.current.values())
        .filter(e => e.count > 0)
        .map(e => e.sub);
      if (activeSubs.length > 0) {
        ws.send(JSON.stringify({ type: 'subscribe', channels: activeSubs }));
      }
    };

    ws.onmessage = (event: MessageEvent) => {
      if (!mountedRef.current) return;

      const raw: unknown = event.data;
      if (typeof raw !== 'string') {
        setConnectionFault(
          WebFault.frameInvalid(FaultOperation.Socket, 'the socket delivered a non-text frame'),
        );
        return;
      }

      let msg: ServerMessage;
      try {
        // Schema scans the original text before decoding can collapse duplicate
        // keys, then validates detail data only for session_detail messages.
        msg = parseServerMessageRaw(raw);
      } catch (error) {
        // Classify without echoing untrusted transcript bytes or exception
        // values: measure the frame, peek its operation, and carry a bounded
        // schema path at most.
        const operation = faultOperationForMessageType(peekMessageType(raw) ?? '');
        const bytes = frameByteLength(raw);
        setConnectionFault(
          bytes > MAX_FRAME_BYTES
            ? WebFault.payloadTooLarge(operation, bytes, MAX_FRAME_BYTES)
            : WebFault.frameInvalid(operation, schemaPathDetail(error)),
        );
        return;
      }

      const acknowledge = (operation: FaultOperation, key: string) => {
        // A success clears only its own operation's data fault, and clears the
        // connection fault only when that fault named the same operation. An
        // unrelated success never clears a retained failure.
        storeRef.current.clearFault(operation, key);
        setConnectionFault((current) => (current && current.operation === operation ? null : current));
      };

      switch (msg.type) {
        case 'connected':
          // The socket is alive, but a valid message is not a recovery signal
          // for a retained data fault; only a reconnect clears a connection
          // fault, and only the same operation's success clears a data fault.
          break;

        case 'error':
          if (msg.topic) {
            const key = msg.topic === ChannelTopic.SessionDetail && msg.id
              ? `${msg.topic}:${msg.id}`
              : msg.topic === ChannelTopic.Annotations && msg.axis && msg.id
                ? `${msg.topic}:${msg.axis}:${msg.id}`
                : msg.topic === ChannelTopic.ProjectFamiliarity && msg.id
                  ? `${msg.topic}:${msg.id}`
                  : msg.topic;
            const operation = faultOperationForTopic(msg.topic);
            const detail = typeof msg.message === 'string' && msg.message.trim().length > 0
              ? msg.message
              : 'the server returned no error detail';
            const code = (msg.data as { code?: string } | undefined)?.code === DiscoveryErrorCode.SelectionVisibility
              ? DiscoveryErrorCode.SelectionVisibility
              : undefined;
            storeRef.current.fail(operation, key, WebFault.topicError(operation, msg.topic, detail, code));
          } else {
            // A refusal with no topic is not one channel's data failure.
            const detail = typeof msg.message === 'string' && msg.message.trim().length > 0
              ? msg.message
              : 'the server returned no error detail';
            setConnectionFault(WebFault.connection(FaultCause.TopicError, detail));
          }
          break;

        case MessageType.Kickstart: {
          // A setup event is delivered to every listener once, in order. One the
          // contract refuses is a bad frame, not a reason to stop listening.
          let event: KickstartEvent;
          try {
            event = parseKickstartEvent(msg.data);
          } catch (error) {
            setConnectionFault(
              WebFault.frameInvalid(
                FaultOperation.Socket,
                error instanceof Error ? error.message : 'a setup update did not match the contract',
              ),
            );
            break;
          }
          for (const listener of Array.from(kickstartListenersRef.current)) listener(event);
          break;
        }

        case 'dashboard':
        case 'sessions':
        case 'trends':
        case 'quality': {
          // Write to the ref-based store — only listeners for THIS channel
          // are notified. No React state change at the provider level.
          const operation = faultOperationForMessageType(msg.type);
          acknowledge(operation, msg.type);
          storeRef.current.set(operation, msg.type, msg.data);
          break;
        }

        case 'session_detail': {
          // Write to both bare key and ID-qualified key so both the
          // legacy ChannelName[] API and typed subscribe.sessionDetail(id) work.
          acknowledge(FaultOperation.SessionDetailRead, msg.type);
          storeRef.current.set(FaultOperation.SessionDetailRead, msg.type, msg.data);
          const payload = msg.data as { id?: string } | undefined;
          if (payload?.id) {
            const key = `session_detail:${payload.id}`;
            acknowledge(FaultOperation.SessionDetailRead, key);
            storeRef.current.set(FaultOperation.SessionDetailRead, key, msg.data);
          }
          break;
        }

        case 'annotations': {
          // Key must match subscriptionKey() format: "annotations:{axis}:{id}"
          const ann = msg.data as { axis?: string; id?: string } | undefined;
          const annKey = ann?.axis && ann?.id
            ? `${msg.type}:${ann.axis}:${ann.id}`
            : msg.type;
          acknowledge(FaultOperation.AnnotationsRead, annKey);
          storeRef.current.set(FaultOperation.AnnotationsRead, annKey, msg.data);
          break;
        }

        case 'project_familiarity': {
          // Key must match subscriptionKey() format: "project_familiarity:{id}".
          // The payload's project identity is `projectHash`, not an `id` member.
          const payload = msg.data as { projectHash?: string } | undefined;
          const key = payload?.projectHash ? `${msg.type}:${payload.projectHash}` : msg.type;
          acknowledge(FaultOperation.ProjectFamiliarity, key);
          storeRef.current.set(FaultOperation.ProjectFamiliarity, key, msg.data);
          break;
        }

        default:
          break;
      }
    };

    ws.onerror = () => {
      if (!mountedRef.current) return;
      // An error on an OPEN socket is not a failed connect: the socket is up and
      // the provider's reconnect loop does not apply. Derive the cause from the
      // socket state so the banner copy stays accurate.
      const connecting = ws.readyState === WebSocket.CONNECTING;
      setConnectionFault(
        WebFault.connection(
          connecting ? FaultCause.ConnectFailed : FaultCause.SocketClosed,
          connecting ? 'the socket could not connect' : 'the socket reported an error',
        ),
      );
    };

    ws.onclose = () => {
      if (!mountedRef.current) return;
      setConnected(false);
      setConnectionFault(
        WebFault.connection(FaultCause.SocketClosed, 'the socket closed'),
      );
      wsRef.current = null;

      const delay = backoffRef.current;
      backoffRef.current = Math.min(delay * BACKOFF_MULTIPLIER, MAX_BACKOFF_MS);
      reconnectTimerRef.current = setTimeout(() => {
        if (mountedRef.current) connect();
      }, delay);
    };
  }, []);

  const reconnect = useCallback(() => {
    if (!mountedRef.current) return;
    const ws = wsRef.current;
    if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;
    if (reconnectTimerRef.current) {
      clearTimeout(reconnectTimerRef.current);
      reconnectTimerRef.current = null;
    }
    backoffRef.current = INITIAL_BACKOFF_MS;
    connect();
  }, [connect]);

  useEffect(() => {
    mountedRef.current = true;
    connect();
    return () => {
      mountedRef.current = false;
      if (reconnectTimerRef.current) {
        clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
      if (wsRef.current) {
        wsRef.current.onclose = null;
        wsRef.current.close();
        wsRef.current = null;
      }
    };
  }, [connect]);

  const wsSubscribe = useCallback((subs: SubscriptionMessage[]) => {
    const newSubs: SubscriptionMessage[] = [];
    for (const sub of subs) {
      const key = subscriptionKey(sub);
      const entry = subsRef.current.get(key);
      if (entry) {
        subsRef.current.set(key, { sub, count: entry.count + 1 });
      } else {
        subsRef.current.set(key, { sub, count: 1 });
        newSubs.push(sub);
      }
    }
    if (newSubs.length > 0) {
      sendRaw({ type: 'subscribe', channels: newSubs });
    }
  }, [sendRaw]);

  const wsUnsubscribe = useCallback((subs: SubscriptionMessage[]) => {
    const removedSubs: SubscriptionMessage[] = [];
    for (const sub of subs) {
      const key = subscriptionKey(sub);
      const entry = subsRef.current.get(key);
      if (!entry) continue;
      if (entry.count <= 1) {
        subsRef.current.delete(key);
        removedSubs.push(sub);
      } else {
        subsRef.current.set(key, { sub, count: entry.count - 1 });
      }
    }
    if (removedSubs.length > 0) {
      sendRaw({ type: 'unsubscribe', channels: removedSubs });
    }
  }, [sendRaw]);

  const wsRefresh = useCallback((subs: SubscriptionMessage[]) => {
    const held = subs.filter((sub) => subsRef.current.has(subscriptionKey(sub)));
    if (held.length > 0) {
      sendRaw({ type: 'subscribe', channels: held });
    }
  }, [sendRaw]);

  const onKickstartEvent = useCallback((listener: (event: KickstartEvent) => void) => {
    kickstartListenersRef.current.add(listener);
    return () => {
      kickstartListenersRef.current.delete(listener);
    };
  }, []);

  // Context value changes ONLY when connected or the connection fault changes
  // (infrequent). Channel data and channel faults live in the ChannelStore ref
  // and are accessed via useSyncExternalStore — they never trigger context
  // re-renders.
  const value: WebSocketContextValue = useMemo(
    () => ({
      connected,
      connectionFault,
      store: storeRef.current,
      wsSubscribe,
      wsUnsubscribe,
      wsRefresh,
      sendMessage: sendRaw,
      onKickstartEvent,
      reconnect,
    }),
    [connected, connectionFault, wsSubscribe, wsUnsubscribe, wsRefresh, sendRaw, onKickstartEvent, reconnect],
  );

  return (
    <WebSocketContext.Provider value={value}>
      {children}
    </WebSocketContext.Provider>
  );
}

// ---------------------------------------------------------------------------
// Hooks
// ---------------------------------------------------------------------------

function useWsContext(): WebSocketContextValue {
  const ctx = useContext(WebSocketContext);
  if (!ctx) {
    throw new Error('useChannel must be used inside <WebSocketProvider>');
  }
  return ctx;
}

/**
 * Normalize the legacy ChannelName-based call API to typed SubscriptionMessage[].
 * Handles: SubscriptionMessage, SubscriptionMessage[], ChannelName[], ChannelName[][0] (single string).
 *
 * @deprecated The ChannelName[] + sessionId? overload exists for backward-compat with existing
 * call sites. New code should pass SubscriptionMessage or SubscriptionMessage[] directly.
 */
function toSubsArray(
  input: SubscriptionMessage | SubscriptionMessage[] | ChannelName[],
  sessionId?: string,
): SubscriptionMessage[] {
  // Already a typed SubscriptionMessage (has .topic property)
  if (!Array.isArray(input)) {
    return [input];
  }
  // Empty array
  if (input.length === 0) return [];

  const first = input[0];
  // If the first element is a string, treat as legacy ChannelName[]
  if (typeof first === 'string') {
    return (input as ChannelName[]).map((ch): SubscriptionMessage => {
      switch (ch) {
        case ChannelTopic.Dashboard:     return mkSub.dashboard();
        case ChannelTopic.Sessions:      return mkSub.sessions();
        case ChannelTopic.Trends:        return mkSub.trends();
        case ChannelTopic.Quality:       return mkSub.quality();
        case ChannelTopic.SessionDetail: return mkSub.sessionDetail(sessionId ?? '');
        default:                         return mkSub.sessions(); // safe fallback
      }
    });
  }
  // Already SubscriptionMessage[]
  return input as SubscriptionMessage[];
}

/**
 * useChannel subscribes to the given subscriptions on mount and unsubscribes
 * on unmount. Channel data is delivered via useSyncExternalStore so that a
 * `sessions` update does NOT re-render a component subscribed to `dashboard`.
 *
 * Returns the latest data for the FIRST subscription in the array (primary),
 * this channel's own topic error (`channelFault`), and the provider-wide
 * socket failure (`connectionFault`). The two faults are separate members: a
 * socket failure is never reported as this channel's data failure.
 *
 * Accepts both the new SubscriptionMessage API and the legacy ChannelName[] + sessionId? API.
 */
export function useChannel<T = unknown>(
  subs: SubscriptionMessage | SubscriptionMessage[] | ChannelName[],
  sessionId?: string,
) {
  const ctx = useWsContext();
  const subsArray = toSubsArray(subs, sessionId);
  const channelsKey = JSON.stringify(subsArray) + (sessionId ?? '');
  const primaryKey = subscriptionKey(subsArray[0]);
  const primaryOperation = faultOperationForTopic(subsArray[0].topic);

  // Server-side subscribe/unsubscribe.
  useEffect(() => {
    ctx.wsSubscribe(subsArray);
    return () => ctx.wsUnsubscribe(subsArray);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [channelsKey]);

  // Subscribe to the primary subscription's data via useSyncExternalStore.
  // Only re-renders when THIS channel's data or fault changes.
  const channelSubscribe = useCallback(
    (onStoreChange: () => void) => ctx.store.subscribe(primaryKey, onStoreChange),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [primaryKey],
  );
  const getSnapshot = useCallback(
    () => ctx.store.get(primaryKey) as T | undefined,
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [primaryKey],
  );
  const data = useSyncExternalStore(channelSubscribe, getSnapshot, getSnapshot);
  const getFaultSnapshot = useCallback(
    () => ctx.store.getFault(primaryOperation, primaryKey),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [primaryKey, primaryOperation],
  );
  const channelFault = useSyncExternalStore(channelSubscribe, getFaultSnapshot, getFaultSnapshot);
  const refresh = useCallback(
    () => ctx.wsRefresh(subsArray),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [channelsKey, ctx.wsRefresh],
  );

  return {
    /** Latest data for the primary channel, or undefined if none received yet. */
    data,
    connected: ctx.connected,
    /** This channel's own topic error, or null. A channel never reads another's. */
    channelFault,
    /** The socket's fault, shared by every consumer and rendered once app-wide. */
    connectionFault: ctx.connectionFault,
    errorCode: codeFromFault(channelFault),
    /** Read this channel again from the server, keeping the current data until the new snapshot lands. */
    refresh,
  };
}

/**
 * Access connection state without subscribing to any channel data, plus the
 * reconnect-now control the local offline notice's `try again` uses. The
 * connection fault is the one provider-wide failure signal the global banner
 * renders.
 */
export function useConnectionState() {
  const ctx = useWsContext();
  return { connected: ctx.connected, connectionFault: ctx.connectionFault, reconnect: ctx.reconnect };
}

/**
 * Subscribe to the live setup topic and hand every event to `listener`.
 *
 * The topic carries no session id: each event names its own, so the caller
 * filters. The subscription is shared with every other page that listens, and
 * the server sends the current snapshot of each open session when it starts.
 * `connected` is returned so a caller can re-read its snapshot after the
 * socket comes back.
 */
export function useKickstartTopic(listener: (event: KickstartEvent) => void): { connected: boolean } {
  const ctx = useWsContext();
  const latest = useRef(listener);
  useEffect(() => {
    latest.current = listener;
  });
  const { wsSubscribe, wsUnsubscribe, onKickstartEvent } = ctx;
  useEffect(() => {
    const subscription = [mkSub.kickstart()];
    wsSubscribe(subscription);
    const stop = onKickstartEvent((event) => latest.current(event));
    return () => {
      stop();
      wsUnsubscribe(subscription);
    };
  }, [wsSubscribe, wsUnsubscribe, onKickstartEvent]);
  return { connected: ctx.connected };
}
