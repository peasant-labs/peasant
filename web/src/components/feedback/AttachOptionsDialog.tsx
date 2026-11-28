'use client';

import { useId, type ReactElement, type RefObject } from 'react';
import { Button, Checkbox, Dialog } from '@/lib/ft-ui';
import type { FeedbackKind } from '@/lib/feedback/types';
import './feedback-dialogs.css';

export const AttachOptionsLabels = Object.freeze({
  Cancel: 'cancel',
  Continue: 'continue',
  PickElement: 'pick an element on the page',
  CapturePage: 'capture the visible page',
  MaskNotice: 'Captures mask transcript text by default. You can review every field and preview before opening GitHub.',
} as const);

export interface AttachOptionsDialogProps {
  open: boolean;
  kind: FeedbackKind;
  attachElement: boolean;
  attachPage: boolean;
  onToggleElement(): void;
  onTogglePage(): void;
  onProceed(): void;
  onClose(): void;
  returnFocusRef?: RefObject<HTMLElement | null>;
}

/**
 * Compact dialog asking whether to pick an element and/or capture the visible page
 * before opening the compose dialog. Both options start unchecked and stay independent.
 */
export function AttachOptionsDialog({
  open,
  kind,
  attachElement,
  attachPage,
  onToggleElement,
  onTogglePage,
  onProceed,
  onClose,
  returnFocusRef,
}: AttachOptionsDialogProps): ReactElement {
  const labelId = useId();

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={kind.label}
      labelId={labelId}
      returnFocusRef={returnFocusRef}
      className="fbw-attach-dialog"
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose}>
            {AttachOptionsLabels.Cancel}
          </Button>
          <Button type="button" variant="primary" onClick={onProceed}>
            {AttachOptionsLabels.Continue}
          </Button>
        </>
      }
    >
      <div className="fbw-attach-options">
        <Checkbox checked={attachElement} onChange={() => onToggleElement()}>
          {AttachOptionsLabels.PickElement}
        </Checkbox>
        <Checkbox checked={attachPage} onChange={() => onTogglePage()}>
          {AttachOptionsLabels.CapturePage}
        </Checkbox>
      </div>
      <p className="fbw-dlg-prose">{AttachOptionsLabels.MaskNotice}</p>
    </Dialog>
  );
}
