import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
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

  it('shows the accepted counts-only recovery copy and copies the kickstart command', async () => {
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
        name: 'Your saved selection hides all projects.',
      }),
    ).toBeVisible();
    expect(exactVisibleText('Peasant hides 2 projects and 5 sessions.')).toBeVisible();
    expect(within(panel).getByText('2 projects')).toBeVisible();
    expect(within(panel).getByText('5 sessions')).toBeVisible();
    expect(within(panel).getByText('The data stays ingested and indexed.')).toBeVisible();
    expect(within(panel).getByText('The web viewer does not list it.')).toBeVisible();
    expect(within(panel).getByText('It is not available for a future push.')).toBeVisible();
    expect(within(panel).getByText('Peasant did not delete data.')).toBeVisible();
    expect(exactVisibleText('To change the selection, run peasant kickstart.')).toBeVisible();
    for (const identity of fixture.forbiddenIdentities) {
      expect(panel.textContent).not.toContain(identity);
    }

    fireEvent.click(within(panel).getByRole('button', { name: 'copy command to clipboard' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('peasant kickstart'));
  });
});
