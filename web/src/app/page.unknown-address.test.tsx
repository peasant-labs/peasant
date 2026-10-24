import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import HomePage from './page';

vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: () => ({ connected: true, data: undefined, error: null }) }));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState({}, '', '/');
});

// The local server answers an address it has no page for with Home's HTML.
describe('Home opened at an address it does not own', () => {
  it('shows the not-found state and reads no sessions', async () => {
    const fetch = vi.fn(async (_input: RequestInfo | URL) => new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetch);
    window.history.replaceState({}, '', '/does-not-exist');
    render(<HomePage />);
    expect(await screen.findByRole('heading', { level: 1, name: 'page not found' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'go to home' })).toHaveAttribute('href', '/');
    expect(screen.queryByRole('region', { name: 'sessions' })).not.toBeInTheDocument();
    expect(fetch.mock.calls.filter(([url]) => String(url).includes('/api/v1/sync/sessions'))).toEqual([]);
  });
});
