import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import {
  CaptureKind,
  ClipboardOutcome,
  FeedbackKind,
  MaskMode,
  type CaptureArtifact,
  type CaptureRequest,
} from '@/lib/feedback/types';

const captureMocks = vi.hoisted(() => ({
  captureFeedbackArtifacts: vi.fn(),
  copyPngToClipboard: vi.fn(),
  downloadPngBlob: vi.fn(),
}));

const wsState = vi.hoisted(() => ({
  serverVersion: '0.12.3' as string | null,
}));

vi.mock('@/lib/feedback/capture', () => captureMocks);
vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useConnectionState: () => ({
    connected: true,
    serverVersion: wsState.serverVersion,
    connectionFault: null,
    reconnect: vi.fn(),
  }),
}));

import { FeedbackWidget } from './FeedbackWidget';

describe('FeedbackWidget', () => {
  const originalCreateObjectURL = URL.createObjectURL;
  const originalRevokeObjectURL = URL.revokeObjectURL;
  const originalOpen = window.open;
  let nextUrlId = 0;

  beforeEach(() => {
    nextUrlId = 0;
    wsState.serverVersion = '0.12.3';
    window.history.replaceState({}, '', '/sessions/test-123?tab=overview');

    vi.spyOn(window, 'requestAnimationFrame').mockImplementation((cb) => {
      cb(0);
      return 1;
    });
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(() => {});

    URL.createObjectURL = vi.fn(() => `blob:feedback-preview-${++nextUrlId}`) as unknown as typeof URL.createObjectURL;
    URL.revokeObjectURL = vi.fn();

    captureMocks.captureFeedbackArtifacts.mockReset();
    captureMocks.copyPngToClipboard.mockReset();
    captureMocks.downloadPngBlob.mockReset();
  });

  afterEach(() => {
    cleanup();
    document.body.innerHTML = '';
    window.history.replaceState({}, '', '/');
    URL.createObjectURL = originalCreateObjectURL;
    URL.revokeObjectURL = originalRevokeObjectURL;
    window.open = originalOpen;
    vi.restoreAllMocks();
  });

  it('completes keyboard and click flow without captures and opens prefilled problem issue URL with version and template preview', async () => {
    const openSpy = vi.fn(() => null);
    window.open = openSpy as unknown as typeof window.open;

    const { container } = render(<FeedbackWidget />);
    expect(container.querySelector('[data-feedback-widget]')).not.toBeNull();

    const launcher = screen.getByRole('button', { name: 'feedback' });
    launcher.focus();
    fireEvent.keyDown(launcher, { key: 'ArrowDown' });

    const problemItem = screen.getByRole('menuitem', { name: FeedbackKind.Problem.label });
    expect(document.activeElement).toBe(problemItem);
    fireEvent.keyDown(problemItem, { key: 'Enter' });

    expect(screen.getByRole('dialog', { name: FeedbackKind.Problem.label })).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'pick an element on the page' })).not.toBeChecked();
    expect(screen.getByRole('checkbox', { name: 'capture the visible page' })).not.toBeChecked();

    fireEvent.click(screen.getByRole('button', { name: 'continue' }));
    expect(captureMocks.captureFeedbackArtifacts).not.toHaveBeenCalled();

    expect(
      screen.getByText('You will write the description on GitHub. The issue opens with this template filled in:'),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText(/description/i)).toBeNull();
    const preview = screen.getByLabelText('issue template preview');
    expect(preview.textContent).toContain('peasant version: 0.12.3');

    fireEvent.change(screen.getByLabelText('title'), { target: { value: 'Sidebar button clipped' } });
    fireEvent.click(screen.getByRole('button', { name: 'open github issue' }));

    expect(captureMocks.copyPngToClipboard).not.toHaveBeenCalled();
    expect(openSpy).toHaveBeenCalledTimes(1);
    const [openedUrl, target, features] = openSpy.mock.calls[0] as unknown as [string, string, string];
    expect(target).toBe('_blank');
    expect(features).toBe('noopener,noreferrer');

    const parsed = new URL(openedUrl);
    expect(parsed.searchParams.get('template')).toBe('in-app-problem.yml');
    expect(parsed.searchParams.has('labels')).toBe(false);
    expect(parsed.searchParams.has('labels[]')).toBe(false);
    expect(parsed.searchParams.has('description')).toBe(false);
    expect(parsed.searchParams.get('title')).toBe('Sidebar button clipped');
    expect(parsed.searchParams.get('environment')?.startsWith('peasant version: 0.12.3\n')).toBe(true);
    expect(parsed.searchParams.get('environment')).toContain('route: /sessions/test-123');
    expect(parsed.searchParams.get('body')).toContain('### What happened?\n\n<!-- Describe it here. -->');
    expect(parsed.searchParams.get('body')).toContain('peasant version: 0.12.3');

    // Still transitions to Submitted even when window.open returns null (popup blocked).
    expect(screen.getByRole('dialog', { name: 'github issue opened' })).toBeInTheDocument();
    expect(screen.getByText(ClipboardOutcome.NotNeeded.label)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'open github issue' })).toHaveAttribute('href', openedUrl);

    fireEvent.click(screen.getByRole('button', { name: 'done' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(launcher);
  });

  it('captures whole page, renders preview, copies synchronously before window.open, and supports submitted recovery actions', async () => {
    const pageBlob = new Blob(['page-png'], { type: 'image/png' });
    const pageArtifact: CaptureArtifact = {
      id: 1,
      kind: CaptureKind.Page,
      elementId: null,
      blob: pageBlob,
      width: 1280,
      height: 720,
      maskMode: MaskMode.Masked,
    };
    // The page shot must not contain the widget's own dialogs or their scrim.
    const dialogsOpenAtCapture: number[] = [];
    captureMocks.captureFeedbackArtifacts.mockImplementation(async () => {
      dialogsOpenAtCapture.push(document.querySelectorAll('[role="dialog"]').length);
      return { captures: [pageArtifact], errors: [] };
    });

    const callOrder: string[] = [];
    let resolveClipboard!: (outcome: ClipboardOutcome) => void;
    captureMocks.copyPngToClipboard.mockImplementation(() => {
      callOrder.push('copy');
      return new Promise<ClipboardOutcome>((resolve) => {
        resolveClipboard = resolve;
      });
    });

    const openSpy = vi.fn(() => {
      callOrder.push('open');
      return {} as Window;
    });
    window.open = openSpy as unknown as typeof window.open;

    render(<FeedbackWidget />);

    fireEvent.click(screen.getByRole('button', { name: 'feedback' }));
    fireEvent.click(screen.getByRole('menuitem', { name: FeedbackKind.Feature.label }));
    fireEvent.click(screen.getByRole('checkbox', { name: 'capture the visible page' }));
    fireEvent.click(screen.getByRole('button', { name: 'continue' }));

    await waitFor(() => {
      expect(captureMocks.captureFeedbackArtifacts).toHaveBeenCalledWith({
        elements: [],
        capturePage: true,
        maskMode: MaskMode.Masked,
        nextCaptureId: 1,
      });
    });

    expect(dialogsOpenAtCapture).toEqual([0]);

    const preview = await screen.findByRole('img', { name: /image 1: whole page capture/i });
    expect(preview).toHaveAttribute('src', 'blob:feedback-preview-1');

    fireEvent.click(screen.getByRole('button', { name: 'open github issue' }));

    // copyPngToClipboard starts and window.open runs synchronously before clipboard settles.
    expect(callOrder).toEqual(['copy', 'open']);
    expect(captureMocks.copyPngToClipboard).toHaveBeenCalledWith(pageBlob);
    expect(openSpy).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolveClipboard(ClipboardOutcome.Copied);
    });

    expect(await screen.findByRole('dialog', { name: 'github issue opened' })).toBeInTheDocument();
    expect(screen.getByText(ClipboardOutcome.Copied.label)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /download/i }));
    expect(captureMocks.downloadPngBlob).toHaveBeenCalledWith(pageBlob, 'peasant-feedback-feature-page.png');
  });

  it('picks multiple elements in order, captures only new pick, re-captures all on unmask, and keeps numbers consistent after removal', async () => {
    const firstEl = document.createElement('button');
    firstEl.id = 'first-btn';
    firstEl.textContent = 'First';
    const secondEl = document.createElement('button');
    secondEl.id = 'second-btn';
    secondEl.textContent = 'Second';
    document.body.append(firstEl, secondEl);

    const el1Blob = new Blob(['el1'], { type: 'image/png' });
    const el2Blob = new Blob(['el2'], { type: 'image/png' });
    const pageBlob = new Blob(['page'], { type: 'image/png' });

    const requests: CaptureRequest[] = [];
    captureMocks.captureFeedbackArtifacts.mockImplementation(async (req: CaptureRequest) => {
      requests.push(req);
      let nextId = req.nextCaptureId ?? 1;
      const captures: CaptureArtifact[] = req.elements.map((target) => ({
        id: nextId++,
        kind: CaptureKind.Element,
        elementId: target.id,
        blob: target.el === firstEl ? el1Blob : el2Blob,
        width: 100,
        height: 40,
        maskMode: req.maskMode,
      }));
      if (req.capturePage) {
        captures.push({
          id: nextId++,
          kind: CaptureKind.Page,
          elementId: null,
          blob: pageBlob,
          width: 1280,
          height: 720,
          maskMode: req.maskMode,
        });
      }
      return { captures, errors: [] };
    });

    render(<FeedbackWidget />);

    fireEvent.click(screen.getByRole('button', { name: 'feedback' }));
    fireEvent.click(screen.getByRole('menuitem', { name: FeedbackKind.Problem.label }));
    fireEvent.click(screen.getByRole('checkbox', { name: 'pick an element on the page' }));
    fireEvent.click(screen.getByRole('checkbox', { name: 'capture the visible page' }));
    fireEvent.click(screen.getByRole('button', { name: 'continue' }));

    fireEvent.click(firstEl);
    await screen.findByRole('img', { name: /image 1: picked element capture/i });
    expect(screen.getByRole('img', { name: /image 2: whole page capture/i })).toBeInTheDocument();
    expect(requests[0].elements).toEqual([{ id: 1, el: firstEl }]);
    expect(requests[0].capturePage).toBe(true);

    // Pick a second element: captures ONLY element 2 and inserts it before page.
    fireEvent.click(screen.getByRole('button', { name: 'pick another element' }));
    fireEvent.click(secondEl);

    await screen.findByRole('img', { name: /image 2: picked element capture/i });
    expect(screen.getByRole('img', { name: /image 3: whole page capture/i })).toBeInTheDocument();
    expect(requests[1].elements).toEqual([{ id: 2, el: secondEl }]);
    expect(requests[1].capturePage).toBe(false);

    // Unmask triggers re-capture of ALL remaining elements + page.
    fireEvent.click(screen.getByRole('button', { name: 'unmask transcript' }));
    fireEvent.click(screen.getByRole('button', { name: 'confirm unmask' }));
    await waitFor(() => expect(requests).toHaveLength(3));
    expect(requests[2].elements).toEqual([
      { id: 1, el: firstEl },
      { id: 2, el: secondEl },
    ]);
    expect(requests[2].capturePage).toBe(true);
    expect(requests[2].maskMode).toBe(MaskMode.Unmasked);

    // Removing image 1 removes element 1 and shifts remaining images to 1 & 2.
    const removeFirstBtn = await screen.findByRole('button', { name: /remove picked element capture \(image 1\)/i });
    fireEvent.click(removeFirstBtn);
    const templatePreview = screen.getByLabelText('issue template preview');
    expect(templatePreview.textContent).not.toContain('button#first-btn');
    expect(templatePreview.textContent).toContain('element 1 (image 1): button#second-btn');
    expect(screen.getByRole('img', { name: /image 1: picked element capture/i })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: /image 2: whole page capture/i })).toBeInTheDocument();

    // Submit copies image 1 automatically, keeps previews on Submitted, and updates copy marker on image 2 copy.
    captureMocks.copyPngToClipboard.mockResolvedValue(ClipboardOutcome.Copied);
    const openSpy = vi.fn(() => ({}) as Window);
    window.open = openSpy as unknown as typeof window.open;

    fireEvent.click(screen.getByRole('button', { name: 'open github issue' }));

    expect(captureMocks.copyPngToClipboard).toHaveBeenCalledWith(el2Blob);
    expect(await screen.findByText('image 1 copied — paste it, then copy the next')).toBeInTheDocument();
    expect(screen.getAllByRole('img')).toHaveLength(2);

    fireEvent.click(screen.getByRole('button', { name: /copy image 2/i }));
    expect(captureMocks.copyPngToClipboard).toHaveBeenLastCalledWith(pageBlob);
    expect(await screen.findByText('image 2 copied — paste it, then copy the next')).toBeInTheDocument();

    const revokeCountBeforeClose = (URL.revokeObjectURL as ReturnType<typeof vi.fn>).mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: 'done' }));
    expect((URL.revokeObjectURL as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(revokeCountBeforeClose);
  });

  it('renders nothing when ?fb=off is present in the query string', () => {
    window.history.replaceState({}, '', '/?fb=off');
    const { container } = render(<FeedbackWidget />);
    expect(container.querySelector('[data-feedback-widget]')).toBeNull();
    expect(screen.queryByRole('button', { name: 'feedback' })).toBeNull();
  });
});
