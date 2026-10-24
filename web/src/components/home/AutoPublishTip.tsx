'use client';

import { useState } from 'react';
import { X, Zap } from 'lucide-react';
import { DocsLink } from '@/components/DocsLink';

/**
 * The Home reminder that sessions can publish on their own. How to set that
 * up lives in the docs, so the tip links there. Dismissing it changes no
 * publishing preference.
 */
export function AutoPublishTip() {
  const [dismissed, setDismissed] = useState(false);
  if (dismissed) return null;
  return (
    <aside aria-label="auto-publish tip" className="home-tip">
      <Zap className="home-tip-icon" aria-hidden="true" />
      <p className="home-tip-copy">
        publish sessions automatically, with secrets hidden.{' '}
        <DocsLink topic="autoPublish" variant="inline">set up</DocsLink>
      </p>
      <button type="button" className="btn btn-ghost btn-sm btn-icon home-tip-dismiss" aria-label="dismiss auto-publish tip" onClick={() => setDismissed(true)}>
        <X aria-hidden="true" />
      </button>
    </aside>
  );
}
