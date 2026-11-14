import { loadContextNavigationFixture } from './context-navigation-fixture.mjs'

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
// the server, and the transcript full-app-smoke.mjs and publish-states-shoot.mjs open. It is the
// child of the canonical context-navigation fixture, read off the file the Go mock store serves, so
// its id passes the schema's session-id grammar. The generated mock sessions' `sess-<uuid>` ids do
// not, so the transcript, commits and pull-requests routes answer 400 for them.
export const SHELL_DEFAULT_SESSION = loadContextNavigationFixture().childSessionId

// The commits and pull-requests routes look the session up in the store, not in the mock data
// store, so a sandboxed mock server answers 404 "session not found" for every mock session and the
// transcript renders without them. Chrome still logs each of those responses as a console error;
// the smoke accepts exactly that resource error on exactly these two routes.
const STORE_ONLY_SESSION_EXTRAS = /^\/api\/v1\/sessions\/[^/]+\/(commits|pull-requests)$/
const FAILED_RESOURCE_404 = /^Failed to load resource: the server responded with a status of 404\b/

export function isStoreOnlySessionExtrasMiss(text, locationUrl) {
  if (!FAILED_RESOURCE_404.test(text)) return false
  try {
    return STORE_ONLY_SESSION_EXTRAS.test(new URL(locationUrl).pathname)
  } catch {
    return false
  }
}
