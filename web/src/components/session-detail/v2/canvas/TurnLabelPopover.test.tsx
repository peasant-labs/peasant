import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/react';
import { TurnLabelPopover, type SavedTurnLabel } from './TurnLabelPopover';
import {
  CUSTOM_LABEL_TYPE,
  FRUSTRATION_SIGNAL_TYPE,
  TURN_OUTCOME_TYPE,
  TURN_FLAG_TYPE,
} from '@/test/fixtures/entry-annotation-types';

// -- Mocks --------------------------------------------------------------------

// TurnLabelPopover composes TWO independent controlled popovers behind two
// triggers: "Label this turn" (the restored outcome+flag modal, composing the
// design system's real `TranscriptLabelPopover` — NOT mocked, so this test
// exercises the actual production component the app renders) and "More
// labels" (the typed annotation-registry picker preserved from before the
// restoration, for types the fixed outcome+flag shape has no room for).
//
// The save paths are the real client wrappers in the app; mock them so the
// test asserts each popover calls the right one with the right shape (no
// network).
const saveTurnLabel = vi.hoisted(() => vi.fn());
const saveTurnLabels = vi.hoisted(() => vi.fn());
vi.mock('@/lib/api/annotations', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/api/annotations')>();
  return { ...actual, saveTurnLabel, saveTurnLabels };
});

beforeEach(() => {
  saveTurnLabel.mockReset();
  saveTurnLabels.mockReset();
});

afterEach(() => {
  cleanup();
});

function renderPopover(
  types: typeof CUSTOM_LABEL_TYPE[],
  opts: { savedLabels?: SavedTurnLabel[]; onSaved?: (l: SavedTurnLabel) => void } = {},
) {
  const onSaved = opts.onSaved ?? vi.fn();
  render(
    <TurnLabelPopover
      sessionId="sess-1"
      entryIndex={3}
      types={types}
      savedLabels={opts.savedLabels}
      onSaved={onSaved}
    />,
  );
  return onSaved;
}

function outcomeTrigger() {
  return screen.getByRole('button', { name: /Label this turn/i });
}
function moreTrigger() {
  return screen.getByRole('button', { name: /More labels/i });
}

describe('TurnLabelPopover — restored outcome+flag modal', () => {
  const TYPES = [TURN_OUTCOME_TYPE, TURN_FLAG_TYPE];

  it('renders the outcome (good/neutral/bad) + flag (none/error/retry loop/revert/highlight) + save/cancel modal — the fidelity oracle shape', () => {
    renderPopover(TYPES);
    fireEvent.click(outcomeTrigger());

    expect(screen.getByText(/label turn 3/i)).toBeInTheDocument();
    for (const outcome of ['good', 'neutral', 'bad']) {
      expect(screen.getByRole('button', { name: outcome })).toBeInTheDocument();
    }
    for (const flag of ['none', 'error', 'retry loop', 'revert', 'highlight']) {
      expect(screen.getByRole('button', { name: flag })).toBeInTheDocument();
    }
    expect(screen.getByRole('button', { name: 'cancel' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'save label' })).toBeInTheDocument();
  });

  it('defaults to neutral outcome / none flag when nothing is saved yet', () => {
    renderPopover(TYPES);
    fireEvent.click(outcomeTrigger());

    expect(screen.getByRole('button', { name: 'neutral' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'none' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('prefills from the turn"s already-saved outcome/flag, mapping the stored retry_loop to the UI"s retry-loop chip', () => {
    renderPopover(TYPES, {
      savedLabels: [
        { entryIndex: 3, typeId: 'quality.turn_outcome', typeName: 'Turn outcome', value: 'good', id: 'a1' },
        { entryIndex: 3, typeId: 'quality.turn_flag', typeName: 'Turn flag', value: 'retry_loop', id: 'a2' },
      ],
    });
    fireEvent.click(outcomeTrigger());

    expect(screen.getByRole('button', { name: 'good' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'retry loop' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('saves both outcome and flag in ONE atomic batch call, mapping the UI"s hyphenated retry-loop to the persisted retry_loop', async () => {
    saveTurnLabels.mockResolvedValue(['id-outcome', 'id-flag']);
    const onSaved = renderPopover(TYPES);

    fireEvent.click(outcomeTrigger());
    fireEvent.click(screen.getByRole('button', { name: 'bad' }));
    fireEvent.click(screen.getByRole('button', { name: 'retry loop' }));
    fireEvent.click(screen.getByRole('button', { name: 'save label' }));

    await waitFor(() => expect(saveTurnLabels).toHaveBeenCalledTimes(1));
    expect(saveTurnLabels).toHaveBeenCalledWith({
      sessionId: 'sess-1',
      entryIndex: 3,
      items: [
        { typeId: 'quality.turn_outcome', value: 'bad' },
        { typeId: 'quality.turn_flag', value: 'retry_loop' },
      ],
    });

    await waitFor(() =>
      expect(onSaved).toHaveBeenCalledWith(
        expect.objectContaining({ typeId: 'quality.turn_outcome', value: 'bad', id: 'id-outcome' }),
      ),
    );
    expect(onSaved).toHaveBeenCalledWith(
      expect.objectContaining({ typeId: 'quality.turn_flag', value: 'retry_loop', id: 'id-flag' }),
    );
  });

  it('maps the "none" flag chip to the persisted "none" value (not an empty string)', async () => {
    saveTurnLabels.mockResolvedValue(['id-outcome', 'id-flag']);
    renderPopover(TYPES);

    fireEvent.click(outcomeTrigger());
    fireEvent.click(screen.getByRole('button', { name: 'save label' }));

    await waitFor(() => expect(saveTurnLabels).toHaveBeenCalledTimes(1));
    expect(saveTurnLabels).toHaveBeenCalledWith(
      expect.objectContaining({
        items: [
          { typeId: 'quality.turn_outcome', value: 'neutral' },
          { typeId: 'quality.turn_flag', value: 'none' },
        ],
      }),
    );
  });

  it('cancel closes without saving', () => {
    renderPopover(TYPES);
    fireEvent.click(outcomeTrigger());
    fireEvent.click(screen.getByRole('button', { name: 'bad' }));
    fireEvent.click(screen.getByRole('button', { name: 'cancel' }));

    expect(saveTurnLabels).not.toHaveBeenCalled();
    expect(screen.queryByText(/label turn 3/i)).not.toBeInTheDocument();
    expect(outcomeTrigger()).toHaveAttribute('aria-expanded', 'false');
  });

  it('shows a retryable, actionable error when the batch save fails, and does not silently swallow it', async () => {
    saveTurnLabels.mockRejectedValue(new Error('network unreachable'));
    renderPopover(TYPES);

    fireEvent.click(outcomeTrigger());
    fireEvent.click(screen.getByRole('button', { name: 'save label' }));

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('network unreachable'));
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument();

    // Retry reopens the modal so the user can attempt the save again.
    fireEvent.click(screen.getByRole('button', { name: /retry/i }));
    expect(screen.getByText(/label turn 3/i)).toBeInTheDocument();
  });
});

describe('TurnLabelPopover — "more labels" preserves the typed registry (custom free text + system classifiers)', () => {
  const TYPES = [CUSTOM_LABEL_TYPE, FRUSTRATION_SIGNAL_TYPE, TURN_OUTCOME_TYPE, TURN_FLAG_TYPE];

  it('excludes the outcome/flag types from the more-labels list (they have their own dedicated modal)', () => {
    renderPopover(TYPES);
    fireEvent.click(moreTrigger());

    expect(screen.getByRole('button', { name: /Custom label/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Frustration Signal/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Turn outcome/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Turn flag/ })).not.toBeInTheDocument();
  });

  it('does not render the more-labels trigger at all when nothing but outcome/flag is applicable', () => {
    renderPopover([TURN_OUTCOME_TYPE, TURN_FLAG_TYPE]);
    expect(screen.queryByRole('button', { name: /More labels/i })).not.toBeInTheDocument();
  });

  it('offers a text input for the free-text custom label type', () => {
    renderPopover(TYPES);
    fireEvent.click(moreTrigger());
    fireEvent.click(screen.getByRole('button', { name: /Custom label/ }));

    expect(
      screen.getByText(/Type a label, then press Enter or Save/i),
    ).toBeInTheDocument();
    expect(screen.getByRole('textbox')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save' })).toBeInTheDocument();
  });

  it('saves the typed free-text string via saveTurnLabel (single-item) and reports it upward', async () => {
    saveTurnLabel.mockResolvedValue('annotation-id-9');
    const onSaved = renderPopover(TYPES);

    fireEvent.click(moreTrigger());
    fireEvent.click(screen.getByRole('button', { name: /Custom label/ }));
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'good handoff' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(saveTurnLabel).toHaveBeenCalledTimes(1));
    expect(saveTurnLabel).toHaveBeenCalledWith({
      sessionId: 'sess-1',
      entryIndex: 3,
      typeId: 'user.custom_label',
      value: 'good handoff',
    });
    await waitFor(() =>
      expect(onSaved).toHaveBeenCalledWith(
        expect.objectContaining({
          entryIndex: 3,
          typeId: 'user.custom_label',
          typeName: 'Custom label',
          value: 'good handoff',
          id: 'annotation-id-9',
        }),
      ),
    );
  });

  it('lists the permissible values for an enumerated system-classifier type and saves the clicked one', async () => {
    saveTurnLabel.mockResolvedValue('id-2');
    renderPopover(TYPES);

    fireEvent.click(moreTrigger());
    fireEvent.click(screen.getByRole('button', { name: /Frustration Signal/ }));
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'detected' }));

    await waitFor(() => expect(saveTurnLabel).toHaveBeenCalledTimes(1));
    expect(saveTurnLabel).toHaveBeenCalledWith(
      expect.objectContaining({ typeId: 'quality.frustration_signal', value: 'detected' }),
    );
  });

  it('resets to the type list when reopened after closing mid-step', () => {
    renderPopover(TYPES);

    fireEvent.click(moreTrigger());
    fireEvent.click(screen.getByRole('button', { name: /Custom label/ }));
    expect(screen.getByRole('textbox')).toBeInTheDocument();

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
    fireEvent.click(moreTrigger());

    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Custom label/ })).toBeInTheDocument();
  });

  it('closes on Escape and returns focus to the trigger', () => {
    renderPopover(TYPES);
    const trigger = moreTrigger();

    fireEvent.click(trigger);
    expect(screen.getByRole('button', { name: /Custom label/ })).toBeInTheDocument();

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(screen.queryByRole('button', { name: /Custom label/ })).not.toBeInTheDocument();
    expect(document.activeElement).toBe(trigger);
  });

  it('closes when the user clicks outside the popover', () => {
    renderPopover(TYPES);

    fireEvent.click(moreTrigger());
    expect(screen.getByRole('button', { name: /Custom label/ })).toBeInTheDocument();

    fireEvent.mouseDown(document.body);

    expect(screen.queryByRole('button', { name: /Custom label/ })).not.toBeInTheDocument();
  });
});
