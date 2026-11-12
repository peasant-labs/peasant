import { test, expect } from '@playwright/test';
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import { parse } from 'yaml';
import { railProblems } from './lib/kickstart-rail.mjs';

/**
 * The setup journey on the built binary: a fresh computer with seeded agent
 * recordings, a real browser, and every claim read off what the server and the
 * page actually did. The runner (run-kickstart.mjs) builds the binary, seeds the
 * computer, starts the server, and proves the served bundle is this branch's
 * before this file runs; this file drives the page.
 *
 * The cases are one story told in order (a fresh install to a saved setup, then
 * the selection page), so they run serially and share the pages they open.
 */

const fixture = parse(readFileSync(new URL('./testdata/kickstart.yaml', import.meta.url), 'utf8'));
const names = fixture.cases.map((entry) => entry.name);
if (new Set(names).size !== names.length || fixture.requiredNames.some((name) => !names.includes(name)) || names.some((name) => !fixture.requiredNames.includes(name))) {
  throw new Error('The setup journey catalogue must be exactly its required names');
}

const artifacts = process.env.KICKSTART_ARTIFACTS;
const configPath = process.env.KICKSTART_CONFIG_PATH;
const home = process.env.KICKSTART_HOME;
const root = dirname(home);
const seeded = JSON.parse(readFileSync(process.env.KICKSTART_SEEDED, 'utf8'));
const chosenSessions = seeded.filter((entry) => entry.repository === fixture.chosen.repository && entry.branch === fixture.chosen.branch);
const chosenIds = new Set(chosenSessions.map((entry) => entry.sessionId));
const chosenTitles = chosenSessions.map((entry) => entry.prompt).sort();
const ledger = fixture.recordings.repositories.find((repo) => repo.name === fixture.chosen.repository);
const ledgerLabel = new URL(ledger.remote).pathname.replace(/^\//, '').replace(/\.git$/, '');
const taxReport = seeded.find((entry) => entry.name === fixture.selectionPage.session);
const marketing = seeded.find((entry) => entry.name === 'hero-copy');
const stray = seeded.find((entry) => entry.name === fixture.branchChoice.recordedElsewhere);
const marketingRepo = fixture.recordings.repositories.find((repo) => repo.name === fixture.branchChoice.repository);
const marketingLabel = new URL(marketingRepo.remote).pathname.replace(/^\//, '').replace(/\.git$/, '');

const theme = () => test.info().project.metadata.theme;
const API = '/api/v1/kickstart';

// ── shared state: the pages of one browser, and what they saw ───────────────
let context;
let pageA;
let pageB;
const shots = [];
const probes = [];
const consoleErrors = [];
const failedResponses = [];
const frames = [];
const requests = [];
const walked = [];
let offerSeen = null;
let ingestCounts = null;
let importedAfterSetup = null;
const laterImports = {};
// The selection the setup saved, as the file holds it, kept in the evidence.
let savedSelection = null;

function watch(page, label) {
  page.on('console', (message) => {
    // The browser logs every non-2xx response as a console error with no URL;
    // those are accounted for one by one in failedResponses below.
    if (message.type() === 'error' && !message.text().startsWith('Failed to load resource')) consoleErrors.push(`${label}: ${message.text()}`);
  });
  page.on('response', (response) => {
    if (response.status() >= 400) failedResponses.push({ tab: label, status: response.status(), method: response.request().method(), path: new URL(response.url()).pathname });
  });
  page.on('pageerror', (error) => consoleErrors.push(`${label} pageerror: ${error.message}`));
  page.on('request', (request) => {
    const entry = { tab: label, method: request.method(), path: new URL(request.url()).pathname };
    // Which scope a page asked a session for says which page opened it.
    if (entry.method === 'POST' && entry.path === `${API}/sessions`) {
      try { entry.scope = JSON.parse(request.postData() ?? '{}').scope; } catch { entry.scope = null; }
    }
    requests.push(entry);
  });
  page.on('websocket', (socket) => socket.on('framereceived', (event) => {
    if (typeof event.payload !== 'string') return;
    let message;
    try { message = JSON.parse(event.payload); } catch { return; }
    if (message.type !== 'kickstart' || !message.data) return;
    const { kind, sessionId, revision, reason, scan, ingest } = message.data;
    frames.push({ tab: label, kind, sessionId, revision, reason, scan, ingest });
  }));
}

async function openPage(label, url) {
  const page = await context.newPage();
  watch(page, label);
  await page.goto(url);
  return page;
}

// ── reading the page ────────────────────────────────────────────────────────
const title = (page) => page.locator('.swz-body h2.ks-title').first();
const next = (page) => page.getByRole('button', { name: 'continue', exact: true });
const save = (page) => page.getByRole('button', { name: 'save and import' });

async function stepOf(page) {
  const text = await page.locator('.swz-count .swz-sr-only').first().textContent();
  const match = /step (\d+) of (\d+)/.exec(text ?? '');
  if (!match) throw new Error(`the step counter reads ${JSON.stringify(text)}, not "step N of M"`);
  return { index: Number(match[1]), total: Number(match[2]) };
}

/** Record each stop the person is shown, once, in order. */
async function seen(page) {
  const text = (await title(page).textContent())?.trim() ?? '';
  if (walked.at(-1) !== text) walked.push(text);
  return text;
}

/** Continue to the named stop. Only the first tab's walk is the one that is compared with the server's list. */
async function advance(page, expectedTitle) {
  await next(page).click();
  await expect(title(page)).toHaveText(expectedTitle);
  if (page === pageA) await seen(page);
}

async function snapshot(request) {
  const status = await (await request.get(`${API}/status`)).json();
  if (!status.activeSessionId) throw new Error('the server reports no open wizard session');
  const response = await request.get(`${API}/sessions/${status.activeSessionId}`);
  expect(response.ok(), await response.text()).toBe(true);
  return { id: status.activeSessionId, view: await response.json() };
}

const visibleStops = (view) => view.stops.filter((stop) => stop.visible);

async function storedTitles(request) {
  const response = await request.get('/api/v1/sync/sessions');
  expect(response.ok()).toBe(true);
  return (await response.json()).items.map((item) => item.title).sort();
}

const readConfig = () => parse(readFileSync(configPath, 'utf8'));

/**
 * What the finish panel says the import did, once it has ended: the sessions
 * the store did not have (new) and the ones it had and re-read (updated).
 */
async function finishCounts(page) {
  await expect(page.getByText('import finished')).toBeVisible();
  const text = await page.locator('.ks-finish .ks-counts').first().textContent();
  const counts = /(\d+) new,\s*(\d+) updated/.exec(text ?? '');
  expect(counts, `the finish panel counts the import (reads ${JSON.stringify(text)})`).not.toBeNull();
  return { new: Number(counts[1]), updated: Number(counts[2]) };
}

/**
 * Open a page the first-run redirect must leave where it is, and show that it
 * did. A redirect is ordered by one read of the status route, so the wake source
 * is the page's own network going quiet after it loaded: a read that started
 * with the page has answered by then, and a redirect it ordered has happened.
 */
async function opensInPlace(page, path) {
  await page.goto(path, { waitUntil: 'networkidle', timeout: 20000 });
  expect(new URL(page.url()).pathname, `${path} is not redirected`).toBe(path);
}

// ── the tree ────────────────────────────────────────────────────────────────
const projectOf = (page, label) => page.locator('li.st-node[data-kind="project"]', { hasText: label });
const project = (page) => projectOf(page, ledgerLabel);
const branch = (page, name) => project(page).locator('li.st-node[data-kind="branch"]').filter({ has: page.getByRole('checkbox', { name, exact: true }) }).last();
const row = (page, name) => page.getByRole('checkbox', { name, exact: false });
const checked = (locator) => locator.getAttribute('aria-checked');

// ── evidence ────────────────────────────────────────────────────────────────
/**
 * What the page computed, not what the stylesheet says: the font stack of every
 * element that draws text, and the corner radius of every drawn element, on the
 * setup page's own surface. A radio's circle is its meaning, so a radio input
 * is the one exemption.
 */
async function probe(page) {
  return page.evaluate(() => {
    const family = /^"?atkinsonHyperlegible(Mono)?"?(,|$)/i;
    const corners = ['borderTopLeftRadius', 'borderTopRightRadius', 'borderBottomLeftRadius', 'borderBottomRightRadius'];
    const surface = document.querySelector('.ks-page');
    const offenders = { font: [], radius: [] };
    let textElements = 0;
    let drawn = 0;
    const describe = (element) => `${element.tagName.toLowerCase()}${element.className && typeof element.className === 'string' ? `.${element.className.trim().split(/\s+/).join('.')}` : ''}`;
    for (const element of surface ? surface.querySelectorAll('*') : []) {
      const style = getComputedStyle(element);
      if (style.display === 'none' || style.visibility === 'hidden') continue;
      drawn += 1;
      if ([...element.childNodes].some((node) => node.nodeType === 3 && node.textContent.trim())) {
        textElements += 1;
        if (!family.test(style.fontFamily)) offenders.font.push(`${describe(element)}: ${style.fontFamily}`);
      }
      if (corners.some((corner) => parseFloat(style[corner]) > 0) && !element.matches('input[type=radio]')) {
        offenders.radius.push(`${describe(element)}: ${corners.map((corner) => style[corner]).join(' ')}`);
      }
    }
    const tabular = [...document.querySelectorAll('.ks-num')].filter((element) => !getComputedStyle(element).fontVariantNumeric.includes('tabular-nums')).map(describe);
    // The step rail: where each label and its text were laid out, against the frame they sit in.
    // The frame decides whether the rail shows its labels (a rail too narrow for them keeps them
    // in the accessible name and draws the numbers), so a rail is labelled when any label is drawn.
    const frame = document.querySelector('.ks-wizard');
    const frameBox = frame ? frame.getBoundingClientRect() : null;
    const railLabels = [...(frame ? frame.querySelectorAll('.swz-rail .swz-label') : [])].map((element) => {
      const box = element.getBoundingClientRect();
      const range = document.createRange();
      range.selectNodeContents(element);
      const text = range.getBoundingClientRect();
      const style = getComputedStyle(element);
      return {
        text: element.textContent.trim(),
        place: (/swz-label--(\w+)/.exec(element.className) ?? [])[1] ?? '',
        left: box.left, right: box.right, textLeft: text.left, textRight: text.right,
        visible: box.width > 0 && box.height > 0 && style.visibility !== 'hidden' && style.display !== 'none',
      };
    });
    const rail = {
      labelled: railLabels.some((label) => label.visible),
      frame: frameBox ? { left: frameBox.left, right: frameBox.right } : { left: 0, right: 0 },
      viewportWidth: document.documentElement.clientWidth,
      labels: railLabels,
    };
    return {
      theme: document.documentElement.getAttribute('data-theme'),
      body: getComputedStyle(document.body).fontFamily,
      loadedFaces: [...new Set([...document.fonts].filter((face) => face.status === 'loaded' && /atkinson/i.test(face.family)).map((face) => face.family))],
      surface: !!surface,
      drawn,
      textElements,
      offenders,
      tabularOffenders: tabular,
      rail,
    };
  });
}

async function shoot(page, name) {
  const path = join(artifacts, `${theme()}-${name}.png`);
  await page.screenshot({ path, animations: 'disabled' });
  shots.push({ name, path });
  probes.push({ name, ...(await probe(page)) });
}

test.describe.configure({ mode: 'serial' });

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext({ viewport: test.info().project.use.viewport, reducedMotion: 'reduce' });
  await context.addInitScript((value) => localStorage.setItem('peasant-theme', value), theme());
});

test.afterAll(async () => {
  writeFileSync(join(artifacts, `${theme()}-evidence.json`), JSON.stringify({
    theme: theme(), shots, probes, consoleErrors, failedResponses, walked, offerSeen, ingestCounts, laterImports, importedAfterSetup, savedSelection,
    frames: frames.map(({ tab, kind, revision, reason, scan, ingest }) => ({ tab, kind, revision, reason, scan, ingest })),
    requests: requests.filter((entry) => entry.path.startsWith('/api/')).map((entry) => `${entry.tab} ${entry.method} ${entry.path}${entry.scope === undefined ? '' : ` (scope ${entry.scope})`}`),
  }, null, 2));
  await context?.close();
});

// ── the cases ───────────────────────────────────────────────────────────────

test('fresh-install-redirects-to-setup', async () => {
  pageA = await context.newPage();
  watch(pageA, 'A');
  const status = await (await pageA.request.get(`${API}/status`)).json();
  expect(status.configured, 'a fresh computer has no saved setup').toBe(false);
  expect(existsSync(configPath), 'no config.yaml exists before setup').toBe(false);

  // A top-level page sends a fresh install to setup.
  await pageA.goto('/settings/');
  await expect(pageA).toHaveURL(/\/kickstart\/?$/);
  // So does the standalone selection page, and it starts nothing on the way: its
  // body waits for the status answer, so no selection session is opened. The
  // setup page the redirect lands on opens its own (full) session.
  const beforeSelection = requests.length;
  await pageA.goto('/selection/');
  await expect(pageA).toHaveURL(/\/kickstart\/?$/);
  await expect(title(pageA)).toHaveText('peasant online');
  const opened = requests.slice(beforeSelection)
    .filter((entry) => entry.tab === 'A' && entry.method === 'POST' && entry.path === `${API}/sessions`)
    .map((entry) => entry.scope);
  expect(opened, 'the setup page opened its own session').toContain('full');
  expect(opened, 'the selection page opened no session before setup was saved').not.toContain('selection');
  // The pages that are never redirected stay where they are.
  await opensInPlace(pageA, '/share/');
  // Deep links into recorded work are honoured: a project's page and a transcript.
  const hash = 'a'.repeat(64);
  for (const deepLink of [`/sessions/${hash}/`, `/projects/${hash}/not-recorded/`]) await opensInPlace(pageA, deepLink);
  await pageA.goto('/');
  await expect(pageA).toHaveURL(/\/kickstart\/?$/);

  await expect(pageA.getByRole('heading', { level: 1, name: 'set up peasant' })).toBeVisible();
  await expect(title(pageA)).toHaveText('peasant online');
  await seen(pageA);
  const { id, view } = await snapshot(pageA.request);
  const first = await stepOf(pageA);
  expect(first.index).toBe(1);
  expect(first.total, 'the page counts the stops the server shows').toBe(visibleStops(view).length);
  await expect.poll(() => frames.some((frame) => frame.tab === 'A' && frame.kind === 'session' && frame.sessionId === id), 'the topic sends the session snapshot on subscribe').toBe(true);
  await shoot(pageA, '01-village');
});

test('scan-progress-and-tree-selection', async () => {
  const first = await stepOf(pageA);
  await pageA.getByRole('button', { name: 'no', exact: true }).click();
  await expect(title(pageA)).toHaveText('choose sessions to import');
  await seen(pageA);

  // The first scan started when the page opened. Rescan, and watch it advance.
  const total = seeded.length;
  await expect(pageA.locator('.ks-counts', { hasText: `${total} sessions found` })).toBeVisible();
  await expect(branch(pageA, 'main')).toBeVisible();
  await pageA.evaluate(() => {
    window.__ks = { values: [] };
    const read = () => {
      const bar = document.querySelector('[role="progressbar"][aria-label="scan progress"]');
      const value = bar?.getAttribute('aria-valuenow');
      if (value !== undefined && value !== null && window.__ks.values.at(-1) !== value) window.__ks.values.push(value);
    };
    new MutationObserver(read).observe(document.body, { subtree: true, childList: true, attributes: true, attributeFilter: ['aria-valuenow'] });
  });
  const frameMark = frames.length;
  const requestMark = requests.length;
  await pageA.getByRole('button', { name: 'scan again' }).click();
  const bar = pageA.getByRole('progressbar', { name: 'scan progress' });
  await expect(bar).toBeVisible();
  await shoot(pageA, '02-scan-progress');
  await expect(bar).toBeHidden({ timeout: 30000 });
  await expect(pageA.locator('.ks-counts', { hasText: `${total} sessions found` })).toBeVisible();

  const values = (await pageA.evaluate(() => window.__ks.values)).map(Number);
  expect(values.some((value) => value > 0 && value < 100), `the bar advanced through the scan (saw ${values.join(', ')})`).toBe(true);
  const scans = frames.slice(frameMark).filter((frame) => frame.tab === 'A' && frame.kind === 'scan');
  expect(new Set(scans.filter((frame) => frame.scan.state === 'running' && frame.scan.done > 0).map((frame) => frame.scan.done)).size, 'the server pushed more than one progress tick').toBeGreaterThan(1);
  expect(scans.at(-1).scan.state, 'the last pushed scan state').toBe('done');
  expect(requests.slice(requestMark).filter((entry) => entry.tab === 'A' && entry.method === 'GET' && /\/tree$/.test(entry.path)).length, 'the forest was read again when the scan ended').toBeGreaterThan(0);

  // Nothing is chosen yet, and the tree is the server's, not the page's.
  await expect(pageA.locator('.ks-counts', { hasText: '0 chosen' })).toBeVisible();
  await branch(pageA, 'main').scrollIntoViewIfNeeded();
  await project(pageA).getByRole('checkbox', { name: 'main', exact: true }).click();
  await expect(pageA.locator('.ks-counts', { hasText: `${chosenSessions.length} chosen` })).toBeVisible();
  await expect(project(pageA).first().getByRole('checkbox').first()).toHaveAttribute('aria-checked', 'mixed');
  await expect(project(pageA).getByRole('checkbox', { name: 'main', exact: true })).toHaveAttribute('aria-checked', 'true');
  for (const entry of chosenSessions) await expect(row(pageA, entry.prompt)).toBeChecked();
  await expect(row(pageA, taxReport.prompt), 'a session on another branch is not chosen').not.toBeChecked();
  await expect(row(pageA, marketing.prompt), 'a session of another project is not chosen').not.toBeChecked();

  // The page counts the stops the server shows, before and after the choice. It
  // decides none of that: an unchosen selection already shows the auto-ingest
  // stop, and the count is whatever the server's own list says.
  const { view } = await snapshot(pageA.request);
  expect(first.total, 'the count before the choice is the server list').toBeGreaterThan(0);
  expect((await stepOf(pageA)).total, 'the count after the choice is the server list').toBe(visibleStops(view).length);
  expect(visibleStops(view).map((stop) => stop.key), 'the auto-ingest stop is in the server list').toContain('auto-ingest');

  // A row's preview is the server's reading of the real transcript.
  await row(pageA, chosenSessions[0].prompt).focus();
  await expect(pageA.locator('.ks-preview')).toContainText(chosenSessions[0].prompt);
  await shoot(pageA, '03-selection-tree');
});

test('second-tab-converges-without-reload', async () => {
  pageB = await openPage('B', '/kickstart/');
  await expect(title(pageB)).toHaveText('peasant online');
  const navigations = { count: 0 };
  pageB.on('framenavigated', () => { navigations.count += 1; });
  await pageB.evaluate(() => { window.__tab = 'tab-b'; });
  const sameSession = await Promise.all([pageA.request, pageB.request].map(async (request) => (await snapshot(request)).id));
  expect(new Set(sameSession).size, 'two tabs of the wizard share one session').toBe(1);

  // B opens on the answers A already gave.
  await next(pageB).click();
  await expect(title(pageB)).toHaveText('choose sessions to import');
  await expect(pageB.locator('.ks-counts', { hasText: `${chosenSessions.length} chosen` })).toBeVisible();
  await expect(project(pageB).getByRole('checkbox', { name: 'main', exact: true })).toHaveAttribute('aria-checked', 'true');

  // Both move to the auto-ingest stop. A changes it; B sees that with no reload.
  await advance(pageA, 'auto-ingest new branches');
  await advance(pageB, 'auto-ingest new branches');
  const autoIngest = (page) => page.getByRole('checkbox', { name: /auto-ingest new branches/ });
  await expect(autoIngest(pageA), 'the server serves the saved default').toBeChecked();
  await expect(autoIngest(pageB)).toBeChecked();
  await autoIngest(pageA).click();
  await expect(autoIngest(pageA)).not.toBeChecked();
  await expect(autoIngest(pageB), "B shows A's answer").not.toBeChecked();
  expect(await pageB.evaluate(() => window.__tab), 'B was not reloaded').toBe('tab-b');
  expect(navigations.count, 'B did not navigate').toBe(0);
  await shoot(pageB, '04-second-tab');

  // And the other way round: B answers, A sees it before A has chosen anything.
  await advance(pageB, 'publication');
  await pageB.getByRole('radio', { name: 'plan to publish later' }).click();
  await advance(pageA, 'publication');
  await expect(pageA.getByRole('radio', { name: 'plan to publish later' }), "A shows B's answer").toBeChecked();
  expect(requests.filter((entry) => /\/api\/v1\/kickstart\/sessions\/[0-9a-f]+$/.test(entry.path) && entry.method === 'GET'), 'no tab read the snapshot to learn of the other').toEqual([]);
});

test('every-stop-the-server-shows', async () => {
  await advance(pageA, 'auto-publish');
  await advance(pageA, 'privacy');
  const example = pageA.getByRole('list', { name: 'example for redaction level' });
  await expect(example).toBeVisible();
  const lines = await example.getByRole('listitem').allTextContents();
  expect(lines.length, 'the example shows a before and an after').toBeGreaterThanOrEqual(2);
  expect(new Set(lines.map((line) => line.trim())).size, 'the lines differ').toBeGreaterThan(1);
  await shoot(pageA, '05-privacy');

  await advance(pageA, 'content license');
  await pageA.getByRole('radio', { name: 'CC0-1.0' }).click();
  await expect(pageA.getByRole('radio', { name: 'CC0-1.0' })).toBeChecked();

  // The server says the village offer follows the license stop. It is drawn on
  // the stop after it, never on the license stop or before.
  const before = await snapshot(pageA.request);
  expect(before.view.connect.offered).toBe(true);
  expect(before.view.connect.afterStop).toBe('license');
  await expect(pageA.getByText(before.view.connect.prompt)).toHaveCount(0);
  await next(pageA).click();
  await expect(title(pageA)).toHaveText(before.view.connect.prompt);
  offerSeen = before.view.connect.prompt;
  await seen(pageA);
  await expect(pageA.getByRole('button', { name: 'yes', exact: true })).toBeVisible();
  await expect(next(pageA), 'the offer has its own answers, not continue').toHaveCount(0);
  await shoot(pageA, '06-offer');
  await pageA.getByRole('button', { name: 'no', exact: true }).click();
  await expect(title(pageA)).toHaveText('claude retention');
  await seen(pageA);
  await advance(pageA, 'review');

  // The stops walked are the stops the server shows, in its order, with the
  // offer after the one it follows.
  const { view } = await snapshot(pageA.request);
  const expected = visibleStops(view).filter((stop) => stop.kind !== 'ingest').map((stop) => stop.title);
  expected.splice(expected.indexOf('content license') + 1, 0, offerSeen);
  expect(walked).toEqual(expected);
  expect(view.connect.offered, 'a skipped offer is not asked again').toBe(false);
});

test('review-summary-and-collapsed-diff', async () => {
  await expect(title(pageA)).toHaveText('review');
  const summary = pageA.locator('.ks-summary');
  await expect(summary).toContainText('your choices');
  await expect(summary).toContainText('what this does');
  await expect(summary, "the server's own sentence for the answer").toContainText('plan to publish later');
  await expect(summary).toContainText('CC0-1.0');
  const diff = pageA.locator('details.ks-diff');
  await expect(diff).not.toHaveAttribute('open', '');
  await expect(diff.locator(':scope > summary')).toContainText(/exact changes to .*config\.yaml/);
  await expect(diff.locator(':scope > summary')).toContainText(/\+\d+\s*[−-]0/);
  // Summary first, then the collapsed diff.
  expect(await pageA.evaluate(() => {
    const first = document.querySelector('.ks-summary');
    const second = document.querySelector('details.ks-diff');
    return !!(first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING);
  })).toBe(true);
  await diff.locator(':scope > summary').click();
  await expect(diff).toHaveAttribute('open', '');
  for (const text of ['selection', 'autoIngestNewBranches', 'CC0-1.0', ledger.remote, chosenSessions[0].cwd]) await expect(diff).toContainText(text);
  await shoot(pageA, '07-review-diff');
});

test('concurrent-change-offers-mine-or-theirs', async () => {
  // Someone edits an answered field in the saved file while the wizard is open.
  const theirs = 'push:\n  license: CC-BY-4.0\n';
  mkdirSync(dirname(configPath), { recursive: true });
  writeFileSync(configPath, theirs);

  await save(pageA).click();
  await expect(pageA.getByRole('alert').filter({ hasText: 'nothing was saved' })).toBeVisible();
  const rows = pageA.locator('.ks-conflict');
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText('CC0-1.0');
  await expect(rows.first()).toContainText('CC-BY-4.0');
  expect(readFileSync(configPath, 'utf8'), 'a refused save writes nothing').toBe(theirs);
  await expect(pageA.getByRole('heading', { name: 'your setup is saved' })).toHaveCount(0);
  await shoot(pageA, '08-review-conflict');

  // B was not asked; it is shown the same rows when it reaches the review.
  while (await next(pageB).count()) await next(pageB).click();
  await expect(title(pageB)).toHaveText('review');
  await expect(pageB.locator('.ks-conflict')).toHaveCount(1);

  // The choice is the person's: take the saved value.
  await pageA.getByRole('button', { name: /^take theirs for/ }).click();
  await expect(pageA.locator('.ks-conflict')).toHaveCount(0);
  await expect(pageB.locator('.ks-conflict'), 'the settled row leaves every tab').toHaveCount(0);
  expect(readFileSync(configPath, 'utf8'), 'choosing writes nothing either').toBe(theirs);
});

test('save-finish-and-ingest-end', async () => {
  await save(pageA).click();
  const saved = (page) => page.getByRole('heading', { name: 'your setup is saved' });
  await expect(saved(pageA)).toBeVisible();
  await expect(saved(pageB), 'the other tab moves to the finish with no reload').toBeVisible();
  await expect(pageA.locator('.ks-finish code')).toHaveText(configPath);
  await expect(pageA.getByText('nothing was published and no hook was installed')).toBeVisible();

  // The import reports its end: idle with counts, never a stream of ticks.
  ingestCounts = await finishCounts(pageA);
  // Every chosen session is new to a fresh store, and nothing is updated. A panel
  // that reports 0 new after the sessions were stored is wrong, not a count.
  expect(ingestCounts, 'the import brought in exactly the chosen sessions').toEqual({ new: fixture.chosen.imported, updated: 0 });
  const ingests = frames.filter((frame) => frame.tab === 'A' && frame.kind === 'ingest');
  expect(ingests.filter((frame) => frame.ingest.state === 'idle'), 'one end event').toHaveLength(1);
  expect(ingests.at(-1).ingest.state).toBe('idle');
  expect(ingests.at(-1).ingest.new).toBe(ingestCounts.new);
  expect(ingests.at(-1).ingest.updated).toBe(ingestCounts.updated);
  expect(frames.filter((frame) => frame.tab === 'A' && frame.kind === 'closed').map((frame) => frame.reason)).toEqual(['committed']);
  await expect(pageA.getByRole('button', { name: 'import now' })).toBeEnabled();

  // The store holds exactly the sessions that were chosen.
  const stored = await storedTitles(pageA.request);
  expect(stored, 'only the chosen sessions were imported').toEqual(chosenTitles);
  await shoot(pageA, '09-finish');

  // The finish only offers: the settings section it names is a real one.
  const link = pageA.getByRole('link', { name: 'choose automatic publishing rules' });
  // The app serves directory-style pages, so the link carries the trailing slash.
  const target = new URL(await link.getAttribute('href'), 'http://app.invalid');
  expect(target.pathname.replace(/\/$/, '')).toBe('/settings');
  expect(target.searchParams.get('section')).toBe('automatic-publishing');
  await link.click();
  await expect(pageA).toHaveURL(/\/settings\/?\?section=automatic-publishing/);
  await expect(pageA.getByRole('heading', { name: 'automatic publishing' }).first()).toBeVisible();

  // Setup is saved, so no page redirects again. The top-level page reads the
  // status route when it loads; the redirect is decided by that answer, so the
  // answer is awaited and the network is let go quiet before the address is read.
  const statusRead = pageA.waitForResponse((response) => new URL(response.url()).pathname === `${API}/status`, { timeout: 20000 });
  await pageA.goto('/', { waitUntil: 'networkidle', timeout: 20000 });
  expect((await (await statusRead).json()).configured, 'the page was told that setup is saved').toBe(true);
  expect(new URL(pageA.url()).pathname, 'a saved setup is never redirected').toBe('/');
  expect((await (await pageA.request.get(`${API}/status`)).json()).configured).toBe(true);
});

test('config-holds-only-the-chosen-selection', async () => {
  const config = readConfig();
  savedSelection = config.selection;
  expect(config.selection.mode).toBe('selected');
  expect(config.selection.autoIngestNewBranches, 'the answer that was given').toBe(false);
  const projects = config.selection.harnesses['claude-code'].projects;
  expect(projects).toHaveLength(1);
  // The setup draws one rule per checkout. The person chose one branch, so the
  // rule is that branch's: the project, its clone path and that branch, even
  // though it is the only branch in its checkout. A branch recorded there later is
  // admitted only by the new-branch setting, which was answered no. The whole rule
  // is compared, so a missing branch list, a second clone or an extra key fails here.
  expect(projects[0], 'the selection is the chosen branch of its checkout and nothing more').toEqual({
    gitRemote: ledger.remote,
    clonePaths: [...new Set(chosenSessions.map((entry) => entry.cwd))],
    branches: [fixture.chosen.branch],
  });
  const text = JSON.stringify(config.selection);
  for (const other of ['marketing-site', 'filler-', 'tax-report', 'stray']) expect(text, `the selection names nothing of ${other}`).not.toContain(other);
  for (const entry of seeded.filter((candidate) => !chosenIds.has(candidate.sessionId))) expect(text).not.toContain(entry.sessionId);
  // The other answers are what was chosen, and the edit made while the wizard
  // was open survived because the person took it.
  expect(config.push.sharePreference).toBe('share-later');
  expect(config.push.license).toBe('CC-BY-4.0');

  // Nothing else was written: no hook, no rule. The retention stop showed a
  // period and the review promised to apply it, so the save applied exactly that
  // one, which is the terminal's behaviour too.
  expect(existsSync(join(dirname(configPath), 'hooks.yaml')), 'no auto-publish rule was saved').toBe(false);
  const claudeSettings = JSON.parse(readFileSync(join(home, '.claude', 'settings.json'), 'utf8'));
  expect(claudeSettings.cleanupPeriodDays, 'the retention period the review promised was applied').toBe(99999);
  for (const repo of fixture.recordings.repositories) {
    const hooks = join(root, 'repos', repo.name, '.git', 'hooks');
    const installed = existsSync(hooks) ? readdirSync(hooks).filter((name) => !name.endsWith('.sample')) : [];
    expect(installed, `no Git hook was installed in ${repo.name}`).toEqual([]);
  }
  const touched = requests.filter((entry) => entry.method !== 'GET' && !entry.path.startsWith(`${API}/`)).map((entry) => `${entry.method} ${entry.path}`);
  expect(touched, 'the only writes were setup\'s own').toEqual([]);
  expect(requests.filter((entry) => entry.path.startsWith('/api/') && /\/auto-publish|\/sync\/push|\/sync\/login|\/village\//.test(entry.path)), 'no publish, sign-in or hook route was called').toEqual([]);
});

test('selection-page-saves-only-selection', async () => {
  const before = readConfig();
  const { selection: beforeSelection, ...beforeRest } = before;

  // A wizard is open with an answer that is not saved.
  const pageC = await openPage('C', '/kickstart/?from=settings');
  await expect(pageC.locator('.ks-page').getByRole('link', { name: 'settings', exact: true }), 'the trail says setup was opened from settings').toBeVisible();
  await expect(title(pageC)).toHaveText('peasant online');
  await pageC.getByRole('button', { name: 'no', exact: true }).click();
  await expect(title(pageC)).toHaveText('choose sessions to import');
  await expect(pageC.locator('.ks-counts', { hasText: `${chosenSessions.length} chosen` }), 're-running setup starts from the saved selection').toBeVisible();
  for (const entry of chosenSessions) await expect(row(pageC, entry.prompt)).toBeChecked();
  await advance(pageC, 'auto-ingest new branches');
  await expect(pageC.getByRole('checkbox', { name: /auto-ingest new branches/ })).not.toBeChecked();
  await advance(pageC, 'publication');
  await expect(pageC.getByRole('radio', { name: 'plan to publish later' }), 'the saved preference is filled in').toBeChecked();
  await pageC.getByRole('radio', { name: 'keep local, do not publish' }).click();
  await advance(pageC, 'auto-publish');
  await advance(pageC, 'privacy');
  await advance(pageC, 'content license');
  await expect(pageC.getByRole('radio', { name: 'CC-BY-4.0' }), 'the saved license is filled in').toBeChecked();
  await shoot(pageC, '11-rerun-prefilled');
  const wizard = (await snapshot(pageC.request)).id;

  // The selection page has its own session and its own three stops.
  const pageD = await openPage('D', '/selection/');
  await expect(pageD.getByRole('heading', { level: 1, name: 'choose sessions' })).toBeVisible();
  await expect(title(pageD)).toHaveText('choose sessions to import');
  expect(await stepOf(pageD), 'the selection page has selection, review and ingest').toEqual({ index: 1, total: 3 });
  expect((await snapshot(pageC.request)).id, 'opening the selection page did not replace the wizard session').toBe(wizard);
  await expect(pageD.locator('.ks-counts', { hasText: `${chosenSessions.length} chosen` })).toBeVisible();
  await row(pageD, taxReport.prompt).click();
  await expect(pageD.locator('.ks-counts', { hasText: `${chosenSessions.length + 1} chosen` })).toBeVisible();
  await shoot(pageD, '10-selection-page');
  await advance(pageD, 'review');
  await expect(pageD.locator('.ks-summary')).toContainText('your choices');
  await save(pageD).click();
  await expect(pageD.getByRole('heading', { name: 'your selection is saved' })).toBeVisible();
  // The sessions stored already are not counted again: the import brings in the one that was added.
  laterImports.selectionPage = await finishCounts(pageD);
  // The sessions stored already are unchanged on disk, so they are neither new nor updated.
  expect(laterImports.selectionPage, 'the import brought in the session that was added and updated none').toEqual({ new: fixture.selectionPage.imported, updated: 0 });

  // Only the selection changed on disk.
  const after = readConfig();
  const { selection: afterSelection, ...afterRest } = after;
  expect(isDeepStrictEqual(afterRest, beforeRest), 'every key but the selection is as it was').toBe(true);
  expect(isDeepStrictEqual(afterSelection, beforeSelection), 'the selection changed').toBe(false);
  expect(JSON.stringify(afterSelection)).toMatch(new RegExp(`${taxReport.sessionId}|feature/tax-report|${taxReport.cwd}`));
  importedAfterSetup = await storedTitles(pageD.request);
  expect(importedAfterSetup).toEqual([...chosenTitles, taxReport.prompt].sort());

  // The wizard is exactly as it was left: same session, same unsaved answer, still open.
  expect((await snapshot(pageC.request)).id).toBe(wizard);
  await expect(pageC.getByRole('heading', { name: 'your setup is saved' })).toHaveCount(0);
  for (const stop of ['privacy', 'auto-publish', 'publication']) {
    await pageC.getByRole('button', { name: 'back' }).click();
    await expect(title(pageC)).toHaveText(stop);
  }
  await expect(pageC.getByRole('radio', { name: 'keep local, do not publish' }), 'the wizard kept its unsaved answer').toBeChecked();
  expect(readConfig().push.sharePreference, 'and saved nothing').toBe('share-later');
});

test('selection-page-names-the-chosen-branch', async () => {
  // One branch of a project whose clone hosts sessions of two branches: the
  // selection page saves a branch rule, and only that.
  const page = await openPage('E', '/selection/');
  await expect(title(page)).toHaveText('choose sessions to import');
  const marketingProject = projectOf(page, marketingLabel);
  await expect(marketingProject).toBeVisible();
  await expect(page.locator('.ks-counts', { hasText: `${chosenSessions.length + 1} chosen` })).toBeVisible();
  await marketingProject.getByRole('checkbox', { name: fixture.branchChoice.branch, exact: true }).click();
  await expect(row(page, marketing.prompt)).toBeChecked();
  await expect(row(page, stray.prompt), 'a session recorded on another branch is not chosen').not.toBeChecked();
  await expect(page.locator('.ks-counts', { hasText: `${chosenSessions.length + 2} chosen` })).toBeVisible();
  await advance(page, 'review');
  await save(page).click();
  await expect(page.getByRole('heading', { name: 'your selection is saved' })).toBeVisible();
  laterImports.branchChoice = await finishCounts(page);
  expect(laterImports.branchChoice, 'the import brought in the session of the chosen branch and updated none').toEqual({ new: fixture.branchChoice.imported, updated: 0 });

  const projects = readConfig().selection.harnesses['claude-code'].projects;
  const entry = projects.find((candidate) => candidate.gitRemote === marketingRepo.remote);
  expect(entry, 'the selection names the project').toBeDefined();
  expect(entry.branches, 'and only the branch that was chosen').toEqual([fixture.branchChoice.branch]);
  expect(await storedTitles(page.request), 'the chosen branch was imported').toContain(marketing.prompt);
  await page.close();
});

test('import-honours-the-recorded-branch', async () => {
  // The scan, the tree and the import's selection filter place a session on the
  // branch its transcript records. The clone sits on main now, so a session
  // recorded on feature/stray is not imported when only main was chosen, even
  // though main is what git has checked out.
  const stored = await storedTitles(pageA.request);
  expect(stored, `a session recorded on ${stray.branch} was not chosen, so it is not imported`).not.toContain(stray.prompt);
});

test('computed-styles-and-themes', async () => {
  const wanted = fixture.screens;
  expect(probes.map((entry) => entry.name).sort(), 'every screen was captured and probed').toEqual([...wanted].sort());
  for (const entry of probes) {
    expect(entry.surface, `${entry.name}: the setup surface was on screen`).toBe(true);
    expect(entry.theme, `${entry.name}: the page is in the ${theme()} theme`).toBe(theme());
    expect(entry.loadedFaces.length, `${entry.name}: an Atkinson face is loaded`).toBeGreaterThan(0);
    expect(entry.body, `${entry.name}: body text is Atkinson`).toMatch(/^"?atkinsonHyperlegible/i);
    expect(entry.textElements, `${entry.name}: text was probed`).toBeGreaterThan(5);
    expect(entry.offenders.font, `${entry.name}: elements whose font is not Atkinson`).toEqual([]);
    expect(entry.offenders.radius, `${entry.name}: elements with a rounded corner`).toEqual([]);
    expect(entry.tabularOffenders, `${entry.name}: counts without tabular numbers`).toEqual([]);
    expect(railProblems(entry.rail), `${entry.name}: the step rail's labels sit inside the frame`).toEqual([]);
  }
  // The rail check must have looked at labels: a rail the frame draws as numbers
  // alone has none to judge, so the screens named in the fixture, every one of
  // them, must have drawn and measured one label per step.
  for (const [name, count] of Object.entries(fixture.railScreens)) {
    const entry = probes.find((candidate) => candidate.name === name);
    expect(entry, `${name} is a captured screen`).toBeDefined();
    expect(entry.rail.labelled, `${name}: the rail shows its labels`).toBe(true);
    expect(entry.rail.labels.map((label) => label.text), `${name}: the rail measured one label per step`).toHaveLength(count);
  }
  expect(consoleErrors, 'no console error or page error in any tab').toEqual([]);
  // Every refused request was one the journey provoked on purpose: the save that
  // found a changed field (409), and the deep links to work that was never recorded.
  const hash = 'a'.repeat(64);
  const refused = failedResponses.map((entry) => `${entry.status} ${entry.method} ${entry.path.replaceAll(hash, '<hash>').replace(/\/sessions\/[0-9a-f]{32}\//, '/sessions/<id>/')}`).sort();
  expect(refused, 'the refused requests are exactly the provoked ones').toEqual(fixture.expectedRefusals.slice().sort());
  await Promise.all([pageA, pageB].map((page) => page?.close()));
});
