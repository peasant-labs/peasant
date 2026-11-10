'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { parseSessionDetailPayloadValue, zSessionDetailReadPayload, type SessionDetailPayload } from '@peasant-labs/schema';
import { decodeLocalAnswer, localRefusal, readLocalAnswer } from '@/lib/api/validated-local';

type HistoryRead = { key: string; detail: SessionDetailPayload | null; loading: boolean; error: string | null };

// Earlier-history turns have no segment or window read. This potentially large
// durable read is allowed only for the reader's explicit history action, never
// for transcript mounting, navigation, jumping, or unrelated page controls.
export function useEarlierHistory(sessionId: string, revision: string) {
  const key = JSON.stringify([sessionId, revision]);
  const [state, setState] = useState<HistoryRead>({ key, detail: null, loading: false, error: null });
  const generation = useRef(0);
  const request = useRef<{ key: string; controller: AbortController; promise: Promise<void> } | null>(null);
  const cache = useRef<{ key: string; detail: SessionDetailPayload } | null>(null);

  useEffect(() => {
    generation.current += 1;
    cache.current = null;
    setState({ key, detail: null, loading: false, error: null });
    return () => {
      generation.current += 1;
      request.current?.controller.abort();
      request.current = null;
    };
  }, [key]);

  const load = useCallback((): Promise<void> => {
    if (cache.current?.key === key) return Promise.resolve();
    if (request.current?.key === key) return request.current.promise;
    const controller = new AbortController();
    const ticket = generation.current;
    const path = `/api/v1/sessions/${encodeURIComponent(sessionId)}`;
    setState({ key, detail: null, loading: true, error: null });
    const promise = (async () => {
      try {
        const answer = await readLocalAnswer(path, { signal: controller.signal });
        if (!answer.ok) throw localRefusal(path, answer);
        const read = decodeLocalAnswer(path, answer, zSessionDetailReadPayload);
        if (read.id !== sessionId || read.revision !== revision) {
          throw new Error('the session or revision changed while loading earlier history. refresh the transcript and try again.');
        }
        const { relationshipNavigation: _navigation, teammate: _teammate, title: _title, medians: _medians, revision: _revision, ...wire } = read;
        const detail = parseSessionDetailPayloadValue(wire);
        if (controller.signal.aborted || generation.current !== ticket) return;
        cache.current = { key, detail };
        setState({ key, detail, loading: false, error: null });
      } catch (error) {
        if (controller.signal.aborted || generation.current !== ticket) return;
        setState({ key, detail: null, loading: false, error: `earlier history could not be loaded: ${error instanceof Error ? error.message : 'unexpected read failure'}. try again.` });
      } finally {
        if (request.current?.controller === controller) request.current = null;
      }
    })();
    request.current = { key, controller, promise };
    return promise;
  }, [key, revision, sessionId]);

  const live = state.key === key ? state : { key, detail: null, loading: false, error: null };
  return { ...live, load };
}
