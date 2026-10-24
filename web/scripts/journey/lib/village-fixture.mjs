import { createServer } from 'node:http';

/** Only the remote dependency is synthetic. Local API, credentials, Store and CLI remain real. */
export async function startVillageFixture(exported, contract, control) {
  const f = exported.fixtures;
  let scenario = 'complete', account = f.Accounts[0];
  const requests = [];
  const time = f.ObservedAt;
  const record = () => ({ id: f.CollectiveID, name: scenario === 'complete' ? f.OldName : f.NewName,
    description: 'Permitted fixture metadata retained as dated evidence', acceptance_mode: 'open', data_access: 'public',
    created_at: time, updated_at: time, created_by: account.UserID, display_members: true,
    linked_github_org: null, transcript_deletion_policy: 'user_choice' });
  const membership = () => ({ ...record(), role: 'member', member_since: time, member_count: 1, transcript_count: 1 });
  function validated(name, payload) {
    const validator = contract[name];
    if (!validator) throw new Error(`Published schema export ${name} is missing; the fixture cannot certify this wire response`);
    const parsed = validator.safeParse(payload);
    if (!parsed.success) throw new Error(`Remote fixture ${name} violates the canonical schema: ${parsed.error.message}`);
    return payload; // preserve JSON's number representation, not Zod's bigint coercions
  }
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://fixture');
    const send = (body, status = 200) => { res.writeHead(status, { 'content-type': 'application/json' }); res.end(JSON.stringify(body)); };
    try {
      if (url.pathname === '/__fixture/control') {
        if (req.method !== 'POST' || req.headers['x-fixture-control'] !== control.token) return send({ error: 'fixture control refused' }, 403);
        const action = url.searchParams.get('action');
        if (action === 'scenario') {
          const name = url.searchParams.get('name');
          if (!control.scenarios.includes(name)) return send({ error: 'unknown named fixture scenario' }, 400);
          scenario = name; return send({ scenario });
        }
        if (action === 'requests') return send(requests);
        if (action === 'reset') {
          await control.run('reset'); account = f.Accounts[0]; scenario = 'complete'; requests.length = 0; return send({ reset: true });
        }
        if (action === 'account') {
          const selected = f.Accounts.find(a => a.Name === url.searchParams.get('name'));
          if (!selected) return send({ error: 'unknown named fixture account' }, 400);
          await control.run('account', selected.Name); account = selected; scenario = 'complete'; return send({ account: selected.Name });
        }
        if (!control.actions.includes(action)) return send({ error: 'unknown named fixture operation' }, 400);
        return send(await control.run(action, url.searchParams));
      }
      const observedRequest = { method: req.method, path: url.pathname };
      requests.push(observedRequest);
      if (req.headers.authorization !== 'Bearer disposable-observations-fixture') return send({ error: 'fixture credentials refused' }, 401);
      if (req.method !== 'GET') return send({ error: 'remote fixture refuses all mutations' }, 405);
      const path = url.pathname;
      if (path === '/api/v1/auth/me') return send(validated('zVillageUser', {
        id: account.UserID, github_id: 1, github_username: account.Username, display_name: account.Username, avatar_url: null,
        created_at: time, updated_at: time, is_discoverable: true, provider: 'github', provider_user_id: account.UserID,
        username_chosen: true, provider_username: account.Username,
      }));
      if (path === '/api/v1/auth/orgs') return send([]);
      if (path === '/api/v1/collectives') return send([validated('zVillageUserCollective', membership())]);
      if (path === '/api/v1/users/me/collectives/contributions') return send(validated('zVillageContributedCollectivesResponse', {
        collectives: [{ id: f.CollectiveID, name: record().name, description: null, linked_github_org: null,
          approved_count: 0, pending_count: 1, rejected_attempt_count: 2, withdrawn_attempt_count: 2 }],
      }));
      if (path === `/api/v1/collectives/${f.CollectiveID}`) return send(validated('zVillageCollectiveDetailResponse', {
        group: record(), can_read: true, your_role: 'member', members: [], contributors: [], models: [], transcripts: [],
        stats: { contributor_count: 0, pull_request_count: 0, total_duration_ms: 0, total_tokens: 0, total_transcripts: 0, total_turns: 0 },
      }));
      if (path === `/api/v1/users/me/collectives/${f.CollectiveID}/submissions`) return send([
        validated('zVillageCollectiveSubmission', { transcript_id: f.TranscriptID, group_id: f.CollectiveID, title: 'Other-device contribution', event_num: 5, status: 'pending', recorded_at: '2026-10-14T10:00:00Z' }),
      ]);
      if (path.endsWith(`/transcripts/${f.TranscriptID}/events`)) {
        if (scenario === 'partial-502') return send({ error: 'owner history fixture unavailable' }, 503);
        if (scenario === 'remote-history-504') return send({ error: 'remote owner history gateway timeout' }, 504);
        if (scenario === 'partial-504' || scenario === 'late-history') {
          observedRequest.stalled = true;
          // The real client deadline closes this response; no synthetic local status or timer.
          await new Promise(done => res.once('close', () => { observedRequest.cancelled = !res.writableEnded; done(); }));
          return;
        }
        return send(JSON.parse(f.AttemptsJSON).map(row => validated('zVillageShareEvent', row)));
      }
      if (path === '/api/v1/schema/version') return send(validated('zSchemaVersionResponse', exported.version));
      if (path === `/api/v1/pull/transcripts/${f.TranscriptID}`) return send(validated('zPullTranscriptInfo', exported.metadata));
      if (path === `/api/v1/pull/transcripts/${f.TranscriptID}/annotations`) return send([]);
      if (path === `/api/v1/pull/transcripts/${f.TranscriptID}/content`) {
        validated('zTranscriptContent', exported.content);
        const bytes = Buffer.from(exported.contentBytesBase64, 'base64');
        validated('zTranscriptContent', JSON.parse(bytes.toString()));
        if (JSON.stringify(JSON.parse(bytes.toString())) !== JSON.stringify(exported.content)) throw new Error('Canonical fixture bytes no longer describe the exported content');
        res.writeHead(200, { 'content-type': 'application/json', etag: `"${exported.metadata.contentHash}"` }); res.end(bytes); return;
      }
      if (path === `/api/v1/transcripts/${f.TranscriptID}/collectives`) {
        if (scenario === 'association-unavailable') return send({ error: 'optional associations unavailable' }, 503);
        if (scenario === 'association-invalidated') return send({ error: 'resource unavailable for this account' }, 403);
        return send(validated('zVillageTranscriptCollectivesResponse', { collectives: JSON.parse(f.AssociationJSON) }));
      }
      return send({ error: 'no documented focused Village fixture route' }, 404);
    } catch (error) { send({ error: error.message }, 500); }
  });
  await new Promise((done, fail) => { server.once('error', fail); server.listen(0, '127.0.0.1', done); });
  return {
    origin: `http://127.0.0.1:${server.address().port}`, requests,
    async close() { server.closeAllConnections(); await new Promise(done => server.close(done)); },
  };
}
