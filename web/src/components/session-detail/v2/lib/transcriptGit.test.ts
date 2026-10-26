import { describe, expect, it } from 'vitest';
import { toTranscriptGitContext } from './transcriptGit';

// The viewer's adapter decides where a commit checkpoint lands from its
// `commitTime`: a present time anchors the commit to a turn, an absent one
// leaves it unattributed. The store reads SQL NULL back as 0, so an unrecorded
// time must reach the adapter as ABSENT, not as an instant.

const DATED = {
  hash: '9f3c1ad0aa11',
  message: 'feat(canvas): port TurnRow',
  authorName: 'Ada Lovelace',
  authorEmail: 'ada@example.test',
  commitTime: BigInt(1752566460000),
  authorTime: BigInt(1752566400000),
};

const UNDATED = {
  hash: '1b2c3d4e5f60',
  message: 'fix(api): keep the refusal reason',
  authorName: 'Ada Lovelace',
  authorEmail: 'ada@example.test',
  commitTime: BigInt(0),
  authorTime: BigInt(0),
};

describe('toTranscriptGitContext', () => {
  it('keeps a recorded millisecond time as the contract bigint', () => {
    const context = toTranscriptGitContext([DATED]);

    expect(context?.commits).toEqual([
      {
        hash: DATED.hash,
        message: DATED.message,
        authorName: DATED.authorName,
        authorEmail: DATED.authorEmail,
        commitTime: DATED.commitTime,
        authorTime: DATED.authorTime,
      },
    ]);
  });

  it('omits the times of a commit whose time was never recorded', () => {
    const context = toTranscriptGitContext([UNDATED]);
    const [row] = context?.commits ?? [];

    // The identifying fields still travel, so the commit is shown; only the
    // unknown instant is withheld.
    expect(row).toMatchObject({
      hash: UNDATED.hash,
      message: UNDATED.message,
      authorName: UNDATED.authorName,
      authorEmail: UNDATED.authorEmail,
    });
    expect(row).not.toHaveProperty('commitTime');
    expect(row).not.toHaveProperty('authorTime');
  });

  it('keeps one dated and one undated row side by side', () => {
    const context = toTranscriptGitContext([DATED, UNDATED]);
    const [dated, undated] = context?.commits ?? [];

    expect(dated).toHaveProperty('commitTime', DATED.commitTime);
    expect(undated).not.toHaveProperty('commitTime');
  });

  it('returns undefined for a session with no commits', () => {
    expect(toTranscriptGitContext([])).toBeUndefined();
  });
});
