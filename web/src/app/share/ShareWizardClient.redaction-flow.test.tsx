import { act } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ShareWizardClient } from '@/app/share/ShareWizardClient';
import type { Redaction } from '@/types/messages';
import * as redactionsApi from '@/lib/share/redactions';
import { DEFAULT_REDACTION_LEVEL } from '@/lib/share/redactions';
import {
  REDACTION_STEP_DISCOVERY_PAYLOAD,
  REDACTION_STEP_FAILURE_EXPECTATIONS,
  REDACTION_STEP_MATCH,
  REDACTION_STEP_SCAN_FAILURE,
  REDACTION_STEP_SESSION,
} from '@/test/fixtures/redaction-step';

vi.mock('@/hooks/useMockConfig', () => ({
  useMockConfig: () => ({
    config: { enabled: false, web: [], tui: [] },
    loading: false,
    error: null,
    refetch: vi.fn(),
  }),
}));

let currentSearchParams = new URLSearchParams();
vi.mock('next/navigation', () => ({
  useSearchParams: () => currentSearchParams,
}));

vi.mock('@/lib/share/redactions', async (importOriginal) => {
  const actual = await importOriginal<typeof redactionsApi>();
  return { ...actual, fetchRedactionPreview: vi.fn() };
});

const fetchPreview = vi.mocked(redactionsApi.fetchRedactionPreview);

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

describe('ShareWizardClient redaction flow', () => {
  beforeEach(() => {
    currentSearchParams = new URLSearchParams({
      sessionId: REDACTION_STEP_SESSION.id,
      step: 'redact',
    });
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith('/api/v1/web/discovery')) {
        return {
          ok: true,
          json: async () => ({ items: [{ sessionId: REDACTION_STEP_SESSION.id, locationLabel: 'workspace', repositoryLocationId: 'rl_workspace', branch: 'main', selectionStatus: 'selected' }] }),
        };
      }
      if (!url.endsWith('/api/v1/sessions')) {
        throw new Error(`unexpected fetch in mounted redaction-flow test: ${url}`);
      }
      return {
        ok: true,
        json: async () => REDACTION_STEP_DISCOVERY_PAYLOAD,
      };
    }));
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetAllMocks();
  });

  it('preserves an in-flight and completed scan across mounted wizard navigation', async () => {
    const scan = deferred<Redaction[]>();
    let scanSettled = false;
    const settleScan = (redactions: Redaction[]) => {
      if (scanSettled) return;
      scanSettled = true;
      act(() => {
        scan.resolve(redactions);
      });
    };
    fetchPreview.mockReturnValue(scan.promise);
    const user = userEvent.setup();

    try {
      render(<ShareWizardClient />);

      await waitFor(() => expect(fetchPreview).toHaveBeenCalledTimes(1));
      expect(fetchPreview).toHaveBeenCalledWith(REDACTION_STEP_SESSION.id, DEFAULT_REDACTION_LEVEL);
      expect(
        await screen.findByText('Scanning your work for sensitive content'),
      ).toBeInTheDocument();

      const rail = screen.getByRole('navigation', { name: 'Contribute progress' });
      await user.click(within(rail).getByRole('button', { name: /submit/i }));
      await user.click(screen.getByRole('button', { name: /back/i }));

      expect(
        await screen.findByText('Scanning your work for sensitive content'),
      ).toBeInTheDocument();
      expect(fetchPreview).toHaveBeenCalledTimes(1);

      settleScan([REDACTION_STEP_MATCH]);

      let review = await screen.findByRole('region', { name: 'redaction review' });
      expect(within(review).getByText(REDACTION_STEP_MATCH.originalText)).toBeInTheDocument();
      expect(
        within(review).getByText(REDACTION_STEP_MATCH.redactedReplacement),
      ).toBeInTheDocument();

      await user.click(within(rail).getByRole('button', { name: /submit/i }));
      await user.click(screen.getByRole('button', { name: /back/i }));

      review = await screen.findByRole('region', { name: 'redaction review' });
      expect(within(review).getByText(REDACTION_STEP_MATCH.originalText)).toBeInTheDocument();
      expect(
        within(review).getByText(REDACTION_STEP_MATCH.redactedReplacement),
      ).toBeInTheDocument();
      expect(fetchPreview).toHaveBeenCalledTimes(1);
    } finally {
      settleScan([]);
    }
  });

  it('preserves a failed scan across mounted wizard navigation without retrying', async () => {
    fetchPreview.mockRejectedValue(new Error(REDACTION_STEP_SCAN_FAILURE));
    const user = userEvent.setup();

    render(<ShareWizardClient />);

    expect(await screen.findByRole('alert')).toHaveTextContent(REDACTION_STEP_SCAN_FAILURE);
    expect(
      screen.getByRole('progressbar', {
        name: REDACTION_STEP_FAILURE_EXPECTATIONS.allFailure.scannedLabel,
      }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(REDACTION_STEP_FAILURE_EXPECTATIONS.forbiddenSafeCopy),
    ).not.toBeInTheDocument();
    expect(fetchPreview).toHaveBeenCalledTimes(1);
    expect(fetchPreview).toHaveBeenCalledWith(REDACTION_STEP_SESSION.id, DEFAULT_REDACTION_LEVEL);

    const rail = screen.getByRole('navigation', { name: 'Contribute progress' });
    await user.click(within(rail).getByRole('button', { name: /submit/i }));
    await user.click(screen.getByRole('button', { name: /back/i }));

    expect(await screen.findByRole('alert')).toHaveTextContent(REDACTION_STEP_SCAN_FAILURE);
    expect(
      screen.getByRole('progressbar', {
        name: REDACTION_STEP_FAILURE_EXPECTATIONS.allFailure.scannedLabel,
      }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(REDACTION_STEP_FAILURE_EXPECTATIONS.forbiddenSafeCopy),
    ).not.toBeInTheDocument();
    expect(fetchPreview).toHaveBeenCalledTimes(1);
  });

});
