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

// Default project used to drill the shell gate's map/changes captures past the
// cross-project picker into a representative, project-scoped surface — the
// SAME underlying mock-generator project full-app-smoke.mjs's SMOKE_PROJECT
// default resolves to ('fortuna'), but given here as its canonical
// ProjectHash rather than the plain label: this gate's exact-path assertion
// (shell-nav-gate.mjs's waitForSection) checks the URL is UNCHANGED after
// navigation, and a label route legitimately canonicalizes/redirects to its
// hash (the soft-retained legacy-label behavior), which a label identifier
// would trip here even though it's correct app behavior (verified against a
// live --mock-data-store=...,map,review server).
export const SHELL_DEFAULT_PROJECT = 'eb162ff109780837cd029d2aa990cb3b3f81ad566e678429099debed9ec0514b'

export const GRAPH_SHELL_NAV_DEFS = Object.freeze([
  {
    id: 'shell-analytics',
    label: 'analytics',
    href: '/analytics',
    mount: '.gan-root',
    body: '.gan-root',
    heading: 'project overview',
    // Analytics is a cross-project surface by design — no picker to drill past.
  },
  {
    id: 'shell-changes',
    label: 'changes',
    href: '/',
    mount: '[data-tour="home"]',
    body: '[data-tour="project-picker"], [data-tour="changes-list"]',
    heading: 'Your projects',
    // `/` is the changes-first project picker by design. The shell SxS must show
    // a real review surface, so after confirming the nav lands on `/` correctly,
    // the gate drills into a real project's changes list (`.gmp-changes-root`,
    // the same selector `smoke-surfaces`'s `review-changes` surface mounts).
    representativePath: (project) => `/review/${encodeURIComponent(project)}/`,
    representativeBody: '.gmp-changes-root',
  },
  {
    id: 'shell-map',
    label: 'code map',
    href: '/map',
    mount: 'main',
    body: '[data-tour="project-picker"], .gmp-navigator',
    heading: 'Map',
    // The bare /map route is the cross-project picker. The shell SxS must show
    // representative MOUNTED body content, so after confirming the nav lands on
    // /map correctly, the gate drills into a real project's navigator-first map
    // composition, matching the same surface the full-app smoke mounts.
    representativePath: (project) => `/map/${encodeURIComponent(project)}/`,
    representativeBody: '.gmp-root',
  },
])

export const GRAPH_SHELL_SURFACE_SET = Object.freeze(GRAPH_SHELL_NAV_DEFS.map((s) => [s.id, null]))

export const GRAPH_SHELL_SURFACE_LABELS = Object.freeze(
  Object.fromEntries(GRAPH_SHELL_NAV_DEFS.map((s) => [s.id, s.label])),
)
