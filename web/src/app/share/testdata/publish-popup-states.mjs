/* The publish bar and popup states fixture: its strict loader, and the Local API
   a case serves.

   One module for both drivers of publish-popup-states.yaml: the mounted test
   (vitest, through a fetch stub) and the real-binary visual harness (puppeteer,
   through request interception). Each case becomes a small stateful "world"
   that answers the publishing routes the way the server does, so a publish
   changes what the next publication read says. Every answer is shaped to the
   Local API contract; the page decodes it with the published schema, so a case
   that drifted from the contract fails at the page's own trust boundary. */

import { parseDocument } from 'yaml'

const CASE_FIELDS = ['name', 'publication', 'signIn', 'village', 'scan', 'push', 'steps', 'expect']
const ROOT_FIELDS = ['requiredNames', 'wizardLinks', 'collectives', 'matches', 'pullRequest', 'cases']
const SIGN_IN = ['signed-in', 'signed-out', 'waits', 'login-failed', 'already-authenticated']
const SCAN = ['matches', 'failure', 'pending', 'matches-then-failure']
const PUSH = /^(published|pending|approval|unauthorized|held|content-failed|(stopped|stopped-once|skipped):.+)$/
const STEP = /^(arrive|open|connect|publish|retry|rescan|add .+|remove .+)$/
const AUDIENCE_READ = ['ok', 'unreachable', 'missing', 'failed']
const BAR_STATES = ['not-published', 'publishing', 'published', 'new-turns', 'auto-publish', 'outside-lists']
const ACTIONS = ['publish', 'update', 'manage']

/** The village the fixture's transcripts live on. */
export const FIXTURE_VILLAGE = 'https://village.peasantlabs.org'
/** The transcript a fixture publish creates. */
export const FIXTURE_TRANSCRIPT_ID = '3f9c0a17-5b2e-4c1d-9a8f-6d0e2b7ce21a'
export const FIXTURE_TRANSCRIPT_URL = `${FIXTURE_VILLAGE}/transcripts/${FIXTURE_TRANSCRIPT_ID}`

function fail(where, reason) {
  throw new Error(`publish-popup-states.yaml is invalid at ${where}: ${reason}; fix the fixture, then rerun the publish state tests`)
}

function record(value, where) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) fail(where, `expected a mapping, received ${JSON.stringify(value)}`)
  return value
}

function exactFields(value, allowed, required, where) {
  const unknown = Object.keys(value).filter((key) => !allowed.includes(key))
  if (unknown.length) fail(where, `unknown fields ${unknown.join(', ')}`)
  const missing = required.filter((key) => !(key in value))
  if (missing.length) fail(where, `missing fields ${missing.join(', ')}`)
}

function text(value, where) {
  if (typeof value !== 'string' || !value.trim()) fail(where, `expected a non-empty string, received ${JSON.stringify(value)}`)
  return value
}

function oneOf(value, allowed, where) {
  if (!allowed.includes(value)) fail(where, `expected one of ${allowed.join(', ')}, received ${JSON.stringify(value)}`)
  return value
}

function list(value, where) {
  if (!Array.isArray(value)) fail(where, `expected a list, received ${JSON.stringify(value)}`)
  return value
}

function names(value, catalog, where) {
  return list(value, where).map((name, index) => {
    const found = text(name, `${where}[${index}]`)
    if (!catalog.includes(found)) fail(`${where}[${index}]`, `no collective named ${JSON.stringify(found)} in the catalog`)
    return found
  })
}

function parseExpect(value, catalog, where) {
  const expect = record(value, where)
  exactFields(expect, ['bar', 'popup', 'request'], ['bar', 'popup'], where)
  const bar = record(expect.bar, `${where}.bar`)
  if ('alert' in bar) {
    // The bar could not read the publication: it shows the reason, no status.
    exactFields(bar, ['alert'], ['alert'], `${where}.bar`)
    text(bar.alert, `${where}.bar.alert`)
  } else {
    exactFields(bar, ['state', 'text', 'action'], ['state', 'text', 'action'], `${where}.bar`)
    oneOf(bar.state, BAR_STATES, `${where}.bar.state`)
    text(bar.text, `${where}.bar.text`)
    oneOf(bar.action, ACTIONS, `${where}.bar.action`)
  }
  let popup = null
  if (expect.popup !== null) {
    popup = record(expect.popup, `${where}.popup`)
    exactFields(popup, ['heading', 'texts', 'primary', 'link'], ['heading', 'texts', 'primary'], `${where}.popup`)
    if (popup.link !== undefined) {
      const link = record(popup.link, `${where}.popup.link`)
      exactFields(link, ['text', 'href'], ['text', 'href'], `${where}.popup.link`)
      text(link.text, `${where}.popup.link.text`)
      text(link.href, `${where}.popup.link.href`)
    }
    text(popup.heading, `${where}.popup.heading`)
    list(popup.texts, `${where}.popup.texts`).forEach((entry, index) => text(entry, `${where}.popup.texts[${index}]`))
    if (popup.primary !== null) {
      const primary = record(popup.primary, `${where}.popup.primary`)
      exactFields(primary, ['label', 'enabled'], ['label', 'enabled'], `${where}.popup.primary`)
      text(primary.label, `${where}.popup.primary.label`)
      if (typeof primary.enabled !== 'boolean') fail(`${where}.popup.primary.enabled`, 'expected a boolean')
    }
  }
  if (popup === null && 'request' in expect) fail(`${where}.request`, 'a case with no popup runs no push')
  if (popup !== null && !('request' in expect)) fail(`${where}.request`, 'a popup case says which push it ran, or null')
  let request = null
  if (expect.request != null) {
    const body = record(expect.request, `${where}.request`)
    exactFields(body, ['add', 'remove'], ['add', 'remove'], `${where}.request`)
    request = { add: names(body.add, catalog, `${where}.request.add`), remove: names(body.remove, catalog, `${where}.request.remove`) }
  }
  return { bar, popup, request }
}

/** Load and validate the fixture. Throws with the offending path. */
export function loadPublishStates(source) {
  if ((source.match(/^---\s*$/gm) ?? []).length > 0) fail('<root>', 'expected exactly one YAML document')
  const document = parseDocument(source, { strict: true, uniqueKeys: true })
  if (document.errors.length) fail('<root>', document.errors.map((error) => error.message).join('; '))
  const root = record(document.toJS(), '<root>')
  exactFields(root, ROOT_FIELDS, ROOT_FIELDS, '<root>')

  const requiredNames = list(root.requiredNames, 'requiredNames').map((name, index) => text(name, `requiredNames[${index}]`))
  if (new Set(requiredNames).size !== requiredNames.length) fail('requiredNames', 'names repeat')

  const wizardLinks = list(root.wizardLinks, 'wizardLinks').map((link, index) => {
    if (typeof link !== 'string') fail(`wizardLinks[${index}]`, 'expected a query string')
    return link
  })

  const collectives = list(root.collectives, 'collectives').map((value, index) => {
    const where = `collectives[${index}]`
    const collective = record(value, where)
    exactFields(collective, ['name', 'members', 'acceptance', 'suggestion', 'role'], ['name', 'members', 'acceptance'], where)
    text(collective.name, `${where}.name`)
    if (!Number.isSafeInteger(collective.members) || collective.members < 0) fail(`${where}.members`, 'expected a count')
    oneOf(collective.acceptance, ['open', 'verified_only', 'curated'], `${where}.acceptance`)
    if (collective.suggestion !== undefined) {
      const suggestion = record(collective.suggestion, `${where}.suggestion`)
      exactFields(suggestion, ['reason', 'match'], ['reason', 'match'], `${where}.suggestion`)
      oneOf(suggestion.reason, ['linked_repository', 'linked_github_org'], `${where}.suggestion.reason`)
      text(suggestion.match, `${where}.suggestion.match`)
    }
    return { ...collective, id: collectiveId(index) }
  })
  const catalog = collectives.map((collective) => collective.name)
  if (new Set(catalog).size !== catalog.length) fail('collectives', 'names repeat')

  const matches = list(root.matches, 'matches').map((value, index) => {
    const where = `matches[${index}]`
    const match = record(value, where)
    const fields = ['category', 'rule', 'display', 'original', 'replacement', 'entryIndex', 'count']
    exactFields(match, fields, fields.filter((field) => field !== 'count'), where)
    if (match.count !== undefined && (!Number.isSafeInteger(match.count) || match.count < 1)) fail(`${where}.count`, 'expected a positive occurrence count')
    oneOf(match.category, ['CREDENTIAL', 'PII', 'PATH', 'INTERNAL'], `${where}.category`)
    for (const field of ['rule', 'display', 'original', 'replacement']) text(match[field], `${where}.${field}`)
    if (!Number.isSafeInteger(match.entryIndex) || match.entryIndex < 0) fail(`${where}.entryIndex`, 'expected a turn index')
    return match
  })

  const pullRequest = record(root.pullRequest, 'pullRequest')
  exactFields(pullRequest, ['owner', 'name', 'number'], ['owner', 'name', 'number'], 'pullRequest')

  const cases = list(root.cases, 'cases').map((value, index) => {
    const where = `cases[${index}]`
    const entry = record(value, where)
    exactFields(entry, CASE_FIELDS, CASE_FIELDS, where)
    text(entry.name, `${where}.name`)
    const publication = record(entry.publication, `${where}.publication`)
    exactFields(publication, ['state', 'outsideSelection', 'autoPublish', 'newTurns', 'audience', 'audienceRead'], ['state'], `${where}.publication`)
    if (publication.audienceRead !== undefined) oneOf(publication.audienceRead, AUDIENCE_READ, `${where}.publication.audienceRead`)
    oneOf(publication.state, ['unpublished', 'published'], `${where}.publication.state`)
    for (const flag of ['outsideSelection', 'autoPublish']) {
      if (publication[flag] !== undefined && typeof publication[flag] !== 'boolean') fail(`${where}.publication.${flag}`, 'expected a boolean')
    }
    if (publication.state === 'published') {
      if (!Number.isSafeInteger(publication.newTurns) || publication.newTurns < 0) fail(`${where}.publication.newTurns`, 'a published case says how many turns followed it')
      names(publication.audience, catalog, `${where}.publication.audience`)
    } else if (publication.newTurns !== undefined || publication.audience !== undefined) {
      fail(`${where}.publication`, 'an unpublished case has no new turns and no audience')
    }
    oneOf(entry.signIn, SIGN_IN, `${where}.signIn`)
    if (entry.village !== 'unreachable') names(entry.village, catalog, `${where}.village`)
    oneOf(entry.scan, SCAN, `${where}.scan`)
    if (typeof entry.push !== 'string' || !PUSH.test(entry.push)) fail(`${where}.push`, `unknown push ${JSON.stringify(entry.push)}`)
    const pushTarget = entry.push.match(/^(?:stopped|stopped-once|skipped):(.+)$/)
    if (pushTarget) names([pushTarget[1]], catalog, `${where}.push`)
    list(entry.steps, `${where}.steps`).forEach((step, stepIndex) => {
      if (typeof step !== 'string' || !STEP.test(step)) fail(`${where}.steps[${stepIndex}]`, `unknown step ${JSON.stringify(step)}`)
      const collective = step.replace(/^(add|remove) /, '')
      if (collective !== step) names([collective], catalog, `${where}.steps[${stepIndex}]`)
    })
    return { ...entry, expect: parseExpect(entry.expect, catalog, `${where}.expect`) }
  })

  const caseNames = cases.map((entry) => entry.name)
  if (new Set(caseNames).size !== caseNames.length) fail('cases', 'names repeat')
  for (const name of requiredNames) if (!caseNames.includes(name)) fail('cases', `the required state ${JSON.stringify(name)} has no case`)
  for (const name of caseNames) if (!requiredNames.includes(name)) fail('requiredNames', `the case ${JSON.stringify(name)} is not a required state`)

  return { requiredNames, wizardLinks, collectives, matches, pullRequest, cases }
}

/** The fixture's collective identifier at a catalog position: a valid v4 UUID. */
export function collectiveId(index) {
  return `6c0a1e5d-2b4f-4a7c-8e1d-${String(index + 1).padStart(12, '0')}`
}

const GROUP_BASE = {
  created_at: '2026-06-01T09:00:00Z',
  created_by: '0b6f8a2e-1c3d-4e5f-8a9b-0c1d2e3f4a5b',
  data_access: 'members_only',
  description: null,
  display_members: true,
  member_since: '2026-06-02T09:00:00Z',
  role: 'member',
  transcript_count: 0,
  transcript_deletion_policy: 'user_choice',
  updated_at: '2026-06-02T09:00:00Z',
}

/**
 * When the publication was made, so that exactly `newTurns` of the page's turns
 * follow it: the midpoint between the last turn it held and the first one after.
 */
export function publishedAtFor(turns, newTurns) {
  const times = turns.map((turn) => Date.parse(turn.timestamp)).filter((time) => !Number.isNaN(time)).sort((a, b) => a - b)
  if (times.length <= newTurns) throw new Error(`a publication followed by ${newTurns} new turns needs more than ${newTurns} timed turns; the session has ${times.length}`)
  const held = times[times.length - 1 - newTurns]
  const next = newTurns === 0 ? held + 2000 : times[times.length - newTurns]
  if (next <= held) throw new Error(`turns ${times.length - 1 - newTurns} and ${times.length - newTurns} share a timestamp, so no publication time separates them`)
  return new Date(Math.floor((held + next) / 2)).toISOString()
}

/**
 * The Local API one case serves. `respond` answers a publishing route, or
 * returns null for any other route; `{ pending: true }` is a request that never
 * answers. It answers as the server does (internal/api/publishing_handlers.go,
 * sync_handler.go, internal/push/share_publish.go): a request with a missing or
 * wrong parameter is refused with the server's 400, a signed-out computer
 * reads every session unpublished and is refused the collectives and the push,
 * errors are JSON `{ error, code }`, and a push changes what the next
 * publication read says.
 */
export function createPublishWorld(fixture, entry, { sessionId, turns }) {
  const byName = new Map(fixture.collectives.map((collective) => [collective.name, collective]))
  const byId = new Map(fixture.collectives.map((collective) => [collective.id, collective]))
  const publication = {
    state: entry.publication.state,
    outsideSelection: entry.publication.outsideSelection ?? false,
    autoPublish: entry.publication.autoPublish ?? false,
    publishedAt: entry.publication.state === 'published' ? publishedAtFor(turns, entry.publication.newTurns) : undefined,
    audience: (entry.publication.audience ?? []).map((name) => ({ collectiveId: byName.get(name).id, name, status: 'approved' })),
  }
  const pushTarget = entry.push.match(/^(stopped|stopped-once|skipped):(.+)$/)
  const target = pushTarget ? { kind: pushTarget[1], id: byName.get(pushTarget[2]).id } : null
  let signedIn = entry.signIn === 'signed-in'
  let pushes = 0
  const world = { pushRequests: [], scanRequests: 0, respond }

  const refuse = (status, error, code) => ({ status, json: code ? { error, code } : { error } })

  function group(collective) {
    return {
      ...GROUP_BASE,
      acceptance_mode: collective.acceptance,
      role: collective.role ?? GROUP_BASE.role,
      id: collective.id,
      linked_github_org: collective.suggestion?.reason === 'linked_github_org' ? collective.suggestion.match : null,
      member_count: collective.members,
      name: collective.name,
    }
  }

  function publicationRow(withAudience) {
    const row = { sessionId, state: 'unpublished', outsideSelection: publication.outsideSelection, autoPublish: publication.autoPublish }
    // A signed-out computer holds no account to read receipts for.
    if (signedIn && publication.state === 'published') {
      Object.assign(row, { state: 'published', transcriptId: FIXTURE_TRANSCRIPT_ID, transcriptUrl: FIXTURE_TRANSCRIPT_URL, publishedAt: publication.publishedAt })
      if (withAudience) row.audience = publication.audience
    }
    return row
  }

  function redactions() {
    const categories = []
    for (const match of fixture.matches) {
      let category = categories.find((entry) => entry.category === match.category)
      if (!category) categories.push(category = { category: match.category, totalCount: 0, rules: [] })
      category.totalCount += match.count ?? 1
      category.rules.push({
        ruleId: match.rule,
        displayName: match.display,
        count: match.count ?? 1,
        items: [{
          category: match.category,
          ruleId: match.rule,
          ruleDisplayName: match.display,
          originalText: match.original,
          redactedReplacement: match.replacement,
          description: match.display,
          lineNumber: match.entryIndex + 1,
          contextBefore: [],
          contextAfter: [],
          entryIndex: match.entryIndex,
        }],
      })
    }
    return { total: categories.reduce((sum, category) => sum + category.totalCount, 0), categories }
  }

  function push(body) {
    pushes += 1
    const add = body.collectives?.add ?? []
    const remove = body.collectives?.remove ?? []
    const failsNow = target && (target.kind === 'stopped' || (target.kind === 'stopped-once' && pushes === 1))
    const contentSkipped = publication.state === 'published' && entry.publication.newTurns === 0;
    const unavailable = entry.push === 'held' || entry.push === 'content-failed';
    const steps = [{ step: 'content', outcome: entry.push === 'content-failed' ? 'failed' : unavailable || contentSkipped ? 'skipped' : 'succeeded',
      ...(unavailable ? { reason: entry.push === 'held' ? 'the session is held until ingest completes; nothing was sent' : 'sending the content failed: village answered 502' } : {}) }];
    if (unavailable) return { new: 0, updated: 0, skipped: entry.push === 'held' ? 1 : 0, errors: entry.push === 'content-failed' ? 1 : 0, sessions: [{ sessionId, status: entry.push === 'held' ? 'held' : 'error', steps }] };
    for (const id of remove) steps.push({ step: 'remove_collective', collectiveId: id, outcome: 'succeeded' })
    let failed = false
    for (const id of add) {
      if (failed) {
        steps.push({ step: 'add_collective', collectiveId: id, outcome: 'not_attempted' })
        continue
      }
      if (failsNow && id === target.id) {
        failed = true
        steps.push({ step: 'add_collective', collectiveId: id, outcome: 'failed', reason: 'sharing with this collective failed, so it cannot read the transcript: village answered 502' })
        continue
      }
      if (target?.kind === 'skipped' && id === target.id) {
        steps.push({ step: 'add_collective', collectiveId: id, outcome: 'skipped', reason: 'Village did not share the transcript with this collective: it shares only with a collective you are a member of that accepts your contributions' })
        continue
      }
      const curated = byId.get(id)?.acceptance === 'curated'
      steps.push({ step: 'add_collective', collectiveId: id, outcome: curated ? 'pending_approval' : 'succeeded' })
    }
    const wasPublished = publication.state === 'published'
    publication.state = 'published'
    publication.publishedAt = new Date(Math.max(...turns.map((turn) => Date.parse(turn.timestamp))) + 1000).toISOString()
    publication.audience = publication.audience.filter((member) => !remove.includes(member.collectiveId))
    for (const step of steps) {
      if (step.step !== 'add_collective' || !['succeeded', 'pending_approval'].includes(step.outcome)) continue
      if (publication.audience.some((member) => member.collectiveId === step.collectiveId)) continue
      publication.audience.push({ collectiveId: step.collectiveId, name: byId.get(step.collectiveId).name, status: step.outcome === 'succeeded' ? 'approved' : 'pending' })
    }
    const result = {
      sessionId,
      status: failed ? 'error' : contentSkipped ? 'skipped' : wasPublished ? 'updated' : 'new',
      steps,
      transcriptUrl: FIXTURE_TRANSCRIPT_URL,
      waitingPullRequests: [{
        owner: fixture.pullRequest.owner,
        name: fixture.pullRequest.name,
        number: fixture.pullRequest.number,
        remote: `${fixture.pullRequest.owner}/${fixture.pullRequest.name}`,
        head_remote: `${fixture.pullRequest.owner}/${fixture.pullRequest.name}`,
        requested_at: '2026-09-29T10:00:00Z',
        state: 'waiting',
      }],
    }
    if (failed) result.error = `stopped at sharing with collective ${target.id}: sharing with this collective failed. Village kept the transcript at ${FIXTURE_TRANSCRIPT_URL}. Publish again to retry.`
    return {
      new: !failed && !wasPublished ? 1 : 0,
      updated: !failed && wasPublished && !contentSkipped ? 1 : 0,
      skipped: !failed && contentSkipped ? 1 : 0,
      errors: failed ? 1 : 0,
      sessions: [result],
    }
  }

  /** Answer one request: `{ status, json }`, `{ pending: true }`, or null. */
  function respond({ method, url, body }) {
    const parsed = new URL(url, 'http://peasant.local')
    const query = parsed.searchParams
    const route = `${method} ${parsed.pathname.replace(/\/$/, '')}`
    switch (route) {
      case 'GET /api/v1/sync/auth':
        return { status: 200, json: signedIn ? { authenticated: true, username: 'alice-dev', villageUrl: FIXTURE_VILLAGE, villageConfigured: true } : { authenticated: false } }
      case 'POST /api/v1/sync/login':
        if (entry.signIn === 'login-failed') return refuse(502, 'github sign-in could not be started: connection refused')
        if (entry.signIn === 'signed-out' || entry.signIn === 'already-authenticated') signedIn = true
        return { status: 200, json: { status: entry.signIn === 'already-authenticated' ? 'already_authenticated' : 'pending' } }
      case 'GET /api/v1/publications': {
        if (query.get('sessionIds') !== sessionId) return refuse(400, 'The publication state could not be read because the request named no session in query field "sessionIds".', 'publications_session_ids_required')
        if (entry.publication.audienceRead === 'failed') return refuse(503, 'the local publication store could not be read; inspect the Peasant log')
        const include = query.get('include')
        if (include !== null && include !== 'audience') return refuse(400, `query field "include" is ${JSON.stringify(include)}, and the only value it takes is "audience".`, 'publications_include_unknown')
        if (include === 'audience' && signedIn && publication.state === 'published') {
          if (entry.publication.audienceRead === 'unreachable') return refuse(502, 'The publication state could not name who can read the transcript because Village could not be read.', 'village_unreachable')
          if (entry.publication.audienceRead === 'missing') return refuse(502, `Village no longer holds transcript ${FIXTURE_TRANSCRIPT_ID}, which this computer's receipt names. Run 'peasant village push --force' choosing only this session to publish it again.`, 'village_transcript_missing')
        }
        return { status: 200, json: { publications: [publicationRow(include === 'audience')] } }
      }
      case 'GET /api/v1/village/collectives':
        if (!signedIn) return refuse(401, 'The Village collectives could not be listed because this computer is not signed in to Village.', 'village_signed_out')
        if (query.get('sessionId') !== sessionId) return refuse(400, 'the collectives read names no session to suggest collectives for')
        if (entry.village === 'unreachable') return refuse(502, 'The Village collectives could not be listed because Village could not be read: connection refused.', 'village_unreachable')
        return {
          status: 200,
          json: {
            collectives: entry.village.map((name) => {
              const collective = byName.get(name)
              return collective.suggestion ? { group: group(collective), suggestion: collective.suggestion } : { group: group(collective) }
            }),
          },
        }
      case 'GET /api/v1/sync/redactions':
        if (query.get('session_id') !== sessionId || query.get('level') !== 'standard') return refuse(400, 'the redaction preview names no session or a level this version does not offer')
        world.scanRequests += 1
        if (entry.scan === 'pending') return { pending: true }
        if (entry.scan === 'failure' || (entry.scan === 'matches-then-failure' && world.scanRequests > 1)) {
          return refuse(500, 'the local scan could not read this session: open the Peasant log for the cause')
        }
        return { status: 200, json: redactions() }
      case 'POST /api/v1/sync/push':
        world.pushRequests.push(body)
        if (!signedIn || entry.push === 'unauthorized') return refuse(401, "not authenticated — run 'peasant village login' first")
        if (entry.push === 'pending') return { pending: true }
        return { status: 200, json: push(body) }
      default:
        return null
    }
  }

  return world
}

/** The push body a case expects, from its collective names. */
export function expectedPushBody(fixture, entry, sessionId) {
  const request = entry.expect.request
  if (!request) return null
  const id = (name) => fixture.collectives.find((collective) => collective.name === name).id
  const body = { sessionIds: [sessionId], redactionLevel: 'standard' }
  if (request.add.length || request.remove.length) {
    body.collectives = {
      ...(request.add.length ? { add: request.add.map(id) } : {}),
      ...(request.remove.length ? { remove: request.remove.map(id) } : {}),
    }
  }
  return body
}
