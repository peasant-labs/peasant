/** GitHub issue-form URL builder with deterministic length capping. */

import {
  IssueFormFieldId,
  type FeedbackContextInfo,
  type FeedbackKind,
  type IssueDraftInput,
  type PickedElementInfo,
} from '@/lib/feedback/types';

export const MAX_ISSUE_URL_LENGTH = 7500;
export const DEFAULT_REPO_ISSUES_URL = 'https://github.com/peasant-labs/peasant/issues/new';

const TRUNCATION_SUFFIX = '… [truncated]';

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

export function formatElementBlock(target: PickedElementInfo | null): string {
  if (!target) {
    return '';
  }
  const lines = [
    `label: ${target.label}`,
    `section: ${target.section}`,
    `selector: ${target.selector}`,
    `box: ${Math.round(target.box.width)}x${Math.round(target.box.height)} at (${Math.round(target.box.left)}, ${Math.round(target.box.top)})`,
  ];
  if (target.textSnippet) {
    lines.push(`text: ${target.textSnippet}`);
  }
  return lines.join('\n');
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
  const initialElementText = formatElementBlock(input.element);
  const separator = baseUrl.includes('?') ? '&' : '?';

  const encodeUrl = (descriptionText: string, elementText: string, titleText: string): string => {
    const params = new URLSearchParams();
    params.set('template', template);
    params.set('title', titleText);
    params.set(IssueFormFieldId.Description.wire, descriptionText);
    if (elementText !== '') {
      params.set(IssueFormFieldId.Element.wire, elementText);
    }
    params.set(IssueFormFieldId.Environment.wire, environmentText);
    return `${baseUrl}${separator}${params.toString()}`;
  };

  const initialUrl = encodeUrl(input.description, initialElementText, title);
  if (initialUrl.length <= maxLength) {
    return { url: initialUrl, template, truncated: false };
  }

  // Shrink description first while keeping element and environment intact.
  const shrunkDesc = shrinkFieldToFit(
    input.description,
    (candidateDesc) => encodeUrl(candidateDesc, initialElementText, title),
    maxLength,
  );
  if (shrunkDesc.fits) {
    return { url: shrunkDesc.url, template, truncated: true };
  }

  // Description is already at its minimum marker; shrink element next.
  const shrunkElem = shrinkFieldToFit(
    initialElementText,
    (candidateElem) => encodeUrl(shrunkDesc.value, candidateElem, title),
    maxLength,
  );
  if (shrunkElem.fits) {
    return { url: shrunkElem.url, template, truncated: true };
  }

  // If even minimal markers on description/element exceed maxLength, drop them before touching title;
  // template and environment are never cut.
  let finalDesc = shrunkDesc.value;
  let finalElem = '';
  let fallbackUrl = encodeUrl(finalDesc, finalElem, title);
  if (fallbackUrl.length > maxLength && finalDesc !== '') {
    finalDesc = '';
    fallbackUrl = encodeUrl(finalDesc, finalElem, title);
  }
  if (fallbackUrl.length > maxLength) {
    const shrunkTitle = shrinkFieldToFit(
      title,
      (candidateTitle) => encodeUrl(finalDesc, finalElem, candidateTitle),
      maxLength,
    );
    fallbackUrl = shrunkTitle.url;
  }

  return { url: fallbackUrl, template, truncated: true };
}
