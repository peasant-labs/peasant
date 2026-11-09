'use client';

import { FeedbackPanel } from '@/lib/ft-ui';
import { useConnectionState } from '@/contexts/WebSocketContext';

/**
 * The one app-level note for a provider-wide socket or frame failure.
 *
 * A stopped app is the offline notice's story (start command, `try again`), so
 * this notice renders only while the socket is open. A frame the schema refused,
 * or an oversized frame, leaves the socket up; that failure is stated here once,
 * for every page, instead of surfacing as each list's own data failure.
 *
 * There is no manual control. The provider clears the fault when the socket
 * returns, and the same operation's next good frame clears a frame failure.
 */
export function ConnectionFaultNotice() {
  const { connected, connectionFault } = useConnectionState();
  if (!connected || !connectionFault) return null;
  return (
    <div role="status" data-testid="connection-fault-notice" data-connection-fault={connectionFault.cause.wire}>
      <FeedbackPanel variant="error">{connectionFault.message}</FeedbackPanel>
    </div>
  );
}
