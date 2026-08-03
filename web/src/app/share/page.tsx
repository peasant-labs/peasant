import { ShareWizardClient } from './ShareWizardClient';

// The persistent top-nav share action and evidence-specific deep links both
// enter the same review, redaction, and publishing flow here.
export default function SyncPage() {
  return <ShareWizardClient />;
}
