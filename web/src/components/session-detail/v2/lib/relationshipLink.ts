import {
  RelationshipNavigationStatus,
  isSessionRelationshipKind,
  type SessionRelationshipNavigation,
} from '@peasant-labs/schema';
import { transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';

/**
 * The host's route decision for one authorized relationship navigation.
 *
 * The one adapter hands the callback exactly the entries it cooked: `resolved`
 * when the navigation carried a verified public anchor, and `general_link_only`
 * for the ordinary current-target link, which is what the local read produces
 * because it authorizes a stored target without publishing a source anchor.
 * Both are real links and open the target's own transcript route. The
 * unavailable, inaccessible, unknown, and conflicting states never produce a
 * route, so an honest status can never send a reader to the wrong session.
 *
 * The returned route is the target's current session route under this host's
 * project segment: an authorized navigation names a session, and the local
 * transcript route addresses one by its stored identifier.
 */
export function relationshipLinkHref(
  projectHash: ProjectHash,
  navigation: SessionRelationshipNavigation,
): string | null {
  const linkable =
    navigation.status === RelationshipNavigationStatus.Resolved ||
    navigation.status === RelationshipNavigationStatus.GeneralLinkOnly;
  if (!linkable) return null;
  if (!isSessionRelationshipKind(navigation.kind)) return null;
  if (!navigation.localId) return null;
  return transcriptHref(projectHash, navigation.localId);
}
