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

/** What the always-mounted live region says when the app stops and when it is back. */
export const OFFLINE_ANNOUNCEMENTS = {
  stopped: 'peasant stopped on this computer.',
  back: 'peasant is running again.',
} as const;

/**
 * Moves focus to the page body when it would otherwise fall to <body> because
 * the notice holding it went away.
 */
function focusPageBody(): void {
  const main = document.querySelector('main');
  if (!main) return;
  if (!main.hasAttribute('tabindex')) main.setAttribute('tabindex', '-1');
  main.focus({ preventScroll: true });
}

/**
 * The page-level notice for "the peasant app on this computer stopped", at the
 * top of the page under the fixed header. It renders nothing while the app is
 * reachable.
 *
 * - It is not fixed: it scrolls with the page, so on a short or zoomed screen
 *   the page and its own `try again` stay reachable. While it shows it writes
 *   its height to `--app-notice-height`, so `--app-header-height` (where page
 *   content starts) grows by exactly the notice and full-height pages still fit
 *   below it; it clears the height when it goes.
 * - An always-mounted live region announces the change both ways, since a
 *   status region that mounts with its text is often not read out.
 * - If focus was inside the notice when the app came back, focus moves to the
 *   page body instead of falling to <body>.
 */
export function LocalOfflineNotice() {
  const { offline, checkedAt, retrying, retry } = useLocalAppHealth();
  const ref = useRef<HTMLDivElement>(null);
  const focusInside = useRef(false);
  const wasOffline = useRef(false);
  const [announcement, setAnnouncement] = useState('');

  useLayoutEffect(() => {
    const element = ref.current;
    if (!offline || !element) return;
    const root = document.documentElement;
    const publish = () => root.style.setProperty('--app-notice-height', `${element.getBoundingClientRect().height}px`);
    publish();
    const observer = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(publish);
    observer?.observe(element);
    return () => {
      observer?.disconnect();
      root.style.removeProperty('--app-notice-height');
    };
  }, [offline]);

  useEffect(() => {
    if (!offline) return;
    const onFocusIn = (event: FocusEvent) => {
      focusInside.current = !!ref.current?.contains(event.target as Node);
    };
    document.addEventListener('focusin', onFocusIn);
    return () => document.removeEventListener('focusin', onFocusIn);
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

  return (
    <>
      <p role="status" className="sr-only">
        {announcement}
      </p>
      {offline && (
        <div ref={ref} className="absolute inset-x-0 top-[var(--nav-h)] z-40">
          <LocalOfflineBanner
            onRetry={retry}
            retrying={retrying}
            checkedAt={checkedAt ?? undefined}
            command={startCommandFor(window.location.port)}
          />
        </div>
      )}
    </>
  );
}
