import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, waitFor } from '@testing-library/react';
import { type ReactNode } from 'react';
import {
  ServerCapabilitiesProvider,
  useServerCapabilities,
  useHasCapability,
} from './ServerCapabilitiesContext';
import { UI_CAPABILITY } from '@/lib/capabilities/tokens';

// The combinatorial fail-closed matrix (loading / thrown / non-OK / malformed /
// null / empty / unknown-token) is exercised end-to-end against the REAL shell
// in LayoutShell.capabilities.test.tsx via the ui_capabilities.yaml fixture.
// This suite pins the direct provider guarantees that integration cannot see:
// the fetch shape (endpoint + fetch-once), the loading→ready transition, and
// the no-provider default that makes an unwrapped read fail closed.
describe('ServerCapabilitiesContext', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetAllMocks();
  });

  const wrapper = ({ children }: { children: ReactNode }) => (
    <ServerCapabilitiesProvider>{children}</ServerCapabilitiesProvider>
  );

  it('fetches the capabilities endpoint exactly once on mount', async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ uiCapabilities: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );

    const { result } = renderHook(() => useServerCapabilities(), { wrapper });

    await waitFor(() => expect(result.current.status).toBe('ready'));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining('/api/v1/config/capabilities'),
    );
  });

  it('transitions loading→ready and advertises the served token by exact membership', async () => {
    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({ uiCapabilities: [UI_CAPABILITY.codeMapNavigationV1] }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );

    const { result } = renderHook(
      () => ({
        state: useServerCapabilities(),
        hasCodeMap: useHasCapability(UI_CAPABILITY.codeMapNavigationV1),
      }),
      { wrapper },
    );

    // Fails closed while the fetch is in flight…
    expect(result.current.state.status).toBe('loading');
    expect(result.current.state.capabilities.size).toBe(0);
    expect(result.current.hasCodeMap).toBe(false);

    // …then the served token unlocks by exact membership.
    await waitFor(() => expect(result.current.state.status).toBe('ready'));
    expect(result.current.hasCodeMap).toBe(true);
    expect(result.current.state.capabilities.has(UI_CAPABILITY.codeMapNavigationV1)).toBe(true);
  });

  it('fails closed for a read outside any provider (default state)', () => {
    const { result } = renderHook(() => ({
      state: useServerCapabilities(),
      hasCodeMap: useHasCapability(UI_CAPABILITY.codeMapNavigationV1),
    }));

    // No fetch without a provider; the default is ready with no capabilities so
    // every gated surface stays hidden rather than flashing while undecided.
    expect(fetchMock).not.toHaveBeenCalled();
    expect(result.current.state.status).toBe('ready');
    expect(result.current.state.capabilities.size).toBe(0);
    expect(result.current.hasCodeMap).toBe(false);
  });
});
