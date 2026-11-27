import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import {
  createBrowserPickerAdapter,
  describePickedElement,
  feedbackDisabled,
  feedbackLabelFor,
  feedbackSectionOf,
  feedbackSelectorFor,
  isIgnoredFeedbackElement,
  isPageRootElement,
  isTooLargeRect,
  safeTextSnippetFor,
  stepDomTarget,
} from './domTarget';
import { ALL_PICKER_DIRECTIONS, PickerDirection } from './types';

const FIXTURE_PATH = resolve(process.cwd(), 'src/lib/feedback/testdata/dom_target_cases.yaml');

const DIRECTION_BY_WIRE: Readonly<Record<string, PickerDirection>> = Object.fromEntries(
  ALL_PICKER_DIRECTIONS.map((member) => [member.wire, member]),
);

function loadInventory(root: Record<string, unknown>, key: string): Record<string, unknown>[] {
  const inventory = requireRecord(root[key], `dom_target_cases.${key}`);
  requireExactRequiredFields(inventory, ['requiredNames', 'cases'], `dom_target_cases.${key}`);
  const requiredNames = inventory.requiredNames;
  if (
    !Array.isArray(requiredNames) ||
    requiredNames.length === 0 ||
    requiredNames.some((name) => typeof name !== 'string' || name.length === 0) ||
    new Set(requiredNames).size !== requiredNames.length
  ) {
    throw new Error(`dom_target_cases.${key}.requiredNames must be unique non-empty strings`);
  }
  if (!Array.isArray(inventory.cases) || inventory.cases.length === 0) {
    throw new Error(`dom_target_cases.${key}.cases must be a non-empty array`);
  }
  const cases = inventory.cases.map((row, index) => requireRecord(row, `dom_target_cases.${key}.cases[${index}]`));
  requireUniqueNames(cases, `dom_target_cases.${key}.cases`);
  const names = cases.map((row) => row.name as string);
  const missing = (requiredNames as string[]).filter((name) => !names.includes(name));
  const undeclared = names.filter((name) => !(requiredNames as string[]).includes(name));
  if (missing.length > 0 || undeclared.length > 0) {
    throw new Error(
      `dom_target_cases.${key} cases must match requiredNames (missing: ${missing.join(', ') || 'none'}; undeclared: ${undeclared.join(', ') || 'none'})`,
    );
  }
  return cases;
}

interface SelectorCase {
  name: string;
  html: string;
  pick: string;
  selector: string;
  label: string;
  section: string;
}

interface TooLargeCase {
  name: string;
  rect: { width: number; height: number };
  viewport: { width: number; height: number };
  expected: boolean;
}

interface SnippetCase {
  name: string;
  html: string;
  repeatToken?: string;
  repeatUnit?: string;
  repeatCount?: number;
  pick: string;
  masked: boolean;
  expected?: string;
  expectedPrefix?: string;
  maxLength: number;
}

interface SwitchCase {
  name: string;
  search: string;
  disabled: boolean;
}

interface IgnoredCase {
  name: string;
  html: string;
  pick: string;
  ignored: boolean;
}

interface SteppingMove {
  start: string | null;
  direction: string;
  expected: string | null;
}

interface SteppingCase {
  name: string;
  html: string;
  moves: SteppingMove[];
}

interface DescribeCase {
  name: string;
  html: string;
  pick: string;
  masked: boolean;
  selector: string;
  label: string;
  section: string;
  textSnippet: string;
  box: { top: number; left: number; width: number; height: number };
}

function loadFixture() {
  const root = requireRecord(parseStrictYAML(readFileSync(FIXTURE_PATH, 'utf8'), 'dom_target_cases'), 'dom_target_cases');
  requireExactRequiredFields(
    root,
    ['selectors', 'tooLarge', 'snippets', 'switches', 'ignored', 'stepping', 'describe'],
    'dom_target_cases',
  );

  const selectors = loadInventory(root, 'selectors').map((row, index) => {
    requireExactRequiredFields(row, ['name', 'html', 'pick', 'selector', 'label', 'section'], `selectors[${index}]`);
    return row as unknown as SelectorCase;
  });

  const tooLarge = loadInventory(root, 'tooLarge').map((row, index) => {
    requireExactRequiredFields(row, ['name', 'rect', 'viewport', 'expected'], `tooLarge[${index}]`);
    const rect = requireRecord(row.rect, `tooLarge[${index}].rect`);
    const viewport = requireRecord(row.viewport, `tooLarge[${index}].viewport`);
    requireExactRequiredFields(rect, ['width', 'height'], `tooLarge[${index}].rect`);
    requireExactRequiredFields(viewport, ['width', 'height'], `tooLarge[${index}].viewport`);
    return row as unknown as TooLargeCase;
  });

  const snippets = loadInventory(root, 'snippets').map((row, index) => {
    requireExactFields(
      row,
      ['name', 'html', 'repeatToken', 'repeatUnit', 'repeatCount', 'pick', 'masked', 'expected', 'expectedPrefix', 'maxLength'],
      `snippets[${index}]`,
    );
    return row as unknown as SnippetCase;
  });

  const switches = loadInventory(root, 'switches').map((row, index) => {
    requireExactRequiredFields(row, ['name', 'search', 'disabled'], `switches[${index}]`);
    return row as unknown as SwitchCase;
  });

  const ignored = loadInventory(root, 'ignored').map((row, index) => {
    requireExactRequiredFields(row, ['name', 'html', 'pick', 'ignored'], `ignored[${index}]`);
    return row as unknown as IgnoredCase;
  });

  const stepping = loadInventory(root, 'stepping').map((row, index) => {
    requireExactRequiredFields(row, ['name', 'html', 'moves'], `stepping[${index}]`);
    if (!Array.isArray(row.moves) || row.moves.length === 0) {
      throw new Error(`stepping[${index}].moves must be a non-empty array`);
    }
    row.moves.forEach((move, moveIndex) => {
      const m = requireRecord(move, `stepping[${index}].moves[${moveIndex}]`);
      requireExactRequiredFields(m, ['start', 'direction', 'expected'], `stepping[${index}].moves[${moveIndex}]`);
      if (typeof m.direction !== 'string' || !(m.direction in DIRECTION_BY_WIRE)) {
        throw new Error(`stepping[${index}].moves[${moveIndex}].direction is invalid`);
      }
    });
    return row as unknown as SteppingCase;
  });

  const describeCases = loadInventory(root, 'describe').map((row, index) => {
    requireExactRequiredFields(
      row,
      ['name', 'html', 'pick', 'masked', 'selector', 'label', 'section', 'textSnippet', 'box'],
      `describe[${index}]`,
    );
    const box = requireRecord(row.box, `describe[${index}].box`);
    requireExactRequiredFields(box, ['top', 'left', 'width', 'height'], `describe[${index}].box`);
    return row as unknown as DescribeCase;
  });

  return { selectors, tooLarge, snippets, switches, ignored, stepping, describeCases };
}

const fixture = loadFixture();

describe('domTarget', () => {
  afterEach(() => {
    document.head.innerHTML = '';
    document.body.innerHTML = '';
  });

  it.each(fixture.selectors)('selector: $name', (c) => {
    document.body.innerHTML = c.html;
    const el = document.body.querySelector(c.pick);
    expect(el).not.toBeNull();
    expect(feedbackSelectorFor(el!)).toBe(c.selector);
    expect(feedbackLabelFor(el!)).toBe(c.label);
    expect(feedbackSectionOf(el!)).toBe(c.section);
  });

  it.each(fixture.tooLarge)('tooLarge: $name', (c) => {
    expect(isTooLargeRect(c.rect, c.viewport)).toBe(c.expected);
  });

  it.each(fixture.snippets)('snippet: $name', (c) => {
    const html =
      c.repeatToken && c.repeatUnit && c.repeatCount
        ? c.html.replace(c.repeatToken, c.repeatUnit.repeat(c.repeatCount))
        : c.html;
    document.body.innerHTML = html;
    const el = document.body.querySelector(c.pick);
    expect(el).not.toBeNull();
    const snippet = safeTextSnippetFor(el!, c.masked, c.maxLength);
    expect(snippet.length).toBeLessThanOrEqual(c.maxLength);
    if (c.expected !== undefined) {
      expect(snippet).toBe(c.expected);
    }
    if (c.expectedPrefix !== undefined) {
      expect(snippet.length).toBe(c.maxLength);
      expect(snippet.startsWith(c.expectedPrefix)).toBe(true);
    }
  });

  it.each(fixture.switches)('switch: $name', (c) => {
    expect(feedbackDisabled(c.search)).toBe(c.disabled);
  });

  it.each(fixture.ignored)('ignored: $name', (c) => {
    document.body.innerHTML = c.html;
    const el = document.body.querySelector(c.pick);
    expect(el).not.toBeNull();
    expect(isIgnoredFeedbackElement(el!)).toBe(c.ignored);
  });

  it.each(fixture.stepping)('stepping: $name', (c) => {
    document.head.innerHTML = '<meta charset="utf-8"><title>peasant</title>';
    document.body.innerHTML = c.html;
    for (const move of c.moves) {
      const startEl = move.start ? document.body.querySelector(move.start) : null;
      if (move.start) expect(startEl).not.toBeNull();
      const direction = DIRECTION_BY_WIRE[move.direction];
      const next = stepDomTarget(startEl, direction, document);
      if (move.expected === null) {
        expect(next).toBeNull();
      } else {
        const expectedEl = document.body.querySelector(move.expected);
        expect(expectedEl).not.toBeNull();
        expect(next).toBe(expectedEl);
        expect(isPageRootElement(next!)).toBe(false);
        expect(isIgnoredFeedbackElement(next!)).toBe(false);
      }
    }
  });

  it.each(fixture.describeCases)('describe: $name', (c) => {
    document.body.innerHTML = c.html;
    const el = document.body.querySelector(c.pick);
    expect(el).not.toBeNull();
    el!.getBoundingClientRect = () =>
      ({
        ...c.box,
        right: c.box.left + c.box.width,
        bottom: c.box.top + c.box.height,
        x: c.box.left,
        y: c.box.top,
        toJSON: () => c.box,
      }) as DOMRect;

    const info = describePickedElement(el!, c.masked);
    expect(info).toEqual({
      selector: c.selector,
      label: c.label,
      section: c.section,
      textSnippet: c.textSnippet,
      box: c.box,
    });

    const adapter = createBrowserPickerAdapter(() => c.masked);
    expect(adapter.describe(el!)).toEqual(info);
    expect(adapter.labelFor(el!)).toBe(c.label);
    expect(adapter.sectionOf(el!)).toBe(c.section);
    expect(adapter.isIgnoredElement(el!)).toBe(false);
  });

  it('treats html, body, head, #root, and #__next as page roots', () => {
    expect(isPageRootElement(document.documentElement)).toBe(true);
    expect(isPageRootElement(document.head)).toBe(true);
    expect(isPageRootElement(document.body)).toBe(true);
    const root = document.createElement('div');
    root.id = 'root';
    const next = document.createElement('div');
    next.id = '__next';
    expect(isPageRootElement(root)).toBe(true);
    expect(isPageRootElement(next)).toBe(true);
  });
});
