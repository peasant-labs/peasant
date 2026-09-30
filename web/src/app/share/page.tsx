import { ShareWizardClient } from './ShareWizardClient';

// Direct links and evidence-specific deep links (from the changes and code-map
// pages) enter the same review, redaction, and publishing flow here.
export default function SyncPage() {
  return <ShareWizardClient />;
}
