import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { VillageAccountRow } from './VillageAccountRow';
import { defaultOnlineUrl } from '@/lib/village/links';

vi.mock('@/lib/api/settings', () => ({ logOutOfVillage: vi.fn() }));

function signedIn(villageUrl: string) {
  return { authenticated: true, username: 'alice-dev', villageUrl } as Parameters<typeof VillageAccountRow>[0]['auth'];
}

afterEach(() => cleanup());

describe('VillageAccountRow — who this computer is signed in as', () => {
  it('names peasant online, not its host, for the default server', () => {
    render(<VillageAccountRow auth={signedIn(defaultOnlineUrl())} onLoggedOut={() => undefined} />);
    const row = screen.getByText(/signed in to peasant online with github/).closest('.srow');
    expect(row).not.toHaveTextContent(new URL(defaultOnlineUrl()).host);
    expect(row).not.toHaveTextContent('server:');
  });

  it('adds the address of any other server as a quiet second detail', () => {
    render(<VillageAccountRow auth={signedIn('http://127.0.0.1:3001')} onLoggedOut={() => undefined} />);
    expect(screen.getByText(/signed in to peasant online with github/)).toBeInTheDocument();
    expect(screen.getByText(/server: 127\.0\.0\.1:3001/)).toHaveClass('stg-muted');
  });
});
