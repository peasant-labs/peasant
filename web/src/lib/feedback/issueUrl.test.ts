import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  ALL_FEEDBACK_KINDS,
  ALL_ISSUE_FORM_FIELD_IDS,
  IssueFormFieldId,
  type FeedbackContextInfo,
  type FeedbackKind,
  type IssueDraftInput,
  type PickedElementInfo,
} from '@/lib/feedback/types';
import {
  buildGitHubIssueUrl,
  DEFAULT_REPO_ISSUES_URL,
  formatElementBlock,
  formatEnvironmentBlock,
  MAX_ISSUE_URL_LENGTH,
  templateForKind,
} from '@/lib/feedback/issueUrl';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

const REQUIRED_CASES = [
  'problem-selects-problem-template',
  'feature-selects-feature-template',
  'prefills-all-field-ids',
  'omits-empty-element-field',
  'never-emits-labels-or-body',
  'truncates-deterministically-under-cap',
] as const;

function findKindByWire(wire: unknown, path: string): FeedbackKind {
  if (typeof wire !== 'string') throw new Error(`${path} must be a string`);
  const match = ALL_FEEDBACK_KINDS.find((item) => item.wire === wire);
  if (!match) throw new Error(`${path} has unknown kind ${JSON.stringify(wire)}`);
  return match;
}

function parseElement(raw: unknown, path: string): PickedElementInfo | null {
  if (raw === null) return null;
  const row = requireRecord(raw, path);
  requireExactRequiredFields(row, ['selector', 'label', 'section', 'textSnippet', 'box'], path);
  const box = requireRecord(row.box, `${path}.box`);
  requireExactRequiredFields(box, ['top', 'left', 'width', 'height'], `${path}.box`);
  return {
    selector: String(row.selector),
    label: String(row.label),
    section: String(row.section),
    textSnippet: String(row.textSnippet),
    box: {
      top: Number(box.top),
      left: Number(box.left),
      width: Number(box.width),
      height: Number(box.height),
    },
  };
}

function parseContext(raw: unknown, path: string): FeedbackContextInfo {
  const row = requireRecord(raw, path);
  requireExactRequiredFields(row, ['route', 'viewport', 'theme', 'appVersion', 'userAgent'], path);
  const viewport = requireRecord(row.viewport, `${path}.viewport`);
  requireExactRequiredFields(viewport, ['width', 'height'], `${path}.viewport`);
  return {
    route: String(row.route),
    viewport: { width: Number(viewport.width), height: Number(viewport.height) },
    theme: String(row.theme),
    appVersion: String(row.appVersion),
    userAgent: String(row.userAgent),
  };
}

interface ParsedUrlCase {
  readonly name: string;
  readonly maxLength?: number;
  readonly input: IssueDraftInput;
  readonly expect: {
    readonly template: string;
    readonly truncated: boolean;
    readonly params?: Record<string, string>;
    readonly absentParams?: readonly string[];
    readonly forbiddenParams?: readonly string[];
    readonly maxUrlLength?: number;
    readonly descriptionEndsWith?: string;
    readonly elementEndsWith?: string;
    readonly elementContains?: readonly string[];
    readonly environmentContains?: readonly string[];
    readonly environmentExcludes?: readonly string[];
  };
}

function parseStringRecord(raw: unknown, path: string): Record<string, string> {
  const row = requireRecord(raw, path);
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(row)) {
    if (typeof value !== 'string') throw new Error(`${path}.${key} must be a string`);
    out[key] = value;
  }
  return out;
}

function parseStringList(raw: unknown, path: string): string[] {
  if (!Array.isArray(raw) || raw.some((item) => typeof item !== 'string')) {
    throw new Error(`${path} must be a string list`);
  }
  return raw as string[];
}

function loadFixture(): ParsedUrlCase[] {
  const path = resolve(process.cwd(), 'src/lib/feedback/testdata/issue_url_cases.yaml');
  const root = requireRecord(parseStrictYAML(readFileSync(path, 'utf8'), 'issue url cases'), 'issue url cases');
  requireExactRequiredFields(root, ['cases'], 'issue url cases');
  if (!Array.isArray(root.cases)) throw new Error('issue url cases.cases must be a list');
  const rows = root.cases.map((value, index) => requireRecord(value, `cases[${index}]`));
  requireUniqueNames(rows, 'issue url cases.cases');
  const names = new Set(rows.map((row) => row.name));
  const missing = REQUIRED_CASES.filter((name) => !names.has(name));
  if (missing.length > 0) {
    throw new Error(`issue url cases missing required cases: ${missing.join(', ')}`);
  }

  return rows.map((row, index) => {
    const casePath = `cases[${index}]`;
    requireExactFields(row, ['name', 'maxLength', 'input', 'expect'], casePath);
    if (!('name' in row) || !('input' in row) || !('expect' in row)) {
      throw new Error(`${casePath} requires name, input, and expect`);
    }
    const rawInput = requireRecord(row.input, `${casePath}.input`);
    requireExactFields(
      rawInput,
      ['kind', 'title', 'description', 'descriptionRepeat', 'element', 'context'],
      `${casePath}.input`,
    );
    let description = typeof rawInput.description === 'string' ? rawInput.description : '';
    if (rawInput.descriptionRepeat !== undefined) {
      const repeat = requireRecord(rawInput.descriptionRepeat, `${casePath}.input.descriptionRepeat`);
      requireExactRequiredFields(repeat, ['unit', 'count'], `${casePath}.input.descriptionRepeat`);
      if (typeof repeat.unit !== 'string' || typeof repeat.count !== 'number') {
        throw new Error(`${casePath}.input.descriptionRepeat requires string unit and number count`);
      }
      description = repeat.unit.repeat(repeat.count);
    }

    const rawExpect = requireRecord(row.expect, `${casePath}.expect`);
    requireExactFields(
      rawExpect,
      [
        'template',
        'truncated',
        'params',
        'absentParams',
        'forbiddenParams',
        'maxUrlLength',
        'descriptionEndsWith',
        'elementEndsWith',
        'elementContains',
        'environmentContains',
        'environmentExcludes',
      ],
      `${casePath}.expect`,
    );
    if (typeof rawExpect.template !== 'string' || typeof rawExpect.truncated !== 'boolean') {
      throw new Error(`${casePath}.expect requires string template and boolean truncated`);
    }

    return {
      name: row.name as string,
      maxLength: typeof row.maxLength === 'number' ? row.maxLength : undefined,
      input: {
        kind: findKindByWire(rawInput.kind, `${casePath}.input.kind`),
        title: String(rawInput.title ?? ''),
        description,
        element: parseElement(rawInput.element, `${casePath}.input.element`),
        context: parseContext(rawInput.context, `${casePath}.input.context`),
      },
      expect: {
        template: rawExpect.template,
        truncated: rawExpect.truncated,
        params: rawExpect.params !== undefined ? parseStringRecord(rawExpect.params, `${casePath}.expect.params`) : undefined,
        absentParams:
          rawExpect.absentParams !== undefined
            ? parseStringList(rawExpect.absentParams, `${casePath}.expect.absentParams`)
            : undefined,
        forbiddenParams:
          rawExpect.forbiddenParams !== undefined
            ? parseStringList(rawExpect.forbiddenParams, `${casePath}.expect.forbiddenParams`)
            : undefined,
        maxUrlLength: typeof rawExpect.maxUrlLength === 'number' ? rawExpect.maxUrlLength : undefined,
        descriptionEndsWith:
          typeof rawExpect.descriptionEndsWith === 'string' ? rawExpect.descriptionEndsWith : undefined,
        elementEndsWith: typeof rawExpect.elementEndsWith === 'string' ? rawExpect.elementEndsWith : undefined,
        elementContains:
          rawExpect.elementContains !== undefined
            ? parseStringList(rawExpect.elementContains, `${casePath}.expect.elementContains`)
            : undefined,
        environmentContains:
          rawExpect.environmentContains !== undefined
            ? parseStringList(rawExpect.environmentContains, `${casePath}.expect.environmentContains`)
            : undefined,
        environmentExcludes:
          rawExpect.environmentExcludes !== undefined
            ? parseStringList(rawExpect.environmentExcludes, `${casePath}.expect.environmentExcludes`)
            : undefined,
      },
    };
  });
}

const fixtureCases = loadFixture();

describe('feedback issueUrl', () => {
  it.each(fixtureCases.map((c) => [c.name, c] as const))('%s', (_name, testCase) => {
    const result = buildGitHubIssueUrl(testCase.input, DEFAULT_REPO_ISSUES_URL, testCase.maxLength);
    expect(result.template).toBe(testCase.expect.template);
    expect(result.truncated).toBe(testCase.expect.truncated);

    const parsed = new URL(result.url);
    expect(`${parsed.origin}${parsed.pathname}`).toBe(DEFAULT_REPO_ISSUES_URL);
    expect(result.url.length).toBeLessThanOrEqual(testCase.expect.maxUrlLength ?? MAX_ISSUE_URL_LENGTH);

    if (testCase.expect.params) {
      for (const [key, value] of Object.entries(testCase.expect.params)) {
        expect(parsed.searchParams.get(key)).toBe(value);
      }
    }
    if (testCase.expect.absentParams) {
      for (const key of testCase.expect.absentParams) {
        expect(parsed.searchParams.has(key)).toBe(false);
      }
    }
    if (testCase.expect.forbiddenParams) {
      for (const key of testCase.expect.forbiddenParams) {
        expect(parsed.searchParams.has(key)).toBe(false);
      }
    }
    if (testCase.expect.descriptionEndsWith) {
      const desc = parsed.searchParams.get(IssueFormFieldId.Description.wire) ?? '';
      expect(desc.endsWith(testCase.expect.descriptionEndsWith)).toBe(true);
    }
    if (testCase.expect.elementEndsWith) {
      const elem = parsed.searchParams.get(IssueFormFieldId.Element.wire) ?? '';
      expect(elem.endsWith(testCase.expect.elementEndsWith)).toBe(true);
    }
    if (testCase.expect.elementContains) {
      const elem = parsed.searchParams.get(IssueFormFieldId.Element.wire) ?? '';
      for (const fragment of testCase.expect.elementContains) {
        expect(elem).toContain(fragment);
      }
    }
    if (testCase.expect.environmentContains) {
      const env = parsed.searchParams.get(IssueFormFieldId.Environment.wire) ?? '';
      for (const fragment of testCase.expect.environmentContains) {
        expect(env).toContain(fragment);
      }
    }
    if (testCase.expect.environmentExcludes) {
      const env = parsed.searchParams.get(IssueFormFieldId.Environment.wire) ?? '';
      for (const fragment of testCase.expect.environmentExcludes) {
        expect(env).not.toContain(fragment);
      }
    }

    // Never prefill screenshot.
    expect(parsed.searchParams.has(IssueFormFieldId.Screenshot.wire)).toBe(false);
  });

  it('formatElementBlock returns empty string for null and templateForKind matches kind.template', () => {
    expect(formatElementBlock(null)).toBe('');
    for (const kind of ALL_FEEDBACK_KINDS) {
      expect(templateForKind(kind)).toBe(kind.template);
    }
    expect(
      formatEnvironmentBlock({
        route: '/review',
        viewport: { width: 1024, height: 768 },
        theme: 'dark',
        appVersion: '0.1.0',
        userAgent: 'TestUA',
      }),
    ).toBe('route: /review\nviewport: 1024x768\ntheme: dark\npeasant: 0.1.0\nbrowser: TestUA');
  });

  it('produces identical URLs on repeated calls (deterministic truncation)', () => {
    const input: IssueDraftInput = {
      kind: ALL_FEEDBACK_KINDS[0],
      title: 'Deterministic test',
      description: '😀'.repeat(2500),
      element: null,
      context: {
        route: '/',
        viewport: { width: 1440, height: 900 },
        theme: 'dark',
        appVersion: '1.0.0',
        userAgent: 'UA',
      },
    };
    const first = buildGitHubIssueUrl(input);
    const second = buildGitHubIssueUrl(input);
    expect(first).toEqual(second);
    expect(first.truncated).toBe(true);
    expect(first.url.length).toBeLessThanOrEqual(MAX_ISSUE_URL_LENGTH);
  });

  it('validates GitHub issue-form YAML templates against schema rules and IssueFormFieldId', () => {
    const expectedFieldIds = new Set(ALL_ISSUE_FORM_FIELD_IDS.map((field) => field.wire));
    const expectedLabelsByKind = new Map<string, readonly string[]>([
      ['in-app-problem.yml', ['bug', 'ui/ux']],
      ['in-app-feature.yml', ['enhancement', 'ui/ux']],
    ]);

    for (const kind of ALL_FEEDBACK_KINDS) {
      const templatePath = resolve(process.cwd(), '../.github/ISSUE_TEMPLATE', kind.template);
      const doc = requireRecord(
        parseStrictYAML(readFileSync(templatePath, 'utf8'), kind.template),
        kind.template,
      );
      expect(typeof doc.name).toBe('string');
      expect((doc.name as string).length).toBeGreaterThan(0);
      expect(typeof doc.description).toBe('string');
      expect((doc.description as string).length).toBeGreaterThan(0);
      expect(doc.labels).toEqual(expectedLabelsByKind.get(kind.template));
      expect(Array.isArray(doc.body)).toBe(true);

      const seenIds = new Set<string>();
      for (const [idx, rawBlock] of (doc.body as unknown[]).entries()) {
        const block = requireRecord(rawBlock, `${kind.template}.body[${idx}]`);
        expect(typeof block.type).toBe('string');
        if (block.type === 'markdown') {
          expect('id' in block).toBe(false);
          const attrs = requireRecord(block.attributes, `${kind.template}.body[${idx}].attributes`);
          expect(typeof attrs.value).toBe('string');
        } else {
          expect(block.type).toBe('textarea');
          expect(typeof block.id).toBe('string');
          const id = block.id as string;
          expect(seenIds.has(id)).toBe(false);
          seenIds.add(id);
          const attrs = requireRecord(block.attributes, `${kind.template}.body[${idx}].attributes`);
          expect(typeof attrs.label).toBe('string');
        }
      }
      expect(seenIds).toEqual(expectedFieldIds);
    }
  });
});
