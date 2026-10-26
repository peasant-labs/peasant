'use client';

import { useSearchParams } from 'next/navigation';
import SettingsPageClient from './SettingsPageClient';

/** The settings page on the section its link names. */
export function SettingsRoute() {
  const searchParams = useSearchParams();
  return <SettingsPageClient section={searchParams?.get('section')} />;
}
