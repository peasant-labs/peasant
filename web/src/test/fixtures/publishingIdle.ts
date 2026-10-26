/**
 * A stand-in for the Local API calls of `@/lib/share/publishing`, for suites
 * whose subject is not the publish flow. Every call stays pending, so the
 * transcript page renders its header with the more menu and no publish status,
 * and the suite never reaches a real Local API: a Peasant server running on
 * this machine cannot leak into its results.
 *
 *   vi.mock('@/lib/share/publishing', async (importOriginal) => ({
 *     ...(await importOriginal<typeof import('@/lib/share/publishing')>()),
 *     ...(await import('@/test/fixtures/publishingIdle')).PUBLISHING_IDLE,
 *   }));
 */

function pending<T>(): Promise<T> {
  return new Promise<T>(() => {});
}

export const PUBLISHING_IDLE = {
  fetchVillageAuth: pending,
  startVillageSignIn: pending,
  fetchPublication: pending,
  fetchPublicationState: pending,
  fetchVillageCollectives: pending,
  publishSessions: pending,
};
