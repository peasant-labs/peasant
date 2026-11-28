/** GitHub issue-form URL builder with deterministic length capping. */

import {
  FeedbackKind,
  IssueFormFieldId,
  type FeedbackContextInfo,
  type IssueDraftInput,
  type PickedElementInfo,
} from '@/lib/feedback/types';

export const MAX_ISSUE_URL_LENGTH = 7500;
export const DEFAULT_REPO_ISSUES_URL = 'https://github.com/peasant-labs/peasant/issues/new';

const TRUNCATION_SUFFIX = '… [truncated]';
const SINGLE_IMAGE_PLACEHOLDER = '<!-- paste image -->';
const SATISFACTION_COMMENT = '<!-- optional: what would ensure user satisfaction -->';

const TOP_HEADING_BY_KIND: Readonly<Record<FeedbackKind['wire'], string>> = Object.freeze({
  [FeedbackKind.Problem.wire]: '# Bug Report',
  [FeedbackKind.Feature.wire]: '# Feature Request',
});

const PROBLEM_PROMPT_BY_KIND: Readonly<Record<FeedbackKind['wire'], string>> = Object.freeze({
  [FeedbackKind.Problem.wire]: '<!-- what happened -->',
  [FeedbackKind.Feature.wire]: '<!-- what is the problem or need? -->',
});

const EXPECTATIONS_PROMPT_BY_KIND: Readonly<Record<FeedbackKind['wire'], string>> = Object.freeze({
  [FeedbackKind.Problem.wire]: '<!-- what was the expected behaviour, bullet points. -->',
  [FeedbackKind.Feature.wire]: '<!-- what should peasant do instead? bullet points. -->',
});

export interface IssueUrlResult {
  readonly url: string;
  readonly body: string;
  readonly template: string;
  readonly truncated: boolean;
}

export function templateForKind(kind: FeedbackKind): string {
  return kind.template;
}

export function formatEnvironmentBlock(ctx: FeedbackContextInfo): string {
  // Defensively strip query/hash in case a caller passed a full location path.
  const route = (ctx.route.split('?')[0] ?? '').split('#')[0] ?? '';
  return [
    `peasant version: ${ctx.appVersion}`,
    `route: ${route}`,
    `viewport: ${ctx.viewport.width}x${ctx.viewport.height}`,
    `theme: ${ctx.theme}`,
    `browser: ${ctx.userAgent}`,
  ].join('\n');
}

export function formatElementBlock(target: PickedElementInfo | null, index = 0): string {
  if (!target) {
    return '';
  }
  const n = index + 1;
  const lines = [
    `element ${n} (image ${n}): ${target.label}`,
    `section: ${target.section}`,
    `selector: ${target.selector}`,
    `box: ${Math.round(target.box.width)}x${Math.round(target.box.height)} at (${Math.round(target.box.left)}, ${Math.round(target.box.top)})`,
  ];
  if (target.textSnippet) {
    lines.push(`text: ${target.textSnippet}`);
  }
  return lines.join('\n');
}

function formatOmittedNote(omittedCount: number): string {
  return `${omittedCount} more ${omittedCount === 1 ? 'element' : 'elements'} omitted`;
}

function fenceTextBlock(content: string): string {
  const longestFence = Math.max(2, ...(content.match(/`+/g) ?? []).map((run) => run.length));
  const fence = '`'.repeat(longestFence + 1);
  return `${fence}text\n${content}\n${fence}`;
}

function formatImagePlaceholders(imageCount: number): string {
  if (imageCount <= 0) {
    return SINGLE_IMAGE_PLACEHOLDER;
  }
  return Array.from({ length: imageCount }, (_, index) => `<!-- paste image ${index + 1} -->`).join('\n');
}

function formatProblemSectionContent(
  kind: FeedbackKind,
  imageCount: number,
  elements: readonly PickedElementInfo[],
  keptCount: number,
  includeDetails = true,
): string {
  const parts: string[] = [formatImagePlaceholders(imageCount)];

  if (elements.length > 0 && includeDetails) {
    const clamped = Math.max(0, Math.min(keptCount, elements.length));
    const elementParts = elements.slice(0, clamped).map((el, index) => fenceTextBlock(formatElementBlock(el, index)));
    const omittedCount = elements.length - clamped;
    if (omittedCount > 0) {
      elementParts.push(formatOmittedNote(omittedCount));
    }
    parts.push(`<details><summary>picked elements</summary>\n\n${elementParts.join('\n\n')}\n\n</details>`);
  }

  parts.push(PROBLEM_PROMPT_BY_KIND[kind.wire]);
  return parts.join('\n\n');
}

function formatExpectationsSectionContent(kind: FeedbackKind): string {
  return `${EXPECTATIONS_PROMPT_BY_KIND[kind.wire]}\n\n${SATISFACTION_COMMENT}`;
}

function formatIssueBody(
  kind: FeedbackKind,
  problemContent: string,
  environmentText: string,
): string {
  return [
    TOP_HEADING_BY_KIND[kind.wire],
    `## ${IssueFormFieldId.Problem.label}\n\n${problemContent}`,
    `## ${IssueFormFieldId.Expectations.label}\n\n${formatExpectationsSectionContent(kind)}`,
    `## ${IssueFormFieldId.Environment.label}\n\n${fenceTextBlock(environmentText)}`,
  ].join('\n\n');
}

function sliceCodeUnitSafe(text: string, maxUnits: number): string {
  if (maxUnits <= 0) return '';
  if (maxUnits >= text.length) return text;
  const lastCode = text.charCodeAt(maxUnits - 1);
  // Avoid splitting a UTF-16 surrogate pair across the truncation boundary.
  if (lastCode >= 0xd800 && lastCode <= 0xdbff) {
    return text.slice(0, maxUnits - 1);
  }
  return text.slice(0, maxUnits);
}

function shrinkFieldToFit<T>(
  raw: string,
  buildCandidate: (candidate: string) => { url: string; payload: T },
  maxLength: number,
): { value: string; url: string; payload: T; fits: boolean } {
  if (raw === '') {
    const built = buildCandidate('');
    return { value: '', url: built.url, payload: built.payload, fits: built.url.length <= maxLength };
  }

  const minBuilt = buildCandidate(TRUNCATION_SUFFIX);
  if (minBuilt.url.length > maxLength) {
    return { value: TRUNCATION_SUFFIX, url: minBuilt.url, payload: minBuilt.payload, fits: false };
  }

  let low = 0;
  let high = raw.length;
  let bestValue = TRUNCATION_SUFFIX;
  let bestBuilt = minBuilt;

  while (low <= high) {
    const mid = Math.floor((low + high) / 2);
    const candidate = `${sliceCodeUnitSafe(raw, mid)}${TRUNCATION_SUFFIX}`;
    const candidateBuilt = buildCandidate(candidate);
    if (candidateBuilt.url.length <= maxLength) {
      bestValue = candidate;
      bestBuilt = candidateBuilt;
      low = mid + 1;
    } else {
      high = mid - 1;
    }
  }

  return { value: bestValue, url: bestBuilt.url, payload: bestBuilt.payload, fits: true };
}

export function buildGitHubIssueUrl(
  input: IssueDraftInput,
  baseUrl: string = DEFAULT_REPO_ISSUES_URL,
  maxLength: number = MAX_ISSUE_URL_LENGTH,
): IssueUrlResult {
  const template = templateForKind(input.kind);
  const title = input.title.trim() || input.kind.label;
  const environmentText = formatEnvironmentBlock(input.context);
  const fullElementCount = input.elements.length;
  const separator = baseUrl.includes('?') ? '&' : '?';

  const encodeUrl = (
    elements: readonly PickedElementInfo[],
    keptCount: number,
    titleText: string,
    includeDetails = true,
  ): { url: string; payload: string } => {
    const problemContent = formatProblemSectionContent(
      input.kind,
      input.imageCount,
      elements,
      keptCount,
      includeDetails,
    );
    const bodyText = formatIssueBody(input.kind, problemContent, environmentText);
    const params = new URLSearchParams();
    params.set('template', template);
    params.set('title', titleText);
    params.set(IssueFormFieldId.Problem.wire, problemContent);
    params.set(IssueFormFieldId.Environment.wire, environmentText);
    params.set('body', bodyText);
    return {
      url: `${baseUrl}${separator}${params.toString()}`,
      payload: bodyText,
    };
  };

  const initial = encodeUrl(input.elements, fullElementCount, title);
  if (initial.url.length <= maxLength) {
    return { url: initial.url, body: initial.payload, template, truncated: false };
  }

  if (fullElementCount >= 1) {
    // Drop trailing element blocks first (keeping at least element 1 so its snippet can shrink next).
    for (let kept = fullElementCount - 1; kept >= 1; kept -= 1) {
      const candidate = encodeUrl(input.elements, kept, title, true);
      if (candidate.url.length <= maxLength) {
        return { url: candidate.url, body: candidate.payload, template, truncated: true };
      }
    }

    // Shorten the remaining element's textSnippet before dropping the last element block.
    const firstEl = input.elements[0];
    if (firstEl.textSnippet !== '') {
      const shrunkSnippet = shrinkFieldToFit(
        firstEl.textSnippet,
        (candidateSnippet) =>
          encodeUrl([{ ...firstEl, textSnippet: candidateSnippet }, ...input.elements.slice(1)], 1, title, true),
        maxLength,
      );
      if (shrunkSnippet.fits) {
        return { url: shrunkSnippet.url, body: shrunkSnippet.payload, template, truncated: true };
      }

      const emptySnippet = encodeUrl(
        [{ ...firstEl, textSnippet: '' }, ...input.elements.slice(1)],
        1,
        title,
        true,
      );
      if (emptySnippet.url.length <= maxLength) {
        return { url: emptySnippet.url, body: emptySnippet.payload, template, truncated: true };
      }
    }

    // Drop the last element as well, keeping the omission note.
    const omittedAll = encodeUrl(input.elements, 0, title, true);
    if (omittedAll.url.length <= maxLength) {
      return { url: omittedAll.url, body: omittedAll.payload, template, truncated: true };
    }
  }

  // Last-resort cap fallback for tiny custom maxLength: drop details block, then shrink title;
  // template, image placeholders, and environment are never cut.
  let fallback = encodeUrl(input.elements, 0, title, false);
  if (fallback.url.length > maxLength) {
    const shrunkTitle = shrinkFieldToFit(
      title,
      (candidateTitle) => encodeUrl(input.elements, 0, candidateTitle, false),
      maxLength,
    );
    fallback = { url: shrunkTitle.url, payload: shrunkTitle.payload };
  }

  return { url: fallback.url, body: fallback.payload, template, truncated: true };
}
