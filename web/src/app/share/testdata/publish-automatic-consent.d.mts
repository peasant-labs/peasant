import type { PublishStatesFixture, PublishStatesCase, PublishWorld } from './publish-popup-states.mjs';
export interface AutomaticConsentCase {
  name: string;
  intent: boolean | 'error';
  value: boolean | null;
  action: 'none' | 'toggle';
  base: string;
  setup: 'installed' | 'blocked' | 'failed' | 'save-error' | 'malformed' | 'install-error' | 'wrong-id' | 'wrong-event' | 'wrong-audience' | 'wrong-paused' | 'wrong-duplicate';
  leave: boolean;
  expect: string;
  add?: string[];
}
export interface AutomaticConsentFixture { repository: string; cases: AutomaticConsentCase[] }
export function loadAutomaticConsent(source: string, fixture: PublishStatesFixture): AutomaticConsentFixture;
export function createAutomaticConsentWorld(fixture: PublishStatesFixture, automaticFixture: AutomaticConsentFixture, entry: AutomaticConsentCase, context: { sessionId: string; turns: readonly { timestamp: string }[] }): {
  world: PublishWorld;
  mutations: { method: string; path: string; body: unknown }[];
  order: string[];
  base: PublishStatesCase;
};
