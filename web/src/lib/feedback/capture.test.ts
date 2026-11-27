import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  captureFeedbackArtifacts,
  copyPngToClipboard,
  downloadPngBlob,
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
      { element: null, capturePage: false, maskMode: MaskMode.Masked },
      { loadSnapdom, waitForPaint: async () => {} },
    );

    expect(loadSnapdom).not.toHaveBeenCalled();
    expect(result).toEqual({ captures: [], errors: [] });
    expect(document.querySelector('style[data-feedback-widget="mask-style"]')).toBeNull();
  });

  it('captures element and viewport-clipped page under mask style then cleans up', async () => {
    const target = document.createElement('div');
    target.className = 'card';
    target.getBoundingClientRect = () => makeBox(320, 180);
    document.body.appendChild(target);

    const elementBlob = new Blob(['el-png'], { type: 'image/png' });
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
      const isPage = el === document.documentElement;
      return {
        meta: isPage
          ? { w0: 1280, h0: 720, targetW: 1280, targetH: 720 }
          : { w0: 320, h0: 180, targetW: 320, targetH: 180 },
        toBlob: async () => (isPage ? pageBlob : elementBlob),
      };
    });

    const result = await captureFeedbackArtifacts(
      { element: target, capturePage: true, maskMode: MaskMode.Masked },
      { loadSnapdom: async () => snapdomMock, waitForPaint: async () => {} },
    );

    expect(document.querySelector('style[data-feedback-widget="mask-style"]')).toBeNull();
    expect(result.errors).toEqual([]);
    expect(result.captures).toEqual([
      { kind: CaptureKind.Element, blob: elementBlob, width: 320, height: 180, maskMode: MaskMode.Masked },
      { kind: CaptureKind.Page, blob: pageBlob, width: 1280, height: 720, maskMode: MaskMode.Masked },
    ]);

    expect(calls).toHaveLength(2);
    expect(calls[0].el).toBe(target);
    expect(calls[0].stylePresent).toBe(true);
    expect(calls[0].styleText).toContain('[data-turn]');
    expect(calls[0].options?.exclude).toEqual([...FEEDBACK_EXCLUDE_SELECTORS]);
    expect(calls[0].options?.clip).toBeUndefined();

    expect(calls[1].el).toBe(document.documentElement);
    expect(calls[1].stylePresent).toBe(true);
    expect(calls[1].options?.exclude).toEqual([...FEEDBACK_EXCLUDE_SELECTORS]);
    expect(calls[1].options?.clip).toBe('viewport');
  });

  it('mask-style-removed-even-on-capture-throw', async () => {
    const target = document.createElement('div');
    document.body.appendChild(target);

    const loadRejecting = vi.fn(async () => {
      expect(document.querySelector('style[data-feedback-widget="mask-style"]')).not.toBeNull();
      throw new Error('chunk load failed');
    });

    const loadFailure = await captureFeedbackArtifacts(
      { element: target, capturePage: false, maskMode: MaskMode.Masked },
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
      { element: target, capturePage: true, maskMode: MaskMode.Unmasked },
      { loadSnapdom: async () => snapdomMock, waitForPaint: async () => {} },
    );

    expect(document.querySelector('style[data-feedback-widget="mask-style"]')).toBeNull();
    expect(result.captures).toEqual([
      { kind: CaptureKind.Page, blob: pageBlob, width: 1024, height: 768, maskMode: MaskMode.Unmasked },
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
