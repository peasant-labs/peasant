/** Pure state machine for the in-app feedback flow. */

import {
  CaptureKind,
  ClipboardOutcome,
  FeedbackEventType,
  FeedbackKind,
  FeedbackStep,
  MAX_PICKED_ELEMENTS,
  MaskMode,
  PendingCaptureKind,
  type CaptureArtifact,
  type FeedbackEvent,
  type FeedbackState,
  type PickedElementInfo,
} from '@/lib/feedback/types';

function assertNever(value: never): never {
  throw new Error(`Unhandled feedback state: ${String(value)}`);
}

function orderCaptures(
  captures: readonly CaptureArtifact[],
  pickedElements: readonly PickedElementInfo[],
): readonly CaptureArtifact[] {
  const elementOrder = new Map<number, number>();
  pickedElements.forEach((el, index) => elementOrder.set(el.id, index));

  const elementCaptures: CaptureArtifact[] = [];
  let pageCapture: CaptureArtifact | null = null;

  for (const artifact of captures) {
    if (artifact.kind === CaptureKind.Page) {
      pageCapture = artifact;
    } else {
      elementCaptures.push(artifact);
    }
  }

  elementCaptures.sort((a, b) => {
    const orderA = a.elementId !== null ? (elementOrder.get(a.elementId) ?? Number.MAX_SAFE_INTEGER) : Number.MAX_SAFE_INTEGER;
    const orderB = b.elementId !== null ? (elementOrder.get(b.elementId) ?? Number.MAX_SAFE_INTEGER) : Number.MAX_SAFE_INTEGER;
    return orderA - orderB || a.id - b.id;
  });

  return pageCapture ? [...elementCaptures, pageCapture] : elementCaptures;
}

function assignMonotonicIds(
  incoming: readonly CaptureArtifact[],
  startingId: number,
  reservedIds: ReadonlySet<number>,
): { artifacts: CaptureArtifact[]; nextCaptureId: number } {
  let nextId = startingId;
  const used = new Set(reservedIds);
  const artifacts = incoming.map((artifact) => {
    let id = artifact.id;
    if (id < nextId || used.has(id)) {
      while (used.has(nextId)) {
        nextId += 1;
      }
      id = nextId;
      nextId += 1;
    } else {
      nextId = Math.max(nextId, id + 1);
    }
    used.add(id);
    return id === artifact.id ? artifact : { ...artifact, id };
  });
  return { artifacts, nextCaptureId: nextId };
}

function applyCaptureBatch(state: FeedbackState, rawIncoming: readonly CaptureArtifact[]): {
  captures: readonly CaptureArtifact[];
  nextCaptureId: number;
} {
  const replaceAll = state.pendingCapture?.kind === PendingCaptureKind.All;
  const reservedIds = new Set(replaceAll ? [] : state.captures.map((c) => c.id));
  const { artifacts, nextCaptureId } = assignMonotonicIds(rawIncoming, state.nextCaptureId, reservedIds);

  if (replaceAll) {
    return {
      captures: orderCaptures(artifacts, state.pickedElements),
      nextCaptureId,
    };
  }

  const merged = [...state.captures];
  for (const artifact of artifacts) {
    const existingIndex =
      artifact.kind === CaptureKind.Page
        ? merged.findIndex((item) => item.kind === CaptureKind.Page)
        : merged.findIndex(
            (item) => item.kind === CaptureKind.Element && item.elementId === artifact.elementId,
          );
    if (existingIndex !== -1) {
      merged[existingIndex] = artifact;
    } else {
      merged.push(artifact);
    }
  }

  return {
    captures: orderCaptures(merged, state.pickedElements),
    nextCaptureId,
  };
}

export function createInitialFeedbackState(): FeedbackState {
  return {
    step: FeedbackStep.Closed,
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
  };
}

export function canSubmitFeedback(state: FeedbackState): boolean {
  return state.step === FeedbackStep.Compose;
}

export function feedbackReducer(state: FeedbackState, event: FeedbackEvent): FeedbackState {
  switch (state.step) {
    case FeedbackStep.Closed: {
      if (event.type === FeedbackEventType.OpenMenu) {
        return { ...state, step: FeedbackStep.MenuOpen };
      }
      return state;
    }

    case FeedbackStep.MenuOpen: {
      switch (event.type) {
        case FeedbackEventType.CloseAll:
          return createInitialFeedbackState();
        case FeedbackEventType.SelectKind:
          return {
            ...state,
            step: FeedbackStep.AttachOptions,
            kind: event.kind,
          };
        default:
          return state;
      }
    }

    case FeedbackStep.AttachOptions: {
      switch (event.type) {
        case FeedbackEventType.CloseAll:
          return createInitialFeedbackState();
        case FeedbackEventType.ToggleAttachElement:
          return { ...state, attachElement: !state.attachElement };
        case FeedbackEventType.ToggleAttachPage:
          return { ...state, attachPage: !state.attachPage };
        case FeedbackEventType.ProceedFromAttach: {
          if (state.attachElement && state.pickedElements.length < MAX_PICKED_ELEMENTS) {
            return {
              ...state,
              step: FeedbackStep.Picking,
              pickReturnStep: FeedbackStep.AttachOptions,
              captureError: null,
            };
          }
          if (state.attachPage || state.pickedElements.length > 0) {
            return {
              ...state,
              step: FeedbackStep.Capturing,
              pendingCapture: { kind: PendingCaptureKind.All },
              captureError: null,
            };
          }
          return {
            ...state,
            step: FeedbackStep.Compose,
            pickedElements: [],
            captures: [],
            pendingCapture: null,
            captureError: null,
          };
        }
        default:
          return state;
      }
    }

    case FeedbackStep.Picking: {
      switch (event.type) {
        case FeedbackEventType.PickCancelled:
          return { ...state, step: state.pickReturnStep };
        case FeedbackEventType.PickConfirmed: {
          if (state.pickedElements.length >= MAX_PICKED_ELEMENTS) {
            return { ...state, step: state.pickReturnStep };
          }
          const assignedElement: PickedElementInfo = {
            ...event.element,
            id: state.nextElementId,
          };
          const hasPageCapture = state.captures.some((c) => c.kind === CaptureKind.Page);
          const includePage = state.attachPage && !hasPageCapture;
          return {
            ...state,
            step: FeedbackStep.Capturing,
            pickedElements: [...state.pickedElements, assignedElement],
            nextElementId: state.nextElementId + 1,
            pendingCapture: {
              kind: PendingCaptureKind.Element,
              elementId: assignedElement.id,
              includePage,
            },
            captureError: null,
          };
        }
        default:
          return state;
      }
    }

    case FeedbackStep.Capturing: {
      switch (event.type) {
        case FeedbackEventType.CaptureSucceeded: {
          const { captures, nextCaptureId } = applyCaptureBatch(state, event.captures);
          return {
            ...state,
            step: FeedbackStep.Compose,
            captures,
            nextCaptureId,
            pendingCapture: null,
            captureError: null,
          };
        }
        case FeedbackEventType.CaptureFailed: {
          const { captures, nextCaptureId } = applyCaptureBatch(state, event.partialCaptures ?? []);
          return {
            ...state,
            step: FeedbackStep.Compose,
            captures,
            nextCaptureId,
            pendingCapture: null,
            captureError: event.error,
          };
        }
        default:
          return state;
      }
    }

    case FeedbackStep.Compose: {
      switch (event.type) {
        case FeedbackEventType.CloseAll:
          return createInitialFeedbackState();
        case FeedbackEventType.ChangeKind:
          return { ...state, kind: event.kind };
        case FeedbackEventType.UpdateDraft:
          return {
            ...state,
            title: event.title,
          };
        case FeedbackEventType.RemoveCapture: {
          const target = state.captures.find((artifact) => artifact.id === event.id);
          if (!target) {
            return state;
          }
          const remainingCaptures = state.captures.filter((artifact) => artifact.id !== event.id);
          const remainingElements =
            target.kind === CaptureKind.Element && target.elementId !== null
              ? state.pickedElements.filter((el) => el.id !== target.elementId)
              : state.pickedElements;
          const nextAttachPage = target.kind === CaptureKind.Page ? false : state.attachPage;
          return {
            ...state,
            captures: remainingCaptures,
            pickedElements: remainingElements,
            attachPage: nextAttachPage,
          };
        }
        case FeedbackEventType.SetMaskMode: {
          if (state.captures.length === 0) {
            return state.maskMode === event.maskMode ? state : { ...state, maskMode: event.maskMode };
          }
          return {
            ...state,
            step: FeedbackStep.Capturing,
            maskMode: event.maskMode,
            pendingCapture: { kind: PendingCaptureKind.All },
            captureError: null,
          };
        }
        case FeedbackEventType.ReopenPicker:
          if (state.pickedElements.length >= MAX_PICKED_ELEMENTS) {
            return state;
          }
          return {
            ...state,
            step: FeedbackStep.Picking,
            attachElement: true,
            pickReturnStep: FeedbackStep.Compose,
            captureError: null,
          };
        case FeedbackEventType.BackToAttach:
          return { ...state, step: FeedbackStep.AttachOptions };
        case FeedbackEventType.SubmitSucceeded:
          return {
            ...state,
            step: FeedbackStep.Submitted,
            submittedUrl: event.url,
            clipboardOutcome: event.clipboardOutcome,
            copiedCaptureId:
              event.clipboardOutcome === ClipboardOutcome.Copied
                ? (event.copiedCaptureId ?? state.captures[0]?.id ?? null)
                : null,
          };
        default:
          return state;
      }
    }

    case FeedbackStep.Submitted: {
      switch (event.type) {
        case FeedbackEventType.CloseAll:
          return createInitialFeedbackState();
        case FeedbackEventType.CopyCaptureSettled:
          return {
            ...state,
            clipboardOutcome: event.clipboardOutcome,
            copiedCaptureId: event.clipboardOutcome === ClipboardOutcome.Copied ? event.captureId : null,
          };
        default:
          return state;
      }
    }

    default:
      return assertNever(state.step);
  }
}
