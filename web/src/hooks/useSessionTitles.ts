'use client';

import { useMemo } from 'react';
import { useChannel } from '@/contexts/WebSocketContext';
import { subscribe } from '@/types/messages';
import type { QualityPayload } from '@/types/messages';

/**
 * sessionId → generated session title.
 *
 * Titles are NOT on the sessions channel: `SessionSummary` carries no title
 * field, and the generated title lives on the QUALITY channel's per-session
 * record (`QualitySession.title`). So any surface that wants to show a title
 * for a row joins the two channels on session id — the same shape the
 * pre-release archive's project detail used when it merged quality data into
 * its session rows.
 *
 * The map is deliberately PARTIAL. The server only fills `title` when a title
 * was generated and stored for that session (`row.Title != nil`), so sessions
 * without one are simply absent here and callers fall back to the id. Never
 * render an empty string as a title.
 */
export function useSessionTitles(): ReadonlyMap<string, string> {
  const { data } = useChannel<QualityPayload>(subscribe.quality());

  return useMemo(() => {
    const titles = new Map<string, string>();
    for (const session of data?.sessions ?? []) {
      const title = session.title?.trim();
      if (title) titles.set(session.id, title);
    }
    return titles;
  }, [data]);
}
