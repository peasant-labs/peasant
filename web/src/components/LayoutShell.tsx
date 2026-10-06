'use client';

import { type ReactNode } from 'react';
import { WebSocketProvider } from '@/contexts/WebSocketContext';
import { ServerCapabilitiesProvider } from '@/contexts/ServerCapabilitiesContext';
import { PublishProvider } from '@/contexts/PublishContext';
import { TopNavbar } from '@/components/TopNavbar';
import { LocalOfflineNotice } from '@/components/LocalOfflineNotice';
import { CommandPalette } from '@/components/command/CommandPalette';
import { DevAnnotateOverlay } from '@/components/dev/DevAnnotateOverlay';

/**
 * LayoutShell wraps the app in the WebSocket provider and renders the
 * persistent chrome: the fixed one-row header and, under it while the local app
 * is unreachable, the offline notice (pinned where the screen has room, at the
 * top of the page and scrolling with it elsewhere — see LocalOfflineNotice).
 * `--app-header-height` is where page content starts (the header plus the
 * notice while it shows), which the page body clears. The WebSocket connection
 * lives here — pages subscribe/unsubscribe to channels without tearing down the
 * socket.
 *
 * The first-run tour (components/tour) is not mounted. Its provider, steps and
 * the `data-tour` anchors stay in the tree so it can come back.
 * The publish scan cache and in-flight publishes also live here,
 * so they outlive the pages that use them.
 */
export function LayoutShell({ children }: { children: ReactNode }) {
  return (
    <WebSocketProvider>
      <ServerCapabilitiesProvider>
        <PublishProvider>
          <TopNavbar />
          <LocalOfflineNotice />
          {children}
          <CommandPalette />
          {process.env.NODE_ENV === 'development' && <DevAnnotateOverlay />}
        </PublishProvider>
      </ServerCapabilitiesProvider>
    </WebSocketProvider>
  );
}
