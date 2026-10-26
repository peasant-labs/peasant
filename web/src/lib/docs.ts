import { DOCS_URL } from '@/lib/village/links';

/**
 * Where each how-to lives in the docs. Instructions live only there: the app
 * links to the page or section that teaches a task instead of restating its
 * commands or steps.
 */
export const DOCS = {
  /** `peasant harvest` (alias `ingest`): scan this computer for agent sessions. */
  record: `${DOCS_URL}#record`,
  /** `peasant kickstart`: choose which projects peasant records. */
  selection: `${DOCS_URL}#kickstart`,
  /** Removing recorded sessions: `peasant prune` (and what `kickstart --reset` deletes). */
  prune: `${DOCS_URL}#prune`,
  /** Automatic publishing: rules, the publish popup's tick, `/peasant auto`, the git hook. */
  autoPublish: `${DOCS_URL}#auto-publish`,
  /** Signing in to peasant online from this computer, and signing out. */
  signIn: `${DOCS_URL}#sign-in`,
  /** config.yaml and hooks.yaml: where they live, and `peasant config`. */
  settingsFiles: `${DOCS_URL}#configuration`,
  /** Reading the code map. */
  codeMap: `${DOCS_URL}#code-map`,
  /** Reading changes: lines of work and the sessions behind them. */
  changes: `${DOCS_URL}#changes`,
} as const;

export type DocsTopic = keyof typeof DOCS;
