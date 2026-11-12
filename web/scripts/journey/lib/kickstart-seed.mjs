import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, utimesSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * Seeds a throwaway computer with agent recordings for the setup journey.
 *
 * The recordings are real Claude Code transcripts in the layout the discovery
 * scan reads (`$HOME/.claude/projects/<encoded cwd>/<session id>.jsonl`). Each
 * is built from the ingest end-to-end fixture's own root session, so the lines
 * are ones the production parser already accepts; only the session id, working
 * directory, branch, timestamps, line ids and the two prompts differ. Each
 * working directory is a real Git checkout with a remote, one per branch the
 * sessions name, so project and branch grouping comes from Git, not from a
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

/**
 * Claude Code names a project directory after its working directory with every
 * punctuation mark flattened to a dash, so the name does not say which dashes
 * were separators. Discovery recovers the directory by probing the disk, and a
 * root with dots or dashes in it can recover a different, real directory (the
 * enclosing checkout's), which silently gives every seeded project that
 * checkout's remote. The seeded computer therefore lives under a root of
 * letters, digits and slashes only; this refuses any other.
 */
export function assertSlugSafeRoot(root) {
  if (!/^\/[A-Za-z0-9]+(\/[A-Za-z0-9]+)*$/.test(root)) {
    throw new Error(`the seeded computer's root ${JSON.stringify(root)} has a character that Claude Code's project names flatten (anything but letters, digits and "/"); discovery could resolve its projects to another directory. Seed under a plain path such as /tmp/ksjourneyXXXX.`);
  }
}

/** The directory of the checkout for one branch of a repository. */
export function worktreeFor(root, repository, branch) {
  return join(root, 'repos', `${repository}--${branch.replace(/[^a-zA-Z0-9]+/g, '-')}`);
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
  assertSlugSafeRoot(root);
  const template = readFileSync(join(repoRoot, fixture.template), 'utf8');
  const projects = join(home, '.claude', 'projects');
  mkdirSync(projects, { recursive: true });
  const written = [];
  const repositories = [...fixture.repositories, ...(fixture.filler ? fillerRepositories(fixture.filler) : [])];
  for (const repo of repositories) {
    const main = join(root, 'repos', repo.name);
    mkdirSync(main, { recursive: true });
    git(main, 'init', '--quiet', '--initial-branch', 'main');
    git(main, 'commit', '--quiet', '--allow-empty', '-m', 'seed');
    git(main, 'remote', 'add', 'origin', repo.remote);
    // One checkout per branch, as an agent that works in worktrees leaves them:
    // a session was recorded in a directory whose checked-out branch is the
    // branch the transcript names. A session marked `sharedClone` was recorded
    // in the main clone while it sat on another branch than the one it names,
    // which is how an old session looks after the clone moved on.
    const checkouts = new Map([['main', main]]);
    for (const branch of new Set(repo.sessions.filter((session) => !session.sharedClone).map((session) => session.branch))) {
      if (checkouts.has(branch)) continue;
      const path = worktreeFor(root, repo.name, branch);
      git(main, 'worktree', 'add', '--quiet', '-b', branch, path);
      checkouts.set(branch, path);
    }
    for (const session of repo.sessions) {
      const cwd = session.sharedClone ? main : checkouts.get(session.branch);
      const directory = join(projects, encodeProjectDirectory(cwd));
      mkdirSync(directory, { recursive: true });
      const sessionId = uuidFrom(fixture.template, repo.name, session.name);
      const lines = sessionLines(template, { sessionId, cwd, branch: session.branch, prompts: session.prompts, startedAt: session.startedAt });
      const path = join(directory, `${sessionId}.jsonl`);
      writeFileSync(path, `${lines.join('\n')}\n`, { mode: 0o600 });
      // A recording that was last written a moment ago is still being written:
      // the import counts it as active and leaves it out of the new and updated
      // counts the finish panel reports. A recording that is over is as old as
      // its session, so the file says when the session started, not when this
      // computer was seeded.
      const recordedAt = new Date(Date.parse(session.startedAt));
      utimesSync(path, recordedAt, recordedAt);
      written.push({ repository: repo.name, remote: repo.remote, branch: session.branch, name: session.name, sessionId, cwd, path, prompt: session.prompts[0], startedAt: session.startedAt });
    }
  }
  return written;
}
