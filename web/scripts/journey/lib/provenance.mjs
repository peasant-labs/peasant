import { createHash } from 'node:crypto';
import { readFileSync, readdirSync, realpathSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';

export const hash = bytes => createHash('sha256').update(bytes).digest('hex');
export function inventory(root) {
  const result = {};
  function walk(directory) {
    for (const item of readdirSync(directory, { withFileTypes: true })) {
      const path = join(directory, item.name);
      if (item.isSymbolicLink()) throw new Error(`Provenance inventory refuses a symlink at ${relative(root, path)}; use the actual published/build files`);
      if (item.isDirectory()) walk(path);
      else if (item.isFile()) result[relative(root, path).replaceAll('\\', '/')] = hash(readFileSync(path));
    }
  }
  walk(root); return result;
}
export function assertExportedSurface(ui, graph) {
  if (typeof ui.VillageObservations !== 'function' || !graph.LOCAL_APP_SECTIONS?.some(row => row.id === 'village' && row.inNav === false)) {
    throw new Error('The installed PUBLISHED Fairtrade package lacks VillageObservations or the canonical route-only village section. Wait for the reviewed npm release, coherently pin it with /village, then rerun. Source aliases, dev links and candidate tarballs are not proof.');
  }
}
export function assertPublishedFiles(installed, unpacked) {
  const actual = inventory(realpathSync(installed)), published = inventory(unpacked);
  // pnpm materializes the declared yaml dependency's command shim inside the
  // installed schema package. It is not a file distributed by that package.
  // Retain its hash as installation evidence; compare every published byte and
  // refuse every other extra file rather than ignoring node_modules wholesale.
  const metadata = JSON.parse(readFileSync(join(unpacked, 'package.json'), 'utf8'));
  const comparable = { ...actual };
  if (metadata.dependencies?.yaml && !Object.hasOwn(published, 'node_modules/.bin/yaml')) delete comparable['node_modules/.bin/yaml'];
  if (JSON.stringify(Object.entries(comparable).sort()) !== JSON.stringify(Object.entries(published).sort())) throw new Error('Installed package bytes differ from the integrity-verified npm tarball; remove overrides/dev links and reinstall the frozen published dependency');
  return actual;
}
export async function verifyServed(origin, out, binary, pid) {
  const html = await fetch(`${origin}/village`, { signal: AbortSignal.timeout(3000) });
  if (!html.ok) throw new Error('The owned binary did not serve /village');
  const bytes = Buffer.from(await html.arrayBuffer());
  if (hash(bytes) !== hash(readFileSync(join(out, 'village/index.html')))) throw new Error('Served /village HTML differs from this export; refusing stale-server captures');
  const text = bytes.toString();
  if (!text.includes('village-page') || !text.includes('online observations')) throw new Error('The actual exported /village body is absent; nav-only proof is refused');
  if (hash(readFileSync(`/proc/${pid}/exe`)) !== hash(readFileSync(binary)) || realpathSync(`/proc/${pid}/exe`) !== realpathSync(binary)) throw new Error('The serving process is not the absolute binary built by this runner');
  const refs = [...text.matchAll(/(?:src|href)="([^"?#]+\.(?:js|css))"/g)].map(m => m[1]).filter(p => p.startsWith('/_next/'));
  if (!refs.length) throw new Error('No exported JS/CSS references found; mounted provenance cannot be established');
  let bodyMarker = false, navMarker = false;
  for (const path of new Set(refs)) {
    const local = resolve(out, `.${path}`);
    if (!local.startsWith(`${resolve(out)}/`)) throw new Error('Asset escaped the owned export');
    const response = await fetch(`${origin}${path}`, { signal: AbortSignal.timeout(5000) });
    const served = Buffer.from(await response.arrayBuffer());
    if (!response.ok || hash(served) !== hash(readFileSync(local))) throw new Error(`Served asset differs from this build: ${path}`);
    bodyMarker ||= served.includes(Buffer.from('village-page'));
    navMarker ||= served.includes(Buffer.from('/village'));
  }
  if (!bodyMarker || !navMarker) throw new Error('Verified assets lack the actual new body/route markers; no capture is trusted');
  return { htmlHash: hash(bytes), assets: [...new Set(refs)], binaryHash: hash(readFileSync(binary)), executable: realpathSync(`/proc/${pid}/exe`) };
}
