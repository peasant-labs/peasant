import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { LabelsStep } from '@/components/share/LabelsStep';
import type { ShareSession, LabelSelection } from '@/lib/share/types';
import type { AnnotationSummary } from '@/lib/api/annotations';
import * as annotationsApi from '@/lib/api/annotations';

vi.mock('@/lib/api/annotations', async (importOriginal) => {
  const actual = await importOriginal<typeof annotationsApi>();
  return { ...actual, fetchSessionAnnotations: vi.fn() };
});

function makeSession(id: string): ShareSession {
  return {
    id,
    provider: 'claude-code',
    projectName: 'demo-project',
    projectHash: 'ph-1',
    hostSlug: 'host',
    startTime: '2026-02-24T09:00:00Z',
    durationMins: 30,
    totalTokens: 10000,
    turnCount: 10,
    model: 'claude-sonnet-4-6',
    shareStatus: 'new',
    preview: 'demo',
  };
}

function annotation(
  id: string,
  annotatorKind: AnnotationSummary['annotatorKind'],
  typeName: string,
  value: string,
): AnnotationSummary {
  return {
    id,
    targetKind: 'session',
    targetSessionId: 'sess-x',
    isPrimary: false,
    annotatorKind,
    annotatorName: annotatorKind === 'human' ? 'human-web' : 'classifier',
    typeId: `type.${typeName}`,
    typeName,
    value,
    createdAt: BigInt(1),
  };
}

describe('LabelsStep', () => {
  let fetchAnnotationsSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchAnnotationsSpy = annotationsApi.fetchSessionAnnotations as ReturnType<typeof vi.fn>;
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  it('groups real annotations into Automatic (rule/agent) and Manual (human)', async () => {
    fetchAnnotationsSpy.mockResolvedValue([
      annotation('a1', 'rule', 'Outcome', 'success'),
      annotation('a2', 'agent', 'Frustration', 'present'),
      annotation('a3', 'human', 'Usefulness', 'high'),
    ]);

    const onLabelsChange = vi.fn();
    render(
      <LabelsStep
        sessions={[makeSession('sess-x')]}
        selectedIds={new Set(['sess-x'])}
        onLabelsChange={onLabelsChange}
        onNext={vi.fn()}
        onFooterActionsChange={vi.fn()}
      />,
    );

    await screen.findByText('Automatic');
    expect(screen.getByText('Manual')).toBeInTheDocument();
    // 3 labels discovered, all included by default.
    expect(screen.getByText(/3 of 3 labels included/)).toBeInTheDocument();
    // The fetch was issued against the real endpoint helper.
    expect(fetchAnnotationsSpy).toHaveBeenCalledWith('sess-x');
  });

  it('defaults to including every label and reports them upward', async () => {
    fetchAnnotationsSpy.mockResolvedValue([
      annotation('a1', 'rule', 'Outcome', 'success'),
      annotation('a2', 'human', 'Usefulness', 'high'),
    ]);

    const onLabelsChange = vi.fn();
    render(
      <LabelsStep
        sessions={[makeSession('sess-x')]}
        selectedIds={new Set(['sess-x'])}
        onLabelsChange={onLabelsChange}
        onNext={vi.fn()}
        onFooterActionsChange={vi.fn()}
      />,
    );

    await screen.findByText('Automatic');

    await waitFor(() => {
      const last = onLabelsChange.mock.calls.at(-1)?.[0] as LabelSelection;
      expect(last.includedIds.has('a1')).toBe(true);
      expect(last.includedIds.has('a2')).toBe(true);
      expect(last.includedIds.size).toBe(2);
    });
  });

  it('excludes a label when its chip is toggled off', async () => {
    fetchAnnotationsSpy.mockResolvedValue([
      annotation('a1', 'rule', 'Outcome', 'success'),
      annotation('a2', 'human', 'Usefulness', 'high'),
    ]);

    const onLabelsChange = vi.fn();
    render(
      <LabelsStep
        sessions={[makeSession('sess-x')]}
        selectedIds={new Set(['sess-x'])}
        onLabelsChange={onLabelsChange}
        onNext={vi.fn()}
        onFooterActionsChange={vi.fn()}
      />,
    );

    const outcomeChip = await screen.findByRole('button', { name: /Outcome/ });
    expect(outcomeChip).toHaveAttribute('aria-pressed', 'true');

    const user = userEvent.setup();
    await user.click(outcomeChip);

    expect(outcomeChip).toHaveAttribute('aria-pressed', 'false');
    await waitFor(() => {
      const last = onLabelsChange.mock.calls.at(-1)?.[0] as LabelSelection;
      expect(last.includedIds.has('a1')).toBe(false);
      expect(last.includedIds.has('a2')).toBe(true);
    });
  });

  it('reads deterministic mock annotations when useMock is set (no network)', async () => {
    const onLabelsChange = vi.fn();
    render(
      <LabelsStep
        sessions={[makeSession('sess-mock-1')]}
        selectedIds={new Set(['sess-mock-1'])}
        onLabelsChange={onLabelsChange}
        onNext={vi.fn()}
        onFooterActionsChange={vi.fn()}
        useMock
      />,
    );

    await waitFor(() => expect(onLabelsChange).toHaveBeenCalled());
    // The real endpoint helper must never be called in mock mode.
    expect(fetchAnnotationsSpy).not.toHaveBeenCalled();
  });

  it('shows the empty state and a Skip action when there are no labels', async () => {
    fetchAnnotationsSpy.mockResolvedValue([]);

    const setFooterActions = vi.fn();
    render(
      <LabelsStep
        sessions={[makeSession('sess-empty')]}
        selectedIds={new Set(['sess-empty'])}
        onLabelsChange={vi.fn()}
        onNext={vi.fn()}
        onFooterActionsChange={setFooterActions}
      />,
    );

    expect(await screen.findByText(/No labels on the selected sessions/)).toBeInTheDocument();
    await waitFor(() => expect(setFooterActions.mock.calls.at(-1)?.[0]?.primary.label).toBe('Skip'));
  });

  it('surfaces a load error in the toolbar instead of crashing', async () => {
    fetchAnnotationsSpy.mockRejectedValue(new Error('boom'));

    render(
      <LabelsStep
        sessions={[makeSession('sess-err')]}
        selectedIds={new Set(['sess-err'])}
        onLabelsChange={vi.fn()}
        onNext={vi.fn()}
        onFooterActionsChange={vi.fn()}
      />,
    );

    expect(await screen.findByText(/Could not load labels: boom/)).toBeInTheDocument();
  });
});
