'use client';

import { useEffect, useRef, useState, type ReactNode } from 'react';
import { usePathname } from 'next/navigation';
import { WebSocketProvider } from '@/contexts/WebSocketContext';
import { ServerCapabilitiesProvider } from '@/contexts/ServerCapabilitiesContext';
import { PublishProvider } from '@/contexts/PublishContext';
import { AppSidebarLayout } from '@/lib/ft-ui';
import { LocalSidebar } from '@/components/LocalSidebar';
import { LocalOfflineNotice } from '@/components/LocalOfflineNotice';
import { CommandPalette } from '@/components/command/CommandPalette';
import { DevAnnotateOverlay } from '@/components/dev/DevAnnotateOverlay';
import { playEnter } from '@/lib/motion/enter';
import { parseTranscriptRoute } from '@/lib/navigation/projectRoutes';

/** The transcript holds dialogs that position against the screen, so it only fades in. */
const FADE_ONLY = ['/projects'];

/**
 * A page arriving in the shell: the page body fades and rises into place on
 * each route change (lib/motion/enter.ts), and only fades where a page holds
 * screen-positioned dialogs, so they never ride a moving box. The first page
 * is already on screen when the app starts, so it does not play.
 */
function usePageEnter(): void {
  const pathname = usePathname() ?? '/';
  const first = useRef(true);
  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    const main = document.querySelector('main');
    if (!main) return;
    const rise = !FADE_ONLY.some((prefix) => pathname === prefix || pathname.startsWith(`${prefix}/`));
    return playEnter(main, { rise });
  }, [pathname]);
}

function PageEnter() {
  usePageEnter();
  return null;
}

/**
 * LayoutShell wraps the app in the WebSocket provider and renders the
 * persistent chrome: fairtrade's sidebar beside the page (a top bar with a
 * drawer on a phone) and, at the top of the page column while the local app is
 * unreachable, the offline notice (pinned where the screen has room, scrolling
 * with the page elsewhere — see LocalOfflineNotice). The WebSocket connection
 * lives here — pages subscribe/unsubscribe to channels without tearing down the
 * socket.
 *
 * The first-run tour (components/tour) is not mounted. Its provider, steps and
 * the `data-tour` anchors stay in the tree so it can come back.
 * The publish scan cache and in-flight publishes also live here,
 * so they outlive the pages that use them.
 */
export function LayoutShell({ children }: { children: ReactNode }) {
  const pathname = usePathname() ?? '/';
  // Static export renders the shell at its fallback route. Start with that
  // same unbounded shell during hydration, then apply the actual route. React
  // does not repair a server/client class mismatch on the existing layout.
  const [hydrated, setHydrated] = useState(false);
  useEffect(() => setHydrated(true), []);
  const route = parseTranscriptRoute(pathname);
  const viewport = hydrated && (route.kind === 'canonical' || route.kind === 'legacy');
  return (
    <WebSocketProvider>
      <ServerCapabilitiesProvider>
        <PublishProvider>
          <AppSidebarLayout sidebar={<LocalSidebar />} viewport={viewport} notice={<LocalOfflineNotice />} mainAs="main">
            {children}
          </AppSidebarLayout>
          <PageEnter />
          <CommandPalette />
          {process.env.NODE_ENV === 'development' && <DevAnnotateOverlay />}
        </PublishProvider>
      </ServerCapabilitiesProvider>
    </WebSocketProvider>
  );
}
