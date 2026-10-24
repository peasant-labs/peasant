import { summarizePrompt } from '@peasant-labs/fairtrade/ui';

/**
 * How the publish wizard names a session row: its first prompt, its short id,
 * and its date, all in the lowercase chrome the local app uses.
 */

/**
 * The session list sends a first prompt cut to its first 40 characters, with
 * no mark that it was cut. A prompt that long is taken as cut, so the row ends
 * it before its last word (which may itself be cut) with an ellipsis. This
 * restates the list's cut for display only; nothing is decided by it.
 */
const LIST_PREVIEW_CHARS = 40;

/** The leading characters of a session id that stand for it in a row. */
const SHORT_ID_CHARS = 8;

export function shortSessionId(id: string): string {
  return id.length > SHORT_ID_CHARS ? `${id.slice(0, SHORT_ID_CHARS)}…` : id;
}

/** The row's title: the first prompt, never cut mid-word, else the short id. */
export function sessionPreviewTitle(preview: string, id: string): string {
  const summary = summarizePrompt(preview);
  if (!summary) return shortSessionId(id);
  if (Array.from(preview).length < LIST_PREVIEW_CHARS || summary.endsWith('…') || /[.!?]$/.test(summary)) return summary;
  const words = summary.replace(/\s+\S*$/, '');
  return `${words && words !== summary ? words : summary}…`;
}

/** A session's start day, lowercase: "oct 22". */
export function sessionDay(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  return date.toLocaleDateString('en-US', { month: 'short', day: 'numeric' }).toLowerCase();
}
