import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, realpathSync, existsSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { readFileSync } from 'node:fs';
import { parse } from 'yaml';
import { assertSlugSafeRoot, encodeProjectDirectory, fillerRepositories, seedRecordings, sessionLines } from './lib/kickstart-seed.mjs';
import { verifyKickstartServed } from './lib/kickstart-provenance.mjs';

const f = parse(readFileSync(new URL('./testdata/kickstart-support.yaml', import.meta.url), 'utf8'));
assert.deepEqual(new Set(f.cases.map((c) => c.name)), new Set(f.requiredNames));
assert.equal(new Set(f.cases.map((c) => c.name)).size, f.cases.length);
const template = f.template.lines.join('\n');
const input = { sessionId: 'sess-a', cwd: '/work/ledger', branch: 'main', prompts: ['first prompt', 'second prompt'], startedAt: '2026-02-03T09:00:00Z' };

for (const row of f.cases) {
  if (['encode', 'lines', 'reproducible', 'unique', 'prompts', 'filler', 'checkouts', 'slug-root'].includes(row.mode)) {
    test(row.name, (t) => {
      if (row.mode === 'encode') return assert.equal(encodeProjectDirectory(row.cwd), row.expected);
      if (row.mode === 'slug-root') {
        assert.doesNotThrow(() => assertSlugSafeRoot('/tmp/ksjourneyAb12/dark'));
        for (const root of ['/work/peasant-893--feat/web/.kickstart-journey-owner/state', '/tmp/a.b', '/tmp/a_b', 'relative/path', '/tmp//x', '/']) {
          assert.throws(() => assertSlugSafeRoot(root), /Claude Code's project names flatten/, root);
        }
        return;
      }
      if (row.mode === 'checkouts') {
        const dir = mkdtempSync('/tmp/ksseed');
        t.after(() => rmSync(dir, { recursive: true, force: true }));
        mkdirSync(join(dir, 'home'));
        writeFileSync(join(dir, 'template.jsonl'), `${f.template.lines.join('\n')}\n`);
        const written = seedRecordings({ home: join(dir, 'home'), root: dir, repoRoot: dir, fixture: f.fixture });
        assert.equal(written.length, 3);
        for (const entry of written) {
          const head = execFileSync('git', ['-C', entry.cwd, 'rev-parse', '--abbrev-ref', 'HEAD']).toString().trim();
          assert.equal(head, entry.branch, `${entry.name} was recorded in a checkout of ${entry.branch}`);
          assert.ok(existsSync(entry.path) && entry.path.includes(encodeProjectDirectory(entry.cwd)), 'the transcript sits in its checkout\'s project directory');
          const remote = execFileSync('git', ['-C', entry.cwd, 'remote', 'get-url', 'origin']).toString().trim();
          assert.equal(remote, 'https://github.com/acme/ledger.git');
        }
        assert.equal(new Set(written.map((entry) => entry.cwd)).size, 2, 'one checkout per branch');
        return;
      }
      if (row.mode === 'prompts') return assert.throws(() => sessionLines(template, { ...input, prompts: ['1', '2', '3'] }), /user lines/);
      if (row.mode === 'filler') {
        const spec = { repositories: 2, sessions: 4 };
        const a = fillerRepositories(spec);
        assert.deepEqual(a, fillerRepositories(spec));
        assert.equal(a.length, 2);
        assert.ok(a.every((repo) => repo.sessions.length === 4 && repo.remote.startsWith('https://')));
        return;
      }
      const lines = sessionLines(template, input).map((line) => JSON.parse(line));
      if (row.mode === 'reproducible') return assert.deepEqual(sessionLines(template, input), sessionLines(template, input));
      if (row.mode === 'unique') {
        const other = sessionLines(template, { ...input, sessionId: 'sess-b' }).map((line) => JSON.parse(line));
        const ids = new Set([...lines, ...other].map((line) => line.uuid));
        return assert.equal(ids.size, lines.length + other.length);
      }
      assert.ok(lines.every((line) => line.sessionId === 'sess-a' && line.cwd === '/work/ledger' && line.gitBranch === 'main'));
      assert.deepEqual(lines.filter((line) => line.type === 'user').map((line) => line.message.content), ['first prompt', 'second prompt']);
      assert.ok(lines.every((line) => line.uuid !== '11111111-1111-4111-8111-111111111111'));
      assert.equal(lines[1].parentUuid, lines[0].uuid, 'a reply still points at the line it answers');
      assert.deepEqual(lines.map((line) => line.timestamp), ['2026-02-03T09:00:00.000Z', '2026-02-03T09:00:01.000Z', '2026-02-03T09:00:02.000Z']);
    });
    continue;
  }
  test(row.name, { timeout: 8000 }, async (t) => {
    const root = mkdtempSync(join(tmpdir(), 'peasant-kickstart-provenance-'));
    t.after(() => rmSync(root, { recursive: true, force: true }));
    mkdirSync(join(root, '_next'));
    mkdirSync(join(root, 'kickstart'));
    const binary = join(root, 'peasant');
    writeFileSync(binary, 'the binary');
    const html = `<html><link href="/_next/app.css" rel="stylesheet"/><script src="/_next/app.js"></script>set up peasant</html>`;
    const asset = 'ks-wizard choose sessions';
    writeFileSync(join(root, '_next/app.js'), asset);
    writeFileSync(join(root, '_next/app.css'), '.ks-wizard{}');
    if (row.mode !== 'no-export') writeFileSync(join(root, 'kickstart/index.html'), html);
    const server = createServer((req, res) => {
      if (req.url === '/kickstart/') return res.end(row.mode === 'stale-page' ? 'an older page' : html);
      if (req.url === '/_next/app.js') return res.end(row.mode === 'stale-asset' ? 'an older bundle' : asset);
      if (req.url === '/_next/app.css') return res.end('.ks-wizard{}');
      res.statusCode = 404; res.end('no');
    });
    t.after(async () => { server.closeAllConnections(); await new Promise((done) => server.close(done)); });
    await new Promise((done) => server.listen(0, '127.0.0.1', done));
    const options = {
      origin: `http://127.0.0.1:${server.address().port}`, out: root, binary, pid: process.pid, exe: realpathSync(binary),
      routes: ['/kickstart/'], markers: row.mode === 'missing-marker' ? ['not-in-the-bundle'] : ['ks-wizard', 'choose sessions'],
      baseHasMarker: (marker) => row.mode === 'base-has-marker' && marker === 'ks-wizard',
    };
    if (row.mode === 'no-markers') options.markers = [];
    if (row.mode === 'wrong-binary') options.exe = process.execPath;
    const check = verifyKickstartServed(options);
    if (row.mode === 'exact') {
      const result = await check;
      assert.equal(Object.keys(result.pages).length, 1);
      assert.deepEqual(Object.keys(result.assets).sort(), ['/_next/app.css', '/_next/app.js']);
      assert.deepEqual(result.markers, { 'ks-wizard': true, 'choose sessions': true });
    } else {
      await assert.rejects(check, new RegExp(row.error));
    }
  });
}
