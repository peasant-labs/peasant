'use client';

import { type ReactNode } from 'react';
import { WebSocketProvider } from '@/contexts/WebSocketContext';
import { ServerCapabilitiesProvider } from '@/contexts/ServerCapabilitiesContext';
import { TopNavbar } from '@/components/TopNavbar';
import { LocalOfflineNotice } from '@/components/LocalOfflineNotice';
import { CommandPalette } from '@/components/command/CommandPalette';
import { DevAnnotateOverlay } from '@/components/dev/DevAnnotateOverlay';

/**
 * LayoutShell wraps the app in the WebSocket provider and renders the
 * persistent top chrome: the header, and under it the offline notice while the
 * local app is unreachable. The chrome is fixed; `--app-header-height` is its
 * height, which the page body clears. The WebSocket connection lives here —
 * pages subscribe/unsubscribe to channels without tearing down the socket.
 *
 * The first-run tour (components/tour) is not mounted. Its provider, steps and
 * the `data-tour` anchors stay in the tree so it can come back.
 */
export function LayoutShell({ children }: { children: ReactNode }) {
  return (
    <WebSocketProvider>
      <ServerCapabilitiesProvider>
        <div className="fixed inset-x-0 top-0 z-50">
          <TopNavbar />
          <LocalOfflineNotice />
        </div>
        {children}
        <CommandPalette />
        {process.env.NODE_ENV === 'development' && <DevAnnotateOverlay />}
      </ServerCapabilitiesProvider>
    </WebSocketProvider>
  );
}
