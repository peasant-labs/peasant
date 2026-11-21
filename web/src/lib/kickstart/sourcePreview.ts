/**
 * What the selection page says about a raw session preview when the server
 * gave it no sentence of its own, and whether it offers to read the session
 * again.
 *
 * The server's words win: a cut or a grown end carries its own `lines`, and a
 * closed capture carries its own `message` when it has one. These tables are
 * the page's fallback for a reason that arrived without a sentence. Both are
 * keyed by the contract's closed sets with no default, so a reason added to the
 * contract stops the build here until it has words.
 *
 * Chrome is lowercase, as the design system sets it.
 */

import {
  LocalKickstartPreviewCloseReason,
  LocalKickstartPreviewCutReason,
} from '@peasant-labs/schema';

type CloseReason = (typeof LocalKickstartPreviewCloseReason)[keyof typeof LocalKickstartPreviewCloseReason];
type CutReason = (typeof LocalKickstartPreviewCutReason)[keyof typeof LocalKickstartPreviewCutReason];

/** The sentence for a preview the server closed, by reason, when it sent none. */
export const PREVIEW_CLOSED_SENTENCE = Object.freeze({
  [LocalKickstartPreviewCloseReason.ClosedByClient]: 'this preview was closed.',
  [LocalKickstartPreviewCloseReason.Replaced]: 'another preview opened in this tab, so this one closed.',
  [LocalKickstartPreviewCloseReason.SessionClosed]: 'this setup session ended, so the preview closed. reload the page to start again.',
  [LocalKickstartPreviewCloseReason.Rescanned]: 'the sessions were scanned again, so this preview closed. read it again from the start.',
  [LocalKickstartPreviewCloseReason.Idle]: 'this preview closed after five minutes without a request. read it again from the start.',
  [LocalKickstartPreviewCloseReason.Evicted]: 'another tab opened a preview, so this one closed. read it again from the start.',
  [LocalKickstartPreviewCloseReason.Shutdown]: 'the app on this computer stopped, so the preview closed. start it, then read this session again.',
  [LocalKickstartPreviewCloseReason.SourceChanged]: "this session's transcript changed after part of it was shown. read it again from the start.",
  [LocalKickstartPreviewCloseReason.LateResult]: 'a later part of this session answers a tool call in a turn already shown. read it again from the start to see the answer in place.',
  [LocalKickstartPreviewCloseReason.Unreadable]: 'the preview could not be read. check that the transcript is readable, then read it again.',
  [LocalKickstartPreviewCloseReason.OutOfOrder]: 'the preview request was out of order. read it again from the start.',
  [LocalKickstartPreviewCloseReason.SlowClient]: 'this tab fell behind the preview, so it closed. read it again from the start.',
  [LocalKickstartPreviewCloseReason.RowNotFound]: 'this row is no longer in the list. scan again, then pick it again.',
  [LocalKickstartPreviewCloseReason.NotASessionRow]: 'this row is not a session, so it has no transcript to show.',
  [LocalKickstartPreviewCloseReason.Unavailable]: 'this session has no transcript peasant can read on this computer.',
  [LocalKickstartPreviewCloseReason.NoTurns]: "no readable turns are in this session's transcript yet.",
  [LocalKickstartPreviewCloseReason.Capacity]: 'both previews are reading. wait a moment, then read this session again.',
} satisfies Record<CloseReason, string>);

/**
 * Whether a closed preview offers `read again`. Reading again opens a new
 * capture from the start; it is offered where that can succeed: the source
 * moved under the capture, the capture timed out or was pushed out, or the
 * server was busy. It is not offered where the row itself has no transcript.
 */
export const PREVIEW_CLOSED_READ_AGAIN = Object.freeze({
  [LocalKickstartPreviewCloseReason.ClosedByClient]: false,
  [LocalKickstartPreviewCloseReason.Replaced]: false,
  [LocalKickstartPreviewCloseReason.SessionClosed]: false,
  [LocalKickstartPreviewCloseReason.Rescanned]: true,
  [LocalKickstartPreviewCloseReason.Idle]: true,
  [LocalKickstartPreviewCloseReason.Evicted]: true,
  [LocalKickstartPreviewCloseReason.Shutdown]: true,
  [LocalKickstartPreviewCloseReason.SourceChanged]: true,
  [LocalKickstartPreviewCloseReason.LateResult]: true,
  [LocalKickstartPreviewCloseReason.Unreadable]: true,
  [LocalKickstartPreviewCloseReason.OutOfOrder]: true,
  [LocalKickstartPreviewCloseReason.SlowClient]: true,
  [LocalKickstartPreviewCloseReason.RowNotFound]: false,
  [LocalKickstartPreviewCloseReason.NotASessionRow]: false,
  [LocalKickstartPreviewCloseReason.Unavailable]: true,
  [LocalKickstartPreviewCloseReason.NoTurns]: false,
  [LocalKickstartPreviewCloseReason.Capacity]: true,
} satisfies Record<CloseReason, boolean>);

/** The sentence for a preview the server cut short, by reason, when it sent no lines. */
export const PREVIEW_CUT_SENTENCE = Object.freeze({
  [LocalKickstartPreviewCutReason.SourceBudget]: 'the preview stops here: it has read as much of this transcript as it reads on this computer.',
  [LocalKickstartPreviewCutReason.SerializedBudget]: 'the preview stops here: it holds as much of this session as a preview keeps.',
  [LocalKickstartPreviewCutReason.TurnBudget]: 'the preview stops here: it holds as many turns as a preview keeps.',
  [LocalKickstartPreviewCutReason.OversizedRecord]: 'the preview stops here: the next record of this transcript is too large to show.',
  [LocalKickstartPreviewCutReason.OriginNotResumable]: 'the preview shows what one read of this transcript holds; the rest cannot be read in parts.',
} satisfies Record<CutReason, string>);

/**
 * How a stop reads: an error card or a plain notice. A stop the reader did not
 * ask for and that lost part of the session is an error; a designed limit, the
 * end, or a notice about the session is information.
 */
export const PreviewNoticeTone = Object.freeze({
  Neutral: 'neutral',
  Error: 'error',
});
export type PreviewNoticeTone = (typeof PreviewNoticeTone)[keyof typeof PreviewNoticeTone];

/**
 * The tone of a preview the server closed, by reason. The source moving under
 * the capture, a transcript that cannot be read, and a broken exchange with the
 * app are errors; the reader's own act, a timeout, a rescan or a row with
 * nothing to show is not.
 */
export const PREVIEW_CLOSED_TONE = Object.freeze({
  [LocalKickstartPreviewCloseReason.ClosedByClient]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCloseReason.Replaced]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCloseReason.SessionClosed]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCloseReason.Rescanned]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCloseReason.Idle]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCloseReason.Evicted]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCloseReason.Shutdown]: PreviewNoticeTone.Error,
  [LocalKickstartPreviewCloseReason.SourceChanged]: PreviewNoticeTone.Error,
  [LocalKickstartPreviewCloseReason.LateResult]: PreviewNoticeTone.Error,
  [LocalKickstartPreviewCloseReason.Unreadable]: PreviewNoticeTone.Error,
  [LocalKickstartPreviewCloseReason.OutOfOrder]: PreviewNoticeTone.Error,
  [LocalKickstartPreviewCloseReason.SlowClient]: PreviewNoticeTone.Error,
  [LocalKickstartPreviewCloseReason.RowNotFound]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCloseReason.NotASessionRow]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCloseReason.Unavailable]: PreviewNoticeTone.Error,
  [LocalKickstartPreviewCloseReason.NoTurns]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCloseReason.Capacity]: PreviewNoticeTone.Neutral,
} satisfies Record<CloseReason, PreviewNoticeTone>);

/**
 * The tone of a preview the server cut short, by reason. The budgets are the
 * preview's designed limits; a record too large to show is a failure to read
 * the session.
 */
export const PREVIEW_CUT_TONE = Object.freeze({
  [LocalKickstartPreviewCutReason.SourceBudget]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCutReason.SerializedBudget]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCutReason.TurnBudget]: PreviewNoticeTone.Neutral,
  [LocalKickstartPreviewCutReason.OversizedRecord]: PreviewNoticeTone.Error,
  [LocalKickstartPreviewCutReason.OriginNotResumable]: PreviewNoticeTone.Neutral,
} satisfies Record<CutReason, PreviewNoticeTone>);

/** The pane's own sentences: none of them comes from the server. */
export const PreviewSentence = Object.freeze({
  /** The socket to the app dropped; a capture is never resumed after a reconnect. */
  ConnectionDropped: 'the connection to the app on this computer dropped. read this session again from the start.',
  /** A frame the contract's merge refused: the pane keeps what it shows and stops. */
  FrameRefused: 'the app on this computer sent a part of this preview that could not be read. read this session again from the start.',
  /** The scan finished while this preview was open. */
  Rescanned: PREVIEW_CLOSED_SENTENCE[LocalKickstartPreviewCloseReason.Rescanned],
});

/** The read-again control's label. */
export const READ_AGAIN = 'read again';
