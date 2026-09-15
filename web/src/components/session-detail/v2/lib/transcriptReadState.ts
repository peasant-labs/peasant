/**
 * Per-session read state the host restores after a navigation round trip.
 *
 * The host owns URL creation and Back restoration (the design boundary with
 * Fairtrade). When a reader follows a context/source link to the current
 * target, the child session is replaced by that target; browser Back re-mounts
 * the child route with the same URL but no component state, because the route
 * component is a fresh mount. Persisting the child's transient reading state
 * (search query, selected turn, earlier-history disclosure, scroll offset)
 * under its own session id is what makes Back land the reader where they left.
 *
 * The store is session-scoped: it survives same-tab navigation and Back, and
 * disappears when the tab closes. Storage being unavailable (private mode,
 * server render) degrades to an empty state, never an error.
 */
export interface TranscriptReadState {
  search: string;
  activeTurn: number | null;
  earlierHistoryOpen: Record<string, boolean>;
  scrollTop: number;
}

const STORAGE_PREFIX = 'peasant:transcript-read-state:';

export function emptyTranscriptReadState(): TranscriptReadState {
  return { search: '', activeTurn: null, earlierHistoryOpen: {}, scrollTop: 0 };
}

/**
 * Coerce stored bytes back into the read-state shape. Anything unrecognized
 * degrades to the empty state field by field, so a stale or hand-edited entry
 * can never break reading or invent a selection.
 */
export function normalizeTranscriptReadState(value: unknown): TranscriptReadState {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    return emptyTranscriptReadState();
  }
  const record = value as Record<string, unknown>;
  const activeTurn =
    typeof record.activeTurn === 'number' && Number.isSafeInteger(record.activeTurn) && record.activeTurn >= 0
      ? record.activeTurn
      : null;
  const earlierHistoryOpen: Record<string, boolean> = {};
  if (record.earlierHistoryOpen !== null && typeof record.earlierHistoryOpen === 'object' && !Array.isArray(record.earlierHistoryOpen)) {
    for (const [key, open] of Object.entries(record.earlierHistoryOpen as Record<string, unknown>)) {
      if (typeof open === 'boolean') earlierHistoryOpen[key] = open;
    }
  }
  return {
    search: typeof record.search === 'string' ? record.search : '',
    activeTurn,
    earlierHistoryOpen,
    scrollTop:
      typeof record.scrollTop === 'number' && Number.isFinite(record.scrollTop) && record.scrollTop >= 0
        ? record.scrollTop
        : 0,
  };
}

export function readTranscriptReadState(sessionId: string): TranscriptReadState {
  if (!sessionId || typeof window === 'undefined') return emptyTranscriptReadState();
  try {
    const raw = window.sessionStorage.getItem(STORAGE_PREFIX + sessionId);
    if (raw === null) return emptyTranscriptReadState();
    return normalizeTranscriptReadState(JSON.parse(raw));
  } catch {
    return emptyTranscriptReadState();
  }
}

export function writeTranscriptReadState(sessionId: string, state: TranscriptReadState): void {
  if (!sessionId || typeof window === 'undefined') return;
  try {
    window.sessionStorage.setItem(STORAGE_PREFIX + sessionId, JSON.stringify(state));
  } catch {
    // A full or unavailable store must never break reading.
  }
}

export function clearTranscriptReadState(sessionId: string): void {
  if (!sessionId || typeof window === 'undefined') return;
  try {
    window.sessionStorage.removeItem(STORAGE_PREFIX + sessionId);
  } catch {
    // Ignore.
  }
}
