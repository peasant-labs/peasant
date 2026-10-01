'use client';

import { useState } from 'react';
import { GitBranch, X } from 'lucide-react';
import { ProviderName } from '@/lib/ft-ui';

/** The Home reminder; dismissing it changes no publishing preference. */
export function AutoPublishTip() {
  const [dismissed, setDismissed] = useState(false);
  if (dismissed) return null;
  return (
    <aside aria-label="auto-publish tip" className="flex items-center gap-3 border border-rule bg-surface-2 py-1.5 pl-4 pr-1.5 text-base text-ink-2">
      <GitBranch size={16} className="shrink-0" aria-hidden="true" />
      <p className="min-w-0 flex-1">
        <strong className="font-bold text-ink">publish automatically:</strong>{' '}
        tick the auto-publish box the next time you publish, or run <code className="mono text-sm text-ink">/peasant auto</code> in <ProviderName harness="claude-code" />.
      </p>
      <button type="button" className="btn btn-ghost btn-sm shrink-0" aria-label="dismiss auto-publish tip" onClick={() => setDismissed(true)}>
        <X size={16} aria-hidden="true" />
      </button>
    </aside>
  );
}
