'use client';

import { useSearchParams } from 'next/navigation';
import { KickstartFlow } from '@/components/kickstart/KickstartFlow';
import { KICKSTART_ORIGIN_PARAM, KickstartOrigin, parseKickstartOrigin } from '@/lib/kickstart/entry';
import { KickstartScope } from '@/lib/kickstart/values';

/**
 * The full setup wizard: every stop the server serves for the `full` scope,
 * drawn from one live session that every tab of this scope shares. A visit from
 * settings says so in its trail; the saved choices are filled in either way,
 * because the server builds the session from the saved file.
 */
export function WizardClient() {
  const origin = parseKickstartOrigin(useSearchParams()?.get(KICKSTART_ORIGIN_PARAM));
  const trail = origin === KickstartOrigin.Settings
    ? [{ label: 'settings', href: '/settings' }, { label: 'set up peasant' }]
    : [{ label: 'set up peasant' }];
  return <KickstartFlow scope={KickstartScope.Full} title="set up peasant" trail={trail} ariaLabel="set up peasant" />;
}
