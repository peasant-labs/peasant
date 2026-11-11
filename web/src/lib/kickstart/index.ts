/**
 * The kickstart client module: the HTTP client, the typed model, the one
 * definition of every closed set, the projections a page draws from, and the
 * live-session hook. The setup wizard and the standalone selection page both
 * import from here; neither re-derives anything the server owns.
 */

export * from './client';
export * from './model';
export * from './values';
export * from './view';
export { useKickstartSession, type KickstartEnding, type KickstartSessionApi } from './useKickstartSession';
export { useVillageSignIn, type VillageSignIn } from './useVillageSignIn';
export { useKickstartPreview, type KickstartPreview } from './usePreview';
export * from './entry';
