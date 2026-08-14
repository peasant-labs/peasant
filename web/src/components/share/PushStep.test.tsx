import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { PushStep } from '@/components/share/PushStep';
import type { ShareSession, ShareLabel, LabelSelection } from '@/lib/share/types';

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

function label(id: string, origin: 'auto' | 'manual'): ShareLabel {
  return {
    id,
    sessionId: 'sess-1',
    origin,
    annotatorKind: origin === 'manual' ? 'human' : 'rule',
    annotatorName: origin === 'manual' ? 'human-web' : 'classifier',
    typeId: `type.${id}`,
    typeName: id,
    value: 'v',
  };
}

describe('PushStep transparency panel', () => {
  it('shows the destination, payload, privacy, and post-publish note', () => {
    const labels: LabelSelection = {
      bySession: new Map([['sess-1', [label('a1', 'auto'), label('a2', 'manual')]]]),
      includedIds: new Set(['a1', 'a2']),
    };

    render(
      <PushStep
        sessions={[makeSession('sess-1')]}
        selectedIds={new Set(['sess-1'])}
        labels={labels}
        redactionLevel="standard"
      />,
    );

    // fairtrade WhereDoesThisGo composite — chrome is lowercased.
    expect(screen.getByText('where does this go?')).toBeInTheDocument();
    // Destination is the commons URL.
    expect(screen.getByText('https://village.peasantlabs.org')).toBeInTheDocument();
    // What gets sent / stays private headings.
    expect(screen.getByText('what gets sent')).toBeInTheDocument();
    expect(screen.getByText('what stays private')).toBeInTheDocument();
    // Redacted transcripts + selected-labels rows (measure encoded into the line).
    expect(screen.getByText(/redacted transcripts/)).toBeInTheDocument();
    expect(screen.getByText(/selected labels/)).toBeInTheDocument();
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
    const labels: LabelSelection = {
      bySession: new Map([['sess-1', [label('a1', 'auto')]]]),
      includedIds: new Set(['a1']),
    };

    render(
      <PushStep
        sessions={[makeSession('sess-1')]}
        selectedIds={new Set(['sess-1'])}
        labels={labels}
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

  it('reflects the included-vs-discovered label count in the panel', () => {
    const labels: LabelSelection = {
      bySession: new Map([
        ['sess-1', [label('a1', 'auto'), label('a2', 'manual'), label('a3', 'auto')]],
      ]),
      // Only one of three labels kept.
      includedIds: new Set(['a1']),
    };

    render(
      <PushStep
        sessions={[makeSession('sess-1')]}
        selectedIds={new Set(['sess-1'])}
        labels={labels}
      />,
    );

    // "1 of 3" included labels (encoded into the "Selected labels" line).
    expect(screen.getByText(/selected labels · 1 of 3/)).toBeInTheDocument();
  });
});
