import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { NodeProps } from '@xyflow/react';
import {
  MapSquareNode,
  mapNodeAriaLabel,
  mapNodeDomId,
  type MapSquareNodeData,
} from './MapSquareNode';
import { NODE_BORDER, NODE_COUNT_TEXT, NODE_FILL, NODE_TEXT } from './intensity';
import { FIXTURE_NODES, LEVEL_NODES } from './test-fixtures';
import type { MapNodeDatum, NodeDelta } from './types';
import type { IntensityLevel } from './intensity';

// The node renders <Handle> elements that require the React Flow store; mock
// the runtime (type imports still come from the real package).
vi.mock('@xyflow/react', () => ({
  Handle: () => null,
  Position: { Top: 'top', Right: 'right', Bottom: 'bottom', Left: 'left' },
}));

const ingest = FIXTURE_NODES.find((n) => n.id === 'internal/ingest')!;
const web = FIXTURE_NODES.find((n) => n.id === 'web')!;
const cmd = FIXTURE_NODES.find((n) => n.id === 'cmd')!;

function renderNode(node: MapNodeDatum, overrides: Partial<MapSquareNodeData> = {}) {
  const data: MapSquareNodeData = {
    node,
    selected: false,
    focused: false,
    showEffort: false,
    sizeMetric: 'loc',
    ...overrides,
  };
  return render(<MapSquareNode {...({ data } as unknown as NodeProps)} />);
}

describe('MapSquareNode', () => {
  it('renders the name and font-mono tabular counts with an aria-label', () => {
    renderNode(ingest);
    const button = screen.getByRole('button', {
      name: 'ingest · package · 6 of 7 files recorded',
    });
    expect(button).toBeInTheDocument();
    expect(screen.getByText('ingest')).toBeInTheDocument();
    const counts = screen.getByText('6/7 · 1200 loc');
    expect(counts.className).toContain('font-mono');
    expect(counts.className).toContain('tabular-nums');
  });

  it('shows the touch metric when sized by activity', () => {
    renderNode(ingest, { sizeMetric: 'touchCount' });
    expect(screen.getByText('6/7 · 18 touches')).toBeInTheDocument();
  });

  it('dims fill, text, and border by traceability · full coverage', () => {
    renderNode(cmd); // 2/2 recorded
    const button = screen.getByRole('button');
    expect(button.className).toContain('bg-surface-elev');
    expect(button.className).toContain('border-rule');
  });

  it('dims fill, text, and border by traceability · dark matter', () => {
    renderNode(web); // 0/20 recorded
    const button = screen.getByRole('button');
    expect(button.className).toContain('bg-canvas');
    expect(button.className).toContain('border-intensity-1');
    // Dimmed labels stop at the text-safe intensity-3 token.
    expect(screen.getByText('web').className).toContain('text-intensity-3');
    expect(screen.getByText('web').className).not.toContain('text-ink-4');
  });

  it('marks NEW nodes with a rule-strong border and a NEW eyebrow tag', () => {
    renderNode(ingest, { delta: 'new' });
    const button = screen.getByRole('button', { name: /· new$/ });
    expect(button.className).toContain('border-rule-strong');
    const tag = screen.getByText('NEW');
    expect(tag.className).toContain('v2-eyebrow');
  });

  it('renders removed nodes with a dashed border, no fill, and ink-4 label', () => {
    renderNode(ingest, { delta: 'removed' });
    const button = screen.getByRole('button', { name: /· removed$/ });
    expect(button.className).toContain('border-dashed');
    expect(button.className).toContain('bg-transparent');
    expect(screen.getByText('ingest').className).toContain('text-ink-4');
  });

  it('renders the bottom-edge effort intensity bar only when the overlay is on', () => {
    const { unmount } = renderNode(ingest, { showEffort: true }); // density 0.8 → level 4
    expect(screen.getByTestId('effort-bar').className).toContain('bg-intensity-4');
    unmount();

    renderNode(ingest, { showEffort: false });
    expect(screen.queryByTestId('effort-bar')).toBeNull();
  });

  it('exposes selection via aria-pressed and data-state', () => {
    renderNode(ingest, { selected: true });
    const button = screen.getByRole('button');
    expect(button).toHaveAttribute('aria-pressed', 'true');
    expect(button).toHaveAttribute('data-state', 'selected');
  });

  it('exposes keyboard focus via data-state', () => {
    renderNode(ingest, { focused: true });
    expect(screen.getByRole('button')).toHaveAttribute('data-state', 'focused');
  });

  it('carries the focus-mono utility', () => {
    renderNode(ingest);
    expect(screen.getByRole('button').className).toContain('focus-mono');
  });

  it('sets the stable DOM id from data.domId', () => {
    renderNode(ingest, { domId: mapNodeDomId(ingest.id) });
    expect(screen.getByRole('button').id).toBe('map-node-internal/ingest');
  });

  it('exposes expandability via aria-expanded only when expandable', () => {
    const { unmount } = renderNode(ingest, { expandable: true });
    expect(screen.getByRole('button')).toHaveAttribute('aria-expanded', 'false');
    unmount();

    renderNode(ingest);
    expect(screen.getByRole('button')).not.toHaveAttribute('aria-expanded');
  });

  it('includes selected state in the aria-label', () => {
    renderNode(ingest, { selected: true });
    expect(
      screen.getByRole('button', {
        name: 'ingest · package · 6 of 7 files recorded · selected',
      }),
    ).toBeInTheDocument();
  });

  it('badges contained violations with a danger glyph and counts them in the aria-label', () => {
    const { unmount } = renderNode(ingest, { containedViolations: 2 });
    const badge = screen.getByTestId('violation-badge');
    expect(badge.className).toContain('text-danger');
    expect(badge.className).toContain('font-mono');
    expect(badge).toHaveTextContent('2');
    expect(badge.querySelector('svg')).not.toBeNull(); // Lucide glyph, no emoji
    expect(
      screen.getByRole('button', {
        name: 'ingest · package · 6 of 7 files recorded · contains 2 violations',
      }),
    ).toBeInTheDocument();
    unmount();

    renderNode(ingest, { containedViolations: 1 });
    expect(
      screen.getByRole('button', {
        name: 'ingest · package · 6 of 7 files recorded · contains 1 violation',
      }),
    ).toBeInTheDocument();
  });

  it('renders no violation badge when nothing is contained', () => {
    renderNode(ingest);
    expect(screen.queryByTestId('violation-badge')).toBeNull();
  });

  it('highlights via rule-strong border + ink marker without touching the fill', () => {
    renderNode(ingest, { highlighted: true }); // 6/7 → level 3
    const button = screen.getByRole('button', {
      name: 'ingest · package · 6 of 7 files recorded · highlighted',
    });
    const buttonClasses = button.className.split(/\s+/);
    expect(buttonClasses).toContain('border-rule-strong');
    expect(buttonClasses).toContain(NODE_FILL[3]); // fill channel untouched
    expect(screen.getByTestId('highlight-marker')).toBeInTheDocument();
  });

  it('renders no highlight marker by default', () => {
    renderNode(ingest);
    expect(screen.queryByTestId('highlight-marker')).toBeNull();
  });
});

describe('mapNodeDomId', () => {
  it('derives a stable id and replaces whitespace', () => {
    expect(mapNodeDomId('internal/ingest')).toBe('map-node-internal/ingest');
    expect(mapNodeDomId('my dir/file name.go')).toBe('map-node-my_dir/file_name.go');
  });
});

describe('mapNodeAriaLabel', () => {
  it('appends delta, contained violations, and selection/expansion in order', () => {
    const node = FIXTURE_NODES.find((n) => n.id === 'internal/ingest')!;
    expect(mapNodeAriaLabel(node)).toBe('ingest · package · 6 of 7 files recorded');
    expect(
      mapNodeAriaLabel(node, {
        delta: 'new',
        containedViolations: 1,
        selected: true,
        expanded: true,
      }),
    ).toBe(
      'ingest · package · 6 of 7 files recorded · new · contains 1 violation · selected · expanded',
    );
  });
});

/**
 * Channel-ownership matrix: traceability owns fill/text/border at
 * every level; neither the effort overlay nor delta='new' may reassign the
 * fill. Only delta='removed' replaces it with no fill.
 */
describe('MapSquareNode class matrix', () => {
  const LEVELS: IntensityLevel[] = [4, 3, 2, 1, 0];
  const VARIANTS: Array<[boolean, NodeDelta | undefined]> = [
    [false, undefined],
    [true, undefined],
    [false, 'new'],
    [true, 'new'],
  ];
  const classList = (el: Element) => (el.className as string).split(/\s+/);

  describe.each(LEVELS)('level %d', (level) => {
    const node = LEVEL_NODES[level];

    it.each(VARIANTS)(
      'showEffort=%s delta=%s · fill/text/border owned by traceability',
      (showEffort, delta) => {
        renderNode(node, { showEffort, delta });
        const button = screen.getByRole('button');
        const buttonClasses = classList(button);

        expect(buttonClasses).toContain(NODE_FILL[level]);
        if (delta === 'new') {
          expect(buttonClasses).toContain('border-rule-strong');
        } else {
          expect(buttonClasses).toContain(NODE_BORDER[level]);
        }
        expect(classList(screen.getByText(node.name))).toContain(NODE_TEXT[level]);
        expect(
          classList(screen.getByText(`${node.recordedFiles}/${node.totalFiles} · 100 loc`)),
        ).toContain(NODE_COUNT_TEXT[level]);
      },
    );

    it("delta='removed' is the only state that alters the fill", () => {
      renderNode(node, { delta: 'removed' });
      const buttonClasses = classList(screen.getByRole('button'));
      expect(buttonClasses).toContain('bg-transparent');
      expect(buttonClasses).not.toContain(NODE_FILL[level]);
      expect(buttonClasses).toContain('border-dashed');
    });
  });
});
