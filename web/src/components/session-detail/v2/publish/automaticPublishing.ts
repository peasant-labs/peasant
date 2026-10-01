import type { AutoPublishRuleRequest } from '@peasant-labs/schema';
import { installAutoPublishRule, saveAutoPublishRule } from '@/lib/api/settings';

/** A local setup failure after the requested transcript publication completed. */
export class AutomaticPublishingError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'AutomaticPublishingError';
  }
}

/**
 * Explicit repository consent uses the stored session target. The server owns
 * resolution and matching; installation uses only its confirmed repository.
 * The same rule id is retained for a retry of a partially completed setup.
 */
export async function enableAutomaticPublishing(ruleId: string, sessionId: string, collectives: readonly string[]): Promise<void> {
  const request: AutoPublishRuleRequest = { sessionId, events: ['pre-push'], collectives: [...collectives] };
  let rule;
  try {
    rule = await saveAutoPublishRule(ruleId, request);
  } catch (error) {
    throw new AutomaticPublishingError(`saving the automatic publishing rule: ${error instanceof Error ? error.message : String(error)}. the transcript was published; check the rule in settings before retrying`);
  }
  const audience = new Set(collectives);
  if (rule.id !== ruleId || rule.events.length !== 1 || rule.events[0] !== 'pre-push'
    || rule.collectives.length !== audience.size || rule.collectives.some((id) => !audience.has(id))) {
    throw new AutomaticPublishingError('confirming automatic publishing consent: peasant returned a different rule, event, or collective audience. the transcript was published; review the saved rule in settings');
  }
  if (rule.kind !== 'folder' || rule.repositories.length !== 1) {
    throw new AutomaticPublishingError('confirming the repository: the rule was saved, but peasant did not confirm exactly one repository for this stored session. the transcript was published; review the rule in settings');
  }
  let repository;
  try {
    repository = await installAutoPublishRule(rule.id, rule.repositories[0].path);
  } catch (error) {
    throw new AutomaticPublishingError(`installing the automatic publishing hook: ${error instanceof Error ? error.message : String(error)}. the transcript was published and the rule was saved; check its hook in settings before retrying`);
  }
  const hook = repository.hooks.find((item) => item.event === 'pre-push');
  if (repository.path !== rule.repositories[0].path || hook?.status !== 'installed') {
    const remedy = hook?.remedy?.message;
    throw new AutomaticPublishingError(`installing the automatic publishing hook: ${remedy ?? 'peasant did not confirm an installed pre-push hook'}. the transcript was published and the rule was saved; review its hook in settings`);
  }
}
