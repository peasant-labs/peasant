import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  Harness,
  newProjectHash,
  zReviewListPayload,
  zSessionDetailPayload,
  zSessionSummary,
} from '@peasant-labs/schema'
import { parseDocument } from 'yaml'

const HERE = dirname(fileURLToPath(import.meta.url))
const FIXTURE_PATH = resolve(HERE, '../../../internal/mock/testdata/strike_mounted_web.yaml')

function record(value, path) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${path} must be a mapping`)
  }
  return value
}

function exactFields(value, fields, path) {
  const keys = Object.keys(value).sort()
  const expected = [...fields].sort()
  if (keys.length !== expected.length || keys.some((key, index) => key !== expected[index])) {
    throw new Error(`${path} fields ${JSON.stringify(keys)} do not exactly match ${JSON.stringify(expected)}`)
  }
}

function requiredString(value, field, path) {
  if (typeof value[field] !== 'string' || value[field].length === 0) {
    throw new Error(`${path}.${field} must be a non-empty string`)
  }
  return value[field]
}

function requiredInteger(value, field, path) {
  if (!Number.isSafeInteger(value[field])) {
    throw new Error(`${path}.${field} must be a safe integer`)
  }
  return value[field]
}

export function loadStrikeSmokeFixture() {
  try {
    const document = parseDocument(readFileSync(FIXTURE_PATH, 'utf8'), {
      prettyErrors: true,
      strict: true,
      uniqueKeys: true,
    })
    if (document.errors.length > 0) throw document.errors[0]
    if (document.contents === null) throw new Error('fixture document is empty')
    const root = record(document.toJS(), 'Strike smoke fixture')
    exactFields(root, ['project', 'sessionDetail', 'mapSession', 'mapCommit', 'reviewList', 'expected'], 'Strike smoke fixture')

    const project = record(root.project, 'Strike smoke fixture.project')
    exactFields(project, ['hash', 'name'], 'Strike smoke fixture.project')
    const projectHash = newProjectHash(requiredString(project, 'hash', 'Strike smoke fixture.project'))
    const projectName = requiredString(project, 'name', 'Strike smoke fixture.project')
    const sessionDetail = zSessionDetailPayload.strict().parse(root.sessionDetail)
    const mapSession = zSessionSummary.strict().parse(root.mapSession)
    const reviewList = zReviewListPayload.strict().parse(root.reviewList)

    const mapCommit = record(root.mapCommit, 'Strike smoke fixture.mapCommit')
    exactFields(mapCommit, ['hash', 'subject', 'timeMs'], 'Strike smoke fixture.mapCommit')
    const expected = record(root.expected, 'Strike smoke fixture.expected')
    exactFields(expected, ['assistantContent', 'mapConversationTitle', 'reviewSessionTitle'], 'Strike smoke fixture.expected')
    const reviewSession = reviewList.sessions.find((session) => session.sessionId === sessionDetail.id)
    if (sessionDetail.harness !== Harness.Strike || mapSession.harness !== Harness.Strike || reviewSession?.harness !== Harness.Strike) {
      throw new Error('transcript, Map, and Review payloads must all carry the canonical Strike harness')
    }
    if (sessionDetail.project !== projectName || mapSession.project !== projectName || mapSession.projectHash !== projectHash || reviewList.projectHash !== projectHash || mapSession.id !== sessionDetail.id) {
      throw new Error('project and session identities must agree across every Strike smoke surface')
    }

    return Object.freeze({
      projectHash,
      projectName,
      sessionId: sessionDetail.id,
      assistantContent: requiredString(expected, 'assistantContent', 'Strike smoke fixture.expected'),
      mapConversationTitle: requiredString(expected, 'mapConversationTitle', 'Strike smoke fixture.expected'),
      reviewSessionTitle: requiredString(expected, 'reviewSessionTitle', 'Strike smoke fixture.expected'),
      mapCommitHash: requiredString(mapCommit, 'hash', 'Strike smoke fixture.mapCommit'),
      mapCommitSubject: requiredString(mapCommit, 'subject', 'Strike smoke fixture.mapCommit'),
      mapCommitTimeMs: requiredInteger(mapCommit, 'timeMs', 'Strike smoke fixture.mapCommit'),
      epochMs: Date.parse(sessionDetail.endTime),
    })
  } catch (error) {
    throw new Error(
      `Strike real-binary fixture could not be loaded because ${error.message} at ${FIXTURE_PATH} before the smoke server started; transcript, Map, and Changes evidence would not share one trustworthy payload; fix the fixture structure or schema values, then rerun full-app-smoke.mjs.`,
      { cause: error },
    )
  }
}
