'use client';

import { useCallback, useId, useMemo, useState } from 'react';
import { SearchIcon } from 'lucide-react';
import { cn } from '@/lib/utils';
import type { MapNodeDatum, ZoomLevel } from '@/components/map';
import { searchMapNodes } from '../lib/mapData';

/**
 * The Map toolbar: a grain control + node search.
 * The lens switcher, size-by switcher, and effort overlay toggle were
 * deleted — node width is always code size, and co-edit coupling lives in
 * the node panel's "Often edited with" rows. The eyebrow + square-control
 * styling stays (DESIGN_SYSTEM §1.4).
 *
 * The grain control sets how deep the map opens: "Overview" shows just
 * the top-level areas (modules), "Folders" opens every area to its packages —
 * which is where the import connections actually live, so the map reads as a
 * connected hierarchy rather than a handful of disconnected boxes — and
 * "Files" lets a folder be drilled into individual files (double-click / E).
 */

// ---------------------------------------------------------------------------
// Grain control: a three-step segmented control
// over the zoom level. The labels are plain-language ("Overview / Folders /
// Files"), not the internal level names ('project'/'package'/'file').
// ---------------------------------------------------------------------------

/** Plain-language label + helptext for each grain step. */
const GRAIN_STEPS: ReadonlyArray<{ level: ZoomLevel; label: string; hint: string }> = [
  { level: 'project', label: 'overview', hint: 'Just the top-level areas' },
  { level: 'package', label: 'folders', hint: 'Every area opened to its folders; shows the connections' },
  { level: 'file', label: 'files', hint: 'Drill a folder down to its files (double-click a box)' },
];

function GrainControl({
  level,
  onLevelChange,
}: {
  level: ZoomLevel;
  onLevelChange: (level: ZoomLevel) => void;
}) {
  return (
    <div className="flex flex-col gap-1">
      <span className="v2-eyebrow">detail</span>
      <div className="inline-flex border border-rule" role="group" aria-label="Map detail level">
        {GRAIN_STEPS.map((step, i) => {
          const active = step.level === level;
          return (
            <button
              key={step.level}
              type="button"
              aria-pressed={active}
              title={step.hint}
              onClick={() => onLevelChange(step.level)}
              className={cn(
                'px-2.5 py-1.5 font-mono text-xs transition-colors focus-mono cursor-pointer',
                i > 0 && 'border-l border-rule',
                active
                  ? 'bg-surface-hover text-ink'
                  : 'bg-surface text-ink-3 hover:bg-surface-hover hover:text-ink-2',
              )}
            >
              {step.label}
            </button>
          );
        })}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Node search is a combobox over the graph's node list.
// Filtering is `searchMapNodes` (case-insensitive over ids + names, capped);
// choosing a result hands the node id to the page, which selects it on the
// canvas (highlight lifts to the visible ancestor) and opens its rail panel.
// ---------------------------------------------------------------------------

function NodeSearch({
  nodes,
  onSelect,
}: {
  nodes: readonly MapNodeDatum[];
  onSelect: (id: string) => void;
}) {
  const baseId = useId();
  const listboxId = `${baseId}-results`;
  const [query, setQuery] = useState('');
  const [activeIndex, setActiveIndex] = useState(0);
  const results = useMemo(() => searchMapNodes(nodes, query), [nodes, query]);
  const open = query.trim().length > 0;

  const choose = useCallback(
    (id: string) => {
      onSelect(id);
      setQuery('');
      setActiveIndex(0);
    },
    [onSelect],
  );

  const optionDomId = (index: number) => `${baseId}-option-${index}`;

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setActiveIndex((i) => Math.min(i + 1, Math.max(results.length - 1, 0)));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActiveIndex((i) => Math.max(i - 1, 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const hit = results[activeIndex] ?? results[0];
      if (hit) choose(hit.id);
    } else if (e.key === 'Escape') {
      setQuery('');
      setActiveIndex(0);
    }
  };

  return (
    <div className="flex flex-col gap-1">
      <span className="v2-eyebrow">find</span>
      <div className="relative">
        <SearchIcon
          size={12}
          className="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-ink-3"
          aria-hidden
        />
        <input
          type="text"
          role="combobox"
          aria-label="Search nodes"
          aria-expanded={open}
          aria-controls={listboxId}
          aria-autocomplete="list"
          aria-activedescendant={
            open && results[activeIndex] ? optionDomId(activeIndex) : undefined
          }
          placeholder="Find a node&hellip;"
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
            setActiveIndex(0);
          }}
          onKeyDown={handleKeyDown}
          className="w-52 border border-rule bg-surface py-1.5 pl-7 pr-2 font-mono text-xs text-ink placeholder:text-ink-4 focus-mono"
        />
        {open && (
          <div
            id={listboxId}
            className="absolute left-0 top-full z-20 -mt-px w-72 border border-rule bg-surface-elev"
          >
            {results.length > 0 ? (
              <ul role="listbox" aria-label="Node search results">
                {results.map((n, i) => (
                  <li
                    key={n.id}
                    id={optionDomId(i)}
                    role="option"
                    aria-selected={i === activeIndex}
                    className={cn(
                      'flex cursor-pointer items-baseline gap-2 px-2 py-1.5 text-xs',
                      i === activeIndex ? 'bg-surface-hover' : 'hover:bg-surface-hover',
                    )}
                    onMouseEnter={() => setActiveIndex(i)}
                    onClick={() => choose(n.id)}
                  >
                    <span className="truncate text-ink">{n.name}</span>
                    <span className="min-w-0 flex-1 truncate text-right font-mono text-ink-3">
                      {n.id}
                    </span>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="px-2 py-1.5 text-xs text-ink-3">No nodes match.</p>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

export interface MapToolbarProps {
  /** Searchable node list (the loaded graph); empty renders nothing. */
  nodes: readonly MapNodeDatum[];
  /** Called with the chosen node id when a search result is picked. */
  onSelect: (id: string) => void;
  /** Current grain (zoom level) — drives the segmented control's active step. */
  level: ZoomLevel;
  /** Called when the grain control picks a new level. */
  onLevelChange: (level: ZoomLevel) => void;
}

/**
 * Toolbar row: grain control + node search. Renders nothing until the
 * graph's nodes exist (the grain control is meaningless with no tree).
 */
export function MapToolbar({ nodes, onSelect, level, onLevelChange }: MapToolbarProps) {
  if (nodes.length === 0) return null;
  return (
    <div className="flex flex-wrap items-end gap-4">
      <GrainControl level={level} onLevelChange={onLevelChange} />
      <NodeSearch nodes={nodes} onSelect={onSelect} />
    </div>
  );
}
