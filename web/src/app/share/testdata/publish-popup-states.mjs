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

const CASE_FIELDS = ['name', 'wireframe', 'publication', 'signIn', 'village', 'scan', 'push', 'steps', 'expect']
const ROOT_FIELDS = ['requiredNames', 'wizardLinks', 'collectives', 'matches', 'pullRequest', 'cases']
const SIGN_IN = ['signed-in', 'signed-out', 'waits']
const SCAN = ['matches', 'failure', 'pending']
const PUSH = /^(published|pending|approval|stopped:.+)$/
const STEP = /^(open|connect|publish|add .+|remove .+)$/
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
  exactFields(bar, ['state', 'text', 'action'], ['state', 'text', 'action'], `${where}.bar`)
  oneOf(bar.state, BAR_STATES, `${where}.bar.state`)
  text(bar.text, `${where}.bar.text`)
  oneOf(bar.action, ACTIONS, `${where}.bar.action`)
  let popup = null
  if (expect.popup !== null) {
    popup = record(expect.popup, `${where}.popup`)
    exactFields(popup, ['heading', 'texts', 'primary'], ['heading', 'texts', 'primary'], `${where}.popup`)
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
    exactFields(collective, ['name', 'members', 'acceptance', 'suggestion'], ['name', 'members', 'acceptance'], where)
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
    const fields = ['category', 'rule', 'display', 'original', 'replacement', 'entryIndex']
    exactFields(match, fields, fields, where)
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
    text(entry.wireframe, `${where}.wireframe`)
    const publication = record(entry.publication, `${where}.publication`)
    exactFields(publication, ['state', 'outsideSelection', 'autoPublish', 'newTurns', 'audience'], ['state'], `${where}.publication`)
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
    names(entry.village, catalog, `${where}.village`)
    oneOf(entry.scan, SCAN, `${where}.scan`)
    if (typeof entry.push !== 'string' || !PUSH.test(entry.push)) fail(`${where}.push`, `unknown push ${JSON.stringify(entry.push)}`)
    if (entry.push.startsWith('stopped:')) names([entry.push.slice('stopped:'.length)], catalog, `${where}.push`)
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
 * answers. A push changes what the next publication read returns.
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
  let signedIn = entry.signIn === 'signed-in'
  const world = { pushRequests: [], scanRequests: 0, respond }

  function group(collective) {
    return {
      ...GROUP_BASE,
      acceptance_mode: collective.acceptance,
      id: collective.id,
      linked_github_org: collective.suggestion?.reason === 'linked_github_org' ? collective.suggestion.match : null,
      member_count: collective.members,
      name: collective.name,
    }
  }

  function publicationRow(withAudience) {
    const row = { sessionId, state: publication.state, outsideSelection: publication.outsideSelection, autoPublish: publication.autoPublish }
    if (publication.state === 'published') {
      Object.assign(row, { transcriptId: FIXTURE_TRANSCRIPT_ID, transcriptUrl: FIXTURE_TRANSCRIPT_URL, publishedAt: publication.publishedAt })
      if (withAudience) row.audience = publication.audience
    }
    return row
  }

  function redactions() {
    const categories = []
    for (const match of fixture.matches) {
      let category = categories.find((entry) => entry.category === match.category)
      if (!category) categories.push(category = { category: match.category, totalCount: 0, rules: [] })
      category.totalCount += 1
      category.rules.push({
        ruleId: match.rule,
        displayName: match.display,
        count: 1,
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
    return { total: fixture.matches.length, categories }
  }

  function push(body) {
    world.pushRequests.push(body)
    const add = body.collectives?.add ?? []
    const remove = body.collectives?.remove ?? []
    const stoppedAt = entry.push.startsWith('stopped:') ? byName.get(entry.push.slice('stopped:'.length)).id : null
    const steps = [{ step: 'content', outcome: 'succeeded' }]
    for (const id of remove) steps.push({ step: 'remove_collective', collectiveId: id, outcome: 'succeeded' })
    let failed = false
    for (const id of add) {
      if (failed) {
        steps.push({ step: 'add_collective', collectiveId: id, outcome: 'not_attempted' })
        continue
      }
      if (id === stoppedAt) {
        failed = true
        steps.push({ step: 'add_collective', collectiveId: id, outcome: 'failed', reason: 'sharing with this collective failed, so it cannot read the transcript: village answered 502' })
        continue
      }
      const curated = byId.get(id)?.acceptance === 'curated'
      steps.push({ step: 'add_collective', collectiveId: id, outcome: entry.push === 'approval' && curated ? 'pending_approval' : 'succeeded' })
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
      status: failed ? 'error' : wasPublished ? 'updated' : 'new',
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
    if (failed) result.error = `stopped at sharing with collective ${stoppedAt}: sharing with this collective failed. Village kept the transcript at ${FIXTURE_TRANSCRIPT_URL}. Publish again to retry.`
    return {
      new: !failed && !wasPublished ? 1 : 0,
      updated: !failed && wasPublished ? 1 : 0,
      skipped: 0,
      errors: failed ? 1 : 0,
      sessions: [result],
    }
  }

  /** Answer one request: `{ status, json }`, `{ status, text }`, `{ pending: true }`, or null. */
  function respond({ method, url, body }) {
    const parsed = new URL(url, 'http://peasant.local')
    const route = `${method} ${parsed.pathname.replace(/\/$/, '')}`
    switch (route) {
      case 'GET /api/v1/sync/auth':
        return { status: 200, json: signedIn ? { authenticated: true, username: 'alice-dev', villageUrl: FIXTURE_VILLAGE, villageConfigured: true } : { authenticated: false } }
      case 'POST /api/v1/sync/login':
        if (entry.signIn === 'signed-out') signedIn = true
        return { status: 200, json: { status: 'pending' } }
      case 'GET /api/v1/publications':
        return { status: 200, json: { publications: [publicationRow(parsed.searchParams.get('include') === 'audience')] } }
      case 'GET /api/v1/village/collectives':
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
        world.scanRequests += 1
        if (entry.scan === 'pending') return { pending: true }
        if (entry.scan === 'failure') return { status: 500, text: 'the local scan could not read this session: open the Peasant log for the cause' }
        return { status: 200, json: redactions() }
      case 'POST /api/v1/sync/push':
        if (entry.push === 'pending') {
          world.pushRequests.push(body)
          return { pending: true }
        }
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
