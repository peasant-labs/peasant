'use client';

import Link from 'next/link';
import { MapPinOff } from 'lucide-react';
import { EmptyState } from '@/lib/ft-ui';

// The body of the not-found page: fairtrade's empty state, with the miss as
// the page heading and a way back home. A client component because the
// fairtrade barrel is one, and the icon is passed to it as a component.
export function NotFoundState() {
  return (
    <div className="asb-page animate-fade-up" data-testid="not-found">
      <EmptyState
        as="h1"
        icon={MapPinOff}
        title="page not found"
        message="nothing lives at this address on this computer. the link may be mistyped, or the page may have moved."
        action={
          <Link href="/" className="btn btn-secondary btn-sm">
            go to home
          </Link>
        }
      />
    </div>
  );
}
