import { useEffect, useRef, useState } from 'react';
import { readTranscriptReadingState, transcriptReadingScrollTop, writeTranscriptReadingState } from './transcriptReadingState';

/** The viewer's one inner scroller, whose offset is the saved reading position. */
const STREAM_SELECTOR = '.txn-stream';

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
 * `hostRef` is a callback ref for the element that wraps the mounted viewer. The
 * page renders a loading skeleton instead of that element while it reads the
 * transcript's segments, so the element can appear, disappear and appear again
 * after the first render. The scroll capture and the replay follow the element
 * itself: they attach when it mounts, detach when it unmounts, and re-attach to
 * the next one. The viewer can also replace its own inner scroller (switching
 * to the graph view and back does), so the capture listens on the host in the
 * capture phase rather than on one scroller element.
 *
 * `replayScroll` is false when the route itself carries a `?turn=` target: the
 * composite's one-time position owns the initial scroll there, and a replay on
 * top of it would move the reader the route never asked to move.
 */
export function useTranscriptReadingState(sessionId: string, replayScroll = true) {
  const [host, setHost] = useState<HTMLElement | null>(null);
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
  // session is presented again. Keyed on the mounted host element, so a host
  // that mounts after a loading skeleton gets its listener, and a host that is
  // replaced releases the old one. The viewer resets its reading position on a
  // session change, so the replay re-applies until the offset sticks (the
  // stream may still be short on the first frames after the body mounts).
  useEffect(() => {
    if (!host || typeof window === 'undefined') return;
    let frame = 0;
    let stopped = false;
    let attempts = 0;
    let stable = 0;
    const savedTop = replayScroll ? transcriptReadingScrollTop(sessionId) : 0;
    // The viewer resets its own reading position on a session change, so the
    // replay keeps correcting the offset until it has held for a short settle
    // window; stopping the moment it first sticks would let a later view reset
    // win the race.
    const SETTLE_FRAMES = 30;
    const MAX_REPLAY_FRAMES = 120;

    // Scroll events do not bubble, but they do pass through the host in the
    // capture phase, so one host listener sees every scroller the viewer mounts.
    const captureScrollTop = (event: Event) => {
      const scroller = event.target;
      if (!(scroller instanceof HTMLElement) || !scroller.matches(STREAM_SELECTOR)) return;
      if (owningSession.current !== sessionId) return;
      writeTranscriptReadingState(sessionId, {
        scrollTop: scroller.scrollTop,
        activeTurn: latest.current.activeTurn,
        search: latest.current.search,
      });
    };
    host.addEventListener('scroll', captureScrollTop, { capture: true, passive: true });

    const replay = () => {
      if (stopped || owningSession.current !== sessionId) return;
      const scroller = host.querySelector<HTMLElement>(STREAM_SELECTOR);
      if (scroller) {
        if (Math.abs(scroller.scrollTop - savedTop) > 1) {
          scroller.scrollTop = savedTop;
          stable = 0;
        } else {
          stable += 1;
          if (stable >= SETTLE_FRAMES) return;
        }
      }
      attempts += 1;
      if (attempts < MAX_REPLAY_FRAMES) frame = window.requestAnimationFrame(replay);
    };
    if (savedTop > 0) frame = window.requestAnimationFrame(replay);

    return () => {
      stopped = true;
      window.cancelAnimationFrame(frame);
      host.removeEventListener('scroll', captureScrollTop, { capture: true });
    };
  }, [host, replayScroll, sessionId]);

  return { hostRef: setHost, activeTurn, setActiveTurn, search, setSearch };
}
