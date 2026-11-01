import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { parse } from 'yaml';
import { createHash } from 'node:crypto';
import { mutateCompiledAsset } from './lib/compiled-mutation.mjs';
import { zCollectiveFetchReport, zCollectiveObservationResponse, zLocalContributionHistoryResponse } from '@peasant-labs/schema';

const f = parse(readFileSync(new URL('./testdata/collective-observations.yaml', import.meta.url), 'utf8'));
if (new Set(f.cases.map(c => c.name)).size !== f.cases.length || f.requiredNames.some(name => !f.cases.some(c => c.name === name))) throw new Error('The focused journey is missing or repeats a required named scenario');
const root = process.env.OBSERVATIONS_ARTIFACTS;
const exported = JSON.parse(readFileSync(join(root, 'fixture-export.json'), 'utf8'));
const mutationMode = process.env.OBSERVATIONS_MUTANTS === '1';
const local = '/api/v1/village/collective-observations', contributions = '/api/v1/village/contributions';
async function control(request, action, params = {}) {
  const url = new URL('/__fixture/control', process.env.OBSERVATIONS_REMOTE_URL);
  url.search = new URLSearchParams({ action, ...params }).toString();
  const response = await request.post(url.toString(), { headers: { 'x-fixture-control': process.env.OBSERVATIONS_CONTROL_TOKEN } });
  expect(response.ok(), await response.text()).toBe(true); return response.json();
}
async function saved(request) { const response = await request.get(local); expect(response.ok()).toBe(true); return zCollectiveObservationResponse.parse(await response.json()); }
async function history(request, kind, page = 1, revision) {
  const params = new URLSearchParams({ kind, page: String(page), limit: '2' }); if (revision) params.set('revision', revision);
  const response = await request.get(`${contributions}?${params}`); expect(response.ok(), await response.text()).toBe(true);
  return zLocalContributionHistoryResponse.parse(await response.json());
}
/** The page area the app mounted. While Next hydrates a route it keeps a hidden copy of the
 * streamed page outside `main`; a CSS query must count only the mounted section, never that copy. */
function mountedPage(page) { return page.locator('main'); }
/** The phone drawer closes as one user-visible transition: the panel slides out at once and
 * `visibility` flips only at the end of `--dur-2` (fairtrade's AppSidebar.css keeps the closed
 * drawer's controls in the focus order until it is out of view). That flip is a delayed style
 * change, not a DOM animation, and it reaches the controls the geometry read measures later
 * than the panel itself: in the retained trace the panel already reported hidden while the
 * four controls in its scroll area still reported visible, off screen. So settle on those
 * controls — never on the panel property that leads them. */
async function closeDrawer(page) {
  const settled = () => page.locator('.asb').first().evaluate(root => {
    const panel = root.querySelector('.asb-panel');
    if (!panel) return true;
    return [...panel.querySelectorAll('a,button')].every(control => getComputedStyle(control).visibility === 'hidden');
  });
  await expect(page.locator('.asb')).toHaveAttribute('data-drawer-open', 'false');
  await expect.poll(settled).toBe(true);
}
async function open(page, theme) {
  await page.goto('/village');
  const html = page.locator('html');
  if (await html.getAttribute('data-theme') !== theme) {
    // The sidebar's footer toggle is named for the mode it switches to; on a phone it sits in the drawer.
    const menu = page.locator('.asb-bar .asb-menu');
    const drawer = await menu.isVisible();
    if (drawer) await menu.click();
    await page.locator('.asb-panel').getByRole('button', { name: `switch to ${theme} mode`, exact: true }).click();
    // Changing the theme does not navigate, so the phone drawer that holds the toggle stays open
    // over the page. Close it through its own control; the theme is stored, so it is not reopened.
    if (drawer) {
      await page.locator('.asb-bar').getByRole('button', { name: 'close the menu', exact: true }).click();
      await closeDrawer(page);
    }
  }
  await expect(html).toHaveAttribute('data-theme', theme);
  // A closed drawer keeps its layout off screen behind the scrim. Every step below drives the
  // page underneath, so a drawer left open would intercept its clicks.
  await expect(page.locator('.asb')).toHaveAttribute('data-drawer-open', 'false');
  await expect(page.getByRole('heading', { name: 'online observations', exact: true })).toBeVisible();
  await expect(mountedPage(page).locator('.vobs')).toBeVisible();
}
async function filterPanel(page) {
  const disclosure = mountedPage(page).locator('.village-page-filter-tools');
  if (await disclosure.getAttribute('open') === null) await disclosure.locator('summary').click();
  return mountedPage(page).locator('form.village-page-filter');
}
async function captureShell(page, path) {
  await page.evaluate(() => window.scrollTo(0, 0));
  const frame = await page.evaluate(() => ({ document: document.documentElement.scrollHeight, viewport: innerHeight }));
  expect(frame.document, 'the complete app shell must fit its bounded viewport').toBeLessThanOrEqual(frame.viewport + 1);
  // A viewport capture preserves dynamic-vh sizing on mobile Chromium; fullPage
  // can expand that viewport and append an artificial blank area after layout.
  await page.screenshot({ path, fullPage: false });
}
test.beforeEach(async ({ request }) => { await control(request, 'reset'); });

/** A sidebar link, opening the phone drawer first where the sidebar is a top bar. */
async function sidebarLink(page, name) {
  const menu = page.locator('.asb-bar .asb-menu');
  if (await menu.isVisible()) await menu.click();
  return page.locator('.asb-panel').getByRole('link', { name, exact: true });
}

// One bounded scenario per test: failures retain traces; neither the local API nor the UI is intercepted.
async function exercise(row, { page, request }, info) {
  const { theme, surface } = info.project.metadata;
  await open(page, theme);
  if (row.mode === 'navigation' || row.mode === 'direct') {
    // The online page is reached by route only: the sidebar neither links to it nor
    // offers online in its app switcher; its collectives link leads online instead.
    if (row.mode === 'navigation') {
      await page.goto('/');
      await expect(await sidebarLink(page, 'home')).toBeVisible();
      await expect(page.locator('.asb-panel a[href^="/village"]')).toHaveCount(0);
      await expect(page.locator('.asb-utils .asb-app[title="online"]')).toHaveCount(0);
      await expect(page.locator('.asb-panel').getByRole('link', { name: /^collectives/ })).toBeVisible();
      await page.goto('/village');
      await expect(page.getByRole('heading', { name: 'online observations', exact: true })).toBeVisible();
    }
    await expect(page.locator('.asb-panel a[href^="/village"]')).toHaveCount(0);
    await expect(page.locator('.asb-panel [aria-current="page"]')).toHaveCount(0);
    await expect(mountedPage(page).locator('.vobs')).toContainText(exported.fixtures.OldName);
    expect(await control(request, 'requests')).toEqual([]);
    return;
  }
  if (row.mode === 'partial' || row.mode === 'deadline') {
    const before = await saved(request);
    await control(request, 'scenario', { name: row.scenario });
    const response = page.waitForResponse(r => r.url().endsWith(`${local}/fetch`) && r.request().method() === 'POST', { timeout: 45000 });
    await page.getByRole('button', { name: 'fetch metadata', exact: true }).click();
    let result;
    try { result = await response; } catch { throw new Error('The mounted metadata deadline did not finish within 45s. The production service/transport timeout must fit the approved 60s test budget; do not replace this with an intercepted local response.'); }
    expect(result.status()).toBe(row.status);
    const report = zCollectiveFetchReport.parse(await result.json());
    expect(report.outcome).toBe('partial');
    await expect(mountedPage(page).locator('.vobs-status')).toContainText('metadata fetch partial');
    expect(report.domains.find(d => d.domain === 'catalog').commit).toBe('updated');
    expect(report.domains.find(d => d.domain === 'history').lastCompletedAt).toBe(before.fetch.history.lastCompletedAt);
    await expect(mountedPage(page).locator('.vobs')).toContainText(exported.fixtures.NewName);
    await expect(mountedPage(page).locator('[data-domain="history"]')).toContainText(before.fetch.history.lastCompletedAt);
    expect((await saved(request)).fetch.history.lastCompletedAt).toBe(before.fetch.history.lastCompletedAt);
    await captureShell(page, join(root, `${theme}-${surface}-${row.scenario}.png`));
    return;
  }
  if (row.mode === 'paging' || row.mode === 'isolation') {
    await page.getByRole('tab', { name: 'history', exact: true }).click();
    for (const category of f.categories) {
      await page.getByRole('tab', { name: category.label, exact: true }).click();
      const filter = await filterPanel(page);
      await filter.locator('input[type="number"]').fill('2');
      const firstAnswer = page.waitForResponse(r => r.url().includes(contributions) && new URL(r.url()).searchParams.get('kind') === category.kind && new URL(r.url()).searchParams.get('limit') === '2');
      await filter.getByRole('button', { name: 'apply to this category' }).click();
      const first = zLocalContributionHistoryResponse.parse(await (await firstAnswer).json());
      await mountedPage(page).locator('.village-page-filter-tools summary').click();
      const ids = [];
      for (let n = 1; n <= Math.ceil(first.total / 2); n++) {
        if (row.mode === 'isolation' && category.kind !== 'operation-diagnostics') await control(request, 'diagnostic');
        const current = n === 1 ? first : await history(request, category.kind, n, first.traversalRevision);
        expect(current.traversalRevision).toBe(first.traversalRevision);
        const values = current[category.array];
        ids.push(...values.map(value => value.receiptId ?? value.operationId ?? `${value.collectiveId}/${value.transcriptId}/${value.attempt.event_num}`));
        // The actual mounted pager performs the same selected-category read, not a dormant helper.
        if (n > 1) {
          const answer = page.waitForResponse(r => r.url().includes(contributions) && new URL(r.url()).searchParams.get('page') === String(n));
          await mountedPage(page).locator('.vobs-pager:visible').getByRole('button', { name: `page ${n}`, exact: true }).click();
          const mounted = zLocalContributionHistoryResponse.parse(await (await answer).json());
          expect(mounted.traversalRevision).toBe(first.traversalRevision); expect(mounted.page).toBe(n);
        }
      }
      expect(new Set(ids).size).toBe(first.total);
      expect(ids.length).toBe(first.total);
      if (row.mode === 'paging') await captureShell(page, join(root, `${theme}-${surface}-${category.kind}.png`));
    }
    return;
  }
  if (row.mode === 'conflict') {
    await page.getByRole('tab', { name: 'history', exact: true }).click();
    await page.getByRole('tab', { name: 'remote attempts', exact: true }).click();
    await filterPanel(page);
    await mountedPage(page).locator('#history-limit').fill('2');
    await page.getByRole('button', { name: 'apply to this category' }).click();
    await expect(mountedPage(page).locator('.vobs-summary:visible')).toContainText(`${JSON.parse(exported.fixtures.AttemptsJSON).length} saved items`);
    await mountedPage(page).locator('.village-page-filter-tools summary').click();
    const receiptBefore = await history(request, 'receipts');
    await control(request, 'terminal');
    const conflict = page.waitForResponse(r => r.url().includes(contributions) && r.status() === 409);
    await mountedPage(page).locator('.vobs-pager:visible').getByRole('button', { name: 'page 2', exact: true }).click();
    await conflict;
    await page.getByRole('button', { name: 'restart this category' }).click();
    await expect(page.getByRole('button', { name: 'restart this category' })).toHaveCount(0);
    expect((await history(request, 'receipts')).traversalRevision).toBe(receiptBefore.traversalRevision);
    return;
  }
  if (row.mode === 'parity') {
    expect(await control(request, 'cli')).toEqual(await saved(request));
    for (const category of f.categories) {
      const api = await history(request, category.kind);
      const cli = await control(request, 'history', { kind: category.kind, page: '1', limit: '2', revision: api.traversalRevision });
      expect(cli).toEqual(api);
    }
    return;
  }
  if (row.mode === 'tracking') {
    await page.getByRole('button', { name: `track ${exported.fixtures.OldName}`, exact: true }).click();
    await expect(page.getByRole('button', { name: `untrack ${exported.fixtures.OldName}`, exact: true })).toBeVisible();
    const observed = await control(request, 'requests');
    expect(observed.some(call => call.path === `/api/v1/collectives/${exported.fixtures.CollectiveID}`)).toBe(true);
    await page.getByRole('button', { name: `untrack ${exported.fixtures.OldName}`, exact: true }).click();
    await expect(page.getByRole('button', { name: `track ${exported.fixtures.OldName}`, exact: true })).toBeVisible();
    expect(await control(request, 'requests')).toEqual(observed);
    return;
  }
  if (row.mode === 'missing-revision') {
    const response = await request.get(`${contributions}?kind=receipts&page=2&limit=2`);
    expect(response.status()).toBe(400);
    expect(await response.text()).toMatch(/revision|page one|restart/i);
    return;
  }
  if (row.mode === 'settings' || row.mode === 'account') {
    if (row.mode === 'account') {
      await control(request, 'scenario', { name: 'late-history' });
      await page.getByRole('button', { name: 'fetch metadata', exact: true }).click();
      await expect.poll(async () => (await control(request, 'requests')).some(c => c.path.endsWith('/events'))).toBe(true);
    }
    await (await sidebarLink(page, 'settings')).click();
    await expect(page.getByRole('button', { name: 'log out', exact: true })).toBeVisible();
    // The rules are their own settings section: the sidebar links it on a wide screen, the section strip on a phone.
    await page.getByRole('link', { name: 'automatic publishing', exact: true }).click();
    // Settings keeps one page title, its own name; the chosen section's name heads its column below it.
    await expect(page.getByRole('heading', { level: 1, name: 'settings', exact: true })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'automatic publishing', exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'add a folder or repository', exact: true }).click();
    const rule = page.getByRole('form', { name: 'add a folder or repository', exact: true });
    await expect(rule.getByText('saving installs no hook.', { exact: false })).toBeVisible();
    await expect(rule.getByRole('button', { name: 'save rule', exact: true })).toBeDisabled();
    await rule.getByRole('button', { name: 'cancel', exact: true }).click();
    await expect(page.getByRole('form', { name: 'add a folder or repository', exact: true })).toHaveCount(0);
    if (row.mode === 'settings') return;
    const [alice, bob] = ['alice', 'bob'].map(name => exported.fixtures.Accounts.find(account => account.Name === name));
    expect(alice, 'the exported fixture names the previous account').toBeTruthy();
    expect(bob, 'the exported fixture names the selected account').toBeTruthy();
    const before = await saved(request);
    // The saved projection, not the page, says which account the server selected first.
    expect(before.scope.userId).toBe(alice.UserID);
    // log out sits in the account section, the one settings opens on.
    await page.getByRole('link', { name: 'account', exact: true }).click();
    await page.getByRole('button', { name: 'log out', exact: true }).click();
    await expect.poll(async () => (await request.get(local)).status()).toBe(401);
    await control(request, 'account', { name: 'bob' });
    await open(page, theme);
    // The server selects the projection from the credential it saved, and the page names that
    // account by its handle. Both must be the newly selected account, and the history shown
    // must be its saved history: nothing of the logged-out account may remain.
    const after = await saved(request);
    expect(after.scope.userId).toBe(bob.UserID);
    expect(after.scope.userId).not.toBe(before.scope.userId);
    const observations = mountedPage(page).locator('.vobs');
    await expect(observations).toContainText(`@${bob.Username}`);
    await expect(observations).toContainText(exported.fixtures.OldName);
    await expect(observations).not.toContainText(`@${alice.Username}`);
    await expect(observations).not.toContainText(before.scope.userId);
    await expect(observations).toContainText(after.fetch.history.lastCompletedAt);
    return;
  }
  if (row.mode === 'offline') {
    await control(request, 'scenario', { name: 'partial-502' });
    await page.getByRole('button', { name: 'fetch metadata', exact: true }).click();
    await expect(mountedPage(page).locator('.vobs-status')).toContainText('partial');
    await expect(page.getByText("peasant isn't running on this computer.", { exact: false })).toHaveCount(0);
    await control(request, 'stop-app');
    await expect(page.getByText(/peasant.*isn't running on this computer/i).first()).toBeVisible({ timeout: 15000 });
    await control(request, 'start-app');
    return;
  }
  if (row.mode === 'pull') {
    await control(request, 'scenario', { name: 'association-unavailable' });
    const result = await control(request, 'pull');
    expect(result.associationMetadata?.state).toMatch(/unknown|stale/);
    expect(result.associationMetadata?.warning).toBeTruthy();
    expect(result.servedBlobHash).toBe(exported.metadata.contentHash);
    expect(result.status).not.toMatch(/error|failed/);
    const calls = await control(request, 'requests');
    expect(calls.some(c => c.path.endsWith('/content'))).toBe(true);
    expect(calls.some(c => c.path.endsWith('/collectives') && c.path.includes('/transcripts/'))).toBe(true);
    return;
  }
  if (row.mode === 'styles') {
    await page.evaluate(() => document.fonts.ready);
    const styles = await mountedPage(page).locator('.vobs').evaluate(element => ({
      font: getComputedStyle(element.querySelector('p')).fontFamily,
      size: Number.parseFloat(getComputedStyle(element.querySelector('p')).fontSize),
      radius: getComputedStyle(element.querySelector('button')).borderRadius,
      tabular: getComputedStyle(element.querySelector('.tnum')).fontVariantNumeric,
      fontLoaded: [...document.fonts].some(face => /Atkinson/i.test(face.family) && face.status === 'loaded'),
      overflow: document.documentElement.scrollWidth > innerWidth,
      panelPosition: getComputedStyle(element.querySelector('.tabpanel:not([hidden])')).position,
    }));
    expect(styles.font).toMatch(/Atkinson/i); expect(styles.fontLoaded).toBe(true); expect(styles.size).toBeGreaterThanOrEqual(16); expect(styles.radius).toBe('0px'); expect(styles.overflow).toBe(false); expect(styles.tabular).toContain('tabular-nums');
    expect(styles.panelPosition).toBe('relative');
    // The sidebar is a column on a wide screen and a top bar with a drawer on a phone. The closed
    // drawer keeps its layout off screen and hides with visibility, so only visible controls count.
    // Settle that drawer again at this read: a fresh already-dark page never took the open and
    // close path in `open`, and the drawer's delayed visibility flip is the only state the read
    // below depends on.
    if (surface === 'mobile') await closeDrawer(page);
    await expect(page.locator(surface === 'desktop' ? '.asb-panel' : '.asb-bar')).toBeVisible();
    // Read the controls and the drawer state in one task, so the geometry that is asserted is
    // the geometry of a snapshot whose closed drawer had already left the focus order.
    const { drawerClosed, chrome } = await page.locator('.asb').first().evaluate(root => {
      const panel = root.querySelector('.asb-panel');
      const chrome = [...root.querySelectorAll('a,button')].filter(element => element.getClientRects().length > 0 && getComputedStyle(element).visibility !== 'hidden').map(element => {
        const box = element.getBoundingClientRect();
        return { name: element.getAttribute('aria-label') ?? element.textContent, left: box.left, right: box.right, width: box.width, height: box.height };
      });
      const drawerClosed = !panel || [...panel.querySelectorAll('a,button')].every(control => getComputedStyle(control).visibility === 'hidden');
      return { drawerClosed, chrome };
    });
    if (surface === 'mobile') expect(drawerClosed, 'the measured shell must be read after the phone drawer closed').toBe(true);
    for (const control of chrome) {
      expect(control.left, control.name).toBeGreaterThanOrEqual(0);
      expect(control.right, control.name).toBeLessThanOrEqual(page.viewportSize().width);
      expect(control.width, control.name).toBeGreaterThanOrEqual(24);
      expect(control.height, control.name).toBeGreaterThanOrEqual(24);
    }
    await page.keyboard.press('Tab');
    const focused = await page.evaluate(() => document.activeElement?.tagName);
    expect(focused).not.toBe('BODY');
    await page.getByRole('button', { name: 'fetch metadata', exact: true }).focus();
    const focus = await page.getByRole('button', { name: 'fetch metadata', exact: true }).evaluate(element => ({ style: getComputedStyle(element).outlineStyle, width: parseFloat(getComputedStyle(element).outlineWidth) }));
    expect(focus.style).not.toBe('none'); expect(focus.width).toBeGreaterThan(0);
    const axe = await new AxeBuilder({ page }).analyze(); expect(axe.violations).toEqual([]);
    const manifest = JSON.parse(readFileSync(join(root, 'build-manifest.json'), 'utf8'));
    expect(manifest.served.length).toBeGreaterThan(0); expect(manifest.packages.every(p => p.integrity.startsWith('sha512-'))).toBe(true);
    await captureShell(page, join(root, `${theme}-${surface}.png`));
    writeFileSync(join(root, `${theme}-${surface}-computed.json`), JSON.stringify({ styles, axe: { violations: axe.violations } }, null, 2));
  }
}

if (!mutationMode) {
  for (const row of f.cases) test(row.name, ({ page, request }, info) => exercise(row, { page, request }, info));
} else {
  const mutations = parse(readFileSync(new URL('./testdata/collective-mutations.yaml', import.meta.url), 'utf8'));
  expect(new Set(mutations.cases.map(row => row.name))).toEqual(new Set(mutations.requiredNames));
  for (const mutation of mutations.cases) test(`rejects ${mutation.name}`, async ({ page, request }, info) => {
    const row = f.cases.find(candidate => candidate.name === mutation.testName);
    if (!row) throw new Error(`Mutation ${mutation.name} names no production journey`);
    const changes = [];
    // Counterfactual SUT mutation ONLY: never intercept Local API or Village data.
    // Published package/server provenance remains the original, verified build.
    await page.route('**/_next/static/chunks/**/*.js', async route => {
      const response = await route.fetch();
      const original = await response.text();
      const changed = route.request().url().includes(mutation.asset) ? mutateCompiledAsset(original, mutation) : null;
      if (changed == null) {
        await route.fulfill({ response }); return;
      }
      const sha = text => createHash('sha256').update(text).digest('hex');
      changes.push({ url: route.request().url(), originalHash: sha(original), mutatedHash: sha(changed) });
      await route.fulfill({ response, body: changed });
    });
    let failure;
    try { await exercise(row, { page, request }, info); } catch (error) { failure = error; }
    expect(changes.length, 'the actual compiled application mutation must execute').toBeGreaterThan(0);
    expect(failure, 'the same mounted user action must reject the mutant').toBeTruthy();
    expect(failure.message).toContain(mutation.expectedError);
    writeFileSync(join(root, `mutation-${mutation.name}.json`), JSON.stringify({ counterfactual: true, mutation, changes, expectedFailure: failure.message }, null, 2));
  });
}
