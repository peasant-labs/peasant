import { parseDocument } from 'yaml';
import { createPublishWorld } from './publish-popup-states.mjs';

const REQUIRED_AUTOMATIC_NAMES = [
  'manual-publish-does-not-create-a-rule',
  'server-effective-keep-local-overrides-saved-intent',
  'preselected-intent-can-be-declined',
  'checked-confirmation-uses-stored-session-and-confirmed-root',
  'blocked-hook-keeps-publication-and-offers-settings',
  'failed-hook-keeps-publication-and-offers-settings',
  'save-failure-installs-nothing',
  'malformed-save-response-installs-nothing',
  'install-request-failure-does-not-claim-automatic-publishing',
  'held-publication-does-not-save-consent',
  'content-failure-does-not-save-consent',
  'existing-active-rule-is-managed-in-settings',
  'pending-install-outlives-navigation-without-duplicate-publish',
  'failed-preference-read-recovers-before-confirmation',
  'saved-rule-id-must-match-this-consent',
  'saved-rule-events-must-match-this-consent',
  'saved-rule-audience-must-match-this-consent',
  'paused-saved-rule-cannot-enable-automatic-publishing',
  'duplicate-saved-audience-cannot-replace-requested-members',
  'declined-requested-share-does-not-save-automatic-consent',
  'pending-approval-share-retains-explicit-automatic-consent',
];
export function loadAutomaticConsent(source, fixture) {
  const document = parseDocument(source, { strict: true, uniqueKeys: true });
  if (document.errors.length || /^---\s*$/m.test(source)) throw new Error('automatic consent fixture requires one valid YAML document');
  const root = document.toJS();
  function fields(value, names, required = names) {
    if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).some((key) => !names.includes(key)) || required.some((key) => !(key in value))) throw new Error('automatic consent fixture fields differ');
  }
  fields(root, ['requiredNames', 'repository', 'cases']);
  if (!Array.isArray(root.requiredNames) || [...root.requiredNames].sort().join() !== [...REQUIRED_AUTOMATIC_NAMES].sort().join() || typeof root.repository !== 'string' || !root.repository || !Array.isArray(root.cases)) throw new Error('automatic consent fixture requires every named case and its repository');
  const names = new Set();
  for (const entry of root.cases) {
    fields(entry, ['name', 'intent', 'value', 'action', 'base', 'setup', 'leave', 'expect', 'add'], ['name', 'intent', 'value', 'action', 'base', 'setup', 'leave', 'expect']);
    if (entry.add !== undefined && (!Array.isArray(entry.add) || entry.add.some((name) => !fixture.collectives.some((item) => item.name === name)))) throw new Error(`invalid automatic consent additions ${entry.name}`);
    if (typeof entry.name !== 'string' || names.has(entry.name) || !REQUIRED_AUTOMATIC_NAMES.includes(entry.name)
      || ![true, false, 'error'].includes(entry.intent) || ![true, false, null].includes(entry.value)
      || !['none', 'toggle'].includes(String(entry.action)) || !['installed', 'blocked', 'failed', 'save-error', 'malformed', 'install-error', 'wrong-id', 'wrong-event', 'wrong-audience', 'wrong-paused', 'wrong-duplicate'].includes(String(entry.setup))
      || typeof entry.leave !== 'boolean' || typeof entry.expect !== 'string' || !entry.expect
      || !fixture.cases.some((base) => base.name === entry.base)) throw new Error(`invalid automatic consent case ${entry.name}`);
    names.add(entry.name);
  }
  if ([...names].sort().join() !== [...REQUIRED_AUTOMATIC_NAMES].sort().join()) throw new Error('automatic consent fixture is missing a required case');
  return root;
}

/** One contract-shaped API world, shared by mounted checks and real-binary captures. */
export function createAutomaticConsentWorld(fixture, automaticFixture, entry, { sessionId, turns }) {
    const base = fixture.cases.find((candidate) => candidate.name === entry.base);
    if (!base) throw new Error(`unknown automatic consent base ${entry.base}`);
    const chosen = [...fixture.collectives.slice(0, 2), ...(entry.add ?? []).map((name) => fixture.collectives.find((item) => item.name === name))];
    const world = createPublishWorld(fixture, base, { sessionId, turns });
    const respond = world.respond;
    const mutations = [];
    const order = [];
    let settingsReads = 0;
    let installed = false;
    world.respond = (request) => {
      const path = new URL(request.url, 'http://peasant.local').pathname;
      if (path === '/api/v1/settings' && request.method === 'GET') {
        settingsReads += 1;
        if (entry.intent === 'error' && settingsReads === 1) return { status: 503, json: { error: 'settings read unavailable' } };
        return { status: 200, json: { settings: [{ key: 'push.autoPublishIntent', kind: 'boolean', value: entry.value, effective: entry.intent === true, editable: true, inPeasantConfig: true, description: 'offer automatic publishing' }], autoPublish: [] } };
      }
      if (path.startsWith('/api/v1/settings/auto-publish/')) {
        mutations.push({ method: request.method, path, body: request.body });
        order.push(request.method);
        if (request.method === 'PUT') {
          if (entry.setup === 'save-error') return { status: 503, json: { error: 'rule save unavailable' } };
          if (entry.setup === 'malformed') return { status: 200, json: { id: path.split('/').at(-1) } };
          return { status: 200, json: {
            id: entry.setup === 'wrong-id' ? 'another-manual-rule' : path.split('/').at(-1), kind: 'folder', match: automaticFixture.repository,
            events: entry.setup === 'wrong-paused' ? [] : [entry.setup === 'wrong-event' ? 'post-commit' : 'pre-push'],
            collectives: entry.setup === 'wrong-duplicate' ? [fixture.collectives[0].id, fixture.collectives[0].id] : (entry.setup === 'wrong-audience' ? fixture.collectives : chosen).map((item) => item.id),
            repositories: [{ path: automaticFixture.repository, hooks: entry.setup === 'wrong-paused' ? [] : [{ event: entry.setup === 'wrong-event' ? 'post-commit' : 'pre-push', status: 'absent' }] }],
          } };
        }
        if (request.method === 'POST') {
          if (entry.setup === 'install-error') return { status: 503, json: { error: 'install request unavailable' } };
          installed = entry.setup === 'installed';
          const hook = entry.setup === 'blocked' || entry.setup === 'failed'
            ? { event: 'pre-push', status: entry.setup, remedy: { message: entry.expect } }
            : { event: 'pre-push', status: 'installed' };
          return { status: 200, json: { path: automaticFixture.repository, hooks: [hook] } };
        }
        throw new Error(`unexpected rule mutation ${request.method}`);
      }
      if (path === '/api/v1/sync/push') order.push('push');
      const answer = respond(request);
      if (path === '/api/v1/publications' && answer && 'json' in answer) {
        const value = answer.json;
        value.publications.forEach((row) => { row.autoPublish ||= installed; });
      }
      return answer;
    };
    return { world, mutations, order, base };
}
