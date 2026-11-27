import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import {
  buildMaskStyleCss,
  FEEDBACK_EXCLUDE_SELECTORS,
  FEEDBACK_MASK_TARGETS,
  isInsideMaskedElement,
  selectorsForMaskMode,
} from './masking';
import { ALL_MASK_CATEGORIES, ALL_MASK_MODES, MaskCategory, MaskMode } from './types';

const FIXTURE_PATH = resolve(process.cwd(), 'src/lib/feedback/testdata/masking_cases.yaml');

const MODE_BY_WIRE: Readonly<Record<string, MaskMode>> = Object.fromEntries(
  ALL_MASK_MODES.map((member) => [member.wire, member]),
);

function loadInventory(root: Record<string, unknown>, key: string): Record<string, unknown>[] {
  const inventory = requireRecord(root[key], `masking_cases.${key}`);
  requireExactRequiredFields(inventory, ['requiredNames', 'cases'], `masking_cases.${key}`);
  const requiredNames = inventory.requiredNames;
  if (
    !Array.isArray(requiredNames) ||
    requiredNames.length === 0 ||
    requiredNames.some((name) => typeof name !== 'string' || name.length === 0) ||
    new Set(requiredNames).size !== requiredNames.length
  ) {
    throw new Error(`masking_cases.${key}.requiredNames must be unique non-empty strings`);
  }
  if (!Array.isArray(inventory.cases) || inventory.cases.length === 0) {
    throw new Error(`masking_cases.${key}.cases must be a non-empty array`);
  }
  const cases = inventory.cases.map((row, index) => requireRecord(row, `masking_cases.${key}.cases[${index}]`));
  requireUniqueNames(cases, `masking_cases.${key}.cases`);
  const names = cases.map((row) => row.name as string);
  const missing = (requiredNames as string[]).filter((name) => !names.includes(name));
  const undeclared = names.filter((name) => !(requiredNames as string[]).includes(name));
  if (missing.length > 0 || undeclared.length > 0) {
    throw new Error(
      `masking_cases.${key} cases must match requiredNames (missing: ${missing.join(', ') || 'none'}; undeclared: ${undeclared.join(', ') || 'none'})`,
    );
  }
  return cases;
}

interface MatchCase {
  name: string;
  html: string;
  target: string;
  mode: string;
  expectedMasked: boolean;
}

interface CssCase {
  name: string;
  mode: string;
  mustContain: string[];
  mustNotContain: string[];
}

function loadFixture() {
  const root = requireRecord(parseStrictYAML(readFileSync(FIXTURE_PATH, 'utf8'), 'masking_cases'), 'masking_cases');
  requireExactRequiredFields(root, ['matchCases', 'cssCases'], 'masking_cases');

  const matchCases = loadInventory(root, 'matchCases').map((row, index) => {
    requireExactRequiredFields(row, ['name', 'html', 'target', 'mode', 'expectedMasked'], `matchCases[${index}]`);
    if (typeof row.mode !== 'string' || !(row.mode in MODE_BY_WIRE)) {
      throw new Error(`matchCases[${index}].mode is invalid`);
    }
    return row as unknown as MatchCase;
  });

  const cssCases = loadInventory(root, 'cssCases').map((row, index) => {
    requireExactRequiredFields(row, ['name', 'mode', 'mustContain', 'mustNotContain'], `cssCases[${index}]`);
    if (typeof row.mode !== 'string' || !(row.mode in MODE_BY_WIRE)) {
      throw new Error(`cssCases[${index}].mode is invalid`);
    }
    return row as unknown as CssCase;
  });

  return { matchCases, cssCases };
}

const fixture = loadFixture();

describe('masking', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('exports exclude selectors and targets for every MaskCategory', () => {
    expect(FEEDBACK_EXCLUDE_SELECTORS).toEqual(['[data-feedback-widget]', '[data-fb]']);
    for (const category of ALL_MASK_CATEGORIES) {
      expect(FEEDBACK_MASK_TARGETS[category.wire].length).toBeGreaterThan(0);
    }
    expect(selectorsForMaskMode(MaskMode.Masked)).toEqual([
      ...FEEDBACK_MASK_TARGETS[MaskCategory.TranscriptContent.wire],
      ...FEEDBACK_MASK_TARGETS[MaskCategory.SensitiveInputs.wire],
    ]);
    expect(selectorsForMaskMode(MaskMode.Unmasked)).toEqual([
      ...FEEDBACK_MASK_TARGETS[MaskCategory.SensitiveInputs.wire],
    ]);
  });

  it.each(fixture.matchCases)('match: $name', (c) => {
    document.body.innerHTML = c.html;
    const el = document.body.querySelector(c.target);
    expect(el).not.toBeNull();
    expect(isInsideMaskedElement(el!, MODE_BY_WIRE[c.mode])).toBe(c.expectedMasked);
  });

  it.each(fixture.cssCases)('css: $name', (c) => {
    const css = buildMaskStyleCss(MODE_BY_WIRE[c.mode]);
    for (const snippet of c.mustContain) {
      expect(css).toContain(snippet);
    }
    for (const snippet of c.mustNotContain) {
      expect(css).not.toContain(snippet);
    }
  });
});
