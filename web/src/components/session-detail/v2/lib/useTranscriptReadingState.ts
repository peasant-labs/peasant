import { useEffect, useState } from 'react';
import type { RefObject } from 'react';
import { readTranscriptReadingState, transcriptReadingScrollTop, writeTranscriptReadingState } from './transcriptReadingState';

/**
 * Host-owned per-session reading state for Back navigation.
 *
 * The adapter passes `earlierHistoryOpen` and `activeTurn` through to the shared
 * viewer as controlled props and replays the inner `.txn-stream` scroll offset
 * once the viewer is mounted. Every change is written to the session-scoped
 * record, so following a stored context/parent link away and returning restores
 * the child exactly as it was read. The route query stays in the URL and is
 * restored by the browser's Back.
 *
 * `enabled` is the viewer-ready signal: the effects only touch the DOM after the
 * viewer body exists, and they re-run on the session change that a Back or a
 * follow performs.
 */
export function useTranscriptReadingState(
  sessionId: string,
  hostRef: RefObject<HTMLElement | null>,
  enabled: boolean,
) {
  const [earlierHistoryOpen, setEarlierHistoryOpen] = useState<Record<string, boolean>>({});
  const [activeTurn, setActiveTurn] = useState<number | undefined>(undefined);

  // Hydrate the per-session disclosure + selection record on mount and on every
  // session change, so the state of the session being left never leaks into the
  // one being read.
  useEffect(() => {
    const stored = readTranscriptReadingState(sessionId);
    setEarlierHistoryOpen(stored?.earlierHistoryOpen ?? {});
    setActiveTurn(stored?.activeTurn);
  }, [sessionId]);

  // Persist disclosure + selection as they change, keeping the scroll offset the
  // scroll listener captured.
  useEffect(() => {
    const current = readTranscriptReadingState(sessionId);
    writeTranscriptReadingState(sessionId, {
      scrollTop: current?.scrollTop ?? 0,
      activeTurn,
      earlierHistoryOpen,
    });
  }, [activeTurn, earlierHistoryOpen, sessionId]);

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
    let scroller: HTMLElement | null = null;
    const savedTop = transcriptReadingScrollTop(sessionId);
    let restored = savedTop <= 0;

    const captureScrollTop = () => {
      if (!scroller) return;
      const current = readTranscriptReadingState(sessionId);
      writeTranscriptReadingState(sessionId, {
        scrollTop: scroller.scrollTop,
        activeTurn: current?.activeTurn,
        earlierHistoryOpen: current?.earlierHistoryOpen ?? {},
      });
    };

    const tick = () => {
      if (stopped) return;
      if (!scroller) {
        scroller = host.querySelector<HTMLElement>('.txn-stream');
        if (scroller) scroller.addEventListener('scroll', captureScrollTop, { passive: true });
      }
      if (scroller && !restored) {
        if (Math.abs(scroller.scrollTop - savedTop) > 1) scroller.scrollTop = savedTop;
        if (Math.abs(scroller.scrollTop - savedTop) <= 1) restored = true;
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
  }, [enabled, hostRef, sessionId]);

  return { earlierHistoryOpen, setEarlierHistoryOpen, activeTurn, setActiveTurn };
}
