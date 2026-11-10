/** A grouping member: any closed value with a stable wire form. */
export interface GroupByMember {
  readonly wire: string;
}

/** The qualified group id the shared table uses for one bare key. */
export function qualifiedGroupId(groupBy: GroupByMember, key: string): string {
  return `${groupBy.wire}:${key}`;
}

/**
 * The heading for one qualified group id, as the table reports it.
 *
 * `labels` maps a qualified id to the display text the host resolved (a project
 * hash to its display name, say). The map is additive: a group the host has no
 * label for is named by its own bare key. The host never parses the id back
 * itself; this is the one place that knows the qualified shape.
 */
export function groupLabelOf(
  groupId: string,
  groupBy: GroupByMember,
  labels: ReadonlyMap<string, string> = new Map(),
): string {
  const prefix = `${groupBy.wire}:`;
  const key = groupId.startsWith(prefix) ? groupId.slice(prefix.length) : groupId;
  return labels.get(groupId) ?? key;
}
