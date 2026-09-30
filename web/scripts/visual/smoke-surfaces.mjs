export const SMOKE_MOCKS = 'web,dashboard,sessions,trends,map,review,qualitySessions,annotations'

export const SMOKE_THEMES = Object.freeze(['dark', 'light'])

export const SMOKE_SURFACE_DEFS = Object.freeze([
  {
    id: 'review-changes',
    label: 'review changes',
    path: ({ project }) => `/review/${encodeURIComponent(project)}/`,
    mount: '.gmp-changes-root',
    cap: '.gmp-changes-root',
  },
  {
    id: 'review-change-detail',
    label: 'review change detail',
    path: ({ project, branch }) => `/review/${encodeURIComponent(project)}/?branch=${encodeURIComponent(branch)}`,
    mount: '.gmp-detail-root',
    cap: '.gmp-detail-root',
  },
  {
    id: 'transcript',
    label: 'transcript',
    path: ({ project, session }) => `/projects/${encodeURIComponent(project)}/${session}/`,
    // The viewer is fairtrade's TranscriptViewer composite now (.txn-app);
    // .tb-detail died with transcript-browser's SessionDetail composer.
    mount: '.txn-app',
    cap: '.txn-app',
  },
  {
    id: 'dashboard',
    label: 'dashboard',
    path: () => '/',
    mount: 'main',
    cap: 'main',
  },
  {
    id: 'map',
    label: 'code map',
    path: ({ project }) => `/map/${encodeURIComponent(project)}/`,
    mount: '.gmp-navigator',
    cap: '.gmp-root',
  },
  {
    id: 'analytics',
    label: 'analytics',
    path: () => '/analytics/',
    mount: '.gan-root',
    cap: '.gan-root',
  },
])

export const SMOKE_SURFACE_SET = Object.freeze(SMOKE_SURFACE_DEFS.map((s) => [s.id, null]))

export const SMOKE_SURFACE_LABELS = Object.freeze(
  Object.fromEntries(SMOKE_SURFACE_DEFS.map((s) => [s.id, s.label])),
)

export function makeSmokeSurfaces({ project, session, branch }) {
  return SMOKE_SURFACE_DEFS.map((s) => ({
    id: s.id,
    path: s.path({ project, session, branch }),
    mount: s.mount,
    cap: s.cap,
  }))
}

// Default project the shell gates open project-scoped route-only pages under (`/review/<hash>/`,
// `/map/<hash>/`): the SAME underlying mock-generator project full-app-smoke.mjs's SMOKE_PROJECT
// default resolves to ('fortuna'), given as its canonical ProjectHash rather than the plain label
// so an exact-path check is not tripped by the legitimate label-to-hash canonicalization.
export const SHELL_DEFAULT_PROJECT = 'eb162ff109780837cd029d2aa990cb3b3f81ad566e678429099debed9ec0514b'

// A mock session of that project: the transcript page the offline arm keeps open while it stops
// the server (the same session boot-peasant.mjs opens).
export const SHELL_DEFAULT_SESSION = 'sess-c3d4e5f6-a7b8-9012-cdef-123456789012'
