import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { providerDisplayName } from '@peasant-labs/fairtrade/ui';
import { Harness } from '@peasant-labs/schema';
import { parseTranscriptRouteQuery } from '@/lib/navigation/projectRoutes';
import { strikeMountedWebFixture } from '@/test/fixtures/strikeMountedWeb';
import { SessionDetailV2 } from './SessionDetailV2';

const routerReplace = vi.hoisted(() => vi.fn());

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => `/projects/${strikeMountedWebFixture.projectHash}/${strikeMountedWebFixture.sessionDetail.id}`,
  useRouter: () => ({ replace: routerReplace }),
}));

vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (subscription: { topic: string }) => ({
    data: subscription.topic === 'session_detail'
      ? strikeMountedWebFixture.sessionDetail
      : { sessions: null },
    connected: true,
    error: null,
  }),
}));

vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));

vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));

function StrikeDetail() {
  const routeQuery = parseTranscriptRouteQuery(new URLSearchParams());
  if (!routeQuery) throw new Error('mounted Strike transcript fixture route query must be valid');
  return (
    <SessionDetailV2
      sessionId={strikeMountedWebFixture.sessionDetail.id}
      projectHash={strikeMountedWebFixture.projectHash}
      projectName={strikeMountedWebFixture.projectName}
      routeQuery={routeQuery}
    />
  );
}

describe('mounted Strike transcript', () => {
  beforeEach(() => {
    routerReplace.mockReset();
    window.localStorage.clear();
    if (typeof HTMLElement.prototype.scrollIntoView !== 'function') {
      Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
        configurable: true,
        value: () => undefined,
      });
    }
    vi.stubGlobal('ResizeObserver', class ResizeObserver {
      observe() {}
      unobserve() {}
      disconnect() {}
    });
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('renders the canonical payload through SessionDetailV2 and the real TranscriptViewer', async () => {
    const user = userEvent.setup();
    const view = render(<StrikeDetail />);
    await waitFor(() => expect(view.container.querySelector('.txn-app')).toBeInTheDocument());

    const providerChip = [...view.container.querySelectorAll('.txn-meta .chip')].find(
      (chip) => chip.textContent?.includes(Harness.Strike),
    );
    expect(providerChip).toBeDefined();
    expect(providerChip?.querySelector('.brand')).toBeInTheDocument();

    expect(screen.getByRole('tab', { name: /^full trace/i })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    const assistantContent = await screen.findByText(
      strikeMountedWebFixture.expected.assistantContent,
    );
    const assistantTurn = assistantContent.closest('.txn-turnwrap');
    const roleLabel = assistantTurn?.querySelector('.txn-rolelabel');
    const canonicalDisplayName = providerDisplayName(strikeMountedWebFixture.sessionDetail.harness);

    expect(canonicalDisplayName).toBeTruthy();
    expect(assistantTurn).toHaveAttribute('data-turn', '1');
    expect(roleLabel).toHaveTextContent('assistant');
    expect(roleLabel?.querySelector('.brand')).toBeInTheDocument();

    const graphToggle = screen.getByRole('button', { name: /^graph$/i });
    expect(graphToggle).toHaveAttribute('aria-pressed', 'false');
    await user.click(graphToggle);
    expect(graphToggle).toHaveAttribute('aria-pressed', 'true');

    const graph = await waitFor(() => {
      const mounted = view.container.querySelector('.tb-graph');
      if (!mounted) throw new Error('real fairtrade/graph did not mount in SessionDetailV2.strike.test.tsx after the user selected graph mode; the mounted transcript is not exercising the production graph path; verify graphSlot still wires TrajectoryGraph and the packed fairtrade artifact exports its graph engine');
      return mounted;
    });
    const assistantGraphNode = graph.querySelector('[data-harness="strike"] .ft-gnode');
    expect(assistantGraphNode).toBeInTheDocument();
    expect(assistantGraphNode).toHaveStyle({ '--ft-gnode-accent': 'var(--clay)' });

    const strikeLegend = [...graph.querySelectorAll('.ft-graph-legend-item')].find(
      (item) => item.textContent?.trim() === canonicalDisplayName.toLocaleLowerCase('en-US'),
    );
    expect(strikeLegend?.querySelector('.ft-graph-legend-glyph')).toHaveStyle({
      background: 'var(--clay)',
    });
    expect(providerChip?.querySelector('.brand[data-brand="strike"]')).toBeInTheDocument();
  });
});
