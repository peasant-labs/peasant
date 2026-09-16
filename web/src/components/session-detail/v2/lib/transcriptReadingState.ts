/**
 * Per-session transcript reading state the host owns for Back navigation.
 *
 * Fairtrade owns the transcript rendering and its view state; the host owns the
 * route, the callbacks, and the restoration of what a reader had open when they
 * followed a stored context or parent link away and came back. The viewer resets
 * its own reading position when the session changes, so this module keeps a
 * small session-scoped record and the host replays it on the way back:
 *
 * - the inner `.txn-stream` scroll offset,
 * - the selected turn while the reader is navigating,
 * - the reader's search query.
 *
 * The route query itself lives in the URL, so the browser's Back already
 * restores it (including the retained-history disclosure); this record covers
 * only the state a URL cannot carry.
 */
export type TranscriptReadingState = {
  scrollTop: number;
  activeTurn?: number;
  search: string;
};

const KEY_PREFIX = 'peasant:transcript-reading:';

export const EMPTY_TRANSCRIPT_READING_STATE: TranscriptReadingState = Object.freeze({
  scrollTop: 0,
  search: '',
});

function storageKey(sessionId: string): string {
  return `${KEY_PREFIX}${sessionId}`;
}

/** Strict reader for one session's stored reading state; malformed records are discarded. */
export function normalizeTranscriptReadingState(value: unknown): TranscriptReadingState | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  const scrollTop = record.scrollTop;
  if (typeof scrollTop !== 'number' || !Number.isFinite(scrollTop) || scrollTop < 0) return null;
  const activeTurn = record.activeTurn;
  if (activeTurn !== undefined && (!Number.isSafeInteger(activeTurn) || (activeTurn as number) < 0)) return null;
  // A missing or non-string query degrades to empty: a stale record must never
  // invent a filter the reader never typed.
  const search = typeof record.search === 'string' ? record.search : '';
  return {
    scrollTop,
    activeTurn: activeTurn as number | undefined,
    search,
  };
}

export function readTranscriptReadingState(sessionId: string): TranscriptReadingState | null {
  if (typeof window === 'undefined') return null;
  try {
    const raw = window.sessionStorage.getItem(storageKey(sessionId));
    if (!raw) return null;
    return normalizeTranscriptReadingState(JSON.parse(raw));
  } catch {
    return null;
  }
}

export function writeTranscriptReadingState(sessionId: string, state: TranscriptReadingState): void {
  if (typeof window === 'undefined') return;
  try {
    window.sessionStorage.setItem(storageKey(sessionId), JSON.stringify(state));
  } catch {
    // A blocked or full storage must never break reading or navigation.
  }
}

/** The stored inner-scroller offset to replay on the way back; 0 when none. */
export function transcriptReadingScrollTop(sessionId: string): number {
  return readTranscriptReadingState(sessionId)?.scrollTop ?? 0;
}
