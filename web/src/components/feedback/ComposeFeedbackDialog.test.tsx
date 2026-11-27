import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import {
  parseStrictYAML,
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
  type FeedbackContextInfo,
  type FeedbackState,
  type PickedElementInfo,
} from '@/lib/feedback/types';
import { ComposeFeedbackDialog, type ComposeFeedbackDialogProps } from './ComposeFeedbackDialog';

interface SubmitCase {
  name: string;
  step: typeof FeedbackStep.Compose | typeof FeedbackStep.Capturing;
  description: string;
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
  requireExactRequiredFields(root, ['submitCases', 'kindCases', 'submittedOutcomeCases'], 'compose dialog fixture');

  const submitRows = assertRequiredInventory(requireRecord(root.submitCases, 'submitCases'), 'submitCases');
  const submitCases: SubmitCase[] = submitRows.map((row, idx) => {
    requireExactRequiredFields(row, ['name', 'step', 'description', 'expectedEnabled'], `submitCases[${idx}]`);
    if (
      (row.step !== FeedbackStep.Compose && row.step !== FeedbackStep.Capturing) ||
      typeof row.description !== 'string' ||
      typeof row.expectedEnabled !== 'boolean'
    ) {
      throw new Error(`submitCases[${idx}] has invalid field types`);
    }
    return {
      name: row.name as string,
      step: row.step,
      description: row.description,
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

  return { submitCases, kindCases, submittedOutcomeCases };
}

const FIXTURE = loadFixture();

const SAMPLE_CONTEXT: FeedbackContextInfo = {
  route: '/projects/abc123/sess-1',
  viewport: { width: 1280, height: 800 },
  theme: 'dark',
  appVersion: '0.9.4',
  userAgent: 'Mozilla/5.0 TestBrowser',
};

const SAMPLE_ELEMENT: PickedElementInfo = {
  selector: 'button.primary-action',
  label: 'button.primary-action',
  section: 'Toolbar',
  textSnippet: 'publish transcript',
  box: { top: 10, left: 20, width: 140, height: 36 },
};

function makeCapture(kind: CaptureKind = CaptureKind.Element, maskMode: MaskMode = MaskMode.Masked): CaptureArtifact {
  return {
    kind,
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
    pickedElement: null,
    maskMode: MaskMode.Masked,
    captures: [],
    captureError: null,
    title: '',
    description: '',
    submittedUrl: null,
    clipboardOutcome: ClipboardOutcome.NotNeeded,
    ...overrides,
  };
}

function makeProps(overrides: Partial<ComposeFeedbackDialogProps> = {}): ComposeFeedbackDialogProps {
  return {
    open: true,
    state: makeState(),
    context: SAMPLE_CONTEXT,
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
        state: makeState({ step: c.step, description: c.description }),
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

  describe('submitted state', () => {
    it.each(FIXTURE.submittedOutcomeCases)('$name', (c) => {
      const captures = c.hasCapture ? [makeCapture(CaptureKind.Element)] : [];
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
        const copyBtn = screen.getByRole('button', { name: /copy image/i });
        const downloadBtn = screen.getByRole('button', { name: /download/i });
        fireEvent.click(copyBtn);
        fireEvent.click(downloadBtn);
        expect(props.onCopyCapture).toHaveBeenCalledWith(CaptureKind.Element);
        expect(props.onDownloadCapture).toHaveBeenCalledWith(CaptureKind.Element);
      } else {
        expect(screen.queryByRole('button', { name: /copy image/i })).toBeNull();
        expect(screen.queryByRole('button', { name: /download/i })).toBeNull();
      }

      fireEvent.click(screen.getByRole('button', { name: 'done' }));
      expect(props.onClose).toHaveBeenCalledTimes(1);
    });
  });

  it('unmask-requires-warning-confirmation', () => {
    const props = makeProps({
      state: makeState({
        maskMode: MaskMode.Masked,
        captures: [makeCapture(CaptureKind.Element, MaskMode.Masked)],
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
            captures: [makeCapture(CaptureKind.Element, MaskMode.Unmasked)],
          }),
        })}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'mask transcript' }));
    expect(props.onRemask).toHaveBeenCalledTimes(1);
  });

  it('creates preview object URLs and revokes them on capture change and unmount', () => {
    const firstCapture = makeCapture(CaptureKind.Element);
    const secondCapture = makeCapture(CaptureKind.Page);
    const props = makeProps({
      state: makeState({ captures: [firstCapture] }),
    });

    const { rerender, unmount } = render(<ComposeFeedbackDialog {...props} />);
    expect(createObjectURLSpy).toHaveBeenCalledTimes(1);
    expect(createObjectURLSpy).toHaveBeenCalledWith(firstCapture.blob);
    const img = screen.getByRole('img', { name: /picked element capture/i });
    expect(img).toHaveAttribute('src', 'blob:test-capture-1');
    expect(revokeObjectURLSpy).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: /remove picked element capture/i }));
    expect(props.onRemoveCapture).toHaveBeenCalledWith(CaptureKind.Element);

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
    expect(screen.getByRole('img', { name: /whole page capture/i })).toHaveAttribute('src', 'blob:test-capture-2');

    unmount();
    expect(revokeObjectURLSpy).toHaveBeenCalledWith('blob:test-capture-2');
  });

  it('renders exact auto-attached context, notices, draft updates, picker reopen, and back action', () => {
    const props = makeProps({
      state: makeState({
        title: 'Initial title',
        description: 'Initial body',
        pickedElement: SAMPLE_ELEMENT,
        captureError: 'SnapDOM timed out for page capture',
      }),
      truncated: true,
    });

    render(<ComposeFeedbackDialog {...props} />);

    expect(screen.getByText(SAMPLE_CONTEXT.route)).toBeInTheDocument();
    expect(screen.getByText(SAMPLE_ELEMENT.selector)).toBeInTheDocument();
    expect(screen.getByText(SAMPLE_ELEMENT.section)).toBeInTheDocument();
    expect(screen.getByText(SAMPLE_ELEMENT.textSnippet)).toBeInTheDocument();
    expect(screen.getByText(`${SAMPLE_CONTEXT.viewport.width}×${SAMPLE_CONTEXT.viewport.height}`)).toBeInTheDocument();
    expect(screen.getByText(SAMPLE_CONTEXT.theme)).toBeInTheDocument();
    expect(screen.getByText(SAMPLE_CONTEXT.appVersion)).toBeInTheDocument();
    expect(screen.getByText(SAMPLE_CONTEXT.userAgent)).toBeInTheDocument();
    expect(screen.getByText('SnapDOM timed out for page capture')).toBeInTheDocument();
    expect(screen.getByText(/length limit and was truncated/i)).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText('title'), { target: { value: 'Updated title' } });
    expect(props.onUpdateDraft).toHaveBeenCalledWith({ title: 'Updated title' });

    fireEvent.change(screen.getByLabelText('description (required)'), { target: { value: 'Updated body' } });
    expect(props.onUpdateDraft).toHaveBeenCalledWith({ description: 'Updated body' });

    fireEvent.click(screen.getByRole('button', { name: 'pick an element' }));
    expect(props.onReopenPicker).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole('button', { name: 'back' }));
    expect(props.onBack).toHaveBeenCalledTimes(1);
  });
});
