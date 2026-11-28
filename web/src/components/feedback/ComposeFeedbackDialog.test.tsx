import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import {
  ALL_CLIPBOARD_OUTCOMES,
  ALL_FEEDBACK_KINDS,
  CaptureKind,
  ClipboardOutcome,
  FeedbackKind,
  FeedbackStep,
  MaskMode,
  type CaptureArtifact,
  type FeedbackState,
  type PickedElementInfo,
} from '@/lib/feedback/types';
import { ComposeFeedbackDialog, type ComposeFeedbackDialogProps } from './ComposeFeedbackDialog';

interface SubmitCase {
  name: string;
  step: typeof FeedbackStep.Compose | typeof FeedbackStep.Capturing;
  expectedEnabled: boolean;
}

interface KindCase {
  name: string;
  initialKind: FeedbackKind;
  targetKind: FeedbackKind;
  expectedTitle: string;
}

interface SubmittedOutcomeCase {
  name: string;
  outcome: ClipboardOutcome;
  expectedLabel: string;
  submittedUrl: string;
  hasCapture: boolean;
}

interface PickLimitCase {
  name: string;
  pickedCount: number;
  expectedLabel: string;
  expectedDisabled: boolean;
  expectedReason?: string;
}

interface TemplatePreviewCase {
  name: string;
  instruction: string;
  bodyPreview: string;
  expectedFragments: readonly string[];
}

interface SubmittedPreviewCase {
  name: string;
  captureCount: number;
  initialCopiedIndex: number;
  expectedMarker: string;
  copyTargetIndex: number;
  expectedNextMarker: string;
}

function assertRequiredInventory(
  section: Record<string, unknown>,
  label: string,
): Record<string, unknown>[] {
  requireExactRequiredFields(section, ['requiredNames', 'cases'], label);
  const requiredNames = section.requiredNames;
  if (
    !Array.isArray(requiredNames) ||
    requiredNames.length === 0 ||
    requiredNames.some((n) => typeof n !== 'string' || n.length === 0) ||
    new Set(requiredNames).size !== requiredNames.length
  ) {
    throw new Error(`${label}.requiredNames must be unique non-empty strings`);
  }
  if (!Array.isArray(section.cases) || section.cases.length === 0) {
    throw new Error(`${label}.cases must be a non-empty array`);
  }
  const rows = section.cases.map((row, idx) => requireRecord(row, `${label}.cases[${idx}]`));
  requireUniqueNames(rows, `${label}.cases`);
  const names = rows.map((r) => r.name as string);
  const missing = (requiredNames as string[]).filter((n) => !names.includes(n));
  const undeclared = names.filter((n) => !(requiredNames as string[]).includes(n));
  if (missing.length > 0 || undeclared.length > 0) {
    throw new Error(
      `${label} cases mismatch requiredNames (missing: ${missing.join(', ') || 'none'}; undeclared: ${undeclared.join(', ') || 'none'})`,
    );
  }
  return rows;
}

function loadFixture() {
  const path = resolve(process.cwd(), 'src/components/feedback/testdata/compose_dialog_cases.yaml');
  const root = requireRecord(parseStrictYAML(readFileSync(path, 'utf8'), 'compose dialog fixture'), 'compose dialog fixture');
  requireExactRequiredFields(
    root,
    ['submitCases', 'kindCases', 'submittedOutcomeCases', 'pickLimitCases', 'templatePreviewCases', 'submittedPreviewCases'],
    'compose dialog fixture',
  );

  const submitRows = assertRequiredInventory(requireRecord(root.submitCases, 'submitCases'), 'submitCases');
  const submitCases: SubmitCase[] = submitRows.map((row, idx) => {
    requireExactRequiredFields(row, ['name', 'step', 'expectedEnabled'], `submitCases[${idx}]`);
    if (
      (row.step !== FeedbackStep.Compose && row.step !== FeedbackStep.Capturing) ||
      typeof row.expectedEnabled !== 'boolean'
    ) {
      throw new Error(`submitCases[${idx}] has invalid field types`);
    }
    return {
      name: row.name as string,
      step: row.step,
      expectedEnabled: row.expectedEnabled,
    };
  });

  const kindRows = assertRequiredInventory(requireRecord(root.kindCases, 'kindCases'), 'kindCases');
  const kindCases: KindCase[] = kindRows.map((row, idx) => {
    requireExactRequiredFields(row, ['name', 'initialKind', 'targetKind', 'expectedTitle'], `kindCases[${idx}]`);
    const initialKind = ALL_FEEDBACK_KINDS.find((k) => k.wire === row.initialKind);
    const targetKind = ALL_FEEDBACK_KINDS.find((k) => k.wire === row.targetKind);
    if (!initialKind || !targetKind || typeof row.expectedTitle !== 'string') {
      throw new Error(`kindCases[${idx}] has invalid field types`);
    }
    return {
      name: row.name as string,
      initialKind,
      targetKind,
      expectedTitle: row.expectedTitle,
    };
  });

  const outcomeRows = assertRequiredInventory(
    requireRecord(root.submittedOutcomeCases, 'submittedOutcomeCases'),
    'submittedOutcomeCases',
  );
  const submittedOutcomeCases: SubmittedOutcomeCase[] = outcomeRows.map((row, idx) => {
    requireExactRequiredFields(
      row,
      ['name', 'outcome', 'expectedLabel', 'submittedUrl', 'hasCapture'],
      `submittedOutcomeCases[${idx}]`,
    );
    const outcome = ALL_CLIPBOARD_OUTCOMES.find((o) => o.wire === row.outcome);
    if (
      !outcome ||
      typeof row.expectedLabel !== 'string' ||
      typeof row.submittedUrl !== 'string' ||
      typeof row.hasCapture !== 'boolean'
    ) {
      throw new Error(`submittedOutcomeCases[${idx}] has invalid field types`);
    }
    return {
      name: row.name as string,
      outcome,
      expectedLabel: row.expectedLabel,
      submittedUrl: row.submittedUrl,
      hasCapture: row.hasCapture,
    };
  });

  const pickLimitRows = assertRequiredInventory(
    requireRecord(root.pickLimitCases, 'pickLimitCases'),
    'pickLimitCases',
  );
  const pickLimitCases: PickLimitCase[] = pickLimitRows.map((row, idx) => {
    requireExactFields(
      row,
      ['name', 'pickedCount', 'expectedLabel', 'expectedDisabled', 'expectedReason'],
      `pickLimitCases[${idx}]`,
    );
    if (
      typeof row.pickedCount !== 'number' ||
      typeof row.expectedLabel !== 'string' ||
      typeof row.expectedDisabled !== 'boolean'
    ) {
      throw new Error(`pickLimitCases[${idx}] has invalid field types`);
    }
    return {
      name: row.name as string,
      pickedCount: row.pickedCount,
      expectedLabel: row.expectedLabel,
      expectedDisabled: row.expectedDisabled,
      expectedReason: typeof row.expectedReason === 'string' ? row.expectedReason : undefined,
    };
  });

  const templatePreviewRows = assertRequiredInventory(
    requireRecord(root.templatePreviewCases, 'templatePreviewCases'),
    'templatePreviewCases',
  );
  const templatePreviewCases: TemplatePreviewCase[] = templatePreviewRows.map((row, idx) => {
    requireExactRequiredFields(
      row,
      ['name', 'instruction', 'bodyPreview', 'expectedFragments'],
      `templatePreviewCases[${idx}]`,
    );
    if (
      typeof row.instruction !== 'string' ||
      typeof row.bodyPreview !== 'string' ||
      !Array.isArray(row.expectedFragments) ||
      row.expectedFragments.some((item) => typeof item !== 'string')
    ) {
      throw new Error(`templatePreviewCases[${idx}] has invalid field types`);
    }
    return {
      name: row.name as string,
      instruction: row.instruction,
      bodyPreview: row.bodyPreview,
      expectedFragments: row.expectedFragments as string[],
    };
  });

  const submittedPreviewRows = assertRequiredInventory(
    requireRecord(root.submittedPreviewCases, 'submittedPreviewCases'),
    'submittedPreviewCases',
  );
  const submittedPreviewCases: SubmittedPreviewCase[] = submittedPreviewRows.map((row, idx) => {
    requireExactRequiredFields(
      row,
      ['name', 'captureCount', 'initialCopiedIndex', 'expectedMarker', 'copyTargetIndex', 'expectedNextMarker'],
      `submittedPreviewCases[${idx}]`,
    );
    if (
      typeof row.captureCount !== 'number' ||
      typeof row.initialCopiedIndex !== 'number' ||
      typeof row.expectedMarker !== 'string' ||
      typeof row.copyTargetIndex !== 'number' ||
      typeof row.expectedNextMarker !== 'string'
    ) {
      throw new Error(`submittedPreviewCases[${idx}] has invalid field types`);
    }
    return {
      name: row.name as string,
      captureCount: row.captureCount,
      initialCopiedIndex: row.initialCopiedIndex,
      expectedMarker: row.expectedMarker,
      copyTargetIndex: row.copyTargetIndex,
      expectedNextMarker: row.expectedNextMarker,
    };
  });

  return { submitCases, kindCases, submittedOutcomeCases, pickLimitCases, templatePreviewCases, submittedPreviewCases };
}

const FIXTURE = loadFixture();

function makeElement(id: number): PickedElementInfo {
  return {
    id,
    selector: `button.primary-action-${id}`,
    label: `button.primary-action-${id}`,
    section: 'Toolbar',
    textSnippet: `publish transcript ${id}`,
    box: { top: 10 * id, left: 20, width: 140, height: 36 },
  };
}

function makeCapture(
  id = 1,
  kind: CaptureKind = CaptureKind.Element,
  elementId: number | null = kind === CaptureKind.Element ? 1 : null,
  maskMode: MaskMode = MaskMode.Masked,
): CaptureArtifact {
  return {
    id,
    kind,
    elementId,
    blob: new Blob(['png-bytes'], { type: 'image/png' }),
    width: 320,
    height: 180,
    maskMode,
  };
}

function makeState(overrides: Partial<FeedbackState> = {}): FeedbackState {
  return {
    step: FeedbackStep.Compose,
    kind: FeedbackKind.Problem,
    attachElement: false,
    attachPage: false,
    pickedElements: [],
    nextElementId: 1,
    nextCaptureId: 1,
    pendingCapture: null,
    pickReturnStep: FeedbackStep.AttachOptions,
    maskMode: MaskMode.Masked,
    captures: [],
    captureError: null,
    title: '',
    submittedUrl: null,
    clipboardOutcome: ClipboardOutcome.NotNeeded,
    copiedCaptureId: null,
    ...overrides,
  };
}

function makeProps(overrides: Partial<ComposeFeedbackDialogProps> = {}): ComposeFeedbackDialogProps {
  return {
    open: true,
    state: makeState(),
    issueBodyPreview:
      '# Bug Report\n\n## Problem Statement\n\n<!-- paste image -->\n\n<!-- what happened -->\n\n## User Requirements and Expectations\n\n<!-- what was the expected behaviour, bullet points. -->\n\n<!-- optional: what would ensure user satisfaction -->\n\n## System Information\n\n```text\npeasant version: 0.9.4\n```',
    issuePreviewUrl: 'https://github.com/peasant-labs/peasant/issues/new?template=in-app-problem.yml',
    truncated: false,
    onChangeKind: vi.fn(),
    onUpdateDraft: vi.fn(),
    onRemoveCapture: vi.fn(),
    onRequestUnmask: vi.fn(),
    onRemask: vi.fn(),
    onReopenPicker: vi.fn(),
    onBack: vi.fn(),
    onSubmit: vi.fn(),
    onCopyCapture: vi.fn(),
    onDownloadCapture: vi.fn(),
    onClose: vi.fn(),
    ...overrides,
  };
}

describe('ComposeFeedbackDialog', () => {
  let nextUrlId = 0;
  let createObjectURLSpy: ReturnType<typeof vi.fn>;
  let revokeObjectURLSpy: ReturnType<typeof vi.fn>;
  const origCreate = URL.createObjectURL;
  const origRevoke = URL.revokeObjectURL;

  beforeEach(() => {
    nextUrlId = 0;
    createObjectURLSpy = vi.fn(() => `blob:test-capture-${++nextUrlId}`);
    revokeObjectURLSpy = vi.fn();
    URL.createObjectURL = createObjectURLSpy as unknown as typeof URL.createObjectURL;
    URL.revokeObjectURL = revokeObjectURLSpy as unknown as typeof URL.revokeObjectURL;
  });

  afterEach(() => {
    cleanup();
    URL.createObjectURL = origCreate;
    URL.revokeObjectURL = origRevoke;
  });

  describe('submit gating', () => {
    it.each(FIXTURE.submitCases)('$name', (c) => {
      const props = makeProps({
        state: makeState({ step: c.step }),
      });
      render(<ComposeFeedbackDialog {...props} />);
      const button = screen.getByRole('button', { name: 'open github issue' });
      if (c.expectedEnabled) {
        expect(button).toBeEnabled();
        fireEvent.click(button);
        expect(props.onSubmit).toHaveBeenCalledTimes(1);
      } else {
        expect(button).toBeDisabled();
        fireEvent.click(button);
        expect(props.onSubmit).not.toHaveBeenCalled();
      }
    });
  });

  describe('kind switching', () => {
    it.each(FIXTURE.kindCases)('$name', (c) => {
      const props = makeProps({
        state: makeState({ kind: c.initialKind }),
      });
      render(<ComposeFeedbackDialog {...props} />);
      expect(screen.getByRole('dialog', { name: c.expectedTitle })).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: c.targetKind.label }));
      expect(props.onChangeKind).toHaveBeenCalledWith(c.targetKind);
    });
  });

  describe('template preview', () => {
    it.each(FIXTURE.templatePreviewCases)('$name', (c) => {
      const props = makeProps({
        issueBodyPreview: c.bodyPreview,
      });
      render(<ComposeFeedbackDialog {...props} />);
      expect(screen.getByText(c.instruction)).toBeInTheDocument();
      expect(screen.queryByLabelText(/description/i)).toBeNull();
      const preview = screen.getByLabelText('issue template preview');
      expect(preview.tagName).toBe('PRE');
      for (const fragment of c.expectedFragments) {
        expect(preview.textContent).toContain(fragment);
      }
    });
  });

  describe('submitted state', () => {
    it.each(FIXTURE.submittedOutcomeCases)('$name', (c) => {
      const captures = c.hasCapture ? [makeCapture(1, CaptureKind.Element, 1)] : [];
      const props = makeProps({
        state: makeState({
          step: FeedbackStep.Submitted,
          clipboardOutcome: c.outcome,
          submittedUrl: c.submittedUrl,
          captures,
        }),
      });
      render(<ComposeFeedbackDialog {...props} />);

      expect(screen.getByText(c.expectedLabel)).toBeInTheDocument();
      expect(screen.getByText(/Ctrl\/Cmd\+V/i)).toBeInTheDocument();

      const fallbackLink = screen.getByRole('link', { name: 'open github issue' });
      expect(fallbackLink).toHaveAttribute('href', c.submittedUrl);

      if (c.hasCapture) {
        const copyBtn = screen.getByRole('button', { name: /copy image 1/i });
        const downloadBtn = screen.getByRole('button', { name: /download image 1/i });
        fireEvent.click(copyBtn);
        fireEvent.click(downloadBtn);
        expect(props.onCopyCapture).toHaveBeenCalledWith(1);
        expect(props.onDownloadCapture).toHaveBeenCalledWith(1);
      } else {
        expect(screen.queryByRole('button', { name: /copy image/i })).toBeNull();
        expect(screen.queryByRole('button', { name: /download/i })).toBeNull();
      }

      fireEvent.click(screen.getByRole('button', { name: 'done' }));
      expect(props.onClose).toHaveBeenCalledTimes(1);
    });

    it.each(FIXTURE.submittedPreviewCases)('$name', (c) => {
      const captures = Array.from({ length: c.captureCount }, (_, idx) => {
        const isLast = idx === c.captureCount - 1;
        return makeCapture(idx + 1, isLast ? CaptureKind.Page : CaptureKind.Element, isLast ? null : idx + 1);
      });
      const initialCopiedId = captures[c.initialCopiedIndex - 1].id;
      const targetCaptureId = captures[c.copyTargetIndex - 1].id;

      const baseProps = makeProps({
        state: makeState({
          step: FeedbackStep.Compose,
          captures,
        }),
      });
      const { rerender } = render(<ComposeFeedbackDialog {...baseProps} />);
      expect(createObjectURLSpy).toHaveBeenCalledTimes(c.captureCount);
      expect(revokeObjectURLSpy).not.toHaveBeenCalled();

      rerender(
        <ComposeFeedbackDialog
          {...makeProps({
            ...baseProps,
            state: makeState({
              step: FeedbackStep.Submitted,
              clipboardOutcome: ClipboardOutcome.Copied,
              submittedUrl: 'https://github.com/peasant-labs/peasant/issues/new?template=in-app-problem.yml',
              captures,
              copiedCaptureId: initialCopiedId,
            }),
          })}
        />,
      );

      // Transitioning to Submitted keeps the exact same object URLs alive without revoking them.
      expect(revokeObjectURLSpy).not.toHaveBeenCalled();
      expect(screen.getByText(c.expectedMarker)).toBeInTheDocument();

      const imgs = screen.getAllByRole('img');
      expect(imgs).toHaveLength(c.captureCount);
      imgs.forEach((img, idx) => {
        expect(img).toHaveAttribute('src', `blob:test-capture-${idx + 1}`);
        expect(screen.getByText(`image ${idx + 1}`)).toBeInTheDocument();
        expect(screen.getByRole('button', { name: new RegExp(`^copy image ${idx + 1}\\b`, 'i') })).toBeInTheDocument();
        expect(screen.getByRole('button', { name: new RegExp(`^download image ${idx + 1}\\b`, 'i') })).toBeInTheDocument();
      });

      fireEvent.click(screen.getByRole('button', { name: new RegExp(`^copy image ${c.copyTargetIndex}\\b`, 'i') }));
      expect(baseProps.onCopyCapture).toHaveBeenCalledWith(targetCaptureId);

      rerender(
        <ComposeFeedbackDialog
          {...makeProps({
            ...baseProps,
            state: makeState({
              step: FeedbackStep.Submitted,
              clipboardOutcome: ClipboardOutcome.Copied,
              submittedUrl: 'https://github.com/peasant-labs/peasant/issues/new?template=in-app-problem.yml',
              captures,
              copiedCaptureId: targetCaptureId,
            }),
          })}
        />,
      );

      expect(screen.getByText(c.expectedNextMarker)).toBeInTheDocument();
      expect(revokeObjectURLSpy).not.toHaveBeenCalled();

      // Closing the pane revokes every object URL.
      rerender(
        <ComposeFeedbackDialog
          {...makeProps({
            ...baseProps,
            open: false,
            state: makeState({
              step: FeedbackStep.Closed,
              captures: [],
            }),
          })}
        />,
      );
      expect(revokeObjectURLSpy).toHaveBeenCalledTimes(c.captureCount);
    });
  });

  describe('pick limit gating', () => {
    it.each(FIXTURE.pickLimitCases)('$name', (c) => {
      const pickedElements = Array.from({ length: c.pickedCount }, (_, i) => makeElement(i + 1));
      const props = makeProps({
        state: makeState({ pickedElements }),
      });
      render(<ComposeFeedbackDialog {...props} />);

      const button = screen.getByRole('button', { name: c.expectedLabel });
      if (c.expectedDisabled) {
        expect(button).toBeDisabled();
        expect(screen.getByText(c.expectedReason!)).toBeInTheDocument();
      } else {
        expect(button).toBeEnabled();
        fireEvent.click(button);
        expect(props.onReopenPicker).toHaveBeenCalledTimes(1);
      }
    });
  });

  it('unmask-requires-warning-confirmation', () => {
    const props = makeProps({
      state: makeState({
        maskMode: MaskMode.Masked,
        captures: [makeCapture(1, CaptureKind.Element, 1, MaskMode.Masked)],
      }),
    });
    const { rerender } = render(<ComposeFeedbackDialog {...props} />);

    fireEvent.click(screen.getByRole('button', { name: 'unmask transcript' }));
    expect(props.onRequestUnmask).not.toHaveBeenCalled();
    expect(
      screen.getByText(
        'This repository is public. Unmasking may reveal prompts, code, file paths, or secrets in your screenshot.',
      ),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'keep masked' }));
    expect(props.onRequestUnmask).not.toHaveBeenCalled();
    expect(
      screen.queryByText(
        'This repository is public. Unmasking may reveal prompts, code, file paths, or secrets in your screenshot.',
      ),
    ).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'unmask transcript' }));
    fireEvent.click(screen.getByRole('button', { name: 'confirm unmask' }));
    expect(props.onRequestUnmask).toHaveBeenCalledTimes(1);

    rerender(
      <ComposeFeedbackDialog
        {...makeProps({
          ...props,
          state: makeState({
            maskMode: MaskMode.Unmasked,
            captures: [makeCapture(2, CaptureKind.Element, 1, MaskMode.Unmasked)],
          }),
        })}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'mask transcript' }));
    expect(props.onRemask).toHaveBeenCalledTimes(1);
  });

  it('creates preview object URLs, numbers captures, and revokes URLs on capture change and unmount', () => {
    const firstCapture = makeCapture(10, CaptureKind.Element, 1);
    const secondCapture = makeCapture(11, CaptureKind.Page, null);
    const props = makeProps({
      state: makeState({ captures: [firstCapture] }),
    });

    const { rerender, unmount } = render(<ComposeFeedbackDialog {...props} />);
    expect(createObjectURLSpy).toHaveBeenCalledTimes(1);
    expect(createObjectURLSpy).toHaveBeenCalledWith(firstCapture.blob);
    const img = screen.getByRole('img', { name: /image 1: picked element capture/i });
    expect(img).toHaveAttribute('src', 'blob:test-capture-1');
    expect(screen.getByText('image 1')).toBeInTheDocument();
    expect(revokeObjectURLSpy).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: /remove picked element capture \(image 1\)/i }));
    expect(props.onRemoveCapture).toHaveBeenCalledWith(10);

    rerender(
      <ComposeFeedbackDialog
        {...makeProps({
          ...props,
          state: makeState({ captures: [secondCapture] }),
        })}
      />,
    );
    expect(revokeObjectURLSpy).toHaveBeenCalledWith('blob:test-capture-1');
    expect(createObjectURLSpy).toHaveBeenCalledTimes(2);
    expect(screen.getByRole('img', { name: /image 1: whole page capture/i })).toHaveAttribute('src', 'blob:test-capture-2');

    unmount();
    expect(revokeObjectURLSpy).toHaveBeenCalledWith('blob:test-capture-2');
  });

  it('renders notices, title updates, picker reopen, and back action', () => {
    const el1 = makeElement(1);
    const el2 = makeElement(2);
    const props = makeProps({
      state: makeState({
        title: 'Initial title',
        pickedElements: [el1, el2],
        captureError: 'SnapDOM timed out for page capture',
      }),
      truncated: true,
    });

    render(<ComposeFeedbackDialog {...props} />);

    expect(screen.getByText('SnapDOM timed out for page capture')).toBeInTheDocument();
    expect(screen.getByText(/length limit and was truncated/i)).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText('title'), { target: { value: 'Updated title' } });
    expect(props.onUpdateDraft).toHaveBeenCalledWith({ title: 'Updated title' });

    fireEvent.click(screen.getByRole('button', { name: 'pick another element' }));
    expect(props.onReopenPicker).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole('button', { name: 'back' }));
    expect(props.onBack).toHaveBeenCalledTimes(1);
  });
});
