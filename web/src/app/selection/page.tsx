import { FirstRunGate } from '@/hooks/useFirstRun';
import { SelectionPageClient } from './SelectionPageClient';

// Re-pick which sessions peasant imports. Like /kickstart it stays outside the
// section registry: nothing in the sidebar or the palette links here, and it is
// reached from the selection row on the settings page.
//
// The page opens a session of its own as soon as it is drawn, so its body waits
// for the first-run decision: an install with no setup is sent to setup, and
// this page starts nothing on the way.
export default function SelectionPage() {
  return (
    <FirstRunGate>
      <SelectionPageClient />
    </FirstRunGate>
  );
}
