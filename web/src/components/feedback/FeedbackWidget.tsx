'use client';

import { useCallback, useEffect, useMemo, useReducer, useRef, useState, type ReactElement } from 'react';
import { AttachOptionsDialog } from '@/components/feedback/AttachOptionsDialog';
import { ComposeFeedbackDialog } from '@/components/feedback/ComposeFeedbackDialog';
import { ElementPickerOverlay } from '@/components/feedback/ElementPickerOverlay';
import { FeedbackLauncher } from '@/components/feedback/FeedbackLauncher';
import { useConnectionState } from '@/contexts/WebSocketContext';
import { useTheme } from '@/hooks/useTheme';
import { captureFeedbackArtifacts, copyPngToClipboard, downloadPngBlob } from '@/lib/feedback/capture';
import { createBrowserPickerAdapter, feedbackDisabled } from '@/lib/feedback/domTarget';
import { buildGitHubIssueUrl } from '@/lib/feedback/issueUrl';
import { isInsideMaskedElement } from '@/lib/feedback/masking';
import { canSubmitFeedback, createInitialFeedbackState, feedbackReducer } from '@/lib/feedback/stateMachine';
import {
  CaptureKind,
  ClipboardOutcome,
  FeedbackEventType,
  FeedbackStep,
  MaskMode,
  type CaptureArtifact,
  type FeedbackContextInfo,
  type FeedbackKind,
  type PickedElementInfo,
} from '@/lib/feedback/types';

function readFeedbackContext(theme: string, serverVersion: string | null): FeedbackContextInfo {
  const domTheme = typeof document !== 'undefined' ? document.documentElement.dataset.theme : undefined;
  return {
    route: typeof window !== 'undefined' ? window.location.pathname : '/',
    viewport: {
      width: typeof window !== 'undefined' ? window.innerWidth : 0,
      height: typeof window !== 'undefined' ? window.innerHeight : 0,
    },
    theme: domTheme || theme,
    appVersion: serverVersion ?? 'unknown',
    userAgent: typeof navigator !== 'undefined' ? navigator.userAgent : '',
  };
}

function primaryCaptureToCopy(captures: readonly CaptureArtifact[]): CaptureArtifact | null {
  return (
    captures.find((artifact) => artifact.kind === CaptureKind.Element) ??
    captures.find((artifact) => artifact.kind === CaptureKind.Page) ??
    null
  );
}

function captureFilename(feedbackKind: FeedbackKind, captureKind: CaptureKind): string {
  return `peasant-feedback-${feedbackKind.wire}-${captureKind.wire}.png`;
}

/**
 * Peasant in-app feedback widget host: owns the flow reducer, browser context,
 * lazy screenshot capture, and synchronous clipboard + GitHub issue handoff.
 * Deferred until after mount and omitted under `?fb=off`.
 */
export function FeedbackWidget(): ReactElement | null {
  const [enabled, setEnabled] = useState(false);
  const [state, dispatch] = useReducer(feedbackReducer, undefined, createInitialFeedbackState);
  const { theme } = useTheme();
  const { serverVersion } = useConnectionState();

  const launcherRef = useRef<HTMLButtonElement | null>(null);
  const pickedElementRef = useRef<Element | null>(null);
  const mountedRef = useRef(true);
  const submittingRef = useRef(false);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  useEffect(() => {
    const sync = () => setEnabled(!feedbackDisabled(window.location.search));
    sync();
    window.addEventListener('popstate', sync);
    return () => window.removeEventListener('popstate', sync);
  }, []);

  useEffect(() => {
    if (state.step !== FeedbackStep.Compose) {
      submittingRef.current = false;
    }
  }, [state.step]);

  useEffect(() => {
    if (!enabled || state.step !== FeedbackStep.Capturing) return;
    let cancelled = false;
    const targetElement = state.pickedElement ? pickedElementRef.current : null;

    void captureFeedbackArtifacts({
      element: targetElement,
      capturePage: state.attachPage,
      maskMode: state.maskMode,
    })
      .then((result) => {
        if (cancelled) return;
        if (result.errors.length > 0) {
          dispatch({
            type: FeedbackEventType.CaptureFailed,
            error: result.errors.join('; '),
            partialCaptures: result.captures,
          });
          return;
        }
        dispatch({
          type: FeedbackEventType.CaptureSucceeded,
          captures: result.captures,
        });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        dispatch({
          type: FeedbackEventType.CaptureFailed,
          error: err instanceof Error && err.message ? err.message : 'Screenshot capture failed',
        });
      });

    return () => {
      cancelled = true;
    };
  }, [enabled, state.step, state.pickedElement, state.attachPage, state.maskMode]);

  const pickerAdapter = useMemo(
    () => createBrowserPickerAdapter((el) => isInsideMaskedElement(el, state.maskMode)),
    [state.maskMode],
  );

  const handleCloseAll = useCallback(() => {
    pickedElementRef.current = null;
    dispatch({ type: FeedbackEventType.CloseAll });
  }, []);

  const handleLauncherOpenChange = useCallback((open: boolean) => {
    if (open) {
      dispatch({ type: FeedbackEventType.OpenMenu });
      return;
    }
    dispatch({ type: FeedbackEventType.CloseAll });
  }, []);

  const handleSelectKind = useCallback((kind: FeedbackKind) => {
    dispatch({ type: FeedbackEventType.SelectKind, kind });
  }, []);

  const handleProceedFromAttach = useCallback(() => {
    if (!state.attachElement) {
      pickedElementRef.current = null;
    }
    dispatch({ type: FeedbackEventType.ProceedFromAttach });
  }, [state.attachElement]);

  const handlePickConfirm = useCallback((el: Element, info: PickedElementInfo) => {
    pickedElementRef.current = el;
    dispatch({ type: FeedbackEventType.PickConfirmed, element: info });
  }, []);

  const handlePickCancel = useCallback(() => {
    dispatch({ type: FeedbackEventType.PickCancelled });
  }, []);

  const handleRemoveCapture = useCallback((kind: CaptureKind) => {
    if (kind === CaptureKind.Element) {
      pickedElementRef.current = null;
    }
    dispatch({ type: FeedbackEventType.RemoveCapture, kind });
  }, []);

  const handleSubmit = useCallback(() => {
    if (!canSubmitFeedback(state) || submittingRef.current) return;
    submittingRef.current = true;

    const { url } = buildGitHubIssueUrl({
      kind: state.kind,
      title: state.title,
      description: state.description,
      element: state.pickedElement,
      context: readFeedbackContext(theme, serverVersion),
    });

    const capture = primaryCaptureToCopy(state.captures);
    // Start clipboard write synchronously before window.open so Safari keeps user-gesture activation.
    const clipboardPromise = capture ? copyPngToClipboard(capture.blob) : null;

    window.open(url, '_blank', 'noopener,noreferrer');

    if (!clipboardPromise) {
      submittingRef.current = false;
      dispatch({
        type: FeedbackEventType.SubmitSucceeded,
        url,
        clipboardOutcome: ClipboardOutcome.NotNeeded,
      });
      return;
    }

    void clipboardPromise
      .then((clipboardOutcome) => {
        if (!mountedRef.current) return;
        dispatch({
          type: FeedbackEventType.SubmitSucceeded,
          url,
          clipboardOutcome,
        });
      })
      .catch(() => {
        if (!mountedRef.current) return;
        dispatch({
          type: FeedbackEventType.SubmitSucceeded,
          url,
          clipboardOutcome: ClipboardOutcome.Failed,
        });
      })
      .finally(() => {
        submittingRef.current = false;
      });
  }, [state, theme, serverVersion]);

  const handleCopyCapture = useCallback(
    (kind: CaptureKind) => {
      const artifact = state.captures.find((item) => item.kind === kind);
      if (!artifact) return;
      void copyPngToClipboard(artifact.blob);
    },
    [state.captures],
  );

  const handleDownloadCapture = useCallback(
    (kind: CaptureKind) => {
      const artifact = state.captures.find((item) => item.kind === kind);
      if (!artifact) return;
      downloadPngBlob(artifact.blob, captureFilename(state.kind, kind));
    },
    [state.captures, state.kind],
  );

  if (!enabled) return null;

  const composeOpen =
    state.step === FeedbackStep.Capturing ||
    state.step === FeedbackStep.Compose ||
    state.step === FeedbackStep.Submitted;

  const context = readFeedbackContext(theme, serverVersion);
  const issuePreview = composeOpen
    ? buildGitHubIssueUrl({
        kind: state.kind,
        title: state.title,
        description: state.description,
        element: state.pickedElement,
        context,
      })
    : null;

  const launcherDisabled = state.step !== FeedbackStep.Closed && state.step !== FeedbackStep.MenuOpen;

  return (
    <div data-feedback-widget="">
      <FeedbackLauncher
        open={state.step === FeedbackStep.MenuOpen}
        onOpenChange={handleLauncherOpenChange}
        onSelectKind={handleSelectKind}
        disabled={launcherDisabled}
        launcherRef={launcherRef}
      />

      <AttachOptionsDialog
        open={state.step === FeedbackStep.AttachOptions}
        kind={state.kind}
        attachElement={state.attachElement}
        attachPage={state.attachPage}
        onToggleElement={() => dispatch({ type: FeedbackEventType.ToggleAttachElement })}
        onTogglePage={() => dispatch({ type: FeedbackEventType.ToggleAttachPage })}
        onProceed={handleProceedFromAttach}
        onClose={handleCloseAll}
        returnFocusRef={launcherRef}
      />

      <ElementPickerOverlay
        active={state.step === FeedbackStep.Picking}
        adapter={pickerAdapter}
        onConfirm={handlePickConfirm}
        onCancel={handlePickCancel}
      />

      <ComposeFeedbackDialog
        open={composeOpen}
        state={state}
        context={context}
        issuePreviewUrl={issuePreview?.url ?? ''}
        truncated={issuePreview?.truncated ?? false}
        onChangeKind={(kind) => dispatch({ type: FeedbackEventType.ChangeKind, kind })}
        onUpdateDraft={(patch) => dispatch({ type: FeedbackEventType.UpdateDraft, ...patch })}
        onRemoveCapture={handleRemoveCapture}
        onRequestUnmask={() => dispatch({ type: FeedbackEventType.SetMaskMode, maskMode: MaskMode.Unmasked })}
        onRemask={() => dispatch({ type: FeedbackEventType.SetMaskMode, maskMode: MaskMode.Masked })}
        onReopenPicker={() => dispatch({ type: FeedbackEventType.ReopenPicker })}
        onBack={() => dispatch({ type: FeedbackEventType.BackToAttach })}
        onSubmit={handleSubmit}
        onCopyCapture={handleCopyCapture}
        onDownloadCapture={handleDownloadCapture}
        onClose={handleCloseAll}
        returnFocusRef={launcherRef}
      />
    </div>
  );
}
