import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import YAML from 'yaml';
import fixtureSource from './testdata/mounted-share-linked-sessions.yaml?raw';
import { ShareWizardClient } from './ShareWizardClient';
import * as useMockConfig from '@/hooks/useMockConfig';

// Mockable search params: the deep-link arm sets ?sessions=, the control arm
// leaves them empty. Both mount the same production surface.
let currentSearchParams = new URLSearchParams();
vi.mock('@/hooks/useMockConfig');
vi.mock('next/navigation', () => ({ useSearchParams: () => currentSearchParams }));

interface SessionRow {
  id: string;
  harness: string;
  project: string;
  projectHash: string;
  startTime: string;
  durationMins: number;
  totalTokens: number;
  turnCount: number;
  toolCallCount: number;
  preview: string;
  shareStatus: string;
  sessionOrigin: string;
}
interface DiscoveryRow {
  sessionId: string;
  locationLabel: string;
  repositoryLocationId: string;
  branch: string;
  selectionStatus: string;
}
interface Fixture {
  linkedSessionId: string;
  discoveryOnly: SessionRow[];
  linkedOnly: SessionRow[];
  items: DiscoveryRow[];
}

// The deletion guard is a required-NAME manifest over the fixture's roles: the
// session ids each list must keep. A count would not notice the two lists
// swapping a row, which is exactly the mistake that would make the deep-link
// assertion vacuous.
const requiredDiscoveryOnlyIds = ['sess-user-browsable'];
const requiredLinkedOnlyIds = ['sess-agent-linked'];

function loadFixture(): Fixture {
  const parsed: unknown = YAML.parse(fixtureSource);
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error('linked-session fixture must be an object');
  }
  const root = parsed as Record<string, unknown>;
  if (Object.keys(root).sort().join(',') !== 'discoveryOnly,items,linkedOnly,linkedSessionId') {
    throw new Error('linked-session fixture must contain exactly linkedSessionId, discoveryOnly, linkedOnly and items');
  }
  const fixture = root as unknown as Fixture;
  if (!Array.isArray(fixture.discoveryOnly) || !Array.isArray(fixture.linkedOnly) || !Array.isArray(fixture.items)) {
    throw new Error('linked-session fixture lists must be arrays');
  }
  requireIds('discoveryOnly', fixture.discoveryOnly, requiredDiscoveryOnlyIds);
  requireIds('linkedOnly', fixture.linkedOnly, requiredLinkedOnlyIds);
  if (!fixture.linkedOnly.some((row) => row.id === fixture.linkedSessionId)) {
    throw new Error('linked-session fixture linkedSessionId must name a row the discovery list withholds');
  }
  for (const row of fixture.linkedOnly) {
    if (row.sessionOrigin !== 'agent') {
      throw new Error(`linked-session fixture row ${row.id} must be agent-driven; a row the discovery list would have offered anyway proves nothing`);
    }
  }
  for (const row of [...fixture.discoveryOnly, ...fixture.linkedOnly]) {
    if (!fixture.items.some((item) => item.sessionId === row.id)) {
      throw new Error(`linked-session fixture has no discovery metadata row for session ${row.id}`);
    }
  }
  return fixture;
}

function requireIds(list: string, rows: SessionRow[], required: string[]): void {
  if (required.length === 0) throw new Error(`linked-session fixture declares no required ${list} session ids`);
  for (const id of required) {
    if (!rows.some((row) => row.id === id)) {
      throw new Error(`linked-session fixture ${list} is missing required session ${id}; restore the row or remove it from the required ids`);
    }
  }
}

const fixture = loadFixture();
const response = (body: unknown) => ({ ok: true, status: 200, json: async () => body, text: async () => '' });

/**
 * Route the mounted surface's fetches by EXACT path, never by containment.
 *
 * The share corpus's mock dispatches on `url.includes('/api/v1/sessions')`,
 * first match wins. A sessions-adjacent route would be swallowed by that arm
 * and answered with the discovery list, so a test asserting the by-id route
 * resolved a hidden session would pass without the route existing at all.
 */
function installFetch(): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const requested = new URL(String(input), 'http://mounted.test');
    switch (requested.pathname) {
      case '/api/v1/sessions':
        return response({ sessions: fixture.discoveryOnly });
      case '/api/v1/session-summaries': {
        const ids = new Set((requested.searchParams.get('ids') ?? '').split(',').filter(Boolean));
        const all = [...fixture.discoveryOnly, ...fixture.linkedOnly];
        return response({ sessions: all.filter((row) => ids.has(row.id)) });
      }
      case '/api/v1/web/discovery':
        return response({ items: fixture.items });
      case '/api/v1/annotations':
        return response({ annotations: [] });
      case '/api/v1/sync/redactions':
        return response({ categories: [] });
      default:
        throw new Error(`unexpected mounted Share fetch: ${String(input)}`);
    }
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

describe('mounted Share link resolution', () => {
  beforeEach(() => {
    currentSearchParams = new URLSearchParams();
    vi.mocked(useMockConfig.useMockConfig).mockReturnValue({
      config: { enabled: false, web: [], tui: [] },
      loading: false,
      error: null,
      refetch: vi.fn(),
    });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it('resolves a link to an agent-driven session the chooser withholds, and offers it for selection', async () => {
    currentSearchParams = new URLSearchParams({ sessions: fixture.linkedSessionId });
    const fetchMock = installFetch();
    render(<ShareWizardClient />);

    const checkbox = await screen.findByRole('checkbox', { name: `select session ${fixture.linkedSessionId}` });
    expect(checkbox).toBeEnabled();
    expect(screen.queryByText(/None of the linked sessions were found on this machine/i)).not.toBeInTheDocument();

    // Resolution went through the by-id route with the linked identifier, not
    // through the discovery list.
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining(`/api/v1/session-summaries?ids=${fixture.linkedSessionId}`),
    ));
  });

  it('leaves the same agent-driven session out of the chooser when no link named it', async () => {
    installFetch();
    render(<ShareWizardClient />);

    // Wait for the surface to finish loading by finding the browsable row.
    for (const row of fixture.discoveryOnly) {
      expect(await screen.findByRole('checkbox', { name: `select session ${row.id}` })).toBeInTheDocument();
    }
    expect(screen.queryByRole('checkbox', { name: `select session ${fixture.linkedSessionId}` })).not.toBeInTheDocument();
  });
});
