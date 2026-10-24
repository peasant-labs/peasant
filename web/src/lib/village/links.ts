/** The peasant online address, when the server does not name one. */
export function defaultOnlineUrl(): string {
  return process.env.NEXT_PUBLIC_COMMONS_URL ?? 'https://village.peasantlabs.org';
}

/** Where peasant online lists the collectives a reader can create or join. */
const ONLINE_COLLECTIVES_PATH = '/collectives';

/**
 * The collectives page on peasant online. The sign-in answer's `villageUrl` is
 * the village API base, not a page a reader can open, so this takes no address
 * from it.
 */
export function onlineCollectivesHref(): string {
  return `${defaultOnlineUrl()}${ONLINE_COLLECTIVES_PATH}`;
}

/** The peasant docs. */
export const DOCS_URL = 'https://peasantlabs.org/docs';
