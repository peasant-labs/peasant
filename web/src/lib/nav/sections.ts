import { GRAPH_APP_SECTIONS } from '@peasant-labs/fairtrade/graph';
import { UI_CAPABILITY, type UICapabilityToken } from '@/lib/capabilities/tokens';

/**
 * App navigation sections — the single source of truth for the top nav, and
 * the seam the breadcrumbs and the Cmd+K command palette read from, so
 * "what sections exist, where they live, and what capability they require" is
 * defined once. This is the only capability-visibility policy point: consumers
 * hold no raw section-visibility logic.
 *
 * Graph shell IA: Home · Analytics · Code map. WHICH sections exist comes from
 * @peasant-labs/fairtrade/graph's GRAPH_APP_SECTIONS; the label and the lead
 * position are app-local overrides below. Home owns `/` — the project picker —
 * and stays active across /review/*. Code map owns /map plus
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
  /**
   * The server-advertised capability token required to expose this section in
   * persistent chrome. Absent means always visible; present means the section
   * is discoverable only when the capability set contains this token. Direct
   * routes are unaffected — this gates discoverability, not reachability.
   */
  requiredCapability?: UICapabilityToken;
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
    requiredCapability: UI_CAPABILITY.codeMapNavigationV1,
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
 * The graph shell registry stays the source of truth for WHICH sections exist,
 * so the `assertKnownGraphSection` guard below still catches an unmapped section
 * arriving in a future fairtrade release. Only the rendered string is replaced.
 *
 * `changes` reads as "home" because the section owns `/` — the page the app
 * opens on. Lowercase, matching fairtrade's chrome convention and the rest of
 * the nav.
 */
const LABEL_OVERRIDES: Partial<Record<GraphSectionId, string>> = {
  changes: 'home',
};

/**
 * The section that leads the nav, ahead of fairtrade's own analytics-first
 * order. Home is where the app opens, so it reads first; every other section
 * keeps the design system's relative order behind it.
 */
const LEAD_SECTION_ID: GraphSectionId = 'changes';

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
  // Stable partition, not a full reorder: the lead section moves to the front
  // and everything else keeps the order fairtrade shipped it in.
}).sort((a, b) => Number(b.id === LEAD_SECTION_ID) - Number(a.id === LEAD_SECTION_ID));

/**
 * Whether a section's discoverability requirement is met by the advertised
 * capability set. A section with no `requiredCapability` is always visible; one
 * with a requirement is visible only when the set contains that exact token.
 * Gated routes stay reachable by URL — this governs persistent chrome only.
 */
function sectionMeetsCapability(section: NavSection, capabilities: ReadonlySet<string>): boolean {
  return section.requiredCapability === undefined || capabilities.has(section.requiredCapability);
}

/** The nav sections to expose given the server's advertised capability set. */
export function visibleNavSections(capabilities: ReadonlySet<string>): NavSection[] {
  return NAV_SECTIONS.filter((section) => sectionMeetsCapability(section, capabilities));
}

/**
 * Whether the section `id` is discoverable given the advertised capability set.
 * The single predicate consumers (e.g. the command palette's per-project "· map"
 * jumps) use for capability-gated visibility — no raw section-id visibility
 * booleans live outside this module and the capabilities provider.
 */
export function isSectionVisible(id: NavSection['id'], capabilities: ReadonlySet<string>): boolean {
  const section = NAV_SECTIONS.find((s) => s.id === id);
  return section !== undefined && sectionMeetsCapability(section, capabilities);
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
