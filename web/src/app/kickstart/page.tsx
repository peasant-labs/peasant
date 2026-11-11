import { Suspense } from 'react';
import { WizardClient } from './WizardClient';

// First-run setup, and "run setup again" from settings. It stays outside the
// section registry: nothing in the sidebar or the palette links here, and it is
// reached by the first-run redirect or the settings link.
export default function KickstartPage() {
  return (
    <Suspense>
      <WizardClient />
    </Suspense>
  );
}
