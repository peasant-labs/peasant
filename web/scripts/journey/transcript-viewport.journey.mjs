import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { parse } from 'yaml';
import { assertServedBuild } from '../visual/served-build.mjs';
const fixture = parse(readFileSync(new URL('./testdata/transcript-viewport.yaml', import.meta.url), 'utf8'));

test('mounted long transcript stays bounded while its stream scrolls', async ({ page }, info) => {
  await assertServedBuild({ origin: process.env.PEASANT_ORIGIN, markers: ['--app-notice-height', 'asb-layout-viewport'], bin: process.env.PEASANT_BIN });
  await page.addInitScript(theme => localStorage.setItem('peasant-theme', theme), info.project.metadata.theme);
  let turnCount = 0;
  page.on('websocket', socket => socket.on('framereceived', event => {
    try { const frame = JSON.parse(event.payload); if (frame.type === 'session_detail' && frame.data?.id === fixture.session) turnCount = frame.data.turns.length; } catch { /* Other frames are irrelevant. */ }
  }));
  await page.goto(`/projects/${fixture.project}/${fixture.session}/`);
  await expect(page.locator('html')).toHaveAttribute('data-theme', info.project.metadata.theme);
  await expect(page.locator('.txn-stream')).toBeVisible();
  await expect(page.locator('.txn-footer')).toBeVisible();
  await expect.poll(() => turnCount).toBe(52);
  await expect(page.locator('.asb')).toBeVisible();
  await expect(page.locator('.txn-app')).toBeVisible();
  await expect(page.locator('.txn-app')).toContainText('52');
  const sample = () => page.evaluate(() => {
    const layout = document.querySelector('.asb-layout');
    const stream = document.querySelector('.txn-stream');
    const footer = document.querySelector('.txn-footer');
    const rect = footer.getBoundingClientRect();
    return { layoutHeight: layout.getBoundingClientRect().height, footerTop: rect.top, footerBottom: rect.bottom,
      documentHeight: document.documentElement.scrollHeight, pageY: window.scrollY,
      streamTop: stream.scrollTop, streamHeight: stream.scrollHeight, streamClient: stream.clientHeight,
      overflow: getComputedStyle(stream).overflowY, viewport: innerHeight };
  });
  const initial = await sample();
  expect(initial.layoutHeight).toBeLessThanOrEqual(initial.viewport + 1);
  expect(initial.footerTop).toBeGreaterThanOrEqual(0);
  expect(initial.footerBottom).toBeLessThanOrEqual(initial.viewport + 1);
  expect(initial.documentHeight).toBeLessThanOrEqual(initial.viewport + 1);
  expect(initial.streamHeight).toBeGreaterThan(initial.streamClient);
  expect(['auto', 'scroll']).toContain(initial.overflow);
  await page.locator('.txn-stream').hover();
  await page.mouse.wheel(0, 600);
  await expect.poll(async () => (await sample()).streamTop).toBeGreaterThan(initial.streamTop);
  const after = await sample();
  expect(after.pageY).toBe(initial.pageY);
  expect(after.footerBottom).toBeLessThanOrEqual(after.viewport + 1);
  expect(after.footerTop).toBeGreaterThanOrEqual(0);
  if (process.env.TRANSCRIPT_CAPTURE_DIR) await page.screenshot({ path: `${process.env.TRANSCRIPT_CAPTURE_DIR}/clean-${info.project.name}.png` });
  // Model the pre-fix hydrated DOM, not a separate layout implementation.
  await page.locator('.asb-layout').evaluate(element => element.classList.remove('asb-layout-viewport'));
  await expect.poll(async () => (await sample()).footerBottom).toBeGreaterThan(initial.viewport + 1);
});

test('recorded pull request opens its original command and result', async ({ page }) => {
  const source = fixture.source;
  await page.routeWebSocket('**/ws', ws => {
    const server = ws.connectToServer();
    server.onMessage(message => {
      let frame;
      try { frame = JSON.parse(message); } catch { ws.send(message); return; }
      {
        if (frame.type === 'session_detail' && frame.data?.id === fixture.session) {
          const turn = frame.data.turns.find(t => t.index === source.turnIndex);
          turn.toolCalls[source.toolCallIndex] = { id: source.toolCallId, name: 'Bash', arguments: JSON.stringify({ command: source.command }), result: source.result };
          message = JSON.stringify(frame);
        }
      }
      ws.send(message);
    });
  });
  await page.route('**/api/v1/sessions/*/pull-requests', route => route.fulfill({ json: {
    pullRequests: [], unverified_pull_requests: [{ owner: 'example-org', repo: 'example-repo', number: 42,
      url: source.result, source: { turnIndex: source.turnIndex, toolCallIndex: source.toolCallIndex, toolCallId: source.toolCallId } }],
  } }));
  await page.goto(`/projects/${fixture.project}/${fixture.session}/?scope=task&scopeVal=0`);
  const evidence = page.getByRole('region', { name: 'unverified pull requests' });
  await expect(evidence).toBeVisible();
  await evidence.getByRole('button', { name: 'view source' }).click();
  await expect.poll(() => new URL(page.url()).searchParams.has('scope')).toBe(false);
  const turn = page.locator(`#turn-${source.turnIndex}`);
  await expect(turn.getByRole('button', { name: `Bash ${source.command}` })).toHaveAttribute('aria-expanded', 'true');
  await expect(turn.getByText(source.command, { exact: true }).last()).toBeVisible();
  await expect(turn.getByLabel('command output')).toHaveText(source.result);
});

test('canonical compact tools preserve reading width and visible keyboard tabs', async ({ page }, info) => {
  await assertServedBuild({ origin: process.env.PEASANT_ORIGIN, markers: ['txn-compact', '--app-notice-height'], bin: process.env.PEASANT_BIN });
  await page.addInitScript(theme => localStorage.setItem('peasant-theme', theme), info.project.metadata.theme);
  await page.goto(`/projects/${fixture.project}/${fixture.session}/`);
  const viewer = page.locator('.txn-app');
  await expect(viewer).toBeVisible();
  await expect.poll(() => viewer.evaluate(element => element.classList.contains('txn-compact'))).toBe(info.project.metadata.compact);
  const prose = viewer.locator('.txn-body:visible').first();
  await expect(prose).toBeVisible();
  const proseWidth = () => prose.evaluate(element => element.getBoundingClientRect().width);
  const before = await proseWidth();
  expect(before).toBeGreaterThanOrEqual(info.project.metadata.minimumProse);
  const grid = viewer.locator('.txn-body-grid');
  const layout = await grid.evaluate(element => ({ left: element.dataset.leftRail, right: element.dataset.rightRail }));
  const tools = viewer.getByRole('button', { name: 'transcript tools', exact: true });
  await tools.click();
  await viewer.locator('.txn-mobile-controls').getByRole('button', { name: 'filters', exact: true }).click();
  const filters = page.getByRole('dialog', { name: 'filters', exact: true });
  await expect(filters).toBeVisible();
  const responses = filters.getByRole('checkbox', { name: /^responses/ });
  await expect(responses).toBeChecked();
  await responses.uncheck();
  await expect(responses).not.toBeChecked();
  expect(await proseWidth(), 'opening native filters does not squeeze the local transcript').toBe(before);
  await page.keyboard.press('Escape');
  await expect(filters).not.toBeVisible();
  await expect(tools).toBeFocused();
  await tools.click();
  await viewer.locator('.txn-mobile-controls').getByRole('button', { name: 'user turns', exact: true }).click();
  const outline = page.getByRole('dialog', { name: 'user turns', exact: true });
  await expect(outline).toBeVisible();
  await outline.getByRole('button', { name: 'close drawer', exact: true }).click();
  await expect(tools).toBeFocused();
  expect(await grid.evaluate(element => ({ left: element.dataset.leftRail, right: element.dataset.rightRail }))).toEqual(layout);
  const tabs = viewer.getByRole('tablist', { name: 'session views' });
  await tabs.getByRole('tab').first().evaluate(element => element.focus({ preventScroll: true }));
  const pagePosition = await page.evaluate(() => ({ x: scrollX, y: scrollY }));
  await page.keyboard.press('End');
  await expect(tabs.getByRole('tab', { name: /^annotations/ })).toBeFocused();
  expect(await tabs.evaluate(strip => {
    const tab = document.activeElement, box = tab.getBoundingClientRect(), clip = strip.getBoundingClientRect();
    const style = getComputedStyle(tab), extent = Math.max(0, parseFloat(style.outlineOffset) + parseFloat(style.outlineWidth));
    return box.left - extent >= clip.left - 1 && box.right + extent <= clip.right + 1 && box.top - extent >= clip.top - 1 && box.bottom + extent <= clip.bottom + 1;
  }), 'focused tab and its ring remain visible inside the local footer').toBe(true);
  await page.keyboard.press('Home');
  await expect(tabs.getByRole('tab', { name: /^highlights/ })).toBeFocused();
  expect(await page.evaluate(() => ({ x: scrollX, y: scrollY }))).toEqual(pagePosition);
  expect(await viewer.evaluate(element => element.scrollWidth - element.clientWidth)).toBe(0);
});
