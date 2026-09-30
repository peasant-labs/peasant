import { useState } from 'react';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { PushStep } from '@/components/share/PushStep';
import type { ShareFooterActions } from '@/components/share/footer-actions';
import type { ShareSession } from '@/lib/share/types';
import * as pushApi from '@/lib/share/push';

vi.mock('@/lib/share/push', () => ({ runPush: vi.fn() }));

function PushHarness() {
  const [actions, setActions] = useState<ShareFooterActions | null>(null);
  return <>
    <PushStep sessions={PUSH_SESSIONS} selectedIds={PUSH_SELECTED} onFooterActionsChange={setActions} />
    {actions?.primary && <button type="button" onClick={actions.primary.onClick}>{actions.primary.label}</button>}
  </>;
}

const PUSH_SESSIONS = [makeSession('sess-1')];
const PUSH_SELECTED = new Set(['sess-1']);

function makeSession(id: string, totalTokens = 10000): ShareSession {
  return {
    id,
    provider: 'claude-code',
    projectName: 'demo-project',
    projectHash: 'ph-1',
    hostSlug: 'host',
    startTime: '2026-02-24T09:00:00Z',
    durationMins: 30,
    totalTokens,
    turnCount: 10,
    model: 'claude-sonnet-4-6',
    shareStatus: 'new',
    preview: 'demo',
  };
}

describe('PushStep transparency panel', () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.resetAllMocks();
  });
  it('shows the destination, payload, privacy, and post-publish note', () => {
    render(
      <PushStep
        sessions={[makeSession('sess-1')]}
        selectedIds={new Set(['sess-1'])}
        redactionLevel="standard"
        onFooterActionsChange={() => {}}
      />,
    );

    // fairtrade WhereDoesThisGo composite — chrome is lowercased.
    expect(screen.getByText('where does this go?')).toBeInTheDocument();
    // Destination is the commons URL.
    expect(screen.getByText('https://village.peasantlabs.org')).toBeInTheDocument();
    // What gets sent / stays private headings.
    expect(screen.getByText('what gets sent')).toBeInTheDocument();
    expect(screen.getByText('what stays private')).toBeInTheDocument();
    // Redacted transcripts row (measure encoded into the line). The labels step
    // is gone, and so is its row: the push never sent the labels it listed.
    expect(screen.getByText(/redacted transcripts/)).toBeInTheDocument();
    expect(screen.queryByText(/selected labels/)).not.toBeInTheDocument();
    // A first publication is private; the wizard never claims it is public.
    expect(screen.getByText('private')).toBeInTheDocument();
    expect(screen.queryByText('public')).not.toBeInTheDocument();
    // Local copy note mentions the sync path.
    expect(
      screen.getByText(/~\/\.local\/share\/peasant\/peasant-sync\//),
    ).toBeInTheDocument();
    // Redaction level is surfaced.
    // The privacy line must describe what redaction FINDS, not promise
    // completeness pattern matching cannot deliver.
    expect(screen.getByText(/rewritten at the standard level, best effort/)).toBeInTheDocument();
    expect(screen.queryByText(/stripped at the/)).not.toBeInTheDocument();
  });

  it('states the boundary-values and context-travels one-liners', () => {
    render(
      <PushStep
        sessions={[makeSession('sess-1')]}
        selectedIds={new Set(['sess-1'])}
        onFooterActionsChange={() => {}}
      />,
    );

    // The boundary values line — one line, plain, before anything is sent.
    // Rendered by the fairtrade WhereDoesThisGo composite (lowercased chrome).
    expect(
      screen.getByText(
        'nothing leaves your machine until you choose to send it. redacted by default.',
      ),
    ).toBeInTheDocument();
    // The context-travels line under "What gets sent".
    expect(
      screen.getByText(
        'Annotations and commit links travel with the transcript — minus whatever redaction removed.',
      ),
    ).toBeInTheDocument();
  });

  it('uses the configured commons destination after a successful submission', async () => {
    vi.stubEnv('NEXT_PUBLIC_COMMONS_URL', 'https://configured.example.test');
    vi.mocked(pushApi.runPush).mockResolvedValue({
      new: 1,
      updated: 0,
      skipped: 0,
      errors: 0,
      sessions: [{ sessionId: 'sess-1', status: 'new' }],
    });
    render(<PushHarness />);
    await userEvent.click(await screen.findByRole('button', { name: 'Submit' }));

    expect(await screen.findByRole('link', { name: /View in the commons/i })).toHaveAttribute(
      'href',
      'https://configured.example.test',
    );
  });
});
