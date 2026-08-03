'use client';

import { SearchIcon, XIcon } from 'lucide-react';
import { Input, Checkbox } from '@/lib/ft-ui';
import MultiSelectPopover from '@/components/MultiSelectPopover';

export interface SessionFilterState {
  search: string;
  providers: string[];
  outcomes: string[];
  projects: string[];
  sources: string[];
  humanLabels: string[];
  agentLabels: string[];
  showSubagents: boolean;
}

interface SessionFilterBarProps {
  filters: SessionFilterState;
  onChange: (filters: SessionFilterState) => void;
  providerOptions: { label: string; value: string }[];
  outcomeOptions: { label: string; value: string }[];
  projectOptions: { label: string; value: string }[];
  sourceOptions: { label: string; value: string }[];
  humanLabelOptions?: { label: string; value: string }[];
  agentLabelOptions?: { label: string; value: string }[];
  /** Hide the Projects filter (e.g. when already scoped to a single project). */
  hideProject?: boolean;
  /** Hide the Outcomes filter. */
  hideOutcome?: boolean;
  /** Hide the Source filter. */
  hideSource?: boolean;
  /** Whether any subagent sessions exist (controls toggle visibility). */
  hasSubagentSessions?: boolean;
  resultCount: number;
}

const EMPTY_FILTERS: SessionFilterState = {
  search: '',
  providers: [],
  outcomes: [],
  projects: [],
  sources: [],
  humanLabels: [],
  agentLabels: [],
  showSubagents: false,
};

export default function SessionFilterBar({
  filters,
  onChange,
  providerOptions,
  outcomeOptions,
  projectOptions,
  sourceOptions,
  humanLabelOptions,
  agentLabelOptions,
  hideProject,
  hideOutcome,
  hideSource,
  hasSubagentSessions,
  resultCount,
}: SessionFilterBarProps) {
  const hasActiveFilters =
    filters.providers.length > 0 ||
    filters.outcomes.length > 0 ||
    filters.projects.length > 0 ||
    filters.sources.length > 0 ||
    filters.humanLabels.length > 0 ||
    filters.agentLabels.length > 0 ||
    filters.showSubagents ||
    filters.search.trim().length > 0;

  return (
    <div className="sticky top-16 z-[250] bg-surface border-b border-rule py-3 -mx-6 px-6">
      <div className="flex items-center gap-2 flex-wrap">
        <div className="relative w-[200px]">
          <Input
            iconLeft={SearchIcon}
            placeholder="search sessions..."
            aria-label="search sessions"
            value={filters.search}
            onChange={(e) => onChange({ ...filters, search: e.target.value })}
            // The control isn't full-width by default; reserve room on the right
            // for the clear button so typed text never slides under it.
            style={{ width: '100%', paddingRight: '1.75rem' }}
          />
          {filters.search && (
            <button
              type="button"
              onClick={() => onChange({ ...filters, search: '' })}
              aria-label="Clear search"
              className="absolute right-2 top-1/2 -translate-y-1/2 z-10 text-ink-3 hover:text-ink cursor-pointer focus-mono transition-colors"
            >
              <XIcon className="size-3.5" />
            </button>
          )}
        </div>

        <MultiSelectPopover
          label="providers"
          options={providerOptions}
          selected={filters.providers}
          onChange={(v) => onChange({ ...filters, providers: v })}
        />

        {!hideProject && (
          <MultiSelectPopover
            label="projects"
            options={projectOptions}
            selected={filters.projects}
            onChange={(v) => onChange({ ...filters, projects: v })}
          />
        )}

        {!hideSource && (
          <MultiSelectPopover
            label="source"
            options={sourceOptions}
            selected={filters.sources}
            onChange={(v) => onChange({ ...filters, sources: v })}
          />
        )}

        {!hideOutcome && (
          <MultiSelectPopover
            label="outcomes"
            options={outcomeOptions}
            selected={filters.outcomes}
            onChange={(v) => onChange({ ...filters, outcomes: v })}
          />
        )}

        {humanLabelOptions && humanLabelOptions.length > 0 && (
          <MultiSelectPopover
            label="human"
            options={humanLabelOptions}
            selected={filters.humanLabels}
            onChange={(v) => onChange({ ...filters, humanLabels: v })}
          />
        )}

        {agentLabelOptions && agentLabelOptions.length > 0 && (
          <MultiSelectPopover
            label="agent"
            options={agentLabelOptions}
            selected={filters.agentLabels}
            onChange={(v) => onChange({ ...filters, agentLabels: v })}
          />
        )}

        {hasSubagentSessions && (
          <Checkbox
            checked={filters.showSubagents}
            onChange={(checked: boolean) => onChange({ ...filters, showSubagents: checked })}
          >
            Subagents
          </Checkbox>
        )}

        {hasActiveFilters && (
          <button
            onClick={() => onChange(EMPTY_FILTERS)}
            className="text-xs text-ink-3 hover:text-danger transition-colors cursor-pointer ml-1 focus-mono"
          >
            Clear
          </button>
        )}

        <span className="ml-auto text-xs text-ink-3 tabular-nums font-mono">
          {resultCount} session{resultCount !== 1 ? 's' : ''}
        </span>
      </div>
    </div>
  );
}
