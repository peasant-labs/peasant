import { GRAPH_APP_SECTIONS } from '@peasant-labs/fairtrade/graph';

/**
 * App navigation sections — the single source of truth for the top nav, and
 * the seam the breadcrumbs and a future Cmd+K command palette read from, so
 * "what sections exist and where they live" is defined once.
 *
 * Graph shell IA: Analytics · Changes · Code map (nav order — from
 * @peasant-labs/fairtrade/graph's GRAPH_APP_SECTIONS, analytics-first). Home
 * (`/`) is still the changes-first project picker regardless of nav order, so
 * Changes owns `/` and stays active across /review/*. Code map owns /map plus
 * /projects/{name}/{id} viewer deep links. Share is a persistent top-nav action
 * outside this graph-section registry and routes to `/share`.
 */

export interface NavSection {
  id: string;
  href: string;
  label: string;
  /** Extra pathname prefixes that keep this section active. */
  activePrefixes: string[];
  /** First-run tour anchor, when the section participates in the tour. */
  tourId?: string;
  /** Hover description (also reusable as a palette hint). */
  title?: string;
}

type GraphSectionId = 'analytics' | 'map' | 'changes';

type GraphShellSection = { id: string; label: string };

const GRAPH_NAV = GRAPH_APP_SECTIONS as readonly GraphShellSection[];

const ROUTES: Record<GraphSectionId, Omit<NavSection, 'id' | 'label'>> = {
  changes: {
    href: '/',
    activePrefixes: ['/review'],
    title: 'Your projects and the lines of work moving through them.',
  },
  map: {
    href: '/map',
    activePrefixes: ['/map', '/projects'],
    title: 'See a project as a map of its code areas and how they connect.',
  },
  analytics: {
    href: '/analytics',
    activePrefixes: ['/analytics'],
    title: 'Read project-level session volume, outcomes, duration, and contributor signals.',
  },
};

/**
 * App-local display labels that replace the ones fairtrade ships.
 *
 * The graph shell registry stays the source of truth for WHICH sections exist and
 * in what ORDER — only the rendered string is overridden here, so the
 * `assertKnownGraphSection` guard below still catches an unmapped section
 * arriving in a future fairtrade release.
 *
 * `changes` reads as "projects" because the section owns `/`, which is the
 * project picker: you land on a list of projects and drill into one. Lowercase,
 * matching fairtrade's chrome convention and the rest of the nav.
 */
const LABEL_OVERRIDES: Partial<Record<GraphSectionId, string>> = {
  changes: 'projects',
};

function assertKnownGraphSection(section: GraphShellSection): asserts section is GraphShellSection & { id: GraphSectionId } {
  if (!(section.id in ROUTES)) {
    throw new Error(
      `Unknown graph shell section "${section.id}" from @peasant-labs/fairtrade/graph in web/src/lib/nav/sections.ts. ` +
      `Update the explicit route metadata for analytics, map, and changes before exposing a new section.`,
    );
  }
}

export const NAV_SECTIONS: NavSection[] = GRAPH_NAV.map((section) => {
  assertKnownGraphSection(section);
  const route = ROUTES[section.id];
  return { ...route, id: section.id, label: LABEL_OVERRIDES[section.id] ?? section.label };
});

/**
 * Sections shelved behind `peasant web start --experimental` (reported by
 * GET /api/v1/config/features). The code map is experimental: its routes stay
 * reachable by URL, but no persistent chrome advertises it on a default server.
 */
const EXPERIMENTAL_SECTION_IDS: ReadonlySet<string> = new Set(['map']);

/** Whether a section is hidden from persistent chrome on a default (non-experimental) server. */
export function isExperimentalSection(section: NavSection): boolean {
  return EXPERIMENTAL_SECTION_IDS.has(section.id);
}

/** The nav sections to expose given the server's experimental gate. */
export function visibleNavSections(experimental: boolean): NavSection[] {
  return experimental ? NAV_SECTIONS : NAV_SECTIONS.filter((s) => !isExperimentalSection(s));
}

/**
 * Whether `pathname` is within a section. Changes owns `/` exactly (plus its
 * activePrefixes); the others match their href prefix plus any extra prefixes.
 */
export function isSectionActive(section: NavSection, pathname: string): boolean {
  const base =
    section.href === '/' ? pathname === '/' : pathname.startsWith(section.href);
  return base || section.activePrefixes.some((p) => pathname.startsWith(p));
}
