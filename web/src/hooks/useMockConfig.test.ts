import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, waitFor, act } from '@testing-library/react';
import { useMockConfig } from '@/hooks/useMockConfig';

describe('useMockConfig', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetAllMocks();
  });

  it('fetches mock config on mount', async () => {
    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => mockConfig,
    });

    renderHook(() => useMockConfig());

    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining('/api/v1/config/mock'),
    );
  });

  it('sets loading state while fetching', async () => {
    fetchMock.mockImplementation(
      () => new Promise(() => {}),
    );

    const { result } = renderHook(() => useMockConfig());

    expect(result.current.loading).toBe(true);
  });

  it('sets config on successful fetch', async () => {
    const mockConfig = { enabled: true, web: ['sessions'], tui: [] };
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => mockConfig,
    });

    const { result } = renderHook(() => useMockConfig());

    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.config).toEqual(mockConfig);
    expect(result.current.error).toBeNull();
  });

  it('sets error on failed fetch', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 500,
      statusText: 'Internal Server Error',
    });

    const { result } = renderHook(() => useMockConfig());

    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.config).toBeNull();
    expect(result.current.error).toContain('Failed to fetch mock config');
  });

  it('sets error on network error', async () => {
    fetchMock.mockRejectedValue(new Error('Network error'));

    const { result } = renderHook(() => useMockConfig());

    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.config).toBeNull();
    expect(result.current.error).toBe('Network error');
  });

  it('refetch function triggers new fetch', async () => {
    const mockConfig = { enabled: true, web: ['sessions'] };
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => mockConfig,
    });

    const { result } = renderHook(() => useMockConfig());

    await waitFor(() => expect(result.current.loading).toBe(false));

    fetchMock.mockClear();

    result.current.refetch();

    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('resets loading and error states on refetch', async () => {
    const mockConfig = { enabled: true, web: ['sessions'] };
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => mockConfig,
    });

    const { result } = renderHook(() => useMockConfig());

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBeNull();

    fetchMock.mockImplementation(
      () => new Promise(() => {}),
    );

    await act(async () => {
      result.current.refetch();
    });

    expect(result.current.loading).toBe(true);
  });
});
