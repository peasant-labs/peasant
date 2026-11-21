import type { CookedSegment } from '@/components/session-detail/v2/lib/transcriptSegments';
import type { TurnDetail } from '@/types/messages';

/**
 * Turn labels for a raw preview, continued across its loaded segments.
 *
 * Each segment is cooked alone, so Fairtrade numbers its turns from that
 * segment's own start: the second segment's first prompt would read `#1`. The
 * preview always holds every segment from the start of the session, so the
 * labels Fairtrade's adapter gives the concatenation of every held turn are the
 * session's own labels. This takes those, by turn index, onto the cooked
 * segments. It writes no numbering rule of its own.
 *
 * Fairtrade's rule labels a turn from the turns at and before it only (a
 * prompt count, then a letter per reply), so appending a segment never changes
 * a label already shown. The one place a later turn touches an earlier one is
 * the adapter folding two adjacent identical turns into one; across a segment
 * boundary that leaves one of the pair out of the concatenation. That turn
 * takes the label of the twin it was folded into, which is the label it showed
 * before the next segment arrived.
 */

type Face = Pick<TurnDetail, 'index' | 'role' | 'content'>;
type Labelled = { readonly index: number; readonly label: string };

function sameFace(a: Face | undefined, b: Face | undefined): boolean {
  return a !== undefined && b !== undefined && a.role === b.role && a.content === b.content;
}

/**
 * @param segments the held segments, cooked one by one, in order
 * @param whole    the turns the adapter cooked from every held turn at once
 * @param wire     every held wire turn, for the adapter's identical-turn fold
 */
export function continueTurnLabels(
  segments: readonly CookedSegment[],
  whole: readonly Labelled[],
  wire: readonly Face[],
): CookedSegment[] {
  const labels = new Map(whole.map((turn) => [turn.index, turn.label]));
  const faces = new Map(wire.map((turn) => [turn.index, turn]));
  const stream = segments.flatMap((segment) => segment.turns);
  const resolved = new Map<number, string>();
  stream.forEach((turn, at) => {
    const own = labels.get(turn.index);
    if (own !== undefined) {
      resolved.set(turn.index, own);
      return;
    }
    for (const twin of [stream[at - 1], stream[at + 1]]) {
      if (twin === undefined) continue;
      const label = labels.get(twin.index);
      if (label !== undefined && sameFace(faces.get(turn.index), faces.get(twin.index))) {
        resolved.set(turn.index, label);
        return;
      }
    }
  });
  return segments.map((segment) => ({
    ...segment,
    turns: segment.turns.map((turn) => {
      const label = resolved.get(turn.index);
      return label === undefined || label === turn.label ? turn : { ...turn, label };
    }),
  }));
}
