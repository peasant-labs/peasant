export type DiscoverySelectionStatus = 'selected' | 'unselected';

export interface DiscoveryItem {
  sessionId: string;
  locationLabel: string;
  branch: string;
  selectionStatus: DiscoverySelectionStatus;
}

export type DiscoveryBySessionId = ReadonlyMap<string, DiscoveryItem>;

const fields = ['sessionId', 'locationLabel', 'branch', 'selectionStatus'] as const;

export function decodeDiscovery(payload: unknown): DiscoveryBySessionId {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) fail('response must be an object');
  const root = payload as Record<string, unknown>;
  if (Object.keys(root).length !== 1 || !('items' in root)) fail('response must contain only the required items field');
  if (!Array.isArray(root.items)) fail('items must be an array');
  const decoded = new Map<string, DiscoveryItem>();
  root.items.forEach((value, index) => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) fail(`items[${index}] must be an object`);
    const row = value as Record<string, unknown>;
    if (Object.keys(row).length !== fields.length || fields.some((field) => !(field in row))) fail(`items[${index}] must contain exactly ${fields.join(', ')}`);
    if (typeof row.sessionId !== 'string' || row.sessionId.length === 0) fail(`items[${index}].sessionId must be a non-empty string`);
    if (typeof row.locationLabel !== 'string' || row.locationLabel.length === 0) fail(`items[${index}].locationLabel must be a non-empty string`);
    if (typeof row.branch !== 'string') fail(`items[${index}].branch must be a string`);
    if (row.selectionStatus !== 'selected' && row.selectionStatus !== 'unselected') fail(`items[${index}].selectionStatus must be selected or unselected`);
    if (decoded.has(row.sessionId)) fail(`items duplicates sessionId ${JSON.stringify(row.sessionId)}`);
    decoded.set(row.sessionId, row as unknown as DiscoveryItem);
  });
  return decoded;
}

export function requireDiscoveryItem(discovery: DiscoveryBySessionId, sessionId: string, consumer: string): DiscoveryItem {
  const item = discovery.get(sessionId);
  if (!item) fail(`${consumer} requires exactly one discovery row for session ${JSON.stringify(sessionId)}`);
  return item;
}

function fail(reason: string): never {
  throw new Error(`Could not decode GET /api/v1/web/discovery because ${reason}. No discovery metadata can be used safely. Refresh the page; if this repeats, update or restart Peasant.`);
}
