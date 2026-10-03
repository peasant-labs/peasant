import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { TurnTouchedFiles } from './TurnTouchedFiles';

afterEach(() => cleanup());

describe('TurnTouchedFiles', () => {
  it('lists each path as text and marks the scoped file current without a link look', () => {
    render(
      <TurnTouchedFiles
        touches={{ turnIndex: 4, edits: ['src/index.ts'], reads: ['README.md'] }}
        activeFile="src/index.ts"
      />,
    );
    expect(screen.queryAllByRole('link')).toEqual([]);
    const active = screen.getByText('src/index.ts');
    expect(active).toHaveAttribute('aria-current', 'true');
    expect(active.className).toContain('font-semibold');
    expect(active.className).not.toContain('underline');
    expect(screen.getByText('README.md')).not.toHaveAttribute('aria-current');
  });
});
