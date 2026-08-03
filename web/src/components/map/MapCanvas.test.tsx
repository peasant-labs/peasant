import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import type React from 'react';
import { FIT_VIEW_OPTIONS, MapCanvas } from './MapCanvas';
import { ACTIVITY_DASH, ACTIVITY_REMOVED_DASH } from './edges';
import { mapNodeDomId } from './MapSquareNode';
import {
  FIXTURE_ACTIVITY_EDGES,
  FIXTURE_NODES,
  FIXTURE_STRUCTURE_EDGES,
  FIXTURE_VIOLATIONS,
} from './test-fixtures';
import type { ActivityEdgeDatum } from './types';

// Render the canvas without the React Flow runtime: the mock ReactFlow paints
// node/edge components straight from `nodeTypes`/`edgeTypes` so the real
// MapCanvas wiring (data, ids, deltas, a11y) is what's under test.
vi.mock('@xyflow/react', () => {
  const rf = { fitView: vi.fn(), zoomIn: vi.fn(), zoomOut: vi.fn() };
  /* eslint-disable @typescript-eslint/no-explicit-any */
  return {
    ReactFlowProvider: ({ children }: { children?: React.ReactNode }) => children,
    useReactFlow: () => rf,
    MarkerType: { ArrowClosed: 'arrowclosed' },
    Handle: () => null,
    Position: { Top: 'top', Right: 'right', Bottom: 'bottom', Left: 'left' },
    getSmoothStepPath: () => ['M 0 0 L 1 1', 0, 0],
    BaseEdge: (props: { id?: string; style?: React.CSSProperties }) => (
      <div
        data-testid="base-edge"
        data-edge-id={props.id}
        data-stroke={props.style?.stroke}
        data-stroke-width={String(props.style?.strokeWidth)}
        data-dash={String(props.style?.strokeDasharray ?? '')}
      />
    ),
    ReactFlow: (props: any) => (
      <div
        data-testid="react-flow"
        data-min-zoom={String(props.minZoom)}
        data-max-zoom={String(props.maxZoom)}
        data-fit-view={String(props.fitView)}
        data-fit-padding={String(props.fitViewOptions?.padding)}
        data-fit-min-zoom={String(props.fitViewOptions?.minZoom)}
        data-fit-max-zoom={String(props.fitViewOptions?.maxZoom)}
      >
        {props.nodes.map((n: any) => {
          const NodeComp = props.nodeTypes[n.type];
          return <NodeComp key={n.id} id={n.id} data={n.data} />;
        })}
        {props.edges.map((e: any) => {
          const EdgeComp = props.edgeTypes[e.type];
          return (
            <EdgeComp
              key={e.id}
              id={e.id}
              data={e.data}
              sourceX={0}
              sourceY={0}
              targetX={10}
              targetY={10}
              sourcePosition="bottom"
              targetPosition="top"
              markerEnd="url(#m)"
            />
          );
        })}
        {props.children}
      </div>
    ),
  };
  /* eslint-enable @typescript-eslint/no-explicit-any */
});

/** Activity edge that aggregates to web→internal · the same visible pair as
 * the web/lib→store structure edge, to prove deltas don't leak across
 * families. */
const CROSS_FAMILY_ACTIVITY: ActivityEdgeDatum[] = [
  ...FIXTURE_ACTIVITY_EDGES,
  { from: 'web/lib', to: 'internal/store', taskCount: 3 },
];

function edgeEl(id: string): HTMLElement {
  const el = document.querySelector(`[data-edge-id="${id}"]`);
  expect(el, `edge ${id} should render`).not.toBeNull();
  return el as HTMLElement;
}

describe('MapCanvas fit constraints', () => {
  it('clamps every fit to FIT_VIEW_OPTIONS while manual zoom keeps the wider range', async () => {
    render(<MapCanvas nodes={FIXTURE_NODES} />);
    const flow = screen.getByTestId('react-flow');

    // Initial fit is enabled and carries the clamped options.
    expect(flow.dataset.fitView).toBe('true');
    expect(flow.dataset.fitPadding).toBe(String(FIT_VIEW_OPTIONS.padding));
    expect(flow.dataset.fitMinZoom).toBe(String(FIT_VIEW_OPTIONS.minZoom));
    expect(flow.dataset.fitMaxZoom).toBe(String(FIT_VIEW_OPTIONS.maxZoom));

    // Manual zoom range stays wider than the fit clamp.
    expect(Number(flow.dataset.minZoom)).toBeLessThan(FIT_VIEW_OPTIONS.minZoom);
    expect(Number(flow.dataset.maxZoom)).toBeGreaterThan(FIT_VIEW_OPTIONS.maxZoom);

    // The Fit button refits with the same clamped options.
    const { useReactFlow } = await import('@xyflow/react');
    const rf = useReactFlow() as unknown as { fitView: ReturnType<typeof vi.fn> };
    fireEvent.click(screen.getByRole('button', { name: 'Fit map to view' }));
    expect(rf.fitView).toHaveBeenLastCalledWith({ ...FIT_VIEW_OPTIONS, duration: 200 });
  });

  it('the fit floor keeps node labels legible (≥ 6px effective at 12px base)', () => {
    expect(12 * FIT_VIEW_OPTIONS.minZoom).toBeGreaterThanOrEqual(6);
  });
});

describe('MapCanvas a11y wiring', () => {
  it('gives node roots stable DOM ids and tracks keyboard focus via aria-activedescendant', () => {
    render(<MapCanvas nodes={FIXTURE_NODES} />);
    const app = screen.getByRole('application');

    // Node roots carry ids derived from node ids before any interaction.
    expect(document.getElementById(mapNodeDomId('cmd'))).not.toBeNull();
    expect(document.getElementById(mapNodeDomId('internal'))).not.toBeNull();
    expect(app).not.toHaveAttribute('aria-activedescendant');

    fireEvent.keyDown(app, { key: 'ArrowDown' }); // anchors to the first node
    expect(app).toHaveAttribute('aria-activedescendant', mapNodeDomId('cmd'));
  });

  it('announces the focused node label and state in a visually hidden live region', () => {
    render(<MapCanvas nodes={FIXTURE_NODES} />);
    const app = screen.getByRole('application');
    const live = screen.getByTestId('map-live-region');
    expect(live.className).toContain('sr-only');
    expect(live).toHaveAttribute('aria-live', 'polite');
    expect(live).toHaveTextContent('');

    fireEvent.keyDown(app, { key: 'ArrowDown' });
    expect(live).toHaveTextContent('cmd · module · 2 of 2 files recorded');

    fireEvent.keyDown(app, { key: 'Enter' }); // select the focused node
    expect(live).toHaveTextContent('cmd · module · 2 of 2 files recorded · selected');
    expect(
      screen.getByRole('button', { name: 'cmd · module · 2 of 2 files recorded · selected' }),
    ).toHaveAttribute('aria-pressed', 'true');
  });

  it('marks aggregates with children as expandable via aria-expanded', () => {
    render(<MapCanvas nodes={FIXTURE_NODES} />);
    expect(
      screen.getByRole('button', { name: /^internal · module/ }),
    ).toHaveAttribute('aria-expanded', 'false');
    expect(
      screen.getByRole('button', { name: /^cmd · module/ }),
    ).not.toHaveAttribute('aria-expanded');
  });
});

describe('MapCanvas per-family edge deltas', () => {
  const baseProps = {
    nodes: FIXTURE_NODES,
    structureEdges: FIXTURE_STRUCTURE_EDGES,
    activityEdges: CROSS_FAMILY_ACTIVITY,
  };

  it('legacy edgeDeltas still applies to both families of a pair', () => {
    render(<MapCanvas {...baseProps} edgeDeltas={{ 'web→internal': 'new' }} />);
    expect(edgeEl('s:web→internal').dataset.stroke).toBe('hsl(var(--edge-strong))');
    expect(edgeEl('a:web→internal').dataset.stroke).toBe('hsl(var(--edge-strong))');
  });

  it('structureEdgeDeltas marks only the structure edge of the pair', () => {
    render(<MapCanvas {...baseProps} structureEdgeDeltas={{ 'web→internal': 'new' }} />);
    expect(edgeEl('s:web→internal').dataset.stroke).toBe('hsl(var(--edge-strong))');
    expect(edgeEl('s:web→internal').dataset.strokeWidth).toBe('1.5');
    const activity = edgeEl('a:web→internal');
    expect(activity.dataset.stroke).toBe('hsl(var(--edge))');
    expect(activity.dataset.dash).toBe(ACTIVITY_DASH);
  });

  it('activityEdgeDeltas marks only the activity edge; removed activity edges recede', () => {
    render(<MapCanvas {...baseProps} activityEdgeDeltas={{ 'web→internal': 'removed' }} />);
    const activity = edgeEl('a:web→internal');
    // Distinct from unchanged dashed activity edges: sparser dash + fixed 1px.
    expect(activity.dataset.dash).toBe(ACTIVITY_REMOVED_DASH);
    expect(activity.dataset.strokeWidth).toBe('1');
    expect(activity.dataset.stroke).toBe('hsl(var(--edge))');
    const structure = edgeEl('s:web→internal');
    expect(structure.dataset.stroke).toBe('hsl(var(--edge))');
    expect(structure.dataset.dash).toBe('');
  });

  it('per-family deltas take precedence over legacy edgeDeltas per key', () => {
    render(
      <MapCanvas
        {...baseProps}
        structureEdgeDeltas={{ 'web→internal': 'new' }}
        edgeDeltas={{ 'web→internal': 'removed' }}
      />,
    );
    // Structure: the family-specific 'new' wins over the legacy 'removed'.
    const structure = edgeEl('s:web→internal');
    expect(structure.dataset.stroke).toBe('hsl(var(--edge-strong))');
    expect(structure.dataset.dash).toBe('');
    // Activity: no family-specific entry → falls back to the legacy delta.
    expect(edgeEl('a:web→internal').dataset.dash).toBe(ACTIVITY_REMOVED_DASH);
  });

  it('keeps unchanged activity edges weight-scaled and 4 3 dashed', () => {
    render(<MapCanvas {...baseProps} />);
    const activity = edgeEl('a:internal→web'); // weight 6 → 2px
    expect(activity.dataset.dash).toBe(ACTIVITY_DASH);
    expect(activity.dataset.strokeWidth).toBe('2');
  });
});

describe('MapCanvas highlightedIds', () => {
  it('highlights visible nodes and lifts collapsed children onto their visible ancestor', () => {
    render(
      <MapCanvas
        nodes={FIXTURE_NODES}
        // 'cmd' is visible at project zoom; pipeline.go is collapsed inside
        // 'internal' and must light its visible ancestor up instead.
        highlightedIds={new Set(['cmd', 'internal/ingest/pipeline.go'])}
      />,
    );
    const cmd = screen.getByRole('button', {
      name: 'cmd · module · 2 of 2 files recorded · highlighted',
    });
    const internal = screen.getByRole('button', {
      name: 'internal · module · 14 of 16 files recorded · highlighted',
    });
    for (const button of [cmd, internal]) {
      expect(button.className.split(/\s+/)).toContain('border-rule-strong');
      expect(button.querySelector('[data-testid="highlight-marker"]')).not.toBeNull();
    }
    // The fill channel stays owned by traceability (cmd 2/2 → level 4,
    // internal 14/16 → level 3).
    expect(cmd.className.split(/\s+/)).toContain('bg-surface-elev');
    expect(internal.className.split(/\s+/)).toContain('bg-surface');
    // Untouched nodes carry neither the emphasis border nor the marker.
    const web = screen.getByRole('button', { name: /^web · module/ });
    expect(web.className.split(/\s+/)).not.toContain('border-rule-strong');
    expect(web.querySelector('[data-testid="highlight-marker"]')).toBeNull();
  });
});

describe('MapCanvas contained violations', () => {
  it('badges the aggregate owning a hidden violation and counts it in the aria-label', () => {
    render(<MapCanvas nodes={FIXTURE_NODES} violations={FIXTURE_VIOLATIONS} />);
    // The store↔ingest cycle collapses inside `internal` at project zoom.
    const internal = screen.getByRole('button', {
      name: 'internal · module · 14 of 16 files recorded · contains 1 violation',
    });
    expect(internal.querySelector('[data-testid="violation-badge"]')).not.toBeNull();
    // Lifted violations still render as danger edges.
    expect(edgeEl('v:internal→cmd').dataset.stroke).toBe('hsl(var(--danger))');
    // Nodes without hidden violations get no badge.
    const cmd = screen.getByRole('button', { name: /^cmd · module/ });
    expect(cmd.querySelector('[data-testid="violation-badge"]')).toBeNull();
  });
});
