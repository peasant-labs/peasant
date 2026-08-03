'use client';

// The design-system Popover is a single props-driven component: it owns the
// open/close state, dismiss-on-outside-click, and Escape-returns-focus that the
// old compound (PopoverTrigger/PopoverContent) split across parts.
import { Popover, Checkbox } from '@/lib/ft-ui';

interface MultiSelectPopoverProps {
  label: string;
  options: (string | { label: string; value: string })[];
  selected: string[];
  onChange: (selected: string[]) => void;
}

function normalizeOption(opt: string | { label: string; value: string }) {
  return typeof opt === "string" ? { label: opt, value: opt } : opt;
}

export default function MultiSelectPopover({ label, options: rawOptions, selected, onChange }: MultiSelectPopoverProps) {
  const options = rawOptions.map(normalizeOption);
  const allSelected = selected.length === 0;

  function toggle(value: string) {
    if (selected.includes(value)) {
      onChange(selected.filter((s) => s !== value));
    } else {
      onChange([...selected, value]);
    }
  }

  let displayLabel: string;
  if (allSelected) {
    displayLabel = label;
  } else if (selected.length === 1) {
    const match = options.find((o) => o.value === selected[0]);
    displayLabel = match ? match.label : selected[0];
  } else {
    displayLabel = `${label} (${selected.length})`;
  }

  return (
    <Popover
      label={`Filter by ${label.toLowerCase()}`}
      title={label}
      triggerClassName="flex h-8 items-center gap-2 border border-rule bg-surface px-3 text-sm text-ink-3 transition-colors hover:text-ink hover:bg-surface-hover cursor-pointer whitespace-nowrap focus-mono"
      content={
        <div className="flex flex-col gap-0.5">
          <Checkbox checked={allSelected} onChange={() => onChange([])}>
            All {label.toLowerCase()}
          </Checkbox>
          <div className="border-t border-rule my-1" />
          {options.map((option) => (
            <Checkbox
              key={option.value}
              checked={allSelected || selected.includes(option.value)}
              onChange={() => toggle(option.value)}
            >
              {option.label}
            </Checkbox>
          ))}
        </div>
      }
    >
      {displayLabel}
      <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="opacity-50">
        <path d="M4 6l4 4 4-4" />
      </svg>
    </Popover>
  );
}
