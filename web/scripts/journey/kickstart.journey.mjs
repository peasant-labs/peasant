import { test, expect } from '@playwright/test';
import { appendFileSync, existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import { parse } from 'yaml';
import { License, LocalKickstartEventKind, LocalKickstartIngestState, LocalKickstartNodeKind, LocalKickstartPreviewContinuation, LocalKickstartPreviewEventKind, LocalKickstartPreviewOp, LocalKickstartScanState, LocalKickstartStopKind, MessageType } from '@peasant-labs/schema';
import { railProblems } from './lib/kickstart-rail.mjs';
import { longSessionLines } from './lib/kickstart-seed.mjs';

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
// unstring-allow: endpoint-literal — pinned schema has no route members
const API = '/api/v1/kickstart';
const Routes = Object.freeze({
  // unstring-allow: endpoint-literal — pinned schema has no route members
  SyncSessions: '/api/v1/sync/sessions',
  // unstring-allow: endpoint-literal — pinned schema has no route members
  SyncAuth: '/api/v1/sync/auth',
  // unstring-allow: endpoint-literal — pinned schema has no route members
  Observations: '/api/v1/village/collective-observations',
  // unstring-allow: endpoint-literal — pinned schema has no route members
  Sessions: '/api/v1/sessions',
  // unstring-allow: endpoint-literal — pinned schema has no route members
  SyncIngest: '/api/v1/sync/ingest',
});
const Method = Object.freeze({ Get: 'GET', Post: 'POST' });
const ConsoleLevel = Object.freeze({ Error: 'error' });
const Tab = Object.freeze({ A: 'A', B: 'B', C: 'C', D: 'D', E: 'E', F: 'F', G: 'G' });
const Css = Object.freeze({ None: 'none', Hidden: 'hidden' });
const Labels = Object.freeze({
  Continue: 'continue', SaveAndImport: 'save and import', ImportFinished: 'import finished',
  SetUp: 'set up peasant', Yes: 'yes', No: 'no', Back: 'back', ScanAgain: 'scan again',
  ScanProgress: 'scan progress', AutoIngest: /auto-ingest new branches/,
  PublishLater: 'plan to publish later', RedactionExample: 'example for redaction level',
  SetupSaved: 'your setup is saved', TakeTheirs: /^take theirs for/,
  NothingPublished: 'nothing was published and no hook was installed',
  ImportNow: 'import now', Importing: 'reading your selected sessions',
  ChooseRules: 'choose automatic publishing rules', AutomaticPublishing: 'automatic publishing',
  Settings: 'settings', KeepLocal: 'keep local, do not publish',
  ChooseSessions: 'choose sessions', SelectionSaved: 'your selection is saved',
  LoadMore: 'load more of this session', Raw: 'raw, on this computer only',
  LoadedSoFar: 'loaded so far', Highlights: 'highlights', FinalResponse: 'final response',
  LatestLoaded: 'latest loaded response', Loading: 'loading', Trace: /^trace/,
});
const previewTitle = (prompt) => `preview ${prompt}`;
// The trace is read past its end, then the page is given this long to start a
// load it must not start. The wake source is the socket: any request it sends
// is recorded the moment it leaves.
const SCROLL_SETTLE_MS = 1500;

// The pointer's click-through: the connection stop's answer and the next stop's
// `continue` sit in the same foot slot, so a right edge or vertical centre that
// differs by more than half a pixel would make the reader move the pointer.
const FOOT_ALIGNMENT_TOLERANCE = 0.5;
// The narrowest foot: at 320 px back and the count keep the first line, and the
// answers take a second full-width line of their own.
const NARROW_FOOT_WIDTH = 320;
// A burst of window focuses on `/village`. The shared reader starts one read,
// holds the rest inside its two-second gap, and makes one trailing read.
const FOCUS_BURST = { focuses: 5, intervalMs: 200, gapMs: 2000, expectedReads: 2 };

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
// Where the connection stop's answers and the next stop's `continue` were laid
// out, and the narrow foot they were measured beside.
let footAlignment = null;
// The sign-in reads a burst of window focuses caused on `/village`: the page's
// own read, then the burst's leading and trailing reads.
let focusBurstReads = null;
// What the finish panel shows once "import now" has run an import after the first
// one ended, and the ingest states the first tab was pushed for that run.
let importNowCounts = null;
let importNowStates = null;
let importedAfterSetup = null;
const laterImports = {};
// The selection the setup saved, as the file holds it, kept in the evidence.
let savedSelection = null;
// The project each session was listed under in the tree the person chose from,
// read from the server's own tree, and what the store holds for each import.
const treeProjectOf = new Map();
let storedRemotes = null;

function watch(page, label) {
  page.on('console', (message) => {
    // The browser logs every non-2xx response as a console error with no URL;
    // those are accounted for one by one in failedResponses below.
    if (message.type() === ConsoleLevel.Error && !message.text().startsWith('Failed to load resource')) consoleErrors.push(`${label}: ${message.text()}`);
  });
  page.on('response', (response) => {
    if (response.status() >= 400) failedResponses.push({ tab: label, status: response.status(), method: response.request().method(), path: new URL(response.url()).pathname });
  });
  page.on('pageerror', (error) => consoleErrors.push(`${label} pageerror: ${error.message}`));
  page.on('request', (request) => {
    const url = new URL(request.url());
    const entry = { tab: label, method: request.method(), path: url.pathname, origin: url.origin, protocol: url.protocol };
    // Which scope a page asked a session for says which page opened it.
    if (entry.method === Method.Post && entry.path === `${API}/sessions`) {
      try { entry.scope = JSON.parse(request.postData() ?? '{}').scope; } catch { entry.scope = null; }
    }
    requests.push(entry);
  });
  page.on('websocket', (socket) => socket.on('framereceived', (event) => {
    if (typeof event.payload !== 'string') return;
    let message;
    try { message = JSON.parse(event.payload); } catch { return; }
    if (message.type !== MessageType.Kickstart || !message.data) return;
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
const next = (page) => page.getByRole('button', { name: Labels.Continue, exact: true });
const save = (page) => page.getByRole('button', { name: Labels.SaveAndImport });

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
  const response = await request.get(Routes.SyncSessions);
  expect(response.ok()).toBe(true);
  return (await response.json()).items.map((item) => item.title).sort();
}

const readConfig = () => parse(readFileSync(configPath, 'utf8'));

/**
 * What the finish panel says the import did, once it has ended: the sessions
 * the store did not have (new) and the ones it had and re-read (updated).
 */
async function finishCounts(page) {
  await expect(page.getByText(Labels.ImportFinished)).toBeVisible();
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
  return page.evaluate((css) => {
    const family = /^"?atkinsonHyperlegible(Mono)?"?(,|$)/i;
    const corners = ['borderTopLeftRadius', 'borderTopRightRadius', 'borderBottomLeftRadius', 'borderBottomRightRadius'];
    const surface = document.querySelector('.wizard-page');
    const offenders = { font: [], radius: [] };
    let textElements = 0;
    let drawn = 0;
    const describe = (element) => `${element.tagName.toLowerCase()}${element.className && typeof element.className === 'string' ? `.${element.className.trim().split(/\s+/).join('.')}` : ''}`;
    for (const element of surface ? surface.querySelectorAll('*') : []) {
      const style = getComputedStyle(element);
      if (style.display === css.None || style.visibility === css.Hidden) continue;
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
    const frame = document.querySelector('.wizard-host');
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
        visible: box.width > 0 && box.height > 0 && style.visibility !== css.Hidden && style.display !== css.None,
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
  }, Css);
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
    theme: theme(), shots, probes, consoleErrors, failedResponses, walked, offerSeen, ingestCounts, importNowCounts, importNowStates, laterImports, importedAfterSetup, savedSelection, storedRemotes, footAlignment, focusBurstReads,
    frames: frames.map(({ tab, kind, revision, reason, scan, ingest }) => ({ tab, kind, revision, reason, scan, ingest })),
    requests: requests.filter((entry) => entry.path.startsWith('/api/')).map((entry) => `${entry.tab} ${entry.method} ${entry.path}${entry.scope === undefined ? '' : ` (scope ${entry.scope})`}`),
  }, null, 2));
  await context?.close();
});

// ── the cases ───────────────────────────────────────────────────────────────

test('fresh-install-redirects-to-setup', async () => {
  pageA = await context.newPage();
  watch(pageA, Tab.A);
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
    .filter((entry) => entry.tab === Tab.A && entry.method === Method.Post && entry.path === `${API}/sessions`)
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

  await expect(pageA.getByRole('heading', { level: 1, name: Labels.SetUp })).toBeVisible();
  await expect(title(pageA)).toHaveText('peasant online');
  await seen(pageA);
  // The opening scan may still be running when the first stop is drawn; its
  // later stops arrive when the scan ends. Capture the surface the reader
  // settles on, not a rail that is still growing.
  await expect.poll(async () => (await snapshot(pageA.request)).view.scan.state, { message: 'the opening scan ended', timeout: 30000 }).toBe('done');
  const { id, view } = await snapshot(pageA.request);
  const expectedStops = visibleStops(view).length;
  await expect.poll(async () => (await stepOf(pageA)).total, 'the page holds the stops the settled view lists').toBe(expectedStops);
  const first = await stepOf(pageA);
  expect(first.index).toBe(1);
  expect(first.total, 'the page counts the stops the server shows').toBe(expectedStops);
  await expect.poll(() => frames.some((frame) => frame.tab === Tab.A && frame.kind === LocalKickstartEventKind.Session && frame.sessionId === id), 'the topic sends the session snapshot on subscribe').toBe(true);
  await shoot(pageA, '01-village');
});

test('answers-sit-where-continue-sits', async () => {
  // The connection stop is answered from the foot, where every other stop's
  // `continue` sits. The reader who clicked through lands on `yes` without
  // moving the pointer, and the narrow foot keeps back and the count on their
  // own line above the answers.
  const yes = pageA.getByRole('button', { name: Labels.Yes, exact: true });
  const answer = await yes.boundingBox();
  expect(answer, 'the connection stop draws yes in its foot').not.toBeNull();
  const desktop = test.info().project.use.viewport;

  await pageA.setViewportSize({ width: NARROW_FOOT_WIDTH, height: desktop.height });
  const narrow = await pageA.locator('.swz-foot').evaluate((foot) => {
    const box = (selector) => {
      const found = foot.querySelector(selector);
      return found === null ? null : found.getBoundingClientRect().toJSON();
    };
    return { start: box('.swz-foot-start'), count: box('.swz-count'), end: box('.swz-foot-end'), answers: box('.ks-foot-answers') };
  });
  expect(narrow.start, 'the narrow foot draws back').not.toBeNull();
  expect(narrow.answers, 'the narrow foot draws the answers').not.toBeNull();
  expect(narrow.answers.top, 'the answers take their own line under back').toBeGreaterThanOrEqual(narrow.start.bottom - FOOT_ALIGNMENT_TOLERANCE);
  expect(narrow.count.top, 'the count keeps the first line with back').toBeLessThan(narrow.answers.top);
  expect(Math.abs(narrow.answers.right - narrow.end.right), "the answers end at the foot's right edge").toBeLessThanOrEqual(FOOT_ALIGNMENT_TOLERANCE);
  await shoot(pageA, '12-answers-narrow');
  await pageA.setViewportSize(desktop);

  // `yes` must sit exactly where the next stop's `continue` sits: same right
  // edge, same vertical centre, so a second click-through needs no pointer move.
  await pageA.getByRole('button', { name: Labels.No, exact: true }).click();
  await expect(title(pageA)).toHaveText('choose sessions to import');
  const proceed = await next(pageA).boundingBox();
  expect(proceed, 'the tree stop draws continue in its foot').not.toBeNull();
  const right = (box) => box.x + box.width;
  expect(Math.abs(right(answer) - right(proceed)), 'the answer and continue share a right edge').toBeLessThanOrEqual(FOOT_ALIGNMENT_TOLERANCE);
  expect(Math.abs((answer.y + answer.height / 2) - (proceed.y + proceed.height / 2)), 'the answer and continue share a vertical centre').toBeLessThanOrEqual(FOOT_ALIGNMENT_TOLERANCE);
  footAlignment = { answer, proceed, narrow };

  // Leave the walk where the story left it: the next case answers the
  // connection stop itself.
  await pageA.getByRole('button', { name: Labels.Back }).click();
  await expect(title(pageA)).toHaveText('peasant online');
});

test('scan-progress-and-tree-selection', async () => {
  const first = await stepOf(pageA);
  await pageA.getByRole('button', { name: Labels.No, exact: true }).click();
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
  await pageA.getByRole('button', { name: Labels.ScanAgain }).click();
  const bar = pageA.getByRole('progressbar', { name: Labels.ScanProgress });
  await expect(bar).toBeVisible();
  await shoot(pageA, '02-scan-progress');
  await expect(bar).toBeHidden({ timeout: 30000 });
  await expect(pageA.locator('.ks-counts', { hasText: `${total} sessions found` })).toBeVisible();

  const values = (await pageA.evaluate(() => window.__ks.values)).map(Number);
  expect(values.some((value) => value > 0 && value < 100), `the bar advanced through the scan (saw ${values.join(', ')})`).toBe(true);
  const scans = frames.slice(frameMark).filter((frame) => frame.tab === Tab.A && frame.kind === LocalKickstartEventKind.Scan);
  expect(new Set(scans.filter((frame) => frame.scan.state === LocalKickstartScanState.Running && frame.scan.done > 0).map((frame) => frame.scan.done)).size, 'the server pushed more than one progress tick').toBeGreaterThan(1);
  expect(scans.at(-1).scan.state, 'the last pushed scan state').toBe('done');
  expect(requests.slice(requestMark).filter((entry) => entry.tab === Tab.A && entry.method === Method.Get && /\/tree$/.test(entry.path)).length, 'the forest was read again when the scan ended').toBeGreaterThan(0);

  // What the tree lists each session under, from the server's own tree: the
  // project row above its branch row. The stored remote is compared with it later.
  const { id: wizardId } = await snapshot(pageA.request);
  const treeNodes = (await (await pageA.request.get(`${API}/sessions/${wizardId}/tree`)).json()).nodes;
  const nodeById = new Map(treeNodes.map((node) => [node.id, node]));
  for (const node of treeNodes.filter((candidate) => candidate.kind === LocalKickstartNodeKind.Session)) {
    let up = node;
    while (up && up.kind !== LocalKickstartNodeKind.Project) up = nodeById.get(up.parentId);
    if (up) treeProjectOf.set(node.sessionId ?? node.id, up.label);
  }
  for (const entry of seeded) expect(treeProjectOf.has(entry.sessionId), `the tree lists ${entry.name} under a project`).toBe(true);

  // Nothing is chosen yet, and the tree is the server's, not the page's.
  await expect(pageA.locator('.ks-counts', { hasText: '0 chosen' })).toBeVisible();
  await branch(pageA, 'main').scrollIntoViewIfNeeded();
  await project(pageA).getByRole('checkbox', { name: fixture.chosen.branch, exact: true }).click();
  await expect(pageA.locator('.ks-counts', { hasText: `${chosenSessions.length} chosen` })).toBeVisible();
  await expect(project(pageA).first().getByRole('checkbox').first()).toHaveAttribute('aria-checked', 'mixed');
  await expect(project(pageA).getByRole('checkbox', { name: fixture.chosen.branch, exact: true })).toHaveAttribute('aria-checked', 'true');
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
  pageB = await openPage(Tab.B, '/kickstart/');
  await expect(title(pageB)).toHaveText('peasant online');
  const navigations = { count: 0 };
  pageB.on('framenavigated', () => { navigations.count += 1; });
  await pageB.evaluate(() => { window.__tab = 'tab-b'; });
  const sameSession = await Promise.all([pageA.request, pageB.request].map(async (request) => (await snapshot(request)).id));
  expect(new Set(sameSession).size, 'two tabs of the wizard share one session').toBe(1);

  // B opens on the answers A already gave.
  await pageB.getByRole('button', { name: Labels.No, exact: true }).click();
  await expect(title(pageB)).toHaveText('choose sessions to import');
  await expect(pageB.locator('.ks-counts', { hasText: `${chosenSessions.length} chosen` })).toBeVisible();
  await expect(project(pageB).getByRole('checkbox', { name: fixture.chosen.branch, exact: true })).toHaveAttribute('aria-checked', 'true');

  // Both move to the auto-ingest stop. A changes it; B sees that with no reload.
  await advance(pageA, 'auto-ingest new branches');
  await advance(pageB, 'auto-ingest new branches');
  const autoIngest = (page) => page.getByRole('checkbox', { name: Labels.AutoIngest });
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
  await pageB.getByRole('radio', { name: Labels.PublishLater }).click();
  await advance(pageA, 'publication');
  await expect(pageA.getByRole('radio', { name: Labels.PublishLater }), "A shows B's answer").toBeChecked();
  expect(requests.filter((entry) => /\/api\/v1\/kickstart\/sessions\/[0-9a-f]+$/.test(entry.path) && entry.method === Method.Get), 'no tab read the snapshot to learn of the other').toEqual([]);
});

test('every-stop-the-server-shows', async () => {
  await advance(pageA, 'auto-publish');
  await advance(pageA, 'privacy');
  const example = pageA.getByRole('list', { name: Labels.RedactionExample });
  await expect(example).toBeVisible();
  const lines = await example.getByRole('listitem').allTextContents();
  expect(lines.length, 'the example shows a before and an after').toBeGreaterThanOrEqual(2);
  expect(new Set(lines.map((line) => line.trim())).size, 'the lines differ').toBeGreaterThan(1);
  await shoot(pageA, '05-privacy');

  await advance(pageA, 'content license');
  await pageA.getByRole('radio', { name: License.CC0 }).click();
  await expect(pageA.getByRole('radio', { name: License.CC0 })).toBeChecked();

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
  await expect(pageA.getByRole('button', { name: Labels.Yes, exact: true })).toBeVisible();
  await expect(next(pageA), 'the offer has its own answers, not continue').toHaveCount(0);
  await shoot(pageA, '06-offer');
  await pageA.getByRole('button', { name: Labels.No, exact: true }).click();
  await expect(title(pageA)).toHaveText('claude retention');
  await seen(pageA);
  await advance(pageA, 'review');

  // The stops walked are the stops the server shows, in its order, with the
  // offer after the one it follows.
  const { view } = await snapshot(pageA.request);
  const expected = visibleStops(view).filter((stop) => stop.kind !== LocalKickstartStopKind.Ingest).map((stop) => stop.title);
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
  await expect(pageA.getByRole('heading', { name: Labels.SetupSaved })).toHaveCount(0);
  await shoot(pageA, '08-review-conflict');

  // B was not asked; it is shown the same rows when it reaches the review.
  while (await next(pageB).count()) await next(pageB).click();
  await expect(title(pageB)).toHaveText('review');
  await expect(pageB.locator('.ks-conflict')).toHaveCount(1);

  // The choice is the person's: take the saved value.
  await pageA.getByRole('button', { name: Labels.TakeTheirs }).click();
  await expect(pageA.locator('.ks-conflict')).toHaveCount(0);
  await expect(pageB.locator('.ks-conflict'), 'the settled row leaves every tab').toHaveCount(0);
  expect(readFileSync(configPath, 'utf8'), 'choosing writes nothing either').toBe(theirs);
});

test('save-finish-and-ingest-end', async () => {
  await save(pageA).click();
  const saved = (page) => page.getByRole('heading', { name: Labels.SetupSaved });
  await expect(saved(pageA)).toBeVisible();
  await expect(saved(pageB), 'the other tab moves to the finish with no reload').toBeVisible();
  await expect(pageA.locator('.ks-finish code')).toHaveText(configPath);
  await expect(pageA.getByText(Labels.NothingPublished)).toBeVisible();

  // The import reports its end: idle with counts, never a stream of ticks.
  ingestCounts = await finishCounts(pageA);
  // Every chosen session is new to a fresh store, and nothing is updated. A panel
  // that reports 0 new after the sessions were stored is wrong, not a count.
  expect(ingestCounts, 'the import brought in exactly the chosen sessions').toEqual({ new: fixture.chosen.imported, updated: 0 });
  const ingests = frames.filter((frame) => frame.tab === Tab.A && frame.kind === LocalKickstartEventKind.Ingest);
  expect(ingests.filter((frame) => frame.ingest.state === LocalKickstartIngestState.Idle), 'one end event').toHaveLength(1);
  expect(ingests.at(-1).ingest.state).toBe(LocalKickstartIngestState.Idle);
  expect(ingests.at(-1).ingest.new).toBe(ingestCounts.new);
  expect(ingests.at(-1).ingest.updated).toBe(ingestCounts.updated);
  expect(frames.filter((frame) => frame.tab === Tab.A && frame.kind === LocalKickstartEventKind.Closed).map((frame) => frame.reason)).toEqual(['committed']);
  await expect(pageA.getByRole('button', { name: Labels.ImportNow })).toBeEnabled();

  // The store holds exactly the sessions that were chosen.
  const stored = await storedTitles(pageA.request);
  expect(stored, 'only the chosen sessions were imported').toEqual(chosenTitles);
  await shoot(pageA, '09-finish');

  // "import now" after the first import ended. The panel has to leave the first
  // import's counts for the second run's: the server pushes the run's start and its
  // end to the saved session, so the page is never left waiting for an end that is
  // not pushed. Every chosen session is stored already, so the second run brings in
  // none and updates none, and its counts differ from the first run's.
  expect(fixture.chosen.imported, 'the first import brought sessions in, so the second run\'s counts differ from it').toBeGreaterThan(0);
  const pushedBefore = frames.filter((frame) => frame.tab === Tab.A && frame.kind === LocalKickstartEventKind.Ingest).length;
  await pageA.getByRole('button', { name: Labels.ImportNow }).click();
  await expect(pageA.locator('.ks-finish .ks-counts').first(), 'the panel shows the end of the second import, with its counts').toHaveText(/^\s*0 new,\s*0 updated\s*$/);
  await expect(pageA.getByText(Labels.ImportFinished)).toBeVisible();
  await expect(pageA.getByText(Labels.Importing), 'the spinner is gone').toHaveCount(0);
  await expect(pageA.getByRole('button', { name: Labels.ImportNow })).toBeEnabled();
  importNowCounts = await finishCounts(pageA);
  importNowStates = frames.filter((frame) => frame.tab === Tab.A && frame.kind === LocalKickstartEventKind.Ingest).slice(pushedBefore).map((frame) => frame.ingest.state);
  expect(importNowStates, 'the server pushed the second import\'s start and then its end').toEqual([LocalKickstartIngestState.Running, LocalKickstartIngestState.Idle]);
  expect(importNowCounts, 'nothing new came in').toEqual({ new: 0, updated: 0 });
  expect(await storedTitles(pageA.request), 'and the store holds the same sessions').toEqual(chosenTitles);

  // The finish only offers: the settings section it names is a real one.
  const link = pageA.getByRole('link', { name: Labels.ChooseRules });
  // The app serves directory-style pages, so the link carries the trailing slash.
  const target = new URL(await link.getAttribute('href'), 'http://app.invalid');
  expect(target.pathname.replace(/\/$/, '')).toBe('/settings');
  expect(target.searchParams.get('section')).toBe('automatic-publishing');
  await link.click();
  await expect(pageA).toHaveURL(/\/settings\/?\?section=automatic-publishing/);
  await expect(pageA.getByRole('heading', { name: Labels.AutomaticPublishing }).first()).toBeVisible();

  // Setup is saved, so no page redirects again. The top-level page reads the
  // status route when it loads; the redirect is decided by that answer, so the
  // answer is awaited and the network is let go quiet before the address is read.
  const statusRead = pageA.waitForResponse((response) => new URL(response.url()).pathname === `${API}/status`, { timeout: 20000 });
  await pageA.goto('/', { waitUntil: 'networkidle', timeout: 20000 });
  expect((await (await statusRead).json()).configured, 'the page was told that setup is saved').toBe(true);
  expect(new URL(pageA.url()).pathname, 'a saved setup is never redirected').toBe('/');
  expect((await (await pageA.request.get(`${API}/status`)).json()).configured).toBe(true);
});

test('focus-burst-reads-sign-in-once', async () => {
  // The sidebar and the online page share one sign-in reader. Five window
  // focuses inside one second are one leading read and one trailing read,
  // however many listeners are mounted; the develop build read once per focus
  // per listener, so the same burst was ten reads. The setup is saved by now,
  // so `/village` is the online page rather than a first-run redirect.
  const pageF = await context.newPage();
  watch(pageF, Tab.F);
  const firstRead = pageF.waitForResponse((response) => new URL(response.url()).pathname === Routes.SyncAuth, { timeout: 20000 });
  await pageF.goto('/village/');
  expect(new URL(pageF.url()).pathname, 'the online page stays the online page').toBe('/village/');
  await (await firstRead).finished();
  const reads = () => requests.filter((entry) => entry.tab === Tab.F && entry.method === Method.Get && entry.path === Routes.SyncAuth).length;
  expect(reads(), 'the online page read the sign-in once when it opened').toBe(1);

  // The burst starts outside the two-second gap the opening read began, so its
  // first focus is a leading read and the rest are held for the trailing one.
  await pageF.evaluate((ms) => new Promise((resolve) => setTimeout(resolve, ms)), FOCUS_BURST.gapMs + 500);
  const before = reads();
  await pageF.evaluate(async ({ focuses, intervalMs }) => {
    for (let index = 0; index < focuses; index += 1) {
      window.dispatchEvent(new Event('focus'));
      if (index + 1 < focuses) await new Promise((resolve) => setTimeout(resolve, intervalMs));
    }
  }, FOCUS_BURST);
  await expect.poll(reads, { message: 'the burst made one leading read and one trailing read', timeout: 15000 }).toBe(before + FOCUS_BURST.expectedReads);
  await expect.poll(() => new URL(pageF.url()).pathname, 'the online page kept the reader').toBe('/village/');
  // The page's own saved-observations read runs at opening and on each focus;
  // wait for the last refusal so the journey's exact refusal list is settled.
  await expect.poll(
    () => failedResponses.filter((entry) => entry.tab === Tab.F && entry.status === 401 && entry.path === Routes.Observations).length,
    { message: 'the online page finished its saved-observations reads', timeout: 15000 },
  ).toBe(FOCUS_BURST.focuses + 1);
  focusBurstReads = { landed: new URL(pageF.url()).pathname, before, after: reads(), ...FOCUS_BURST };
  await pageF.close();
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
  const touched = requests.filter((entry) => entry.method !== Method.Get && !entry.path.startsWith(`${API}/`)).map((entry) => `${entry.method} ${entry.path}`);
  // The only write outside the setup's own routes is the finish panel's "import now".
  expect(touched, 'the only writes were setup\'s own').toEqual([`${Method.Post} ${Routes.SyncIngest}`]);
  // The online page is mounted later for the sign-in read; its own saved
  // observations read is not part of the setup's walk.
  expect(requests.filter((entry) => entry.tab !== Tab.F && entry.path.startsWith('/api/') && /\/auto-publish|\/sync\/push|\/sync\/login|\/village\//.test(entry.path)), 'no publish, sign-in or hook route was called').toEqual([]);
});

test('selection-page-saves-only-selection', async () => {
  const before = readConfig();
  const { selection: beforeSelection, ...beforeRest } = before;

  // A wizard is open with an answer that is not saved.
  const pageC = await openPage(Tab.C, '/kickstart/?from=settings');
  await expect(pageC.locator('.wizard-page').getByRole('link', { name: Labels.Settings, exact: true }), 'the trail says setup was opened from settings').toBeVisible();
  await expect(title(pageC)).toHaveText('peasant online');
  await pageC.getByRole('button', { name: Labels.No, exact: true }).click();
  await expect(title(pageC)).toHaveText('choose sessions to import');
  await expect(pageC.locator('.ks-counts', { hasText: `${chosenSessions.length} chosen` }), 're-running setup starts from the saved selection').toBeVisible();
  for (const entry of chosenSessions) await expect(row(pageC, entry.prompt)).toBeChecked();
  await advance(pageC, 'auto-ingest new branches');
  await expect(pageC.getByRole('checkbox', { name: Labels.AutoIngest })).not.toBeChecked();
  await advance(pageC, 'publication');
  await expect(pageC.getByRole('radio', { name: Labels.PublishLater }), 'the saved preference is filled in').toBeChecked();
  await pageC.getByRole('radio', { name: Labels.KeepLocal }).click();
  await advance(pageC, 'auto-publish');
  await advance(pageC, 'privacy');
  await advance(pageC, 'content license');
  await expect(pageC.getByRole('radio', { name: License.CCBY }), 'the saved license is filled in').toBeChecked();
  await shoot(pageC, '11-rerun-prefilled');
  const wizard = (await snapshot(pageC.request)).id;

  // The selection page has its own session and its own three stops.
  const pageD = await openPage(Tab.D, '/selection/');
  await expect(pageD.getByRole('heading', { level: 1, name: Labels.ChooseSessions })).toBeVisible();
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
  await expect(pageD.getByRole('heading', { name: Labels.SelectionSaved })).toBeVisible();
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
  await expect(pageC.getByRole('heading', { name: Labels.SetupSaved })).toHaveCount(0);
  for (const stop of ['privacy', 'auto-publish', 'publication']) {
    await pageC.getByRole('button', { name: Labels.Back }).click();
    await expect(title(pageC)).toHaveText(stop);
  }
  await expect(pageC.getByRole('radio', { name: Labels.KeepLocal }), 'the wizard kept its unsaved answer').toBeChecked();
  expect(readConfig().push.sharePreference, 'and saved nothing').toBe('share-later');
});

test('selection-page-names-the-chosen-branch', async () => {
  // One branch of a project whose clone hosts sessions of two branches: the
  // selection page saves a branch rule, and only that.
  const page = await openPage(Tab.E, '/selection/');
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
  await expect(page.getByRole('heading', { name: Labels.SelectionSaved })).toBeVisible();
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

test('stored-remote-matches-the-tree', async () => {
  // Every imported session was stored with the remote and the branch it was
  // listed under in the tree the person chose from. The tree's project row, the
  // project the store files the session under, and the remote and branch the
  // store holds for it are read from the server, and the seeded repository is
  // the only place the journey says what they should be.
  const list = (await (await pageA.request.get(Routes.Sessions)).json()).sessions;
  const stored = [];
  for (const row of list) {
    const detail = await (await pageA.request.get(`${Routes.Sessions}/${row.id}`)).json();
    stored.push({ id: row.id, project: row.project, gitRemote: detail.gitRemote, gitBranch: detail.gitBranch });
  }
  const imported = new Set([...chosenIds, taxReport.sessionId, marketing.sessionId]);
  expect(new Set(stored.map((entry) => entry.id)), 'exactly the sessions that were chosen and imported are stored').toEqual(imported);
  storedRemotes = stored.map((entry) => {
    const recorded = seeded.find((candidate) => candidate.sessionId === entry.id);
    return { name: recorded.name, tree: treeProjectOf.get(entry.id), ...entry };
  });
  for (const entry of storedRemotes) {
    const recorded = seeded.find((candidate) => candidate.name === entry.name);
    expect(entry.tree, `${entry.name}: the tree listed it under a project`).toBeTruthy();
    expect(entry.tree, `${entry.name}: the tree's project row names its repository`).toContain(new URL(recorded.remote).pathname.replace(/^\//, '').replace(/\.git$/, ''));
    expect(entry.project, `${entry.name}: the store files it under the project the tree listed it under`).toBe(entry.tree);
    expect(entry.gitRemote, `${entry.name}: the remote it was stored with is its repository's`).toBe(recorded.remote);
    expect(entry.gitBranch, `${entry.name}: the branch it was stored with is the branch it recorded`).toBe(recorded.branch);
  }
});

// ── the raw session preview ─────────────────────────────────────────────────
// One page of the selection route, its socket routed through the journey so a
// segment can be held back to show the pane loading. Every preview request the
// page sends and every preview frame it receives is recorded by kind, op, seq
// and continuation only: never its text.
const previewSessions = {
  long: seeded.find((entry) => entry.name === fixture.preview.long),
  growing: seeded.find((entry) => entry.name === fixture.preview.growing),
  oversized: seeded.find((entry) => entry.name === fixture.preview.oversized),
};
let pageG;
let pageGHref;
const previewSent = [];
const previewReceived = [];
const previewHold = { active: false, queue: [], forward: null };

function previewMessage(text) {
  if (typeof text !== 'string') return null;
  try {
    const message = JSON.parse(text);
    return message.type === MessageType.KickstartPreview ? message : null;
  } catch {
    return null;
  }
}

async function openPreviewPage() {
  const page = await context.newPage();
  watch(page, Tab.G);
  await page.routeWebSocket(/\/api\/v1\/ws$/, (ws) => {
    const server = ws.connectToServer();
    ws.onMessage((message) => {
      const preview = previewMessage(message);
      if (preview) previewSent.push({ op: preview.preview?.op, after: preview.preview?.after });
      server.send(message);
    });
    server.onMessage((message) => {
      const preview = previewMessage(message);
      if (preview) {
        const { kind, seq, continuation, segment, reason } = preview.data ?? {};
        previewReceived.push({ kind, seq, continuation, reason, segment: segment?.index ?? null });
        if (previewHold.active && kind === LocalKickstartPreviewEventKind.Segment) {
          previewHold.queue.push(message);
          return;
        }
      }
      ws.send(message);
    });
    previewHold.forward = (message) => ws.send(message);
  });
  // After the story's saved setup this is the selection page. Run on its own
  // (`--grep=preview-`), the computer is not set up, the page is sent to setup,
  // and the wizard's own selection stop is the same step.
  await page.goto('/selection/');
  const selection = title(page).filter({ hasText: 'choose sessions to import' });
  const setup = page.getByRole('button', { name: Labels.No, exact: true });
  await expect(selection.or(setup)).toBeVisible({ timeout: 20000 });
  if (await selection.count() === 0) await setup.click();
  await expect(title(page)).toHaveText('choose sessions to import');
  pageGHref = page.url();
  return page;
}

const pane = (page) => page.locator('.ks-session-preview');
const loadMore = (page) => pane(page).getByRole('button', { name: Labels.LoadMore, exact: true });
const sent = (op) => previewSent.filter((entry) => entry.op === op).length;
const segmentsReceived = () => previewReceived.filter((entry) => entry.kind === LocalKickstartPreviewEventKind.Segment && entry.segment !== null).length;

async function openPreview(page, entry) {
  const button = page.getByRole('button', { name: previewTitle(entry.prompt), exact: true });
  await button.scrollIntoViewIfNeeded();
  await button.click();
  await expect(pane(page).getByText(Labels.Raw)).toBeVisible();
  await expect(pane(page)).toContainText(entry.prompt);
}

/** Activate load more until the directory completes. Each activation sends one request and waits for its frame. */
async function loadToEnd(page) {
  for (let guard = 0; guard < 32 && await loadMore(page).count() > 0; guard += 1) {
    const before = previewReceived.length;
    await loadMore(page).click();
    await expect.poll(() => previewReceived.length, { message: 'the server answered the load' }).toBeGreaterThan(before);
    await expect(loadMore(page).or(pane(page).locator('.ks-preview-notice')).or(pane(page).locator('[data-transcript-continuation]'))).toBeVisible();
    if (await loadMore(page).count() > 0) await expect(loadMore(page)).toBeEnabled();
  }
  await expect(loadMore(page)).toHaveCount(0);
}

async function highlightsSay(page, words) {
  await pane(page).getByRole('tab', { name: new RegExp(`^${Labels.Highlights}`) }).click();
  // The outline rail names the same response; it is closed, so only the drawn tab counts.
  await expect(pane(page).getByText(words, { exact: false }).filter({ visible: true }).first()).toBeVisible();
}

test('preview-session-in-the-viewer', async () => {
  pageG = await openPreviewPage();
  await openPreview(pageG, previewSessions.long);
  expect(sent(LocalKickstartPreviewOp.Open), 'one title click opened one capture').toBe(1);
  expect(sent(LocalKickstartPreviewOp.More), 'the first segment painted without a click').toBe(0);
  // The code block of a reply is highlighted: its tokens are drawn in more than one colour.
  const colours = await pane(pageG).locator('pre').first().evaluate((block) => [...new Set([...block.querySelectorAll('span')].map((span) => getComputedStyle(span).color))]);
  expect(colours.length, `the first code block draws its tokens in more than one colour (saw ${colours.join(', ')})`).toBeGreaterThan(1);
  // Previewing does not choose the session.
  await expect(row(pageG, previewSessions.long.prompt)).not.toBeChecked();
  await shoot(pageG, '13-preview-open');
});

test('preview-scroll-to-end-loads-nothing', async () => {
  const before = previewSent.length;
  await pane(pageG).evaluate((root) => {
    for (const element of [root, ...root.querySelectorAll('*')]) {
      if (element.scrollHeight > element.clientHeight) element.scrollTop = element.scrollHeight;
    }
  });
  await pane(pageG).hover();
  await pageG.mouse.wheel(0, 20000);
  await pageG.setViewportSize({ width: 1180, height: 820 });
  await pageG.setViewportSize(test.info().project.use.viewport);
  await pageG.waitForTimeout(SCROLL_SETTLE_MS);
  expect(previewSent.slice(before), 'reading to the end of the trace, wheeling and resizing sent nothing').toEqual([]);
});

test('preview-position-reads-loaded-so-far', async () => {
  await expect(pane(pageG).getByText(Labels.LoadedSoFar, { exact: false }).first()).toBeVisible();
  await highlightsSay(pageG, Labels.LatestLoaded);
  await expect(pane(pageG).getByText(Labels.FinalResponse, { exact: true }).filter({ visible: true })).toHaveCount(0);
  await pane(pageG).getByRole('tab', { name: Labels.Trace }).click();
});

test('preview-load-more-appends-one-segment', async () => {
  const segmentsBefore = segmentsReceived();
  const moreBefore = sent(LocalKickstartPreviewOp.More);
  previewHold.active = true;
  await loadMore(pageG).click();
  await expect(loadMore(pageG)).toBeDisabled();
  await expect(loadMore(pageG)).toHaveText(Labels.Loading);
  await expect.poll(() => previewHold.queue.length, { message: 'the segment the click asked for arrived and is held' }).toBe(1);
  await shoot(pageG, '14-preview-loading');
  previewHold.active = false;
  for (const message of previewHold.queue.splice(0)) previewHold.forward(message);
  await expect(loadMore(pageG)).toBeEnabled();
  expect(sent(LocalKickstartPreviewOp.More) - moreBefore, 'one activation sent one request').toBe(1);
  expect(segmentsReceived() - segmentsBefore, 'and the server sent one segment').toBe(1);
});

test('preview-end-says-final', async () => {
  await loadToEnd(pageG);
  const last = previewReceived.at(-1);
  expect(last.continuation, 'the last frame ended the capture').toBe(LocalKickstartPreviewContinuation.End);
  await expect(pane(pageG).getByText(Labels.LoadedSoFar, { exact: false })).toHaveCount(0);
  await highlightsSay(pageG, Labels.FinalResponse);
  await expect(pane(pageG).getByText(Labels.LatestLoaded, { exact: false }).filter({ visible: true })).toHaveCount(0);
  await pane(pageG).getByRole('tab', { name: Labels.Trace }).click();
  await shoot(pageG, '15-preview-end');
});

test('preview-cut-shows-the-server-sentence', async () => {
  await openPreview(pageG, previewSessions.oversized);
  await loadToEnd(pageG);
  expect(previewReceived.at(-1).continuation, 'the capture was cut').toBe(LocalKickstartPreviewContinuation.Cut);
  await expect(pane(pageG).locator('.ks-preview-notice')).toContainText(fixture.preview.cutNotice);
  await shoot(pageG, '16-preview-cut');
});

test('preview-grown-session-notice', async () => {
  await openPreview(pageG, previewSessions.growing);
  // The session grows after its preview opened: more prompt and reply lines are appended.
  const lines = readFileSync(previewSessions.growing.path, 'utf8').trimEnd().split('\n');
  const extra = longSessionLines(lines.at(-1), { sessionId: previewSessions.growing.sessionId, exchanges: fixture.preview.appendExchanges, replyBytes: 2000, batch: 'grown' });
  appendFileSync(previewSessions.growing.path, `${extra.join('\n')}\n`);
  await loadToEnd(pageG);
  expect(previewReceived.at(-1).continuation, 'the capture ended at the extent it fixed').toBe(LocalKickstartPreviewContinuation.End);
  await expect(pane(pageG).locator('.ks-preview-notice')).toContainText(fixture.preview.grownNotice);
});

test('preview-leaves-no-trace', async () => {
  const words = ['the long read', 'ledger totals add up', previewSessions.long.prompt];
  const traces = await pageG.evaluate(async (needles) => {
    const held = (storage) => Array.from({ length: storage.length }, (_, index) => `${storage.key(index)}=${storage.getItem(storage.key(index))}`).join('\n');
    const text = `${held(localStorage)}\n${held(sessionStorage)}`;
    return {
      storage: needles.filter((needle) => text.includes(needle)),
      databases: typeof indexedDB.databases === 'function' ? (await indexedDB.databases()).map((db) => db.name) : [],
      caches: 'caches' in window ? await caches.keys() : [],
    };
  }, words);
  expect(traces.storage, 'browser storage holds no preview text').toEqual([]);
  expect(traces.databases, 'no IndexedDB database was opened').toEqual([]);
  expect(traces.caches, 'no Cache Storage was written').toEqual([]);
  expect(pageG.url(), 'the URL never changed').toBe(pageGHref);
  expect(await pane(pageG).locator('a[href]').count(), 'the pane has no link that could change the URL').toBe(0);
  const origin = new URL(pageGHref).origin;
  const offsite = requests.filter((entry) => entry.tab === Tab.G && /^(https?|wss?):$/.test(entry.protocol) && entry.origin !== origin);
  expect(offsite, 'no request left this computer').toEqual([]);
  // Every capture ended by completing its directory: none was closed under the page.
  expect(previewReceived.filter((entry) => entry.kind === LocalKickstartPreviewEventKind.Closed), 'no capture was closed by the server').toEqual([]);
  await pageG.close();
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
  // The signed-out online page reads its saved observations at opening and on
  // each focus of the burst; every one is refused with 401. That is the page
  // saying nobody is signed in, not a refusal of the setup walk's routes, so it
  // is counted here and left out of the exact list below.
  const signedOut = failedResponses.filter((entry) => entry.tab === Tab.F && entry.status === 401 && entry.path === Routes.Observations);
  expect(signedOut, 'the signed-out online page refused one saved-observations read at opening and one per focus').toHaveLength(FOCUS_BURST.focuses + 1);
  // Every other refused request was one the journey provoked on purpose: the save
  // that found a changed field (409), and the deep links to work never recorded.
  const hash = 'a'.repeat(64);
  const refused = failedResponses.filter((entry) => !signedOut.includes(entry)).map((entry) => `${entry.status} ${entry.method} ${entry.path.replaceAll(hash, '<hash>').replace(/\/sessions\/[0-9a-f]{32}\//, '/sessions/<id>/')}`).sort();
  expect(refused, 'the refused requests are exactly the provoked ones').toEqual(fixture.expectedRefusals.slice().sort());
  await Promise.all([pageA, pageB].map((page) => page?.close()));
});
