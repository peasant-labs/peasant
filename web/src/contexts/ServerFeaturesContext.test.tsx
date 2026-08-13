import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, waitFor } from '@testing-library/react';
import { type ReactNode } from 'react';
import { ServerFeaturesProvider, useServerFeatures } from './ServerFeaturesContext';

describe('ServerFeaturesContext', () => {
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
    <ServerFeaturesProvider>{children}</ServerFeaturesProvider>
  );

  it('fetches the features config on mount', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ experimental: false }) });

    renderHook(() => useServerFeatures(), { wrapper });

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/api/v1/config/features')),
    );
  });

  it('reports experimental once the server vouches for it', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ experimental: true }) });

    const { result } = renderHook(() => useServerFeatures(), { wrapper });

    // Fails closed while the fetch is in flight…
    expect(result.current.experimental).toBe(false);
    // …then unlocks.
    await waitFor(() => expect(result.current.experimental).toBe(true));
  });

  it('stays non-experimental on a default server', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ experimental: false }) });

    const { result } = renderHook(() => useServerFeatures(), { wrapper });

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(result.current.experimental).toBe(false);
  });

  it('fails closed on fetch errors', async () => {
    fetchMock.mockRejectedValue(new Error('network down'));

    const { result } = renderHook(() => useServerFeatures(), { wrapper });

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(result.current.experimental).toBe(false);
  });

  it('fails closed on non-OK responses', async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 503 });

    const { result } = renderHook(() => useServerFeatures(), { wrapper });

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(result.current.experimental).toBe(false);
  });

  it('defaults to all-off without a provider', () => {
    const { result } = renderHook(() => useServerFeatures());
    expect(result.current.experimental).toBe(false);
  });
});
