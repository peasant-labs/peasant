import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import YAML from 'yaml';
import {
  ALL_FEEDBACK_KINDS,
  ALL_ISSUE_FORM_FIELD_IDS,
  FeedbackKind,
  IssueFormFieldId,
  type FeedbackContextInfo,
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
  'body-bug-heading',
  'body-feature-heading',
  'body-image-placeholders-numbered',
  'body-elements-collapsed',
  'prefill-problem-matches-body-section',
  'never-emits-labels',
  'drops-trailing-elements-when-elements-exceed-cap',
  'shortens-element-snippet-when-single-element-exceeds-cap',
] as const;

function findKindByWire(wire: unknown, path: string): FeedbackKind {
  if (typeof wire !== 'string') throw new Error(`${path} must be a string`);
  const match = ALL_FEEDBACK_KINDS.find((item) => item.wire === wire);
  if (!match) throw new Error(`${path} has unknown kind ${JSON.stringify(wire)}`);
  return match;
}

function parseElements(raw: unknown, path: string): PickedElementInfo[] {
  if (!Array.isArray(raw)) {
    throw new Error(`${path} must be an array`);
  }
  return raw.map((item, idx) => {
    const itemPath = `${path}[${idx}]`;
    const row = requireRecord(item, itemPath);
    requireExactRequiredFields(row, ['id', 'selector', 'label', 'section', 'textSnippet', 'box'], itemPath);
    const box = requireRecord(row.box, `${itemPath}.box`);
    requireExactRequiredFields(box, ['top', 'left', 'width', 'height'], `${itemPath}.box`);
    return {
      id: Number(row.id),
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
  });
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
    readonly forbiddenParams?: readonly string[];
    readonly maxUrlLength?: number;
    readonly problemMatchesBodySection?: boolean;
    readonly problemContains?: readonly string[];
    readonly problemExcludes?: readonly string[];
    readonly bodyStartsWith?: string;
    readonly bodyContains?: readonly string[];
    readonly bodyExcludes?: readonly string[];
    readonly bodyOrder?: readonly string[];
    readonly environmentStartsWith?: string;
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
    requireExactRequiredFields(
      rawInput,
      ['kind', 'title', 'elements', 'imageCount', 'context'],
      `${casePath}.input`,
    );
    if (typeof rawInput.imageCount !== 'number') {
      throw new Error(`${casePath}.input.imageCount must be a number`);
    }

    const rawExpect = requireRecord(row.expect, `${casePath}.expect`);
    requireExactFields(
      rawExpect,
      [
        'template',
        'truncated',
        'params',
        'forbiddenParams',
        'maxUrlLength',
        'problemMatchesBodySection',
        'problemContains',
        'problemExcludes',
        'bodyStartsWith',
        'bodyContains',
        'bodyExcludes',
        'bodyOrder',
        'environmentStartsWith',
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
        elements: parseElements(rawInput.elements, `${casePath}.input.elements`),
        imageCount: rawInput.imageCount,
        context: parseContext(rawInput.context, `${casePath}.input.context`),
      },
      expect: {
        template: rawExpect.template,
        truncated: rawExpect.truncated,
        params: rawExpect.params !== undefined ? parseStringRecord(rawExpect.params, `${casePath}.expect.params`) : undefined,
        forbiddenParams:
          rawExpect.forbiddenParams !== undefined
            ? parseStringList(rawExpect.forbiddenParams, `${casePath}.expect.forbiddenParams`)
            : undefined,
        maxUrlLength: typeof rawExpect.maxUrlLength === 'number' ? rawExpect.maxUrlLength : undefined,
        problemMatchesBodySection: rawExpect.problemMatchesBodySection === true,
        problemContains:
          rawExpect.problemContains !== undefined
            ? parseStringList(rawExpect.problemContains, `${casePath}.expect.problemContains`)
            : undefined,
        problemExcludes:
          rawExpect.problemExcludes !== undefined
            ? parseStringList(rawExpect.problemExcludes, `${casePath}.expect.problemExcludes`)
            : undefined,
        bodyStartsWith: typeof rawExpect.bodyStartsWith === 'string' ? rawExpect.bodyStartsWith : undefined,
        bodyContains:
          rawExpect.bodyContains !== undefined
            ? parseStringList(rawExpect.bodyContains, `${casePath}.expect.bodyContains`)
            : undefined,
        bodyExcludes:
          rawExpect.bodyExcludes !== undefined
            ? parseStringList(rawExpect.bodyExcludes, `${casePath}.expect.bodyExcludes`)
            : undefined,
        bodyOrder:
          rawExpect.bodyOrder !== undefined
            ? parseStringList(rawExpect.bodyOrder, `${casePath}.expect.bodyOrder`)
            : undefined,
        environmentStartsWith:
          typeof rawExpect.environmentStartsWith === 'string' ? rawExpect.environmentStartsWith : undefined,
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

function extractProblemSectionFromBody(body: string): string {
  const startMarker = `## ${IssueFormFieldId.Problem.label}\n\n`;
  const endMarker = `\n\n## ${IssueFormFieldId.Expectations.label}`;
  const startIdx = body.indexOf(startMarker);
  const endIdx = body.indexOf(endMarker);
  if (startIdx === -1 || endIdx === -1 || endIdx <= startIdx) {
    throw new Error('Body missing Problem Statement or Expectations section markers');
  }
  return body.slice(startIdx + startMarker.length, endIdx);
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
    expect(parsed.searchParams.get('body')).toBe(result.body);

    if (testCase.expect.params) {
      for (const [key, value] of Object.entries(testCase.expect.params)) {
        expect(parsed.searchParams.get(key)).toBe(value);
      }
    }
    if (testCase.expect.forbiddenParams) {
      for (const key of testCase.expect.forbiddenParams) {
        expect(parsed.searchParams.has(key)).toBe(false);
      }
    }
    if (testCase.expect.problemMatchesBodySection) {
      const problemParam = parsed.searchParams.get(IssueFormFieldId.Problem.wire) ?? '';
      expect(problemParam).toBe(extractProblemSectionFromBody(result.body));
    }
    if (testCase.expect.problemContains) {
      const problemParam = parsed.searchParams.get(IssueFormFieldId.Problem.wire) ?? '';
      for (const fragment of testCase.expect.problemContains) {
        expect(problemParam).toContain(fragment);
      }
    }
    if (testCase.expect.problemExcludes) {
      const problemParam = parsed.searchParams.get(IssueFormFieldId.Problem.wire) ?? '';
      for (const fragment of testCase.expect.problemExcludes) {
        expect(problemParam).not.toContain(fragment);
      }
    }
    if (testCase.expect.bodyStartsWith) {
      expect(result.body.startsWith(testCase.expect.bodyStartsWith)).toBe(true);
    }
    if (testCase.expect.bodyContains) {
      for (const fragment of testCase.expect.bodyContains) {
        expect(result.body).toContain(fragment);
      }
    }
    if (testCase.expect.bodyExcludes) {
      for (const fragment of testCase.expect.bodyExcludes) {
        expect(result.body).not.toContain(fragment);
      }
    }
    if (testCase.expect.bodyOrder) {
      let lastIndex = -1;
      for (const heading of testCase.expect.bodyOrder) {
        const idx = result.body.indexOf(heading);
        expect(idx).toBeGreaterThan(lastIndex);
        lastIndex = idx;
      }
    }
    if (testCase.expect.environmentStartsWith) {
      const env = parsed.searchParams.get(IssueFormFieldId.Environment.wire) ?? '';
      expect(env.startsWith(testCase.expect.environmentStartsWith)).toBe(true);
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

    // Never prefill expectations, element, description, or screenshot.
    expect(parsed.searchParams.has(IssueFormFieldId.Expectations.wire)).toBe(false);
    expect(parsed.searchParams.has('element')).toBe(false);
    expect(parsed.searchParams.has('description')).toBe(false);
    expect(parsed.searchParams.has('screenshot')).toBe(false);
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
    ).toBe('peasant version: 0.1.0\nroute: /review\nviewport: 1024x768\ntheme: dark\nbrowser: TestUA');
  });

  it('produces identical URLs on repeated calls (deterministic truncation)', () => {
    const input: IssueDraftInput = {
      kind: ALL_FEEDBACK_KINDS[0],
      title: 'Deterministic test',
      elements: Array.from({ length: 5 }, (_, idx) => ({
        id: idx + 1,
        selector: `main > section:nth-child(${idx + 1}) > div.very-long-container > button.primary`,
        label: `button.primary-${idx + 1}`,
        section: 'Transcript',
        textSnippet: '😀'.repeat(600),
        box: { top: idx * 40, left: 20, width: 300, height: 40 },
      })),
      imageCount: 5,
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

  it('form-ids-and-labels-match', () => {
    const expectedIds = ALL_ISSUE_FORM_FIELD_IDS.map((field) => field.wire);
    const expectedLabels = ALL_ISSUE_FORM_FIELD_IDS.map((field) => field.label);
    const expectedMeta = new Map<
      FeedbackKind,
      { readonly name: string; readonly type: string; readonly labels: readonly string[]; readonly problemPrompt: string; readonly expectationsPrompt: string }
    >([
      [
        FeedbackKind.Problem,
        {
          name: 'Bug report (in-app)',
          type: 'Bug',
          labels: ['bug', 'ui/ux'],
          problemPrompt: '<!-- what happened -->',
          expectationsPrompt: '<!-- what was the expected behaviour, bullet points. -->',
        },
      ],
      [
        FeedbackKind.Feature,
        {
          name: 'Feature request (in-app)',
          type: 'Feature',
          labels: ['enhancement', 'ui/ux'],
          problemPrompt: '<!-- what is the problem or need? -->',
          expectationsPrompt: '<!-- what should peasant do instead? bullet points. -->',
        },
      ],
    ]);

    for (const kind of ALL_FEEDBACK_KINDS) {
      const templatePath = resolve(process.cwd(), '../.github/ISSUE_TEMPLATE', kind.template);
      const doc = requireRecord(YAML.parse(readFileSync(templatePath, 'utf8')), kind.template);
      const expected = expectedMeta.get(kind)!;

      expect(doc.name).toBe(expected.name);
      expect(doc.type).toBe(expected.type);
      expect(doc.labels).toEqual(expected.labels);
      expect(typeof doc.description).toBe('string');
      expect((doc.description as string).length).toBeGreaterThan(0);
      expect(Array.isArray(doc.body)).toBe(true);

      const blocks = doc.body as unknown[];
      const warningBlock = requireRecord(blocks[0], `${kind.template}.body[0]`);
      expect(warningBlock.type).toBe('markdown');
      expect('id' in warningBlock).toBe(false);

      const textareaBlocks = blocks.slice(1);
      const actualIds: string[] = [];
      const actualLabels: string[] = [];

      for (const [idx, rawBlock] of textareaBlocks.entries()) {
        const block = requireRecord(rawBlock, `${kind.template}.body[${idx + 1}]`);
        expect(block.type).toBe('textarea');
        expect(typeof block.id).toBe('string');
        actualIds.push(block.id as string);

        const attrs = requireRecord(block.attributes, `${kind.template}.body[${idx + 1}].attributes`);
        expect(typeof attrs.label).toBe('string');
        actualLabels.push(attrs.label as string);
        expect('render' in attrs).toBe(false);
        expect(typeof attrs.value).toBe('string');

        const validations = requireRecord(block.validations, `${kind.template}.body[${idx + 1}].validations`);
        expect(validations.required).toBe(true);

        if (block.id === IssueFormFieldId.Problem.wire) {
          expect(attrs.value).toContain('<!-- paste image -->');
          expect(attrs.value).toContain(expected.problemPrompt);
        } else if (block.id === IssueFormFieldId.Expectations.wire) {
          expect(attrs.value).toContain(expected.expectationsPrompt);
          expect(attrs.value).toContain('<!-- optional: what would ensure user satisfaction -->');
        } else if (block.id === IssueFormFieldId.Environment.wire) {
          expect(attrs.value).toContain('<!-- user system -->');
          expect(String(attrs.description)).toContain('peasant version');
        }
      }

      expect(actualIds).toEqual(expectedIds);
      expect(actualLabels).toEqual(expectedLabels);
    }
  });
});
