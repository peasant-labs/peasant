import { closed } from '@peasant-labs/fairtrade/closed-value';

/**
 * Where the setup page lives and where a visit to it can come from. The page is
 * outside the section registry: no sidebar item or palette entry links to it.
 * Only the first-run redirect and the settings page send people here.
 */
export const KICKSTART_ROUTE = '/kickstart';
export const SELECTION_ROUTE = '/selection';

/** The query parameter that names where a visit came from. */
export const KICKSTART_ORIGIN_PARAM = 'from';

/** Where a visit to the setup page came from. Only the settings page names one. */
export const KickstartOrigin = Object.freeze({
  Settings: closed('settings', 'settings'),
} as const);
export type KickstartOrigin = (typeof KickstartOrigin)[keyof typeof KickstartOrigin];
export const ALL_KICKSTART_ORIGINS = Object.freeze([KickstartOrigin.Settings] as const);

const ORIGIN_BY_WIRE = new Map<string, KickstartOrigin>(ALL_KICKSTART_ORIGINS.map((origin) => [origin.wire, origin]));

/** The origin a URL names. A missing or unknown value is no origin: the page still opens. */
export function parseKickstartOrigin(raw: string | null | undefined): KickstartOrigin | null {
  return raw === null || raw === undefined ? null : ORIGIN_BY_WIRE.get(raw) ?? null;
}

/** The link to the setup page, with the origin it was followed from. */
export function kickstartHref(origin?: KickstartOrigin): string {
  return origin === undefined ? KICKSTART_ROUTE : `${KICKSTART_ROUTE}?${KICKSTART_ORIGIN_PARAM}=${origin.wire}`;
}
