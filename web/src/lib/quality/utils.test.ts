import { describe, it, expect } from 'vitest';
import { decodeProjectPath, displayProject, middleTruncatePath } from './utils';

// ---------------------------------------------------------------------------
// displayProject
// ---------------------------------------------------------------------------

describe('displayProject', () => {
  it('returns the last segment of a unix filesystem path', () => {
    expect(displayProject('/Users/sampleuser/Documents/Projects/phaze')).toBe('phaze');
  });

  it('returns the last segment of a windows filesystem path', () => {
    expect(displayProject('C:\\Users\\acme\\Projects\\phaze')).toBe('phaze');
  });

  it('returns the last dash-segment of a host slug', () => {
    expect(displayProject('~Users-acme-dev-Documents-Projects-phaze')).toBe('phaze');
  });

  it('collapses a Claude-encoded path to the project folder', () => {
    expect(
      displayProject('/Users/sampleuser/.claude/projects/-Users-sampleuser-Desktop-widget-demo'),
    ).toBe('widget-demo');
  });

  it('collapses a bare Claude-encoded path segment', () => {
    expect(displayProject('-Users-sampleuser-Desktop-widget-demo')).toBe('widget-demo');
  });

  it('returns the home directory name for a bare home path', () => {
    expect(displayProject('/Users/sampleuser')).toBe('sampleuser');
  });

  it('leaves a plain dash-separated project name unchanged', () => {
    expect(displayProject('sample-project')).toBe('sample-project');
  });

  it('returns a plain name unchanged', () => {
    expect(displayProject('phaze')).toBe('phaze');
  });

  // The server prefers a git-remote-derived
  // "host:owner/repo" display label over a raw path. This label legitimately
  // contains a "/" between owner and repo, so it must pass through unchanged
  // instead of being truncated down to just the repo name by the path logic.
  it('leaves a github remote-derived label unchanged', () => {
    expect(displayProject('github:example-org/garden-app')).toBe('github:example-org/garden-app');
  });

  it('leaves a gitlab remote-derived label unchanged', () => {
    expect(displayProject('gitlab:example-team/sample-project')).toBe(
      'gitlab:example-team/sample-project',
    );
  });

  it('leaves an unrecognized-host remote-derived label unchanged', () => {
    expect(displayProject('git.example.com:acme/widgets')).toBe('git.example.com:acme/widgets');
  });

  it('still collapses a windows path with a drive letter to its last segment', () => {
    // Regression guard for the remote-label detector: "C:\Users\..." must NOT
    // be mistaken for a "host:owner/repo" label just because it has a colon.
    expect(displayProject('C:\\Users\\acme\\Projects\\phaze')).toBe('phaze');
  });

  // Adversarial shapes locking down the remote-label shape-sniff (reviewed:
  // see the DESIGN NOTE above REMOTE_DISPLAY_LABEL in utils.ts for why this
  // sniffs the overloaded `project` field instead of an explicit signal).
  // None of these real-world path/name shapes may be misclassified as an
  // already-formatted "host:owner/repo" label.
  it('does not mistake a Windows UNC path for a remote label', () => {
    expect(displayProject('\\\\server\\share\\Projects\\phaze')).toBe('phaze');
  });

  it('does not mistake a single-letter-drive path with a trailing colon-only segment for a remote label', () => {
    // "C:\phaze" has a colon but the segment after it is backslash-separated,
    // never a forward slash — must still resolve as a Windows path, not a label.
    expect(displayProject('C:\\phaze')).toBe('phaze');
  });

  it('does not mistake a forward-slash Windows drive path for a remote label', () => {
    // "C:/Users/acme/Projects/phaze" — the forward-slash form of a Windows
    // path (e.g. from a tool that normalizes backslashes) has BOTH a colon
    // and a slash, the exact shape REMOTE_DISPLAY_LABEL looks for. It is
    // safe today only because the char class between the colon and the
    // required slash excludes "/" itself (a naive regex that dropped that
    // exclusion would misclassify this as a remote label and return it
    // unformatted instead of "phaze", which this regression case locks in).
    expect(displayProject('C:/Users/acme/Projects/phaze')).toBe('phaze');
  });

  it('does not mistake a host-only string with a port and no path for a remote label', () => {
    // "docker:5000" has a colon but no "/" at all — REMOTE_DISPLAY_LABEL
    // requires a slash after the colon-prefixed segment, so this falls
    // through to the plain-name passthrough, not the remote-label branch.
    expect(displayProject('docker:5000')).toBe('docker:5000');
  });

  it('does not mistake a bare 64-character project hash fallback for a remote label', () => {
    const hash = 'a'.repeat(64);
    expect(displayProject(hash)).toBe(hash);
  });

  // KNOWN, ACCEPTED false-positive edge case (documented in the DESIGN NOTE):
  // a LOCAL project name that happens to already look like "token:token/token"
  // is indistinguishable from a real remote-derived label and passes through
  // unchanged instead of being basename-reduced. This is a cosmetic-only
  // collision (no data leak, no crash) — locking in the CURRENT behavior here
  // so a future change to the heuristic is a deliberate decision, not a silent
  // drift.
  it('passes through a coincidentally label-shaped local project name unchanged (known false positive)', () => {
    expect(displayProject('docker:5000/registry')).toBe('docker:5000/registry');
  });
});

// ---------------------------------------------------------------------------
// decodeProjectPath
// ---------------------------------------------------------------------------

describe('decodeProjectPath', () => {
  it('decodes a Claude-encoded path to a real slash path', () => {
    expect(decodeProjectPath('-Users-sampleuser-Desktop-widget-demo')).toBe(
      '/Users/sampleuser/Desktop/widget/demo',
    );
  });

  it('decodes a host slug to a tilde-rooted path', () => {
    expect(decodeProjectPath('~Users-acme-dev-Documents-Projects-phaze')).toBe(
      '~/Users/acme/dev/Documents/Projects/phaze',
    );
  });

  it('passes a real unix path through unchanged', () => {
    expect(decodeProjectPath('/Users/sampleuser/Documents/Projects/peasant')).toBe(
      '/Users/sampleuser/Documents/Projects/peasant',
    );
  });

  it('passes a bare project name through unchanged', () => {
    expect(decodeProjectPath('sample-project')).toBe('sample-project');
  });

  it('returns empty string for empty input', () => {
    expect(decodeProjectPath('')).toBe('');
  });
});

// ---------------------------------------------------------------------------
// middleTruncatePath
// ---------------------------------------------------------------------------

describe('middleTruncatePath', () => {
  it('collapses the home prefix to a tilde', () => {
    expect(middleTruncatePath('/Users/sampleuser/work', 80)).toBe('~/work');
  });

  it('keeps the meaningful tail and drops middle segments', () => {
    const result = middleTruncatePath(
      '/Users/sampleuser/Documents/Projects/peasant',
      24,
    );
    expect(result.startsWith('~')).toBe(true);
    expect(result).toContain('…');
    expect(result.endsWith('peasant')).toBe(true);
  });

  it('returns the path unchanged when within the budget', () => {
    expect(middleTruncatePath('~/Projects/peasant', 80)).toBe('~/Projects/peasant');
  });

  it('decodes-then-truncates: a Claude-encoded path is never truncated as a blob', () => {
    const decoded = decodeProjectPath('-Users-sampleuser-Desktop-deeply-nested-widget-demo');
    const truncated = middleTruncatePath(decoded, 24);
    // The unique tail survives; the home prefix is collapsed.
    expect(truncated.endsWith('demo')).toBe(true);
    expect(truncated).toContain('…');
  });

  it('returns empty string for empty input', () => {
    expect(middleTruncatePath('', 24)).toBe('');
  });
});
