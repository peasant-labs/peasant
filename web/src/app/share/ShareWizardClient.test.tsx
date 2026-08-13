import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ShareWizardClient } from '@/app/share/ShareWizardClient';
import * as useMockConfig from '@/hooks/useMockConfig';
import * as mockData from '@/lib/share/mock-data';

vi.mock('@/hooks/useMockConfig');
vi.mock('@/lib/share/mock-data');

// Four visible steps: Choose → Labels → Redact → Submit. RedactionStep is
// the heaviest leaf (simulated pipeline + diff views); stub it so the
// deep-link test can assert the wizard reached the Redact step without
// standing up that machinery.
vi.mock('@/components/share/RedactionStep', () => ({
  RedactionStep: ({ selectedIds }: { selectedIds: Set<string> }) => (
    <div
      data-testid="redaction-step"
      data-selected-ids={Array.from(selectedIds).join(',')}
    >
      Redaction step stub
    </div>
  ),
}));

// Mockable URLSearchParams returned by next/navigation's useSearchParams hook.
// Tests can mutate this via setSearchParams() before rendering.
let currentSearchParams = new URLSearchParams();
function setSearchParams(params: Record<string, string>) {
  currentSearchParams = new URLSearchParams(params);
}
vi.mock('next/navigation', () => ({
  useSearchParams: () => currentSearchParams,
}));

// The mounted hierarchy toolbar carries the running selection and token tally.
function gmsTally(): string {
  const el = document.querySelector('.gms-tally');
  return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('ShareWizardClient', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let mockConfigSpy: ReturnType<typeof vi.spyOn>;
  let fetchMockSessionsSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    mockConfigSpy = vi.spyOn(useMockConfig, 'useMockConfig');
    fetchMockSessionsSpy = vi.spyOn(mockData, 'fetchMockSessions');
    setSearchParams({});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetAllMocks();
  });

  it('shows loading state while config is loading', () => {
    mockConfigSpy.mockReturnValue({
      config: null,
      loading: true,
      error: null,
      refetch: vi.fn(),
    });

    render(<ShareWizardClient />);

    expect(screen.getByText('Loading sessions...')).toBeInTheDocument();
  });

  it('fetches mock sessions when mock is enabled for sessions', async () => {
    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMockSessionsSpy.mockReturnValue({
      sessions: [
        {
          id: 'test-1',
          provider: 'claude-code' as const,
          projectName: 'test-project',
          projectHash: 'ph-001',
          hostSlug: 'test-host',
          startTime: '2026-02-24T09:00:00Z',
          durationMins: 30,
          totalTokens: 10000,
          turnCount: 10,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
      ],
      counts: { new: 1, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 },
    });

    render(<ShareWizardClient />);

    await waitFor(() => {
      expect(fetchMockSessionsSpy).toHaveBeenCalled();
    });
    expect(fetch).not.toHaveBeenCalledWith(expect.stringContaining('/api/v1/sessions'));
  });

  it('fetches real sessions when mock is not enabled', async () => {
    const mockConfig = { enabled: false, web: [], tui: [] };
    const backendSessions = [
      {
        id: 'backend-1',
        harness: 'claude-code',
        startTime: '2026-02-24T09:00:00Z',
        durationMins: 30,
        totalTokens: 10000,
        turnCount: 10,
        toolCallCount: 5,
      },
    ];
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => backendSessions,
    });
    fetchMockSessionsSpy.mockReturnValue({
      sessions: [],
      counts: { new: 0, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 },
    });

    render(<ShareWizardClient />);

    await waitFor(() => {
      expect(fetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/sessions'),
      );
    });
    expect(fetchMockSessionsSpy).not.toHaveBeenCalled();
  });

  it('shows error UI when real fetch fails (no mock fallback)', async () => {
    const mockConfig = { enabled: false, web: [], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMock.mockRejectedValue(new Error('Network error'));

    render(<ShareWizardClient />);

    await waitFor(() => {
      expect(screen.getByText('Network error')).toBeInTheDocument();
    });
    expect(screen.getByText('Retry')).toBeInTheDocument();
    expect(fetchMockSessionsSpy).not.toHaveBeenCalled();
  });

  it('retries real fetch when retry button is clicked', async () => {
    const mockConfig = { enabled: false, web: [], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    // First fetch fails
    fetchMock.mockRejectedValueOnce(new Error('Network error'));

    render(<ShareWizardClient />);

    await waitFor(() => {
      expect(screen.getByText('Network error')).toBeInTheDocument();
    });

    // Set up success response for retry
    const backendSessions = [
      {
        id: 'retry-1',
        harness: 'claude-code',
        startTime: '2026-02-24T09:00:00Z',
        durationMins: 30,
        totalTokens: 10000,
        turnCount: 10,
        toolCallCount: 5,
      },
    ];
    fetchMock.mockResolvedValueOnce({
      ok: true,
      json: async () => backendSessions,
    });
    fetchMock.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ items: [{ sessionId: 'retry-1', locationLabel: 'workspace', repositoryLocationId: 'rl_workspace', branch: 'main', selectionStatus: 'selected' }] }),
    });

    const user = userEvent.setup();
    await user.click(screen.getByText('Retry'));

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1, name: 'Contribute' })).toBeInTheDocument();
    });
  });

  it('honors deep-link sessionId + legacy step query params', async () => {
    // Legacy step name "redact" maps onto the now-visible Redact step. The
    // deep-linked session is preselected (selection is non-empty), so the
    // Redact step is reachable.
    setSearchParams({ sessionId: 'deeplink-1', step: 'redact' });

    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMockSessionsSpy.mockReturnValue({
      sessions: [
        // A sibling in the same project that must NOT be auto-included when a
        // specific session is deep-linked.
        {
          id: 'deeplink-sibling',
          provider: 'claude-code' as const,
          projectName: 'deeplink-project',
          projectHash: 'ph-dl',
          hostSlug: 'test-host',
          startTime: '2026-02-23T09:00:00Z',
          durationMins: 20,
          totalTokens: 5000,
          turnCount: 5,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
        {
          id: 'deeplink-1',
          provider: 'claude-code' as const,
          projectName: 'deeplink-project',
          projectHash: 'ph-dl',
          hostSlug: 'test-host',
          startTime: '2026-02-24T09:00:00Z',
          durationMins: 30,
          totalTokens: 10000,
          turnCount: 10,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
      ],
      counts: { new: 2, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 },
    });

    render(<ShareWizardClient />);

    // Wizard jumps straight to the Redact step and the selection is *only*
    // the linked session — not its whole project.
    const stub = await screen.findByTestId('redaction-step');
    expect(stub).toHaveAttribute('data-selected-ids', 'deeplink-1');
    // The Choose step toolbar must NOT be rendered (the session-picker is
    // hidden because we jumped past it).
    expect(
      screen.queryByRole('button', { name: 'Select all' }),
    ).not.toBeInTheDocument();
  });

  it('renders the project-primary Choose step with NOTHING selected by default', async () => {
    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMockSessionsSpy.mockReturnValue({
      sessions: [
        {
          id: 'test-1',
          provider: 'claude-code' as const,
          projectName: 'alpha-project',
          projectHash: 'ph-001',
          hostSlug: 'test-host',
          startTime: '2026-02-24T09:00:00Z',
          durationMins: 30,
          totalTokens: 10000,
          turnCount: 10,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
        {
          id: 'test-2',
          provider: 'claude-code' as const,
          projectName: 'alpha-project',
          projectHash: 'ph-001',
          hostSlug: 'test-host',
          startTime: '2026-02-23T09:00:00Z',
          durationMins: 20,
          totalTokens: 4000,
          turnCount: 6,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
      ],
      counts: { new: 2, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 },
    });

    render(<ShareWizardClient />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1, name: 'Contribute' })).toBeInTheDocument();
    });
    // Selection is project-primary: the project name is shown directly in the
    // picker. The per-step "Projects" header was removed per UI feedback —
    // the StepIndicator carries that framing now.
    expect(screen.getByText('alpha-project')).toBeInTheDocument();
    // NOTHING is selected by default — the picker toolbar reads 0 selected.
    expect(gmsTally()).toContain('0 selected');
    // The page subtitle was removed per UI feedback — the title alone carries
    // the framing, no boundary/lock chip and no "Choose, label, redact, …"
    // sermon.
    expect(
      screen.queryByText(/Choose, label, redact/),
    ).not.toBeInTheDocument();
  });

  it('exposes a Select all / Deselect all affordance that toggles every selectable session', async () => {
    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMockSessionsSpy.mockReturnValue({
      sessions: [
        {
          id: 'sel-1',
          provider: 'claude-code' as const,
          projectName: 'alpha-project',
          projectHash: 'ph-001',
          hostSlug: 'test-host',
          startTime: '2026-02-24T09:00:00Z',
          durationMins: 30,
          totalTokens: 10000,
          turnCount: 10,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
        {
          id: 'sel-2',
          provider: 'claude-code' as const,
          projectName: 'beta-project',
          projectHash: 'ph-002',
          hostSlug: 'test-host',
          startTime: '2026-02-23T09:00:00Z',
          durationMins: 20,
          totalTokens: 4000,
          turnCount: 6,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
      ],
      counts: { new: 2, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 },
    });

    render(<ShareWizardClient />);

    const selectAll = await screen.findByRole('button', { name: 'select all' });
    // Starts empty.
    expect(gmsTally()).toContain('0 selected');

    const user = userEvent.setup();
    await user.click(selectAll);

    // Every selectable session across both projects is now selected.
    expect(gmsTally()).toContain('2 selected');
    expect(gmsTally()).toContain('14k');
    expect(selectAll).toHaveAttribute('aria-pressed', 'true');
    // The tri-state select-all toggles back off and clears the selection.
    await user.click(selectAll);
    expect(gmsTally()).toContain('0 selected');
    expect(selectAll).toHaveAttribute('aria-pressed', 'false');
  });

  it('filters the Choose list to the ?sessions= evidence set WITHOUT preselecting, and "Select these N sessions" selects them in one click', async () => {
    // Evidence-set entry: arriving from "Contribute sessions →" in
    // Review. Filtered, not preselected — the one click is the opt-in.
    setSearchParams({ sessions: 'ev-1,ev-2' });

    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMockSessionsSpy.mockReturnValue({
      sessions: [
        {
          id: 'ev-1',
          provider: 'claude-code' as const,
          projectName: 'alpha-project',
          projectHash: 'ph-001',
          hostSlug: 'test-host',
          startTime: '2026-02-24T09:00:00Z',
          durationMins: 30,
          totalTokens: 10000,
          turnCount: 10,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
        {
          id: 'ev-2',
          provider: 'claude-code' as const,
          projectName: 'beta-project',
          projectHash: 'ph-002',
          hostSlug: 'test-host',
          startTime: '2026-02-23T09:00:00Z',
          durationMins: 20,
          totalTokens: 4000,
          turnCount: 6,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
        // NOT in the evidence set — must be filtered out of the Choose list.
        {
          id: 'other-1',
          provider: 'claude-code' as const,
          projectName: 'gamma-project',
          projectHash: 'ph-003',
          hostSlug: 'test-host',
          startTime: '2026-02-22T09:00:00Z',
          durationMins: 10,
          totalTokens: 2000,
          turnCount: 4,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
      ],
      counts: { new: 3, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 },
    });

    render(<ShareWizardClient />);

    // The Choose list shows ONLY the evidence set's projects.
    expect(await screen.findByText('alpha-project')).toBeInTheDocument();
    expect(screen.getByText('beta-project')).toBeInTheDocument();
    expect(screen.queryByText('gamma-project')).not.toBeInTheDocument();

    // Filtered, NOT preselected — the picker toolbar still reads 0 selected.
    expect(gmsTally()).toContain('0 selected');

    // One click selects the whole evidence set.
    const user = userEvent.setup();
    await user.click(
      screen.getByRole('button', { name: 'Select these 2 sessions' }),
    );
    expect(gmsTally()).toContain('2 selected');
  });

  it('cold entry (no ?sessions=) lists everything and the evidence affordance is absent', async () => {
    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMockSessionsSpy.mockReturnValue({
      sessions: [
        {
          id: 'cold-1',
          provider: 'claude-code' as const,
          projectName: 'alpha-project',
          projectHash: 'ph-001',
          hostSlug: 'test-host',
          startTime: '2026-02-24T09:00:00Z',
          durationMins: 30,
          totalTokens: 10000,
          turnCount: 10,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
      ],
      counts: { new: 1, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 },
    });

    render(<ShareWizardClient />);

    expect(await screen.findByText('alpha-project')).toBeInTheDocument();
    expect(gmsTally()).toContain('0 selected');
    expect(
      screen.queryByRole('button', { name: /Select these \d+ sessions/ }),
    ).not.toBeInTheDocument();
  });

  it('shows all four visible steps in the indicator (choose / labels / redact / submit)', async () => {
    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMockSessionsSpy.mockReturnValue({
      sessions: [
        {
          id: 'steps-1',
          provider: 'claude-code' as const,
          projectName: 'alpha-project',
          projectHash: 'ph-001',
          hostSlug: 'test-host',
          startTime: '2026-02-24T09:00:00Z',
          durationMins: 30,
          totalTokens: 10000,
          turnCount: 10,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,          preview: 'Test preview message',
        },
      ],
      counts: { new: 1, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 },
    });

    render(<ShareWizardClient />);

    const nav = await screen.findByRole('navigation', {
      name: 'Contribute progress',
    });
    for (const label of ['choose', 'labels', 'redact', 'submit']) {
      expect(within(nav).getByText(label)).toBeInTheDocument();
    }
  });

  it('keeps the first-run tour anchor on the retained Contribute route', async () => {
    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    mockConfigSpy.mockReturnValue({
      config: mockConfig,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
    fetchMockSessionsSpy.mockReturnValue({
      sessions: [
        {
          id: 'tour-1',
          provider: 'claude-code' as const,
          projectName: 'alpha-project',
          projectHash: 'ph-001',
          hostSlug: 'test-host',
          startTime: '2026-02-24T09:00:00Z',
          durationMins: 30,
          totalTokens: 10000,
          turnCount: 10,
          model: 'claude-sonnet-4-6',
          shareStatus: 'new' as const,
          preview: 'Test preview message',
        },
      ],
      counts: { new: 1, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 },
    });

    render(<ShareWizardClient />);

    expect(await screen.findByText('alpha-project')).toBeInTheDocument();
    expect(document.querySelector('[data-tour="share-nav"]')).not.toBeNull();
  });
});
