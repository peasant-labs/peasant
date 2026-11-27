/** Pure state machine for the in-app feedback flow. */

import {
  ClipboardOutcome,
  FeedbackEventType,
  FeedbackKind,
  FeedbackStep,
  MaskMode,
  type FeedbackEvent,
  type FeedbackState,
} from '@/lib/feedback/types';

function assertNever(value: never): never {
  throw new Error(`Unhandled feedback state: ${String(value)}`);
}

export function createInitialFeedbackState(): FeedbackState {
  return {
    step: FeedbackStep.Closed,
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
  };
}

export function canSubmitFeedback(state: FeedbackState): boolean {
  return state.step === FeedbackStep.Compose && state.description.trim().length > 0;
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
          if (state.attachElement) {
            return {
              ...state,
              step: FeedbackStep.Picking,
              captureError: null,
            };
          }
          if (state.attachPage) {
            return {
              ...state,
              step: FeedbackStep.Capturing,
              pickedElement: null,
              captureError: null,
            };
          }
          return {
            ...state,
            step: FeedbackStep.Compose,
            pickedElement: null,
            captures: [],
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
          return { ...state, step: FeedbackStep.AttachOptions };
        case FeedbackEventType.PickConfirmed:
          return {
            ...state,
            step: FeedbackStep.Capturing,
            pickedElement: event.element,
            captureError: null,
          };
        default:
          return state;
      }
    }

    case FeedbackStep.Capturing: {
      switch (event.type) {
        case FeedbackEventType.CaptureSucceeded:
          return {
            ...state,
            step: FeedbackStep.Compose,
            captures: event.captures,
            captureError: null,
          };
        case FeedbackEventType.CaptureFailed:
          return {
            ...state,
            step: FeedbackStep.Compose,
            captures: event.partialCaptures ?? [],
            captureError: event.error,
          };
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
            title: event.title !== undefined ? event.title : state.title,
            description: event.description !== undefined ? event.description : state.description,
          };
        case FeedbackEventType.RemoveCapture: {
          const remaining = state.captures.filter((artifact) => artifact.kind !== event.kind);
          if (remaining.length === state.captures.length) {
            return state;
          }
          return { ...state, captures: remaining };
        }
        case FeedbackEventType.SetMaskMode: {
          if (state.captures.length === 0) {
            return state.maskMode === event.maskMode ? state : { ...state, maskMode: event.maskMode };
          }
          return {
            ...state,
            step: FeedbackStep.Capturing,
            maskMode: event.maskMode,
            captureError: null,
          };
        }
        case FeedbackEventType.ReopenPicker:
          return {
            ...state,
            step: FeedbackStep.Picking,
            attachElement: true,
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
          };
        default:
          return state;
      }
    }

    case FeedbackStep.Submitted: {
      if (event.type === FeedbackEventType.CloseAll) {
        return createInitialFeedbackState();
      }
      return state;
    }

    default:
      return assertNever(state.step);
  }
}
