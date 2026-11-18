import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import YAML from 'yaml'
import { checkSource } from './assert-directive-prologue.mjs'

const fixture = YAML.parse(await readFile(new URL('./testdata/directive-prologue.yaml', import.meta.url), 'utf8'))
const manifest = YAML.parse(await readFile(new URL('./testdata/directive-prologue.manifest.yaml', import.meta.url), 'utf8'))
const names = fixture.cases.map((entry) => entry.name)
assert.equal(new Set(names).size, names.length, 'duplicate fixture case name')
assert.deepEqual([...names].sort(), [...manifest.requiredCaseNames].sort(), 'directive guard case names')
for (const entry of fixture.cases) {
  if (entry.rejected) assert.throws(() => checkSource(entry.source, 'fixture.tsx'), /must be the first statement/, entry.name)
  else assert.doesNotThrow(() => checkSource(entry.source, 'fixture.tsx'), entry.name)
}
console.log('directive prologue fixture cases passed')
