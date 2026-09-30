'use client';

import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { LocalOfflineBanner } from '@/lib/ft-ui';
import { useLocalAppHealth } from '@/hooks/useLocalAppHealth';

/** `peasant web start` listens on this port unless told otherwise. */
const DEFAULT_WEB_PORT = '8690';

/**
 * The command that starts the app this page came from. The page is served by
 * that app, so its own port is the one to start it on again.
 */
export function startCommandFor(port: string): string {
  return port && port !== DEFAULT_WEB_PORT ? `peasant web start --port ${port}` : 'peasant web start';
}

/**
 * What the always-mounted live region says. `stopped` repeats the banner's own
 * headline; `stillStopped` is followed by the time of the failed check, so each
 * failed `try again` is read out. The shell manifest
 * (scripts/visual/testdata/shell-header.yaml) holds the same strings for the
 * tests and the mounted gates.
 */
export const OFFLINE_ANNOUNCEMENTS = {
  stopped: "peasant isn't running on this computer.",
  stillStopped: "peasant still isn't running on this computer. checked",
  back: 'peasant is running again.',
} as const;

/**
 * Pinned under the header on a screen with room for it (the `notice-pinned`
 * variant, declared once in globals.css). On a smaller or zoomed screen it
 * scrolls with the page instead, so it can never cover the page or its own
 * `try again`.
 */
const PINNED = 'notice-pinned:fixed';

/**
 * Moves focus to the page body. <main> is focusable only for this move: it
 * takes tabindex -1 now and drops it on blur, so a click in the page never
 * makes <main> the focus (which would steer Tab and keyboard scrolling).
 */
function focusPageBody(): void {
  const main = document.querySelector('main');
  if (!main) return;
  main.setAttribute('tabindex', '-1');
  main.addEventListener('blur', () => main.removeAttribute('tabindex'), { once: true });
  main.focus({ preventScroll: true });
}

/** hh:mm:ss in 24-hour time, as the banner's `last checked` shows it. */
function clockTime(date: Date): string {
  return date.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
}

/**
 * The page-level notice for "the peasant app on this computer stopped", under
 * the fixed header. It renders nothing while the app is reachable.
 *
 * - On a screen with room it is pinned under the header, so a page scrolled
 *   down still shows it; elsewhere it sits at the top of the page and scrolls.
 *   While it shows it writes its height to `--app-notice-height`, so page
 *   content starts below it (`--app-header-height`) and full-height pages fill
 *   what is left (`--app-body-height`, which keeps a floor on short screens).
 * - An always-mounted live region announces the stop, each failed `try again`,
 *   and the return: a status region that mounts with its text is often not read
 *   out, and the banner cannot announce its own removal. It stands in until
 *   fairtrade's banner carries such a region itself.
 * - If focus was inside the notice when the app came back, focus moves to the
 *   page body instead of falling to <body>.
 */
export function LocalOfflineNotice() {
  const { offline, checkedAt, retrying, retry } = useLocalAppHealth();
  const ref = useRef<HTMLDivElement>(null);
  const focusInside = useRef(false);
  const wasOffline = useRef(false);
  // The notice height last published, and whether the notice was pinned then.
  const published = useRef({ height: 0, pinned: false });
  const offlineNow = useRef(offline);
  offlineNow.current = offline;
  const [announcement, setAnnouncement] = useState('');

  // Publish the notice's height. Where the notice scrolls with the page, a
  // reader scrolled down keeps their place: <main>'s top padding grows by the
  // notice (which the browser's scroll anchoring does not follow), so the page
  // scrolls by the same amount, and back when the notice goes.
  useLayoutEffect(() => {
    const element = ref.current;
    if (!offline || !element) return;
    const root = document.documentElement;
    // Instant, even where the page scrolls smoothly: keeping a place must not visibly move it.
    const keepPlace = (delta: number, pinned: boolean) => {
      if (delta !== 0 && !pinned && window.scrollY > 0) window.scrollBy({ top: delta, behavior: 'instant' });
    };
    const publish = () => {
      const height = element.getBoundingClientRect().height;
      const pinned = getComputedStyle(element).position === 'fixed';
      root.style.setProperty('--app-notice-height', `${height}px`);
      keepPlace(height - published.current.height, pinned);
      published.current = { height, pinned };
    };
    publish();
    const observer = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(publish);
    observer?.observe(element);
    return () => {
      observer?.disconnect();
      root.style.removeProperty('--app-notice-height');
      keepPlace(-published.current.height, published.current.pinned);
      published.current = { height: 0, pinned: false };
    };
  }, [offline]);

  // Whether keyboard focus is in the notice: set by focus moving in or out,
  // and cleared by a click elsewhere (a click on plain text moves focus to
  // <body> without a focus event).
  useEffect(() => {
    if (!offline) return;
    const onFocusIn = (event: FocusEvent) => {
      focusInside.current = !!ref.current?.contains(event.target as Node);
    };
    const onPointerDown = (event: PointerEvent) => {
      if (!ref.current?.contains(event.target as Node)) focusInside.current = false;
    };
    document.addEventListener('focusin', onFocusIn);
    document.addEventListener('pointerdown', onPointerDown);
    return () => {
      document.removeEventListener('focusin', onFocusIn);
      document.removeEventListener('pointerdown', onPointerDown);
    };
  }, [offline]);

  useEffect(() => {
    if (offline) {
      wasOffline.current = true;
      setAnnouncement(OFFLINE_ANNOUNCEMENTS.stopped);
      return;
    }
    if (!wasOffline.current) return;
    wasOffline.current = false;
    setAnnouncement(OFFLINE_ANNOUNCEMENTS.back);
    const active = document.activeElement;
    if (focusInside.current && (!active || active === document.body)) focusPageBody();
    focusInside.current = false;
  }, [offline]);

  // A failed `try again` is read out each time: the text is cleared first, so
  // two failures in the same second still change the region. An answer made
  // moot (null: a newer check, or the app came back) says nothing, and nor
  // does a failure the app's return overtakes during the clear.
  const tryAgain = () => {
    void retry().then((answered) => {
      if (answered !== false) return;
      const text = `${OFFLINE_ANNOUNCEMENTS.stillStopped} ${clockTime(new Date())}.`;
      setAnnouncement('');
      setTimeout(() => {
        if (offlineNow.current) setAnnouncement(text);
      }, 50);
    });
  };

  return (
    <>
      <p role="status" className="sr-only">
        {announcement}
      </p>
      {offline && (
        <div ref={ref} className={`absolute inset-x-0 top-[var(--nav-h)] z-40 ${PINNED}`}>
          <LocalOfflineBanner
            onRetry={tryAgain}
            retrying={retrying}
            checkedAt={checkedAt ?? undefined}
            command={startCommandFor(window.location.port)}
          />
        </div>
      )}
    </>
  );
}
