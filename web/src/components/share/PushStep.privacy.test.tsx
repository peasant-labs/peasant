import { useState } from 'react';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { PushStep } from '@/components/share/PushStep';
import type { ShareFooterActions } from '@/components/share/footer-actions';
import type { ShareSession, LabelSelection } from '@/lib/share/types';
import { LICENSE_NOTICE_CASES } from '@/components/share/PushStep.privacy.fixtures';

// PushStep names the license a push will apply and links the privacy notice
// before the user submits. It reads the effective license from
// GET /api/v1/config/publish; the notice link renders regardless, so a fetch
// failure never blocks publishing.

function makeSession(id: string): ShareSession {
  return {
    id,
    provider: 'claude-code',
    projectName: 'demo-project',
    projectHash: 'ph-1',
    hostSlug: 'host',
    startTime: '2026-02-24T09:00:00Z',
    durationMins: 30,
    totalTokens: 10000,
    turnCount: 10,
    model: 'claude-sonnet-4-6',
    shareStatus: 'new',
    preview: 'demo',
  };
}

const EMPTY_LABELS: LabelSelection = { bySession: new Map(), includedIds: new Set() };
// Stable identities: PushStep's footer effect fires on every change, so inline
// literal props here would re-create on each render and loop forever.
const HARNESS_SESSIONS = [makeSession('sess-1')];
const HARNESS_SELECTED = new Set(['sess-1']);

function PushHarness() {
  const [actions, setActions] = useState<ShareFooterActions | null>(null);
  return (
    <>
      <PushStep
        sessions={HARNESS_SESSIONS}
        selectedIds={HARNESS_SELECTED}
        labels={EMPTY_LABELS}
        onFooterActionsChange={setActions}
      />
      {actions?.primary && (
        <button type="button" onClick={actions.primary.onClick}>
          {actions.primary.label}
        </button>
      )}
    </>
  );
}

function stubPublishConfig(license: string): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).includes('/api/v1/config/publish')) {
        return new Response(JSON.stringify({ license }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        });
      }
      throw new Error(`unexpected fetch to ${String(input)}`);
    }),
  );
}

const NOTICE_HREF = 'https://village.peasantlabs.org/privacy';

describe('PushStep license notice', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.unstubAllEnvs();
    vi.resetAllMocks();
  });

  for (const c of LICENSE_NOTICE_CASES) {
    it(`names the license and links the notice for ${c.name}`, async () => {
      stubPublishConfig(c.license);
      render(<PushHarness />);

      // The phrase appears once the effective license loads.
      const phrase = await screen.findByText(new RegExp(c.phrase));
      expect(phrase).toBeInTheDocument();

      // The notice link points at the commons privacy page.
      const link = screen.getByRole('link', { name: 'privacy notice' });
      expect(link).toHaveAttribute('href', NOTICE_HREF);

      // The submit button names the license choice.
      expect(
        await screen.findByRole('button', { name: c.buttonLabel }),
      ).toBeInTheDocument();
    });
  }

  it('still links the notice and does not block submit when the fetch fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new Error('network down');
      }),
    );
    render(<PushHarness />);

    // The notice link renders without the license value.
    const link = await screen.findByRole('link', { name: 'privacy notice' });
    expect(link).toHaveAttribute('href', NOTICE_HREF);

    // No license phrase, and the generic submit label stays usable.
    expect(screen.queryByText(/publish under|publish without a license/)).toBeNull();
    expect(screen.getByRole('button', { name: 'Submit' })).toBeInTheDocument();
  });
});
