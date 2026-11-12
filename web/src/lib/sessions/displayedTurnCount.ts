/**
 * The turn count a reader is shown for a session: its conversation turns
 * (top-level user and assistant text or image entries). `turnCount` is the
 * number of folded turns, system and tool rows included, and is not the number
 * a list prints.
 *
 * The contract leaves `conversationTurnCount` optional: a producer or a stored
 * row older than the field does not carry it. Only then does this fall back to
 * `turnCount`, so an unmeasured session still prints a number. A measured zero
 * is kept. This is the one place the fallback is written; lists, summaries and
 * adapters call it and never read either field for display themselves.
 */
export function displayedTurnCount(session: {
  readonly turnCount: number;
  readonly conversationTurnCount?: number;
}): number {
  return session.conversationTurnCount ?? session.turnCount;
}
