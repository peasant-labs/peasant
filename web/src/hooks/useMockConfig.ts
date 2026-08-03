'use client';

import { useState, useEffect, useCallback } from 'react';
import { getApiBaseUrl } from '@/lib/api/base';

export interface MockConfig {
  enabled: boolean;
  web?: string[];
  tui?: string[];
  api?: string[];
}

export interface UseMockConfigResult {
  config: MockConfig | null;
  loading: boolean;
  error: string | null;
  refetch: () => void;
}

export function useMockConfig(): UseMockConfigResult {
  const [config, setConfig] = useState<MockConfig | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchConfig = useCallback(() => {
    setLoading(true);
    setError(null);

    const url = `${getApiBaseUrl()}/api/v1/config/mock`;
    fetch(url)
      .then((res) => {
        if (!res.ok) {
          throw new Error(`Failed to fetch mock config: ${res.status} ${res.statusText}`);
        }
        return res.json();
      })
      .then((data: MockConfig) => {
        setConfig(data);
        setLoading(false);
      })
      .catch((err) => {
        setError(err instanceof Error ? err.message : 'Unknown error');
        setLoading(false);
      });
  }, []);

  useEffect(() => {
    fetchConfig();
  }, [fetchConfig]);

  return { config, loading, error, refetch: fetchConfig };
}
