'use client';

import { KickstartFlow } from '@/components/kickstart/KickstartFlow';
import { KickstartScope } from '@/lib/kickstart/values';

/**
 * The standalone selection page: pick which recorded sessions peasant imports,
 * without walking the rest of setup.
 *
 * It is the setup wizard's own component with the `selection` scope. The scope
 * is the only difference: it opens its own live session (the wizard's session,
 * and its answers, are not touched), the server serves only the selection,
 * review and ingest stops for it, and a save writes only the selection because
 * nothing else can be answered here. The tree, the preview, the conflict rows,
 * the save and the import are the same code the wizard draws.
 */
export function SelectionPageClient() {
  return (
    <KickstartFlow
      scope={KickstartScope.Selection}
      title="choose sessions"
      trail={[{ label: 'settings', href: '/settings' }, { label: 'choose sessions' }]}
      ariaLabel="choose sessions"
    />
  );
}
