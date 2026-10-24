/* Mounted bounded Home on the exact built binary, with deterministic local API
 * dependencies. Never targets a user's server/store. The server-backed handler
 * contract is verified separately by API tests, not by these browser routes.
 */
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { existsSync, mkdtempSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { join, resolve, dirname, relative } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createHash } from 'node:crypto';
import YAML from 'yaml';

const repo = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const binary = join(repo, 'peasant');
const out = process.env.CAPTURES || '/tmp/opencode/home-pages-captures';
const port = Number(process.env.PEASANT_ROOT_PORT || 18731);
assert.notEqual(port, 8690, 'Home evidence must never use the user server port');
const origin = `http://localhost:${port}`;
const fixture = YAML.parse(readFileSync(join(repo, 'web/src/app/testdata/home-pages.yaml'), 'utf8'));
const stimulus = fixture.cases.find(row => row.name === 'loaded-refresh-keeps-content-on-failure');
const filesBelow = directory => readdirSync(directory, { withFileTypes: true }).flatMap(entry => entry.isDirectory() ? filesBelow(join(directory, entry.name)) : [join(directory, entry.name)]);
const chunks = filesBelow(join(repo, 'web/out/_next/static/chunks')).filter(path => path.endsWith('.js'));
const chunk = chunks.find(path => readFileSync(path, 'utf8').includes('data-home-bounded'));
assert.ok(chunk, 'Rebuild this branch: exported Home marker is absent');
const bytes = readFileSync(binary);
assert.ok(bytes.includes(Buffer.from('data-home-bounded')), 'Rebuild the CLI: embedded Home marker is absent');
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const home = mkdtempSync('/tmp/opencode/home-browser-');
const env = { ...process.env, HOME: home, XDG_CONFIG_HOME: join(home, 'config'), XDG_DATA_HOME: join(home, 'data'), XDG_STATE_HOME: join(home, 'state'), XDG_CACHE_HOME: join(home, 'cache') };
const server = spawn(binary, ['web', 'start', '--port', String(port), '--foreground', '--no-browser', '--mock-data-store=web'], { cwd: repo, env, stdio: ['ignore', 'ignore', 'pipe'] });
let stderr = '';
server.stderr.on('data', data => { stderr += data; });
const stopped = new Promise(resolve => server.once('exit', resolve));
const timeout = AbortSignal.timeout(90_000);
timeout.addEventListener('abort', () => server.kill('SIGKILL'), { once: true });
let browser;
const evidence = [];
try {
  let healthy = false;
  for (let attempt = 0; attempt < 80; attempt++) {
    if (server.exitCode !== null) throw new Error(`Sandbox server exited: ${stderr}`);
    if ((await fetch(`${origin}/api/v1/health`).catch(() => null))?.ok) { healthy = true; break; }
    await new Promise(done => setTimeout(done, 100));
  }
  assert.ok(healthy, `Sandbox server did not start: ${stderr}`);
  browser = await chromium.launch({ executablePath: process.env.CHROME_PATH, headless: true });
  mkdirSync(out, { recursive: true });
  function row(id, childCount = 0) {
    return { sync: { id, harness: 'claude-code', projectName: 'fixture-project', projectHash: fixture.visual.projectHash, hostSlug: 'fixture-host', startTime: '2026-10-15T10:00:00Z', durationMs: 60000, totalTokens: 10, turnCount: 2, model: 'fixture', syncStatus: 'new', previouslyPushed: false }, publication: { sessionId: id, state: 'unpublished', autoPublish: false, outsideSelection: false }, title: `session ${id}`, preview: '', childCount };
  }
  const roots = Array.from({ length: stimulus.roots }, (_, index) => row(`root-${index}`, index === 0 ? stimulus.children : 0));
  Object.assign(roots.at(-1).sync, fixture.visual.projects[1]);
  const children = Array.from({ length: stimulus.children }, (_, index) => row(`child-${index}`));
  for (const theme of fixture.visual.themes) for (const viewport of fixture.visual.viewports) {
    const page = await browser.newPage({ viewport: { width: viewport.width, height: viewport.height }, reducedMotion: 'reduce' });
    const reads = [];
    const forbidden = [];
    const subscriptions = [];
    let fail = false;
    await page.addInitScript(value => localStorage.setItem('peasant-theme', value), theme);
    page.on('websocket', socket => socket.on('framesent', event => { subscriptions.push(String(event.payload)); }));
    await page.route('**/api/v1/**', async route => {
      const url = new URL(route.request().url());
      if (url.pathname === '/api/v1/sync/sessions' && url.searchParams.get('view') === 'project-options') return route.fulfill({ json: { projects: fixture.visual.projects } });
      if (url.pathname === '/api/v1/sync/sessions' && url.searchParams.get('view') === 'home') {
        reads.push(url);
        if (fail) return route.fulfill({ status: 503, json: { error: 'temporary local read failure; retry the session list' } });
        const source = url.searchParams.has('parentSessionId') ? children : roots;
        const start = url.searchParams.get('pageToken') === 'after-first' ? 25 : url.searchParams.get('pageToken') === 'after-second' ? 50 : 0;
        return route.fulfill({ json: { items: source.slice(start, start + 25), next: start + 25 < source.length ? start === 0 ? 'after-first' : 'after-second' : '' } });
      }
      if (url.pathname === '/api/v1/sync/auth') return route.fulfill({ json: { authenticated: false } });
      if (url.pathname === '/api/v1/publications') return route.fulfill({ json: { publications: [] } });
      if (['/api/v1/sync/sessions', '/api/v1/projects/summary'].includes(url.pathname)) forbidden.push(url.pathname);
      return route.continue();
    });
    await page.goto(`${origin}/`, { waitUntil: 'networkidle' });
    await page.getByRole('link', { name: 'session root-0', exact: true }).waitFor();
    assert.equal(reads.length, 1);
    assert.equal(reads[0].searchParams.get('maxItems'), '25');
    // The project choices live in the filters menu.
    await page.getByRole('button', { name: /^filters/ }).click();
    await page.getByRole('menuitemradio', { name: 'unseen-project', exact: true }).waitFor();
    assert.equal(await page.getByRole('menuitemradio', { name: 'unseen-project', exact: true }).count(), 1, 'Project metadata must include off-page projects');
    await page.keyboard.press('Escape');
    assert.deepEqual(forbidden, []);
    assert.ok(!subscriptions.some(frame => /"(?:sessions|quality|trends)"/.test(frame)), `Eager subscription: ${subscriptions}`);
    assert.equal(await page.locator('html').getAttribute('data-theme'), theme);
    assert.equal(await page.locator('[data-root-session-list] [data-session-id]').count(), 25);
    await page.locator('[data-session-group="root-0"] button').click();
    await page.getByRole('link', { name: 'session child-0', exact: true }).waitFor();
    assert.equal(reads.at(-1).searchParams.has('publication'), false);
    // The pager reads the next server page on request and shows one page at a time.
    await page.getByRole('button', { name: 'next page', exact: true }).click();
    await page.getByRole('link', { name: 'session root-25', exact: true }).waitFor();
    assert.equal(await page.getByRole('link', { name: 'session root-0', exact: true }).count(), 0, 'One page at a time');
    const before = reads.length;
    fail = true;
    await page.evaluate(() => window.dispatchEvent(new Event('focus')));
    await page.getByRole('button', { name: 'retry the session list' }).waitFor();
    assert.equal(reads.length, before + 2, 'Failed first pages must not drain later continuation');
    assert.equal(await page.getByRole('link', { name: 'session root-25', exact: true }).count(), 1);
    fail = false;
    await page.getByRole('button', { name: 'retry the session list' }).click();
    await page.getByRole('button', { name: 'retry the session list' }).waitFor({ state: 'detached' });
    await page.getByRole('button', { name: 'page 1', exact: true }).click();
    await page.getByRole('link', { name: 'session child-0', exact: true }).waitFor();
    await page.evaluate(() => window.scrollTo(0, 0));
    const servedChunk = await page.request.get(`${origin}/${relative(join(repo, 'web/out'), chunk)}`);
    assert.equal(servedChunk.status(), 200);
    assert.equal(createHash('sha256').update(await servedChunk.body()).digest('hex'), createHash('sha256').update(readFileSync(chunk)).digest('hex'));
    assert.equal(await page.locator('.asb').count(), 1, 'Persistent product chrome must be mounted');
    const file = join(out, `home-${theme}-${viewport.name}.png`);
    await page.screenshot({ path: file, fullPage: false });
    evidence.push({ theme, viewport, file, pages: reads.map(url => url.search), chunk, binarySha256: createHash('sha256').update(bytes).digest('hex'), fontFamily: await page.getByRole('heading', { name: 'sessions', exact: true }).evaluate(node => getComputedStyle(node).fontFamily) });
    await page.close();
  }
  writeFileSync(join(out, 'evidence.json'), JSON.stringify(evidence, null, 2));
  console.log(`Mounted Home evidence: ${out}/home-{dark,light}-{desktop,phone}.png`);
} finally {
  await browser?.close();
  server.kill('SIGTERM');
  const kill = setTimeout(() => server.kill('SIGKILL'), 3000);
  await stopped;
  clearTimeout(kill);
  rmSync(home, { recursive: true, force: true });
}
