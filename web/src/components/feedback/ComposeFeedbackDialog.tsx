'use client';

import { useEffect, useId, useState, type ReactElement, type RefObject } from 'react';
import {
  AlertTriangle,
  CheckCircle2,
  Copy,
  Crosshair,
  Download,
  ExternalLink,
  Eye,
  EyeOff,
  Info,
  Trash2,
} from 'lucide-react';
import { Button, Chip, Dialog, Input, Segmented, Spinner } from '@/lib/ft-ui';
import {
  ALL_FEEDBACK_KINDS,
  ClipboardOutcome,
  FeedbackKind,
  FeedbackStep,
  MAX_PICKED_ELEMENTS,
  MaskMode,
  type CaptureArtifact,
  type FeedbackState,
} from '@/lib/feedback/types';
import './feedback-dialogs.css';

export interface ComposeFeedbackDialogProps {
  open: boolean;
  state: FeedbackState;
  issueBodyPreview: string;
  issuePreviewUrl: string;
  truncated: boolean;
  onChangeKind(kind: FeedbackKind): void;
  onUpdateDraft(patch: { title: string }): void;
  onRemoveCapture(id: number): void;
  onRequestUnmask(): void;
  onRemask(): void;
  onReopenPicker(): void;
  onBack(): void;
  onSubmit(): void;
  onCopyCapture(id: number): void;
  onDownloadCapture(id: number): void;
  onClose(): void;
  returnFocusRef?: RefObject<HTMLElement | null>;
}

interface CapturePreviewEntry {
  readonly artifact: CaptureArtifact;
  readonly url: string;
}

const TITLE_PLACEHOLDERS: Readonly<Record<FeedbackKind['wire'], string>> = Object.freeze({
  [FeedbackKind.Problem.wire]: 'Short summary of what went wrong',
  [FeedbackKind.Feature.wire]: 'Short summary of the improvement',
});

const OUTCOME_TONES: Readonly<Record<ClipboardOutcome['wire'], { readonly cls: string; readonly icon: typeof Info }>> =
  Object.freeze({
    [ClipboardOutcome.Copied.wire]: { cls: 'is-ok', icon: CheckCircle2 },
    [ClipboardOutcome.Downloaded.wire]: { cls: 'is-ok', icon: CheckCircle2 },
    [ClipboardOutcome.NotNeeded.wire]: { cls: '', icon: Info },
    [ClipboardOutcome.Failed.wire]: { cls: 'is-err', icon: AlertTriangle },
  });

const UNMASK_WARNING_TEXT =
  'This repository is public. Unmasking may reveal prompts, code, file paths, or secrets in your screenshot.';

/**
 * Wide feedback dialog rendering both the compose review step and the
 * post-submit confirmation/recovery step.
 */
export function ComposeFeedbackDialog({
  open,
  state,
  issueBodyPreview,
  issuePreviewUrl,
  truncated,
  onChangeKind,
  onUpdateDraft,
  onRemoveCapture,
  onRequestUnmask,
  onRemask,
  onReopenPicker,
  onBack,
  onSubmit,
  onCopyCapture,
  onDownloadCapture,
  onClose,
  returnFocusRef,
}: ComposeFeedbackDialogProps): ReactElement {
  const labelId = useId();
  const pickLimitReasonId = useId();
  const [confirmingUnmask, setConfirmingUnmask] = useState(false);
  const [previews, setPreviews] = useState<readonly CapturePreviewEntry[]>([]);

  useEffect(() => {
    if (!open || state.captures.length === 0 || typeof URL.createObjectURL !== 'function') {
      setPreviews([]);
      return;
    }
    const created = state.captures.map((artifact) => ({
      artifact,
      url: URL.createObjectURL(artifact.blob),
    }));
    setPreviews(created);
    return () => {
      if (typeof URL.revokeObjectURL !== 'function') return;
      for (const entry of created) {
        URL.revokeObjectURL(entry.url);
      }
    };
  }, [open, state.captures]);

  useEffect(() => {
    if (!open || state.maskMode === MaskMode.Unmasked || state.captures.length === 0) {
      setConfirmingUnmask(false);
    }
  }, [open, state.maskMode, state.captures.length]);

  const isSubmitted = state.step === FeedbackStep.Submitted;
  const isCapturing = state.step === FeedbackStep.Capturing;
  const atPickLimit = state.pickedElements.length >= MAX_PICKED_ELEMENTS;
  const pickButtonLabel = state.pickedElements.length > 0 ? 'pick another element' : 'pick an element';
  const pickLimitReason = `maximum ${MAX_PICKED_ELEMENTS} elements reached`;
  const canSubmit = !isCapturing;
  const dialogTitle = isSubmitted ? 'github issue opened' : state.kind.label;

  const outcomeDisplay = OUTCOME_TONES[state.clipboardOutcome.wire];
  const OutcomeIcon = outcomeDisplay.icon;
  const copiedIndex =
    state.copiedCaptureId !== null ? state.captures.findIndex((c) => c.id === state.copiedCaptureId) : -1;
  const clipboardMarkerText =
    copiedIndex !== -1
      ? state.captures.length > 1
        ? `image ${copiedIndex + 1} copied — paste it, then copy the next`
        : `image ${copiedIndex + 1} copied — paste it into the issue`
      : null;

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={dialogTitle}
      labelId={labelId}
      returnFocusRef={returnFocusRef}
      size="wide"
      dismissible={!isCapturing}
      className="fbw-compose-dialog"
      footer={
        isSubmitted ? (
          <Button type="button" variant="primary" onClick={onClose}>
            done
          </Button>
        ) : (
          <div className="fbw-footer-split">
            <Button type="button" variant="ghost" disabled={isCapturing} onClick={onBack}>
              back
            </Button>
            <div className="fbw-footer-actions">
              <Button type="button" variant="ghost" disabled={isCapturing} onClick={onClose}>
                cancel
              </Button>
              <Button
                type="button"
                variant="primary"
                disabled={!canSubmit}
                loading={isCapturing}
                icon={ExternalLink}
                onClick={onSubmit}
              >
                open github issue
              </Button>
            </div>
          </div>
        )
      }
    >
      {isSubmitted ? (
        <>
          <div className={`fbw-submitted-outcome ${outcomeDisplay.cls}`.trim()} role="status">
            <OutcomeIcon aria-hidden="true" />
            <span>{state.clipboardOutcome.label}</span>
          </div>

          <p className="fbw-dlg-prose">
            Paste your screenshot into the screenshot field on GitHub with Ctrl/Cmd+V, or drag and drop a downloaded
            PNG.
          </p>

          {clipboardMarkerText && (
            <div className="fbw-clipboard-marker" role="status">
              <CheckCircle2 aria-hidden="true" />
              <span>{clipboardMarkerText}</span>
            </div>
          )}

          {state.captures.length > 0 && (
            <div className="fbw-section">
              <span className="label">screenshots</span>
              <div className="fbw-captures-grid">
                {state.captures.map((capture, index) => {
                  const imageNumber = index + 1;
                  const previewUrl = previews.find((entry) => entry.artifact.id === capture.id)?.url;
                  const isOnClipboard = state.copiedCaptureId === capture.id;
                  return (
                    <div key={capture.id} className="fbw-capture-card">
                      {previewUrl && (
                        <img
                          src={previewUrl}
                          alt={`image ${imageNumber}: ${capture.kind.label} capture (${capture.maskMode.label})`}
                          className="fbw-capture-img"
                        />
                      )}
                      <div className="fbw-capture-meta">
                        <span className="fbw-capture-index">image {imageNumber}</span>
                        <span className="fbw-capture-label">{capture.kind.label}</span>
                        <span className="fbw-capture-dims tnum">
                          {capture.width}×{capture.height}
                        </span>
                        {isOnClipboard && (
                          <Chip chrome tone="ok" icon={CheckCircle2}>
                            on clipboard
                          </Chip>
                        )}
                      </div>
                      <div className="fbw-submitted-capture-actions">
                        <Button
                          type="button"
                          variant="secondary"
                          size="sm"
                          icon={Copy}
                          aria-label={`copy image ${imageNumber} (${capture.kind.label})`}
                          onClick={() => onCopyCapture(capture.id)}
                        >
                          copy image
                        </Button>
                        <Button
                          type="button"
                          variant="secondary"
                          size="sm"
                          icon={Download}
                          aria-label={`download image ${imageNumber} (${capture.kind.label})`}
                          onClick={() => onDownloadCapture(capture.id)}
                        >
                          download
                        </Button>
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          )}

          {state.submittedUrl && (
            <div className="fbw-fallback-row">
              <span className="fbw-dlg-prose">If the tab did not open:</span>
              <a
                href={state.submittedUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="link"
              >
                open github issue
              </a>
            </div>
          )}
        </>
      ) : (
        <>
          <div className="fbw-field-group">
            <span className="label">kind</span>
            <Segmented
              label="feedback kind"
              options={ALL_FEEDBACK_KINDS.map((kind) => ({
                value: kind.wire,
                label: kind.label,
              }))}
              value={state.kind.wire}
              onChange={(wire) => {
                const next = ALL_FEEDBACK_KINDS.find((kind) => kind.wire === wire);
                if (next && next !== state.kind) {
                  onChangeKind(next);
                }
              }}
            />
          </div>

          <Input
            label="title"
            value={state.title}
            disabled={isCapturing}
            onChange={(event) => onUpdateDraft({ title: event.target.value })}
            placeholder={TITLE_PLACEHOLDERS[state.kind.wire]}
          />

          <div className="fbw-section">
            <p className="fbw-dlg-prose">
              You will write the description on GitHub. The issue opens with this template filled in:
            </p>
            <pre className="fbw-template-preview" tabIndex={0} aria-label="issue template preview">
              {issueBodyPreview}
            </pre>
            {issuePreviewUrl && (
              <details className="fbw-url-details">
                <summary>preview github issue url</summary>
                <code>{issuePreviewUrl}</code>
              </details>
            )}
          </div>

          {isCapturing && (
            <div className="fbw-busy-notice">
              <Spinner>capturing screenshot</Spinner>
            </div>
          )}

          {state.captureError && (
            <div className="fbw-error-notice" role="alert">
              <AlertTriangle aria-hidden="true" />
              <span>{state.captureError}</span>
            </div>
          )}

          <div className="fbw-section">
            <div className="fbw-section-head">
              <span className="label">screenshots and element</span>
              <div className="fbw-section-actions">
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  icon={Crosshair}
                  disabled={isCapturing || atPickLimit}
                  aria-describedby={atPickLimit ? pickLimitReasonId : undefined}
                  title={atPickLimit ? pickLimitReason : undefined}
                  onClick={onReopenPicker}
                >
                  {pickButtonLabel}
                </Button>
                {atPickLimit && (
                  <span id={pickLimitReasonId} className="fbw-capture-dims">
                    {pickLimitReason}
                  </span>
                )}
              </div>
            </div>

            {state.captures.length > 0 && (
              <>
                <div className="fbw-unmask-bar">
                  <Chip
                    chrome
                    tone={state.maskMode === MaskMode.Unmasked ? 'warn' : undefined}
                    icon={state.maskMode === MaskMode.Masked ? EyeOff : Eye}
                  >
                    {state.maskMode.label}
                  </Chip>
                  {state.maskMode === MaskMode.Masked ? (
                    <Button
                      type="button"
                      variant="secondary"
                      size="sm"
                      icon={Eye}
                      disabled={isCapturing}
                      onClick={() => setConfirmingUnmask(true)}
                    >
                      unmask transcript
                    </Button>
                  ) : (
                    <Button
                      type="button"
                      variant="secondary"
                      size="sm"
                      icon={EyeOff}
                      disabled={isCapturing}
                      onClick={onRemask}
                    >
                      mask transcript
                    </Button>
                  )}
                </div>

                {confirmingUnmask && state.maskMode === MaskMode.Masked && (
                  <div className="fbw-unmask-warning" role="alert">
                    <div className="fbw-unmask-warning-head">
                      <AlertTriangle aria-hidden="true" />
                      <span>{UNMASK_WARNING_TEXT}</span>
                    </div>
                    <div className="fbw-unmask-actions">
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={() => setConfirmingUnmask(false)}
                      >
                        keep masked
                      </Button>
                      <Button
                        type="button"
                        variant="secondary"
                        size="sm"
                        disabled={isCapturing}
                        onClick={() => {
                          setConfirmingUnmask(false);
                          onRequestUnmask();
                        }}
                      >
                        confirm unmask
                      </Button>
                    </div>
                  </div>
                )}

                <div className="fbw-captures-grid">
                  {previews.map((entry, index) => {
                    const imageNumber = index + 1;
                    return (
                      <div key={entry.artifact.id} className="fbw-capture-card">
                        <img
                          src={entry.url}
                          alt={`image ${imageNumber}: ${entry.artifact.kind.label} capture (${entry.artifact.maskMode.label})`}
                          className="fbw-capture-img"
                        />
                        <div className="fbw-capture-meta">
                          <span className="fbw-capture-index">image {imageNumber}</span>
                          <span className="fbw-capture-label">{entry.artifact.kind.label}</span>
                          <span className="fbw-capture-dims tnum">
                            {entry.artifact.width}×{entry.artifact.height}
                          </span>
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            icon={Trash2}
                            disabled={isCapturing}
                            aria-label={`remove ${entry.artifact.kind.label} capture (image ${imageNumber})`}
                            onClick={() => onRemoveCapture(entry.artifact.id)}
                          >
                            remove
                          </Button>
                        </div>
                      </div>
                    );
                  })}
                </div>
              </>
            )}
          </div>

          {truncated && (
            <div className="fbw-trunc-notice" role="status">
              <AlertTriangle aria-hidden="true" />
              <span>
                The prefilled GitHub issue URL reached the length limit and was truncated. Paste any missing details
                after GitHub opens.
              </span>
            </div>
          )}
        </>
      )}
    </Dialog>
  );
}
