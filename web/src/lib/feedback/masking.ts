import { MaskCategory, MaskMode } from './types';

/** Elements excluded from all feedback captures (the widget itself and the dev overlay). */
export const FEEDBACK_EXCLUDE_SELECTORS: readonly string[] = Object.freeze([
  '[data-feedback-widget]',
  '[data-fb]',
]);

/**
 * Canonical CSS selectors masked during capture, grouped by `MaskCategory.wire`.
 * `TranscriptContent` is masked by default and toggleable after explicit warning;
 * `SensitiveInputs` stays masked in every mode.
 */
export const FEEDBACK_MASK_TARGETS: Readonly<Record<MaskCategory['wire'], readonly string[]>> = Object.freeze({
  [MaskCategory.TranscriptContent.wire]: Object.freeze([
    '[data-turn]',
    '[data-source-entry-ref]',
    '.txn-turnwrap',
    '.txn-turn',
    '.txn-body',
    '.txn-thinking',
    '.txn-toolcall',
    '.txn-work',
    '.txn-turnwork',
    '.txn-modelactions',
    '.txn-diff',
    '.txn-diffentry',
    '.txn-systemrow',
    '.txn-control-row',
    '.txn-rail-turn-copy',
    '.txn-ol-leaf',
  ]),
  [MaskCategory.SensitiveInputs.wire]: Object.freeze([
    'input[type="password"]',
    '[data-feedback-mask="always"]',
  ]),
});

const MASKED_SELECTORS: readonly string[] = Object.freeze([
  ...FEEDBACK_MASK_TARGETS[MaskCategory.TranscriptContent.wire],
  ...FEEDBACK_MASK_TARGETS[MaskCategory.SensitiveInputs.wire],
]);

const UNMASKED_SELECTORS: readonly string[] = Object.freeze([
  ...FEEDBACK_MASK_TARGETS[MaskCategory.SensitiveInputs.wire],
]);

/** Returns the active mask selectors for the given `MaskMode`. */
export function selectorsForMaskMode(mode: MaskMode): readonly string[] {
  switch (mode) {
    case MaskMode.Masked:
      return MASKED_SELECTORS;
    case MaskMode.Unmasked:
      return UNMASKED_SELECTORS;
    default:
      throw new TypeError(`Unknown mask mode: ${String(mode)}`);
  }
}

/** True when `el` is or sits inside an element covered by `mode` masking. */
export function isInsideMaskedElement(el: Element, mode: MaskMode = MaskMode.Masked): boolean {
  const selectors = selectorsForMaskMode(mode);
  if (selectors.length === 0) return false;
  return el.closest(selectors.join(', ')) !== null;
}

/**
 * Generates temporary stylesheet rules that paint masked regions as flat neutral
 * blocks with transparent text (overriding inline syntax-highlighting colors) and
 * hide descendant media elements.
 */
export function buildMaskStyleCss(mode: MaskMode): string {
  const selectors = selectorsForMaskMode(mode);
  if (selectors.length === 0) return '';

  const blockTargets = selectors.flatMap((selector) => [selector, `${selector} *`]).join(',\n');
  const mediaTargets = selectors
    .flatMap((selector) => [
      `${selector}:is(img, svg, canvas, video)`,
      `${selector} img`,
      `${selector} svg`,
      `${selector} canvas`,
      `${selector} video`,
    ])
    .join(',\n');

  return [
    `${blockTargets} {`,
    '  color: transparent !important;',
    '  -webkit-text-fill-color: transparent !important;',
    '  text-shadow: none !important;',
    '  background: var(--surface-2) !important;',
    '}',
    `${mediaTargets} {`,
    '  visibility: hidden !important;',
    '}',
  ].join('\n');
}
