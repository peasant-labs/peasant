import type { ReactNode } from 'react';

/** The accessible name of the selection step's preview column. */
export const PREVIEW_REGION_LABEL = 'session preview';

/** What the slot holds: the transcript viewer, or a state shown in its place. */
export const PreviewSlotContent = Object.freeze({
  Viewer: 'viewer',
  State: 'state',
});
export type PreviewSlotContent = (typeof PreviewSlotContent)[keyof typeof PreviewSlotContent];

export interface PreviewSlotProps {
  readonly content: PreviewSlotContent;
  readonly children: ReactNode;
}

/**
 * The selection step's right column: a labelled region with the reading
 * pane's fixed size and no frame of its own. The transcript viewer is its
 * only child once a segment arrives; before that the instruction, the reading
 * skeleton or a stop sits in the same slot at the same size, so the tree
 * beside it does not move when a preview loads or fails.
 *
 * It lives apart from the viewer so the selection step can draw the empty and
 * loading states without loading the viewer's chunk.
 */
export default function PreviewSlot({ content, children }: PreviewSlotProps) {
  const isViewer = content === PreviewSlotContent.Viewer;
  return (
    <section
      className={isViewer ? 'ks-session-preview' : 'ks-session-preview ks-session-preview-state'}
      aria-label={PREVIEW_REGION_LABEL}
      data-preview-session={isViewer ? true : undefined}
      data-preview-pane-state={isViewer ? undefined : true}
    >
      {children}
    </section>
  );
}
