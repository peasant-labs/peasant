import { SelectionPageClient } from './SelectionPageClient';

// Re-pick which sessions peasant imports. Like /kickstart it stays outside the
// section registry: nothing in the sidebar or the palette links here, and it is
// reached from the selection row on the settings page.
export default function SelectionPage() {
  return <SelectionPageClient />;
}
