/**
 * The grouped sync list the share chooser reads (`GET /api/v1/sync/sessions`),
 * built from the flat session rows of a visual fixture. A script that draws the
 * chooser answers that route with this, so the chooser lists the fixture's
 * sessions and never the sessions of whatever store the server was started on.
 * The route, shell, chooser, hierarchy and helper-group code remain real.
 */
export function groupedSyncPayload(sessions) {
  const items = sessions.map(({ helperGroups, ...session }) => ({
    kind: 'transcript',
    transcript: {
      session,
      sync: {
        id: session.id,
        harness: session.harness,
        projectName: session.project,
        projectHash: session.projectHash,
        hostSlug: 'visual-host',
        startTime: session.startTime,
        durationMs: Math.round(session.durationMins * 60000),
        totalTokens: session.totalTokens,
        turnCount: session.turnCount,
        model: 'visual-model',
        syncStatus: 'new',
        previouslyPushed: false,
      },
    },
    helperGroups: (helperGroups || []).map((group) => ({ ...group, purpose: 'helper_review' })),
  }))
  return {
    items,
    // The one page the chooser asks for holds every fixture session: nothing follows it.
    nextItems: [],
    page: 1,
    perPage: 25,
    revision: 'visual-fixture',
    total: items.length,
    ordinarySessionTotal: sessions.length,
    helperThreadTotal: sessions.reduce((total, session) => total + (session.helperGroups || []).reduce((count, group) => count + group.helperThreadCount, 0), 0),
  }
}
