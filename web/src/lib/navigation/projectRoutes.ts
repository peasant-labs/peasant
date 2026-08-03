import { isCodeMapViewportScale } from '@peasant-labs/fairtrade/graph';
import { isProjectHash, type ProjectHash } from '@peasant-labs/schema';

export type { ProjectHash } from '@peasant-labs/schema';

const HASH_LIKE = /^(?:[0-9a-fA-F]{63,65}|[0-9A-Za-z]{64})$/;
const CONTROL = /[\u0000-\u001f\u007f]/;
const RESIDUAL_ESCAPE = /%[0-9a-fA-F]{2}/;
const ENCODED_SEPARATOR = /%(?:2f|5c)/i;

export function parseProjectHash(raw: string | null | undefined): ProjectHash | null {
  return isProjectHash(raw) ? raw : null;
}

export const RouteOrigin = {
  Map: 'Map',
  Review: 'Review',
} as const;
export type RouteOrigin = (typeof RouteOrigin)[keyof typeof RouteOrigin];

export type ReturnLocation = {
  readonly version: 1;
  readonly origin: RouteOrigin;
  readonly href: string;
};

export type MapRouteState = {
  projectHash: ProjectHash;
  node: string | null;
  mode: 'navigator' | 'canvas';
  grain: 'project' | 'package' | 'file';
  expanded: readonly string[];
  filter: string;
  focus: string | null;
  viewport: { scale: number; panX: number; panY: number } | null;
};

export type MapRouteOptions = Partial<Omit<MapRouteState, 'projectHash'>> & {
  /** Compatibility input for old callers. New code uses `expanded`. */
  expand?: string;
};

export const DEFAULT_MAP_ROUTE_STATE: Omit<MapRouteState, 'projectHash'> = Object.freeze({
  node: null,
  mode: 'navigator',
  grain: 'package',
  expanded: Object.freeze([]) as readonly string[],
  filter: '',
  focus: null,
  viewport: null,
});

export type RouteMalformedCode =
  | 'wrong_route_shape'
  | 'malformed_escape'
  | 'encoded_separator'
  | 'double_encoded_segment'
  | 'unsafe_segment'
  | 'malformed_project_hash';

export type ProjectRoute =
  | { kind: 'canonical'; projectHash: ProjectHash }
  | { kind: 'legacy'; projectLabel: string }
  | { kind: 'missing' }
  | { kind: 'malformed'; code: RouteMalformedCode; message: string };

export type TranscriptRoute =
  | ({ sessionId: string } & Exclude<ProjectRoute, { kind: 'missing' } | { kind: 'malformed' }>)
  | { kind: 'missing' }
  | { kind: 'malformed'; code: RouteMalformedCode; message: string };

const MAP_FIELDS = new Set(['node', 'mode', 'grain', 'expand', 'filter', 'focus', 'scale', 'panX', 'panY']);
const REVIEW_FIELDS = new Set(['branch']);
const TRANSCRIPT_FIELDS = new Set(['turn', 'scope', 'scopeVal', 'origin', 'originNode', 'originBranch', 'returnTo']);

export const TranscriptScope = {
  Task: 'task',
  File: 'file',
  Change: 'change',
} as const;
export type TranscriptScope = (typeof TranscriptScope)[keyof typeof TranscriptScope];

function malformed(code: RouteMalformedCode, where: string, reason: string): ProjectRoute {
  return {
    kind: 'malformed',
    code,
    message: `The ${where} route could not be opened because ${reason}. Return to the project picker and open a fresh link.`,
  };
}

function decodeSegment(raw: string, where: string): string | ProjectRoute {
  if (ENCODED_SEPARATOR.test(raw)) return malformed('encoded_separator', where, 'a path segment contains an encoded slash');
  let decoded: string;
  try {
    decoded = decodeURIComponent(raw);
  } catch {
    return malformed('malformed_escape', where, 'a path segment contains a malformed percent escape');
  }
  if (RESIDUAL_ESCAPE.test(decoded)) return malformed('double_encoded_segment', where, 'a path segment is still percent-encoded after one decode');
  if (!decoded || decoded.includes('/') || decoded.includes('\\') || CONTROL.test(decoded)) {
    return malformed('unsafe_segment', where, 'a path segment is empty or contains an unsafe character');
  }
  return decoded;
}

function projectIdentity(decoded: string, where: string): ProjectRoute {
  const projectHash = parseProjectHash(decoded);
  if (projectHash) return { kind: 'canonical', projectHash };
  if (HASH_LIKE.test(decoded)) return malformed('malformed_project_hash', where, 'the project hash is not exactly 64 lowercase hexadecimal characters');
  return { kind: 'legacy', projectLabel: decoded };
}

function routeSegments(pathname: string, prefix: '/map' | '/review' | '/projects'): string[] | ProjectRoute {
  if (pathname === prefix || pathname === `${prefix}/`) return [];
  if (!pathname.startsWith(`${prefix}/`)) return malformed('wrong_route_shape', prefix.slice(1), `the path does not start with ${prefix}/`);
  // next.config.ts sets trailingSlash: true (the static-export/SPA-serving
  // shape the Go embed handler requires), so a canonically-served,
  // freshly-loaded URL for a single- or multi-segment route legitimately
  // ends in one '/' (e.g. `/map/{hash}/`) — strip exactly that one trailing
  // slash before splitting, so it doesn't read as an embedded empty
  // segment. A genuine malformed path (an internal empty segment from a
  // double slash, e.g. `/map//x` or a doubled trailing `/map/{hash}//`)
  // still has an empty element left after stripping the single trailing
  // slash and is still rejected below.
  const remainder = pathname.slice(prefix.length + 1);
  const withoutTrailingSlash = remainder.endsWith('/') ? remainder.slice(0, -1) : remainder;
  const raw = withoutTrailingSlash.split('/');
  if (raw.some((part) => part === '')) return malformed('wrong_route_shape', prefix.slice(1), 'the path contains an empty segment');
  return raw;
}

function parseSingleProjectRoute(pathname: string, prefix: '/map' | '/review'): ProjectRoute {
  const segments = routeSegments(pathname, prefix);
  if (!Array.isArray(segments)) return segments;
  if (segments.length === 0) return { kind: 'missing' };
  if (segments.length !== 1) return malformed('wrong_route_shape', prefix.slice(1), 'it must contain exactly one project segment');
  const decoded = decodeSegment(segments[0], prefix.slice(1));
  return typeof decoded === 'string' ? projectIdentity(decoded, prefix.slice(1)) : decoded;
}

export function parseMapRoute(pathname: string): ProjectRoute {
  return parseSingleProjectRoute(pathname, '/map');
}

export function parseReviewRoute(pathname: string): ProjectRoute {
  return parseSingleProjectRoute(pathname, '/review');
}

/** Parse the retired `/projects/{project}` detail route for its map redirect. */
export function parseProjectDetailRoute(pathname: string): ProjectRoute {
  const segments = routeSegments(pathname, '/projects');
  if (!Array.isArray(segments)) return segments;
  if (segments.length === 0) return { kind: 'missing' };
  if (segments.length !== 1) return malformed('wrong_route_shape', 'project detail', 'it must contain exactly one project segment');
  const decoded = decodeSegment(segments[0], 'project detail');
  return typeof decoded === 'string' ? projectIdentity(decoded, 'project detail') : decoded;
}

export function parseTranscriptRoute(pathname: string): TranscriptRoute {
  const segments = routeSegments(pathname, '/projects');
  if (!Array.isArray(segments)) return segments as Extract<TranscriptRoute, { kind: 'malformed' }>;
  if (segments.length === 0) return { kind: 'missing' };
  if (segments.length !== 2) return malformed('wrong_route_shape', 'transcript', 'it must contain exactly one project and one session segment') as Extract<TranscriptRoute, { kind: 'malformed' }>;
  const project = decodeSegment(segments[0], 'transcript');
  if (typeof project !== 'string') return project as Extract<TranscriptRoute, { kind: 'malformed' }>;
  const sessionId = decodeSegment(segments[1], 'transcript');
  if (typeof sessionId !== 'string') return sessionId as Extract<TranscriptRoute, { kind: 'malformed' }>;
  const identity = projectIdentity(project, 'transcript');
  return identity.kind === 'canonical'
    ? { ...identity, sessionId }
    : identity.kind === 'legacy'
      ? { ...identity, sessionId }
      : identity as Extract<TranscriptRoute, { kind: 'malformed' }>;
}

function appendValue(params: URLSearchParams, key: string, value: string | number | null | undefined): void {
  if (value !== undefined && value !== null && value !== '') params.set(key, String(value));
}

function safeBuilderSegment(value: string, where: string): string {
  if (!value || value.includes('/') || value.includes('\\') || CONTROL.test(value) || RESIDUAL_ESCAPE.test(value)) {
    throw new Error(`Cannot create ${where} link: the identifier contains an empty, encoded, or path-separator value. Use the canonical identifier returned by Peasant.`);
  }
  return encodeURIComponent(value);
}

export function mapHref(projectHash: ProjectHash, state: MapRouteOptions = {}): string {
  const params = new URLSearchParams();
  appendValue(params, 'node', state.node);
  appendValue(params, 'mode', state.mode);
  appendValue(params, 'grain', state.grain);
  const expanded = [...new Set(state.expanded ?? (state.expand ? [state.expand] : []))].sort();
  for (const value of expanded) params.append('expand', value);
  appendValue(params, 'filter', state.filter);
  appendValue(params, 'focus', state.focus);
  if (state.viewport) {
    appendValue(params, 'scale', state.viewport.scale);
    appendValue(params, 'panX', state.viewport.panX);
    appendValue(params, 'panY', state.viewport.panY);
  }
  const query = params.toString();
  return `/map/${projectHash}${query ? `?${query}` : ''}`;
}

export function reviewHref(projectHash: ProjectHash, options: { branch?: string; returnLocation?: ReturnLocation } = {}): string {
  const params = new URLSearchParams();
  appendValue(params, 'branch', options.branch);
  if (options.returnLocation) params.set('returnTo', formatReturnLocation(options.returnLocation));
  const query = params.toString();
  return `/review/${projectHash}${query ? `?${query}` : ''}`;
}

export type TranscriptHrefOptions = {
  turn?: number;
  scope?: TranscriptScope;
  scopeVal?: string;
  origin?: RouteOrigin;
  originNode?: string;
  originBranch?: string;
  returnLocation?: ReturnLocation;
};

export function transcriptHref(projectHash: ProjectHash, sessionId: string, options: TranscriptHrefOptions = {}): string {
  const params = new URLSearchParams();
  if (options.turn !== undefined && Number.isInteger(options.turn) && options.turn >= 0) params.set('turn', String(options.turn));
  appendValue(params, 'scope', options.scope);
  appendValue(params, 'scopeVal', options.scopeVal);
  appendValue(params, 'origin', options.origin);
  appendValue(params, 'originNode', options.originNode);
  appendValue(params, 'originBranch', options.originBranch);
  if (options.returnLocation) params.set('returnTo', formatReturnLocation(options.returnLocation));
  const query = params.toString();
  return `/projects/${projectHash}/${safeBuilderSegment(sessionId, 'transcript')}${query ? `?${query}` : ''}`;
}

function singleton(params: URLSearchParams, key: string): string | null | undefined {
  const values = params.getAll(key);
  if (values.length > 1) return undefined;
  return values.length === 0 ? null : values[0];
}

function safeQueryString(value: string | null | undefined): value is string {
  return value != null && !CONTROL.test(value) && !value.includes('\\') && !RESIDUAL_ESCAPE.test(value);
}

function finiteParam(params: URLSearchParams, key: string): number | null | undefined {
  const raw = singleton(params, key);
  if (raw === undefined) return undefined;
  if (raw === null || raw.trim() === '') return null;
  const parsed = Number(raw);
  return Number.isFinite(parsed) ? parsed : undefined;
}

export function parseMapRouteState(pathname: string, search: string | URLSearchParams): MapRouteState | null {
  const route = parseMapRoute(pathname);
  if (route.kind !== 'canonical') return null;
  const params = typeof search === 'string' ? new URLSearchParams(search) : new URLSearchParams(search);
  for (const key of params.keys()) if (!MAP_FIELDS.has(key)) return null;
  for (const key of MAP_FIELDS) if (key !== 'expand' && params.getAll(key).length > 1) return null;
  const mode = singleton(params, 'mode');
  if (mode !== null && mode !== 'navigator' && mode !== 'canvas') return null;
  const grain = singleton(params, 'grain');
  if (grain !== null && grain !== 'project' && grain !== 'package' && grain !== 'file') return null;
  const node = singleton(params, 'node');
  const filter = singleton(params, 'filter');
  const focus = singleton(params, 'focus');
  const expanded = params.getAll('expand');
  if ([node, filter, focus, ...expanded].some((value) => value !== null && !safeQueryString(value))) return null;
  if (expanded.some((value) => value.trim() === '')) return null;
  const scale = finiteParam(params, 'scale');
  const panX = finiteParam(params, 'panX');
  const panY = finiteParam(params, 'panY');
  if (scale === undefined || panX === undefined || panY === undefined) return null;
  const viewportValues = [scale, panX, panY];
  const hasViewport = viewportValues.every((value) => value !== null);
  if (!hasViewport && viewportValues.some((value) => value !== null)) return null;
  if (scale !== null && !isCodeMapViewportScale(scale)) return null;
  return {
    projectHash: route.projectHash,
    node: node || null,
    mode: mode ?? DEFAULT_MAP_ROUTE_STATE.mode,
    grain: grain ?? DEFAULT_MAP_ROUTE_STATE.grain,
    expanded: [...new Set(expanded)].sort(),
    filter: filter ?? DEFAULT_MAP_ROUTE_STATE.filter,
    focus: focus || null,
    viewport: hasViewport ? { scale: scale!, panX: panX!, panY: panY! } : null,
  };
}

export function formatMapRouteState(state: MapRouteState): string {
  return mapHref(state.projectHash, state);
}

export type ReviewRouteQuery = { branch: string | null; returnLocation: ReturnLocation | null };

export function parseReviewRouteQuery(search: string | URLSearchParams): ReviewRouteQuery | null {
  const params = typeof search === 'string' ? new URLSearchParams(search) : new URLSearchParams(search);
  for (const key of params.keys()) if (key !== 'branch' && key !== 'returnTo') return null;
  if (params.getAll('branch').length > 1 || params.getAll('returnTo').length > 1) return null;
  const branch = params.get('branch');
  if (branch != null && !safeQueryString(branch)) return null;
  const rawReturn = params.get('returnTo');
  const parsedReturn = parseReturnLocation(rawReturn);
  if (rawReturn && !parsedReturn) return null;
  return { branch: branch || null, returnLocation: parsedReturn };
}

export type TranscriptRouteQuery = {
  turn: number | null;
  scope: TranscriptScope | null;
  scopeVal: string;
  origin: RouteOrigin | null;
  originNode: string | null;
  originBranch: string | null;
  returnLocation: ReturnLocation | null;
};

export function parseTranscriptRouteQuery(search: string | URLSearchParams): TranscriptRouteQuery | null {
  const params = typeof search === 'string' ? new URLSearchParams(search) : new URLSearchParams(search);
  for (const key of params.keys()) if (!TRANSCRIPT_FIELDS.has(key)) return null;
  for (const key of TRANSCRIPT_FIELDS) if (params.getAll(key).length > 1) return null;
  const rawTurn = params.get('turn');
  const turn = rawTurn == null ? null : Number(rawTurn);
  if (rawTurn != null && (!/^\d+$/.test(rawTurn) || !Number.isSafeInteger(turn))) return null;
  const rawScope = params.get('scope');
  if (rawScope != null && rawScope !== TranscriptScope.Task && rawScope !== TranscriptScope.File && rawScope !== TranscriptScope.Change) return null;
  const scopeVal = params.get('scopeVal') ?? '';
  if (!safeQueryString(scopeVal) || (rawScope == null) !== (scopeVal === '')) return null;
  const rawOrigin = params.get('origin');
  if (rawOrigin != null && rawOrigin !== RouteOrigin.Map && rawOrigin !== RouteOrigin.Review) return null;
  const originNode = params.get('originNode');
  const originBranch = params.get('originBranch');
  if ([originNode, originBranch].some((value) => value != null && !safeQueryString(value))) return null;
  const rawReturn = params.get('returnTo');
  const parsedReturn = parseReturnLocation(rawReturn);
  if (rawReturn && !parsedReturn) return null;
  return {
    turn,
    scope: rawScope as TranscriptScope | null,
    scopeVal,
    origin: rawOrigin as RouteOrigin | null,
    originNode,
    originBranch,
    returnLocation: parsedReturn,
  };
}

function validateLocalReturnHref(href: string): ReturnLocation | null {
  if (!href.startsWith('/') || href.startsWith('//') || href.includes('\\') || href.includes('#') || CONTROL.test(href)) return null;
  let url: URL;
  try {
    url = new URL(href, 'http://peasant.local');
  } catch {
    return null;
  }
  if (url.origin !== 'http://peasant.local') return null;
  if (parseMapRoute(url.pathname).kind === 'canonical' && parseMapRouteState(url.pathname, url.searchParams) != null) {
    return { version: 1, origin: RouteOrigin.Map, href: `${url.pathname}${url.search}` };
  }
  if (parseReviewRoute(url.pathname).kind === 'canonical') {
    for (const key of url.searchParams.keys()) if (!REVIEW_FIELDS.has(key)) return null;
    if (parseReviewRouteQuery(url.searchParams) == null) return null;
    return { version: 1, origin: RouteOrigin.Review, href: `${url.pathname}${url.search}` };
  }
  return null;
}

export function returnLocation(href: string): ReturnLocation | null {
  return validateLocalReturnHref(href);
}

export function formatReturnLocation(location: ReturnLocation): string {
  return `v1.${location.href}`;
}

export function parseReturnLocation(raw: string | null | undefined): ReturnLocation | null {
  if (!raw) return null;
  if (raw.startsWith('v1.')) return validateLocalReturnHref(raw.slice(3));
  if (/^v\d+\./.test(raw)) return null;
  return validateLocalReturnHref(raw);
}
