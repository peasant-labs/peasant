'use client';

import {
  createContext,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from 'react';
import { zUICapabilitiesResponse } from '@peasant-labs/schema';
import { getApiBaseUrl } from '@/lib/api/base';
import type { UICapabilityToken } from '@/lib/capabilities/tokens';

/**
 * Server UI capabilities, fetched once from GET /api/v1/config/capabilities.
 *
 * The dashboard is a static export served by the Go binary, so runtime
 * capabilities (e.g. what `peasant web start --experimental` unlocks) cannot
 * ride in as build-time env — the server advertises a token set and the shell
 * adapts. Gated surfaces stay HIDDEN until the server confirms the matching
 * token: the set is EMPTY while loading and after ANY failure, so a default
 * server (or no server at all) never flashes a gated surface.
 *
 * The wire envelope is owned by the schema module; the runtime boundary is
 * `zUICapabilitiesResponse.safeParse`, which fails closed on a non-OK response,
 * a thrown fetch, a null-bearing body, a malformed body, or an unknown
 * top-level key. Unknown advertised token strings parse fine and are ignored by
 * the exact-membership predicate `useHasCapability`.
 */

export type ServerCapabilitiesState = {
  status: 'loading' | 'ready';
  /** Advertised tokens; empty while loading or after any failure (fail closed). */
  capabilities: ReadonlySet<string>;
};

const EMPTY_CAPABILITIES: ReadonlySet<string> = new Set();

/**
 * Default (no provider) state: ready with no capabilities. A component read
 * outside a provider therefore fails closed — every gated surface stays hidden.
 */
const DEFAULT_STATE: ServerCapabilitiesState = {
  status: 'ready',
  capabilities: EMPTY_CAPABILITIES,
};

const ServerCapabilitiesContext = createContext<ServerCapabilitiesState>(DEFAULT_STATE);

export function ServerCapabilitiesProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<ServerCapabilitiesState>({
    status: 'loading',
    capabilities: EMPTY_CAPABILITIES,
  });

  useEffect(() => {
    let cancelled = false;
    const ready = (capabilities: ReadonlySet<string>) => {
      if (!cancelled) setState({ status: 'ready', capabilities });
    };

    fetch(`${getApiBaseUrl()}/api/v1/config/capabilities`)
      .then(async (res) => {
        if (!res.ok) {
          // Fail closed: an error status advertises nothing.
          ready(EMPTY_CAPABILITIES);
          return;
        }
        const body: unknown = await res.json();
        const parsed = zUICapabilitiesResponse.safeParse(body);
        if (!parsed.success) {
          // Malformed, null-bearing, or unknown-top-level-key body → fail closed.
          ready(EMPTY_CAPABILITIES);
          return;
        }
        ready(new Set(parsed.data.uiCapabilities ?? []));
      })
      .catch(() => {
        // Thrown fetch / rejected JSON → fail closed.
        ready(EMPTY_CAPABILITIES);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <ServerCapabilitiesContext.Provider value={state}>
      {children}
    </ServerCapabilitiesContext.Provider>
  );
}

/** The server's advertised capability snapshot; fails closed without a provider. */
export function useServerCapabilities(): ServerCapabilitiesState {
  return useContext(ServerCapabilitiesContext);
}

/**
 * Whether the server advertised `token`, by exact membership. Unknown advertised
 * tokens are ignored by construction (they are never a `UICapabilityToken`).
 */
export function useHasCapability(token: UICapabilityToken): boolean {
  return useContext(ServerCapabilitiesContext).capabilities.has(token);
}
