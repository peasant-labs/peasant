import { MapRouter } from './MapRouter';

// Only pre-render the base /map/ path for static export.
// Sub-paths (e.g. /map/my-project) are handled client-side via usePathname,
// with the Go SPA handler serving map/index.html for unmatched paths.
export function generateStaticParams() {
  return [{ segments: [] }];
}

export default function MapPage() {
  return <MapRouter />;
}
