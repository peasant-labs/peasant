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
const SCREENSHOTS_BODY_LINE = 'Paste the copied images here.';

const DESCRIPTION_HEADING_BY_KIND: Readonly<Record<FeedbackKind['wire'], string>> = Object.freeze({
  [FeedbackKind.Problem.wire]: 'What happened?',
  [FeedbackKind.Feature.wire]: 'What would you like to see?',
});

export interface IssueUrlResult {
  readonly url: string;
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
    `route: ${route}`,
    `viewport: ${ctx.viewport.width}x${ctx.viewport.height}`,
    `theme: ${ctx.theme}`,
    `peasant: ${ctx.appVersion}`,
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

export function formatElementsBlock(
  elements: readonly PickedElementInfo[],
  keptCount: number = elements.length,
  includeOmittedNote = true,
): string {
  if (elements.length === 0) {
    return '';
  }
  const clamped = Math.max(0, Math.min(keptCount, elements.length));
  const blocks = elements.slice(0, clamped).map((el, index) => formatElementBlock(el, index));
  const omittedCount = elements.length - clamped;
  if (omittedCount > 0 && includeOmittedNote) {
    blocks.push(formatOmittedNote(omittedCount));
  }
  return blocks.join('\n\n');
}

function fenceTextBlock(content: string): string {
  const longestFence = Math.max(2, ...(content.match(/`+/g) ?? []).map((run) => run.length));
  const fence = '`'.repeat(longestFence + 1);
  return `${fence}text\n${content}\n${fence}`;
}

function formatIssueBody(
  kind: FeedbackKind,
  descriptionText: string,
  elements: readonly PickedElementInfo[],
  keptCount: number,
  environmentText: string,
  includeOmittedNote = true,
): string {
  const sections: string[] = [
    `### ${DESCRIPTION_HEADING_BY_KIND[kind.wire]}\n\n${descriptionText}`,
  ];
  if (elements.length > 0) {
    const clamped = Math.max(0, Math.min(keptCount, elements.length));
    const elementParts = elements.slice(0, clamped).map((el, index) => fenceTextBlock(formatElementBlock(el, index)));
    const omittedCount = elements.length - clamped;
    if (omittedCount > 0 && includeOmittedNote) {
      elementParts.push(formatOmittedNote(omittedCount));
    }
    if (elementParts.length > 0) {
      sections.push(`### Picked elements\n\n${elementParts.join('\n\n')}`);
    }
  }
  sections.push(`### Screenshots\n\n${SCREENSHOTS_BODY_LINE}`);
  sections.push(`### App context\n\n${fenceTextBlock(environmentText)}`);
  return sections.join('\n\n');
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

function shrinkFieldToFit(
  raw: string,
  buildUrl: (candidate: string) => string,
  maxLength: number,
): { value: string; url: string; fits: boolean } {
  if (raw === '') {
    const url = buildUrl('');
    return { value: '', url, fits: url.length <= maxLength };
  }

  const minUrl = buildUrl(TRUNCATION_SUFFIX);
  if (minUrl.length > maxLength) {
    return { value: TRUNCATION_SUFFIX, url: minUrl, fits: false };
  }

  let low = 0;
  let high = raw.length;
  let bestValue = TRUNCATION_SUFFIX;
  let bestUrl = minUrl;

  while (low <= high) {
    const mid = Math.floor((low + high) / 2);
    const candidate = `${sliceCodeUnitSafe(raw, mid)}${TRUNCATION_SUFFIX}`;
    const candidateUrl = buildUrl(candidate);
    if (candidateUrl.length <= maxLength) {
      bestValue = candidate;
      bestUrl = candidateUrl;
      low = mid + 1;
    } else {
      high = mid - 1;
    }
  }

  return { value: bestValue, url: bestUrl, fits: true };
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
    descriptionText: string,
    keptCount: number,
    titleText: string,
    includeOmittedNote = true,
  ): string => {
    const elementFieldText = formatElementsBlock(input.elements, keptCount, includeOmittedNote);
    const bodyText = formatIssueBody(
      input.kind,
      descriptionText,
      input.elements,
      keptCount,
      environmentText,
      includeOmittedNote,
    );
    const params = new URLSearchParams();
    params.set('template', template);
    params.set('title', titleText);
    params.set(IssueFormFieldId.Description.wire, descriptionText);
    if (elementFieldText !== '') {
      params.set(IssueFormFieldId.Element.wire, elementFieldText);
    }
    params.set(IssueFormFieldId.Environment.wire, environmentText);
    params.set('body', bodyText);
    return `${baseUrl}${separator}${params.toString()}`;
  };

  const initialUrl = encodeUrl(input.description, fullElementCount, title);
  if (initialUrl.length <= maxLength) {
    return { url: initialUrl, template, truncated: false };
  }

  // Shrink description first in both the field param and body while keeping elements and environment intact.
  const shrunkDesc = shrinkFieldToFit(
    input.description,
    (candidateDesc) => encodeUrl(candidateDesc, fullElementCount, title),
    maxLength,
  );
  if (shrunkDesc.fits) {
    return { url: shrunkDesc.url, template, truncated: true };
  }

  // Drop element blocks from the end with an omission note before dropping description/title.
  for (let kept = fullElementCount - 1; kept >= 0; kept -= 1) {
    const candidateUrl = encodeUrl(shrunkDesc.value, kept, title, true);
    if (candidateUrl.length <= maxLength) {
      return { url: candidateUrl, template, truncated: true };
    }
  }

  // If even minimal markers exceed a tiny maxLength, drop the omission note and description before touching title;
  // template and environment are never cut.
  let finalDesc = shrunkDesc.value;
  let fallbackUrl = encodeUrl(finalDesc, 0, title, false);
  if (fallbackUrl.length > maxLength && finalDesc !== '') {
    finalDesc = '';
    fallbackUrl = encodeUrl(finalDesc, 0, title, false);
  }
  if (fallbackUrl.length > maxLength) {
    const shrunkTitle = shrinkFieldToFit(
      title,
      (candidateTitle) => encodeUrl(finalDesc, 0, candidateTitle, false),
      maxLength,
    );
    fallbackUrl = shrunkTitle.url;
  }

  return { url: fallbackUrl, template, truncated: true };
}
