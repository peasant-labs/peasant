import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import MultiSelectPopover from './MultiSelectPopover';

// Behavioral-parity smoke for the compound→single Popover rewrite. The
// design-system Popover owns open/close, dismiss-on-Escape (focus returns to
// the trigger), and click-inside-keeps-open; the Checkbox is the real native
// input. We assert the multi-select contract still holds through that rewrite:
// open, toggle, "all" clears, Escape closes.

const OPTIONS = [
  { label: 'Claude', value: 'claude' },
  { label: 'Codex', value: 'codex' },
];

describe('MultiSelectPopover', () => {
  it('is closed until the trigger is activated', () => {
    render(
      <MultiSelectPopover label="Providers" options={OPTIONS} selected={[]} onChange={() => {}} />,
    );
    // The trigger is the only button and shows the label while nothing is picked.
    expect(screen.getByRole('button', { name: /providers/i })).toBeInTheDocument();
    // The floating dialog (the option list) is not mounted while closed.
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('opens the option list, toggles an option, and closes on Escape', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <MultiSelectPopover label="Providers" options={OPTIONS} selected={[]} onChange={onChange} />,
    );

    await user.click(screen.getByRole('button'));
    expect(screen.getByRole('dialog')).toBeInTheDocument();

    // Picking an option while "all" is on switches the filter to just that one.
    await user.click(screen.getByLabelText('Codex'));
    expect(onChange).toHaveBeenCalledWith(['codex']);

    // Escape dismisses the popover (focus returns to the trigger).
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('button')).toHaveFocus();
  });

  it('removes an already-selected option without dropping the others', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <MultiSelectPopover
        label="Providers"
        options={OPTIONS}
        selected={['claude', 'codex']}
        onChange={onChange}
      />,
    );

    await user.click(screen.getByRole('button'));
    await user.click(screen.getByLabelText('Claude'));
    expect(onChange).toHaveBeenCalledWith(['codex']);
  });

  it('selecting "all …" clears the filter', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <MultiSelectPopover
        label="Providers"
        options={OPTIONS}
        selected={['claude']}
        onChange={onChange}
      />,
    );

    await user.click(screen.getByRole('button'));
    await user.click(screen.getByLabelText(/all providers/i));
    expect(onChange).toHaveBeenCalledWith([]);
  });
});
