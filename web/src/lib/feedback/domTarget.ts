import {
  PickerDirection,
  type ElementPickerAdapter,
  type LegacyDomTarget,
  type PickedElementDraft,
  type ViewportSize,
} from './types';

/** Query parameter that turns the feedback tools off for the page. */
const DISABLE_PARAM = 'fb';
const DISABLE_VALUE = 'off';

/** Ancestor steps kept in generated CSS selectors. */
const SELECTOR_DEPTH_LIMIT = 6;
/** Classes kept in hover labels and each selector step. */
const CLASS_LIMIT = 3;
/** Legacy dev-overlay `outerHTML` snippet cap. */
const LEGACY_SNIPPET_MAX_CHARS = 400;
/** Default plain-text snippet cap for picked elements. */
const DEFAULT_SAFE_SNIPPET_MAX_CHARS = 120;
const MASKED_TEXT_SNIPPET = '[masked transcript content]';

const TOO_LARGE_WIDTH_RATIO = 0.97;
const TOO_LARGE_HEIGHT_RATIO = 0.92;

const IGNORED_CHROME_SELECTOR = 'head, [data-fb], [data-feedback-widget]';
const LANDMARK_SELECTOR = 'header, nav, main, aside, section, [role="region"], [data-feedback-target]';

/** True when the page loaded with the disable switch, e.g. `?fb=off`. */
export function feedbackDisabled(search: string): boolean {
  return new URLSearchParams(search).get(DISABLE_PARAM) === DISABLE_VALUE;
}

/** True for document/framework root containers that should never be picked directly. */
export function isPageRootElement(el: Element): boolean {
  const tag = el.tagName.toUpperCase();
  return tag === 'HTML' || tag === 'BODY' || tag === 'HEAD' || el.id === 'root' || el.id === '__next';
}

/** True when an element belongs to feedback/dev overlay chrome, `<head>`, or a page root. */
export function isIgnoredFeedbackElement(el: Element): boolean {
  return isPageRootElement(el) || el.closest(IGNORED_CHROME_SELECTOR) !== null;
}

/** True when a bounding box spans effectively the entire viewport. */
export function isTooLargeRect(rect: Pick<DOMRect, 'width' | 'height'>, viewport: ViewportSize): boolean {
  if (viewport.width <= 0 || viewport.height <= 0) return false;
  return (
    rect.width >= viewport.width * TOO_LARGE_WIDTH_RATIO &&
    rect.height >= viewport.height * TOO_LARGE_HEIGHT_RATIO
  );
}

/**
 * A stable-ish CSS selector: an id when one exists, otherwise a bounded ancestor
 * chain with up to {@link CLASS_LIMIT} classes per step and an `:nth-of-type`
 * disambiguator when siblings share a tag.
 */
export function feedbackSelectorFor(el: Element): string {
  if (el.id) return '#' + el.id;
  const parts: string[] = [];
  let node: Element | null = el;
  let depth = 0;
  const body = el.ownerDocument?.body ?? (typeof document !== 'undefined' ? document.body : null);
  while (node && node.nodeType === 1 && node !== body && depth++ < SELECTOR_DEPTH_LIMIT) {
    const current: Element = node;
    let part = current.tagName.toLowerCase();
    if (current.classList.length) {
      part += '.' + Array.from(current.classList).slice(0, CLASS_LIMIT).join('.');
    }
    const parent = current.parentElement;
    if (parent) {
      const sameTag = Array.from(parent.children).filter((child) => child.tagName === current.tagName);
      if (sameTag.length > 1) part += `:nth-of-type(${sameTag.indexOf(current) + 1})`;
    }
    parts.unshift(part);
    if (current.id) {
      parts[0] = '#' + current.id;
      break;
    }
    node = parent;
  }
  return parts.join(' > ');
}

/**
 * A cheap label for the hover tag and payload fallback: tag, id, and the first
 * {@link CLASS_LIMIT} classes. Never touches `textContent` so pointer-move stays layout-free.
 */
export function feedbackLabelFor(el: Element): string {
  const classes = Array.from(el.classList).slice(0, CLASS_LIMIT);
  return el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + (classes.length ? '.' + classes.join('.') : '');
}

/**
 * The owning section of an element: the nearest `<section>` ancestor, named by its
 * `aria-label`, its first heading, or its id. Empty when no section scopes it.
 */
export function feedbackSectionOf(el: Element): string {
  let node: Element | null = el;
  const body = el.ownerDocument?.body ?? (typeof document !== 'undefined' ? document.body : null);
  while (node && node !== body) {
    if (node.tagName === 'SECTION') {
      const label = node.getAttribute('aria-label');
      if (label && label.trim()) return label.trim();
      const heading =
        node.querySelector(':scope > h1, :scope > h2, :scope > h3, :scope > h4') ?? node.querySelector('h1, h2, h3, h4');
      const headingText = (heading?.textContent ?? '').trim();
      if (headingText) return headingText;
      if (node.id) return '#' + node.id;
    }
    node = node.parentElement;
  }
  return '';
}

/** Builds the legacy dev-annotate element description. */
export function feedbackTargetFrom(el: Element): LegacyDomTarget {
  return {
    selector: feedbackSelectorFor(el),
    label: feedbackLabelFor(el),
    section: feedbackSectionOf(el),
    snippet: el.outerHTML.slice(0, LEGACY_SNIPPET_MAX_CHARS),
  };
}

/**
 * Collapses whitespace in `textContent` and caps at `maxChars` (default 120).
 * Returns a fixed placeholder when the element is inside a masked region.
 */
export function safeTextSnippetFor(
  el: Element,
  masked: boolean,
  maxChars: number = DEFAULT_SAFE_SNIPPET_MAX_CHARS,
): string {
  if (masked) return MASKED_TEXT_SNIPPET;
  if (maxChars <= 0) return '';
  const collapsed = (el.textContent ?? '').replace(/\s+/g, ' ').trim();
  return collapsed.length <= maxChars ? collapsed : collapsed.slice(0, maxChars);
}

/** Builds the sanitized picked-element summary for the in-app feedback widget. */
export function describePickedElement(el: Element, masked: boolean): PickedElementDraft {
  const rect = el.getBoundingClientRect();
  return {
    selector: feedbackSelectorFor(el),
    label: feedbackLabelFor(el),
    section: feedbackSectionOf(el),
    textSnippet: safeTextSnippetFor(el, masked),
    box: {
      top: rect.top,
      left: rect.left,
      width: rect.width,
      height: rect.height,
    },
  };
}

function firstSelectableDescendant(root: Element): Element | null {
  for (let child = root.firstElementChild; child; child = child.nextElementSibling) {
    if (child.closest(IGNORED_CHROME_SELECTOR) !== null) continue;
    if (!isPageRootElement(child)) return child;
    const nested = firstSelectableDescendant(child);
    if (nested) return nested;
  }
  return null;
}

/**
 * Moves the picker target across the DOM tree or landmark regions without ever
 * landing on `html`, `body`, `head`, framework roots, or widget chrome.
 */
export function stepDomTarget(current: Element | null, direction: PickerDirection, doc?: Document): Element | null {
  const targetDoc = doc ?? current?.ownerDocument ?? (typeof document !== 'undefined' ? document : undefined);
  if (!targetDoc) return null;

  switch (direction) {
    case PickerDirection.Parent: {
      if (!current) return null;
      let node = current.parentElement;
      while (node) {
        if (!isIgnoredFeedbackElement(node)) return node;
        node = node.parentElement;
      }
      return null;
    }

    case PickerDirection.Child: {
      const root = current ?? targetDoc.body;
      return root ? firstSelectableDescendant(root) : null;
    }

    case PickerDirection.NextSibling: {
      if (!current) return null;
      let sibling = current.nextElementSibling;
      while (sibling) {
        if (!isIgnoredFeedbackElement(sibling)) return sibling;
        sibling = sibling.nextElementSibling;
      }
      return null;
    }

    case PickerDirection.PrevSibling: {
      if (!current) return null;
      let sibling = current.previousElementSibling;
      while (sibling) {
        if (!isIgnoredFeedbackElement(sibling)) return sibling;
        sibling = sibling.previousElementSibling;
      }
      return null;
    }

    case PickerDirection.NextLandmark: {
      const landmarks = Array.from(targetDoc.querySelectorAll(LANDMARK_SELECTOR)).filter(
        (el) => !isIgnoredFeedbackElement(el),
      );
      if (landmarks.length === 0) return null;
      if (!current) return landmarks[0];
      const index = landmarks.indexOf(current);
      if (index !== -1) {
        return landmarks[(index + 1) % landmarks.length];
      }
      const following = landmarks.find(
        (lm) => (current.compareDocumentPosition(lm) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0,
      );
      return following ?? landmarks[0];
    }

    case PickerDirection.PrevLandmark: {
      const landmarks = Array.from(targetDoc.querySelectorAll(LANDMARK_SELECTOR)).filter(
        (el) => !isIgnoredFeedbackElement(el),
      );
      if (landmarks.length === 0) return null;
      if (!current) return landmarks[landmarks.length - 1];
      const index = landmarks.indexOf(current);
      if (index !== -1) {
        return landmarks[(index - 1 + landmarks.length) % landmarks.length];
      }
      for (let i = landmarks.length - 1; i >= 0; i -= 1) {
        if ((current.compareDocumentPosition(landmarks[i]) & Node.DOCUMENT_POSITION_PRECEDING) !== 0) {
          return landmarks[i];
        }
      }
      return landmarks[landmarks.length - 1];
    }

    default:
      throw new TypeError(`Unknown picker direction: ${String(direction)}`);
  }
}

/** Creates the browser DOM adapter consumed by `ElementPickerOverlay`. */
export function createBrowserPickerAdapter(isMasked: (el: Element) => boolean): ElementPickerAdapter {
  return {
    isIgnoredElement: isIgnoredFeedbackElement,
    isTooLargeRect,
    labelFor: feedbackLabelFor,
    sectionOf: feedbackSectionOf,
    describe: (el) => describePickedElement(el, isMasked(el)),
    step: stepDomTarget,
  };
}
