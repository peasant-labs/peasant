import { buildMaskStyleCss, FEEDBACK_EXCLUDE_SELECTORS } from './masking';
import {
  CaptureKind,
  ClipboardOutcome,
  type CaptureArtifact,
  type CaptureBatchResult,
  type CaptureRequest,
} from './types';

const MASK_STYLE_ATTR = 'data-feedback-widget';
const MASK_STYLE_VALUE = 'mask-style';
const PNG_MIME = 'image/png';
const DEFAULT_PNG_FILENAME = 'peasant-feedback.png';
const OBJECT_URL_REVOKE_DELAY_MS = 1000;

const DEFAULT_DARK_BACKGROUND = '#0b0b0a';
const DEFAULT_LIGHT_BACKGROUND = '#fbfaf7';

export interface SnapdomCaptureOptions {
  backgroundColor?: string | null;
  exclude?: string | ((el: Element) => boolean) | Array<string | ((el: Element) => boolean)>;
  excludeMode?: 'hide' | 'remove';
  clip?: 'viewport' | { x: number; y: number; width: number; height: number } | null;
  scale?: number;
  dpr?: number;
  format?: 'png';
}

export interface SnapdomCaptureResult {
  readonly meta?: Readonly<{
    readonly w0: number;
    readonly h0: number;
    readonly targetW: number;
    readonly targetH: number;
  }>;
  toBlob(options?: { format?: 'png'; type?: 'png'; scale?: number; dpr?: number }): Promise<Blob>;
}

export type SnapdomFn = (element: Element, options?: SnapdomCaptureOptions) => Promise<SnapdomCaptureResult>;

export interface CaptureDeps {
  loadSnapdom?: () => Promise<SnapdomFn>;
  waitForPaint?: () => Promise<void>;
  doc?: Document;
  win?: Window;
}

async function defaultLoadSnapdom(): Promise<SnapdomFn> {
  const mod = await import('@zumer/snapdom');
  return mod.snapdom;
}

function defaultWaitForPaint(win: Window): Promise<void> {
  return new Promise((resolve) => {
    if (typeof win.requestAnimationFrame === 'function') {
      // Two frames: the first lets React's commit (dialog and scrim removal) reach
      // layout, the second runs after that frame has painted.
      win.requestAnimationFrame(() => win.requestAnimationFrame(() => resolve()));
    } else {
      resolve();
    }
  });
}

function errorMessage(err: unknown, fallback: string): string {
  if (err instanceof Error && err.message) return err.message;
  if (typeof err === 'string' && err) return err;
  return fallback;
}

function resolveDimension(primary: number | undefined, fallback: number): number {
  if (typeof primary === 'number' && Number.isFinite(primary) && primary > 0) {
    return Math.round(primary);
  }
  if (Number.isFinite(fallback) && fallback > 0) {
    return Math.round(fallback);
  }
  return 0;
}

function isTransparentColor(raw: string): boolean {
  const value = raw.trim().toLowerCase();
  if (!value || value === 'transparent' || value === 'none') {
    return true;
  }
  const slashAlpha = value.match(/\/\s*([0-9.]+%?)\s*\)$/);
  if (slashAlpha) {
    return Number.parseFloat(slashAlpha[1]) <= 0;
  }
  const rgbaMatch = value.match(/^(?:rgba|hsla)\([^,]+,[^,]+,[^,]+,\s*([0-9.]+)\s*\)$/);
  if (rgbaMatch) {
    return Number.parseFloat(rgbaMatch[1]) <= 0;
  }
  if (/^#[0-9a-f]{4}$/.test(value)) {
    return value.endsWith('0');
  }
  if (/^#[0-9a-f]{8}$/.test(value)) {
    return value.endsWith('00');
  }
  return false;
}

/**
 * Resolves an opaque background color for captures so dark/light element shots
 * never get transparent corners: `body` background -> `html` background ->
 * `--canvas` CSS custom property -> theme fallback.
 */
export function resolveOpaqueBackgroundColor(doc: Document, win?: Window): string {
  const getStyle = win && typeof win.getComputedStyle === 'function'
    ? win.getComputedStyle.bind(win)
    : typeof getComputedStyle === 'function'
      ? getComputedStyle
      : null;

  if (getStyle) {
    if (doc.body) {
      const bodyBg = getStyle(doc.body).backgroundColor;
      if (bodyBg && !isTransparentColor(bodyBg)) {
        return bodyBg;
      }
    }
    if (doc.documentElement) {
      const htmlBg = getStyle(doc.documentElement).backgroundColor;
      if (htmlBg && !isTransparentColor(htmlBg)) {
        return htmlBg;
      }
      const canvasVar = getStyle(doc.documentElement).getPropertyValue('--canvas').trim();
      if (canvasVar && !isTransparentColor(canvasVar)) {
        return canvasVar;
      }
    }
    if (doc.body) {
      const bodyCanvasVar = getStyle(doc.body).getPropertyValue('--canvas').trim();
      if (bodyCanvasVar && !isTransparentColor(bodyCanvasVar)) {
        return bodyCanvasVar;
      }
    }
  }

  return doc.documentElement?.dataset?.theme === 'light' ? DEFAULT_LIGHT_BACKGROUND : DEFAULT_DARK_BACKGROUND;
}

/**
 * Lazily loads `@zumer/snapdom` and captures the requested element and/or visible
 * viewport under a temporary privacy-masking `<style>` element that is always
 * removed in `finally`. Per-capture failures are collected into `errors` without throwing.
 */
export async function captureFeedbackArtifacts(
  request: CaptureRequest,
  deps: CaptureDeps = {},
): Promise<CaptureBatchResult> {
  if (request.elements.length === 0 && !request.capturePage) {
    return { captures: [], errors: [] };
  }

  const doc =
    deps.doc ?? request.elements[0]?.el.ownerDocument ?? (typeof document !== 'undefined' ? document : undefined);
  if (!doc) {
    return { captures: [], errors: ['Document is unavailable for capture'] };
  }
  const win = deps.win ?? doc.defaultView ?? (typeof window !== 'undefined' ? window : undefined);

  const styleEl = doc.createElement('style');
  styleEl.setAttribute(MASK_STYLE_ATTR, MASK_STYLE_VALUE);
  styleEl.textContent = buildMaskStyleCss(request.maskMode);
  const styleParent = doc.head ?? doc.documentElement;
  styleParent?.appendChild(styleEl);

  try {
    await (deps.waitForPaint ? deps.waitForPaint() : win ? defaultWaitForPaint(win) : Promise.resolve());

    let snapdomFn: SnapdomFn;
    try {
      snapdomFn = await (deps.loadSnapdom ?? defaultLoadSnapdom)();
    } catch (err) {
      return {
        captures: [],
        errors: [errorMessage(err, 'Failed to load capture engine')],
      };
    }

    const captures: CaptureArtifact[] = [];
    const errors: string[] = [];
    const exclude = [...FEEDBACK_EXCLUDE_SELECTORS];
    const backgroundColor = resolveOpaqueBackgroundColor(doc, win);
    let nextId = request.nextCaptureId ?? 1;

    for (const target of request.elements) {
      try {
        const result = await snapdomFn(target.el, {
          backgroundColor,
          exclude,
          excludeMode: 'remove',
          format: 'png',
        });
        const blob = await result.toBlob({ format: 'png' });
        const rect = target.el.getBoundingClientRect();
        captures.push({
          id: nextId++,
          kind: CaptureKind.Element,
          elementId: target.id,
          blob,
          width: resolveDimension(result.meta?.targetW ?? result.meta?.w0, rect.width),
          height: resolveDimension(result.meta?.targetH ?? result.meta?.h0, rect.height),
          maskMode: request.maskMode,
        });
      } catch (err) {
        errors.push(`Element capture failed: ${errorMessage(err, 'unknown error')}`);
      }
    }

    if (request.capturePage) {
      const pageRoot = doc.documentElement ?? doc.body;
      if (!pageRoot) {
        errors.push('Page capture failed: document root element is unavailable');
      } else {
        try {
          const result = await snapdomFn(pageRoot, {
            backgroundColor,
            exclude,
            excludeMode: 'remove',
            clip: 'viewport',
            format: 'png',
          });
          const blob = await result.toBlob({ format: 'png' });
          const fallbackWidth = win?.innerWidth ?? pageRoot.clientWidth;
          const fallbackHeight = win?.innerHeight ?? pageRoot.clientHeight;
          captures.push({
            id: nextId++,
            kind: CaptureKind.Page,
            elementId: null,
            blob,
            width: resolveDimension(result.meta?.targetW ?? result.meta?.w0, fallbackWidth),
            height: resolveDimension(result.meta?.targetH ?? result.meta?.h0, fallbackHeight),
            maskMode: request.maskMode,
          });
        } catch (err) {
          errors.push(`Page capture failed: ${errorMessage(err, 'unknown error')}`);
        }
      }
    }

    return { captures, errors };
  } finally {
    styleEl.remove();
  }
}

/** Triggers a browser download for a PNG `Blob`. */
export function downloadPngBlob(
  blob: Blob,
  filename: string,
  doc: Document | undefined = typeof document !== 'undefined' ? document : undefined,
): void {
  if (!doc || typeof URL === 'undefined' || typeof URL.createObjectURL !== 'function') {
    throw new Error('Download is unavailable in this environment');
  }
  const url = URL.createObjectURL(blob);
  const anchor = doc.createElement('a');
  anchor.href = url;
  anchor.download = filename;
  anchor.rel = 'noopener';
  anchor.style.display = 'none';
  const parent = doc.body ?? doc.documentElement;
  parent?.appendChild(anchor);
  try {
    anchor.click();
  } finally {
    anchor.remove();
    setTimeout(() => {
      if (typeof URL.revokeObjectURL === 'function') {
        URL.revokeObjectURL(url);
      }
    }, OBJECT_URL_REVOKE_DELAY_MS);
  }
}

function tryDownloadFallback(blob: Blob, doc?: Document): ClipboardOutcome {
  try {
    downloadPngBlob(blob, DEFAULT_PNG_FILENAME, doc);
    return ClipboardOutcome.Downloaded;
  } catch {
    return ClipboardOutcome.Failed;
  }
}

/**
 * Copies a PNG `Blob` to the clipboard, calling `navigator.clipboard.write()`
 * synchronously before any `await` so Safari preserves user-gesture activation.
 * Automatically falls back to downloading the PNG when clipboard access is
 * missing, insecure, or rejected.
 */
export async function copyPngToClipboard(
  blob: Blob,
  nav: Pick<Navigator, 'clipboard'> | undefined = typeof navigator !== 'undefined' ? navigator : undefined,
  isSecure: boolean = typeof window !== 'undefined' ? window.isSecureContext : false,
  doc?: Document,
): Promise<ClipboardOutcome> {
  if (!isSecure || !nav?.clipboard || typeof nav.clipboard.write !== 'function' || typeof ClipboardItem === 'undefined') {
    return tryDownloadFallback(blob, doc);
  }

  let writePromise: Promise<void>;
  try {
    const item = new ClipboardItem({ [PNG_MIME]: Promise.resolve(blob) });
    writePromise = nav.clipboard.write([item]);
  } catch {
    return tryDownloadFallback(blob, doc);
  }

  try {
    await writePromise;
    return ClipboardOutcome.Copied;
  } catch {
    return tryDownloadFallback(blob, doc);
  }
}
