'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { fetchProjectResolution } from '@/lib/api/map';
import { DiscoveryRequestError } from '@/lib/api/errors';
import { parseProjectHash, type ProjectHash } from './projectRoutes';

export type ProjectIdentityState =
  | { phase: 'resolving'; requestedIdentity: string }
  | { phase: 'ready'; requestedIdentity: string; projectHash: ProjectHash; label: string }
  | { phase: 'missing'; requestedIdentity: string; message: string }
  | { phase: 'error'; requestedIdentity: string; message: string };

export function useProjectIdentity(requestedIdentity: string | null): {
  state: ProjectIdentityState;
  retry: () => void;
} {
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<ProjectIdentityState>(() => requestedIdentity
    ? { phase: 'resolving', requestedIdentity }
    : { phase: 'missing', requestedIdentity: '', message: 'No project identity was provided. Pick a project and retry.' });
  const request = useRef(0);

  useEffect(() => {
    const generation = ++request.current;
    if (!requestedIdentity) {
      setState({ phase: 'missing', requestedIdentity: '', message: 'No project identity was provided. Pick a project and retry.' });
      return;
    }
    setState({ phase: 'resolving', requestedIdentity });
    fetchProjectResolution(requestedIdentity)
      .then((payload) => {
        if (generation !== request.current) return;
        const projectHash = parseProjectHash(payload.projectHash);
        if (!projectHash) {
          setState({
            phase: 'error',
            requestedIdentity,
            message: `Project resolution returned malformed identity ${JSON.stringify(payload.projectHash)}. Update Peasant and retry; no stale project was opened.`,
          });
          return;
        }
        setState({ phase: 'ready', requestedIdentity, projectHash, label: payload.project });
      })
      .catch((error: unknown) => {
        if (generation !== request.current) return;
        const message = error instanceof Error ? error.message : 'Project resolution failed without an error message. Retry, then inspect the Peasant server logs.';
        setState(error instanceof DiscoveryRequestError && error.status === 404
          ? { phase: 'missing', requestedIdentity, message }
          : { phase: 'error', requestedIdentity, message });
      });
    return () => {
      request.current++;
    };
  }, [requestedIdentity, revision]);

  const retry = useCallback(() => setRevision((value) => value + 1), []);
  const visibleState: ProjectIdentityState = requestedIdentity
    ? state.requestedIdentity === requestedIdentity
      ? state
      : { phase: 'resolving', requestedIdentity }
    : { phase: 'missing', requestedIdentity: '', message: 'No project identity was provided. Pick a project and retry.' };
  return { state: visibleState, retry };
}
