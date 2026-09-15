import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { newProjectHash } from '@peasant-labs/schema'
import { parseDocument } from 'yaml'

const HERE = dirname(fileURLToPath(import.meta.url))
const FIXTURE_PATH = resolve(HERE, '../../../internal/mock/testdata/context_navigation.yaml')
const SESSION_ID = /^sess?_[a-zA-Z0-9]+$/

function record(value, path) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${path} must be a mapping`)
  return value
}

function exactFields(value, fields, path) {
  const keys = Object.keys(value).sort()
  const expected = [...fields].sort()
  if (keys.length !== expected.length || keys.some((key, index) => key !== expected[index])) {
    throw new Error(`${path} fields ${JSON.stringify(keys)} do not exactly match ${JSON.stringify(expected)}`)
  }
}

function requiredSessionId(value, field, path) {
  if (typeof value[field] !== 'string' || !SESSION_ID.test(value[field])) throw new Error(`${path}.${field} must be an owner-local session id`)
  return value[field]
}

function requiredString(value, field, path) {
  if (typeof value[field] !== 'string' || value[field].length === 0) throw new Error(`${path}.${field} must be a non-empty string`)
  return value[field]
}

/**
 * Loads the ONE mounted current-parent navigation fixture shared with the Go
 * mock provider, so the capture and the server can never disagree about the
 * child route, its exact context/source and parent targets, or the labels the
 * real viewer renders.
 */
export function loadContextNavigationFixture() {
  try {
    const document = parseDocument(readFileSync(FIXTURE_PATH, 'utf8'), { prettyErrors: true, strict: true, uniqueKeys: true })
    if (document.errors.length > 0) throw document.errors[0]
    if (document.contents === null) throw new Error('fixture document is empty')
    const root = record(document.toJS(), 'context navigation fixture')
    exactFields(root, ['project', 'child', 'source', 'parent', 'expected'], 'context navigation fixture')

    const project = record(root.project, 'context navigation fixture.project')
    exactFields(project, ['hash', 'name'], 'context navigation fixture.project')
    const projectHash = newProjectHash(requiredString(project, 'hash', 'context navigation fixture.project'))
    const projectName = requiredString(project, 'name', 'context navigation fixture.project')

    const child = record(root.child, 'context navigation fixture.child')
    const source = record(root.source, 'context navigation fixture.source')
    const parent = record(root.parent, 'context navigation fixture.parent')
    const childSessionId = requiredSessionId(child, 'id', 'context navigation fixture.child')
    const sourceId = requiredSessionId(source, 'id', 'context navigation fixture.source')
    const parentId = requiredSessionId(parent, 'id', 'context navigation fixture.parent')
    const relationships = child.relationships
    if (!Array.isArray(relationships) || relationships.length !== 2) throw new Error('context navigation fixture.child.relationships must name the context_from source and the started_by parent')
    for (const relationship of relationships) {
      exactFields(record(relationship, 'context navigation fixture.child.relationships[]'), ['kind', 'targetState', 'targetLocalId', 'evidence'], 'context navigation fixture.child.relationships[]')
    }
    const context = relationships.find((relationship) => relationship.kind === 'context_from')
    const starter = relationships.find((relationship) => relationship.kind === 'started_by')
    if (context?.targetLocalId !== sourceId || starter?.targetLocalId !== parentId) throw new Error('context navigation fixture relationships must target the declared source and parent sessions')
    if (context?.targetState !== 'target_known' || starter?.targetState !== 'target_known' || context?.evidence !== 'native_typed' || starter?.evidence !== 'native_typed') throw new Error('context navigation fixture relationships must be known native-typed targets')

    const expected = record(root.expected, 'context navigation fixture.expected')
    exactFields(expected, ['contextLabel', 'starterLabel', 'starterLinkAction', 'earlierSummary'], 'context navigation fixture.expected')

    return Object.freeze({
      projectHash,
      projectName,
      childSessionId,
      sourceId,
      parentId,
      contextLabel: requiredString(expected, 'contextLabel', 'context navigation fixture.expected'),
      starterLabel: requiredString(expected, 'starterLabel', 'context navigation fixture.expected'),
      starterLinkAction: requiredString(expected, 'starterLinkAction', 'context navigation fixture.expected'),
      earlierSummary: requiredString(expected, 'earlierSummary', 'context navigation fixture.expected'),
    })
  } catch (error) {
    throw new Error(
      `Mounted current-parent navigation fixture could not be loaded because ${error.message} at ${FIXTURE_PATH} before the capture server started; the mounted link/Back evidence would not match the served data; fix the fixture structure or schema values, then rerun current-parent-navigation.mjs.`,
      { cause: error },
    )
  }
}
