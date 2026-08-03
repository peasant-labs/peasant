import {
  createCodeMapState,
  type CodeMapPresentation,
  type CodeMapState,
} from '@peasant-labs/fairtrade/graph';
import {
  formatMapRouteState,
  parseMapRouteState,
  type ProjectHash,
} from '@/lib/navigation/projectRoutes';

export type CodeMapLocationState = {
  projectHash: ProjectHash;
  state: CodeMapState;
};

function isFairtradeStateNormalizationError(error: unknown): error is Error {
  return error instanceof Error
    && /^Code map (?:create|normalize) state failed:/.test(error.message)
    && error.message.includes('where: @peasant-labs/fairtrade/graph ');
}

/**
 * Translate Peasant's shareable URL into Fairtrade's canonical interaction
 * state. Peasant owns route identity and history; Fairtrade owns every map
 * interaction field carried by that route.
 */
export function parseCodeMapLocation(
  pathname: string,
  search: string | URLSearchParams,
): CodeMapLocationState | null {
  const route = parseMapRouteState(pathname, search);
  if (!route) return null;

  try {
    return {
      projectHash: route.projectHash,
      state: createCodeMapState({
        presentation: route.mode,
        selectedId: route.node,
        grain: route.grain,
        expandedIds: Array.from(route.expanded),
        navigatorFilter: route.filter,
        navigatorFocusedId: route.focus,
        viewport: route.viewport,
      }),
    };
  } catch (error) {
    // A URL is an untrusted hydration boundary. Fairtrade's actionable state
    // errors mean this history entry cannot be mounted and should take the
    // existing route-repair path; unrelated application failures still surface.
    if (isFairtradeStateNormalizationError(error)) return null;
    throw error;
  }
}

/** Build the one default used when a mounted route has no usable state. */
export function defaultCodeMapState(presentation: CodeMapPresentation): CodeMapState {
  return createCodeMapState({ presentation });
}

/** Serialize only canonical Fairtrade state back into Peasant's route shape. */
export function formatCodeMapLocation(projectHash: ProjectHash, state: CodeMapState): string {
  const canonical = createCodeMapState(state);
  return formatMapRouteState({
    projectHash,
    node: canonical.selectedId,
    mode: canonical.presentation,
    grain: canonical.grain,
    expanded: canonical.expandedIds,
    filter: canonical.navigatorFilter,
    focus: canonical.navigatorFocusedId,
    viewport: canonical.viewport,
  });
}
