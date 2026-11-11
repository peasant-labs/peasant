import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * Seeds a throwaway computer with agent recordings for the setup journey.
 *
 * The recordings are real Claude Code transcripts in the layout the discovery
 * scan reads (`$HOME/.claude/projects/<encoded cwd>/<session id>.jsonl`). Each
 * is built from the ingest end-to-end fixture's own root session, so the lines
 * are ones the production parser already accepts; only the session id, working
 * directory, branch, timestamps, line ids and the two prompts differ. Each
 * working directory is a real Git repository with a remote and the branches
 * the sessions name, so project and branch grouping comes from Git, not from a
 * string in the transcript.
 */

/** Claude Code names a project directory after its working directory, every non-alphanumeric byte as `-`. */
export function encodeProjectDirectory(cwd) {
  return cwd.replace(/[^a-zA-Z0-9]/g, '-');
}

const uuidFrom = (...parts) => {
  const hex = createHash('sha256').update(parts.join('\u0000')).digest('hex');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-4${hex.slice(13, 16)}-8${hex.slice(17, 20)}-${hex.slice(20, 32)}`;
};

/**
 * The transcript lines of one session, derived from the fixture template.
 * Pure: the same inputs give the same bytes, so a seeded computer is reproducible.
 */
export function sessionLines(template, { sessionId, cwd, branch, prompts, startedAt }) {
  const rows = template.trim().split('\n').map((line) => JSON.parse(line));
  const userRows = rows.filter((row) => row.type === 'user');
  if (userRows.length < prompts.length) throw new Error(`the fixture template has ${userRows.length} user lines; the session names ${prompts.length} prompts`);
  const ids = new Map();
  const fresh = (original) => {
    if (typeof original !== 'string') return original;
    if (!ids.has(original)) ids.set(original, uuidFrom(sessionId, original));
    return ids.get(original);
  };
  const start = Date.parse(startedAt);
  if (Number.isNaN(start)) throw new Error(`startedAt ${JSON.stringify(startedAt)} is not a timestamp`);
  let userIndex = 0;
  return rows.map((row, index) => {
    const next = { ...row, sessionId, cwd, gitBranch: branch, uuid: fresh(row.uuid), timestamp: new Date(start + index * 1000).toISOString() };
    if (row.parentUuid !== undefined) next.parentUuid = fresh(row.parentUuid);
    if (row.type === 'user' && userIndex < prompts.length) {
      next.message = { ...row.message, content: prompts[userIndex] };
      userIndex += 1;
    }
    return JSON.stringify(next);
  });
}

/**
 * Extra repositories of generated sessions, so a scan of the library takes
 * long enough to show its progress. They are never selected on the journey.
 * Pure: the same spec gives the same repositories.
 */
export function fillerRepositories({ repositories, sessions }) {
  return Array.from({ length: repositories }, (_, r) => ({
    name: `filler-${r + 1}`,
    remote: `https://github.com/acme/filler-${r + 1}.git`,
    sessions: Array.from({ length: sessions }, (_, i) => ({
      name: `session-${i + 1}`,
      branch: i % 3 === 0 ? `feature/filler-${(i % 5) + 1}` : 'main',
      startedAt: '2026-03-01T09:00:00Z',
      prompts: [`Filler task ${r + 1}.${i + 1}`, 'Continue.'],
    })),
  }));
}

function git(cwd, ...args) {
  return execFileSync('git', args, {
    cwd,
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_SYSTEM: '/dev/null', GIT_AUTHOR_NAME: 'journey', GIT_AUTHOR_EMAIL: 'journey@example.invalid', GIT_COMMITTER_NAME: 'journey', GIT_COMMITTER_EMAIL: 'journey@example.invalid' },
  }).toString();
}

/**
 * Write the fixture's repositories and recordings under `home` and `root`.
 * Returns what a journey needs to know: where each session was written and the
 * counts it should expect.
 */
export function seedRecordings({ home, root, repoRoot, fixture }) {
  const template = readFileSync(join(repoRoot, fixture.template), 'utf8');
  const projects = join(home, '.claude', 'projects');
  mkdirSync(projects, { recursive: true });
  const written = [];
  const repositories = [...fixture.repositories, ...(fixture.filler ? fillerRepositories(fixture.filler) : [])];
  for (const repo of repositories) {
    const cwd = join(root, 'repos', repo.name);
    mkdirSync(cwd, { recursive: true });
    git(cwd, 'init', '--quiet', '--initial-branch', 'main');
    git(cwd, 'commit', '--quiet', '--allow-empty', '-m', 'seed');
    git(cwd, 'remote', 'add', 'origin', repo.remote);
    for (const branch of new Set(repo.sessions.map((session) => session.branch))) {
      if (branch !== 'main') git(cwd, 'branch', branch);
    }
    const directory = join(projects, encodeProjectDirectory(cwd));
    mkdirSync(directory, { recursive: true });
    for (const session of repo.sessions) {
      const sessionId = uuidFrom(fixture.template, repo.name, session.name);
      const lines = sessionLines(template, { sessionId, cwd, branch: session.branch, prompts: session.prompts, startedAt: session.startedAt });
      const path = join(directory, `${sessionId}.jsonl`);
      writeFileSync(path, `${lines.join('\n')}\n`, { mode: 0o600 });
      written.push({ repository: repo.name, remote: repo.remote, branch: session.branch, name: session.name, sessionId, path, prompt: session.prompts[0] });
    }
  }
  return written;
}
