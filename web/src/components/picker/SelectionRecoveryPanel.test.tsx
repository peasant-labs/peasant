import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import { DOCS } from '@/lib/docs';
import {
  ProjectListState,
  SelectionRecoveryPanel,
  projectListState,
} from './SelectionRecoveryPanel';
import {
  PROJECT_VIEWER_STATE_FIXTURES,
  projectViewerStateFixture,
} from './projectViewerStateFixtures';

const writeText = vi.fn<() => Promise<void>>();

describe('project list state', () => {
  for (const testCase of PROJECT_VIEWER_STATE_FIXTURES) {
    it(testCase.name, () => {
      expect(projectListState(testCase.summary)).toBe(testCase.expectedState);
    });
  }
});

describe('SelectionRecoveryPanel', () => {
  beforeEach(() => {
    writeText.mockReset();
    writeText.mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
  });

  afterEach(() => cleanup());

  it('shows the accepted counts-only recovery copy and links to the docs to change the selection', async () => {
    const fixture = projectViewerStateFixture('all hidden by saved selection');
    expect(fixture.expectedState).toBe(ProjectListState.SelectionRecovery);
    render(<SelectionRecoveryPanel {...fixture.summary.selection} />);

    const panel = screen.getByRole('status', { name: 'project selection recovery' });
    const exactVisibleText = (text: string) =>
      within(panel).getByText(
        (_content, element) => element?.textContent?.replace(/\s+/g, ' ').trim() === text,
      );
    expect(
      within(panel).getByRole('heading', {
        name: 'your saved selection hides all projects.',
      }),
    ).toBeVisible();
    expect(exactVisibleText('peasant hides 2 projects and 5 sessions.')).toBeVisible();
    expect(within(panel).getByText('2 projects')).toBeVisible();
    expect(within(panel).getByText('5 sessions')).toBeVisible();
    expect(within(panel).getByText('the data stays ingested and indexed.')).toBeVisible();
    expect(within(panel).getByText('the web viewer does not list it.')).toBeVisible();
    expect(within(panel).getByText('it is not available for a future push.')).toBeVisible();
    expect(within(panel).getByText('peasant did not delete data.')).toBeVisible();
    for (const identity of fixture.forbiddenIdentities) {
      expect(panel.textContent).not.toContain(identity);
    }

    // How to change the selection lives in the docs; the panel teaches no command.
    const docs = within(panel).getByRole('link', { name: /^change the selection/ });
    expect(docs).toHaveAttribute('href', DOCS.selection);
    expect(docs).toHaveAttribute('target', '_blank');
    expect(panel.textContent).not.toContain('peasant kickstart');
    expect(within(panel).queryByRole('button', { name: 'copy command to clipboard' })).toBeNull();
  });
});
