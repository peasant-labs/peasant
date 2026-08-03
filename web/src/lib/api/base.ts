/**
 * Derive the local API base URL from the current page origin so the dashboard
 * works on whatever `--port` the server was started with. Falls back to the
 * default dev port during SSR/tests where `window` is absent.
 *
 * Single source of truth — every fetch helper resolves the base through here.
 */
export function getApiBaseUrl(): string {
  return (
    process.env.NEXT_PUBLIC_API_URL ??
    (typeof window !== 'undefined' ? window.location.origin : 'http://localhost:8690')
  );
}
