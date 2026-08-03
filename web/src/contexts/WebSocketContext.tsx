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
import { subscriptionKey, subscribe as mkSub, ChannelTopic } from '@/types/messages';

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
type ChannelError = { message: string; code?: 'selection_visibility' };

/**
 * ChannelStore holds per-channel data outside React state. Components
 * subscribe to specific channels via useSyncExternalStore, so a `sessions`
 * update does NOT re-render a component that only cares about `dashboard`.
 *
 * Keyed by subscriptionKey() strings (e.g. "dashboard", "session_detail:abc123").
 */
class ChannelStore {
  private data = new Map<string, unknown>();
  private errors = new Map<string, ChannelError>();
  private listeners = new Map<string, Set<ChannelListener>>();

  get(key: string): unknown {
    return this.data.get(key);
  }

  getError(key: string): ChannelError | null {
    return this.errors.get(key) ?? null;
  }

  set(key: string, value: unknown): void {
    const prev = JSON.stringify(this.data.get(key));
    const next = JSON.stringify(value);
    const hadError = this.errors.delete(key);
    if (prev === next && !hadError) return; // no-op if unchanged
    this.data.set(key, value);
    // Notify only listeners for this specific channel.
    const subs = this.listeners.get(key);
    if (subs) {
      for (const fn of subs) fn();
    }
  }

  /** Clear stale data and publish one topic-scoped failure notification. */
  fail(key: string, error: ChannelError): void {
    const previous = this.errors.get(key);
    const changed = this.data.delete(key)
      || previous?.message !== error.message
      || previous?.code !== error.code;
    this.errors.set(key, error);
    if (!changed) return;
    const subs = this.listeners.get(key);
    if (subs) for (const fn of subs) fn();
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
  error: string | null;
  store: ChannelStore;
  /** Subscribe to WS channels on the server. */
  wsSubscribe: (channels: SubscriptionMessage[]) => void;
  /** Unsubscribe from WS channels on the server. */
  wsUnsubscribe: (channels: SubscriptionMessage[]) => void;
  /** Send an arbitrary client message. */
  sendMessage: (msg: ClientMessage) => void;
}

const WebSocketContext = createContext<WebSocketContextValue | null>(null);

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

export function WebSocketProvider({ children }: { children: ReactNode }) {
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Stable refs — never trigger React re-renders.
  const storeRef = useRef(new ChannelStore());
  const wsRef = useRef<WebSocket | null>(null);
  const backoffRef = useRef(INITIAL_BACKOFF_MS);
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const mountedRef = useRef(true);
  // Keyed by subscriptionKey(); stores the full subscription + ref-count for reconnect.
  const subsRef = useRef<Map<string, { sub: SubscriptionMessage; count: number }>>(new Map());

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
      setError(null);
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

      let msg: ServerMessage;
      try {
        msg = JSON.parse(event.data as string) as ServerMessage;
      } catch {
        return;
      }

      switch (msg.type) {
        case 'connected':
          setError(null);
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
            storeRef.current.fail(key, {
              message: msg.message ?? 'This data could not be loaded because the server returned an error without details. Retry, then inspect the Peasant server logs if the failure continues.',
        code: (msg.data as { code?: string } | undefined)?.code === 'selection_visibility'
        ? 'selection_visibility'
        : undefined,
            });
          } else {
            setError(msg.message ?? 'Unknown server error');
          }
          break;

        case 'dashboard':
        case 'sessions':
        case 'trends':
        case 'quality':
          // Write to the ref-based store — only listeners for THIS channel
          // are notified. No React state change at the provider level.
          setError(null);
          storeRef.current.set(msg.type, msg.data);
          break;

        case 'session_detail': {
          // Write to both bare key and ID-qualified key so both the
          // legacy ChannelName[] API and typed subscribe.sessionDetail(id) work.
          setError(null);
          storeRef.current.set(msg.type, msg.data);
          const payload = msg.data as { id?: string } | undefined;
          if (payload?.id) {
            storeRef.current.set(`session_detail:${payload.id}`, msg.data);
          }
          break;
        }

        case 'annotations': {
          // Key must match subscriptionKey() format: "annotations:{axis}:{id}"
          setError(null);
          const ann = msg.data as { axis?: string; id?: string } | undefined;
          const annKey = ann?.axis && ann?.id
            ? `${msg.type}:${ann.axis}:${ann.id}`
            : msg.type;
          storeRef.current.set(annKey, msg.data);
          break;
        }

        default:
          break;
      }
    };

    ws.onerror = () => {
      if (!mountedRef.current) return;
      // User-facing copy stays jargon-free; the connected flag drives the UI.
      setError('Lost the connection to the Peasant app on this computer.');
    };

    ws.onclose = () => {
      if (!mountedRef.current) return;
      setConnected(false);
      wsRef.current = null;

      const delay = backoffRef.current;
      backoffRef.current = Math.min(delay * BACKOFF_MULTIPLIER, MAX_BACKOFF_MS);
      reconnectTimerRef.current = setTimeout(() => {
        if (mountedRef.current) connect();
      }, delay);
    };
  }, []);

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

  // Context value changes ONLY when connected/error change (infrequent).
  // Channel data lives in the ChannelStore ref and is accessed via
  // useSyncExternalStore — it never triggers context re-renders.
  const value: WebSocketContextValue = useMemo(
    () => ({
      connected,
      error,
      store: storeRef.current,
      wsSubscribe,
      wsUnsubscribe,
      sendMessage: sendRaw,
    }),
    [connected, error, wsSubscribe, wsUnsubscribe, sendRaw],
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
 * Returns the latest data for the FIRST subscription in the array (primary).
 * Connection state (connected, error) is also returned.
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

  // Server-side subscribe/unsubscribe.
  useEffect(() => {
    ctx.wsSubscribe(subsArray);
    return () => ctx.wsUnsubscribe(subsArray);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [channelsKey]);

  // Subscribe to the primary subscription's data via useSyncExternalStore.
  // Only re-renders when THIS channel's data changes.
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
  const getErrorSnapshot = useCallback(
    () => ctx.store.getError(primaryKey),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [primaryKey],
  );
  const channelError = useSyncExternalStore(channelSubscribe, getErrorSnapshot, getErrorSnapshot);

  return {
    /** Latest data for the primary channel, or undefined if none received yet. */
    data,
    connected: ctx.connected,
    error: channelError?.message ?? ctx.error,
    errorCode: channelError?.code,
  };
}

/** Access connection state without subscribing to any channel data. */
export function useConnectionState() {
  const ctx = useWsContext();
  return { connected: ctx.connected };
}
