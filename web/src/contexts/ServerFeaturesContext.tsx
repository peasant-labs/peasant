'use client';

import {
  createContext,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from 'react';
import { getApiBaseUrl } from '@/lib/api/base';

/**
 * Server feature gates, fetched once from GET /api/v1/config/features.
 *
 * The dashboard is a static export served by the Go binary, so runtime gates
 * (e.g. `peasant web start --experimental`) cannot ride in as build-time env —
 * the server reports them and the shell adapts. Shelved surfaces stay HIDDEN
 * until the server confirms they are unlocked: the default is `experimental:
 * false` while loading and on any fetch failure, so a default server (or no
 * server at all) never flashes an experimental surface.
 */

export interface ServerFeatures {
  /** Whether the server was started with `--experimental` (unlocks the code map section). */
  experimental: boolean;
}

const DEFAULT_FEATURES: ServerFeatures = { experimental: false };

const ServerFeaturesContext = createContext<ServerFeatures>(DEFAULT_FEATURES);

export function ServerFeaturesProvider({ children }: { children: ReactNode }) {
  const [features, setFeatures] = useState<ServerFeatures>(DEFAULT_FEATURES);

  useEffect(() => {
    let cancelled = false;
    fetch(`${getApiBaseUrl()}/api/v1/config/features`)
      .then((res) => {
        if (!res.ok) throw new Error(`features config: ${res.status}`);
        return res.json();
      })
      .then((data: { experimental?: boolean }) => {
        if (!cancelled) setFeatures({ experimental: data.experimental === true });
      })
      .catch(() => {
        // Fail closed: no experimental surfaces without a server that vouches.
        if (!cancelled) setFeatures(DEFAULT_FEATURES);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <ServerFeaturesContext.Provider value={features}>
      {children}
    </ServerFeaturesContext.Provider>
  );
}

/** Feature gates reported by the server; defaults (all off) without a provider. */
export function useServerFeatures(): ServerFeatures {
  return useContext(ServerFeaturesContext);
}
