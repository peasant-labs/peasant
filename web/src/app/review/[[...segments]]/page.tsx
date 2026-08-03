import { Suspense } from 'react';
import { ReviewRouter } from './ReviewRouter';

// Only pre-render the base /review/ path for static export.
// Sub-paths (e.g. /review/my-project?branch=feat/x) are handled client-side
// via usePathname/useSearchParams, with the Go SPA handler serving
// review/index.html for unmatched paths.
export function generateStaticParams() {
  return [{ segments: [] }];
}

export default function ReviewPage() {
  return (
    // useSearchParams (the ?branch= selector) requires a Suspense boundary
    // for static export prerendering.
    <Suspense>
      <ReviewRouter />
    </Suspense>
  );
}
