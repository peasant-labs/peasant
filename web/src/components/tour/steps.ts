/**
 * First-run product tour — step catalogue.
 *
 * Each step highlights one `data-tour` anchor and shows a short popover. Copy
 * is intentionally ≤20 words per step. The flow follows the changes-first
 * home: picker row → the changes list → a change → the viewer → Contribute.
 * Steps span several routes, so each carries enough information for the
 * {@link TourProvider} to navigate there and wait for the anchor to mount
 * before spotlighting it; anchors that never mount skip gracefully.
 */

/** Stable anchor ids — added as `data-tour="<id>"` on exactly one element each. */
export const TOUR_ANCHORS = {
  home: 'home',
  picker: 'project-picker',
  changes: 'changes-list',
  change: 'change-detail',
  transcript: 'transcript-view',
  share: 'share-nav',
} as const;

export type TourAnchorId = (typeof TOUR_ANCHORS)[keyof typeof TOUR_ANCHORS];

/**
 * How a step resolves its destination route:
 *  - `static`  → navigate to a fixed pathname (home, Contribute).
 *  - `dynamic` → derive the pathname at runtime from a link already on the
 *    page (a picker row → the changes list; a change row → its detail; a
 *    task link → the viewer). Returns `null` when nothing is available,
 *    which makes the step skip gracefully.
 */
export type TourRoute =
  | { kind: 'static'; path: string }
  | { kind: 'dynamic'; resolve: () => string | null };

export interface TourStep {
  /** The `data-tour` anchor this step spotlights. */
  anchor: TourAnchorId;
  /** Popover copy (≤20 words). */
  body: string;
  /** Short heading shown above the body. */
  title: string;
  /** Route resolution strategy for this step. */
  route: TourRoute;
}

/** First `href` matching `selector` inside the document, or `null`. */
function firstHref(selector: string): string | null {
  if (typeof document === 'undefined') return null;
  const el = document.querySelector<HTMLAnchorElement>(selector);
  const href = el?.getAttribute('href');
  return href && href.length > 0 ? href : null;
}

/**
 * First session-viewer link inside `scopeSelector`, or `null`. A viewer URL
 * has three path segments (`/projects/<name>/<id>`); CSS can't count slashes,
 * so we filter the candidates in JS. Falls back to the whole document when
 * the scope element is absent.
 */
function firstSessionHref(scopeSelector: string): string | null {
  if (typeof document === 'undefined') return null;
  const scope = document.querySelector(scopeSelector) ?? document;
  const anchors = scope.querySelectorAll<HTMLAnchorElement>('a[href^="/projects/"]');
  for (const a of Array.from(anchors)) {
    const href = a.getAttribute('href');
    if (!href) continue;
    // /projects / <name> / <id>  →  3 non-empty segments after the leading slash
    const segments = href.split('/').filter(Boolean);
    if (segments.length >= 3) return href;
  }
  return null;
}

export const TOUR_STEPS: readonly TourStep[] = [
  {
    anchor: TOUR_ANCHORS.home,
    title: 'welcome to peasant',
    body: 'Your AI coding sessions stay on your machine: nothing leaves it. Here’s a quick tour.',
    route: { kind: 'static', path: '/' },
  },
  {
    anchor: TOUR_ANCHORS.picker,
    title: 'pick a project',
    body: 'Each row is one codebase: sessions, recorded coverage, last work, open changes. Click through to its changes.',
    // With a single project the home embeds that project's changes list (no
    // picker) — the anchor never mounts and the step skips gracefully.
    route: { kind: 'static', path: '/' },
  },
  {
    anchor: TOUR_ANCHORS.changes,
    title: 'the changes list',
    body: 'Every branch, measured against the default: files, sessions, structure deltas. Click a change for its story.',
    // Jump through the first picker row; with a single project the changes
    // list is already embedded on the current page.
    route: {
      kind: 'dynamic',
      resolve: () => {
        const href = firstHref('[data-tour="project-picker"] a[href^="/review/"]');
        if (href) return href;
        if (typeof document === 'undefined') return null;
        return document.querySelector('[data-tour="changes-list"]')
          ? window.location.pathname
          : null;
      },
    },
  },
  {
    anchor: TOUR_ANCHORS.change,
    title: 'a change',
    body: 'The story first (what was asked and the recorded work), then the changed slice and files.',
    // Jump through the first change row (?branch= rides in the query string).
    route: {
      kind: 'dynamic',
      resolve: () => {
        if (typeof document === 'undefined') return null;
        if (document.querySelector('[data-tour="change-detail"]')) {
          return window.location.pathname;
        }
        return firstHref('a[href^="/review/"][href*="?branch="]');
      },
    },
  },
  {
    anchor: TOUR_ANCHORS.transcript,
    title: 'session transcript',
    body: 'Every recorded turn, its file touches, and the way back to the change it shaped.',
    // The change detail's work rail links tasks into /projects/<name>/<id>.
    route: {
      kind: 'dynamic',
      resolve: () => firstSessionHref('[data-tour="change-detail"]'),
    },
  },
  {
    anchor: TOUR_ANCHORS.share,
    title: 'contribute',
    body: 'When you contribute, you’ll see exactly what leaves your machine and where it lands.',
    route: { kind: 'static', path: '/share' },
  },
] as const;

export const TOUR_STEP_COUNT = TOUR_STEPS.length;
