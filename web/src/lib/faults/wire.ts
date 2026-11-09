/**
 * Boundary helpers shared by the provider and the fault tests.
 *
 * A frame is measured and, when it fails, its operation is peeked from the raw
 * text before any decode. The failed frame never reaches a surface; only the
 * bounded detail below does.
 */

/** The app accepts a server frame up to 8 MiB, the same bound the schema scan uses. */
export const MAX_FRAME_BYTES = 8 << 20;

const TYPE_PEEK_CHARS = 512;
const MESSAGE_TYPE_PATTERN = /"type"\s*:\s*"([a-z_]+)"/;

/** UTF-8 byte length of a WebSocket text frame. */
export function frameByteLength(text: string): number {
  return new TextEncoder().encode(text).length;
}

/**
 * Read the server message type from a frame that failed to decode.
 *
 * The server's encoder writes `type` first, so the bounded head answers for a
 * large frame. A small frame gets a whole-text scan because a fixture or an
 * older encoder may put `type` last.
 */
export function peekMessageType(text: string): string | null {
  const head = MESSAGE_TYPE_PATTERN.exec(text.slice(0, TYPE_PEEK_CHARS));
  if (head) return head[1];
  if (text.length > MAX_FRAME_BYTES) return null;
  return MESSAGE_TYPE_PATTERN.exec(text)?.[1] ?? null;
}

type ZodLikeIssue = { code?: unknown; path?: unknown };
type ZodLikeError = { issues: ZodLikeIssue[] };

function isZodLike(error: unknown): error is ZodLikeError {
  return (
    typeof error === 'object' &&
    error !== null &&
    Array.isArray((error as { issues?: unknown }).issues)
  );
}

function issuePath(path: unknown): string {
  if (!Array.isArray(path) || path.length === 0) return 'the root';
  return `/${path.map((part) => String(part)).join('/')}`;
}

/**
 * Turn a schema failure into a bounded path, never the exception text and never
 * the payload. A raw scanner failure keeps its short reason (a key name or a
 * bound), which is what the reader needs to act on.
 */
export function schemaPathDetail(error: unknown): string {
  if (isZodLike(error) && error.issues.length > 0) {
    const [issue] = error.issues;
    const code = typeof issue.code === 'string' ? issue.code : 'invalid';
    return `${issuePath(issue.path)} (${code})`;
  }
  if (error instanceof TypeError) {
    const reason = /scanning near character \d+: (.*?);/.exec(error.message);
    if (reason) return reason[1];
  }
  return 'the frame is not valid JSON';
}
