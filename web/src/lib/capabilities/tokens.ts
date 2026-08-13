/**
 * Peasant-owned UI capability tokens.
 *
 * The wire envelope (`uiCapabilities: string[]`, validated by
 * `zUICapabilitiesResponse` from `@peasant-labs/schema`) is owned by the schema
 * module; token identity, meaning, and lifecycle are owned here. This is the
 * closed inventory of tokens the web app understands — tokens advertised by the
 * server that are absent here parse fine and are ignored by exact-membership
 * predicates (fail closed on anything unrecognized).
 *
 * Tokens are revisioned: a change with incompatible semantics gets a new
 * `_vN` suffix rather than silently redefining an existing token.
 */
export const UI_CAPABILITY = {
  /**
   * Makes code-map entry points discoverable in persistent navigation and the
   * command palette. It does NOT gate the direct `/map` or `/projects` routes,
   * which stay reachable by URL regardless of this capability.
   */
  codeMapNavigationV1: 'code_map_navigation_v1',
} as const;

export type UICapabilityToken = (typeof UI_CAPABILITY)[keyof typeof UI_CAPABILITY];
