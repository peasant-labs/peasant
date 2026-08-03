#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import YAML from 'yaml'
import puppeteer from 'puppeteer-core'
import { assertKnownProjects } from './validate-mock-coordinates.mjs'

const fixtureUrl = new URL('./testdata/transcript-input.yaml', import.meta.url)
const fixtureSource = fs.readFileSync(fixtureUrl, 'utf8')
const origin = process.env.PEASANT_ORIGIN || 'http://localhost:8699'
const chrome = process.env.CHROME_PATH
const captureDir = process.env.TRANSCRIPT_INPUT_OUTPUT
const fixtureFields = new Set(['expectedCaseCount', 'cases'])
const baseCaseFields = ['name', 'width', 'height', 'theme', 'project', 'session', 'action', 'expectScrollable']
const actions = new Set(['wheel', 'trackpad', 'pageDown', 'end', 'home', 'focusedControlWheel', 'deepLink'])

function fail(reason, location, fix) {
  console.error([
    'transcript input gate failed.',
    `What went wrong: ${reason}.`,
    'Why it happened: the mounted production transcript did not satisfy its fixture-defined input and viewport contract.',
    `Where: ${location}.`,
    'When: real-browser transcript input matrix.',
    'What it means: users may be unable to move transcript context out of the way or land on a copied turn link.',
    `How to fix: ${fix}.`,
  ].join('\n'))
  process.exit(1)
}

function requireRecord(value, location) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${location} must be a mapping`)
  return value
}

function requireExactFields(value, expected, location) {
  const actual = Object.keys(value)
  const missing = expected.filter((key) => !actual.includes(key))
  const unknown = actual.filter((key) => !expected.includes(key))
  if (missing.length || unknown.length) {
    throw new Error(`${location} has missing fields [${missing.join(', ')}] and unknown fields [${unknown.join(', ')}]`)
  }
}

function loadFixture(source) {
  const documents = YAML.parseAllDocuments(source, { uniqueKeys: true })
  if (documents.length !== 1) throw new Error(`fixture must contain exactly one YAML document, found ${documents.length}`)
  if (documents[0].errors.length) throw documents[0].errors[0]
  const fixture = requireRecord(documents[0].toJS(), 'fixture')
  requireExactFields(fixture, [...fixtureFields], 'fixture')
  if (!Number.isInteger(fixture.expectedCaseCount) || fixture.expectedCaseCount < actions.size) {
    throw new Error(`fixture expectedCaseCount must be an integer of at least ${actions.size}`)
  }
  if (!Array.isArray(fixture.cases) || fixture.cases.length !== fixture.expectedCaseCount) {
    throw new Error(`fixture has ${fixture.cases?.length ?? 0} cases, want ${fixture.expectedCaseCount}`)
  }
  const names = new Set()
  const foundActions = new Set()
  for (const [index, value] of fixture.cases.entries()) {
    const testCase = requireRecord(value, `fixture.cases[${index}]`)
    const expectedFields = testCase.action === 'deepLink' ? [...baseCaseFields, 'turn'] : baseCaseFields
    requireExactFields(testCase, expectedFields, `fixture.cases[${index}]`)
    if (typeof testCase.name !== 'string' || testCase.name.length === 0 || names.has(testCase.name)) {
      throw new Error(`fixture.cases[${index}].name must be a non-empty unique string`)
    }
    names.add(testCase.name)
    foundActions.add(testCase.action)
    if (!actions.has(testCase.action)
      || !['dark', 'light'].includes(testCase.theme)
      || !Number.isInteger(testCase.width) || testCase.width <= 0
      || !Number.isInteger(testCase.height) || testCase.height <= 0
      || typeof testCase.project !== 'string' || testCase.project.length === 0
      || typeof testCase.session !== 'string' || testCase.session.length === 0
      || typeof testCase.expectScrollable !== 'boolean'
      || (testCase.action === 'deepLink' && (!Number.isInteger(testCase.turn) || testCase.turn < 0))) {
      throw new Error(`fixture.cases[${index}] has an invalid action, viewport, theme, project, session, turn, or scroll expectation`)
    }
  }
  for (const action of actions) {
    if (!foundActions.has(action)) throw new Error(`fixture is missing the required ${action} action family`)
  }
  if (!fixture.cases.some((testCase) => testCase.expectScrollable === true)
    || !fixture.cases.some((testCase) => testCase.expectScrollable === false)) {
    throw new Error('fixture must exercise both scrollable and non-scrollable transcripts')
  }
  return fixture
}

function requireMutationRejection(name, source) {
  try {
    loadFixture(source)
  } catch {
    return
  }
  throw new Error(`fixture mutation ${name} was accepted`)
}

let fixture
try {
  fixture = loadFixture(fixtureSource)
  requireMutationRejection('duplicate mapping key', fixtureSource.replace('width: 1460', 'width: 1460, width: 1460'))
  requireMutationRejection('unknown row field', fixtureSource.replace('action: wheel', 'action: wheel, mystery: true'))
  requireMutationRejection('duplicate case name', fixtureSource.replace('long desktop light trackpad equivalent', 'long desktop dark wheel'))
  requireMutationRejection('missing expectScrollable', fixtureSource.replace(', expectScrollable: true', ''))
  requireMutationRejection('missing deep-link turn', fixtureSource.replace(', turn: 40', ''))
  requireMutationRejection('trailing YAML document', `${fixtureSource}\n---\nextra: true\n`)
} catch (error) {
  fail(error instanceof Error ? error.message : String(error), 'web/scripts/visual/testdata/transcript-input.yaml validation', 'restore the strict one-document fixture shape, unique names, required action families, and per-action fields')
}

if (process.env.TRANSCRIPT_INPUT_FIXTURE_ONLY === '1') {
  console.log(`transcript input fixture: ${fixture.cases.length} strict cases and mutation teeth passed`)
  process.exit(0)
}

if (!chrome) fail('CHROME_PATH is unset', 'web/scripts/visual/transcript-input-gate.mjs startup', 'set CHROME_PATH to the Chromium executable and rerun')
if (captureDir) fs.mkdirSync(captureDir, { recursive: true })

// Fail-fast coordinate check BEFORE Puppeteer boots — validate every DISTINCT project referenced
// across the fixture matrix ONCE, up front (see validate-mock-coordinates.mjs for why: this is the
// systemic guard against the recurring stale-mock-project-default class of bug — this exact fixture
// was the second live instance of it, fixed in 88216f34).
try {
  await assertKnownProjects(origin, fixture.cases.map((c) => c.project), { where: 'transcript-input-gate.mjs' })
} catch (e) {
  console.error(e.message)
  process.exit(2)
}

const browser = await puppeteer.launch({ executablePath: chrome, headless: 'new' })
try {
  for (const testCase of fixture.cases) {
    const page = await browser.newPage()
    try {
      await page.setViewport({ width: testCase.width, height: testCase.height, deviceScaleFactor: 1 })
      await page.evaluateOnNewDocument((theme) => localStorage.setItem('peasant-theme', theme), testCase.theme)
      const query = testCase.turn == null ? '' : `?turn=${testCase.turn}`
      const url = `${origin}/projects/${encodeURIComponent(testCase.project)}/${testCase.session}/${query}`
      const response = await page.goto(url, { waitUntil: 'domcontentloaded' })
      if (!response || response.status() !== 200) fail(`${testCase.name} returned HTTP ${response?.status() ?? 0}`, url, 'start the exact built Peasant binary with the web mock provider')
      await page.waitForSelector('.txn-stream', { timeout: 15000 })
      await page.waitForSelector('.txn-stream-prelude', { timeout: 15000 })
      const initial = await page.$eval('.txn-stream', (element) => ({
        top: element.scrollTop,
        scrollHeight: element.scrollHeight,
        clientHeight: element.clientHeight,
      }))
      if (testCase.expectScrollable && initial.scrollHeight <= initial.clientHeight) {
        fail(`${testCase.name} did not overflow (${initial.scrollHeight} <= ${initial.clientHeight})`, url, 'use the long fixture session and restore the transcript stream height contract')
      }
      if (!testCase.expectScrollable && initial.scrollHeight > initial.clientHeight) {
        fail(`${testCase.name} unexpectedly overflowed (${initial.scrollHeight} > ${initial.clientHeight})`, url, 'keep the short transcript within the mounted mobile viewport or explicitly reclassify the fixture after manual review')
      }

      if (testCase.action === 'wheel' || testCase.action === 'trackpad') {
        await page.hover('.txn-stream')
        const steps = testCase.action === 'trackpad' ? 6 : 1
        for (let i = 0; i < steps; i += 1) await page.mouse.wheel({ deltaX: testCase.action === 'trackpad' ? 3 : 0, deltaY: testCase.action === 'trackpad' ? 55 : 420 })
      } else if (testCase.action === 'focusedControlWheel') {
        const controlSelector = '.txn-stream button'
        await page.waitForSelector(controlSelector, { timeout: 15000 })
        await page.click(controlSelector)
        await page.hover('.txn-stream')
        await page.mouse.wheel({ deltaY: 420 })
      } else if (testCase.action === 'home') {
        await page.$eval('.txn-stream', (element) => { element.scrollTop = element.scrollHeight })
        await page.focus('.txn-stream')
        await page.keyboard.press('Home')
      } else if (testCase.action === 'pageDown' || testCase.action === 'end') {
        await page.focus('.txn-stream')
        await page.keyboard.press(testCase.action === 'pageDown' ? 'PageDown' : 'End')
      }
      await new Promise((resolve) => setTimeout(resolve, 350))

      const final = await page.$eval('.txn-stream', (element) => ({ top: element.scrollTop, max: element.scrollHeight - element.clientHeight }))
      if (testCase.expectScrollable && testCase.action !== 'deepLink') {
        const moved = testCase.action === 'home' ? final.top < initial.scrollHeight - initial.clientHeight : final.top > initial.top
        if (!moved) fail(`${testCase.name} did not move the transcript stream (before ${initial.top}, after ${final.top})`, url, 'restore native wheel and keyboard scrolling on .txn-stream without trapping focused controls')
        const preludeVisible = await page.$eval('.txn-stream-prelude', (element) => {
          const stream = element.closest('.txn-stream').getBoundingClientRect()
          return element.getBoundingClientRect().bottom > stream.top
        })
        if (testCase.action !== 'home' && preludeVisible) fail(`${testCase.name} left the transcript prelude fixed in view`, url, 'keep the host controls inside the real transcript scroller so they move away with the turns')
      }
      if (testCase.action === 'deepLink') {
        const positioned = await page.$eval(`#turn-${testCase.turn}`, (element) => {
          const stream = element.closest('.txn-stream').getBoundingClientRect()
          const turn = element.getBoundingClientRect()
          return turn.top >= stream.top - 16 && turn.top < stream.bottom
        })
        if (!positioned || final.top <= 0) fail(`${testCase.name} did not position turn ${testCase.turn}`, url, 'honor initialTurnIndex by scrolling the production TranscriptViewer after turn refs mount')
      }
      const theme = await page.$eval('html', (element) => element.getAttribute('data-theme'))
      if (theme !== testCase.theme) fail(`${testCase.name} rendered theme ${theme}, want ${testCase.theme}`, url, 'restore theme propagation on the mounted production route')
      const computed = await page.$eval('.txn-stream', (element) => {
        const prelude = element.querySelector('.txn-stream-prelude')
        const streamStyle = getComputedStyle(element)
        const preludeStyle = prelude ? getComputedStyle(prelude) : null
        return {
          overflowY: streamStyle.overflowY,
          streamPosition: streamStyle.position,
          preludePosition: preludeStyle?.position ?? 'missing',
        }
      })
      if (!['auto', 'scroll'].includes(computed.overflowY)) fail(`${testCase.name} computed overflow-y=${computed.overflowY}`, url, 'restore the real .txn-stream overflow container')
      if (['fixed', 'sticky'].includes(computed.preludePosition)) fail(`${testCase.name} computed prelude position=${computed.preludePosition}`, url, 'keep the transcript prelude in normal flow inside .txn-stream')
      if (captureDir) {
        const filename = `${String(fixture.cases.indexOf(testCase) + 1).padStart(2, '0')}-${testCase.name.replace(/[^a-z0-9]+/gi, '-').replace(/^-|-$/g, '').toLowerCase()}.png`
        await page.screenshot({ path: path.join(captureDir, filename), fullPage: false })
      }
      console.log(`COMPUTED ${testCase.name}: overflow-y=${computed.overflowY} stream-position=${computed.streamPosition} prelude-position=${computed.preludePosition}`)
      console.log(`PASS ${testCase.name}`)
    } finally {
      await page.close()
    }
  }
} finally {
  await browser.close()
}

console.log(`transcript input gate: all ${fixture.cases.length} fixture cases passed`)
