/* Served-build provenance: proves a running server serves THIS checkout's web build before a gate
   trusts a capture.

   A stale server, a server from another worktree, or a binary that embeds an earlier `web/out`
   silently invalidates every screenshot and computed-style probe. `assertServedBuild` fails first:
   the served page must reference exactly the chunks `web/out/index.html` does, those chunks must
   carry every marker the caller names (strings only the change under test brings into the bundle),
   and, when a binary is given, the binary must not be older than `web/out`. Node-side only.
 */
import { existsSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const WEB_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')

const chunksOf = (html) => [...new Set(html.match(/\/_next\/static\/chunks\/[^"']+\.js/g) || [])].sort()

/**
 * The markers the local shell brings into the served bundle: the offline notice's height variable
 * and the host live region's own announcements (`back`, `stillStopped`) from the shell manifest.
 * The `stopped` announcement repeats fairtrade's banner headline, so it proves nothing about this
 * build and is not a marker.
 * @param {{ announcements: { back: string, stillStopped: string } }} manifest
 * @returns {string[]}
 */
export function shellProvenanceMarkers(manifest) {
  return ['--app-notice-height', manifest.announcements.back, manifest.announcements.stillStopped]
}

/**
 * Throws unless the server at `origin` serves this checkout's `web/out` carrying every marker;
 * returns the evidence for the log.
 * @param {{ origin: string, markers: readonly string[], bin?: string, webRoot?: string }} options
 * @returns {Promise<{ chunks: string[], markerChunks: Record<string, string[]>, binChecked: boolean }>}
 */
export async function assertServedBuild({ origin, markers, bin, webRoot = WEB_ROOT }) {
  const outIndex = join(webRoot, 'out', 'index.html')
  if (!existsSync(outIndex)) throw new Error(`${outIndex} is missing; run make build`)
  if (bin) {
    if (!existsSync(bin)) throw new Error(`${bin} does not exist; run make build`)
    if (statSync(bin).mtimeMs < statSync(outIndex).mtimeMs) throw new Error(`${bin} is older than ${outIndex}: the binary embeds an earlier web build; run make build`)
  }
  const localChunks = chunksOf(readFileSync(outIndex, 'utf8'))
  const servedChunks = chunksOf(await (await fetch(`${origin}/`)).text())
  if (JSON.stringify(servedChunks) !== JSON.stringify(localChunks)) {
    throw new Error(`the server at ${origin} serves chunks ${JSON.stringify(servedChunks)}, not this checkout's web/out ${JSON.stringify(localChunks)}; it is a stale server or another checkout`)
  }
  const bodies = await Promise.all(servedChunks.map(async (chunk) => [chunk, await (await fetch(`${origin}${chunk}`)).text()]))
  const markerChunks = Object.fromEntries(markers.map((marker) => [marker, bodies.filter(([, body]) => body.includes(marker)).map(([chunk]) => chunk)]))
  const missing = markers.filter((marker) => markerChunks[marker].length === 0)
  if (missing.length) {
    throw new Error(`the bundle served at ${origin} lacks ${JSON.stringify(missing)}: a stale build, or the text changed in the manifest (testdata/shell-header.yaml announcements) without the page`)
  }
  return { chunks: servedChunks, markerChunks, binChecked: !!bin }
}
