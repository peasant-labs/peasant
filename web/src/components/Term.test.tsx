import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Term } from './Term';
import { GLOSSARY, getTerm, type GlossaryKey } from '@/lib/glossary';

// The full set of keys every surface relies on. If a <Term k="…"> is added
// without a matching glossary entry, this list (and the type) catch it.
const REQUIRED_KEYS: GlossaryKey[] = [
  'change', 'defaultBranch', 'commit', 'merged', 'ahead', 'pr', 'repo', 'diff',
  'active', 'idle', 'stale',
  'session', 'task', 'recorded', 'transcript', 'scope', 'coverage',
  'node', 'structureEdge', 'violation', 'darkMatter', 'slice', 'timeStrip',
  'shapedBy', 'oftenEditedWith',
  'linked', 'candidate', 'unrecorded',
  'outputTokens', 'cost', 'connection', 'contribute',
];

function renderTerm(ui: React.ReactElement) {
  // The design-system Tooltip is self-contained — no provider to mount.
  return render(ui);
}

describe('glossary', () => {
  it('defines every required key with a non-empty term and definition', () => {
    for (const key of REQUIRED_KEYS) {
      const entry = getTerm(key);
      expect(entry.term.length, `term for ${key}`).toBeGreaterThan(0);
      expect(entry.short.length, `definition for ${key}`).toBeGreaterThan(10);
    }
  });

  it('has no entry whose definition leans on banned verbs', () => {
    for (const [key, entry] of Object.entries(GLOSSARY)) {
      expect(entry.short.toLowerCase(), key).not.toMatch(/\b(proves|guarantees)\b/);
    }
  });
});

describe('Term', () => {
  it('renders the glossary term and exposes the definition to assistive tech', () => {
    renderTerm(<Term k="commit" />);
    const el = screen.getByText('saved update');
    expect(el).toBeInTheDocument();
    expect(el).toHaveAttribute('aria-label', expect.stringContaining('saved snapshot'));
    expect(el).toHaveAttribute('tabindex', '0');
  });

  it('lets children override the visible text while keeping the definition', () => {
    renderTerm(<Term k="change">lines of work</Term>);
    const el = screen.getByText('lines of work');
    expect(el).toHaveAttribute('aria-label', expect.stringContaining('separate line of work'));
  });
});
