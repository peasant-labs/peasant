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
    expect(within(panel).getByText('Your saved selection hides all projects.')).toBeInTheDocument();
    const body = panel.querySelector('.cx-teach-body');
    expect(
      Array.from(body?.firstElementChild?.children ?? []).map((line) => line.textContent),
    ).toEqual([
      'Peasant hides 2 projects and 5 sessions.',
      'The data stays ingested and indexed.',
      'The web viewer does not list it.',
      'It is not available for a future push.',
      'Peasant did not delete data.',
      'To change the selection, run peasant kickstart.',
    ]);
    expect(
      Array.from(panel.querySelectorAll('.tabular-nums')).map((count) => count.textContent),
    ).toEqual(['2 projects', '5 sessions']);
    for (const identity of fixture.forbiddenIdentities) {
      expect(panel.textContent).not.toContain(identity);
    }

    fireEvent.click(within(panel).getByRole('button', { name: 'copy command to clipboard' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('peasant kickstart'));
  });
});
