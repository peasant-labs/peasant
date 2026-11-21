import { test } from 'node:test';
import assert from 'node:assert/strict';
import { applyDeterminism, applyPlaywrightDeterminism, freezeDocument, FROZEN_EPOCH_MS, PRNG_SEED } from '../visual/determinism.mjs';

/** A page that records the browser calls a determinism helper makes. */
function recordingPage() {
  const calls = [];
  const record = name => async (...args) => { calls.push({ name, args }); };
  return { calls, emulateMedia: record('emulateMedia'), addInitScript: record('addInitScript'), emulateMediaFeatures: record('emulateMediaFeatures'), evaluateOnNewDocument: record('evaluateOnNewDocument') };
}

// The init script is the real function and its values one argument object, so
// the browser library receives data, never program text with values spliced in.
test('playwright pages get the freeze as a function with one argument object', async () => {
  const page = recordingPage();
  await applyPlaywrightDeterminism(page, { epochMs: 1234 });
  assert.deepEqual(page.calls.map(call => call.name), ['emulateMedia', 'addInitScript']);
  assert.deepEqual(page.calls[0].args, [{ reducedMotion: 'reduce' }]);
  const [script, argument, ...rest] = page.calls[1].args;
  assert.equal(script, freezeDocument);
  assert.deepEqual(rest, []);
  assert.deepEqual(argument, { epochMs: 1234, seed: PRNG_SEED });
  assert.deepEqual(JSON.parse(JSON.stringify(argument)), argument);
});

test('puppeteer pages get the same function and argument object', async () => {
  const page = recordingPage();
  await applyDeterminism(page);
  assert.deepEqual(page.calls.map(call => call.name), ['emulateMediaFeatures', 'evaluateOnNewDocument']);
  const [script, argument, ...rest] = page.calls[1].args;
  assert.equal(script, freezeDocument);
  assert.deepEqual(rest, []);
  assert.deepEqual(argument, { epochMs: FROZEN_EPOCH_MS, seed: PRNG_SEED });
});

// freezeDocument runs in the page; here it runs against this process's globals,
// which are restored afterwards.
test('the freeze pins the clock and seeds randomness from its argument object', t => {
  const real = { Date: globalThis.Date, random: Math.random, window: globalThis.window };
  t.after(() => { globalThis.Date = real.Date; Math.random = real.random; globalThis.window = real.window; });
  globalThis.window = globalThis;
  freezeDocument({ epochMs: 4242, seed: 7 });
  assert.equal(Date.now(), 4242);
  assert.equal(new Date().getTime(), 4242);
  assert.equal(new Date(5).getTime(), 5);
  const first = [Math.random(), Math.random(), Math.random()];
  freezeDocument({ epochMs: 4242, seed: 7 });
  assert.deepEqual([Math.random(), Math.random(), Math.random()], first);
  freezeDocument({ epochMs: 4242, seed: 8 });
  assert.notDeepEqual([Math.random(), Math.random(), Math.random()], first);
});
