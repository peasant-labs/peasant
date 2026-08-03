/* Mock-project-coordinate validator — the fix for a recurring desync class: JS-side scripts and YAML
   fixtures across this harness family hardcode project name/hash literals independently of the Go
   mock's actual catalog (`internal/mock/provider_map.go`'s `mockRepoProject`/`mockRepoLessProject`),
   with nothing to catch drift when that catalog changes. This has bitten THREE times (the shell/smoke
   script defaults, the transcript-input.yaml fixture, and boot-peasant.mjs's PEASANT_PROJECT default)
   — each time as a misleading downstream failure (a "mount never appeared" / "WS path broken"
   diagnosis) that sent the investigator toward the wrong hypothesis, because the REAL cause (an invalid
   project coordinate 404ing at the project-resolve step) surfaced 12-30+ seconds later as something
   that looked like a transport/adapter regression instead.

   This module closes that gap WITHOUT a new source of truth: it queries the SAME live server every
   script in this family is about to drive anyway, via `GET /api/v1/projects/summary` — the exact REST
   endpoint the Home project picker already uses, so it is guaranteed to reflect whatever the Go mock
   (or a real backend) is currently serving, with zero duplicated/second-sourced data. Call
   `assertKnownProject` (or `assertKnownProjects` for a YAML-fixture matrix) FIRST, before booting
   Puppeteer, so a stale coordinate fails LOUD at resolution time with an obviously-correct diagnosis —
   not as a 12-second timeout deep inside a Puppeteer wait. */

/** @typedef {{ project: string, projectHash: string }} KnownProject */

/**
 * Fetch the live, canonical project catalog from `GET {origin}/api/v1/projects/summary`.
 * @param {string} origin
 * @param {{ where?: string }} [opts]
 * @returns {Promise<KnownProject[]>}
 */
async function fetchKnownProjects(origin, { where = 'validate-mock-coordinates.mjs' } = {}) {
  const base = origin.replace(/\/$/, '')
  const url = `${base}/api/v1/projects/summary`
  let res
  try {
    res = await fetch(url)
  } catch (e) {
    throw new Error(
      `ERROR [${where}] Could not reach the backend to validate mock project coordinates.\n` +
      `  What failed: fetch("${url}") threw: ${e.message}\n` +
      `  Why: no server is listening at "${base}", or it is still starting up.\n` +
      `  Where: validate-mock-coordinates.mjs fetchKnownProjects(), called from ${where}.\n` +
      `  When: before booting Puppeteer, as the coordinate-resolution pre-check.\n` +
      `  Means: this script cannot know whether its configured project coordinate is valid, so it refuses to proceed and risk a misleading 12s+ downstream timeout.\n` +
      `  Fix: start a backend at "${base}" (e.g. \`./bin/peasant web start --port <port> --mock-data-store=...\`) before running this script, or point the origin env var at a running one.`
    )
  }
  if (!res.ok) {
    throw new Error(
      `ERROR [${where}] The backend rejected the project-catalog request used to validate mock coordinates.\n` +
      `  What failed: GET ${url} returned HTTP ${res.status}.\n` +
      `  Why: the server is reachable but /api/v1/projects/summary is erroring or not mounted at this origin.\n` +
      `  Where: validate-mock-coordinates.mjs fetchKnownProjects(), called from ${where}.\n` +
      `  When: before booting Puppeteer, as the coordinate-resolution pre-check.\n` +
      `  Means: this script cannot confirm its configured project coordinate is valid.\n` +
      `  Fix: confirm the origin points at a real peasant server (not a stale/wrong port) and that /api/v1/projects/summary responds 200.`
    )
  }
  const payload = await res.json()
  const projects = Array.isArray(payload?.projects) ? payload.projects : []
  return projects.map((p) => ({ project: p.project, projectHash: p.projectHash }))
}

/**
 * Assert that `coordinate` (a plain project label OR a canonical ProjectHash — both conventions are
 * already in use across this script family) matches a project the live backend actually serves.
 * Throws an actionable error naming the exact invalid coordinate and the current live valid set if it
 * doesn't match anything.
 * @param {string} origin        backend origin, e.g. "http://localhost:8699"
 * @param {string} coordinate    the project label or hash the caller resolved (env var, override, or YAML case value)
 * @param {{ where?: string }} [opts]  `where`: the calling script's filename, for the diagnostic
 * @returns {Promise<void>}
 */
export async function assertKnownProject(origin, coordinate, opts = {}) {
  const where = opts.where || 'validate-mock-coordinates.mjs'
  const known = await fetchKnownProjects(origin, { where })
  const match = known.some((p) => p.project === coordinate || p.projectHash === coordinate)
  if (match) return
  const labels = known.map((p) => p.project).join(', ') || '(none served)'
  const hashes = known.map((p) => p.projectHash).join(', ') || '(none served)'
  throw new Error(
    `ERROR [${where}] Unknown mock project coordinate — this WOULD 404 at the project-resolve step.\n` +
    `  What failed: "${coordinate}" does not match any project label or hash the backend at "${origin}" currently serves.\n` +
    `  Why: this script's configured/default project coordinate has drifted from the mock catalog\n` +
    `       (\`internal/mock/provider_map.go\`'s ProjectSummaries()) — either the coordinate is stale,\n` +
    `       or it points at a real (non-mock) backend that legitimately doesn't have this project.\n` +
    `  Where: validate-mock-coordinates.mjs assertKnownProject(), called from ${where}, BEFORE Puppeteer launched.\n` +
    `  When: the coordinate-resolution pre-check, ahead of any navigation.\n` +
    `  Means: had this check not run, the script would instead fail 12-30+ seconds later with a\n` +
    `       misleading "mount never appeared" / "WS path broken" diagnosis, sending you toward the\n` +
    `       wrong hypothesis (a transport/adapter regression) instead of the real cause (a stale coordinate).\n` +
    `  Fix: use one of the backend's currently-served projects — labels: [${labels}]; hashes: [${hashes}] —\n` +
    `       or, if you intended a real (non-mock) backend, confirm the project actually exists there.`
  )
}

/**
 * Convenience for a YAML-fixture matrix that references many session/project cases: validate every
 * DISTINCT project referenced across all cases ONCE, up front, rather than per-case.
 * @param {string} origin
 * @param {Iterable<string>} coordinates
 * @param {{ where?: string }} [opts]
 * @returns {Promise<void>}
 */
export async function assertKnownProjects(origin, coordinates, opts = {}) {
  const where = opts.where || 'validate-mock-coordinates.mjs'
  const known = await fetchKnownProjects(origin, { where })
  const knownSet = new Set(known.flatMap((p) => [p.project, p.projectHash]))
  const distinct = [...new Set(coordinates)]
  const unknown = distinct.filter((c) => !knownSet.has(c))
  if (!unknown.length) return
  const labels = known.map((p) => p.project).join(', ') || '(none served)'
  const hashes = known.map((p) => p.projectHash).join(', ') || '(none served)'
  throw new Error(
    `ERROR [${where}] Unknown mock project coordinate(s) across the fixture matrix — these WOULD 404 at the project-resolve step.\n` +
    `  What failed: [${unknown.join(', ')}] do not match any project label or hash the backend at "${origin}" currently serves.\n` +
    `  Why: one or more fixture rows reference a project coordinate that has drifted from the mock catalog\n` +
    `       (\`internal/mock/provider_map.go\`'s ProjectSummaries()).\n` +
    `  Where: validate-mock-coordinates.mjs assertKnownProjects(), called from ${where}, BEFORE Puppeteer launched.\n` +
    `  When: the coordinate-resolution pre-check, ahead of driving any fixture case.\n` +
    `  Means: had this check not run, the affected case(s) would instead fail deep inside the per-case\n` +
    `       Puppeteer run with a misleading downstream diagnosis instead of this precise, up-front one.\n` +
    `  Fix: correct the unknown coordinate(s) in the fixture to one of the backend's currently-served\n` +
    `       projects — labels: [${labels}]; hashes: [${hashes}].`
  )
}
