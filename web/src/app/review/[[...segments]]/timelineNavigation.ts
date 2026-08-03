import {
  assertTimelineNavigationAction,
  type TimelineNavigationAction,
} from '@peasant-labs/fairtrade/graph';
import {
  mapHref,
  returnLocation as makeReturnLocation,
  reviewHref,
  RouteOrigin,
  transcriptHref,
  type ProjectHash,
  type ReturnLocation,
} from '@/lib/navigation/projectRoutes';

export type TimelineNavigationContext = {
  projectHash: ProjectHash;
  defaultBranch: string | null | undefined;
  returnLocation: ReturnLocation | null;
  pagination: {
    cursorAvailable: boolean;
    handlerAvailable: boolean;
  };
};

export type TimelineNavigationCommand =
  | { kind: 'navigate'; href: string }
  | { kind: 'show-older' }
  | { kind: 'stay' };

/**
 * Resolve Fairtrade's semantic timeline gesture into one host command. This is
 * the only timeline-to-route boundary: components never infer routing from DOM
 * text, callback timing, or a prior gesture.
 */
export function resolveTimelineNavigation(
  value: unknown,
  context: TimelineNavigationContext,
): TimelineNavigationCommand {
  assertTimelineNavigationAction(value);
  const action: TimelineNavigationAction = value;

  switch (action.type) {
    case 'open-change':
      return action.change.branch && action.change.branch !== context.defaultBranch
        ? {
            kind: 'navigate',
            href: reviewHref(context.projectHash, {
              branch: action.change.branch,
              returnLocation: context.returnLocation ?? undefined,
            }),
          }
        : { kind: 'stay' };

    case 'open-session': {
      const currentReview = makeReturnLocation(reviewHref(context.projectHash));
      return {
        kind: 'navigate',
        href: transcriptHref(context.projectHash, action.sessionId, {
          origin: context.returnLocation?.origin ?? RouteOrigin.Review,
          originBranch:
            action.source.kind === 'commit'
              ? action.source.commit.branch ?? undefined
              : undefined,
          returnLocation: context.returnLocation ?? currentReview ?? undefined,
        }),
      };
    }

    case 'open-map':
      return {
        kind: 'navigate',
        href:
          context.returnLocation?.origin === RouteOrigin.Map
            ? context.returnLocation.href
            : mapHref(context.projectHash),
      };

    case 'show-older':
      if (!context.pagination.cursorAvailable || !context.pagination.handlerAvailable) {
        throw new Error([
          'Older timeline entries are unavailable.',
          'What went wrong: Fairtrade requested timeline pagination, but this Peasant route has no complete older-page capability.',
          'Why it happened: a real review API cursor and its host handler must both be available before show-older can run.',
          'Where: resolveTimelineNavigation, the Peasant timeline host boundary.',
          'When: while resolving a show-older user gesture.',
          'What it means: the current visible timeline remains unchanged and no route is written.',
          'How to fix: provide both a review-list pagination cursor and its handler before setting Changes.hasMore.',
        ].join('\n'));
      }
      return { kind: 'show-older' };
  }

  const unreachable: never = action;
  return unreachable;
}
