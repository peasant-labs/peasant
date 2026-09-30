'use client';

import { useLayoutEffect, useRef } from 'react';
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
 * The page-level notice for "the peasant app on this computer stopped", under
 * the header. It renders nothing while the app is reachable. While it shows it
 * writes its height to `--app-notice-height`, so the fixed top chrome
 * (`--app-header-height`) grows by exactly the notice and no page content sits
 * under it; it clears the height when it goes.
 */
export function LocalOfflineNotice() {
  const { offline, checkedAt, retrying, retry } = useLocalAppHealth();
  const ref = useRef<HTMLDivElement>(null);

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

  if (!offline) return null;
  return (
    <div ref={ref}>
      <LocalOfflineBanner
        onRetry={retry}
        retrying={retrying}
        checkedAt={checkedAt ?? undefined}
        command={startCommandFor(window.location.port)}
      />
    </div>
  );
}
