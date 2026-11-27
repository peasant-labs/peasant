import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import {
  CaptureKind,
  ClipboardOutcome,
  FeedbackKind,
  MaskMode,
  type CaptureArtifact,
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

  it('completes keyboard and click flow without captures and opens prefilled problem issue URL without labels', async () => {
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

    fireEvent.change(screen.getByLabelText('title'), { target: { value: 'Sidebar button clipped' } });
    fireEvent.change(screen.getByLabelText('description (required)'), {
      target: { value: 'The bottom control clips at 800px height.' },
    });

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
    expect(parsed.searchParams.get('title')).toBe('Sidebar button clipped');
    expect(parsed.searchParams.get('description')).toBe('The bottom control clips at 800px height.');
    expect(parsed.searchParams.get('environment')).toContain('route: /sessions/test-123');
    expect(parsed.searchParams.get('environment')).toContain('peasant: 0.12.3');

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
      kind: CaptureKind.Page,
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
        element: null,
        capturePage: true,
        maskMode: MaskMode.Masked,
      });
    });

    expect(dialogsOpenAtCapture).toEqual([0]);

    const preview = await screen.findByRole('img', { name: /whole page capture/i });
    expect(preview).toHaveAttribute('src', 'blob:feedback-preview-1');

    fireEvent.change(screen.getByLabelText('description (required)'), {
      target: { value: 'Add keyboard shortcut hint on page.' },
    });
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

  it('renders nothing when ?fb=off is present in the query string', () => {
    window.history.replaceState({}, '', '/?fb=off');
    const { container } = render(<FeedbackWidget />);
    expect(container.querySelector('[data-feedback-widget]')).toBeNull();
    expect(screen.queryByRole('button', { name: 'feedback' })).toBeNull();
  });
});
