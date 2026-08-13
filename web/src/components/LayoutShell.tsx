'use client';

import { type ReactNode } from 'react';
import { WebSocketProvider, useConnectionState } from '@/contexts/WebSocketContext';
import { ServerFeaturesProvider } from '@/contexts/ServerFeaturesContext';
import { TopNavbar } from '@/components/TopNavbar';
import { CommandPalette } from '@/components/command/CommandPalette';
import { DevAnnotateOverlay } from '@/components/dev/DevAnnotateOverlay';
import { TourProvider } from '@/components/tour/TourProvider';

/** Inner component that reads connection state (not channel data). */
function LayoutInner({ children }: { children: ReactNode }) {
  const { connected } = useConnectionState();

  return (
    <TourProvider>
      <TopNavbar connected={connected} />
      {children}
      <CommandPalette />
      {process.env.NODE_ENV === 'development' && <DevAnnotateOverlay />}
    </TourProvider>
  );
}

/**
 * LayoutShell wraps the app in the WebSocket provider and renders the
 * persistent TopNavbar. The WebSocket connection lives here — pages
 * subscribe/unsubscribe to channels without tearing down the socket.
 */
export function LayoutShell({ children }: { children: ReactNode }) {
  return (
    <WebSocketProvider>
      <ServerFeaturesProvider>
        <LayoutInner>{children}</LayoutInner>
      </ServerFeaturesProvider>
    </WebSocketProvider>
  );
}
