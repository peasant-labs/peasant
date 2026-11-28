import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  captureFeedbackArtifacts,
  copyPngToClipboard,
  downloadPngBlob,
  resolveOpaqueBackgroundColor,
  type SnapdomCaptureOptions,
  type SnapdomFn,
} from './capture';
import { FEEDBACK_EXCLUDE_SELECTORS } from './masking';
import { CaptureKind, ClipboardOutcome, MaskMode } from './types';

function makeBox(width: number, height: number): DOMRect {
  return {
    top: 0,
    left: 0,
    width,
    height,
    right: width,
    bottom: height,
    x: 0,
    y: 0,
    toJSON: () => ({ width, height }),
  } as DOMRect;
}

describe('capture', () => {
  const originalCreateObjectURL = URL.createObjectURL;
  const originalRevokeObjectURL = URL.revokeObjectURL;
  const originalClipboardItem = globalThis.ClipboardItem;

  afterEach(() => {
    document.head.innerHTML = '';
    document.body.innerHTML = '';
    URL.createObjectURL = originalCreateObjectURL;
    URL.revokeObjectURL = originalRevokeObjectURL;
    globalThis.ClipboardItem = originalClipboardItem;
    vi.restoreAllMocks();
  });

  it('has no static import of @zumer/snapdom', () => {
    const source = readFileSync(resolve(process.cwd(), 'src/lib/feedback/capture.ts'), 'utf8');
    expect(source).not.toMatch(/^\s*import\s+[\s\S]*?from\s+['"]@zumer\/snapdom['"]/m);
    expect(source).toContain("import('@zumer/snapdom')");
  });

  it('lazy-snapdom-not-imported-until-capture', async () => {
    const loadSnapdom = vi.fn<() => Promise<SnapdomFn>>();
    const result = await captureFeedbackArtifacts(
      { elements: [], capturePage: false, maskMode: MaskMode.Masked },
      { loadSnapdom, waitForPaint: async () => {} },
    );

    expect(loadSnapdom).not.toHaveBeenCalled();
    expect(result).toEqual({ captures: [], errors: [] });
    expect(document.querySelector('style[data-feedback-widget="mask-style"]')).toBeNull();
  });

  it('captures elements and viewport-clipped page under mask style with monotonic ids then cleans up', async () => {
    const target1 = document.createElement('div');
    target1.className = 'card';
    target1.getBoundingClientRect = () => makeBox(320, 180);
    const target2 = document.createElement('button');
    target2.className = 'action';
    target2.getBoundingClientRect = () => makeBox(120, 40);
    document.body.append(target1, target2);

    const el1Blob = new Blob(['el1-png'], { type: 'image/png' });
    const el2Blob = new Blob(['el2-png'], { type: 'image/png' });
    const pageBlob = new Blob(['page-png'], { type: 'image/png' });
    const calls: Array<{ el: Element; options: SnapdomCaptureOptions | undefined; stylePresent: boolean; styleText: string }> = [];

    const snapdomMock: SnapdomFn = vi.fn(async (el, options) => {
      const styleEl = document.querySelector('style[data-feedback-widget="mask-style"]');
      calls.push({
        el,
        options,
        stylePresent: styleEl !== null,
        styleText: styleEl?.textContent ?? '',
      });
      if (el === document.documentElement) {
        return {
          meta: { w0: 1280, h0: 720, targetW: 1280, targetH: 720 },
          toBlob: async () => pageBlob,
        };
      }
      if (el === target2) {
        return {
          meta: { w0: 120, h0: 40, targetW: 120, targetH: 40 },
          toBlob: async () => el2Blob,
        };
      }
      return {
        meta: { w0: 320, h0: 180, targetW: 320, targetH: 180 },
        toBlob: async () => el1Blob,
      };
    });

    const result = await captureFeedbackArtifacts(
      {
        elements: [
          { id: 1, el: target1 },
          { id: 2, el: target2 },
        ],
        capturePage: true,
        maskMode: MaskMode.Masked,
        nextCaptureId: 10,
      },
      { loadSnapdom: async () => snapdomMock, waitForPaint: async () => {} },
    );

    expect(document.querySelector('style[data-feedback-widget="mask-style"]')).toBeNull();
    expect(result.errors).toEqual([]);
    expect(result.captures).toEqual([
      { id: 10, kind: CaptureKind.Element, elementId: 1, blob: el1Blob, width: 320, height: 180, maskMode: MaskMode.Masked },
      { id: 11, kind: CaptureKind.Element, elementId: 2, blob: el2Blob, width: 120, height: 40, maskMode: MaskMode.Masked },
      { id: 12, kind: CaptureKind.Page, elementId: null, blob: pageBlob, width: 1280, height: 720, maskMode: MaskMode.Masked },
    ]);

    expect(calls).toHaveLength(3);
    expect(calls[0].el).toBe(target1);
    expect(calls[0].stylePresent).toBe(true);
    expect(calls[0].styleText).toContain('[data-turn]');
    expect(calls[0].options?.exclude).toEqual([...FEEDBACK_EXCLUDE_SELECTORS]);
    expect(calls[0].options?.clip).toBeUndefined();

    expect(calls[1].el).toBe(target2);
    expect(calls[1].options?.clip).toBeUndefined();

    expect(calls[2].el).toBe(document.documentElement);
    expect(calls[2].stylePresent).toBe(true);
    expect(calls[2].options?.exclude).toEqual([...FEEDBACK_EXCLUDE_SELECTORS]);
    expect(calls[2].options?.clip).toBe('viewport');
  });

  it('capture-passes-opaque-background', async () => {
    const target = document.createElement('div');
    target.getBoundingClientRect = () => makeBox(120, 48);
    document.body.appendChild(target);

    const seenBackgrounds: Array<string | null | undefined> = [];
    const seenDprs: Array<number | undefined> = [];
    const snapdomMock: SnapdomFn = vi.fn(async (_el, options) => {
      seenBackgrounds.push(options?.backgroundColor);
      seenDprs.push(options?.dpr);
      return {
        meta: { w0: 120, h0: 48, targetW: 120, targetH: 48 },
        toBlob: async () => new Blob(['png'], { type: 'image/png' }),
      };
    });

    // 1) Prefers document.body computed backgroundColor when opaque.
    document.body.style.backgroundColor = 'rgb(15, 16, 18)';
    await captureFeedbackArtifacts(
      { elements: [{ id: 1, el: target }], capturePage: false, maskMode: MaskMode.Masked },
      { loadSnapdom: async () => snapdomMock, waitForPaint: async () => {} },
    );

    // 2) Falls back to html backgroundColor when body is transparent.
    document.body.style.backgroundColor = 'transparent';
    document.documentElement.style.backgroundColor = 'rgb(240, 240, 235)';
    await captureFeedbackArtifacts(
      { elements: [{ id: 1, el: target }], capturePage: false, maskMode: MaskMode.Masked },
      { loadSnapdom: async () => snapdomMock, waitForPaint: async () => {} },
    );

    // 3) Falls back to --canvas CSS variable when body and html are transparent.
    document.documentElement.style.backgroundColor = 'rgba(0, 0, 0, 0)';
    document.documentElement.style.setProperty('--canvas', '#141413');
    await captureFeedbackArtifacts(
      { elements: [{ id: 1, el: target }], capturePage: false, maskMode: MaskMode.Masked },
      { loadSnapdom: async () => snapdomMock, waitForPaint: async () => {} },
    );

    // 4) Never passes transparent even if body, html, and --canvas are unset.
    document.documentElement.style.removeProperty('--canvas');
    await captureFeedbackArtifacts(
      { elements: [{ id: 1, el: target }], capturePage: false, maskMode: MaskMode.Masked },
      { loadSnapdom: async () => snapdomMock, waitForPaint: async () => {} },
    );

    expect(seenBackgrounds).toEqual(['rgb(15, 16, 18)', 'rgb(240, 240, 235)', '#141413', '#0b0b0a']);
    // Element captures render at least 2x so small targets stay legible.
    expect(seenDprs.every((dpr) => dpr !== undefined && dpr >= 2)).toBe(true);
    for (const bg of seenBackgrounds) {
      expect(bg).toBeTruthy();
      expect(bg).not.toBe('transparent');
      expect(bg).not.toBe('rgba(0, 0, 0, 0)');
    }
    document.documentElement.dataset.theme = 'light';
    expect(resolveOpaqueBackgroundColor(document, window)).toBe('#fbfaf7');
    delete document.documentElement.dataset.theme;
  });

  it('mask-style-removed-even-on-capture-throw', async () => {
    const target = document.createElement('div');
    document.body.appendChild(target);

    const loadRejecting = vi.fn(async () => {
      expect(document.querySelector('style[data-feedback-widget="mask-style"]')).not.toBeNull();
      throw new Error('chunk load failed');
    });

    const loadFailure = await captureFeedbackArtifacts(
      { elements: [{ id: 1, el: target }], capturePage: false, maskMode: MaskMode.Masked },
      { loadSnapdom: loadRejecting, waitForPaint: async () => {} },
    );

    expect(document.querySelector('style[data-feedback-widget="mask-style"]')).toBeNull();
    expect(loadFailure.captures).toEqual([]);
    expect(loadFailure.errors).toEqual(['chunk load failed']);
  });

  it('collects per-capture errors without throwing and preserves partial captures', async () => {
    const target = document.createElement('div');
    document.body.appendChild(target);
    const pageBlob = new Blob(['page-png'], { type: 'image/png' });

    const snapdomMock: SnapdomFn = vi.fn(async (el) => {
      if (el === target) {
        throw new Error('element tainted');
      }
      return {
        meta: { w0: 1024, h0: 768, targetW: 1024, targetH: 768 },
        toBlob: async () => pageBlob,
      };
    });

    const result = await captureFeedbackArtifacts(
      { elements: [{ id: 1, el: target }], capturePage: true, maskMode: MaskMode.Unmasked },
      { loadSnapdom: async () => snapdomMock, waitForPaint: async () => {} },
    );

    expect(document.querySelector('style[data-feedback-widget="mask-style"]')).toBeNull();
    expect(result.captures).toEqual([
      { id: 1, kind: CaptureKind.Page, elementId: null, blob: pageBlob, width: 1024, height: 768, maskMode: MaskMode.Unmasked },
    ]);
    expect(result.errors).toEqual(['Element capture failed: element tainted']);
  });

  it('calls navigator.clipboard.write synchronously before any await', async () => {
    const blob = new Blob(['png-bytes'], { type: 'image/png' });
    const receivedItems: Array<Record<string, unknown>> = [];
    class FakeClipboardItem {
      readonly items: Record<string, unknown>;
      constructor(items: Record<string, unknown>) {
        this.items = items;
        receivedItems.push(items);
      }
    }
    globalThis.ClipboardItem = FakeClipboardItem as unknown as typeof ClipboardItem;

    let writeCalledSync = false;
    const write = vi.fn(async () => {
      writeCalledSync = true;
    });

    const outcomePromise = copyPngToClipboard(
      blob,
      { clipboard: { write } as unknown as Clipboard },
      true,
    );
    expect(writeCalledSync).toBe(true);
    expect(write).toHaveBeenCalledTimes(1);
    expect(receivedItems).toHaveLength(1);
    await expect(receivedItems[0]['image/png']).resolves.toBe(blob);
    await expect(outcomePromise).resolves.toBe(ClipboardOutcome.Copied);
  });

  it('clipboard-fallback-downloads-on-insecure-context', async () => {
    const blob = new Blob(['png-bytes'], { type: 'image/png' });
    URL.createObjectURL = vi.fn(() => 'blob:mock-url');
    URL.revokeObjectURL = vi.fn();
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    const write = vi.fn(async () => {});

    const outcome = await copyPngToClipboard(
      blob,
      { clipboard: { write } as unknown as Clipboard },
      false,
    );

    expect(write).not.toHaveBeenCalled();
    expect(clickSpy).toHaveBeenCalledTimes(1);
    expect(outcome).toBe(ClipboardOutcome.Downloaded);
  });

  it('falls back to download when clipboard.write rejects', async () => {
    const blob = new Blob(['png-bytes'], { type: 'image/png' });
    class FakeClipboardItem {
      constructor(public readonly items: Record<string, unknown>) {}
    }
    globalThis.ClipboardItem = FakeClipboardItem as unknown as typeof ClipboardItem;
    URL.createObjectURL = vi.fn(() => 'blob:mock-url');
    URL.revokeObjectURL = vi.fn();
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    const write = vi.fn(async () => {
      throw new DOMException('NotAllowedError');
    });

    const outcome = await copyPngToClipboard(
      blob,
      { clipboard: { write } as unknown as Clipboard },
      true,
    );

    expect(write).toHaveBeenCalledTimes(1);
    expect(clickSpy).toHaveBeenCalledTimes(1);
    expect(outcome).toBe(ClipboardOutcome.Downloaded);
  });

  it('returns Failed only when both clipboard and download fallback fail', async () => {
    const blob = new Blob(['png-bytes'], { type: 'image/png' });
    URL.createObjectURL = vi.fn(() => {
      throw new Error('createObjectURL unavailable');
    });

    const outcome = await copyPngToClipboard(blob, undefined, false);
    expect(outcome).toBe(ClipboardOutcome.Failed);
  });

  it('downloadPngBlob creates, clicks, and removes an anchor and revokes the object URL', () => {
    vi.useFakeTimers();
    try {
      const blob = new Blob(['png-bytes'], { type: 'image/png' });
      URL.createObjectURL = vi.fn(() => 'blob:shot-1');
      URL.revokeObjectURL = vi.fn();
      let downloadedAttr = '';
      let hrefAttr = '';
      const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
        downloadedAttr = this.download;
        hrefAttr = this.href;
      });

      downloadPngBlob(blob, 'feedback-shot.png', document);

      expect(clickSpy).toHaveBeenCalledTimes(1);
      expect(downloadedAttr).toBe('feedback-shot.png');
      expect(hrefAttr).toBe('blob:shot-1');
      expect(document.body.querySelector('a')).toBeNull();

      vi.advanceTimersByTime(1000);
      expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:shot-1');
    } finally {
      vi.useRealTimers();
    }
  });
});
