import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  ALL_CAPTURE_KINDS,
  ALL_CLIPBOARD_OUTCOMES,
  ALL_FEEDBACK_KINDS,
  ALL_MASK_MODES,
  CaptureKind,
  ClipboardOutcome,
  FeedbackEventType,
  FeedbackKind,
  FeedbackStep,
  MaskMode,
  type CaptureArtifact,
  type FeedbackEvent,
  type FeedbackState,
  type PickedElementInfo,
} from '@/lib/feedback/types';
import {
  canSubmitFeedback,
  createInitialFeedbackState,
  feedbackReducer,
} from '@/lib/feedback/stateMachine';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

const REQUIRED_CASES = [
  'open-menu-from-closed',
  'select-problem-opens-attach',
  'attach-none-skips-straight-to-compose',
  'attach-page-only-enters-capturing',
  'attach-element-enters-picking',
  'pick-cancel-returns-to-attach',
  'capture-failure-still-opens-compose',
  'unmask-triggers-recapture',
  'change-kind-preserves-draft',
  'close-resets-state',
] as const;

const SAMPLE_ELEMENT: PickedElementInfo = {
  selector: 'main > button.primary',
  label: 'button submit',
  section: 'main',
  textSnippet: 'Submit',
  box: { top: 10, left: 20, width: 80, height: 32 },
};

function findByWire<T extends { readonly wire: string }>(
  list: readonly T[],
  wire: unknown,
  label: string,
): T {
  if (typeof wire !== 'string') {
    throw new Error(`${label} must be a string wire value`);
  }
  const found = list.find((item) => item.wire === wire);
  if (!found) {
    throw new Error(`${label} has unknown wire value ${JSON.stringify(wire)}`);
  }
  return found;
}

function makeCapture(kind: CaptureKind, maskMode: MaskMode = MaskMode.Masked): CaptureArtifact {
  return {
    kind,
    blob: new Blob(['png'], { type: 'image/png' }),
    width: 100,
    height: 80,
    maskMode,
  };
}

function parseEvent(raw: unknown, path: string, maskMode: MaskMode): FeedbackEvent {
  const row = requireRecord(raw, path);
  if (typeof row.type !== 'string') {
    throw new Error(`${path}.type must be a string`);
  }
  switch (row.type) {
    case FeedbackEventType.OpenMenu:
      requireExactFields(row, ['type'], path);
      return { type: FeedbackEventType.OpenMenu };
    case FeedbackEventType.CloseAll:
      requireExactFields(row, ['type'], path);
      return { type: FeedbackEventType.CloseAll };
    case FeedbackEventType.SelectKind:
      requireExactRequiredFields(row, ['type', 'kind'], path);
      return {
        type: FeedbackEventType.SelectKind,
        kind: findByWire(ALL_FEEDBACK_KINDS, row.kind, `${path}.kind`),
      };
    case FeedbackEventType.ChangeKind:
      requireExactRequiredFields(row, ['type', 'kind'], path);
      return {
        type: FeedbackEventType.ChangeKind,
        kind: findByWire(ALL_FEEDBACK_KINDS, row.kind, `${path}.kind`),
      };
    case FeedbackEventType.ToggleAttachElement:
      requireExactFields(row, ['type'], path);
      return { type: FeedbackEventType.ToggleAttachElement };
    case FeedbackEventType.ToggleAttachPage:
      requireExactFields(row, ['type'], path);
      return { type: FeedbackEventType.ToggleAttachPage };
    case FeedbackEventType.ProceedFromAttach:
      requireExactFields(row, ['type'], path);
      return { type: FeedbackEventType.ProceedFromAttach };
    case FeedbackEventType.BackToAttach:
      requireExactFields(row, ['type'], path);
      return { type: FeedbackEventType.BackToAttach };
    case FeedbackEventType.PickConfirmed:
      requireExactFields(row, ['type'], path);
      return { type: FeedbackEventType.PickConfirmed, element: SAMPLE_ELEMENT };
    case FeedbackEventType.PickCancelled:
      requireExactFields(row, ['type'], path);
      return { type: FeedbackEventType.PickCancelled };
    case FeedbackEventType.CaptureSucceeded: {
      requireExactRequiredFields(row, ['type', 'captures'], path);
      if (!Array.isArray(row.captures)) throw new Error(`${path}.captures must be an array`);
      const captures = row.captures.map((item, idx) =>
        makeCapture(findByWire(ALL_CAPTURE_KINDS, item, `${path}.captures[${idx}]`), maskMode),
      );
      return { type: FeedbackEventType.CaptureSucceeded, captures };
    }
    case FeedbackEventType.CaptureFailed: {
      requireExactFields(row, ['type', 'error', 'partialCaptures'], path);
      if (typeof row.error !== 'string') throw new Error(`${path}.error must be a string`);
      let partialCaptures: CaptureArtifact[] | undefined;
      if (row.partialCaptures !== undefined) {
        if (!Array.isArray(row.partialCaptures)) throw new Error(`${path}.partialCaptures must be an array`);
        partialCaptures = row.partialCaptures.map((item, idx) =>
          makeCapture(findByWire(ALL_CAPTURE_KINDS, item, `${path}.partialCaptures[${idx}]`), maskMode),
        );
      }
      return { type: FeedbackEventType.CaptureFailed, error: row.error, partialCaptures };
    }
    case FeedbackEventType.RemoveCapture:
      requireExactRequiredFields(row, ['type', 'kind'], path);
      return {
        type: FeedbackEventType.RemoveCapture,
        kind: findByWire(ALL_CAPTURE_KINDS, row.kind, `${path}.kind`),
      };
    case FeedbackEventType.SetMaskMode:
      requireExactRequiredFields(row, ['type', 'maskMode'], path);
      return {
        type: FeedbackEventType.SetMaskMode,
        maskMode: findByWire(ALL_MASK_MODES, row.maskMode, `${path}.maskMode`),
      };
    case FeedbackEventType.UpdateDraft:
      requireExactFields(row, ['type', 'title', 'description'], path);
      return {
        type: FeedbackEventType.UpdateDraft,
        title: typeof row.title === 'string' ? row.title : undefined,
        description: typeof row.description === 'string' ? row.description : undefined,
      };
    case FeedbackEventType.SubmitSucceeded:
      requireExactRequiredFields(row, ['type', 'url', 'clipboardOutcome'], path);
      if (typeof row.url !== 'string') throw new Error(`${path}.url must be a string`);
      return {
        type: FeedbackEventType.SubmitSucceeded,
        url: row.url,
        clipboardOutcome: findByWire(ALL_CLIPBOARD_OUTCOMES, row.clipboardOutcome, `${path}.clipboardOutcome`),
      };
    case FeedbackEventType.ReopenPicker:
      requireExactFields(row, ['type'], path);
      return { type: FeedbackEventType.ReopenPicker };
    default:
      throw new Error(`${path}.type has unsupported event ${JSON.stringify(row.type)}`);
  }
}

interface StateAssertion {
  readonly step?: string;
  readonly kind?: FeedbackKind;
  readonly attachElement?: boolean;
  readonly attachPage?: boolean;
  readonly pickedElement?: null;
  readonly maskMode?: MaskMode;
  readonly captures?: readonly CaptureKind[];
  readonly captureError?: string | null;
  readonly title?: string;
  readonly description?: string;
  readonly submittedUrl?: string | null;
  readonly clipboardOutcome?: ClipboardOutcome;
}

function parseExpectation(raw: unknown, path: string): StateAssertion {
  const row = requireRecord(raw, path);
  requireExactFields(
    row,
    [
      'step',
      'kind',
      'attachElement',
      'attachPage',
      'pickedElement',
      'maskMode',
      'captures',
      'captureError',
      'title',
      'description',
      'submittedUrl',
      'clipboardOutcome',
    ],
    path,
  );
  let captures: CaptureKind[] | undefined;
  if (row.captures !== undefined) {
    if (!Array.isArray(row.captures)) throw new Error(`${path}.captures must be an array`);
    captures = row.captures.map((item, idx) => findByWire(ALL_CAPTURE_KINDS, item, `${path}.captures[${idx}]`));
  }
  return {
    step: typeof row.step === 'string' ? row.step : undefined,
    kind: row.kind !== undefined ? findByWire(ALL_FEEDBACK_KINDS, row.kind, `${path}.kind`) : undefined,
    attachElement: typeof row.attachElement === 'boolean' ? row.attachElement : undefined,
    attachPage: typeof row.attachPage === 'boolean' ? row.attachPage : undefined,
    pickedElement: row.pickedElement === null ? null : undefined,
    maskMode: row.maskMode !== undefined ? findByWire(ALL_MASK_MODES, row.maskMode, `${path}.maskMode`) : undefined,
    captures,
    captureError: row.captureError === null || typeof row.captureError === 'string' ? row.captureError : undefined,
    title: typeof row.title === 'string' ? row.title : undefined,
    description: typeof row.description === 'string' ? row.description : undefined,
    submittedUrl: row.submittedUrl === null || typeof row.submittedUrl === 'string' ? row.submittedUrl : undefined,
    clipboardOutcome:
      row.clipboardOutcome !== undefined
        ? findByWire(ALL_CLIPBOARD_OUTCOMES, row.clipboardOutcome, `${path}.clipboardOutcome`)
        : undefined,
  };
}

function loadFixture() {
  const path = resolve(process.cwd(), 'src/lib/feedback/testdata/state_machine_cases.yaml');
  const root = requireRecord(parseStrictYAML(readFileSync(path, 'utf8'), 'state machine cases'), 'state machine cases');
  requireExactRequiredFields(root, ['cases'], 'state machine cases');
  if (!Array.isArray(root.cases)) throw new Error('state machine cases.cases must be a list');
  const rows = root.cases.map((value, index) => requireRecord(value, `cases[${index}]`));
  requireUniqueNames(rows, 'state machine cases.cases');
  const names = new Set(rows.map((row) => row.name));
  const missing = REQUIRED_CASES.filter((name) => !names.has(name));
  if (missing.length > 0) {
    throw new Error(`state machine cases missing required cases: ${missing.join(', ')}`);
  }

  return rows.map((row, index) => {
    const casePath = `cases[${index}]`;
    requireExactRequiredFields(row, ['name', 'steps'], casePath);
    if (!Array.isArray(row.steps) || row.steps.length === 0) {
      throw new Error(`${casePath}.steps must be a non-empty list`);
    }
    const steps = row.steps.map((rawStep, stepIndex) => {
      const stepPath = `${casePath}.steps[${stepIndex}]`;
      const step = requireRecord(rawStep, stepPath);
      requireExactFields(step, ['event', 'sameState', 'expect'], stepPath);
      if (!('event' in step)) throw new Error(`${stepPath} must have event`);
      return {
        rawEvent: step.event,
        sameState: step.sameState === true,
        expect: 'expect' in step ? parseExpectation(step.expect, `${stepPath}.expect`) : undefined,
        stepPath,
      };
    });
    if (!steps.some((step) => step.expect !== undefined)) {
      throw new Error(`${casePath} must assert at least one expectation`);
    }
    return { name: row.name as string, steps };
  });
}

function assertStateMatches(state: FeedbackState, expected: StateAssertion) {
  if (expected.step !== undefined) expect(state.step).toBe(expected.step);
  if (expected.kind !== undefined) expect(state.kind).toBe(expected.kind);
  if (expected.attachElement !== undefined) expect(state.attachElement).toBe(expected.attachElement);
  if (expected.attachPage !== undefined) expect(state.attachPage).toBe(expected.attachPage);
  if (expected.pickedElement === null) expect(state.pickedElement).toBeNull();
  if (expected.maskMode !== undefined) expect(state.maskMode).toBe(expected.maskMode);
  if (expected.captures !== undefined) {
    expect(state.captures.map((c) => c.kind)).toEqual(expected.captures);
  }
  if (expected.captureError !== undefined) expect(state.captureError).toBe(expected.captureError);
  if (expected.title !== undefined) expect(state.title).toBe(expected.title);
  if (expected.description !== undefined) expect(state.description).toBe(expected.description);
  if (expected.submittedUrl !== undefined) expect(state.submittedUrl).toBe(expected.submittedUrl);
  if (expected.clipboardOutcome !== undefined) expect(state.clipboardOutcome).toBe(expected.clipboardOutcome);
}

const fixtureCases = loadFixture();

describe('feedback stateMachine', () => {
  it.each(fixtureCases.map((c) => [c.name, c.steps] as const))('%s', (_name, steps) => {
    let state = createInitialFeedbackState();
    for (const step of steps) {
      const event = parseEvent(step.rawEvent, `${step.stepPath}.event`, state.maskMode);
      const next = feedbackReducer(state, event);
      if (step.sameState) {
        expect(next).toBe(state);
      }
      if (step.expect) {
        assertStateMatches(next, step.expect);
      }
      state = next;
    }
  });

  it('canSubmitFeedback requires compose step and non-whitespace description', () => {
    let state = createInitialFeedbackState();
    expect(canSubmitFeedback(state)).toBe(false);

    state = feedbackReducer(state, { type: FeedbackEventType.OpenMenu });
    state = feedbackReducer(state, { type: FeedbackEventType.SelectKind, kind: FeedbackKind.Problem });
    state = feedbackReducer(state, { type: FeedbackEventType.ProceedFromAttach });
    expect(state.step).toBe(FeedbackStep.Compose);
    expect(canSubmitFeedback(state)).toBe(false);

    state = feedbackReducer(state, { type: FeedbackEventType.UpdateDraft, description: '   \n\t  ' });
    expect(canSubmitFeedback(state)).toBe(false);

    state = feedbackReducer(state, { type: FeedbackEventType.UpdateDraft, description: 'Button overlaps label' });
    expect(canSubmitFeedback(state)).toBe(true);

    state = feedbackReducer(state, {
      type: FeedbackEventType.SubmitSucceeded,
      url: 'https://github.com/peasant-labs/peasant/issues/new',
      clipboardOutcome: ClipboardOutcome.NotNeeded,
    });
    expect(canSubmitFeedback(state)).toBe(false);
  });

  it('returns the exact same state object for invalid events across all steps', () => {
    const initial = createInitialFeedbackState();
    expect(feedbackReducer(initial, { type: FeedbackEventType.SelectKind, kind: FeedbackKind.Feature })).toBe(initial);

    const menu = feedbackReducer(initial, { type: FeedbackEventType.OpenMenu });
    expect(feedbackReducer(menu, { type: FeedbackEventType.ProceedFromAttach })).toBe(menu);

    const attach = feedbackReducer(menu, { type: FeedbackEventType.SelectKind, kind: FeedbackKind.Problem });
    expect(feedbackReducer(attach, { type: FeedbackEventType.PickCancelled })).toBe(attach);

    const picking = feedbackReducer(
      feedbackReducer(attach, { type: FeedbackEventType.ToggleAttachElement }),
      { type: FeedbackEventType.ProceedFromAttach },
    );
    expect(feedbackReducer(picking, { type: FeedbackEventType.CloseAll })).toBe(picking);
    expect(feedbackReducer(picking, { type: FeedbackEventType.SubmitSucceeded, url: 'x', clipboardOutcome: ClipboardOutcome.NotNeeded })).toBe(picking);

    const capturing = feedbackReducer(picking, { type: FeedbackEventType.PickConfirmed, element: SAMPLE_ELEMENT });
    expect(feedbackReducer(capturing, { type: FeedbackEventType.UpdateDraft, title: 'x' })).toBe(capturing);

    const compose = feedbackReducer(capturing, { type: FeedbackEventType.CaptureSucceeded, captures: [] });
    expect(feedbackReducer(compose, { type: FeedbackEventType.OpenMenu })).toBe(compose);

    const submitted = feedbackReducer(compose, {
      type: FeedbackEventType.SubmitSucceeded,
      url: 'https://github.com/peasant-labs/peasant/issues/new',
      clipboardOutcome: ClipboardOutcome.NotNeeded,
    });
    expect(feedbackReducer(submitted, { type: FeedbackEventType.UpdateDraft, title: 'edited' })).toBe(submitted);
  });
});
