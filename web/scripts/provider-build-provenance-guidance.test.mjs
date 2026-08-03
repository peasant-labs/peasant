import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import YAML from 'yaml'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(resolve(here, 'provider-build-provenance.mjs'), 'utf8')
const document = YAML.parseDocument(readFileSync(resolve(here, 'testdata/provider-build-provenance-guidance.yaml'), 'utf8'), { strict: true, uniqueKeys: true })
if (document.errors.length > 0) {
  throw new Error(`provider provenance guidance fixture is invalid: ${document.errors.map((error) => error.message).join('; ')}`)
}
const fixture = document.toJS()

if (
  !fixture ||
  Object.keys(fixture).sort().join(',') !== 'expected_guidance,forbidden_fragments' ||
  typeof fixture.expected_guidance !== 'string' ||
  !Array.isArray(fixture.forbidden_fragments) ||
  fixture.forbidden_fragments.length !== 2
) {
  throw new Error('provider provenance guidance fixture must define one expected guidance string and exactly two forbidden fragments')
}

const occurrences = source.split(fixture.expected_guidance).length - 1
if (occurrences !== 1) {
  throw new Error(`provider provenance must contain the final Schema release guidance exactly once; found ${occurrences}`)
}
for (const fragment of fixture.forbidden_fragments) {
  if (typeof fragment !== 'string' || fragment.length === 0 || source.includes(fragment)) {
    throw new Error(`provider provenance guidance must not reference Schema prerelease fragment ${JSON.stringify(fragment)}`)
  }
}

console.log('provider build provenance guidance: exact final Schema release guidance is protected')
