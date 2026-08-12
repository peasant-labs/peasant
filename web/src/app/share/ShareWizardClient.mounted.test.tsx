import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import YAML from 'yaml';
import fixtureSource from './testdata/mounted-share.yaml?raw';
import { ShareWizardClient } from './ShareWizardClient';
import * as useMockConfig from '@/hooks/useMockConfig';

vi.mock('@/hooks/useMockConfig');
vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams() }));
vi.mock('@/components/share/LabelsStep', () => ({ LabelsStep: ({ onNext }: { onNext: () => void }) => <button onClick={onNext}>continue labels</button> }));
vi.mock('@/components/share/RedactionStep', () => ({ RedactionStep: ({ onNext }: { onNext: () => void }) => <button onClick={onNext}>continue redaction</button> }));

type SessionRow = Record<'id' | 'harness' | 'project' | 'startTime' | 'preview' | 'shareStatus', string> & Record<'durationMins' | 'totalTokens' | 'turnCount' | 'toolCallCount', number>;
type DiscoveryRow = Record<'sessionId' | 'locationLabel' | 'repositoryLocationId' | 'branch' | 'selectionStatus', string>;
interface InvalidCase { name: string; operation: 'remove' | 'duplicate' | 'malformed' | 'unknown-status'; sessionId: string }
interface Fixture { sessions: SessionRow[]; items: DiscoveryRow[]; invalidCases: InvalidCase[] }

function loadFixture(): Fixture {
  const parsed: unknown = YAML.parse(fixtureSource);
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('mounted Share fixture must be an object');
  const root = parsed as Record<string, unknown>;
  if (Object.keys(root).sort().join(',') !== 'invalidCases,items,sessions' || !Array.isArray(root.sessions) || root.sessions.length !== 6 || !Array.isArray(root.items) || root.items.length !== 7 || !Array.isArray(root.invalidCases) || root.invalidCases.length !== 4) throw new Error('mounted Share fixture must contain exactly six sessions, seven discovery items, and four invalid cases');
  return root as unknown as Fixture;
}

const fixture = loadFixture();
const response = (body: unknown) => ({ ok: true, status: 200, json: async () => body, text: async () => '' });

function installFetch(items: unknown = fixture.items) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.includes('/api/v1/sessions')) return response({ sessions: fixture.sessions });
    if (url.includes('/api/v1/web/discovery')) return response({ items });
    if (url.includes('/api/v1/sync/push')) return response({ new: 2, updated: 0, skipped: 0, errors: 0, sessions: [{ sessionId: 'sess-new', status: 'new' }, { sessionId: 'sess-updated', status: 'new' }] });
    throw new Error(`unexpected mounted Share fetch: ${url} ${init?.method ?? 'GET'}`);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

describe('mounted Share production boundary', () => {
  beforeEach(() => {
    vi.mocked(useMockConfig.useMockConfig).mockReturnValue({ config: { enabled: false, web: [], tui: [] }, loading: false, error: null, refetch: vi.fn() });
  });
  afterEach(() => { vi.unstubAllGlobals(); vi.clearAllMocks(); });

  it('decodes, joins, groups, tri-state selects eligible IDs, and submits them through PushStep', async () => {
    const fetchMock = installFetch();
    const user = userEvent.setup();
    render(<ShareWizardClient />);
    const project = await screen.findByRole('region', { name: 'project alpha' });
    expect(within(project).getAllByRole('region', { name: 'repository location same label' })).toHaveLength(2);
    expect(within(project).getByRole('region', { name: 'branch main' })).toBeInTheDocument();
    expect(within(project).getByRole('region', { name: 'branch feature' })).toBeInTheDocument();
    const projectBox = within(project).getByRole('checkbox', { name: 'select project alpha' }) as HTMLInputElement;
    expect(projectBox).not.toBeChecked();
    await user.click(within(project).getByRole('checkbox', { name: 'select session sess-new' }));
    expect(projectBox.indeterminate).toBe(true);
    expect(projectBox).toHaveAttribute('data-indeterminate', 'true');
    expect(projectBox).toHaveAttribute('aria-checked', 'mixed');
    await user.click(projectBox);
    expect(projectBox.indeterminate).toBe(false);
    expect(projectBox).toBeChecked();
    expect(within(project).getByRole('checkbox', { name: 'select session sess-updated' })).toBeChecked();
    for (const id of ['sess-shared', 'sess-held']) expect(within(project).getByRole('checkbox', { name: `select session ${id}` })).toBeDisabled();
    await user.click(projectBox);
    expect(projectBox).not.toBeChecked();
    await user.click(projectBox);
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await user.click(screen.getByRole('button', { name: 'continue labels' }));
    await user.click(screen.getByRole('button', { name: 'continue redaction' }));
    expect(await screen.findByText((_, element) => element?.tagName === 'P' && element.textContent?.includes('2 sessions will be uploaded.') === true)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Submit' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/api/v1/sync/push'), expect.objectContaining({ body: JSON.stringify({ sessionIds: ['sess-new', 'sess-updated'], redactionLevel: 'standard', visibility: 'public' }) })));
  });

  it.each(fixture.invalidCases)('fails closed for $name discovery metadata', async ({ operation, sessionId }) => {
    const target = fixture.items.find((row) => row.sessionId === sessionId)!;
    const items: unknown[] = operation === 'remove'
      ? fixture.items.filter((row) => row !== target)
      : operation === 'duplicate'
        ? [...fixture.items, target]
        : fixture.items.map((row) => row === target
          ? operation === 'malformed' ? { ...row, locationLabel: 7 } : { ...row, selectionStatus: 'unknown' }
          : row);
    installFetch(items);
    render(<ShareWizardClient />);
    expect(await screen.findByText(/discovery|metadata|duplicate|locationLabel/i)).toBeInTheDocument();
    expect(screen.getByText('Retry')).toBeInTheDocument();
  });
});
