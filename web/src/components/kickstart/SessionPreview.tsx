'use client';

import { useCallback, useMemo, useState } from 'react';
import {
  TranscriptContinuationState,
  TranscriptTail,
  TranscriptViewer,
  adaptTranscript,
  type SegmentDirectoryVM,
} from '@peasant-labs/fairtrade/ui';
import type { LocalKickstartPreviewState } from '@peasant-labs/schema';
import { Button, Skeleton } from '@/lib/ft-ui';
import { useTheme } from '@/hooks/useTheme';
import { useKickstartSourcePreview } from '@/lib/kickstart/useKickstartSourcePreview';
import { PreviewNoticeTone, READ_AGAIN } from '@/lib/kickstart/sourcePreview';
import { continueTurnLabels } from '@/lib/kickstart/previewTurnLabels';
import { cookSegment } from '@/components/session-detail/v2/lib/transcriptSegments';
import ErrorCard from '@/components/ErrorCard';

/**
 * The raw preview of one session row, read on this computer and shown in the
 * transcript viewer's segment mode.
 *
 * The server streams the session one segment at a time over this tab's socket
 * (`useKickstartSourcePreview`); the first segment arrives without a click and
 * every later one only when the reader activates `load more` at the end of the
 * trace. While more can load the directory is open: the viewer labels what it
 * holds as loaded so far and calls the last response the latest loaded one.
 *
 * Nothing here can leave the page: the viewer has every capability off, no
 * app-initiated clipboard copy, no turn permalinks (so no URL change), and no
 * Markdown images (so no request). The text is raw: the preview is not
 * redacted, which is why it never leaves this computer.
 */

const NO_TRAIL: never[] = [];

const NO_CAPABILITIES = Object.freeze({
  canEdit: false,
  canLabel: false,
  canContribute: false,
  canChangeVisibility: false,
  canExport: false,
});

/** The viewer's segment directory for the wire's open directory. */
function directoryVM(held: LocalKickstartPreviewState): SegmentDirectoryVM {
  const wire = held.directory;
  return {
    sessionId: wire.sessionId,
    revision: wire.revision,
    complete: wire.complete,
    segments: wire.segments.map((ref) => ({ ...ref })),
    totalSegments: wire.totalSegments ?? wire.loadedSegments,
    totalTurns: wire.totalTurns ?? wire.loadedTurns,
    // A position is stated against a total only once the server sent one, so
    // nothing is invented while the directory is still open.
    ...(wire.totalConversationTurns != null ? { totalConversationTurns: wire.totalConversationTurns } : {}),
  };
}

type PreviewSessionVM = ReturnType<typeof adaptTranscript>['session'];

/** The header figures a capture does not measure; see {@link withoutUnmeasuredCounts}. */
const UNMEASURED_COUNTS = ['durationMins', 'totalTokens', 'tokensIn', 'tokensOut', 'toolCallCount'] as const;

/**
 * The adapted session without the figures the capture never measured. The
 * viewer's type lists them as numbers, but its header leaves out any figure that
 * is absent (fairtrade 0.1.1: "a figure that was not measured is left out,
 * never shown as unknown or as zero"), and nothing else in the viewer reads
 * them, so leaving them out is how the preview says it does not know them.
 */
function withoutUnmeasuredCounts(session: PreviewSessionVM): PreviewSessionVM {
  const copy: Partial<PreviewSessionVM> = { ...session };
  for (const key of UNMEASURED_COUNTS) delete copy[key];
  return copy as PreviewSessionVM;
}

export interface SessionPreviewProps {
  /** The setup session the tree belongs to. */
  readonly sessionId: string;
  /** The session row the reader clicked. */
  readonly nodeId: string;
  /** The row's label, used as the viewer's title. */
  readonly label: string;
  /** Changes when a scan finishes; a finished scan closes the capture. */
  readonly scanGeneration: number;
}

export default function SessionPreview({ sessionId, nodeId, label, scanGeneration }: SessionPreviewProps) {
  const preview = useKickstartSourcePreview(sessionId, nodeId, scanGeneration);
  const { held, continuation, notice, noticeTone, canReadAgain, loadMore, readAgain } = preview;
  const { theme } = useTheme();
  const [leftRailOpen, setLeftRailOpen] = useState(false);
  const [rightRailOpen, setRightRailOpen] = useState(false);

  // The final response is only known once the directory is complete.
  const tail = held !== null && held.directory.complete ? TranscriptTail.Complete : TranscriptTail.Incomplete;

  const viewModel = useMemo(() => {
    if (held === null || held.segments.length === 0) return null;
    const adapted = adaptTranscript(
      {
        ...held.header,
        turns: held.segments.flatMap((segment) => segment.turns),
        nativeMetadata: [...(held.header.nativeMetadata ?? []), ...held.segments.flatMap((segment) => segment.nativeMetadata ?? [])],
        retainedUnknown: held.segments.flatMap((segment) => segment.retainedUnknown ?? []),
      },
      undefined,
      undefined,
      { tail },
    );
    // The row's label is the title: a first prompt often opens with harness
    // markup, which must not become the heading. The header counts the turns
    // the directory says are held, the same numbers the position labels read
    // against, rather than the capture header's own count.
    //
    // The capture's header is the session as far as it is read, before any
    // import: the server measures no duration, tokens or tool calls for it, and
    // the wire's required numbers arrive as zero. The viewer leaves a figure it
    // is not given out of the header, so those are not given: a preview never
    // shows a count that was not measured.
    const wire = held.directory;
    return {
      ...adapted,
      session: {
        ...withoutUnmeasuredCounts(adapted.session),
        title: label,
        turnCount: wire.loadedTurns,
        conversationTurnCount: wire.loadedConversationTurns ?? undefined,
      },
    };
  }, [held, label, tail]);

  // Each segment is cooked alone, then given the labels the whole held
  // session has, so turn numbers continue across loaded segments.
  const segments = useMemo(() => {
    if (held === null) return [];
    const cooked = held.segments.map((segment) => cookSegment(held.header, segment, { tail }));
    if (viewModel === null) return cooked;
    return continueTurnLabels(cooked, viewModel.turns, held.segments.flatMap((segment) => segment.turns));
  }, [held, tail, viewModel]);
  const directory = useMemo(() => (held === null ? null : directoryVM(held)), [held]);

  // Every segment the directory names is already held, so a jump or a
  // scrubber tick is answered from memory and asks the server for nothing.
  const answerFromMemory = useCallback(() => {}, []);

  const readAgainControl = canReadAgain ? (
    <Button variant="secondary" size="sm" onClick={readAgain}>
      {READ_AGAIN}
    </Button>
  ) : null;

  if (viewModel === null || directory === null) {
    if (notice !== null) {
      if (noticeTone === PreviewNoticeTone.Error) {
        return (
          <ErrorCard className="ks-preview-closed">
            <p className="ks-help ks-preview-error-text">{notice}</p>
            {readAgainControl}
          </ErrorCard>
        );
      }
      return (
        <div className="ks-preview-closed" role="status">
          <p className="ks-help">{notice}</p>
          {readAgainControl}
        </div>
      );
    }
    return <Skeleton label="reading the preview" avatar={false} lines={2} />;
  }

  const continuationBody = (
    <>
      {notice !== null && <span>{notice}</span>}
      {continuation === TranscriptContinuationState.Failed && readAgainControl}
    </>
  );
  // A stop that lost part of the session reads as an error card, with its
  // `read again` inside; an end or a designed limit stays a plain notice.
  const continuationNotice = notice === null && readAgainControl === null ? undefined : noticeTone === PreviewNoticeTone.Error ? (
    <ErrorCard className="ks-preview-notice ks-preview-notice-error">{continuationBody}</ErrorCard>
  ) : (
    <span className="ks-preview-notice" role="status">{continuationBody}</span>
  );

  return (
    <div className="ks-session-preview" data-preview-session>
      <p className="ks-preview-raw">raw, on this computer only</p>
      <div className="ks-session-preview-viewer">
        <TranscriptViewer
          viewModel={viewModel}
          theme={theme}
          headingLevel={3}
          // No trail: the viewer's default one links to `#`, which would write
          // to the URL. The tree beside the pane already says where it is.
          breadcrumb={NO_TRAIL}
          capabilities={NO_CAPABILITIES}
          allowMarkdownImages={false}
          allowClipboard={false}
          leftRailOpen={leftRailOpen}
          onLeftRailOpenChange={setLeftRailOpen}
          rightRailOpen={rightRailOpen}
          onRightRailOpenChange={setRightRailOpen}
          directory={directory}
          segments={segments}
          onLoadSegment={answerFromMemory}
          continuation={continuation === null ? undefined : { state: continuation, notice: continuationNotice, onLoadMore: loadMore }}
        />
      </div>
    </div>
  );
}
