import { ProjectsRouter } from './ProjectsRouter';

// Only pre-render the base /projects/ path for static export.
// Sub-paths (e.g. /projects/foo/bar) are handled client-side via usePathname,
// with the Go SPA handler serving projects/index.html for unmatched paths.
export function generateStaticParams() {
  return [{ segments: [] }];
}

export default function ProjectsPage() {
  return <ProjectsRouter />;
}
