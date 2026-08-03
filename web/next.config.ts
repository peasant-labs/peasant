import type { NextConfig } from 'next';

const nextConfig: NextConfig = {
  // Static export only in production builds. In dev mode, Next.js handles
  // dynamic catch-all routes natively; in production the Go SPA handler
  // serves projects/index.html for all /projects/* sub-paths.
  ...(process.env.NODE_ENV === 'production' ? { output: 'export' as const } : {}),
  // Generate directory-style output (projects/index.html instead of projects.html)
  // so the Go SPA handler's nearestIndex() finds the correct fallback page.
  trailingSlash: true,
};

export default nextConfig;
