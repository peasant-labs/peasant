import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent } from '@testing-library/react';
import { ChangeTreemap, fileRowAnchor } from './ChangeTreemap';
import { CHANGE_DETAIL_PAYLOAD } from './test-fixtures';
import type { DecodedChangeDetailPayload } from '@/lib/api/map';

afterEach(() => cleanup());

/** Parse a percent style ("12.5%") to a number. */
function pct(v: string): number {
  return parseFloat(v);
}

function tileArea(el: HTMLElement): number {
  return pct(el.style.width) * pct(el.style.height);
}

// ft-ui Treemap (Treemap.jsx) aria-label format: "${label}: ${value} ${unit}"
// e.g. "internal/api/provider.go: 90 lines"
// Old ChangeTreemap used comma: "${path}, ${value} ${unit}"

describe('ChangeTreemap', () => {
  it('renders one tile per changed file, each aria-labelled with path + churn', () => {
    render(<ChangeTreemap payload={CHANGE_DETAIL_PAYLOAD} onSelectFile={() => {}} />);
    // ft-ui Treemap aria-label format uses a colon separator: "path: N lines"
    for (const f of CHANGE_DETAIL_PAYLOAD.files) {
      expect(
        screen.getByRole('button', { name: new RegExp(`^${escapeRe(f.path)}: `) }),
      ).toBeInTheDocument();
    }
  });

  it('sizes tiles proportionally — a higher-churn file gets a larger tile', () => {
    render(<ChangeTreemap payload={CHANGE_DETAIL_PAYLOAD} onSelectFile={() => {}} />);
    // provider.go is +90/-0 (churn 90); README.md is +2/-1 (churn 3).
    const big = screen.getByRole('button', { name: /internal\/api\/provider\.go: / });
    const small = screen.getByRole('button', { name: /README\.md: / });
    expect(tileArea(big)).toBeGreaterThan(tileArea(small));
  });

  it('is strict-monochrome — intensity ramp via data-level, never color/diff tokens', () => {
    const { container } = render(
      <ChangeTreemap payload={CHANGE_DETAIL_PAYLOAD} onSelectFile={() => {}} />,
    );
    const tiles = screen.getAllByRole('button');
    for (const t of tiles) {
      // ft-ui Treemap uses data-level={0..4} attribute (not bg-intensity-N class).
      // Every tile gets the tm-tile class.
      expect(t).toHaveAttribute('data-level');
      const level = Number(t.getAttribute('data-level'));
      expect(level).toBeGreaterThanOrEqual(0);
      expect(level).toBeLessThanOrEqual(4);
      expect(t.className).toContain('tm-tile');
    }
    const html = container.innerHTML;
    expect(html).not.toMatch(/diff-/); // diff tokens are diff-block only
    expect(html).not.toMatch(/rounded|shadow|blur/); // radius-0, no shadow
    expect(html).not.toMatch(/\$/); // no cost
  });

  it('marks the highest-intensity tiles with tm-tile--ink-flip for contrast', () => {
    render(<ChangeTreemap payload={CHANGE_DETAIL_PAYLOAD} onSelectFile={() => {}} />);
    // provider.go: +90/-0 → churn 90 = maxChurn → quantizeLevel(90,90) = 4.
    // ft-ui Treemap adds tm-tile--ink-flip when level >= INK_FLIPS_AT (3).
    const big = screen.getByRole('button', { name: /internal\/api\/provider\.go: / });
    expect(big.getAttribute('data-level')).toBe('4');
    expect(big.className).toContain('tm-tile--ink-flip');

    // A lower-churn tile should NOT be ink-flipped.
    // messages.go: churn 11 → quantizeLevel(11,90) ≈ 12% → level 1 → no ink-flip.
    const mid = screen.getByRole('button', { name: /internal\/api\/messages\.go: / });
    expect(mid.className).not.toContain('tm-tile--ink-flip');
  });

  it('renders visible leaf labels on the prominent tiles', () => {
    render(<ChangeTreemap payload={CHANGE_DETAIL_PAYLOAD} onSelectFile={() => {}} />);
    // ft-ui Treemap shows the leaf of the path (everything after the last "/") in
    // a span.tm-tile-label when the tile is large enough (>= 9% in each dimension).
    const big = screen.getByRole('button', { name: /internal\/api\/provider\.go: / });
    const label = big.querySelector('span.tm-tile-label');
    expect(label).not.toBeNull();
    expect(label?.textContent).toBe('provider.go');

    // messages.go: churn 11 — mid-sized tile; should also show its label.
    const mid = screen.getByRole('button', { name: /internal\/api\/messages\.go: / });
    expect(mid.querySelector('span.tm-tile-label')?.textContent).toBe('messages.go');

    // At least some tiles have visible labels.
    const labelled = screen
      .getAllByRole('button')
      .filter((b) => b.querySelector('span.tm-tile-label') !== null);
    expect(labelled.length).toBeGreaterThan(0);
  });

  it('keeps a title + aria-label on sub-threshold tiles (no label span, still identifiable)', () => {
    render(<ChangeTreemap payload={CHANGE_DETAIL_PAYLOAD} onSelectFile={() => {}} />);
    // README.md is the smallest tile (~3/380 area fraction) — below the 9% width
    // threshold, so no visible .tm-tile-label span, but identifiable via aria-label.
    // ft-ui aria-label format: "path: value unit"
    const tiny = screen.getByRole('button', { name: /^README\.md: / });
    expect(tiny.querySelector('span.tm-tile-label')).toBeNull();
    expect(tiny).toHaveAttribute('title', expect.stringContaining('README.md'));
  });

  it('calls onSelectFile with the file path when a tile is clicked', () => {
    const onSelectFile = vi.fn();
    render(<ChangeTreemap payload={CHANGE_DETAIL_PAYLOAD} onSelectFile={onSelectFile} />);
    // ft-ui Treemap fires onSelect(id, datum); ChangeTreemap passes id=f.path to onSelectFile.
    fireEvent.click(screen.getByRole('button', { name: /internal\/api\/provider\.go: / }));
    expect(onSelectFile).toHaveBeenCalledWith('internal/api/provider.go');
  });

  it('renders nothing when there are no changed files', () => {
    const empty: DecodedChangeDetailPayload = { ...CHANGE_DETAIL_PAYLOAD, files: [] };
    const { container } = render(<ChangeTreemap payload={empty} onSelectFile={() => {}} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('fileRowAnchor is a stable id derived from the path', () => {
    expect(fileRowAnchor('internal/api/server.go')).toBe('review-file:internal/api/server.go');
  });
});

function escapeRe(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
