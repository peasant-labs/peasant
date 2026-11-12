'use client';

import { Button } from '@/lib/ft-ui';
import type { KickstartOverwrite, KickstartSession } from '@/lib/kickstart/model';
import { overwriteNotices } from '@/lib/kickstart/view';

/**
 * What a stale write of this tab replaced in another tab. The server applied
 * the write and named the fields; the notice says so and goes when dismissed.
 */
export function StaleNotice({
  session,
  overwritten,
  dismiss,
}: {
  session: KickstartSession;
  overwritten: readonly KickstartOverwrite[];
  dismiss: () => void;
}) {
  if (overwritten.length === 0) return null;
  return (
    <div className="ks-notice" role="status">
      <span>{overwriteNotices(session, overwritten).join(' ')}</span>
      <Button variant="ghost" size="sm" onClick={dismiss}>
        dismiss
      </Button>
    </div>
  );
}
