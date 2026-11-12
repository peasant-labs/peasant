import { readFileSync, realpathSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { execFileSync } from 'node:child_process';
import { hash } from './provenance.mjs';

/**
 * Served-build provenance for the setup journey. It fails closed: a capture is
 * trusted only when the process answering is the binary this run built, the
 * pages and assets it serves are byte-identical to the export that binary
 * embeds, and the served bundle carries strings only this branch introduces.
 *
 * Every check refuses rather than skips. A marker that the base commit also
 * has proves nothing about this branch, so it is refused as a marker.
 */

const assetRefs = (html) =>
  [...html.matchAll(/(?:src|href)="([^"?#]+\.(?:js|css))"/g)].map((match) => match[1]).filter((path) => path.startsWith('/_next/'));

/** The error every provenance failure uses: what, why, and what to do. */
function refuse(what, why, fix) {
  throw new Error(`Setup journey provenance failed: ${what}; ${why}; ${fix}. No capture is trusted.`);
}

/**
 * Whether `marker` occurs in the web sources of `commit`. A runner passes the
 * real `git grep`; the support tests pass a stub.
 */
export function markerInCommit(repo, commit, marker) {
  try {
    execFileSync('git', ['grep', '-qF', '--', marker, commit, '--', 'web/src'], { cwd: repo, stdio: 'ignore' });
    return true;
  } catch (error) {
    if (error.status === 1) return false;
    throw new Error(`git grep for ${JSON.stringify(marker)} in ${commit} failed (${error.message}); the base commit must be present in this checkout`);
  }
}

/**
 * Verify what a started binary serves.
 *
 * @param {object} options
 * @param {string} options.origin  the server's origin
 * @param {string} options.out     the web export directory the binary embeds
 * @param {string} options.binary  the absolute path of the binary this run built
 * @param {number} options.pid     the serving process
 * @param {readonly string[]} options.routes   pages that must be served byte for byte
 * @param {readonly string[]} options.markers  strings only this branch introduces
 * @param {(marker: string) => boolean} options.baseHasMarker whether the base commit's web sources hold the marker
 * @param {string} [options.exe]   test seam for the serving process's executable path
 */
export async function verifyKickstartServed({ origin, out, binary, pid, routes, markers, baseHasMarker, exe }) {
  if (!markers.length) refuse('the fixture names no marker', 'a bundle with nothing to find proves nothing', 'name at least one string only this branch introduces');
  const present = markers.filter((marker) => baseHasMarker(marker));
  if (present.length) refuse(`the base commit already contains ${JSON.stringify(present)}`, 'a string the base has cannot show that this branch is what is served', 'choose markers the branch added');

  const executable = exe ?? realpathSync(`/proc/${pid}/exe`);
  if (executable !== realpathSync(binary) || hash(readFileSync(executable)) !== hash(readFileSync(binary))) {
    refuse('the serving process is not the binary this run built', `process ${pid} runs ${executable}`, 'stop the stale server and rerun');
  }

  const served = { pages: {}, assets: {}, markers: {} };
  const bodies = [];
  for (const route of routes) {
    const response = await fetch(`${origin}${route}`, { redirect: 'follow', signal: AbortSignal.timeout(5000) });
    if (!response.ok) refuse(`${route} answered ${response.status}`, 'the page this journey drives is not served', 'rebuild the web export and the binary');
    const bytes = Buffer.from(await response.arrayBuffer());
    const local = join(out, route.replace(/^\//, ''), 'index.html');
    let expected;
    try { expected = readFileSync(local); } catch { refuse(`${route} has no export at ${local}`, 'the build emitted no page for it', 'run the web build'); }
    if (hash(bytes) !== hash(expected)) refuse(`${route} served HTML differs from this export`, 'a stale server or another build answers', 'stop it and rerun');
    served.pages[route] = hash(bytes);
    bodies.push(bytes.toString());
    for (const path of assetRefs(bytes.toString())) {
      if (served.assets[path]) continue;
      const file = resolve(out, `.${path}`);
      if (!file.startsWith(`${resolve(out)}/`)) refuse(`asset ${path} escapes the export`, 'the page references a path outside it', 'rebuild');
      const asset = await fetch(`${origin}${path}`, { signal: AbortSignal.timeout(5000) });
      const assetBytes = Buffer.from(await asset.arrayBuffer());
      if (!asset.ok || hash(assetBytes) !== hash(readFileSync(file))) refuse(`served asset ${path} differs from this build`, 'a stale server or another build answers', 'stop it and rerun');
      served.assets[path] = hash(assetBytes);
      bodies.push(assetBytes.toString());
    }
  }
  if (!Object.keys(served.assets).length) refuse('no exported JS or CSS was referenced', 'the pages carry no auditable bundle', 'rebuild the web export');
  for (const marker of markers) {
    const found = bodies.some((body) => body.includes(marker));
    served.markers[marker] = found;
    if (!found) refuse(`the served bundle lacks ${JSON.stringify(marker)}`, 'the build does not contain this branch', 'rebuild the web export and the binary from this branch');
  }
  return { ...served, binaryHash: hash(readFileSync(binary)), executable };
}
