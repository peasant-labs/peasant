import { act, useCallback, useEffect, type ComponentProps } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ShareWizardClient } from '@/app/share/ShareWizardClient';
import { PublishProvider, usePublishState } from '@/contexts/PublishContext';
import type { RedactionStep } from '@/components/share/RedactionStep';
import type { ShareFooterActions } from '@/components/share/footer-actions';
import * as reviewAdapter from '@/lib/share/reviewMatches';
import * as mockData from '@/lib/share/mock-data';
import { buildGroupedSyncResponse } from '@/app/share/testdata/grouped-sync';
import type { Redaction } from '@/types/messages';
import * as redactionsApi from '@/lib/share/redactions';
import { DEFAULT_REDACTION_LEVEL } from '@/lib/share/redactions';
import { boundaryDomain, boundaryResponse, REDACTION_REVIEW_BOUNDARIES } from '@/test/fixtures/redaction-review-boundaries';
import {
  REDACTION_STEP_FAILURE_EXPECTATIONS,
  REDACTION_STEP_MATCH,
  REDACTION_STEP_SCAN_FAILURE,
  REDACTION_STEP_SESSION,
} from '@/test/fixtures/redaction-step';

let mockMode = false;
const REAL_CONFIG = { enabled: false, web: [], tui: [] };
const MOCK_CONFIG = { enabled: true, web: ['sessions'], tui: [] };
let registeredFooter: ShareFooterActions | null = null;
vi.mock('@/components/share/RedactionStep', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/components/share/RedactionStep')>();
  return { ...actual, RedactionStep: (props: ComponentProps<typeof RedactionStep>) => {
    const register = useCallback((actions: ShareFooterActions | null) => {
      registeredFooter = actions;
      props.onFooterActionsChange(actions);
    }, [props.onFooterActionsChange]);
    return <actual.RedactionStep {...props} onFooterActionsChange={register} />;
  } };
});
vi.mock('@/hooks/useMockConfig', () => ({
  useMockConfig: () => ({
    config: mockMode ? MOCK_CONFIG : REAL_CONFIG,
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
  const fetchRedactionPreview = vi.fn();
  return { ...actual, fetchRedactionPreview, fetchRedactionScan: async (id: string, level: redactionsApi.RedactionLevel) => {
    const redactions = await fetchRedactionPreview(id, level);
    return { redactions, matchCount: redactions.length };
  } };
});

const fetchPreview = vi.mocked(redactionsApi.fetchRedactionPreview);

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

function SeedScan({ mutation }: { mutation: Parameters<typeof boundaryDomain>[0] }) {
  const { updateRedactionCache } = usePublishState();
  useEffect(() => updateRedactionCache((cache) => new Map(cache).set(`standard:${REDACTION_STEP_SESSION.id}`, {
    status: 'success', redactions: boundaryDomain(mutation),
  })), [mutation, updateRedactionCache]);
  return null;
}

function realContinue() {
  const footer = document.querySelector('.swz-foot');
  if (!(footer instanceof HTMLElement)) throw new Error('production wizard footer is missing');
  return within(footer).getByRole('button', { name: 'Continue' });
}

describe('ShareWizardClient redaction flow', () => {
  beforeEach(() => {
    mockMode = false;
    registeredFooter = null;
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
      if (!url.includes('/api/v1/sync/sessions')) {
        throw new Error(`unexpected fetch in mounted redaction-flow test: ${url}`);
      }
      return {
        ok: true,
        json: async () => buildGroupedSyncResponse([
          {
            id: REDACTION_STEP_SESSION.id,
            harness: REDACTION_STEP_SESSION.provider,
            startTime: REDACTION_STEP_SESSION.startTime,
            durationMins: REDACTION_STEP_SESSION.durationMins,
            totalTokens: REDACTION_STEP_SESSION.totalTokens,
            turnCount: REDACTION_STEP_SESSION.turnCount,
            project: REDACTION_STEP_SESSION.projectName,
            projectHash: REDACTION_STEP_SESSION.projectHash,
            preview: REDACTION_STEP_SESSION.preview,
          },
        ]),
      };
    }));
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    vi.resetAllMocks();
  });

  for (const entry of REDACTION_REVIEW_BOUNDARIES) {
    it(`validates the real scan boundary on the mounted wizard: ${entry.name}`, async () => {
      const actual = await vi.importActual<typeof redactionsApi>('@/lib/share/redactions');
      fetchPreview.mockImplementation((id, level) => actual.fetchRedactionPreview(id, level));
      const otherFetch = globalThis.fetch;
      let response = boundaryResponse(entry.source === 'adapter' ? 'none' : entry.mutation);
      let scanGate: Promise<void> | undefined;
      const originalAdapter = reviewAdapter.reviewMatches;
      const adapter = entry.source === 'adapter' ? vi.spyOn(reviewAdapter, 'reviewMatches').mockImplementation((id, redactions) => {
        if (entry.mutation === 'none') throw new TypeError('injected conversion rejection');
        const matches = originalAdapter(id, redactions);
        if (matches[0]) Object.assign(matches[0], { category: 'LOCATION' });
        return matches;
      }) : null;
      mockMode = entry.source === 'mock';
      const mock = vi.spyOn(mockData, 'fetchMockRedactionPreview').mockReturnValue(boundaryDomain(entry.mutation).map((item) => ({ ...item, minimumLevel: entry.minimumLevel ?? 'standard' })));
      const discovered = mockData.fetchMockSessions();
      vi.spyOn(mockData, 'fetchMockSessions').mockReturnValue({ ...discovered, sessions: [REDACTION_STEP_SESSION] });
      const pushes: unknown[] = [];
      vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url.includes('/sync/redactions')) { await scanGate; return Response.json(response); }
        if (url.includes('/sync/push')) { pushes.push(init?.body); return Response.json({}); }
        return otherFetch(input, init);
      }));
      const user = userEvent.setup();
      render(<PublishProvider>{entry.source === 'cache' && <SeedScan mutation={entry.mutation} />}<ShareWizardClient /></PublishProvider>);
      if (entry.error) {
        expect(await screen.findByRole('alert')).toHaveTextContent(entry.error);
        const action = realContinue();
        expect(action).toBeDisabled();
        await user.click(action);
        expect(registeredFooter?.primary.disabled).toBe(true);
        expect(registeredFooter?.primary.onClick).toBeTypeOf('function');
        act(() => registeredFooter!.primary.onClick());
        expect(screen.getByRole('region', { name: 'redaction review' })).toBeInTheDocument();
        expect(pushes).toEqual([]);
        if (entry.source === 'cache') expect(fetchPreview).not.toHaveBeenCalled();
        response = boundaryResponse();
        adapter?.mockRestore();
        mock.mockReturnValue(boundaryDomain('none').map((item) => ({ ...item, minimumLevel: 'standard' })));
        await user.click(await screen.findByRole('button', { name: 'Re-scan' }));
      }
      await waitFor(() => expect(realContinue()).toBeEnabled());
      const review = screen.getByRole('region', { name: 'redaction review' });
      for (const item of boundaryDomain('none')) {
        const row = within(review).getByText(item.originalText).closest('.rdx-card');
        expect(row).not.toBeNull();
        expect(row!.querySelector('.rdx-badge-cat')?.textContent).toBe(item.category);
      }
      await user.click(within(review).getAllByRole('button', { name: 'keep' })[0]);
      expect(within(review).getByText('will be sent')).toBeInTheDocument();
      const staleContinue = registeredFooter!.primary.onClick;
      const rescan = registeredFooter!.secondary!.onClick;
      const gate = deferred<void>();
      scanGate = gate.promise;
      response = boundaryResponse('unknown-item');
      mock.mockReturnValue(boundaryDomain('unknown-item').map((item) => ({ ...item, minimumLevel: 'standard' })));
      act(() => { rescan(); staleContinue(); });
      expect(screen.getByText('Scanning your work for sensitive content')).toBeInTheDocument();
      expect(realContinue()).toBeDisabled();
      expect(screen.queryByRole('button', { name: 'Submit' })).not.toBeInTheDocument();
      await act(async () => gate.resolve());
      expect(await screen.findByRole('alert')).toHaveTextContent('is not recognized');
      act(() => staleContinue());
      expect(realContinue()).toBeDisabled();
      expect(screen.queryByRole('button', { name: 'Submit' })).not.toBeInTheDocument();
      response = boundaryResponse();
      scanGate = undefined;
      mock.mockReturnValue(boundaryDomain('none').map((item) => ({ ...item, minimumLevel: 'standard' })));
      await user.click(screen.getByRole('button', { name: 'Re-scan' }));
      await waitFor(() => expect(realContinue()).toBeEnabled());
      await user.click(realContinue());
      expect(await screen.findByRole('button', { name: 'Submit' })).toBeEnabled();
      expect(screen.queryByRole('region', { name: 'redaction review' })).not.toBeInTheDocument();
      // Review is local: even a valid Continue does not consent to a push.
      expect(pushes).toEqual([]);
    }, 15000);
  }

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
      render(<PublishProvider><ShareWizardClient /></PublishProvider>);

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
      expect(within(review).queryByRole('group', { name: 'redaction level' })).not.toBeInTheDocument();
      expect(screen.queryByText('minimal', { exact: true })).not.toBeInTheDocument();
      expect(screen.queryByText('maximum', { exact: true })).not.toBeInTheDocument();
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

    render(<PublishProvider><ShareWizardClient /></PublishProvider>);

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
    const footer = document.querySelector('.swz-foot') as HTMLElement;
    expect(within(footer).getByRole('button', { name: 'Continue' })).toBeDisabled();
    expect(screen.getAllByRole('button', { name: 'Continue' })).toHaveLength(1);

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
