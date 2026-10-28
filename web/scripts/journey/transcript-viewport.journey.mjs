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
