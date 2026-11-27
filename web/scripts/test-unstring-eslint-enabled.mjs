import assert from 'node:assert/strict'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import YAML from 'yaml'
import { checkDirectory } from './assert-unstring-eslint-enabled.mjs'

const FIXTURE = new URL('./testdata/unstring-eslint-guard.yaml', import.meta.url)
const MANIFEST = new URL('./testdata/unstring-eslint-guard.manifest.yaml', import.meta.url)
const fixture = YAML.parse(await readFile(FIXTURE, 'utf8'))
const manifest = YAML.parse(await readFile(MANIFEST, 'utf8'))
const names = fixture.cases.map((entry) => entry.name)
assert.equal(new Set(names).size, names.length, 'duplicate fixture case name')
assert.deepEqual([...names].sort(), [...manifest.requiredCaseNames].sort(), 'ESLint guard case names')

const directory = await mkdtemp(join(tmpdir(), 'unstring-eslint-guard-'))
try {
  for (const entry of fixture.cases) {
    await writeFile(join(directory, 'source.jsx'), entry.source)
    if (entry.rejected) {
      await assert.rejects(checkDirectory(directory), /unstring\/closed-set-literal/, entry.name)
    } else {
      await assert.doesNotReject(checkDirectory(directory), entry.name)
    }
  }
} finally {
  await rm(directory, { recursive: true, force: true })
}
console.log('ESLint guard fixture cases passed')
