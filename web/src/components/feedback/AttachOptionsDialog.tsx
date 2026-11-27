'use client';

import { useId, type ReactElement, type RefObject } from 'react';
import { Button, Checkbox, Dialog } from '@/lib/ft-ui';
import type { FeedbackKind } from '@/lib/feedback/types';
import './feedback-dialogs.css';

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
      size="default"
      className="fbw-attach-dialog"
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose}>
            cancel
          </Button>
          <Button type="button" variant="primary" onClick={onProceed}>
            continue
          </Button>
        </>
      }
    >
      <div className="fbw-attach-options">
        <Checkbox checked={attachElement} onChange={() => onToggleElement()}>
          pick an element on the page
        </Checkbox>
        <Checkbox checked={attachPage} onChange={() => onTogglePage()}>
          capture the visible page
        </Checkbox>
      </div>
      <p className="fbw-dlg-prose">
        Captures mask transcript text by default. You can review every field and preview before opening GitHub.
      </p>
    </Dialog>
  );
}
