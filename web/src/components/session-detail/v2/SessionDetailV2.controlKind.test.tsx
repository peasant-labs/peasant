import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';
import { parseServerMessageRaw } from '@peasant-labs/schema';
import { adaptTranscript, TranscriptViewer } from '@peasant-labs/fairtrade/ui';

/**
 * The local app is the consumer that closes the contract loop. WebSocketProvider
 * calls parseServerMessageRaw on the raw session_detail frame. That parser owns
 * the Zod boundary: it strips unknown turn fields and keeps published ones.
 * A producer-classified control record must survive that parse and reach the
 * mounted viewer as a control row. The web does not decide control-ness itself.
 */
const SESSION_ID = '9f6a1d21-4c30-4f7d-8b5e-2a1c0d9e8f77';

const completeFilters = {
  categories: { prompts: true, responses: true, thinking: true, toolcalls: true, system: true },
  toolGroups: { edits: true, bash: true, read: true, search: true, fetch: true, tasks: true, other: true },
  tags: { errors: false, retries: false, revert: false },
  views: { hidden: true, expandAll: false, compact: false },
  checkpoint: 'all',
};

function envelope(): string {
  return JSON.stringify({
    type: 'session_detail',
    topic: 'session_detail',
    id: SESSION_ID,
    data: {
      id: SESSION_ID,
      harness: 'claude-code',
      startTime: '2026-10-06T12:00:00Z',
      endTime: '2026-10-06T12:01:00Z',
      durationMins: 1,
      totalTokens: 0,
      tokensIn: 0,
      tokensOut: 0,
      turnCount: 2,
      toolCallCount: 0,
      turns: [
        {
          index: 0,
          role: 'user',
          content: 'run the checks',
          timestamp: '2026-10-06T12:00:00Z',
          depth: 0,
        },
        {
          index: 1,
          role: 'system',
          content: 'permission mode: auto',
          controlKind: 'permission-mode',
          notAContractField: 'must-be-stripped',
          timestamp: '2026-10-06T12:00:30Z',
          depth: 0,
        },
      ],
    },
  });
}

describe('session_detail controlKind through the local client', () => {
  it('survives parseServerMessageRaw and reaches the viewer as a control row', () => {
    const message = parseServerMessageRaw(envelope());
    expect(message.type).toBe('session_detail');
    const detail = message.data as {
      turns: { index: number; controlKind?: string | null; notAContractField?: string }[];
    };
    // The parser is the Zod boundary WebSocketProvider calls. An unknown turn
    // field is stripped; controlKind is a published field and must remain.
    expect(detail.turns[1].notAContractField).toBeUndefined();
    expect(detail.turns[1].controlKind).toBe('permission-mode');

    const vm = adaptTranscript(detail as never);
    const control = vm.turns.find((turn) => turn.index === 1);
    expect(control?.controlKind).toBe('permission-mode');
    expect(control?.isControl).toBe(true);
    // The recorded bytes survive the cook unchanged; only the row's display drops the
    // leading phrase that repeats the kind.
    expect(control?.content).toBe('permission mode: auto');

    const { container } = render(
      <TranscriptViewer
        viewModel={vm}
        presentation="ledger"
        capabilities={{
          canEdit: false,
          canLabel: false,
          canContribute: false,
          canChangeVisibility: false,
          canExport: false,
        }}
        filters={completeFilters}
      />,
    );
    expect(container.querySelector('#turn-1 .txn-control-row')).not.toBeNull();
    expect(container.querySelector('#turn-1 .txn-control-kind')?.textContent).toBe('permission-mode');
    expect(container.querySelector('#turn-1 .txn-control-value')?.textContent).toBe('auto');
  });
});
