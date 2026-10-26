import { Suspense } from 'react';
import { SettingsRoute } from './SettingsRoute';

// Every peasant setting and the auto-publish rules: fairtrade's `settings`
// section, one view per section at `/settings?section=<id>`.
export default function SettingsPage() {
  return <Suspense><SettingsRoute /></Suspense>;
}
