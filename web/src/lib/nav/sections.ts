import type { LucideIcon } from 'lucide-react';
import { LOCAL_APP_SECTIONS } from '@peasant-labs/fairtrade/graph';
import { UI_CAPABILITY, type UICapabilityToken } from '@/lib/capabilities/tokens';

/**
 * App sections — the single source of truth for the header and the Cmd+K
 * command palette, so "what sections exist, where they live, and which ones
 * persistent chrome may link to" is defined once. This is the only
 * section-visibility policy point: consumers hold no raw section-id logic.
 *
 * WHICH sections exist, their order, their labels, and whether each one is
 * listed in the navigation come from @peasant-labs/fairtrade/graph's
 * LOCAL_APP_SECTIONS: `home` and `settings` are nav sections; `analytics`,
 * `changes` and `code map` are reached by route only. This module adds only
 * the local route each id owns. `/share` is not a section: it stays outside
 * this registry.
 */

export interface NavSection {
  id: string;
  href: string;
  label: string;
  /** Hover description (also reusable as a palette hint). */
  title?: string;
  /** The glyph the section's header link leads with. */
  icon?: LucideIcon;
  /**
   * The server-advertised capability token required to expose this section in
   * persistent chrome. Absent means always visible; present means the section
   * is discoverable only when the capability set contains this token. Direct
   * routes are unaffected — this gates discoverability, not reachability.
   */
  requiredCapability?: UICapabilityToken;
}

/** A registry section as this app wires it. `inNav` comes from fairtrade. */
export interface LocalSection extends NavSection {
  inNav: boolean;
}

type LocalSectionId = 'home' | 'settings' | 'analytics' | 'changes' | 'map';

type SectionRoute = Omit<NavSection, 'id' | 'label'>;

/**
 * The page each registry id owns in this app. `null` means the app has no page
 * for the section yet: the registry lists it, but a link to it would be dead,
 * so persistent chrome leaves it out until the page ships.
 */
const ROUTES: Record<LocalSectionId, SectionRoute | null> = {
  home: {
    href: '/',
    title: 'Your projects and the sessions recorded in them.',
  },
  // The local settings page has not shipped; the header gains its link when it does.
  settings: null,
  analytics: {
    href: '/analytics',
    title: 'Read project-level session volume, outcomes, duration, and contributor signals.',
  },
  changes: {
    href: '/review',
    title: 'The lines of work moving through a project.',
  },
  map: {
    href: '/map',
    title: 'See a project as a map of its code areas and how they connect.',
    // Only takes effect if the registry ever lists the code map in the nav again.
    requiredCapability: UI_CAPABILITY.codeMapNavigationV1,
  },
};

function assertKnownSection(id: string): asserts id is LocalSectionId {
  if (!Object.hasOwn(ROUTES, id)) {
    throw new Error(
      `Unknown local app section "${id}" from @peasant-labs/fairtrade/graph LOCAL_APP_SECTIONS in web/src/lib/nav/sections.ts. ` +
      `Map its route (or record that its page has not shipped) before this app can render it.`,
    );
  }
}

/** Every registry section this app has a page for, in fairtrade's order. */
export const LOCAL_SECTIONS: readonly LocalSection[] = LOCAL_APP_SECTIONS.flatMap((section) => {
  assertKnownSection(section.id);
  const route = ROUTES[section.id];
  if (route === null) return [];
  return [{ ...route, id: section.id, label: section.label, inNav: section.inNav !== false }];
});

/** The sections persistent chrome may link to: listed in the nav and routed here. */
export const NAV_SECTIONS: readonly NavSection[] = LOCAL_SECTIONS.filter((section) => section.inNav);

/**
 * The sections reached by URL only. Their routes stay mounted; nothing in the
 * header or the palette links to them.
 */
export const ROUTE_ONLY_SECTIONS: readonly NavSection[] = LOCAL_SECTIONS.filter((section) => !section.inNav);

/**
 * Whether a section's discoverability requirement is met by the advertised
 * capability set. A section with no `requiredCapability` is always visible; one
 * with a requirement is visible only when the set contains that exact token.
 */
function sectionMeetsCapability(section: NavSection, capabilities: ReadonlySet<string>): boolean {
  return section.requiredCapability === undefined || capabilities.has(section.requiredCapability);
}

/** The nav sections to expose given the server's advertised capability set. */
export function visibleNavSections(capabilities: ReadonlySet<string>): NavSection[] {
  return NAV_SECTIONS.filter((section) => sectionMeetsCapability(section, capabilities));
}

/**
 * The links the header carries beside its brand: the visible nav sections
 * other than the one that owns `/`, which the `peasant` brand link already is.
 */
export function headerNavSections(capabilities: ReadonlySet<string>): NavSection[] {
  return visibleNavSections(capabilities).filter((section) => section.href !== '/');
}

/**
 * Whether `pathname` is within a section: the section's own page or anything
 * under it. Home owns `/` exactly.
 */
export function isSectionActive(section: NavSection, pathname: string): boolean {
  const path = pathname.replace(/\/+$/, '') || '/';
  if (section.href === '/') return path === '/';
  return path === section.href || path.startsWith(`${section.href}/`);
}
