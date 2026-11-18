import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { Harness } from '@peasant-labs/schema';
import { providerDisplayName } from '@peasant-labs/fairtrade/ui';
import { ProviderIcon, ProviderName } from '@/lib/ft-ui';

/**
 * schema 0.54.0 adds antigravity-cli / antigravity-ide. Peasant does not map
 * them itself: ProviderName and ProviderIcon take Harness and render Fairtrade's
 * identity (Antigravity CLI, gemini mark, teal accent).
 */
describe('ProviderName and ProviderIcon', () => {
  afterEach(cleanup);

  it.each([Harness.AntigravityCLI, Harness.AntigravityIDE])('renders the Fairtrade name and gemini mark for %s', (harness) => {
    render(
      <>
        <ProviderName harness={harness} />
        <ProviderIcon harness={harness} label accent />
      </>,
    );

    const displayName = providerDisplayName(harness);
    const name = document.querySelector('.pv-name');
    expect(name?.querySelector('.pv-name-label')).toHaveTextContent(harness);
    expect(name?.querySelector('.brand')).toHaveAttribute('data-brand', 'gemini');

    const mark = screen.getByRole('img', { name: displayName });
    expect(mark).toHaveAttribute('data-brand', 'gemini');
    expect(mark.closest('.pv-icon')).toHaveStyle({ color: 'var(--teal)' });
    expect(displayName).toBe({
      [Harness.AntigravityCLI]: 'Antigravity CLI',
      [Harness.AntigravityIDE]: 'Antigravity IDE',
    }[harness]);
  });
});
