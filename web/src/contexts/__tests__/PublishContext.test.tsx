import { afterEach, describe, expect, it } from 'vitest';
import { act, cleanup, render, screen } from '@testing-library/react';
import { PublishProvider, usePublishState } from '@/contexts/PublishContext';

afterEach(() => {
  cleanup();
});

function CacheWriter() {
  const store = usePublishState();
  return (
    <button
      onClick={() => store.updateRedactionCache((cache) => new Map(cache).set('standard:session', { status: 'failure', error: 'scan failed' }))}
    >
      save scan failure
    </button>
  );
}

function CacheReader() {
  const store = usePublishState();
  const entry = store.redactionCache.get('standard:session');
  return <p>{entry?.status === 'failure' ? entry.error : 'no cached scan'}</p>;
}

describe('usePublishState', () => {
  it('fails loudly, naming the missing provider, outside PublishProvider', () => {
    function BareConsumer() {
      usePublishState();
      return null;
    }

    expect(() => render(<BareConsumer />)).toThrow(/PublishProvider/);
  });

  it('shares one app-level store between consumers under PublishProvider', async () => {
    render(
      <PublishProvider>
        <CacheWriter />
        <CacheReader />
      </PublishProvider>,
    );

    await act(async () => {
      screen.getByRole('button', { name: 'save scan failure' }).click();
    });
    expect(screen.getByText('scan failed')).toBeInTheDocument();
  });

  it('gives each provider mount its own store', async () => {
    render(
      <PublishProvider>
        <CacheReader />
      </PublishProvider>,
    );

    expect(screen.getByText('no cached scan')).toBeInTheDocument();
  });
});
