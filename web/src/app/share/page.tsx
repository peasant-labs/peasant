import { SharePageClient } from './SharePageClient';

// The canonical publish route. A link that names one session opens that
// transcript with its publish popup; direct visits and evidence-specific deep
// links (from the changes and code-map pages) enter the multi-session wizard.
export default function SyncPage() {
  return <SharePageClient />;
}
