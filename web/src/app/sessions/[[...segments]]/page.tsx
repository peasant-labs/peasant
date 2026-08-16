import { SessionsRouter } from './SessionsRouter';

// Only pre-render the base /sessions/ path for static export. Sub-paths
// (/sessions/{projectHash}) are handled client-side via usePathname, with the
// Go SPA handler serving sessions/index.html for unmatched paths.
export function generateStaticParams() {
  return [{ segments: [] }];
}

export default function SessionsPage() {
  return <SessionsRouter />;
}
