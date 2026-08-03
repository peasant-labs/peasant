import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { TimeStrip, type TimeStripBranch, type TimeStripDay } from './TimeStrip';

const DAYS: TimeStripDay[] = [
  { date: '2026-06-01', sessions: 0 },
  { date: '2026-06-02', sessions: 1 },
  { date: '2026-06-03', sessions: 4 },
  { date: '2026-06-04', sessions: 8 },
];

const BRANCHES: TimeStripBranch[] = [
  { name: 'feat/graph-cache', aheadCount: 3 },
  { name: 'fix/diff', aheadCount: 1 },
];

describe('TimeStrip', () => {
  it('renders one bar per day on the intensity ramp', () => {
    const { container } = render(<TimeStrip days={DAYS} />);
    expect(
      screen.getByRole('img', { name: 'Session activity: 13 sessions over 4 days' }),
    ).toBeInTheDocument();
    expect(container.querySelectorAll('.bg-intensity-0')).toHaveLength(1); // zero day
    expect(container.querySelectorAll('.bg-intensity-4')).toHaveLength(1); // max day
  });

  it('renders square font-mono branch chips that call back on click', () => {
    const onBranchClick = vi.fn();
    render(
      <TimeStrip
        days={DAYS}
        branches={BRANCHES}
        defaultBranch="develop"
        onBranchClick={onBranchClick}
      />,
    );
    expect(screen.getByText('develop')).toBeInTheDocument();
    const chip = screen.getByRole('button', { name: 'Review branch feat/graph-cache' });
    expect(chip.className).toContain('font-mono');
    expect(chip).toHaveTextContent('feat/graph-cache +3');
    fireEvent.click(chip);
    expect(onBranchClick).toHaveBeenCalledWith('feat/graph-cache');
  });

  it('folds extra branches into a non-interactive overflow chip', () => {
    render(
      <TimeStrip
        days={DAYS}
        branches={[...BRANCHES, { name: 'b3' }, { name: 'b4' }]}
        maxBranchChips={2}
      />,
    );
    expect(screen.getByText('+2')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Review branch b3' })).toBeNull();
  });

  it('is fully static by default · no playhead, no scrub targets', () => {
    render(<TimeStrip days={DAYS} playheadIndex={2} />);
    expect(screen.queryByTestId('timestrip-playhead')).toBeNull();
    expect(screen.queryByRole('button', { name: /Scrub to/ })).toBeNull();
  });

  it('renders the playhead and scrub targets only behind the scrubbable prop', () => {
    const onScrub = vi.fn();
    render(<TimeStrip days={DAYS} scrubbable playheadIndex={2} onScrub={onScrub} />);
    expect(screen.getByTestId('timestrip-playhead')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Scrub to 2026-06-03' }));
    expect(onScrub).toHaveBeenCalledWith(2);
  });

  it('anchors the sparkline right so overflow clips history, never "now"', () => {
    render(<TimeStrip days={DAYS} />);
    const sparkline = screen.getByRole('img', { name: /Session activity/ });
    expect(sparkline.className).toContain('justify-end');
    expect(sparkline.className).toContain('overflow-hidden');
  });

  it('positions the playhead from the right edge, consistent with the anchor', () => {
    const { rerender } = render(<TimeStrip days={DAYS} scrubbable playheadIndex={2} />);
    // 5px bar + 2px gap = 7px pitch; index 2 of 4 sits one bar in from "now".
    expect(screen.getByTestId('timestrip-playhead').style.right).toBe('9px');

    rerender(<TimeStrip days={DAYS} scrubbable playheadIndex={3} />);
    expect(screen.getByTestId('timestrip-playhead').style.right).toBe('2px'); // "now"

    rerender(<TimeStrip days={DAYS} scrubbable playheadIndex={99} />); // clamps to now
    expect(screen.getByTestId('timestrip-playhead').style.right).toBe('2px');
  });
});
