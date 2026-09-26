import { useEffect, useRef, useState } from 'react';
import type { RefObject } from 'react';
import { readTranscriptReadingState, transcriptReadingScrollTop, writeTranscriptReadingState } from './transcriptReadingState';

/**
 * Host-owned per-session reading state for Back navigation.
 *
 * The adapter passes `activeTurn` and `search` through to the shared viewer as
 * controlled props and replays the inner `.txn-stream` scroll offset once the
 * viewer is mounted. Every change is written to the session-scoped record, so
 * following a stored context/parent link away and returning restores the child
 * exactly as it was read. The route query stays in the URL and is restored by
 * the browser's Back — including the retained-history disclosure, which is read
 * state the route owns rather than this record.
 *
 * `enabled` is the viewer-ready signal: the scroll effect only touches the DOM
 * after the viewer body exists, and it re-runs on the session change that a Back
 * or a follow performs. `replayScroll` is false when the route itself carries a
 * `?turn=` target: the composite's one-time position owns the initial scroll
 * there, and a replay on top of it would move the reader the route never asked
 * to move.
 */
export function useTranscriptReadingState(
  sessionId: string,
  hostRef: RefObject<HTMLElement | null>,
  enabled: boolean,
  replayScroll = true,
) {
  const [activeTurn, setActiveTurn] = useState<number | undefined>(undefined);
  const [search, setSearch] = useState<string>('');
  // The session that currently owns the mounted stream. A session change renders
  // before its DOM effects, so the render-time assignment lets the outgoing
  // session's still-attached scroll listener ignore the incoming session's
  // position reset instead of writing it into the outgoing session's record.
  const owningSession = useRef(sessionId);
  owningSession.current = sessionId;
  // The latest selection and query, so the scroll listener persists a consistent
  // record without re-subscribing on every change.
  const latest = useRef({ activeTurn, search });
  latest.current = { activeTurn, search };
  // The session whose stored record has already been hydrated. Until then the
  // persist effect must not write its pre-hydration defaults over that record.
  const hydrated = useRef<string | null>(null);

  // Hydrate the per-session selection + query record on mount and on every
  // session change, so the state of the session being left never leaks into the
  // one being read.
  useEffect(() => {
    const stored = readTranscriptReadingState(sessionId);
    setActiveTurn(stored?.activeTurn);
    setSearch(stored?.search ?? '');
    hydrated.current = sessionId;
  }, [sessionId]);

  // Persist selection + query as they change, keeping the scroll offset the
  // scroll listener captured.
  useEffect(() => {
    if (hydrated.current !== sessionId) return;
    const current = readTranscriptReadingState(sessionId);
    writeTranscriptReadingState(sessionId, {
      scrollTop: current?.scrollTop ?? 0,
      activeTurn,
      search,
    });
  }, [activeTurn, search, sessionId]);

  // Capture the inner scroller offset while reading and replay it when this
  // session is presented again. The viewer resets its reading position on a
  // session change, so the replay re-applies until the offset sticks (the
  // stream may still be short on the first frames after the body mounts).
  useEffect(() => {
    const host = hostRef.current;
    if (!enabled || !host || typeof window === 'undefined') return;
    let frame = 0;
    let stopped = false;
    let attempts = 0;
    let stable = 0;
    let scroller: HTMLElement | null = null;
    const savedTop = replayScroll ? transcriptReadingScrollTop(sessionId) : 0;
    // The viewer resets its own reading position on a session change, so the
    // replay keeps correcting the offset until it has held for a short settle
    // window; stopping the moment it first sticks would let a later view reset
    // win the race.
    const SETTLE_FRAMES = 30;
    let restored = savedTop <= 0;

    const captureScrollTop = () => {
      if (!scroller || owningSession.current !== sessionId) return;
      writeTranscriptReadingState(sessionId, {
        scrollTop: scroller.scrollTop,
        activeTurn: latest.current.activeTurn,
        search: latest.current.search,
      });
    };

    const tick = () => {
      if (stopped || owningSession.current !== sessionId) return;
      if (!scroller) {
        scroller = host.querySelector<HTMLElement>('.txn-stream');
        if (scroller) scroller.addEventListener('scroll', captureScrollTop, { passive: true });
      }
      if (scroller && !restored) {
        if (Math.abs(scroller.scrollTop - savedTop) > 1) {
          scroller.scrollTop = savedTop;
          stable = 0;
        } else {
          stable += 1;
          if (stable >= SETTLE_FRAMES) restored = true;
        }
      }
      attempts += 1;
      if ((!scroller || !restored) && attempts < 120) frame = window.requestAnimationFrame(tick);
    };
    frame = window.requestAnimationFrame(tick);

    return () => {
      stopped = true;
      window.cancelAnimationFrame(frame);
      scroller?.removeEventListener('scroll', captureScrollTop);
    };
  }, [enabled, hostRef, replayScroll, sessionId]);

  return { activeTurn, setActiveTurn, search, setSearch };
}
