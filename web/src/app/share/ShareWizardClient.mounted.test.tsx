import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import YAML from 'yaml';
import fixtureSource from './testdata/mounted-share.yaml?raw';
import { ShareWizardClient } from './ShareWizardClient';
import * as useMockConfig from '@/hooks/useMockConfig';

vi.mock('@/hooks/useMockConfig');
vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams() }));

type SessionRow = Record<'id' | 'harness' | 'project' | 'projectHash' | 'startTime' | 'preview' | 'shareStatus', string> & Record<'durationMins' | 'totalTokens' | 'turnCount' | 'toolCallCount', number>;
type DiscoveryRow = Record<'sessionId' | 'locationLabel' | 'repositoryLocationId' | 'branch' | 'selectionStatus', string>;
interface InvalidCase { name: string; operation: 'remove' | 'duplicate' | 'malformed' | 'unknown-status'; sessionId: string }
interface Fixture { sessions: SessionRow[]; items: DiscoveryRow[]; invalidCases: InvalidCase[] }

function loadFixture(): Fixture {
  const parsed: unknown = YAML.parse(fixtureSource);
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('mounted Share fixture must be an object');
  const root = parsed as Record<string, unknown>;
   if (Object.keys(root).sort().join(',') !== 'invalidCases,items,sessions' || !Array.isArray(root.sessions) || root.sessions.length !== 8 || !Array.isArray(root.items) || root.items.length !== 9 || !Array.isArray(root.invalidCases) || root.invalidCases.length !== 4) throw new Error('mounted Share fixture must contain exactly eight sessions, nine discovery items, and four invalid cases');
  return root as unknown as Fixture;
}

const fixture = loadFixture();
const response = (body: unknown) => ({ ok: true, status: 200, json: async () => body, text: async () => '' });

function installFetch(items: unknown = fixture.items, annotationsGate: Promise<void> = Promise.resolve(), redactionsGate: Promise<void> = Promise.resolve()) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.includes('/api/v1/sessions')) return response({ sessions: fixture.sessions });
    if (url.includes('/api/v1/web/discovery')) return response({ items });
    if (url.includes('/api/v1/annotations')) {
      await annotationsGate;
      return response({ annotations: [] });
    }
    if (url.includes('/api/v1/sync/redactions')) {
      await redactionsGate;
      return response({ categories: [] });
    }
    if (url.includes('/api/v1/sync/push')) return response({ new: 4, updated: 0, skipped: 0, errors: 0, sessions: [] });
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
    let releaseAnnotations!: () => void;
    let releaseRedactions!: () => void;
    const annotationsGate = new Promise<void>((resolve) => { releaseAnnotations = resolve; });
    const redactionsGate = new Promise<void>((resolve) => { releaseRedactions = resolve; });
    const fetchMock = installFetch(fixture.items, annotationsGate, redactionsGate);
    const user = userEvent.setup();
    render(<ShareWizardClient />);
    const projects = await screen.findAllByRole('region', { name: 'project alpha' });
    expect(projects).toHaveLength(2);
    const project = projects.find((candidate) => within(candidate).queryByRole('checkbox', { name: 'select session sess-new' }))!;
    const otherProject = projects.find((candidate) => candidate !== project)!;
    expect(within(project).getAllByRole('region', { name: 'repository location same label' })).toHaveLength(2);
    expect(within(otherProject).getAllByRole('region', { name: 'repository location same label' })).toHaveLength(2);
    const projectBox = within(project).getByRole('checkbox', { name: 'select project alpha' }) as HTMLInputElement;
    const otherProjectBox = within(otherProject).getByRole('checkbox', { name: 'select project alpha' }) as HTMLInputElement;
    expect(projectBox).not.toBeChecked();
    const locations = within(project).getAllByRole('region', { name: 'repository location same label' });
    const repository = locations.find((candidate) => within(candidate).queryByRole('checkbox', { name: 'select session sess-repo-feature' }))!;
    const repositoryBox = within(repository).getByRole('checkbox', { name: 'select repository location same label' }) as HTMLInputElement;
    const mainBranch = within(repository).getByRole('region', { name: 'branch main' });
    const featureBranch = within(repository).getByRole('region', { name: 'branch feature' });
    const mainBox = within(mainBranch).getByRole('checkbox', { name: 'select branch main' }) as HTMLInputElement;
    const featureBox = within(featureBranch).getByRole('checkbox', { name: 'select branch feature' }) as HTMLInputElement;
    await user.click(mainBox);
    expect(within(mainBranch).getByRole('checkbox', { name: 'select session sess-new' })).toBeChecked();
    expect(within(mainBranch).getByRole('checkbox', { name: 'select session sess-repo-main' })).toBeChecked();
    expect(within(featureBranch).getByRole('checkbox', { name: 'select session sess-repo-feature' })).not.toBeChecked();
    expect(mainBox).toBeChecked();
    expect(featureBox).not.toBeChecked();
    expect(repositoryBox).toHaveAttribute('aria-checked', 'mixed');
    expect(projectBox).toHaveAttribute('aria-checked', 'mixed');
    await user.click(repositoryBox);
    expect(repositoryBox).toBeChecked();
    expect(featureBox).toBeChecked();
    await user.click(repositoryBox);
    expect(repositoryBox).not.toBeChecked();
    await user.click(projectBox);
    expect(projectBox).toBeChecked();
    expect(within(otherProject).getByRole('checkbox', { name: 'select session sess-shared' })).toBeDisabled();
    expect(otherProjectBox).not.toBeChecked();
    for (const id of ['sess-held']) expect(within(otherProject).getByRole('checkbox', { name: `select session ${id}` })).toBeDisabled();
    await user.click(otherProjectBox);
    const footer = document.querySelector('.swz-foot') as HTMLElement;
    expect(within(footer).getByRole('button', { name: 'Continue' })).toBeEnabled();
    expect(screen.getAllByRole('button', { name: 'Continue' })).toHaveLength(1);
    await user.click(within(footer).getByRole('button', { name: 'Continue' }));
    await waitFor(() => expect(within(footer).getByRole('button', { name: 'Continue' })).toBeDisabled());
    releaseAnnotations();
    const skip = await within(footer).findByRole('button', { name: 'Skip' });
    expect(screen.getAllByRole('button', { name: 'Skip' })).toHaveLength(1);
    await user.click(skip);
    await waitFor(() => expect(within(footer).getByRole('button', { name: 'Continue' })).toBeDisabled());
    releaseRedactions();
    const review = await screen.findByRole('region', { name: 'redaction review' });
    expect(within(review).queryByRole('group', { name: 'redaction level' })).not.toBeInTheDocument();
    expect(screen.queryByText('minimal', { exact: true })).not.toBeInTheDocument();
    expect(screen.queryByText('maximum', { exact: true })).not.toBeInTheDocument();
    const continueRedaction = await waitFor(() => {
      const action = within(footer).getByRole('button', { name: 'Continue' });
      expect(action).toBeEnabled();
      return action;
    });
    expect(screen.getAllByRole('button', { name: 'Continue' })).toHaveLength(1);
    await user.click(continueRedaction);
    expect(await screen.findByText((_, element) => element?.tagName === 'P' && element.textContent?.includes('4 sessions will be uploaded.') === true)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Submit' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/api/v1/sync/push'), expect.objectContaining({ body: JSON.stringify({ sessionIds: ['sess-new', 'sess-updated', 'sess-repo-main', 'sess-repo-feature'], redactionLevel: 'standard', visibility: 'public' }) })));
    expect(await screen.findByRole('link', { name: /View in the commons/i })).toHaveAttribute(
      'href',
      'https://village.peasantlabs.org',
    );
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
