'use client';

import { closed } from '@peasant-labs/fairtrade/closed-value';

import { useCallback, useEffect, useRef, useState } from 'react';
import { fetchProjectResolution } from '@/lib/api/map';
import { DiscoveryRequestError } from '@/lib/api/errors';
import { parseProjectHash, type ProjectHash } from './projectRoutes';

export const ProjectIdentityPhase = Object.freeze({ Resolving: closed('resolving', 'resolving').wire, Ready: closed('ready', 'ready').wire, Missing: closed('missing', 'missing').wire, Error: closed('error', 'error').wire } as const);

export type ProjectIdentityState =
  | { phase: typeof ProjectIdentityPhase.Resolving; requestedIdentity: string }
  | { phase: typeof ProjectIdentityPhase.Ready; requestedIdentity: string; projectHash: ProjectHash; label: string }
  | { phase: typeof ProjectIdentityPhase.Missing; requestedIdentity: string; message: string }
  | { phase: typeof ProjectIdentityPhase.Error; requestedIdentity: string; message: string };

export function useProjectIdentity(requestedIdentity: string | null): {
  state: ProjectIdentityState;
  retry: () => void;
} {
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<ProjectIdentityState>(() => requestedIdentity
    ? { phase: ProjectIdentityPhase.Resolving, requestedIdentity }
    : { phase: ProjectIdentityPhase.Missing, requestedIdentity: '', message: 'No project identity was provided. Pick a project and retry.' });
  const request = useRef(0);

  useEffect(() => {
    const generation = ++request.current;
    if (!requestedIdentity) {
      setState({ phase: ProjectIdentityPhase.Missing, requestedIdentity: '', message: 'No project identity was provided. Pick a project and retry.' });
      return;
    }
    setState({ phase: ProjectIdentityPhase.Resolving, requestedIdentity });
    fetchProjectResolution(requestedIdentity)
      .then((payload) => {
        if (generation !== request.current) return;
        const projectHash = parseProjectHash(payload.projectHash);
        if (!projectHash) {
          setState({
            phase: ProjectIdentityPhase.Error,
            requestedIdentity,
            message: `Project resolution returned malformed identity ${JSON.stringify(payload.projectHash)}. Update Peasant and retry; no stale project was opened.`,
          });
          return;
        }
        setState({ phase: ProjectIdentityPhase.Ready, requestedIdentity, projectHash, label: payload.project });
      })
      .catch((error: unknown) => {
        if (generation !== request.current) return;
        const message = error instanceof DiscoveryRequestError
          ? error.detail
          : error instanceof Error ? error.message : 'Project resolution failed without an error message. Retry, then inspect the Peasant server logs.';
        setState(error instanceof DiscoveryRequestError && error.status === 404
          ? { phase: ProjectIdentityPhase.Missing, requestedIdentity, message }
          : { phase: ProjectIdentityPhase.Error, requestedIdentity, message });
      });
    return () => {
      request.current++;
    };
  }, [requestedIdentity, revision]);

  const retry = useCallback(() => setRevision((value) => value + 1), []);
  const visibleState: ProjectIdentityState = requestedIdentity
    ? state.requestedIdentity === requestedIdentity
      ? state
      : { phase: ProjectIdentityPhase.Resolving, requestedIdentity }
    : { phase: ProjectIdentityPhase.Missing, requestedIdentity: '', message: 'No project identity was provided. Pick a project and retry.' };
  return { state: visibleState, retry };
}
