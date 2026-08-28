import { createHash } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { Harness } from '@peasant-labs/schema'

const HERE = dirname(fileURLToPath(import.meta.url))
const WEB = resolve(HERE, '..')
const REPO = resolve(WEB, '..')
const OUT = resolve(WEB, 'out')
const BINARY = resolve(REPO, 'bin/peasant')
const OFFICIAL_STRIKE_PATH = 'M18.5 6L11 17.5h5.2L13.5 26 21 14.5h-5.2L18.5 6z'
const STRIKE_POLICY_MARKERS = Object.freeze([
  'strike:"Strike"',
  'strike:"strike"',
  'strike:"clay"',
  'strike:"6 6 20 20"',
  OFFICIAL_STRIKE_PATH,
  'Provider harness validation failed',
])
const ROUTES = Object.freeze([
  ['projects', resolve(OUT, 'index.html')],
  ['transcript', resolve(OUT, 'projects/index.html')],
  ['Map', resolve(OUT, 'map/index.html')],
  ['Changes', resolve(OUT, 'review/index.html')],
])
const LEGACY_POLICY_IDENTITIES = Object.freeze([
  'claude-code',
  'gemini-cli',
  'codex',
  'opencode',
  'cursor',
  'antigravity',
])

function fail(what, why, where, meaning, fix) {
  throw new Error(
    `Provider build provenance failed because ${what}; ${why}; at ${where} after the production web and Go builds completed; ${meaning}; ${fix}.`,
  )
}

function routeChunks(route, htmlPath) {
  if (!existsSync(htmlPath)) {
    fail(
      `${route} export ${htmlPath} is absent`,
      'the route was not emitted by the clean Next production export',
      'provider-build-provenance.mjs route discovery',
      `${route} cannot be proven to serve the canonical provider policy`,
      'run the pnpm production build and restore the route export before embedding bin/peasant',
    )
  }
  const html = readFileSync(htmlPath, 'utf8')
  const sources = [...html.matchAll(/<script\b[^>]*\bsrc="([^"]+\.js)"/g)].map((match) => match[1])
  if (sources.length === 0) {
    fail(
      `${route} export references no JavaScript chunks`,
      'the mounted route would have no auditable client bundle',
      htmlPath,
      `${route} could render only static shell bytes or stale content`,
      'rebuild the real Next route and preserve its script references',
    )
  }
  return [...new Set(sources)].map((source) => {
    const path = resolve(OUT, decodeURIComponent(source.replace(/^\//, '')))
    if (!existsSync(path)) {
      fail(
        `${route} references missing chunk ${source}`,
        'the exported HTML and static assets are inconsistent',
        htmlPath,
        'the route would fail at runtime before provider identity could render',
        'clean web/out and web/.next, then rebuild with one immutable dependency tree',
      )
    }
    return { source, path, content: readFileSync(path, 'utf8') }
  })
}

function looksLikeProviderPolicy(content) {
  return content.includes('Provider harness validation failed') || (
    content.includes('Google Antigravity') && LEGACY_POLICY_IDENTITIES.every((identity) => content.includes(identity))
  )
}

const canonicalHarnesses = Object.freeze(Object.values(Harness))
if (canonicalHarnesses.length !== 7 || !canonicalHarnesses.includes(Harness.Strike)) {
  fail(
    `installed @peasant-labs/schema exposes ${JSON.stringify(canonicalHarnesses)} instead of the seven-harness final contract`,
    'the build dependency tree is not the contract this Peasant source targets',
    'provider-build-provenance.mjs schema inventory',
    'a provider-policy result would be ambiguous',
    'install the exact @peasant-labs/schema@0.1.0 final contract and rebuild',
  )
}

const routeEvidence = []
for (const [route, htmlPath] of ROUTES) {
  const chunks = routeChunks(route, htmlPath)
  const stale = chunks.filter(({ content }) =>
    content.includes('Google Antigravity') &&
    LEGACY_POLICY_IDENTITIES.every((identity) => content.includes(identity)) &&
    !content.includes('strike:"Strike"'),
  )
  if (stale.length > 0) {
    fail(
      `${route} references six-provider chunk(s) ${stale.map(({ source }) => source).join(', ')}`,
      'those chunks contain the previous complete provider inventory but no Strike display policy',
      htmlPath,
      `${route} can reject or misrender a canonical Strike session`,
      'consume the immutable Strike-capable Fairtrade package, clean all generated output, and rebuild',
    )
  }
  const policyChunks = chunks.filter(({ content }) => looksLikeProviderPolicy(content))
  if (policyChunks.length === 0) {
    fail(
      `${route} references no identifiable fail-closed provider-policy chunk`,
      'none of its mounted client chunks contains the canonical validation and display inventory',
      htmlPath,
      `${route} provider identity cannot be audited`,
      'ensure the mounted route consumes Fairtrade provider components and rebuild',
    )
  }
  for (const chunk of policyChunks) {
    const missingHarnesses = canonicalHarnesses.filter((harness) => !chunk.content.includes(harness))
    const missingMarkers = STRIKE_POLICY_MARKERS.filter((marker) => !chunk.content.includes(marker))
    if (missingHarnesses.length > 0 || missingMarkers.length > 0) {
      fail(
        `${route} policy chunk ${chunk.source} is incomplete (missing harnesses ${JSON.stringify(missingHarnesses)}, markers ${JSON.stringify(missingMarkers)})`,
        'a route-referenced provider policy must be seven-provider, fail-closed, and carry the official Strike identity atomically',
        chunk.path,
        `${route} could ship a mixed old/new provider boundary`,
        'remove stale generated output, install only immutable packed upstream artifacts, and rebuild',
      )
    }
  }
  routeEvidence.push({ route, chunks: policyChunks.map(({ source }) => source) })
}

if (!existsSync(BINARY)) {
  fail(
    `${BINARY} is absent`,
    'the Go artifact has not been built after the web export',
    'provider-build-provenance.mjs binary check',
    'embedded production bytes cannot be verified',
    'run make build so bin/peasant embeds the audited web/out tree',
  )
}
const binary = readFileSync(BINARY)
const missingBinaryMarkers = STRIKE_POLICY_MARKERS.filter((marker) => !binary.includes(Buffer.from(marker)))
if (missingBinaryMarkers.length > 0) {
  fail(
    `bin/peasant is missing ${JSON.stringify(missingBinaryMarkers)}`,
    'the binary does not contain the same complete Strike policy proven in web/out',
    BINARY,
    'the served artifact may differ from the inspected web export',
    'rebuild bin/peasant only after the clean provider-capable web export succeeds',
  )
}

const binarySHA256 = createHash('sha256').update(binary).digest('hex')
console.log(`provider build provenance: ${routeEvidence.map(({ route, chunks }) => `${route}=[${chunks.join(',')}]`).join(' ')}`)
console.log(`provider build provenance: seven harnesses, official Strike SVG/policy, and fail-closed validation found in web/out and bin/peasant (${binarySHA256})`)
