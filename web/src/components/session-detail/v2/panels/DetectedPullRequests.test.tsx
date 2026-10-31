import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import type { SessionPullRequest } from '@/lib/api/session-extras';
import { DetectedPullRequests } from './DetectedPullRequests';

afterEach(() => cleanup());

const PULL_REQUESTS: SessionPullRequest[] = [
  {
    owner: 'storytold',
    repo: 'photocraft',
    number: 42,
    url: 'https://github.com/storytold/photocraft/pull/42',
    source: 'gh_pr_create',
  },
  {
    owner: 'storytold',
    repo: 'photocraft',
    number: 7,
    url: 'https://github.com/storytold/photocraft/pull/7',
    source: 'gh_pr_create',
  },
];

/** The evidence sentence under the list (the eyebrow is the section's other paragraph). */
function evidenceParagraph(section: HTMLElement): Element {
  const evidence = section.querySelector('p:not(.v2-eyebrow)');
  if (!evidence) throw new Error('the evidence label is missing from the pull request section');
  return evidence;
}

describe('DetectedPullRequests', () => {
  it('names the count, links each pull request to GitHub, and says where the rows came from', () => {
    render(<DetectedPullRequests pullRequests={PULL_REQUESTS} />);

    const section = screen.getByRole('region', { name: /pull requests/i });
    expect(section).toHaveTextContent('pull requests');
    expect(section.querySelector('.tnum')).toHaveTextContent('2');

    const links = screen.getAllByRole('link');
    expect(links).toHaveLength(2);
    expect(links[0]).toHaveTextContent('storytold/photocraft #42');
    expect(links[0]).toHaveAttribute('href', 'https://github.com/storytold/photocraft/pull/42');
    expect(links[1]).toHaveAttribute('href', 'https://github.com/storytold/photocraft/pull/7');
    for (const link of links) {
      expect(link).toHaveAttribute('target', '_blank');
      expect(link).toHaveAttribute('rel', 'noreferrer');
    }

    // The evidence label is visible, and names both the command and the limit of
    // what the page knows: nothing here is verified with GitHub or village.
    const evidence = evidenceParagraph(section);
    expect(evidence).toHaveTextContent('detected from gh pr create; not verified by village.');
    // A title that only repeats the visible sentence adds noise, not information.
    expect(evidence).not.toHaveAttribute('title');

    // Detection is evidence about this transcript, never a binding: the copy
    // must not claim the transcript is linked to the pull request.
    expect(section.textContent?.toLowerCase()).not.toContain('linked');
  });

  it('names an unknown source as reported instead of claiming gh pr create', () => {
    render(<DetectedPullRequests pullRequests={[{ ...PULL_REQUESTS[0], source: 'glab_MR_create' }]} />);

    const section = screen.getByRole('region', { name: /pull requests/i });
    // Verbatim: the page names the source the server reported, it does not
    // normalize it into a casing the server never used.
    expect(evidenceParagraph(section).textContent).toBe('detected from glab_MR_create; not verified by village.');
    expect(section.textContent).not.toContain('gh pr create');
  });

  it('names several distinct sources in one sentence', () => {
    render(
      <DetectedPullRequests
        pullRequests={[PULL_REQUESTS[0], { ...PULL_REQUESTS[1], source: 'glab_MR_create' }]}
      />,
    );

    const section = screen.getByRole('region', { name: /pull requests/i });
    // One sentence for the whole list; the same two rows from one command still
    // name that command once.
    const evidence = evidenceParagraph(section);
    expect(evidence.textContent).toBe('detected from gh pr create, glab_MR_create; not verified by village.');
    expect(evidence.querySelectorAll('.font-mono')).toHaveLength(1);
    expect(section.textContent?.match(/detected from/g)).toHaveLength(1);
  });

  it('renders no evidence sentence when the list is empty', () => {
    // The caller gates on length, so this is a prop-contract guard: an empty
    // list must never render the dangling "detected from ;" sentence.
    render(<DetectedPullRequests pullRequests={[]} />);

    const section = screen.getByRole('region', { name: /pull requests/i });
    expect(section).toHaveTextContent('pull requests');
    expect(section.textContent).not.toContain('detected from');
    expect(screen.queryAllByRole('link')).toHaveLength(0);
    expect(section.querySelector('p:not(.v2-eyebrow)')).toBeNull();
  });
});
