import { describe, it, expect, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent, within } from '@testing-library/react';
import { Harness } from '@peasant-labs/schema';
import type { SessionSummary } from '@/types/messages';
import type { ProjectHash } from '@/lib/navigation/projectRoutes';
import { AllSessions } from './AllSessions';

/** A syntactically valid ProjectHash (64 chars) — parseProjectHash rejects
 *  anything shorter, which is what makes a row unopenable. */
function hash(seed: string): ProjectHash {
  return seed.repeat(64).slice(0, 64) as ProjectHash;
}

function session(over: Partial<SessionSummary> = {}): SessionSummary {
  return {
    id: 'e5773272aaaa',
    harness: Harness.ClaudeCode,
    startTime: '2026-08-12T14:20:00.000Z',
    durationMins: 1,
    toolCallCount: 0,
    totalTokens: 28_600,
    turnCount: 4,
    project: 'tmp',
    projectHash: hash('a'),
    ...over,
  } as SessionSummary;
}

describe('AllSessions', () => {
  afterEach(cleanup);

  it('opens the session viewer for a row with a project hash', () => {
    render(<AllSessions sessions={[session()]} />);
    const link = screen.getByRole('link', { name: /Open session e5773272/ });
    expect(link).toHaveAttribute('href', `/projects/${hash('a')}/e5773272aaaa`);
  });

  it('renders a session with no project hash as static text, not a broken link', () => {
    render(<AllSessions sessions={[session({ projectHash: undefined, project: undefined })]} />);
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.getByLabelText(/no project recorded, not openable/)).toBeInTheDocument();
  });

  it('filters by project, harness, and the visible short id', () => {
    const sessions = [
      session({ id: 'aaaa1111zzzz', project: 'peasant', projectHash: hash('b') }),
      session({ id: 'bbbb2222zzzz', project: 'village', projectHash: hash('c') }),
    ];
    render(<AllSessions sessions={sessions} />);
    const box = screen.getByLabelText('search sessions');

    fireEvent.change(box, { target: { value: 'village' } });
    expect(screen.queryByText('aaaa1111')).not.toBeInTheDocument();
    expect(screen.getByText('bbbb2222')).toBeInTheDocument();

    fireEvent.change(box, { target: { value: 'aaaa1111' } });
    expect(screen.getByText('aaaa1111')).toBeInTheDocument();
    expect(screen.queryByText('bbbb2222')).not.toBeInTheDocument();

    // Harness matches every row here, so both come back.
    fireEvent.change(box, { target: { value: 'claude' } });
    expect(screen.getByText('aaaa1111')).toBeInTheDocument();
    expect(screen.getByText('bbbb2222')).toBeInTheDocument();
  });

  it('shows the generated title when there is one, and the id when there is not', () => {
    const sessions = [
      session({ id: 'titled00zzzz', projectHash: hash('b') }),
      session({ id: 'untitled0zzz', projectHash: hash('c') }),
    ];
    render(
      <AllSessions sessions={sessions} titles={new Map([['titled00zzzz', 'refactor the ingest pipeline']])} />,
    );
    expect(screen.getByText('refactor the ingest pipeline')).toBeInTheDocument();
    // No title stored for the second session, so it keeps its short id.
    expect(screen.getByText('untitled')).toBeInTheDocument();
  });

  it('finds a session by its title', () => {
    const sessions = [
      session({ id: 'aaaa1111zzzz', projectHash: hash('b') }),
      session({ id: 'bbbb2222zzzz', projectHash: hash('c') }),
    ];
    render(
      <AllSessions sessions={sessions} titles={new Map([['bbbb2222zzzz', 'fix the redaction gate']])} />,
    );
    fireEvent.change(screen.getByLabelText('search sessions'), { target: { value: 'redaction' } });
    expect(screen.getByText('fix the redaction gate')).toBeInTheDocument();
    expect(screen.queryByText('aaaa1111')).not.toBeInTheDocument();
  });

  it('states plainly when a query matches nothing', () => {
    render(<AllSessions sessions={[session()]} />);
    fireEvent.change(screen.getByLabelText('search sessions'), {
      target: { value: 'nothing-matches-this' },
    });
    expect(screen.getByText(/no sessions match/)).toBeInTheDocument();
  });

  it('returns to the first page when the query narrows the result', () => {
    // 60 sessions over a 25-row page size = 3 pages.
    const many = Array.from({ length: 60 }, (_, i) =>
      session({
        id: `id${String(i).padStart(6, '0')}`,
        projectHash: hash(String(i % 10)),
        project: i === 59 ? 'needle' : 'haystack',
      }),
    );
    render(<AllSessions sessions={many} />);

    fireEvent.click(screen.getByRole('button', { name: 'next' }));
    expect(screen.getByText(/page 2 of 3/)).toBeInTheDocument();

    // Narrowing to a single match must land on page 1, not a stale page 2.
    fireEvent.change(screen.getByLabelText('search sessions'), { target: { value: 'needle' } });
    expect(screen.queryByText(/page 2/)).not.toBeInTheDocument();
    expect(screen.getByText('id000059')).toBeInTheDocument();
  });

  it('shows the filtered count against the total while searching', () => {
    const sessions = [
      session({ id: 'aaaa1111zzzz', project: 'peasant', projectHash: hash('b') }),
      session({ id: 'bbbb2222zzzz', project: 'village', projectHash: hash('c') }),
    ];
    render(<AllSessions sessions={sessions} />);
    expect(screen.getByText('2 sessions')).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText('search sessions'), { target: { value: 'village' } });
    expect(screen.getByText(/1 session of 2/)).toBeInTheDocument();
  });

  it('renders nothing at all when there are no sessions', () => {
    const { container } = render(<AllSessions sessions={[]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('orders newest first', () => {
    const sessions = [
      session({ id: 'older0000000', startTime: '2026-08-01T10:00:00.000Z', projectHash: hash('b') }),
      session({ id: 'newer0000000', startTime: '2026-08-20T10:00:00.000Z', projectHash: hash('c') }),
    ];
    render(<AllSessions sessions={sessions} />);
    const links = screen.getAllByRole('link');
    expect(within(links[0]).getByText('newer000')).toBeInTheDocument();
  });
});
